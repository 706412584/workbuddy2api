// sidecar_probe_test.go 同步期出口探测的回归锁（2026-10-24）。
//
// 背景：槽位由订阅生成，而机场会轮换节点。实测订阅里 5 个节点全部不可用，
// 展开成槽位后经它们访问 generate_204 一律 502/超时，但面板健康列全绿 ——
// 健康账只记「真实出站」，而这些槽位从没被用过，用户绑上账号才发现全废。
//
// 本文件锁死三件事：
//  1. 探测不通的候选被丢弃；
//  2. 丢弃后从候选池**后续节点补足**到 max_nodes（候选池必须大于 max_nodes）；
//  3. **全部不通时不落盘**（返回错误，保留原槽位配置）——账号失去出口比报错严重。
package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"workbuddy2api/internal/mihomosub"
	"workbuddy2api/internal/upstream"
)

// withProbeStub 把探测替换成桩，返回还原函数。
// badPorts 里的端口视为不可用，其余可用。
func withProbeStub(t *testing.T, badPorts map[int]bool) func() {
	t.Helper()
	orig := probeFn
	probeFn = func(_ context.Context, url string) upstream.ProbeResult {
		var port int
		if _, err := fmt.Sscanf(url, "http://127.0.0.1:%d", &port); err != nil {
			return upstream.ProbeResult{Stage: "dial", Error: "bad url: " + url}
		}
		if badPorts[port] {
			return upstream.ProbeResult{Stage: "request", Status: 502,
				Error: "出口返回 HTTP 502（节点不可用）"}
		}
		return upstream.ProbeResult{OK: true, Stage: "ok", Status: 204}
	}
	return func() { probeFn = orig }
}

// fakeBindings 造 n 个候选 binding，端口从 basePort 起（与 BuildBindings 同规则）。
func fakeBindings(n, basePort int) []mihomosub.Binding {
	out := make([]mihomosub.Binding, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, mihomosub.Binding{
			Port: basePort + i,
			Name: fmt.Sprintf("sidecar-n%02d-节点%02d", i, i),
			Node: mihomosub.Node{Name: fmt.Sprintf("节点%02d", i), Type: "hysteria2"},
		})
	}
	return out
}

// TestProbeBindingsDropsDead 探测不通的候选被丢弃，且**保序**（确定性）。
func TestProbeBindingsDropsDead(t *testing.T) {
	defer withProbeStub(t, map[int]bool{21081: true, 21083: true})()
	s := &Sidecar{cfg: ProxySidecarConfig{BasePort: 21080, MaxNodes: 5}}
	bs := fakeBindings(5, 21080)

	alive, dead := s.probeBindings(bs)
	if len(dead) != 2 {
		t.Errorf("应有 2 个不可用：got %d（%v）", len(dead), dead)
	}
	if len(alive) != 3 {
		t.Fatalf("应有 3 个可用：got %d", len(alive))
	}
	// 保序：剩下的必须是 21080 / 21082 / 21084
	wantPorts := []int{21080, 21082, 21084}
	for i, b := range alive {
		if b.Port != wantPorts[i] {
			t.Errorf("存活候选顺序不对：idx=%d got port=%d want=%d", i, b.Port, wantPorts[i])
		}
	}
}

// TestProbeAllDeadIsError 全部不通时不得返回任何存活候选。
// 调用方（applySubscription）据此返回错误且不落盘。
func TestProbeAllDeadIsError(t *testing.T) {
	bad := map[int]bool{}
	for p := 21080; p < 21090; p++ {
		bad[p] = true
	}
	defer withProbeStub(t, bad)()
	s := &Sidecar{cfg: ProxySidecarConfig{BasePort: 21080, MaxNodes: 5}}

	alive, dead := s.probeBindings(fakeBindings(10, 21080))
	if len(alive) != 0 {
		t.Fatalf("全部不通时不该有存活候选：got %d", len(alive))
	}
	if len(dead) != 10 {
		t.Errorf("应记全部 10 个失败：got %d", len(dead))
	}
}

