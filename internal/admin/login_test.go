package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newLoginHandler 造一个只接线了 RunLogin 的 handler。
// 注入 RunLogin 后 runLogin/ensureLoginBin 都不会碰真实二进制与网络。
func newLoginHandler(run func(args ...string) (string, string, int, error)) *Handler {
	return New(Config{RunLogin: run})
}

// postLogin 发一个本机请求（绕开 isLocal），body 为空时发空体。
func postLogin(h *Handler, path, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(http.MethodPost, path, nil)
	} else {
		r = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	}
	r.RemoteAddr = "127.0.0.1:12345" // 绕开 isLocal 拦截
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// decodeMap 解响应体为 map，便于断言字段。
func decodeMap(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, rec.Body.String())
	}
	return m
}

// TestLoginStartReturnsSessionID url 子命令要带上生成的 sessionId，
// 且响应把它回给前端 —— 这是并发登录隔离的起点。
func TestLoginStartReturnsSessionID(t *testing.T) {
	var gotArgs []string
	h := newLoginHandler(func(args ...string) (string, string, int, error) {
		gotArgs = args
		return "https://copilot.tencent.com/login?state=abc\n", "", 0, nil
	})

	rec := postLogin(h, "/__admin/login/start", `{"region":"cn"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	m := decodeMap(t, rec)
	if m["authUrl"] != "https://copilot.tencent.com/login?state=abc" {
		t.Errorf("authUrl = %v", m["authUrl"])
	}
	sid, _ := m["sessionId"].(string)
	if !sessionIDRE.MatchString(sid) {
		t.Fatalf("sessionId = %q，不合法", sid)
	}
	// 参数必须原样传到 login 工具：url <region> <sid>
	if len(gotArgs) != 3 || gotArgs[0] != "url" || gotArgs[1] != "cn" || gotArgs[2] != sid {
		t.Errorf("args = %v, want [url cn %s]", gotArgs, sid)
	}
}

// TestLoginStartGeneratesDistinctSessionIDs 两次 start 必须得到不同的 sid，
// 否则并发登录仍会共用 state 文件。
func TestLoginStartGeneratesDistinctSessionIDs(t *testing.T) {
	h := newLoginHandler(func(args ...string) (string, string, int, error) {
		return "https://example.com/login\n", "", 0, nil
	})
	a := decodeMap(t, postLogin(h, "/__admin/login/start", `{"region":"cn"}`))["sessionId"]
	b := decodeMap(t, postLogin(h, "/__admin/login/start", `{"region":"global"}`))["sessionId"]
	if a == b {
		t.Fatalf("两次登录拿到同一个 sessionId: %v", a)
	}
}

// TestLoginStartUpstreamFailure 子进程非 0 → 502，并把 stderr 透出来。
func TestLoginStartUpstreamFailure(t *testing.T) {
	h := newLoginHandler(func(args ...string) (string, string, int, error) {
		return "", "auth state failed: dial tcp: connection refused", 1, nil
	})
	rec := postLogin(h, "/__admin/login/start", `{"region":"cn"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "connection refused") {
		t.Errorf("502 响应应含 stderr 原文: %s", rec.Body.String())
	}
}

// TestLoginPollRequiresSessionID 没带 sessionId 必须回 400 而非无限 pending。
// 这正是「浏览器提示成功、页面一直等待」的修复点：会话不可用要立刻报错。
func TestLoginPollRequiresSessionID(t *testing.T) {
	called := false
	h := newLoginHandler(func(args ...string) (string, string, int, error) {
		called = true
		return "", "", 0, nil
	})
	for _, body := range []string{`{}`, `{"sessionId":""}`} {
		rec := postLogin(h, "/__admin/login/poll", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body=%s: status = %d, want 400", body, rec.Code)
		}
	}
	if called {
		t.Error("sessionId 缺失时不应调用 login 工具")
	}
}

// TestLoginPollRejectsMalformedSessionID 非法 sid 必须被挡在调用之前，
// 因为它会被拼进 state 文件名（路径穿越风险）。
func TestLoginPollRejectsMalformedSessionID(t *testing.T) {
	called := false
	h := newLoginHandler(func(args ...string) (string, string, int, error) {
		called = true
		return "", "", 0, nil
	})
	for _, sid := range []string{"../../etc/passwd", "a/b", "has space", "x.y", strings.Repeat("a", 65)} {
		rec := postLogin(h, "/__admin/login/poll", `{"sessionId":`+mustJSON(t, sid)+`}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("sid=%q: status = %d, want 400", sid, rec.Code)
		}
	}
	if called {
		t.Error("非法 sessionId 时不应调用 login 工具")
	}
}

// TestLoginPollPassesSessionID 合法 sid 必须原样传给 login poll。
func TestLoginPollPassesSessionID(t *testing.T) {
	var gotArgs []string
	h := newLoginHandler(func(args ...string) (string, string, int, error) {
		gotArgs = args
		return "", "登录未完成（waiting for login）", 1, nil
	})
	const sid = "0123456789abcdef0123456789abcdef"
	rec := postLogin(h, "/__admin/login/poll", `{"sessionId":"`+sid+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if len(gotArgs) != 2 || gotArgs[0] != "poll" || gotArgs[1] != sid {
		t.Errorf("args = %v, want [poll %s]", gotArgs, sid)
	}
	m := decodeMap(t, rec)
	if m["status"] != "pending" {
		t.Errorf("status = %v, want pending", m["status"])
	}
	if msg, _ := m["message"].(string); !strings.Contains(msg, "尚未在浏览器完成登录") {
		t.Errorf("message = %q，应映射为「尚未在浏览器完成登录」", msg)
	}
}

// TestLoginPollBusyIsOKNotConflict busy 是「稍后再问」而非错误：
// 必须回 200 + status=busy，否则前端的 adminJSON 会把它当错误抛出并终止轮询。
func TestLoginPollBusyIsOKNotConflict(t *testing.T) {
	h := newLoginHandler(func(args ...string) (string, string, int, error) {
		return "", "", 1, nil
	})
	// 手工占住 pollMu，模拟「上一次查询还没回来」。
	h.pollMu.Lock()
	defer h.pollMu.Unlock()

	rec := postLogin(h, "/__admin/login/poll", `{"sessionId":"deadbeef"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	m := decodeMap(t, rec)
	if m["status"] != "busy" {
		t.Errorf("status = %v, want busy", m["status"])
	}
}

// TestLoginPollSuccessWritesAuthFile 成功路径：落盘凭证 + 热重载 + 回 ok。
func TestLoginPollSuccessWritesAuthFile(t *testing.T) {
	dir := t.TempDir()
	reloaded := 0
	h := New(Config{
		AuthDir:        dir,
		ReloadAccounts: func() (int, error) { reloaded++; return 7, nil },
		RunLogin: func(args ...string) (string, string, int, error) {
			return `{"access_token":"tok","refresh_token":"ref","expires_in":3600,` +
				`"domain":"www.workbuddy.ai","uid":"u-1","enterprise_id":"e-1","nickname":"nick"}` + "\n", "", 0, nil
		},
	})

	rec := postLogin(h, "/__admin/login/poll", `{"sessionId":"deadbeef"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	m := decodeMap(t, rec)
	if m["status"] != "ok" {
		t.Fatalf("status = %v, want ok (%s)", m["status"], rec.Body.String())
	}
	if m["file"] != "workbuddy-u-1.json" {
		t.Errorf("file = %v", m["file"])
	}
	if m["loaded"] != float64(7) {
		t.Errorf("loaded = %v, want 7", m["loaded"])
	}
	if reloaded != 1 {
		t.Errorf("reloadAccounts 调用 %d 次, want 1", reloaded)
	}
	// 凭证文件确实落盘，且是 internal/auth 能解析的嵌套形状。
	if _, err := readAuthFile(filepath.Join(dir, "workbuddy-u-1.json")); err != nil {
		t.Errorf("凭证文件不可解析: %v", err)
	}
}

// readAuthFile 读回刚写入的凭证文件，确认是合法 JSON。
func readAuthFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return raw, nil
}

// mustJSON 把字符串编成 JSON 字面量（用于构造含特殊字符的请求体）。
func mustJSON(t *testing.T, s string) string {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}
