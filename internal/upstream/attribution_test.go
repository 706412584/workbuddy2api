// attribution_test.go 用量归属头 + 客户端 IP 透传单测。
package upstream

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"workbuddy2api/internal/auth"
)

// chatHeadersReq 构造一个 chat 请求并应用 ChatHeaders，发到测试 server，
// 返回 server 捕获到的所有头。便于断言归属/IP 头。
// clientIP 为透传参数（PassthroughIP 开启时注入；空串表示不透传）。
func chatHeadersReq(t *testing.T, c *Client, a *auth.Auth, clientIP string) http.Header {
	t.Helper()
	var captured http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Clone()
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()
	c.ChatBaseCN = srv.URL
	c.ChatHTTP = srv.Client()
	c.HTTP = srv.Client()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v2/chat/completions", nil)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	c.ChatHeaders(req, a, clientIP, "")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	return captured
}

// TestAgentPurposeHeadersSet ClientName 配 WorkBuddy 时四头齐全跟随该值。
func TestAgentPurposeHeadersSet(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	c := &Client{ClientName: "WorkBuddy"}
	h := chatHeadersReq(t, c, a, "")
	for _, tc := range []struct {
		header string
		want   string
	}{
		{"X-Agent-Purpose", "conversation"},
		{"X-IDE-Name", "WorkBuddy"},
		{"X-IDE-Type", "WorkBuddy"},
		{"X-Product", "WorkBuddy"},
	} {
		if got := h.Get(tc.header); got != tc.want {
			t.Errorf("%s = %q want %q", tc.header, got, tc.want)
		}
	}
}

// TestAttributionIncludesIDEVersion client_name + client_version 都配时，
// X-IDE-* 四头齐全（Name/Type/Product 跟随 client_name，Version 跟随 client_version）。
func TestAttributionIncludesIDEVersion(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	c := &Client{ClientName: "WorkBuddy", ClientVersion: "6.0.0"}
	h := chatHeadersReq(t, c, a, "")
	for _, tc := range []struct {
		header string
		want   string
	}{
		{"X-Agent-Purpose", "conversation"},
		{"X-IDE-Name", "WorkBuddy"},
		{"X-IDE-Type", "WorkBuddy"},
		{"X-IDE-Version", "6.0.0"},
		{"X-Product", "WorkBuddy"},
	} {
		if got := h.Get(tc.header); got != tc.want {
			t.Errorf("%s = %q want %q", tc.header, got, tc.want)
		}
	}
	// X-IDE-Version 缺省（client_version 空）= 内置默认 5.8.4，且四头齐全。
	c2 := &Client{ClientName: "WorkBuddy"}
	h2 := chatHeadersReq(t, c2, a, "")
	if got := h2.Get("X-IDE-Version"); got != "5.8.4" {
		t.Errorf("X-IDE-Version = %q want %q (default)", got, "5.8.4")
	}
	// ClientName 空时 X-IDE-Version 仍为内置默认——2026-10-21 起 IDE 头无条件发送
	// （对齐官方 applyGatewayHeaders 的常量组），不再随 client_name 开关。
	c3 := &Client{}
	h3 := chatHeadersReq(t, c3, a, "")
	if got := h3.Get("X-IDE-Version"); got != "5.8.4" {
		t.Errorf("X-IDE-Version = %q want %q (unconditional)", got, "5.8.4")
	}
}

// TestProductDefaultSaaS ClientName 空（缺省）时 X-Product=SaaS，
// 但 X-IDE-* / X-Agent-Purpose 仍然发送（2026-10-21 起对齐官方常量组，无条件注入）。
func TestProductDefaultSaaS(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	c := &Client{} // ClientName 空
	h := chatHeadersReq(t, c, a, "")
	if got := h.Get("X-Product"); got != "SaaS" {
		t.Errorf("X-Product = %q want %q", got, "SaaS")
	}
	// 官方客户端无条件带这三头，缺任何一个都是可区分的特征差异。
	for _, tc := range []struct{ hdr, want string }{
		{"X-IDE-Name", "WorkBuddy"},
		{"X-IDE-Type", "WorkBuddy"},
		{"X-Agent-Purpose", "conversation"},
	} {
		if got := h.Get(tc.hdr); got != tc.want {
			t.Errorf("%s = %q want %q (unconditional)", tc.hdr, got, tc.want)
		}
	}
}

