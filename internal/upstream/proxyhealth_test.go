package upstream

import (
	"net"
	"strconv"
	"testing"
	"time"
)

func resetHealthForTest() {
	ResetProxyHealth()
}

// TestProxyHealthBelowThresholdNotUnhealthy 首次失败**不**判定不健康。
//
// 设计取舍：每次失败都做 TCP 探测会给偶发抖动也加一次 RTT。首次失败只换号
// （代价可接受），达 proxyFailThreshold 才探测确认。
func TestProxyHealthBelowThresholdNotUnhealthy(t *testing.T) {
	resetHealthForTest()
	const u = "socks5://127.0.0.1:1"
	NoteProxyResult(u, false, "boom")
	if ProxyUnhealthy(u) {
		t.Fatalf("首次失败不应判定不健康（阈值 %d）", proxyFailThreshold)
	}
	if got := ProxyFails(u); got != 1 {
		t.Errorf("fails=%d want 1", got)
	}
}

// TestProxyHealthSuccessClears 成功一次即清零（代理自愈最可靠的信号）。
func TestProxyHealthSuccessClears(t *testing.T) {
	resetHealthForTest()
	const u = "http://127.0.0.1:2"
	NoteProxyResult(u, false, "boom")
	NoteProxyResult(u, false, "boom")
	NoteProxyResult(u, true, "")
	if ProxyFails(u) != 0 {
		t.Errorf("成功后 fails 应清零，得到 %d", ProxyFails(u))
	}
	if ProxyUnhealthy(u) {
		t.Error("成功后不应不健康")
	}
}

// TestMaybeProbeMarksUnhealthyWhenPortClosed 达阈值且端口确实连不上 → 标记不健康。
//
// 端口选一个必然无人监听的（用 :1 需要权限但 connect 会立即被拒，够用）。
func TestMaybeProbeMarksUnhealthyWhenPortClosed(t *testing.T) {
	resetHealthForTest()
	// 先占用一个端口拿到确定性，再关掉 → 该端口上无监听者。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	u := "socks5://127.0.0.1:" + strconv.Itoa(port)
	// 达阈值（连续失败 ×2）
	NoteProxyResult(u, false, "dial failed")
	NoteProxyResult(u, false, "dial failed")
	if !MaybeProbeProxy(u) {
		t.Fatal("端口无监听者时应判定不健康")
	}
	if !ProxyUnhealthy(u) {
		t.Error("判定后 ProxyUnhealthy 应为 true")
	}
}

// TestMaybeProbeKeepsHealthyWhenPortOpen 端口能连上 → **不**判不健康（是上游抖动而非代理故障）。
//
// 这是本设计的关键正确性：不主动探测就无法区分「代理挂了」与「上游抖动」，
// 而后者若误判成代理故障，会把健康出口集体摘掉。
func TestMaybeProbeKeepsHealthyWhenPortOpen(t *testing.T) {
	resetHealthForTest()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// 接受连接（探活只连不通信）
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	u := "http://127.0.0.1:" + strconv.Itoa(port)

	NoteProxyResult(u, false, "upstream flake")
	NoteProxyResult(u, false, "upstream flake")
	if MaybeProbeProxy(u) {
		t.Fatal("端口可达时不应判定不健康（应视为上游抖动）")
	}
	if ProxyUnhealthy(u) {
		t.Error("端口可达时不应不健康")
	}
	if ProxyFails(u) != 0 {
		t.Errorf("确认端口可达后应清零失败计数，得到 %d", ProxyFails(u))
	}
}

// TestUnhealthyCooldownExpires 冷却到期后自动视为健康（给自愈出口）。
func TestUnhealthyCooldownExpires(t *testing.T) {
	resetHealthForTest()
	const u = "http://127.0.0.1:3"
	// 直接构造不健康状态（绕过探测，聚焦 TTL 逻辑）
	proxyHealthReg.mu.Lock()
	proxyHealthReg.m[u] = &proxyEntry{
		fails:          proxyFailThreshold,
		unhealthyUntil: time.Now().Add(-time.Second), // 已过期
	}
	proxyHealthReg.mu.Unlock()
	if ProxyUnhealthy(u) {
		t.Error("冷却已过期应视为健康")
	}
}

// TestDirectConnectNotTracked 直连（空 URL）不记账、恒健康。
func TestDirectConnectNotTracked(t *testing.T) {
	resetHealthForTest()
	NoteProxyResult("", false, "x")
	if ProxyUnhealthy("") {
		t.Error("直连不应被判不健康")
	}
	if len(ProxyHealth()) != 0 {
		t.Errorf("直连不应产生记账，得到 %d 条", len(ProxyHealth()))
	}
}

// TestProxyHealthSnapshotHidesCredentials 快照里的 URL 必须抹掉凭据（日志/面板会展示）。
func TestProxyHealthSnapshotHidesCredentials(t *testing.T) {
	resetHealthForTest()
	const u = "http://alice:s3cret@127.0.0.1:8080"
	NoteProxyResult(u, false, "boom")
	snaps := ProxyHealth()
	if len(snaps) != 1 {
		t.Fatalf("应有 1 条，得到 %d", len(snaps))
	}
	if got := snaps[0].URL; got != "http://127.0.0.1:8080" {
		t.Errorf("URL=%q 应抹掉凭据", got)
	}
}
