package updater

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestNewTransportDirectByDefault 未配代理 → 直连（Proxy 字段为 nil，不读环境变量）。
// 这是关键回归：原先 httpClient 回落 http.DefaultClient，会隐式读 HTTP(S)_PROXY，
// 而启动脚本清空代理后 GitHub 直连不通 → 自更新卡在下载超时（2026-10-15 实测）。
func TestNewTransportDirectByDefault(t *testing.T) {
	tr, err := newTransport("", "")
	if err != nil {
		t.Fatalf("newTransport(\"\") 不应报错: %v", err)
	}
	if tr.Proxy != nil {
		t.Error("未配代理时 Transport.Proxy 必须为 nil（不继承任何环境变量代理）")
	}
}

// TestNewTransportHTTPProxy 配了 http 代理 → Proxy 函数返回该代理。
func TestNewTransportHTTPProxy(t *testing.T) {
	tr, err := newTransport("http://127.0.0.1:7887", "")
	if err != nil {
		t.Fatalf("newTransport: %v", err)
	}
	req, _ := http.NewRequest("GET", "https://github.com/x", nil)
	u, err := tr.Proxy(req)
	if err != nil {
		t.Fatalf("Proxy: %v", err)
	}
	if u == nil || u.Host != "127.0.0.1:7887" {
		t.Fatalf("Proxy=%v, want http://127.0.0.1:7887", u)
	}
}

// TestNewTransportNoProxyBypass no_proxy 命中的主机绕过代理（返回 nil = 直连）。
func TestNewTransportNoProxyBypass(t *testing.T) {
	tr, err := newTransport("http://127.0.0.1:7887", "github.com,*.internal.example.com")
	if err != nil {
		t.Fatalf("newTransport: %v", err)
	}
	cases := []struct {
		url      string
		wantProx bool
	}{
		{"https://github.com/a", false},             // 精确命中 → 直连
		{"https://api.github.com/a", true},          // 非精确（子域未列）→ 走代理
		{"https://x.internal.example.com/a", false}, // *. 后缀命中
		{"https://internal.example.com/a", false},   // 后缀同时匹配裸域
		{"https://example.com/a", true},             // 不相关 → 走代理
	}
	for _, c := range cases {
		req, _ := http.NewRequest("GET", c.url, nil)
		u, err := tr.Proxy(req)
		if err != nil {
			t.Fatalf("Proxy(%s): %v", c.url, err)
		}
		if got := u != nil; got != c.wantProx {
			t.Errorf("Proxy(%s) proxied=%v, want %v", c.url, got, c.wantProx)
		}
	}
}

// TestNewTransportRejectsBadScheme 非法协议直接报错，不静默直连。
func TestNewTransportRejectsBadScheme(t *testing.T) {
	if _, err := newTransport("ftp://127.0.0.1:21", ""); err == nil {
		t.Error("不支持的协议应报错")
	}
	if _, err := newTransport("http://", ""); err == nil {
		t.Error("缺少主机的代理 URL 应报错")
	}
}

// TestNewTransportSocks5 socks5 走 DialContext（Transport.Proxy 保持 nil）。
func TestNewTransportSocks5(t *testing.T) {
	tr, err := newTransport("socks5://127.0.0.1:1080", "")
	if err != nil {
		t.Fatalf("newTransport(socks5): %v", err)
	}
	if tr.Proxy != nil {
		t.Error("socks5 应走 DialContext，Transport.Proxy 须为 nil")
	}
	if tr.DialContext == nil {
		t.Error("socks5 必须设置 DialContext")
	}
}

// TestHTTPClientIgnoresEnvProxy 端到端：env 里放了（失效的）代理变量，
// 但未配 update.proxy 时必须直连 —— 这正是本次修复的核心行为。
func TestHTTPClientIgnoresEnvProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1") // 故意指向一个必然连不上的端口
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "http://127.0.0.1:1")

	// httptest 服务器：只有直连才能命中（若走了 env 里的死代理必然失败）。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := &Client{} // 未配 Proxy/NoProxy
	resp, err := c.httpClient().Get(srv.URL)
	if err != nil {
		t.Fatalf("未配代理时应直连成功（env 代理必须被忽略）: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
}

// TestHTTPClientUsesConfiguredProxy 配了代理时确实走它（用一个本地代理桩验证）。
func TestHTTPClientUsesConfiguredProxy(t *testing.T) {
	var proxied bool
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied = true
		w.Write([]byte("via-proxy"))
	}))
	defer proxySrv.Close()

	c := &Client{Proxy: proxySrv.URL}
	// 目标地址不可达也无妨：请求会先到代理桩，由它直接应答。
	resp, err := c.httpClient().Get("http://upstream.invalid/x")
	if err != nil {
		t.Fatalf("经代理应成功: %v", err)
	}
	defer resp.Body.Close()
	if !proxied {
		t.Error("配了 update.proxy 时必须经代理发出请求")
	}
}

// TestHTTPClientInjectedWins 测试注入的 HTTPClient 优先级最高（既有测试依赖）。
func TestHTTPClientInjectedWins(t *testing.T) {
	injected := &http.Client{}
	c := &Client{HTTPClient: injected, Proxy: "http://127.0.0.1:9"}
	if got := c.httpClient(); got != injected {
		t.Error("注入的 HTTPClient 必须原样返回，不被 Proxy 配置覆盖")
	}
}

// TestNewTransportInvalidProxyFallsBack 非法代理 URL：httpClient 不 panic，
// 回落直连（错误由请求本身暴露）。
func TestNewTransportInvalidProxyFallsBack(t *testing.T) {
	c := &Client{Proxy: "not-a-valid-url://x"}
	if got := c.httpClient(); got == nil {
		t.Fatal("非法代理 URL 不应导致 nil client")
	}
}
