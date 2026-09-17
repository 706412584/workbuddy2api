// loopguard_test.go 思考死循环判据与提示注入的单元测试。
package server

import (
	"encoding/json"
	"fmt"
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

// TestLoopGuardDetect 覆盖判据的边界。
//
// 共同前提：正文为 0、样本足够、耗时够长。满足前提后两条判据取其一即命中：
//   - 停滞：连续 MaxStaleChunks 块无新内容（主判据，与周期长度无关）
//   - 占比：唯一块占比低（兜底，覆盖新块偶发出现的慢漂移）
//
// 任一前提不满足都必须放过 —— 误杀正常长思考的代价比漏判大得多
// （漏判只损失一次请求，误杀会让正常请求无谓重试甚至报错）。
func TestLoopGuardDetect(t *testing.T) {
	g := DefaultLoopGuard()
	cases := []struct {
		name                                string
		think, text, stale, distinct, total int
		elapsed                             time.Duration
		want                                bool
	}{
		{"命中·停滞：连续无新块", 500_000, 0, 2000, 500, 20_000, time.Minute, true},
		{"命中·占比：新块偶发但整体打转", 500_000, 0, 100, 10, 20_000, time.Minute, true},
		{"有正文输出", 500_000, 1, 2000, 500, 20_000, time.Minute, false},
		{"样本不足下限", 29_999, 0, 2000, 500, 20_000, time.Minute, false},
		{"耗时不足（刚开跑）", 500_000, 0, 2000, 500, 20_000, 10 * time.Second, false},
		{"无块（还没切出完整块）", 500_000, 0, 0, 0, 0, time.Minute, false},
		{"正常行文：新块持续出现且占比高", 500_000, 0, 0, 15_000, 20_000, time.Minute, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := g.Detect(c.think, c.text, c.stale, c.distinct, c.total, c.elapsed)
			if got != c.want {
				t.Errorf("Detect(think=%d,text=%d,stale=%d,distinct=%d,total=%d,%v)=%v want %v",
					c.think, c.text, c.stale, c.distinct, c.total, c.elapsed, got, c.want)
			}
		})
	}
}

// TestLoopGuardStallWindowBoundary 停滞窗口的临界值：等于窗口即命中、差一块不命中。
// 边界写错（>= 写成 >）会让检测永远晚一个块，且没有其它测试能发现。
//
// 用 distinct == total（占比 1.0，即"每个块都是新的"）隔离出停滞这一条判据 ——
// 否则占比判据会抢先命中，边界就测不到了。
func TestLoopGuardStallWindowBoundary(t *testing.T) {
	g := DefaultLoopGuard()
	const total = 20_000
	if g.Detect(500_000, 0, g.MaxStaleChunks-1, total, total, time.Minute) {
		t.Error("停滞数 = 窗口-1 不应命中")
	}
	if !g.Detect(500_000, 0, g.MaxStaleChunks, total, total, time.Minute) {
		t.Error("停滞数 = 窗口 应命中")
	}
}

