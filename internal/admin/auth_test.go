// auth_test.go 管理接口的访问控制：本机免口令、局域网需 admin.token。
//
// 这层是「能写文件」与「任何人都能写文件」之间的屏障，网关默认监听 0.0.0.0，
// 所以每条路径都得测到 —— 漏一条就是一个能删账号的洞。
package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// callAdmin 用指定来源地址与口令发一个管理请求。
// remoteAddr 为空时用本机地址。
func callAdmin(h *Handler, remoteAddr, token, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	if remoteAddr == "" {
		remoteAddr = "127.0.0.1:12345"
	}
	r.RemoteAddr = remoteAddr
	if token != "" {
		r.Header.Set(adminTokenHeader, token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// TestAdminLocalAlwaysAllowed 本机无条件放行，未配口令也一样。
// 这是改造前的行为，必须保持不变 —— 否则本机用面板也得填口令。
//
// 地址用 Go 实际写入 RemoteAddr 的形式：IPv6 带方括号（`[::1]:port`）。
// 不带方括号的 `::1:port` 是歧义的，SplitHostPort 会失败，不是真实输入。
func TestAdminLocalAlwaysAllowed(t *testing.T) {
	for _, addr := range []string{
		"127.0.0.1:1234",
		"[::1]:1234",
		"[::ffff:127.0.0.1]:1234", // IPv4-mapped，某些栈会这样写
	} {
		h := New(Config{}) // 未配 Token
		rec := callAdmin(h, addr, "", "/__admin/accounts")
		if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
			t.Errorf("本机 %s 被拒: code=%d body=%s", addr, rec.Code, rec.Body)
		}
	}
}

// TestAdminRemoteRejectedEvenWithTokenWhenNotConfigured 反向确认：
// 局域网地址（含本机网段外的私网地址）不会因为"看起来像内网"而被放行。
// 判定只认回环地址，不做网段推断 —— 内网同样可能有不可信设备。
func TestAdminRemoteRejectedEvenWithTokenWhenNotConfigured(t *testing.T) {
	h := New(Config{})
	for _, addr := range []string{
		"192.168.1.50:5555",
		"10.0.0.7:5555",
		"172.16.3.9:5555",
		"[fe80::1]:5555",
	} {
		rec := callAdmin(h, addr, "guessed-token", "/__admin/accounts")
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s 被放行（code=%d）—— 只有回环地址才应免鉴权", addr, rec.Code)
		}
	}
}

// TestAdminRemoteWithoutTokenConfigured 未配 Token 时，非本机一律拒绝（403）。
// 这是「想开局域网必须显式配口令」的保证 —— 不存在误开成裸奔的路径。
func TestAdminRemoteWithoutTokenConfigured(t *testing.T) {
	h := New(Config{})
	rec := callAdmin(h, "192.168.1.50:5555", "", "/__admin/accounts")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d want 403（未配口令时局域网必须被拒）body=%s", rec.Code, rec.Body)
	}
	// 即使带了任意口令也不能过 —— 没有配置就没有正确口令可言。
	rec = callAdmin(h, "192.168.1.50:5555", "anything", "/__admin/accounts")
	if rec.Code != http.StatusForbidden {
		t.Errorf("未配口令时带口令竟被放行: code=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "admin.token") {
		t.Errorf("403 文案未提示如何开启: %s", rec.Body)
	}
}

// TestAdminRemoteTokenRequired 配了口令后：带对放行、带错/不带 401。
func TestAdminRemoteTokenRequired(t *testing.T) {
	const tok = "s3cret-admin-token"
	h := New(Config{Token: tok})

	// 不带口令 → 401（而非 403）：有口令但没带，客户端据此提示"输入口令"。
	rec := callAdmin(h, "192.168.1.50:5555", "", "/__admin/accounts")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("不带口令 code=%d want 401 body=%s", rec.Code, rec.Body)
	}
	// 口令错误 → 401。
	rec = callAdmin(h, "192.168.1.50:5555", "wrong", "/__admin/accounts")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("口令错误 code=%d want 401", rec.Code)
	}
	// 口令正确 → 放行（不再被鉴权层拦；具体结果由业务 handler 决定）。
	rec = callAdmin(h, "192.168.1.50:5555", tok, "/__admin/accounts")
	if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
		t.Errorf("正确口令被拒: code=%d body=%s", rec.Code, rec.Body)
	}
}

// TestAdminTokenComparisonIsExact 口令比较必须是完整匹配，不接受前缀/子串。
//
// 用 == 而非 ConstantTimeCompare 时前缀匹配会通过；这条测试锁住"不接受前缀"
// 这个语义（常量时间性本身无法在单测里断言，但正确性可以）。
func TestAdminTokenComparisonIsExact(t *testing.T) {
	const tok = "abcdef123456"
	h := New(Config{Token: tok})
	for _, bad := range []string{"abcdef", "abcdef1234567", "abcdef12345", "ABCDEF123456", " abcdef123456"} {
		rec := callAdmin(h, "192.168.1.50:5555", bad, "/__admin/accounts")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("口令 %q 被接受（code=%d）—— 比较不是完整匹配", bad, rec.Code)
		}
	}
}

// TestAdminAuthCoversAllPaths 鉴权必须在 mux 之前生效，覆盖每一条管理路径。
//
// 逐条列出而非抽查：新加一个路由时若绕过 ServeHTTP 的鉴权层，
// 这条测试会立刻发现（而不是等到有人从局域网删了账号）。
func TestAdminAuthCoversAllPaths(t *testing.T) {
	const tok = "t"
	paths := []struct{ method, path string }{
		{http.MethodGet, "/__admin/accounts"},
		{http.MethodPost, "/__admin/accounts/import"},
		{http.MethodPost, "/__admin/accounts/delete"},
		{http.MethodPost, "/__admin/accounts/test"},
		{http.MethodGet, "/__admin/loaded"},
		{http.MethodPost, "/__admin/login/start"},
		{http.MethodPost, "/__admin/login/poll"},
		{http.MethodPost, "/__admin/pool/reload"},
		{http.MethodPost, "/__admin/pool/reset"},
		{http.MethodGet, "/__admin/logs"},
		{http.MethodGet, "/__admin/stats"},
		{http.MethodGet, "/__admin/schedule"},
		{http.MethodPost, "/__admin/schedule/run"},
		{http.MethodGet, "/__admin/models"},
		{http.MethodGet, "/__admin/apikeys"},
		{http.MethodPost, "/__admin/apikeys"},
	}
	// 未配口令 + 局域网来源：每条都必须 403，绝不能落到业务 handler。
	h := New(Config{})
	for _, p := range paths {
		r := httptest.NewRequest(p.method, p.path, strings.NewReader("{}"))
		r.RemoteAddr = "192.168.1.50:5555"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s 未鉴权却 code=%d（应在鉴权层被拦）", p.method, p.path, rec.Code)
		}
	}
	// 配了口令但没带：同样每条都必须 401。
	h2 := New(Config{Token: tok})
	for _, p := range paths {
		r := httptest.NewRequest(p.method, p.path, strings.NewReader("{}"))
		r.RemoteAddr = "192.168.1.50:5555"
		rec := httptest.NewRecorder()
		h2.ServeHTTP(rec, r)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s 缺口令却 code=%d（应在鉴权层被拦）", p.method, p.path, rec.Code)
		}
	}
}
