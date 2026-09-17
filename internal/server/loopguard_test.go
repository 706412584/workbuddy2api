// loopguard_test.go 思考死循环判据与提示注入的单元测试。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// TestLoopGuardDetect 覆盖判据的三个必要条件与各自的边界。
//
// 判据（缺一不可）：思考字符超阈值、正文为 0、唯一块占比低。
// 任何一条不满足都必须放过 —— 误杀正常长思考的代价比漏判大得多
// （漏判只损失一次请求，误杀会让正常请求无谓重试甚至报错）。
func TestLoopGuardDetect(t *testing.T) {
	g := DefaultLoopGuard()
	cases := []struct {
		name                         string
		think, text, distinct, total int
		elapsed                      time.Duration
		want                         bool
	}{
		{"命中：思考超阈值+无正文+高重复", 500_000, 0, 10, 20_000, time.Minute, true},
		{"思考不足阈值", 399_999, 0, 10, 20_000, time.Minute, false},
		{"有正文输出", 500_000, 1, 10, 20_000, time.Minute, false},
		{"占比不够低（正常行文）", 500_000, 0, 15_000, 20_000, time.Minute, false},
		{"耗时不足（刚开跑）", 500_000, 0, 10, 20_000, 10 * time.Second, false},
		{"无块（还没切出完整块）", 500_000, 0, 0, 0, time.Minute, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := g.Detect(c.think, c.text, c.distinct, c.total, c.elapsed)
			if got != c.want {
				t.Errorf("Detect(%d,%d,%d,%d,%v)=%v want %v",
					c.think, c.text, c.distinct, c.total, c.elapsed, got, c.want)
			}
		})
	}
}

// TestLoopGuardDisabled 关闭开关后一律不命中（保留原有行为）。
func TestLoopGuardDisabled(t *testing.T) {
	g := DefaultLoopGuard()
	g.Enabled = false
	if g.Detect(1_000_000, 0, 1, 100_000, time.Hour) {
		t.Error("Enabled=false 仍命中")
	}
}

// TestInjectLoopNudge 验证提示追加到 messages 末尾，且不改动既有消息。
func TestInjectLoopNudge(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	out := InjectLoopNudge(body)

	var obj struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("注入后 body 不可解析: %v", err)
	}
	if len(obj.Messages) != 2 {
		t.Fatalf("messages 长度=%d want 2", len(obj.Messages))
	}
	// 原消息逐字保留：重试请求除末尾提示外与原始请求一致，便于对照排查。
	if obj.Messages[0].Role != "user" || obj.Messages[0].Content != "hi" {
		t.Errorf("原消息被改动: %+v", obj.Messages[0])
	}
	if obj.Messages[1].Role != "system" {
		t.Errorf("追加消息 role=%q want system", obj.Messages[1].Role)
	}
	if obj.Messages[1].Content != loopNudgeText {
		t.Errorf("追加消息内容不符: %q", obj.Messages[1].Content)
	}
	// 其余字段原样保留（model/stream 是转发路径依赖的）。
	if obj.Model != "deepseek-v4.1-flash" || !obj.Stream {
		t.Errorf("model/stream 被改动: model=%q stream=%v", obj.Model, obj.Stream)
	}
}

// TestInjectLoopNudgeBadBody 坏 body 原样返回，不二次错误化（与 prepareBody 同口径）。
func TestInjectLoopNudgeBadBody(t *testing.T) {
	bad := []byte(`{not json`)
	if got := InjectLoopNudge(bad); string(got) != string(bad) {
		t.Errorf("坏 body 被改动: %q", got)
	}
	if got := InjectLoopNudge(nil); got != nil {
		t.Errorf("空 body 被改动: %q", got)
	}
}

// TestLoopGuardErrTripsOnce 命中后 loopTripped 保证只报一次（Stream 逐帧回调）。
func TestLoopGuardErrTripsOnce(t *testing.T) {
	g := DefaultLoopGuard()
	r := &chatStatsReader{start: time.Now().Add(-time.Minute), guard: &g}
	// 直接构造判据已满足的统计量：40 万个字符、无正文、全是同一块。
	r.thinkChars = 500_000
	r.textChars = 0
	r.chunkSeen = map[string]struct{}{"x": {}}
	r.chunkTotal = 20_000

	if err := r.LoopGuardErr(); err == nil {
		t.Fatal("命中条件满足却未报错")
	}
	if err := r.LoopGuardErr(); err != nil {
		t.Errorf("第二次调用应返回 nil（已报过），得到 %v", err)
	}
}