// TestLoopGuardDisabled 关闭开关后一律不命中（保留原有行为）。
func TestLoopGuardDisabled(t *testing.T) {
	g := DefaultLoopGuard()
	g.Enabled = false
	if g.Detect(1_000_000, 0, 100_000, 1, 100_000, time.Hour) {
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
		MaxStaleChunks:    777,
	}.toLoopGuard()
	if g.MinThinkChars != 123_456 || g.MaxDistinctRatio != 0.42 ||
		g.MinElapsed != 7*time.Second || g.MaxRetries != 5 || g.MaxStaleChunks != 777 {
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

// testLoopHandler 构造「判据极易命中」的 handler：字符下限压到几百、耗时门槛归零，
// 于是测试用不着造 48KB 的流。判据本身由 TestLoopGuardDetect / 停滞用例单独覆盖。
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

// loopSSEPeriod 构造一段长周期循环的 SSE：先生成 periodBytes 的唯一内容作周期，
// 再把它重复到 totalBytes。
//
// 为什么要长周期：短周期（如 60 字节）下唯一块占比会瞬间跌到 0.01，占比判据一早就
// 命中，测不出停滞判据。实测空转的周期是 7–53KB —— 一整段推理文本在打转，
// 不是单个短语在重复。本函数复现的正是这个形态。
//
// pieceSize 是每帧携带的字节数，便于用「帧数 × pieceSize」反推已送出的字符数。
// periodBytes 需为 pieceSize 的整数倍，这样按帧切片不会在周期边界上错位。
func loopSSEPeriod(periodBytes, totalBytes, pieceSize int) string {
	var period strings.Builder
	for i := 0; period.Len() < periodBytes; i++ {
		fmt.Fprintf(&period, "step %05d inspecting module %05d before deciding the next action; ", i, i*7919)
	}
	p := period.String()[:periodBytes]

	var sb strings.Builder
	for written := 0; written < totalBytes; written += pieceSize {
		off := written % periodBytes
		b, _ := json.Marshal(p[off : off+pieceSize])
		sb.WriteString(`data: {"choices":[{"delta":{"reasoning_content":`)
		sb.Write(b)
		sb.WriteString(`}}]}` + "\n\n")
	}
	sb.WriteString("data: [DONE]\n\n")
	return sb.String()
}

// TestChatThinkingLoopStallCatchesShortStream 停滞判据必须在远短于旧阈值的流上命中，
// 且必须是停滞判据（而非占比）立的功。
//
// 这是用户点出的那个缺口：旧判据（唯一块占比 + 40 万字符下限）在实测那次 20.8 万
// 字符的空转上根本不触发 —— 那次挂了 382 秒、正文为零。
//
// 本用例用**出厂默认判据**（只把耗时门槛归零，因为测试流在毫秒内跑完），
// 喂一段周期 24KB、总长 10 万字符的循环文本：
//   - 停滞判据：周期跑完（24KB=1000 块）后再无新块，到 72KB 时停滞数达 2000 → 命中
//   - 占比判据：10 万字符时占比仍有 1000/4167≈0.24 > 0.15 → 不命中
//   - 旧判据：10 万 < 40 万字符下限 → 永不命中
//
// 所以关掉停滞判据后本用例必 RED（流会跑完且无任何拦截）。
func TestChatThinkingLoopStallCatchesShortStream(t *testing.T) {
	const (
		periodBytes = 24000
		totalBytes  = 100000
		pieceSize   = 240
		totalFrames = totalBytes / pieceSize // 417
	)
	var nCalls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		nCalls++
		return 200, loopSSEPeriod(periodBytes, totalBytes, pieceSize), true
	})
	h := NewHandler(Config{
		Pool: testPoolWith(
			&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
			&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		),
		Upstream: up,
		// 出厂默认判据（停滞窗口 2000 块 / 字符下限 3 万），只关掉「最早可判定耗时」。
		LoopGuard: LoopGuardConfig{Enabled: true},
	})
	h.loopGuard.MinElapsed = 0

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "thinking_loop") {
		t.Fatalf("10 万字符的长周期循环未被拦下（占比判据够不到、旧阈值也够不到）: code=%d", rec.Code)
	}
	// 命中点在停滞窗口处（约 7.2 万字符 = 300 帧），重试一轮共约 600 帧。
	// 若判据失效、流跑到底，两轮就是 2×417=834 帧 —— 用这个差值证明是中途切断。
	if n := strings.Count(body, `"reasoning_content"`); n >= 2*totalFrames {
		t.Errorf("送出了 %d 帧（≥ 跑完两轮的 %d）：说明是跑完才判，不是中途切断", n, 2*totalFrames)
	}
	if nCalls != 2 {
		t.Errorf("上游调用 %d 次 want 2（首次 + 1 次重试）", nCalls)
	}
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

// TestLoopLogRingBuffer 环形缓冲：超容量时丢最旧的、保留最近 N 条，且不泄漏底层数组。
//
// 底层数组泄漏是真会发生的写法：l.hits = l.hits[1:] 只移动切片头，
// 之后每次 append 都会在新位置写入，容量不足时重新分配 —— 老数据虽不可见，
// 但切片头持续后移期间 append 会反复扩容。这里断言长度恒等于容量上限。
func TestLoopLogRingBuffer(t *testing.T) {
	var l loopLog
	total := loopHitsCap + 5
	for i := 0; i < total; i++ {
		l.record(LoopHit{UID: fmt.Sprintf("u%d", i), ThinkChars: i})
	}
	gotTotal, hits := l.snapshot()
	if gotTotal != total {
		t.Errorf("total=%d want %d（累计计数不该被容量截断）", gotTotal, total)
	}
	if len(hits) != loopHitsCap {
		t.Fatalf("len(hits)=%d want %d", len(hits), loopHitsCap)
	}
	// 最旧的 5 条被丢弃：首条应是 u5，末条是 u{total-1}。
	if hits[0].UID != "u5" {
		t.Errorf("首条 uid=%q want u5（最旧的应被丢弃）", hits[0].UID)
	}
	if last := hits[len(hits)-1]; last.UID != fmt.Sprintf("u%d", total-1) {
		t.Errorf("末条 uid=%q want u%d", last.UID, total-1)
	}
	// 顺序必须是时间序：面板按此顺序展示，乱序会读成「空转忽多忽少」。
	for i := 1; i < len(hits); i++ {
		if hits[i].ThinkChars <= hits[i-1].ThinkChars {
			t.Fatalf("顺序错乱: hits[%d]=%d <= hits[%d]=%d", i, hits[i].ThinkChars, i-1, hits[i-1].ThinkChars)
		}
	}
}