// TestProductWorkBuddy_WhenConfigured client_name=WorkBuddy 时 X-Product 跟随（等价覆盖首测，保独立命名）。
func TestProductWorkBuddy_WhenConfigured(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	c := &Client{ClientName: "WorkBuddy"}
	h := chatHeadersReq(t, c, a, "")
	if got := h.Get("X-Product"); got != "WorkBuddy" {
		t.Errorf("X-Product = %q want %q", got, "WorkBuddy")
	}
	// client_name 其他值也应跟随。
	c2 := &Client{ClientName: "MyEditor"}
	h2 := chatHeadersReq(t, c2, a, "")
	if got := h2.Get("X-Product"); got != "MyEditor" {
		t.Errorf("X-Product = %q want %q", got, "MyEditor")
	}
}

// TestIPNotForwarded_ByDefault PassthroughIP 缺省 false：即使传入非空 clientIP 也不注入任何 IP 头。
func TestIPNotForwarded_ByDefault(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	// 即使 clientIP 参数非空，PassthroughIP 关闭也不透传。
	c := &Client{}
	h := chatHeadersReq(t, c, a, "10.0.0.1")
	for _, hdr := range []string{"X-Forwarded-For", "X-Real-IP", "X-Client-IP"} {
		if got := h.Get(hdr); got != "" {
			t.Errorf("%s = %q want empty (passthrough off)", hdr, got)
		}
	}
}

// TestIPForwarded_WhenEnabled PassthroughIP=true 且 clientIP 参数非空时三头透传。
func TestIPForwarded_WhenEnabled(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	c := &Client{PassthroughIP: true}
	h := chatHeadersReq(t, c, a, "203.0.113.5")
	for _, hdr := range []string{"X-Forwarded-For", "X-Real-IP", "X-Client-IP"} {
		if got := h.Get(hdr); got != "203.0.113.5" {
			t.Errorf("%s = %q want %q", hdr, got, "203.0.113.5")
		}
	}
}

// TestIPForwarded_NoLeakWhenClientIPEmpty PassthroughIP=true 但 clientIP 为空时不注入 IP 头。
func TestIPForwarded_NoLeakWhenClientIPEmpty(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	c := &Client{PassthroughIP: true}
	h := chatHeadersReq(t, c, a, "")
	for _, hdr := range []string{"X-Forwarded-For", "X-Real-IP", "X-Client-IP"} {
		if got := h.Get(hdr); got != "" {
			t.Errorf("%s = %q want empty (no clientIP)", hdr, got)
		}
	}
}

// TestClientIPConcurrentNoCrossTalk 并发 goroutine 各自带不同 clientIP 调 ChatHeaders，
// 断言每个请求捕获到的 IP 头严格等于自己的 IP——验证按请求传递后无跨请求串扰
// （旧实现读写共享 ClientIP 字段会张冠李戴，-race 下暴露竞态）。
func TestClientIPConcurrentNoCrossTalk(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	c := &Client{PassthroughIP: true}

	// 共享一个测试 server：所有 goroutine 打到同一 server，各自捕获请求头。
	var mu sync.Mutex
	captured := map[string]string{} // clientIP -> 实际捕获到的 X-Forwarded-For
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		captured[r.Header.Get("X-Forwarded-For")] = r.Header.Get("X-Forwarded-For")
		mu.Unlock()
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()
	c.ChatBaseCN = srv.URL
	c.ChatHTTP = srv.Client()
	c.HTTP = srv.Client()

	ips := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5"}
	var wg sync.WaitGroup
	for _, ip := range ips {
		wg.Add(1)
		go func(myIP string) {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/v2/chat/completions", nil)
			if err != nil {
				t.Errorf("new req: %v", err)
				return
			}
			// 每个请求独立构造头：clientIP 作为参数传入，不存在共享可污染。
			c.ChatHeaders(req, a, myIP, "")
			resp, err := c.HTTP.Do(req)
			if err != nil {
				t.Errorf("do: %v (ip=%s)", err, myIP)
				return
			}
			resp.Body.Close()
		}(ip)
	}
	wg.Wait()

	// 每个 IP 都应被至少一次请求携带到上游（出现在 captured 里）。
	for _, ip := range ips {
		if _, ok := captured[ip]; !ok {
			t.Errorf("clientIP %q never reached upstream (cross-talk/lost)", ip)
		}
	}
}