// TestLoopGuardErrNilGuard 未装配判据时零开销返回 nil。
func TestLoopGuardErrNilGuard(t *testing.T) {
	r := &chatStatsReader{start: time.Now().Add(-time.Hour)}
	r.thinkChars = 1_000_000
	if err := r.LoopGuardErr(); err != nil {
		t.Errorf("guard=nil 时返回 %v want nil", err)
	}
}

// TestLoopGuardConfigToGuard 锁定配置 → 运行时判据的转换：
// 零值/负值一律回落默认（"未设置"语义），显式值生效。
//
// 这条链断了的表现是「配置改了不生效」或「默认阈值变 0 → 所有请求都被误杀」，
// 两种都不会有编译错误，只能靠测试发现。
func TestLoopGuardConfigToGuard(t *testing.T) {
	// Enabled=false → 完全不检测。
	if g := (LoopGuardConfig{}).toLoopGuard(); g != nil {
		t.Errorf("Enabled=false 应返回 nil，得到 %+v", g)
	}

	// 只开开关：其余字段全是零值 → 全部回落默认。
	g := LoopGuardConfig{Enabled: true}.toLoopGuard()
	if g == nil {
		t.Fatal("Enabled=true 必须返回判据")
	}
	def := DefaultLoopGuard()
	if g.MinThinkChars != def.MinThinkChars || g.MaxDistinctRatio != def.MaxDistinctRatio ||
		g.MinElapsed != def.MinElapsed || g.MaxRetries != def.MaxRetries {
		t.Errorf("零值未回落默认: got %+v want %+v", *g, def)
	}

	// 显式值覆盖默认。
	g = LoopGuardConfig{
		Enabled:           true,
		MinThinkChars:     123_456,
		MaxDistinctRatio:  0.42,
		MinElapsedSeconds: 7,
		MaxRetries:        5,
	}.toLoopGuard()
	if g.MinThinkChars != 123_456 || g.MaxDistinctRatio != 0.42 ||
		g.MinElapsed != 7*time.Second || g.MaxRetries != 5 {
		t.Errorf("显式配置未生效: %+v", *g)
	}

	// 负值同样回落默认（"未设置"语义），不得变成"阈值 0 → 全部命中"。
	g = LoopGuardConfig{Enabled: true, MinThinkChars: -1, MaxRetries: -3}.toLoopGuard()
	if g.MinThinkChars != def.MinThinkChars || g.MaxRetries != def.MaxRetries {
		t.Errorf("负值未回落默认: %+v", *g)
	}
}

// TestLoopGuardMaxRetriesFallback maxRetries() 对未装配/零值判据的兜底。
func TestLoopGuardMaxRetriesFallback(t *testing.T) {
	if got := (LoopGuard{}).maxRetries(); got != maxLoopRetries {
		t.Errorf("零值 maxRetries()=%d want %d", got, maxLoopRetries)
	}
	if got := (LoopGuard{MaxRetries: 4}).maxRetries(); got != 4 {
		t.Errorf("maxRetries()=%d want 4", got)
	}
}

// loopSSE 构造一段「思考空转」的 SSE：正文零输出、同一短语反复，
// 并显式以 [DONE] 收尾 —— 用来证明切断发生在流自然结束之前。
func loopSSE(n int) string {
	var sb strings.Builder
	b, _ := json.Marshal("1 let me check the file 2 look at the code 3 me verify this ")
	for i := 0; i < n; i++ {
		sb.WriteString(`data: {"choices":[{"delta":{"reasoning_content":`)
		sb.Write(b)
		sb.WriteString(`}}]}` + "\n\n")
	}
	sb.WriteString("data: [DONE]\n\n")
	return sb.String()
}

// testLoopHandler 构造「判据极易命中」的 handler：阈值压到几百字符、耗时门槛归零，
// 于是测试用不着造 40 万字符的流。判据本身由 TestLoopGuardDetect 单独覆盖。
//
// MinElapsed 直接改运行时判据而非走配置：配置的秒级字段 0 表示「用默认 30s」，
// 表达不了「不设耗时门槛」，而测试的流在毫秒内结束。
func testLoopHandler(t *testing.T, up *upstream.Client, p *pool.Pool, maxRetries int) *Handler {
	t.Helper()
	h := NewHandler(Config{
		Pool:     p,
		Upstream: up,
		LoopGuard: LoopGuardConfig{
			Enabled:          true,
			MinThinkChars:    500,
			MaxDistinctRatio: 0.15,
			MaxRetries:       maxRetries,
		},
	})
	if h.loopGuard == nil {
		t.Fatal("loopGuard 未装配（Enabled=true 时必须有判据）")
	}
	h.loopGuard.MinElapsed = 0
	return h
}