// TestLoopLogSnapshotIsCopy 快照必须与后续写入隔离 ——
// /status 序列化期间若有新命中写入，共享底层数组会产生撕裂读（甚至 race）。
func TestLoopLogSnapshotIsCopy(t *testing.T) {
	var l loopLog
	l.record(LoopHit{UID: "u1"})
	_, snap := l.snapshot()
	l.record(LoopHit{UID: "u2"})
	if len(snap) != 1 || snap[0].UID != "u1" {
		t.Errorf("快照被后续写入影响: %+v", snap)
	}
}

// TestStatusExposesLoopLog /status 必须暴露近期空转与累计次数，
// 且无命中时是空数组而非 null（面板不必为 null 写特例）。
func TestStatusExposesLoopLog(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: &upstream.Client{}, LoopGuard: LoopGuardConfig{Enabled: true}})

	// 无命中：total=0、数组为空。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	if body["thinking_loop_total"] != float64(0) {
		t.Errorf("thinking_loop_total=%v want 0", body["thinking_loop_total"])
	}
	if hits, ok := body["thinking_loops"].([]any); !ok || len(hits) != 0 {
		t.Errorf("thinking_loops=%v want 空数组（非 null）", body["thinking_loops"])
	}

	// 记一条后：total=1、含上下文估算。
	h.loopLog.record(LoopHit{
		At: time.Now(), UID: "u1", Model: "deepseek-v4.1-flash",
		ThinkChars: 61536, Ratio: 0.218, StaleChunks: 2000,
		ReqEstTokens: 232535, Retry: 1,
	})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	if body["thinking_loop_total"] != float64(1) {
		t.Errorf("thinking_loop_total=%v want 1", body["thinking_loop_total"])
	}
	hits, ok := body["thinking_loops"].([]any)
	if !ok || len(hits) != 1 {
		t.Fatalf("thinking_loops=%v want 1 条", body["thinking_loops"])
	}
	hit, _ := hits[0].(map[string]any)
	if hit["req_est_tokens"] != float64(232535) {
		t.Errorf("req_est_tokens=%v want 232535（这是判断上下文是否占满的关键字段）", hit["req_est_tokens"])
	}
	if hit["model"] != "deepseek-v4.1-flash" || hit["retry"] != float64(1) {
		t.Errorf("hit=%v 缺 model/retry", hit)
	}
}

// TestChatThinkingLoopRecordsHit 端到端：命中后必须落进环形缓冲。
// 记录逻辑若只写日志不进缓冲，/status 会永远显示 0 —— 面板看着正常但其实没数据。
//
// 池里放两个号：单账号池在首轮命中后被 tried 排除、第二轮无号可选就直接退出，
// 走不到第二次命中，测不出 retry 序号。
func TestChatThinkingLoopRecordsHit(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, loopSSE(200), true
	})
	h := testLoopHandler(t, up, testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	), 1)

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	h.ServeHTTP(httptest.NewRecorder(), req)

	total, hits := h.loopLog.snapshot()
	// maxRetries=1：首次命中记一条，重试命中再记一条，共 2 条。
	if total != 2 || len(hits) != 2 {
		t.Fatalf("total=%d len=%d want 2/2（首次 + 重试各记一条）", total, len(hits))
	}
	if hits[0].Retry != 1 || hits[1].Retry != 2 {
		t.Errorf("retry 序号=%d,%d want 1,2（据此区分「一次请求重试多次」与「多起独立事件」）",
			hits[0].Retry, hits[1].Retry)
	}
	// 两条分属不同账号：命中的号会被 tried 排除，重试必然换号。
	if hits[0].UID == hits[1].UID {
		t.Errorf("两次命中都用 %s：重试没有换号", hits[0].UID)
	}
	if hits[0].Model != "glm-5.2" {
		t.Errorf("记录缺 model: %+v", hits[0])
	}
	if hits[0].ReqEstTokens <= 0 {
		t.Errorf("req_est_tokens=%d want >0", hits[0].ReqEstTokens)
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
