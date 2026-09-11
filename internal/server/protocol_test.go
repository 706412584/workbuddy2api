package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// captureUpstream 与 newFakeUpstream 相同，但额外记录每次请求的 body，
// 用于断言「客户端模型名被映射成了哪个上游模型」。
func captureUpstream(t *testing.T, bodies *[]string) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Body != nil {
				raw, _ := io.ReadAll(r.Body)
				*bodies = append(*bodies, string(raw))
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

// anthropicHandler 构造一个启用协议映射的 Anthropic 测试 handler。
func anthropicHandler(t *testing.T, bodies *[]string) *Handler {
	t.Helper()
	return NewHandler(Config{
		Pool:     testPoolWith(cnAuth("cn", "at-cn")),
		Upstream: captureUpstream(t, bodies),
		Protocol: ProtocolConfig{DefaultModel: "deepseek-v4.1-flash"},
	})
}

// ── Anthropic /v1/messages ──────────────────────────────────────

func TestAnthropicMessagesNonStream(t *testing.T) {
	var bodies []string
	h := anthropicHandler(t, &bodies)

	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5-20250929","max_tokens":256,
		"system":"你是助手","messages":[{"role":"user","content":"你好"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应非 JSON: %v (%s)", err, rec.Body)
	}
	// Anthropic 响应的判别字段是 type=message 与 stop_reason。
	if resp["type"] != "message" {
		t.Errorf("type=%v want message", resp["type"])
	}
	if resp["role"] != "assistant" {
		t.Errorf("role=%v want assistant", resp["role"])
	}
	if _, ok := resp["content"].([]any); !ok {
		t.Errorf("content 应为块数组: %v", resp["content"])
	}
	if _, ok := resp["stop_reason"]; !ok {
		t.Error("缺少 stop_reason")
	}
	if resp["id"] == nil || resp["id"] == "" {
		t.Error("缺少 id")
	}
}

// TestAnthropicMessagesModelMapping 客户端模型名必须被换成配置的上游模型，
// 且区域路由要按上游模型名判断。
func TestAnthropicMessagesModelMapping(t *testing.T) {
	var bodies []string
	h := anthropicHandler(t, &bodies)

	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5-20250929","max_tokens":256,
		"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if len(bodies) == 0 {
		t.Fatal("未捕获到上游请求")
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(bodies[0]), &sent); err != nil {
		t.Fatalf("上游请求非 JSON: %v", err)
	}
	if sent["model"] != "deepseek-v4.1-flash" {
		t.Errorf("上游 model=%v want deepseek-v4.1-flash（映射未生效）", sent["model"])
	}
	// 顶层 system 必须变成 messages 里的 system 条目。
	msgs, _ := sent["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("上游请求缺 messages")
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Errorf("首条 role=%v want system", first["role"])
	}
	// 上游要求 stream=true（网关强制）。
	if sent["stream"] != true {
		t.Errorf("stream=%v want true（上游拒绝非流式）", sent["stream"])
	}
}

// TestAnthropicMessagesStreamEventSequence Claude Code 依赖的事件链必须完整。
func TestAnthropicMessagesStreamEventSequence(t *testing.T) {
	var bodies []string
	h := anthropicHandler(t, &bodies)

	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5","max_tokens":256,"stream":true,
		"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type=%q want text/event-stream", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"event: message_start", "event: content_block_start", "event: content_block_delta",
		"event: content_block_stop", "event: message_delta", "event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("缺少 %q\n实际:\n%s", want, body)
		}
	}
	// message_start 必须在 message_stop 之前。
	if strings.Index(body, "message_start") > strings.Index(body, "message_stop") {
		t.Error("message_start 应在 message_stop 之前")
	}
	// Anthropic 流不用 [DONE] 收尾（那是 OpenAI 的约定）。
	if strings.Contains(body, "[DONE]") {
		t.Error("Anthropic 流不应出现 [DONE]")
	}
}