// TestExtractClientIP 从入站请求取 X-Forwarded-For 首段，回落 X-Real-IP，皆空返回空。
func TestExtractClientIP(t *testing.T) {
	for _, tc := range []struct {
		name string
		xff  string
		real string
		want string
	}{
		{"xff_single", "1.2.3.4", "", "1.2.3.4"},
		{"xff_multi_first", "1.2.3.4, 5.6.7.8", "", "1.2.3.4"},
		{"real_fallback", "", "9.9.9.9", "9.9.9.9"},
		{"none_empty", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, "http://x", nil)
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			if tc.real != "" {
				req.Header.Set("X-Real-IP", tc.real)
			}
			if got := ExtractClientIP(req); got != tc.want {
				t.Errorf("ExtractClientIP = %q want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 客户端指纹头（官方 CLI 对所有出站请求无条件注入，2026-10-21 补齐）
// ---------------------------------------------------------------------------

// TestClientFingerprintHeadersPresent 官方客户端无条件发送的一批头必须齐全。
//
// 回归背景：2026-10-20 起上游按账号灰度返回 403 code=11140 "request illegal"
// （displayMsg「账号存在异常行为，已被限制使用」）。排查证实是**账号级**封控
// （同套请求头下健康账号 200、被封账号 403；换出口 IP 与换 UA 版本号均无效），
// 但对照客户端 5.8.4 发现我们缺了一整批 CLI 统一拦截器注入的头——
// 这是"请求不像官方客户端"最明显的缺口，故补齐作预防（见 injectClientFingerprint）。
func TestClientFingerprintHeadersPresent(t *testing.T) {
	c := &Client{}
	a := &auth.Auth{UID: "u1", AccessToken: "at", EnterpriseID: "ent-1"}
	req, _ := http.NewRequest("POST", "https://example.com/v2/chat/completions", nil)
	c.ChatHeaders(req, a, "", "")

	for _, tc := range []struct{ hdr, want string }{
		{"x-codebuddy-request", "1"},
		{"X-Agent-Intent", "craft"},
		{"X-Agent-Type", "main"},
		{"X-Private-Data", "true"},
		{"X-Tenant-Id", "ent-1"},
	} {
		if got := req.Header.Get(tc.hdr); got != tc.want {
			t.Errorf("%s = %q want %q", tc.hdr, got, tc.want)
		}
	}
	// X-Request-ID：官方是 UUID 去掉连字符（32 hex）。
	if got := req.Header.Get("X-Request-ID"); len(got) != 32 {
		t.Errorf("X-Request-ID = %q want 32 hex chars", got)
	}
	// 链路追踪头：traceparent / b3 / X-B3-* / X-Trace-ID 齐全且 traceId 一致。
	tid := req.Header.Get("X-Trace-ID")
	if len(tid) != 32 {
		t.Fatalf("X-Trace-ID = %q want 32 hex chars", tid)
	}
	sid := req.Header.Get("X-B3-SpanId")
	if len(sid) != 16 {
		t.Errorf("X-B3-SpanId = %q want 16 hex chars", sid)
	}
	for _, tc := range []struct{ hdr, want string }{
		{"traceparent", "00-" + tid + "-" + sid + "-01"},
		{"b3", tid + "-" + sid + "-1"},
		{"X-B3-TraceId", tid},
		{"X-B3-Sampled", "1"},
	} {
		if got := req.Header.Get(tc.hdr); got != tc.want {
			t.Errorf("%s = %q want %q", tc.hdr, got, tc.want)
		}
	}
}

// TestTraceHeadersVaryPerRequest 追踪头表达"本次请求"，两次请求不应相同。
func TestTraceHeadersVaryPerRequest(t *testing.T) {
	c := &Client{}
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	tids := map[string]bool{}
	for i := 0; i < 5; i++ {
		req, _ := http.NewRequest("POST", "https://example.com/v2/chat/completions", nil)
		c.ChatHeaders(req, a, "", "")
		tids[req.Header.Get("X-Trace-ID")] = true
	}
	if len(tids) != 5 {
		t.Errorf("5 次请求产生 %d 个不同 traceId，期望 5（不应复用）", len(tids))
	}
}

// ---------------------------------------------------------------------------
// 会话三头（上游 2026-10-16 起强制校验，缺失回 400 code=11-128）
// ---------------------------------------------------------------------------

// TestConversationHeadersAlwaysPresent 三个会话头必须始终存在且非空。
//
// 回归背景：上游 2026-10-16 01:21 起对 /v2/chat/completions 新增强制校验，
// 三个头缺一不可（实测：无 ID 头 → "conversation id missing"；
// 有 ID 缺其余 → "request illegal"；三头齐全 → 200）。此前网关完全不发这三头，
// 导致全池 400，而同一账号在 IDE 里正常（IDE 发三头）。
func TestConversationHeadersAlwaysPresent(t *testing.T) {
	c := &Client{}
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	req, _ := http.NewRequest("POST", "https://example.com/v2/chat/completions", nil)
	c.ChatHeaders(req, a, "", "")

	for _, h := range []string{
		"X-Conversation-ID",
		"X-Conversation-Request-ID",
		"X-Conversation-Message-ID",
	} {
		if v := req.Header.Get(h); v == "" {
			t.Errorf("头 %s 缺失或为空（上游会回 400 code=11-128）", h)
		}
	}
}

// TestConversationIDUsesProvidedValue 传入的 convID 用作 X-Conversation-ID，
// 使同一会话在上游看到稳定 ID（会话粘性对齐）。
func TestConversationIDUsesProvidedValue(t *testing.T) {
	c := &Client{}
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	req, _ := http.NewRequest("POST", "https://example.com/v2/chat/completions", nil)
	c.ChatHeaders(req, a, "", "conv-abc-123")

	if got := req.Header.Get("X-Conversation-ID"); got != "conv-abc-123" {
		t.Errorf("X-Conversation-ID=%q want conv-abc-123", got)
	}
}

// TestConversationIDsAreIndependent Request-ID 与 Message-ID 各自独立生成
// （上游不校验三者关系，但同一请求内不应出现重复值）。
func TestConversationIDsAreIndependent(t *testing.T) {
	c := &Client{}
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	req, _ := http.NewRequest("POST", "https://example.com/v2/chat/completions", nil)
	c.ChatHeaders(req, a, "", "conv-fixed")

	cid := req.Header.Get("X-Conversation-ID")
	rid := req.Header.Get("X-Conversation-Request-ID")
	mid := req.Header.Get("X-Conversation-Message-ID")
	if rid == mid {
		t.Error("Request-ID 与 Message-ID 不应相同")
	}
	if rid == cid || mid == cid {
		t.Error("Request-ID/Message-ID 不应复用 Conversation-ID")
	}
}

// TestNewConversationIDShape 生成的 ID 为 UUID v4 形态且每次不同。
func TestNewConversationIDShape(t *testing.T) {
	a, b := newConversationID(), newConversationID()
	if a == b {
		t.Error("两次生成不应相同")
	}
	if len(a) != 36 {
		t.Errorf("长度=%d want 36（UUID 形态）", len(a))
	}
	// 版本位与变体位：第 15 个字符为 '4'，第 20 个字符属 [89ab]
	if a[14] != '4' {
		t.Errorf("version nibble=%c want '4'", a[14])
	}
	if !strings.ContainsRune("89ab", rune(a[19])) {
		t.Errorf("variant nibble=%c want 8/9/a/b", a[19])
	}
}
