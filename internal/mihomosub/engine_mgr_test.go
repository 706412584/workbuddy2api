package mihomosub

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestIsLoopback bind 地址必须识别出 loopback 与非 loopback。
//
// 回归背景：mihomo 的 mixed listener **无鉴权**，绑到非 loopback 等于给本机开一个
// 开放代理出口（同一局域网的人可直接借道）。参考项目有 validateLoopbackBind 校验，
// 我们照搬并覆盖常见写法。
func TestIsLoopback(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"127.0.0.1", true},
		{"127.0.0.1 ", true},
		{"LOCALHOST", true},
		{"::1", true},
		{"0.0.0.0", false},
		{"192.168.1.3", false},
		{"", false},
		{"example.com", false},
	} {
		if got := isLoopback(tc.in); got != tc.want {
			t.Errorf("isLoopback(%q)=%v want %v", tc.in, got, tc.want)
		}
	}
}

// TestEnsureRunningRejectsNonLoopback 非 loopback 必须被拒（安全红线）。
func TestEnsureRunningRejectsNonLoopback(t *testing.T) {
	e := NewEngine("", t.TempDir())
	for _, addr := range []string{"0.0.0.0", "192.168.1.3", ""} {
		err := e.EnsureRunning("s1", "sidecar-", addr, nil, "deadbeef")
		if err == nil {
			t.Fatalf("bind=%q 应被拒绝（非 loopback）", addr)
		}
		if !strings.Contains(err.Error(), "loopback") {
			t.Errorf("bind=%q 的错误信息未点明 loopback：%v", addr, err)
		}
	}
}

// TestPreflightPortsDetectsOccupied 端口被占用时必须提前报错。
//
// 回归背景（关键）：mihomo 遇到已被占用的 listener 端口**不会退出**，只记一条 bind
// 错误并继续运行。参考项目没做预检，于是会陷入「启动成功 → 5s 探活失败 → 重启 →
// 仍然失败」的空转；且期间面板显示 running、代理全废。
func TestPreflightPortsDetectsOccupied(t *testing.T) {
	// 占一个端口
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	busy := ln.Addr().(*net.TCPAddr).Port

	if err := preflightPorts([]Binding{{Port: busy}}); err == nil {
		t.Fatalf("端口 %d 已被占用，preflightPorts 应报错", busy)
	} else if !strings.Contains(err.Error(), strconv.Itoa(busy)) {
		t.Errorf("错误信息应包含端口号 %d：%v", busy, err)
	}

	// 空闲端口应通过（先拿一个再释放）
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen2: %v", err)
	}
	free := ln2.Addr().(*net.TCPAddr).Port
	_ = ln2.Close()
	if err := preflightPorts([]Binding{{Port: free}}); err != nil {
		t.Errorf("空闲端口 %d 应通过预检：%v", free, err)
	}
}

// TestEnsureRunningNeedsBinaryOrHash 没有二进制时必须给出可操作的错误信息
// （不能只提参考项目的环境变量名——那在本项目不存在）。
func TestEnsureRunningNeedsBinaryOrHash(t *testing.T) {
	e := NewEngine(filepath.Join(t.TempDir(), "definitely-missing-mihomo"), t.TempDir())
	err := e.EnsureRunning("s1", "sidecar-", "127.0.0.1", nil, "deadbeef")
	if err == nil {
		t.Fatal("二进制不存在时应报错")
	}
	if !strings.Contains(err.Error(), "mihomo") {
		t.Errorf("错误信息应点明 mihomo：%v", err)
	}
}

// TestEnsureRunningRequiresHashWhenNoBindings bindings 与 configHash 同时缺失时必须报错
// （无法确定该起哪份配置），而不是起一个空 listener 的进程。
func TestEnsureRunningRequiresHashWhenNoBindings(t *testing.T) {
	dir := t.TempDir()
	// 造一个假的“可执行文件”让 ResolveBinary 通过
	fake := filepath.Join(dir, "mihomo.exe")
	if err := os.WriteFile(fake, []byte("stub"), 0o700); err != nil {
		t.Fatal(err)
	}
	e := NewEngine(fake, dir)
	err := e.EnsureRunning("s1", "sidecar-", "127.0.0.1", nil, "")
	if err == nil {
		t.Fatal("无 bindings 且无 hash 时应报错")
	}
	if !strings.Contains(err.Error(), "configHash") && !strings.Contains(err.Error(), "bindings") {
		t.Errorf("错误信息应说明缺少 bindings/configHash：%v", err)
	}
}

// TestConfigPathShape 配置路径形状（source 分目录，与参考项目一致）。
func TestConfigPathShape(t *testing.T) {
	e := NewEngine("", filepath.Join("data", "proxy-sidecar"))
	want := filepath.Join("data", "proxy-sidecar", "source-s1", "config.yaml")
	if got := e.ConfigPathFor("s1"); got != want {
		t.Errorf("ConfigPathFor=%q want %q", got, want)
	}
}

// TestStopAllIdempotent 空引擎/重复 StopAll 不应 panic。
func TestStopAllIdempotent(t *testing.T) {
	var nilEngine *Engine
	nilEngine.StopAll()       // 不应 panic
	nilEngine.StopSource("x") // 不应 panic
	if n := nilEngine.StatusSnapshot(); len(n) != 0 {
		t.Errorf("nil engine 状态应为空，得到 %v", n)
	}
	e := NewEngine("", t.TempDir())
	e.StopAll()
	e.StopAll()
	if len(e.StatusSnapshot()) != 0 {
		t.Error("StopAll 后状态应为空")
	}
}

// TestRepeatSyncIdempotentNotBlockedBySelf 回归：本进程自己的 mihomo 正占着端口时，
// 再次同步同一份订阅必须**短路成功**，不能被端口预检误判成「端口被占」。
//
// 真实故障（2026-10-21 面板实测）：启动时自动同步起了 mihomo 占住 21680-21683，
// 用户点「同步订阅」时预检试绑被自己的 mihomo 挡下 → 同步永久失败。
// 修法：把「已在运行且配置未变」的短路判断移到预检之前（写配置是幂等的）。
func TestRepeatSyncIdempotentNotBlockedBySelf(t *testing.T) {
	d, err := os.ReadFile("testdata/subscription-sample.yaml")
	if err != nil {
		t.Skip("无样例订阅")
	}
	nodes, err := ParseSubscription(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) == 0 {
		t.Skip("样例无节点")
	}
	// 只验证「同样的 bindings → 同样的 hash」这一幂等前提，
	// 真正起进程的端到端由上层（cmd/server）冒烟覆盖。
	b1, err := BuildBindings(nodes, "sidecar-", "127.0.0.1", "mixed", 21780, 2, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := BuildBindings(nodes, "sidecar-", "127.0.0.1", "mixed", 21780, 2, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	h1, err := WriteMihomoConfig(filepath.Join(dir, "a.yaml"), "127.0.0.1", "sidecar-", b1)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := WriteMihomoConfig(filepath.Join(dir, "b.yaml"), "127.0.0.1", "sidecar-", b2)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Errorf("同样 bindings 应得同样 hash（幂等前提），得到 %s vs %s", h1, h2)
	}
}