// TestAnthropicCountTokens 本地估算必须返回正整数，不能报错。
func TestAnthropicCountTokens(t *testing.T) {
	var bodies []string
	h := anthropicHandler(t, &bodies)

	req := httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(`{
		"model":"claude-sonnet-4-5","max_tokens":256,
		"system":"你是助手","messages":[{"role":"user","content":"hello world 你好"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	n, ok := resp["input_tokens"].(float64)
	if !ok || n < 1 {
		t.Errorf("input_tokens=%v want ≥1", resp["input_tokens"])
	}
	// 估算不应调用上游。
	if len(bodies) != 0 {
		t.Errorf("count_tokens 不该访问上游，实际 %d 次", len(bodies))
	}
}

// TestAnthropicRejectsUnsupportedModelRegion 密钥绑定 global + 模型仅 CN 可用 → 404。
// 这是「该模型对此密钥不存在」的正确语义，错误体须为 Anthropic 格式。
func TestAnthropicRejectsUnsupportedModelRegion(t *testing.T) {
	var bodies []string
	h := NewHandler(Config{
		Pool:     testPoolWith(globalAuth("intl", "at-intl")),
		Upstream: captureUpstream(t, &bodies),
		APIKeys:  []APIKeySpec{{Key: "gk", Region: auth.RegionGlobal}},
		// 映射到 CN 独有模型 → 与 global 密钥的区域无交集
		Protocol: ProtocolConfig{DefaultModel: "deepseek-v4-pro"},
	})
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5","max_tokens":256,
		"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer gk")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d want 404 body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["type"] != "error" {
		t.Errorf("type=%v want error（必须是 Anthropic 错误格式）", resp["type"])
	}
	if _, ok := resp["error"].(map[string]any); !ok {
		t.Errorf("缺少 error 对象: %s", rec.Body)
	}
	if len(bodies) != 0 {
		t.Error("区域无交集时不该调用上游")
	}
}

// TestAnthropicNoAccountInModelRegion 模型区域与密钥不冲突、但池中该区域无账号 → 503。
// 与 OpenAI 路径同语义（区别于上面的 404：那是模型对密钥不可见）。
func TestAnthropicNoAccountInModelRegion(t *testing.T) {
	var bodies []string
	h := NewHandler(Config{
		Pool:     testPoolWith(globalAuth("intl", "at-intl")),
		Upstream: captureUpstream(t, &bodies),
		// 未绑定区域的密钥 → 区域仅由模型决定；映射到 CN 独有模型但池里只有 global 号
		Protocol: ProtocolConfig{DefaultModel: "deepseek-v4-pro"},
	})
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5","max_tokens":256,
		"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 body=%s", rec.Code, rec.Body)
	}
	if len(bodies) != 0 {
		t.Error("无可用账号时不该调用上游")
	}
}

// TestAnthropicRejectsEmptyModel 缺 model 是明确的客户端错误。
func TestAnthropicRejectsEmptyModel(t *testing.T) {
	var bodies []string
	h := anthropicHandler(t, &bodies)
	req := httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"max_tokens":256,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", rec.Code)
	}
}

// ── OpenAI /v1/responses ────────────────────────────────────────

func responsesHandler(t *testing.T, bodies *[]string) *Handler {
	t.Helper()
	return NewHandler(Config{
		Pool:     testPoolWith(cnAuth("cn", "at-cn")),
		Upstream: captureUpstream(t, bodies),
		Protocol: ProtocolConfig{DefaultModel: "deepseek-v4.1-flash"},
	})
}

func TestResponsesNonStream(t *testing.T) {
	var bodies []string
	h := responsesHandler(t, &bodies)

	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{
		"model":"gpt-5-codex","instructions":"你是编码助手",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"写个函数"}]}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应非 JSON: %v", err)
	}
	// Responses 的判别字段是 object=response 与 output 数组。
	if resp["object"] != "response" {
		t.Errorf("object=%v want response", resp["object"])
	}
	// output 不能为空数组——严格客户端不接受空 output。
	out, ok := resp["output"].([]any)
	if !ok || len(out) == 0 {
		t.Errorf("output=%v want 非空数组", resp["output"])
	}
	if resp["status"] == nil {
		t.Error("缺少 status")
	}

	var sent map[string]any
	json.Unmarshal([]byte(bodies[0]), &sent)
	if sent["model"] != "deepseek-v4.1-flash" {
		t.Errorf("上游 model=%v want deepseek-v4.1-flash", sent["model"])
	}
	msgs, _ := sent["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("上游请求缺 messages")
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Errorf("首条 role=%v want system（instructions 需前置）", first["role"])
	}
}

// TestResponsesStreamEventOrder Codex 对事件顺序是硬要求：
// delta 必须在其 output_item.added 之后。
func TestResponsesStreamEventOrder(t *testing.T) {
	var bodies []string
	h := responsesHandler(t, &bodies)

	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{
		"model":"gpt-5-codex","stream":true,"input":"hi"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{"event: response.created", "event: response.output_item.added", "event: response.completed"} {
		if !strings.Contains(body, want) {
			t.Errorf("缺少 %q\n实际:\n%s", want, body)
		}
	}
	// added 必须早于首个 delta。
	iAdded := strings.Index(body, "response.output_item.added")
	iDelta := strings.Index(body, "response.output_text.delta")
	if iAdded >= 0 && iDelta >= 0 && iAdded > iDelta {
		t.Errorf("output_item.added 必须早于 output_text.delta（Codex 严格校验）")
	}
}

func TestResponsesRejectsEmptyModel(t *testing.T) {
	var bodies []string
	h := responsesHandler(t, &bodies)
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"input":"hi"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", rec.Code)
	}
}

// TestProtocolEndpointsRequireAuth 两个新端点必须与旧端点一样走鉴权。
func TestProtocolEndpointsRequireAuth(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "cn", AccessToken: "at", ExpiresAt: 9999999999, Domain: "copilot.tencent.com"}),
		Upstream: up,
		APIKey:   "secret",
	})
	for _, path := range []string{"/v1/messages", "/v1/responses", "/v1/messages/count_tokens"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(`{"model":"x","max_tokens":1}`)))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s 无密钥 code=%d want 401", path, rec.Code)
		}
	}
}