// TestChatThinkingLoopRetriesThenSucceeds 端到端锁定用户要的处置链路：
// 命中思考空转 → 切断 → 追加 system 提示 → 换号重发 → 成功。
//
// 三条断言各自对应一个设计点，缺一条都不算实现正确：
//   - 第一次响应不带 [DONE]：带了客户端会认为本轮已结束、停止读取，重试内容送不到；
//   - 第二次请求体多一条 system 提示：这是「提示模型别重复」的载体；
//   - 最终响应含 [DONE] 且只有一份正文：客户端拿到的是完整可解析的流。
func TestChatThinkingLoopRetriesThenSucceeds(t *testing.T) {
	var (
		mu        sync.Mutex
		nCalls    int
		secondMsg []map[string]any
	)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		mu.Lock()
		defer mu.Unlock()
		nCalls++
		if nCalls == 1 {
			return 200, loopSSE(200), true
		}
		return 200, sseOK, true
	})
	// 捕获第二次请求体，验证提示注入。
	up.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		nCalls++
		first := nCalls == 1
		if !first {
			raw, _ := io.ReadAll(r.Body)
			var obj struct {
				Messages []map[string]any `json:"messages"`
			}
			_ = json.Unmarshal(raw, &obj)
			secondMsg = obj.Messages
		}
		mu.Unlock()
		body, isStream := loopSSE(200), true
		if !first {
			body, isStream = sseOK, true
		}
		ct := "application/json"
		if isStream {
			ct = "text/event-stream"
		}
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{ct}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}

	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := testLoopHandler(t, up, p, 2)

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "你好") {
		t.Errorf("重试后的正文未送达客户端")
	}
	// [DONE] 必须恰好一份，且属于最终成功的那一轮。
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] 出现 %d 次 want 1（切断轮不得写 [DONE]，否则客户端提前收尾）", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if nCalls != 2 {
		t.Fatalf("上游调用 %d 次 want 2（命中一次 + 重试一次）", nCalls)
	}
	// 提示注入到**上游请求**（对客户端不可见）：末尾一条 system，且是循环提示原文。
	if len(secondMsg) == 0 {
		t.Fatal("未捕获到第二次请求体")
	}
	last := secondMsg[len(secondMsg)-1]
	if last["role"] != "system" || last["content"] != loopNudgeText {
		t.Errorf("第二次请求末尾消息=%v want role=system content=循环提示", last)
	}
}

// TestChatThinkingLoopExhaustsRetriesReturns503 重试用尽必须回明确错误，
// 而不是让客户端无限等待（这正是要修的症状：挂 15 分钟、产出为零）。
func TestChatThinkingLoopExhaustsRetriesReturns503(t *testing.T) {
	var nCalls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		nCalls++
		return 200, loopSSE(200), true
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	// maxRetries=1：命中一次后用尽 → 第 2 次命中即回错误。
	h := testLoopHandler(t, up, p, 1)

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "thinking_loop") {
		t.Errorf("未回明确错误码: code=%d body=%q", rec.Code, body)
	}
	if strings.Contains(body, "data: [DONE]") {
		t.Errorf("错误路径不应写 [DONE]: %q", body)
	}
	if nCalls != 2 {
		t.Errorf("上游调用 %d 次 want 2（首次 + 1 次重试）", nCalls)
	}
}

// TestChatThinkingLoopDisabledNoIntervention 判据关闭时保持原有行为：
// 流照常透传、不被切断、不重试。
func TestChatThinkingLoopDisabledNoIntervention(t *testing.T) {
	var nCalls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		nCalls++
		return 200, loopSSE(200), true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		// LoopGuard 零值 → Enabled=false → 不检测。
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if nCalls != 1 {
		t.Errorf("上游调用 %d 次 want 1（关闭判据不得重试）", nCalls)
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Errorf("关闭判据时流应正常收尾")
	}
}