// TestProbeCapExceedsMaxNodes 候选池必须大于 max_nodes ——
// 否则「前 N 个恰好失效」会直接变成同步失败，而订阅后面还有能用的节点。
func TestProbeCapExceedsMaxNodes(t *testing.T) {
	cases := []struct {
		maxNodes, total, want int
	}{
		{maxNodes: 8, total: 53, want: 32},   // 8*3+8=32，未触顶
		{maxNodes: 5, total: 6, want: 6},     // 受节点总数限制
		{maxNodes: 20, total: 100, want: 40}, // 触 probeCeiling
		{maxNodes: 8, total: 0, want: 0},
	}
	for _, c := range cases {
		s := &Sidecar{cfg: ProxySidecarConfig{MaxNodes: c.maxNodes}}
		if got := s.probeCap(c.total); got != c.want {
			t.Errorf("probeCap(max=%d,total=%d): got %d want %d", c.maxNodes, c.total, got, c.want)
		}
	}
}

// TestSelectAliveTruncatesToMax 存活数超过 max_nodes 时截断，且取的是**前面的**
// （候选已按 identity 稳定排序，故截断是确定性的）。
func TestSelectAliveTruncatesToMax(t *testing.T) {
	alive := fakeBindings(10, 21080)
	got := selectAlive(alive, 3)
	if len(got) != 3 {
		t.Fatalf("应截断到 3：got %d", len(got))
	}
	if got[0].Port != 21080 || got[2].Port != 21082 {
		t.Errorf("截断应取前 3 个：got ports %d..%d", got[0].Port, got[2].Port)
	}
	// 不超过时原样返回
	if same := selectAlive(alive[:2], 5); len(same) != 2 {
		t.Errorf("存活数 < max 时不该截断：got %d", len(same))
	}
}

// TestTopUpUsesLaterNodes 是本功能的核心场景：
// 前 max_nodes 个恰好全部失效，但订阅后面还有能用的节点 —— 必须补得进来。
//
// 用 probeCap 展开候选（> max_nodes），前 8 个坏的、后面 8 个好的，
// 期望存活的是**后面那批**，而不是「前 8 个坏 → 同步失败」。
func TestTopUpUsesLaterNodes(t *testing.T) {
	bad := map[int]bool{}
	for p := 21080; p < 21088; p++ { // 前 8 个（max_nodes=8）全坏
		bad[p] = true
	}
	defer withProbeStub(t, bad)()

	s := &Sidecar{cfg: ProxySidecarConfig{BasePort: 21080, MaxNodes: 8}}
	// 候选池 = probeCap(16) = 16 个（8*3+8=32 但受总数 16 限制）
	cap16 := s.probeCap(16)
	if cap16 != 16 {
		t.Fatalf("probeCap(16) 应为 16：got %d", cap16)
	}
	alive, dead := s.probeBindings(fakeBindings(cap16, 21080))
	if len(alive) != 8 {
		t.Fatalf("应从后 8 个补足到 8 个可用：got alive=%d dead=%d", len(alive), len(dead))
	}
	// 补进来的必须是后面的（端口 >= 21088）
	for _, b := range alive {
		if b.Port < 21088 {
			t.Errorf("补进来的候选不该是坏的：port=%d", b.Port)
		}
	}
	// 截断后仍是 8 个（不超 max_nodes）
	if got := selectAlive(alive, s.cfg.MaxNodes); len(got) != 8 {
		t.Errorf("截断后应为 8：got %d", len(got))
	}
}

// TestFirstLineTrimsProbeError 探测错误模板很长，面板只放一行。
func TestFirstLineTrimsProbeError(t *testing.T) {
	if got := firstLine("第一行\n第二行"); got != "第一行" {
		t.Errorf("firstLine 应取首行：got %q", got)
	}
	long := strings.Repeat("x", 10)
	if got := firstLine(long); got != long {
		t.Errorf("无换行时应原样返回：got %q", got)
	}
}
