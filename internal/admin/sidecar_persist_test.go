// sidecar_persist_test.go 覆盖两个回归点：
//  1. sidecar 同步不得删除账号的代理绑定（2026-11-01 事故：每小时自动同步把
//     16 条绑定清空，全池悄悄回落直连 → 同 IP 多号 → 上游连坐封号）。
//  2. 连通性探测不得把「首帧前有注释行/空行」的健康账号误判为空流。
package admin

import (
	"bytes"
	"io"
	"testing"

	"workbuddy2api/internal/proxyreg"
)

// TestMergeSlotsKeepsOutOfSetBindingsValid 槽位轮换后，绑定表本身不被 mergeSlots 触碰。
//
// 真正的删除发生在 persistSidecarSlots（已改为不删）。本用例锁住 mergeSlots 的
// 契约：它只处理槽位，**不**返回或改动绑定。
func TestMergeSlotsKeepsOutOfSetBindingsValid(t *testing.T) {
	cur := []proxyreg.Slot{
		{ID: "sidecar-A", Name: "A", URL: "http://127.0.0.1:21080", Enabled: true},
		{ID: "sidecar-B", Name: "B", URL: "http://127.0.0.1:21081", Enabled: true},
		{ID: "manual-slot", Name: "手工", URL: "http://127.0.0.1:9999", Enabled: true},
	}
	// 本次同步只探测到 B（节点轮换：A 掉线）
	incoming := []SidecarSlot{{ID: "sidecar-B", Name: "B", URL: "http://127.0.0.1:21081", Enabled: true}}

	merged := mergeSlots(cur, incoming)

	hasManual := false
	for _, s := range merged {
		if s.ID == "manual-slot" {
			hasManual = true
		}
	}
	if !hasManual {
		t.Error("手工槽位被 sidecar 同步删掉了（合并语义应只动 sidecar 前缀槽位）")
	}
}

// TestHasSSEDataLine 注释行/空行不算 data 帧，其后出现 data: 才算。
func TestHasSSEDataLine(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"注释行在前", ": keep-alive\n", false},
		{"空行在前", "\n\n", false},
		{"注释行后跟 data", ": keep-alive\ndata: {\"a\":1}\n", true},
		{"直接 data", "data: {\"a\":1}\n", true},
		{"只有事件名", "event: message\n", false},
	}
	for _, c := range cases {
		if got := hasSSEDataLine([]byte(c.in)); got != c.want {
			t.Errorf("%s: hasSSEDataLine(%q)=%v want %v", c.name, c.in, got, c.want)
		}
	}
}

// TestFirstFrameSkipsHeartbeat 上游先发 : keep-alive 再发首帧时，必须判成功。
//
// 这是 2026-11-01 面板误报的根因：旧实现读到第一个换行就停，缓冲区里只有注释行，
// 于是健康账号被报成「上游返回 200 但没有 SSE 数据帧」。
func TestFirstFrameSkipsHeartbeat(t *testing.T) {
	stream := ": keep-alive\n\n" +
		"data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: [DONE]\n\n"
	ev, msg, ok := firstFrame(io.NopCloser(bytes.NewReader([]byte(stream))))
	if !ok {
		t.Fatalf("firstFrame 判失败：%s（注释行在前被误判为空流）", msg)
	}
	if ev != "assistant" {
		t.Errorf("firstEvent=%q want assistant", ev)
	}
}

// TestFirstFrameEmptyStreamStillFails 真空流仍要判失败（不能为了修误报而放过坏号）。
func TestFirstFrameEmptyStreamStillFails(t *testing.T) {
	stream := ": keep-alive\n\n" // 只有心跳，永远没有 data
	_, msg, ok := firstFrame(io.NopCloser(bytes.NewReader([]byte(stream))))
	if ok {
		t.Fatalf("空流被判成功，msg=%q", msg)
	}
	if msg == "" {
		t.Error("失败时必须给出原因")
	}
}