// TestChatThinkingLoopSmallPoolStillReportsLoop 账号池比重试预算小时，
// 轮换预算（MaxRotate=3）会先耗尽 —— 出口必须仍回 thinking_loop，
// 而不是「no_healthy_account（账号不可用：不存在 / 冷却中 / 已禁用）」。
//
// 这条路径此前会被通用文案吞掉：把客户端引去查账号池，而真正的故障在模型侧空转。
func TestChatThinkingLoopSmallPoolStillReportsLoop(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, loopSSE(200), true
	})
	// 单账号池：轮换 1 次就无号可选（MaxRotate=3 但池里只有一个号）。
	h := testLoopHandler(t, up, testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
	), 2)

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "thinking_loop") {
		t.Errorf("单账号池应仍报 thinking_loop: code=%d body=%q", rec.Code, body)
	}
	if strings.Contains(body, "no_healthy_account") {
		t.Errorf("被通用文案吞掉（把排查方向引向账号池）: %q", body)
	}
}

// TestAnthropicThinkingLoopRetries 覆盖 Anthropic 协议的循环守卫。
//
// 这条路径的转发不走 upstream.Stream，而是 relayAnthropicStream 经 chatChunkReader
// 逐帧转换，中断检查也在那边 —— 两处实现若不同步，此协议下守卫会静默失效。
func TestAnthropicThinkingLoopRetries(t *testing.T) {
	var (
		mu     sync.Mutex
		nCalls int
	)
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			mu.Lock()
			nCalls++
			first := nCalls == 1
			mu.Unlock()
			body := sseOK
			if first {
				body = loopSSE(200)
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	h := testLoopHandler(t, up, testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	), 2)

	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-5-20250929","max_tokens":256,"stream":true,
		"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	// 重试成功后正文与收尾事件都必须送达（message_stop 缺了客户端会一直等）。
	if !strings.Contains(body, "你好") {
		t.Errorf("重试后的正文未送达")
	}
	if !strings.Contains(body, "message_stop") {
		t.Errorf("缺收尾事件 message_stop（客户端会挂起）")
	}
	mu.Lock()
	defer mu.Unlock()
	if nCalls != 2 {
		t.Errorf("上游调用 %d 次 want 2（命中一次 + 重试一次）", nCalls)
	}
}

// TestResponsesThinkingLoopRetries 覆盖 Responses 协议的循环守卫（同 Anthropic 的理由）。
func TestResponsesThinkingLoopRetries(t *testing.T) {
	var (
		mu     sync.Mutex
		nCalls int
	)
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			mu.Lock()
			nCalls++
			first := nCalls == 1
			mu.Unlock()
			body := sseOK
			if first {
				body = loopSSE(200)
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	h := testLoopHandler(t, up, testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	), 2)

	req := httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"input":"hi"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "你好") {
		t.Errorf("重试后的正文未送达")
	}
	// Codex 靠 response.completed 判定本轮结束；缺了它会把未完成的工具调用写进历史。
	if !strings.Contains(body, "response.completed") {
		t.Errorf("缺收尾事件 response.completed")
	}
	mu.Lock()
	defer mu.Unlock()
	if nCalls != 2 {
		t.Errorf("上游调用 %d 次 want 2（命中一次 + 重试一次）", nCalls)
	}
}

// TestChatChunkReaderAbortsOnLoop chatChunkReader.next 必须把中断信号透出，
// 否则 Anthropic / Responses 两条协议适配路径的循环守卫形同虚设
// （它们的转发走 chatChunkReader，不经过 upstream.Stream 的读循环）。
func TestChatChunkReaderAbortsOnLoop(t *testing.T) {
	g := DefaultLoopGuard()
	phrase := "1 let me check the file 2 look at the code 3 me verify this "
	var sb strings.Builder
	for i := 0; i < 20_000; i++ {
		sb.WriteString(`data: {"choices":[{"delta":{"reasoning_content":`)
		b, _ := json.Marshal(phrase)
		sb.Write(b)
		sb.WriteString(`}}]}` + "\n\n")
	}
	sb.WriteString("data: [DONE]\n\n")

	stats := newChatStatsReaderSince(strings.NewReader(sb.String()), time.Now().Add(-time.Minute))
	stats.SetLoopGuard(&g)
	reader := newChatChunkReader(stats)

	var got error
	for i := 0; i < 100_000; i++ {
		if _, err := reader.next(); err != nil {
			got = err
			break
		}
	}
	if got != ErrThinkingLoop {
		t.Fatalf("读到 %v want ErrThinkingLoop（中断未透出）", got)
	}
}
