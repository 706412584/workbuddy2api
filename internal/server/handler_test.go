package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/prompt"
	"workbuddy2api/internal/redisstore"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

// TestMain 默认关闭聊天表格日志（chatLogEnabled=false），消除 go test 期间的 stdout 噪音。
// 断言表格行输出的测试（logging_test.go 中的 ChatLogs/LogChatRow 系列）用 withChatLog 临时开启。
func TestMain(m *testing.M) {
	chatLogEnabled = false
	os.Exit(m.Run())
}

const sseOK = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n" +
	"data: [DONE]\n\n"

// newFakeUpstream 返回一个 ChatStream 走 fake 的 upstream.Client。
// fake 依据 Authorization 头决定行为。
func newFakeUpstream(t *testing.T, behavior func(auth string) (status int, body string, isStream bool)) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			authz := r.Header.Get("Authorization")
			status, body, isStream := behavior(authz)
			ct := "application/json"
			if isStream {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// bindStore 记录粘性绑定镜像调用（不联网），供 D4 端到端断言绑定收敛到最终成功号。
type bindStore struct {
	redisstore.Noop
	mu     sync.Mutex
	binds  map[string]string
	delCnt int
}

func newBindStore() *bindStore { return &bindStore{binds: map[string]string{}} }

func (b *bindStore) SetBind(key, uid string, ttl time.Duration) {
	b.mu.Lock()
	b.binds[key] = uid
	b.mu.Unlock()
}
func (b *bindStore) DelBind(key string) {
	b.mu.Lock()
	b.delCnt++
	delete(b.binds, key)
	b.mu.Unlock()
}
func (b *bindStore) LoadBinds() map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]string{}
	for k, v := range b.binds {
		out[k] = v
	}
	return out
}
func (b *bindStore) lastUID(key string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	u, ok := b.binds[key]
	return u, ok
}

// resetModelsCache 清空两区的动态模型缓存（TTL 缓存 + 失败负缓存）。
// 无参清两区：多数用例只关心「不受上一次 fetch 影响」，逐个指定区域反而易漏。
func resetModelsCache() {
	dynamicModelsCache.mu.Lock()
	dynamicModelsCache.cn = modelsSlot{}
	dynamicModelsCache.global = modelsSlot{}
	dynamicModelsCache.mu.Unlock()
}

// testPoolWith 构建一个所有账号 credits=1000 的池，并注入确定性随机源：
// randInt64N 恒返回 0 → pickWeighted 必选候选集中积分最高者（第一个）。
// 这让依赖"bad 先被选中"的轮转测试（如 TestChatRotatesOnHardCredit）完全确定，
// 不再受加权随机影响而 flake。
func testPoolWith(auths ...*auth.Auth) *pool.Pool {
	p := pool.New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	for _, a := range auths {
		p.Add(a)
		p.SetCredits(a.UID, 1000)
	}
	return p
}

// TestChatBodyLimitExactAllowed 恰好等于上限的请求体正常放行到上游（不被 413 误伤）。
func TestChatBodyLimitExactAllowed(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxBodyBytes: 100})
	const prefix = `{"model":"glm-5.2","messages":[],"pad":"`
	const suffix = `"}`
	pad := strings.Repeat("a", 100-len(prefix)-len(suffix)) // 恰好 100 字节
	if body := prefix + pad + suffix; len(body) != 100 {
		t.Fatalf("fixture len=%d want 100", len(body))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(prefix+pad+suffix)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (exactly-at-limit body must proceed)", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Errorf("upstream calls=%d want 1", calls)
	}
}

// TestChatOversizedBodyReturns413 请求体超过上限 → 直接 413 request_body_too_large：
// 不打上游（calls=0）、不罚账号（无冷却/无熔断计数/无禁用）、不轮转。
func TestChatOversizedBodyReturns413(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxBodyBytes: 100})
	const prefix = `{"model":"glm-5.2","messages":[],"pad":"`
	const suffix = `"}`
	// 101 字节 > 100 上限。
	pad := strings.Repeat("a", 100-len(prefix)-len(suffix)+1)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(prefix+pad+suffix)))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code=%d body=%s want 413", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "request_body_too_large") {
		t.Errorf("body should carry request_body_too_large: %s", body)
	}
	if !strings.Contains(body, "server.max_body_mb") {
		t.Errorf("413 message should name config key server.max_body_mb: %s", body)
	}
	if calls != 0 {
		t.Errorf("upstream must not be called on 413, got %d", calls)
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 {
		t.Errorf("413 must not penalize account: %+v", st)
	}
}

// TestChatOversizedBodyDefaultLimitHeader 未显式设置 MaxBodyBytes 时兜底 8MB：
// 8MB+1 的请求体必须 413（不再静默截断喂给上游，issue #41 根因）。
func TestChatOversizedBodyDefaultLimit(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up}) // 不注入 MaxBodyBytes → 默认 defaultMaxBodyBytes

	body := make([]byte, defaultMaxBodyBytes+1) // 超默认上限 1 字节
	copy(body, `{"model":"glm-5.2","messages":[]}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body)))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code=%d want 413 (default+1 must be rejected)", rec.Code)
	}
	if calls != 0 {
		t.Errorf("upstream must not be called, got %d", calls)
	}
}

// TestChatBadParamsRotatesWithoutPenalty 上游 400 + Unmarshal chat params failed（11101）
// → 该类归 ErrBadParams：不罚账号（无冷却/无禁用/无熔断计数/无 errTotal），但**仍然轮转**
// （换号重试可能命中不同权限的账号）。端到端断言 bad 失败、good 成功、账号完好。
func TestChatBadParamsRotatesWithoutPenalty(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 400, `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000) // 确定性源 r=0 → 先选 bad
	p.SetCredits("good", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (want 200 after rotate to good)", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 1 {
		t.Errorf("calls=%v want bad/good 各 1 次", calls)
	}
	// 账号完好：无冷却、无禁用、无熔断计数、无 errTotal。
	st, _ := p.Status("bad")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.BreakerFails != 0 {
		t.Errorf("ErrBadParams must not penalize account: %+v", st)
	}
}

// TestChatAllBadParams503CarriesUpstreamBody 全部账号都 11101 时 503 文案必须包含
// 上游原始 11101 信息（不再是空洞的 no_healthy_account）。
// 现状即透传 lastErr.Error()（含上游 body），本测试把它锁定为回归。
func TestChatAllBadParams503CarriesUpstreamBody(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s (want 503)", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "11101") || !strings.Contains(body, "Unmarshal chat params failed") {
		t.Errorf("503 message should carry upstream 11101 info: %s", body)
	}
}

func TestChatNonStreamAggregates(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz != "Bearer at1" {
			t.Errorf("auth=%q", authz)
		}
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	if resp["object"] != "chat.completion" {
		t.Errorf("object=%v", resp["object"])
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好" {
		t.Errorf("content=%q", msg["content"])
	}
}

func TestChatStreamPassthrough(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("ct=%q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "你好") || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("body=%q", body)
	}
}

func TestChatRotatesOnHardCredit(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 402, `{"code":1,"msg":"余额不足"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// 让 bad 积分更高被先选中
	p.SetCredits("bad", 2000)
	p.SetCredits("good", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 1 {
		t.Errorf("calls=%v", calls)
	}
	st, _ := p.Status("bad")
	if !st.Cooling || st.Reason == "" {
		t.Errorf("bad account should be cooling: %+v", st)
	}
}

// TestChatSoftCoolsOnRateLimitBody 端到端回归 issue #28：上游用非 429 状态码
// （400 + 限流文案）表达模型侧限流时，该账号必须进入 CoolSoft 冷却，而不是只换号。
// 修复前 Classify 归 ErrClient → applyErrorPolicy 走 default 分支只换号不罚，
// 账号留在可用池里，下一个请求仍会被选中。
func TestChatSoftCoolsOnRateLimitBody(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 400, `{"code":1,"msg":"The model provider is rate-limiting requests. Please wait a moment and try again."}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// 让 bad 积分更高被先选中（与 TestChatRotatesOnHardCredit 同一确定性手法）。
	p.SetCredits("bad", 2000)
	p.SetCredits("good", 1000)
	const soft = 45 * time.Second
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: soft})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 1 {
		t.Errorf("calls=%v want bad/good 各 1 次", calls)
	}
	st, _ := p.Status("bad")
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("bad 应进入 soft_rate 冷却: %+v", st)
	}
	// 冷却时长取自注入的 SoftCooldown，不依赖真实等待。
	if max := int64(soft / time.Second); st.CoolRemaining <= 0 || st.CoolRemaining > max {
		t.Errorf("cool_remaining_sec=%d want in (0,%d]", st.CoolRemaining, max)
	}

	// 冷却生效：同一账号在冷却期内不得再被选中。
	before := calls["Bearer at-bad"]
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec2.Code != 200 {
		t.Fatalf("second code=%d body=%s", rec2.Code, rec2.Body)
	}
	if calls["Bearer at-bad"] != before {
		t.Errorf("冷却中的账号不应再次被选中: calls=%v", calls)
	}
}

// TestApplyErrorPolicySoftRateExponentialBackoff handler 层回归：同一账号连续被限流，
// 冷却时长必须 600s → 1200s → 2400s 指数增长（时长断言全部取自注入值，不依赖真实等待）。
// 直接驱动 applyErrorPolicy 而非发 HTTP 请求：账号在冷却期内不会被再次选中，
// 走完整请求会需要等冷却自然到期（真实 sleep），而这里要验的正是"连续限流"的退避本身。
// Classify→applyErrorPolicy 的接线由 TestChatSoftCoolsOnRateLimitBody 端到端覆盖。
func TestApplyErrorPolicySoftRateExponentialBackoff(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})

	for i, want := range []int64{600, 1200, 2400} {
		h.applyErrorPolicy("u1", upstream.ErrSoftRate, "", "")
		st, _ := p.Status("u1")
		if !st.Cooling || st.CoolKind != "soft_rate" {
			t.Fatalf("call %d: 应为 soft_rate 冷却: %+v", i+1, st)
		}
		if st.SoftStreak != i+1 {
			t.Errorf("call %d: soft_streak=%d want %d", i+1, st.SoftStreak, i+1)
		}
		if st.CoolRemaining < want-3 || st.CoolRemaining > want {
			t.Errorf("call %d: cool_remaining_sec=%d want ~%d", i+1, st.CoolRemaining, want)
		}
	}
}

// TestApplyErrorPolicyNotFoundUsesFixedBase 404 分流：偶发上游 404 的冷却基数固定 60s
// （notFoundCooldown），不取 soft_rate 的 600s 基数，也不受其配置值影响。
//
// 关于退避：404 仍走 Cooldown(CoolSoft)，因此与 429 共用同一 softStreak（本用例锚定这一
// 现状）。这不构成"偶发 404 罚过重"的场景——streak 只在**连续**无成功时累积，
// 中间任何一次成功（NoteSuccess）都会把它清零；故只有持续 404 的坏号才会退避升级。
func TestApplyErrorPolicyNotFoundUsesFixedBase(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 20 * time.Minute}) // soft_rate 配得很大，验证 404 不受其影响

	notFoundSec := int64(notFoundCooldown / time.Second)
	for i, want := range []int64{notFoundSec, 2 * notFoundSec, 4 * notFoundSec} {
		h.applyErrorPolicy("u1", upstream.ErrNotFound, "", "")
		st, _ := p.Status("u1")
		if !st.Cooling || st.CoolKind != "soft_rate" {
			t.Fatalf("call %d: 应为 soft 冷却: %+v", i+1, st)
		}
		if st.SoftStreak != i+1 {
			t.Errorf("call %d: soft_streak=%d want %d", i+1, st.SoftStreak, i+1)
		}
		// 基数取自 notFoundCooldown（60s）而非注入的 soft_rate（20m）。
		if st.CoolRemaining < want-3 || st.CoolRemaining > want {
			t.Errorf("call %d: 404 cool_remaining_sec=%d want ~%d（固定基数 %ds，非 soft_rate）",
				i+1, st.CoolRemaining, want, notFoundSec)
		}
	}

	// 成功后 streak 归零 → 下次 404 回到 60s 基数。
	// （签到解冻 ReenableIfCredits 不适用于本场景：它保留 streak，是冷却域的续期。）
	p.NoteSuccess("u1")
	h.applyErrorPolicy("u1", upstream.ErrNotFound, "", "")
	if st, _ := p.Status("u1"); st.SoftStreak != 1 || st.CoolRemaining > notFoundSec {
		t.Errorf("success should reset 404 backoff: %+v", st)
	}
}

// TestNewHandlerSoftCooldownDefault 端到端：未注入 SoftCooldown 时基数回落到 600s（原为 60s）。
func TestNewHandlerSoftCooldownDefault(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 429, `{"code":1,"msg":"rate limit"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000)
	p.SetCredits("good", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up}) // 不注入 SoftCooldown

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("bad")
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("bad 应进入 soft_rate 冷却: %+v", st)
	}
	if st.CoolRemaining <= 599 || st.CoolRemaining > 600 {
		t.Errorf("default soft cooldown cool_remaining_sec=%d want 600", st.CoolRemaining)
	}
}

// TestChatStickyFollowsFinalSuccess 端到端验证 D4：粘性号失败换号成功后，会话绑定收敛到成功号。
func TestChatStickyFollowsFinalSuccess(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"bad", "good"} },
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// 先把会话预绑定到 bad（模拟历史粘性），bad 失败、good 成功 → 绑定应切到 good。
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 500, `{"code":500}`, false
		}
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:         p,
		Upstream:     up,
		Session:      sess,
		SoftCooldown: time.Minute,
	})
	// 预绑定：sess.Bind("conv-1", "bad")，然后请求体带同 conversation_id。
	sess.Bind("conv-1", "bad")
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// 绑定必须收敛到最终成功的 good。
	if uid, ok := st.lastUID("conv-1"); !ok || uid != "good" {
		t.Fatalf("sticky binding should follow final success to good, got %s ok=%v (binds=%v)", uid, ok, st.binds)
	}
}

// TestChatStickySuccessKeepsBinding 粘性号直接成功 → 绑定不变（仍为该号）。
func TestChatStickySuccessKeepsBinding(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"good"} },
	})
	p := testPoolWith(&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})
	sess.Bind("conv-1", "good")
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if uid, ok := st.lastUID("conv-1"); !ok || uid != "good" {
		t.Fatalf("binding should stay good after success, got %s ok=%v", uid, ok)
	}
}

// TestChatStickyFullFallsBackToRotation 端到端验证 C3 语义：粘性号满载不可用时，
// 请求在同一轮内解绑并回落普通轮换选中健康账号，绑定收敛到最终成功号——而非空耗一轮。
func TestChatStickyFullFallsBackToRotation(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"bad", "good"} },
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// bad 占满唯一在途名额：粘性命中校验将返回 nil（healthy 但 inFlight 满）→ 解绑 + 回落轮换。
	p.SetMaxInFlight(1)
	p.Acquire("bad")

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:         p,
		Upstream:     up,
		Session:      sess,
		SoftCooldown: time.Minute,
	})
	sess.Bind("conv-1", "bad")
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// 满载粘性号被解绑，绑定收敛到最终成功号 good。
	if uid, ok := st.lastUID("conv-1"); !ok || uid != "good" {
		t.Fatalf("sticky binding should fall back to good, got %s ok=%v (binds=%v)", uid, ok, st.binds)
	}
	p.Release("bad")
}

func TestChatHardCreditCooldownUntilNextDay4AM(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 402, `{"code":1,"msg":"余额不足"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000) // bad 积分高，确定性源 → 先被选中
	p.SetCredits("good", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, ok := p.Status("bad")
	if !ok || !st.Cooling {
		t.Fatalf("bad should be cooling: %+v ok=%v", st, ok)
	}
	if st.Reason != "余额不足" {
		t.Errorf("reason=%q", st.Reason)
	}
	// 硬信贷冷却必须是次日 04:00，而不是固定 12h/配置时长。
	if st.Until.Hour() != 4 {
		t.Errorf("until hour=%d want 4 (next-day 04:00)", st.Until.Hour())
	}
	// 距次日 04:00 最长 28h（凌晨 00:00~04:00 间运行时 now→次日 04:00 跨度 > 24h，属正常）。
	if d := time.Until(st.Until); d <= 0 || d > 28*time.Hour {
		t.Errorf("until %v not within (0,28h]: %v", st.Until, d)
	}
	// 立即换号成功：good 被选中。
	stGood, _ := p.Status("good")
	if stGood.Cooling || stGood.Disabled {
		t.Errorf("good should stay healthy: %+v", stGood)
	}
}

// TestChat6004ModelResetCoolsToParsedTime 端到端回归 issue #31：上游 429 + code 6004
// TestChat6004ModelResetCoolsToParsedTime 6004 + 重置时间 → 冷却 until 精确等于解析时间
// （而非 600s 固定基数/指数退避），且记录触发模型 → 同模型请求仍被冷却、切模型请求按豁免可选。
//
// 中英文文案都要测：生产实测 6004 **全是英文**，而本用例原本只构造了中文 body，
// 于是「解析正则只写了中文」这个 bug 在测试里完全看不出来 —— 它让模型级冷却
// （CooldownSoftForModel）在生产中从未生效，账号被按全模型冷却。
func TestChat6004ModelResetCoolsToParsedTime(t *testing.T) {
	// 用未来 5 分钟的重置时间（wall-clock）构造上游响应。
	reset := time.Now().Add(5 * time.Minute)
	ts := reset.In(upstream.SoftRateResetLoc()).Format("2006-01-02 15:04:05")
	langs := []struct {
		name string
		body string
	}{
		{"中文", `{"code":6004,"msg":"将在 ` + ts + ` UTC+8 重置"}`},
		// 生产原文（intl 与 cn 实测一致）。
		{"英文", `{"code":6004,"msg":"usage exceeds frequency limit, but don't worry, your usage will reset at ` + ts + ` UTC+8, alternatively, you can switch to the other models to continue using it."}`},
	}
	for _, lang := range langs {
		t.Run(lang.name, func(t *testing.T) {
			body := lang.body
			up := newFakeUpstream(t, func(authz string) (int, string, bool) {
				if authz == "Bearer at-bad" {
					return 429, body, false
				}
				return 200, sseOK, true
			})
			p := testPoolWith(
				&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
				&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
			)
			p.SetCredits("bad", 2000)
			p.SetCredits("good", 1000)
			// 隔离对 breaker 的干扰：熔断阈值默认 3，一次失败不触发。
			h := NewHandler(Config{Pool: p, Upstream: up})
			req := httptest.NewRequest("POST", "/v1/chat/completions",
				strings.NewReader(`{"model":"glm-5.3","messages":[]}`))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Fatalf("code=%d body=%s (want 200 after rotate to good)", rec.Code, rec.Body)
			}
			// bad 已进入 soft 冷却，until ≈ reset。
			st, _ := p.Status("bad")
			if !st.Cooling || st.CoolKind != "soft_rate" {
				t.Fatalf("bad should be soft cooling from 6004: %+v", st)
			}
			if d := st.Until.Sub(reset); d < -time.Second || d > time.Second {
				t.Errorf("until=%v want ~reset=%v (diff %v)：未按上游明说的重置时刻冷却", st.Until, reset, d)
			}
			// 记录触发模型（bad 池内 private 字段需经 Status 不可见，改用行为断言）：
			// 同模型 glm-5.3 的请求不应选中 bad（仍冷却）；
			// 不同模型 hy3-x 的请求应豁免冷却选中 bad（最高分）——
			// 这正是上游文案里 "switch to the other models" 承诺的语义。
			p.SetRandomSource(func(n int64) int64 { return 0 })
			same := p.PickExcludingForModel(nil, "glm-5.3")
			if same == nil || same.UID != "good" {
				t.Fatalf("same-model pick should skip bad (still cooling), got %+v", same)
			}
			diff := p.PickExcludingForModel(nil, "hy3-x")
			if diff == nil || diff.UID != "bad" {
				t.Fatalf("different-model pick should bypass bad soft cooling, got %+v", diff)
			}
		})
	}
}

// TestChat6004WithoutResetFallsBackToBackoff 6004 无时间文案 → 退回 600s 基数软冷却
// （现状不变）。
func TestChat6004WithoutResetFallsBackToBackoff(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		if authz == "Bearer at-bad" {
			return 429, `{"code":6004,"msg":"model usage limit exceeded"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000)
	p.SetCredits("good", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("bad")
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("bad should be soft cooling: %+v", st)
	}
	// 冷却时长 = 注入 soft 基数(60s)，非解析时间（无重置文案）。
	if st.CoolRemaining <= 0 || st.CoolRemaining > 60 {
		t.Errorf("cool_remaining_sec=%d want ~60 (soft base, not parsed)", st.CoolRemaining)
	}
}

func TestChatAllUnavailableReturns503(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 402, `{"code":1,"msg":"余额不足"}`, false
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Errorf("code=%d body=%s", rec.Code, rec.Body)
	}
	var e map[string]any
	json.Unmarshal(rec.Body.Bytes(), &e)
	if e["error"] == nil {
		t.Errorf("want error envelope: %s", rec.Body)
	}
}

func TestChatSessionDeadDisables(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 401, `{"code":12153,"msg":"Offline user session not found"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Errorf("code=%d", rec.Code)
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Errorf("account should be disabled: %+v", st)
	}
}

func TestChatTransportErrorDoesNotPenalize(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("connection refused")
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	// 传输错误不喂熔断计数：一次 transport error 不应累计 errTotal 也不应熔断。
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.ErrTotal != 0 {
		t.Fatalf("transport error should not penalize account: %+v", st)
	}
}

// TestChatHTTP5xxDoesNotPenalize 上游 5xx 不罚账号（无冷却/熔断/NoteError），只换号。
// 上游整体故障（APISIX 502/504 或 500）时每个账号都会失败；若喂熔断计数，健康号会被
// 逐批误杀——实测一次上游故障把 16/22 打进冷却、healthy 掉到 4，且候选池塌缩后剩余
// 账号被反复选中、更快撞够阈值。上游恢复后还要等熔断到期才满血。
// 5xx 是「上游此刻病了」而不是「这个号坏了」，与 ErrClient 同待遇（只换号不罚）。
func TestChatHTTP5xxDoesNotPenalize(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"500 JSON", 500, `{"code":500}`},
		{"502 网关错误页", 502, `<html><head><title>502 Bad Gateway</title></head></html>`},
		{"504 网关超时页", 504, `<html><head><title>504 Gateway Time-out</title></head></html>`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
			// 熔断阈值 1：若误喂 NoteError 一次即熔断。
			p.SetBreaker(1, time.Hour, time.Hour)
			up := newFakeUpstream(t, func(authz string) (int, string, bool) {
				return c.status, c.body, false
			})
			h := NewHandler(Config{Pool: p, Upstream: up})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
				strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
			// 轮转耗尽仍回 503（客户端视角不变）；变的是账号不被罚。
			if rec.Code != 503 {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
			}
			st, _ := p.Status("u1")
			if st.Cooling {
				t.Errorf("upstream 5xx should not cool the account: %+v", st)
			}
			if st.BreakerFails != 0 {
				t.Errorf("upstream 5xx should not feed breaker: breaker_fails=%d", st.BreakerFails)
			}
			if st.ErrTotal != 0 {
				t.Errorf("upstream 5xx should not record err_total: %d", st.ErrTotal)
			}
		})
	}
}

func TestChatHTTP4xxClientDoesNotPenalize(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":400,"msg":"bad request"}`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.ErrTotal != 0 {
		t.Fatalf("generic 4xx should not penalize account: %+v", st)
	}
}

// TestChatContextExceededReturns400NoRotate 上下文超限必须立即回 400，且不换号。
//
// 这是一条**必须原样透传**的错误，三处细节缺一不可（2026-09-18 实测踩全了）：
//   - 状态码 4xx：原先落 ErrClient → 换号重试 → 503。客户端把 5xx 读作「服务过载，
//     稍后重试」，于是原样重发同一份超长上下文，自动压缩永不触发。
//   - 不轮换：窗口是模型属性不是账号属性，每个账号都返回一字不差的 400，
//     轮换只让客户端白等 3 倍往返（实测 3 × 9s ≈ 28s）。
//   - 消息保住 "prompt is too long: N tokens > M maximum"：客户端按它触发压缩。
func TestChatContextExceededReturns400NoRotate(t *testing.T) {
	const prod = `{"code":11115,"msg":"prompt is too long: 1049589 tokens > 1048576 maximum","requestId":"x","extError":{"code":"context_length_exceeded","message":"..."}}`
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 400, prod, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u3", AccessToken: "at3", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400（503 会让客户端以为该重试）body=%s", rec.Code, rec.Body)
	}
	// 按客户端的方式断言：解析 JSON 后取 message。
	// 序列化会把 '>' 转义成 \u003e，解析后还原 —— 真实客户端也走 JSON.parse，
	// 故断言解析结果而非原始字节。
	var resp struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("非 OpenAI 错误体: %v body=%s", err, rec.Body)
	}
	// 客户端按此文案触发上下文压缩，必须与上游原文逐字一致（含 token 数）。
	if resp.Error.Message != "prompt is too long: 1049589 tokens > 1048576 maximum" {
		t.Errorf("message=%q 未透传上游原文（客户端按它触发压缩）", resp.Error.Message)
	}
	if resp.Error.Type != "invalid_request_error" {
		t.Errorf("type=%q want invalid_request_error（api_error 会被当成服务故障）", resp.Error.Type)
	}
	if resp.Error.Code != "context_length_exceeded" {
		t.Errorf("code=%q want context_length_exceeded", resp.Error.Code)
	}
	if strings.Contains(rec.Body.String(), "no_healthy_account") {
		t.Errorf("被通用文案吞掉，客户端看不出是上下文问题: %s", rec.Body)
	}
	// 只调一次上游：不轮换。
	if calls != 1 {
		t.Errorf("上游调用 %d 次 want 1（换号改变不了请求体，重试纯属浪费）", calls)
	}
	// 账号无过错，不得受罚。
	for _, uid := range []string{"u1", "u2", "u3"} {
		if st, _ := p.Status(uid); st.Cooling || st.ErrTotal != 0 {
			t.Errorf("%s 被误罚: %+v", uid, st)
		}
	}
}

// TestAnthropicContextExceededIsInvalidRequest Anthropic 协议的上下文超限必须是
// invalid_request_error，**绝不能是 overloaded_error**。
//
// 这条断言直接对应线上故障：原先回 503 → anthropicErrType 映射为 "overloaded_error"
// → Claude Code 读作「服务过载，稍后重试」→ 原样重发 → 自动压缩永不触发，
// 日志里同一请求每 10 秒重发一次、token 数一字不差、连续数十次。
// Claude Code 触发压缩的信号是 400 + invalid_request_error。
func TestAnthropicContextExceededIsInvalidRequest(t *testing.T) {
	const prod = `{"code":11115,"msg":"prompt is too long: 1049589 tokens > 1048576 maximum","extError":{"code":"context_length_exceeded"}}`
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, prod, false
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4-5-20250929","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("非 Anthropic 错误体: %v body=%s", err, rec.Body)
	}
	if resp.Error.Type == "overloaded_error" {
		t.Fatal("overloaded_error 会被 Claude Code 读作「服务过载，稍后重试」——正是它导致无限重传")
	}
	if resp.Error.Type != "invalid_request_error" {
		t.Errorf("error.type=%q want invalid_request_error", resp.Error.Type)
	}
	if !strings.Contains(resp.Error.Message, "prompt is too long") {
		t.Errorf("message=%q 未透传上游原文", resp.Error.Message)
	}
}

func TestModelsEndpoint(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: upstream.New()})
	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["object"] != "list" {
		t.Errorf("object=%v", resp["object"])
	}
	data := resp["data"].([]any)
	if len(data) < 5 {
		t.Errorf("models count=%d", len(data))
	}
	found := false
	for _, m := range data {
		if m.(map[string]any)["id"] == "glm-5.2" {
			found = true
		}
	}
	if !found {
		t.Error("glm-5.2 missing")
	}
}

func TestModelsDynamic(t *testing.T) {
	// 清缓存（按区域分槽：本用例账号属 CN，故清 CN 槽）。
	slot := dynamicModelsCache.slot(auth.RegionCN)
	dynamicModelsCache.mu.Lock()
	slot.ids = nil
	slot.fetched = time.Time{}
	slot.lastFail = time.Time{}
	dynamicModelsCache.mu.Unlock()

	// 假上游返回动态模型（含 agents + maxInputTokens/maxOutputTokens）
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[{"id":"dyn-model-a","maxInputTokens":65536,"maxOutputTokens":8192},{"id":"dyn-model-b","maxInputTokens":131072,"maxOutputTokens":16384},{"id":"glm-9.9","maxInputTokens":262144,"maxOutputTokens":32768}],"agents":[{"name":"cli","models":["dyn-model-a","dyn-model-b","glm-9.9"]}]}}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	data := resp["data"].([]any)
	if len(data) != 3 {
		t.Fatalf("want 3 dynamic models, got %d: %v", len(data), data)
	}
	ids := map[string]bool{}
	for _, m := range data {
		ids[m.(map[string]any)["id"].(string)] = true
	}
	if !ids["dyn-model-a"] || !ids["glm-9.9"] {
		t.Errorf("dynamic ids missing: %v", ids)
	}

	// 断言字段映射：maxInputTokens → context_length，maxOutputTokens → max_output_tokens
	for _, m := range data {
		mm := m.(map[string]any)
		switch mm["id"] {
		case "dyn-model-a":
			if mm["context_length"].(float64) != 65536 {
				t.Errorf("dyn-model-a context_length=%v want 65536", mm["context_length"])
			}
			if mm["max_output_tokens"].(float64) != 8192 {
				t.Errorf("dyn-model-a max_output_tokens=%v want 8192", mm["max_output_tokens"])
			}
		case "glm-9.9":
			if mm["context_length"].(float64) != 262144 {
				t.Errorf("glm-9.9 context_length=%v want 262144", mm["context_length"])
			}
			if mm["max_output_tokens"].(float64) != 32768 {
				t.Errorf("glm-9.9 max_output_tokens=%v want 32768", mm["max_output_tokens"])
			}
		}
	}

	// 第二次调用走缓存（把上游关掉也成功）
	dynamicModelsCache.mu.RLock()
	cached := len(slot.ids)
	dynamicModelsCache.mu.RUnlock()
	if cached != 3 {
		t.Errorf("cache not populated: %d", cached)
	}
}

func TestModelsDynamicFallsBackToStatic(t *testing.T) {
	resetModelsCache()

	// 假上游 500
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 500, `boom`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	data := resp["data"].([]any)
	// 回退静态表（≥5 个）
	if len(data) < 5 {
		t.Errorf("static fallback failed: %d", len(data))
	}
}

// TestModelsFetchFailureDoesNotPenalizeAccount models 拉取失败与 chat 熔断解耦
// （P1-6/发现 6）：/v1/models 的动态拉取失败（Models 端点网络抖动）不喂
// NoteError——该熔断器保护的是 chat 选号，models 拉取失败 ≠ 账号 chat 不可用，
// 跨界惩罚会让上游 models 端点偶发 5xx 把好号提前打进熔断。失败只进 5min 负缓存。
func TestModelsFetchFailureDoesNotPenalizeAccount(t *testing.T) {
	resetModelsCache()

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetBreaker(1, time.Hour, time.Hour) // 熔断阈值 1：若误喂 NoteError 一次即熔断
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 500, `boom`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d (static fallback)", rec.Code)
	}
	st, _ := p.Status("u1")
	// 熔断器零观测：不喂 fails（breaker_fails=0）、不熔断（Cooling=false）、
	// 不记 last_err/err_total。负缓存是唯一的失败退避（另测）。
	if st.BreakerFails != 0 {
		t.Errorf("models fetch failure should not feed breaker: breaker_fails=%d", st.BreakerFails)
	}
	if st.Cooling {
		t.Errorf("models fetch failure should not trip breaker with threshold=1: %+v", st)
	}
	if st.ErrTotal != 0 {
		t.Errorf("models fetch failure should not record err_total: %d", st.ErrTotal)
	}
	// 负缓存仍然生效：拉取失败进 lastFail（5min 冷却）。
	dynamicModelsCache.mu.RLock()
	failTs := dynamicModelsCache.cn.lastFail
	dynamicModelsCache.mu.RUnlock()
	if failTs.IsZero() {
		t.Error("negative cache (lastFail) should be set on fetch failure")
	}
}

func TestModelsNegativeCacheOnFetchFailure(t *testing.T) {
	resetModelsCache()

	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 500, `boom`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	// 连续 3 次请求，上游持续 500 → 只应触发 1 次 fetch（负缓存生效），
	// 其余走静态 fallback（仍返回 200）。
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
		if rec.Code != 200 {
			t.Fatalf("req %d: code=%d body=%s", i, rec.Code, rec.Body)
		}
	}
	if calls != 1 {
		t.Errorf("want 1 fetch, got %d", calls)
	}

	// 冷却期结束（把失败时间戳拨回 10 分钟前）→ 应重新 fetch。
	dynamicModelsCache.mu.Lock()
	dynamicModelsCache.slot(auth.RegionCN).lastFail = time.Now().Add(-10 * time.Minute)
	dynamicModelsCache.mu.Unlock()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("after cooldown: code=%d", rec.Code)
	}
	if calls != 2 {
		t.Errorf("want 2 fetch after cooldown, got %d", calls)
	}
}

func TestAPIKeyAuth(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		APIKey:   "secret",
	})
	// 无 key
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("no key: code=%d", rec.Code)
	}
	// 错 key
	req = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("wrong key: code=%d", rec.Code)
	}
	// 对 key（请求会继续打到上游，但此处上游 client 会失败 —— 只要不是 401 就行）
	req = httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("right key: code=%d", rec.Code)
	}
}

// TestAPIKeyAuthConstantTime Bearer 比较的边界回归（P2-8，发现 7）：
// 正确 key 通过；错误/空/前缀相同但长度不同一律 401。
// 常量时间属性（subtle.ConstantTimeCompare）本身无法用单元测试观测，
// 此处锁的是行为等价——换实现前后四条断言必须同样成立。
func TestAPIKeyAuthConstantTime(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		APIKey:   "secret",
	})
	cases := []struct {
		name string
		bear string // 完整 Authorization 头（不含 "Bearer " 前缀则按原样发）
		want int
	}{
		{"correct key", "secret", 200},
		{"wrong key", "wrong", 401},
		{"empty key", "", 401},
		{"same prefix longer", "secret-extra", 401},
		{"same prefix shorter", "sec", 401},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/v1/models", nil)
			req.Header.Set("Authorization", "Bearer "+c.bear)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			// 正确 key 会继续打到上游（GET /v1/models 走静态表 → 200）；
			// 其余必须被 401 挡在鉴权层。
			if rec.Code != c.want {
				t.Errorf("Bearer %q: code=%d want %d", c.bear, rec.Code, c.want)
			}
		})
	}
}

func TestStatusEndpoint(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "nick", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCredits("u1", 42)
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	req := httptest.NewRequest("GET", "/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"uid":"u1"`) || !strings.Contains(body, `"credits":42`) {
		t.Errorf("body=%s", body)
	}
	if strings.Contains(body, "AccessToken") || strings.Contains(body, `"at"`) {
		t.Error("token leaked in status output")
	}
	// Phase 3 汇总字段。
	var statusBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &statusBody); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	if statusBody["total"] != float64(1) || statusBody["healthy"] != float64(1) ||
		statusBody["cooling"] != float64(0) || statusBody["disabled"] != float64(0) ||
		statusBody["in_flight_full"] != float64(0) {
		t.Errorf("summary=%v want total=1 healthy=1 cooling=0 disabled=0 in_flight_full=0", statusBody)
	}
	// Phase v3：池级 sticky_sessions + redis_mode。
	if statusBody["sticky_sessions"] != float64(0) {
		t.Errorf("sticky_sessions=%v want 0", statusBody["sticky_sessions"])
	}
	if statusBody["redis_mode"] != "noop" {
		t.Errorf("redis_mode=%v want noop", statusBody["redis_mode"])
	}
}

// TestStatusInFlightFull /status 透出满载计数：healthy 且占满在途的账号数。
func TestStatusInFlightFull(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "full", AccessToken: "at", ExpiresAt: 9999999999},
		&auth.Auth{UID: "free", AccessToken: "at", ExpiresAt: 9999999999},
	)
	p.SetMaxInFlight(1)
	p.Acquire("full")
	defer p.Release("full")

	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var statusBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &statusBody); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	if statusBody["in_flight_full"] != float64(1) {
		t.Errorf("in_flight_full=%v want 1", statusBody["in_flight_full"])
	}
	if statusBody["healthy"] != float64(2) {
		t.Errorf("healthy=%v want 2 (full is still healthy by state-machine semantics)", statusBody["healthy"])
	}
}

func TestStatusPortraitFields(t *testing.T) {
	// Phase 3：/status 单账号需返回健康画像字段。
	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "nick", AccessToken: "at", ExpiresAt: 9999999999})
	p.NoteSuccess("u1")
	p.NoteSuccess("u1")
	p.NoteError("u1") // 记录 last_err + err_total（累计，不冷却）
	p.Cooldown("u1", pool.CoolSoft, time.Hour, "429 rate limit")
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var body struct {
		Accounts []pool.Status `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	if len(body.Accounts) != 1 {
		t.Fatalf("accounts=%d", len(body.Accounts))
	}
	st := body.Accounts[0]
	if !st.Cooling || st.CoolKind != "soft_rate" || st.CoolRemaining <= 0 {
		t.Errorf("cooling portrait=%+v", st)
	}
	if st.SuccessCount != 2 {
		t.Errorf("success_count=%d want 2", st.SuccessCount)
	}
	if st.ErrTotal != 1 {
		t.Errorf("err_total=%d want 1", st.ErrTotal)
	}
	if st.LastSuccessTime.IsZero() {
		t.Error("last_success should be set")
	}
	if st.LastErrTime.IsZero() {
		t.Error("last_err should be set")
	}
}

// TestHealthzEmptyPool 空池（healthy=0）→ 503，表示暂不可服务。
func TestHealthzEmptyPool(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code=%d want 503 (healthy=0)", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}
	if resp["healthy"] != float64(0) || resp["total"] != float64(0) {
		t.Errorf("healthz json=%v", resp)
	}
}

// TestHealthz503WhenNoHealthy 所有账号禁用/冷却 → 503。
func TestHealthz503WhenNoHealthy(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.Disable("u1", "session dead")
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("ct=%q want json", ct)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}
	if resp["healthy"] != float64(0) || resp["total"] != float64(1) {
		t.Errorf("healthz json=%v want healthy=0 total=1", resp)
	}
}

// TestHealthz503WhenAllInFlightFull 全部账号 healthy 但都占满在途 → 503（与 chat 同口径），
// 且 healthy 语义未变（仍为 1）：满载不是状态机健康维度的变化，是探活口径单独叠加。
func TestHealthz503WhenAllInFlightFull(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetMaxInFlight(1)
	p.Acquire("u1") // 占满唯一在途名额
	defer p.Release("u1")

	if p.ServableNow() {
		t.Fatal("servable should be false when the only healthy account is in-flight full")
	}
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 (healthy but all in-flight full)", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}
	// healthy 语义未变：账号仍是 healthy（只占满在途，非冷却/禁用）。
	if resp["healthy"] != float64(1) || resp["total"] != float64(1) {
		t.Errorf("healthz json=%v want healthy=1 total=1 (healthy semantics unchanged)", resp)
	}
}

// TestHealthz200WhenAllModelExempt 全部账号处于 6004 单模型软冷却（对其他模型仍可选）
// → /healthz 必须 200，与 chat 的模型级豁免（切模型立即可用）同口径。
// 回归：此前 ServableNow 只看账号级 healthy，"全号被 v4.1 限流但 glm 可用"时误报 503。
func TestHealthz200WhenAllModelExempt(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "429 rate limit")
	// 账号级 healthy 已为 0（冷却中），但模型豁免使 chat 对非 glm 请求仍可达。
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 (model-exempt account keeps pool servable)", rec.Code)
	}
}

// TestHealthz200WhenHealthy 有健康账号 → 200。
func TestHealthz200WithHealthy(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}
	if resp["healthy"] != float64(1) || resp["total"] != float64(1) {
		t.Errorf("healthz json=%v want healthy=1 total=1", resp)
	}
}

// TestHealthzServiceIdentity /healthz 无论 200 还是 503 都必须带网关身份标识
// （响应体 service 字段 + X-Service 头）：宿主探测打到同端口的旧服务/其他服务时，
// 对方即使返回 2xx 也不带本标识，宿主据此判"假成功"。
func TestHealthzServiceIdentity(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(*pool.Pool)
		wantCode int
	}{
		{"healthy", func(*pool.Pool) {}, http.StatusOK},
		{"unhealthy", func(p *pool.Pool) { p.Disable("u1", "session dead") }, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
			tc.setup(p)
			h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
			if rec.Code != tc.wantCode {
				t.Fatalf("code=%d want %d", rec.Code, tc.wantCode)
			}
			if got := rec.Header().Get("X-Service"); got != ServiceName {
				t.Errorf("X-Service=%q want %q", got, ServiceName)
			}
			var resp map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
			}
			if resp["service"] != ServiceName {
				t.Errorf("service=%v want %q", resp["service"], ServiceName)
			}
		})
	}
}

// TestHealthzServiceIdentityWithoutAuth /healthz 保持无鉴权（负载均衡友好）：
// 配了 api_key 也不要求 Bearer，身份字段照常返回。
func TestHealthzServiceIdentityWithoutAuth(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		APIKey:   "secret",
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz must stay unauthenticated: code=%d", rec.Code)
	}
	if got := rec.Header().Get("X-Service"); got != ServiceName {
		t.Errorf("X-Service=%q want %q", got, ServiceName)
	}
}

func TestStatusRequiresAuth(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "nick", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), APIKey: "secret"})

	// 无 token → 401
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 401 {
		t.Errorf("no token: code=%d", rec.Code)
	}

	// 带 token → 200
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("Authorization", "Bearer secret")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("with token: code=%d", rec.Code)
	}

	// /healthz 无鉴权仍 200
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Errorf("healthz: code=%d", rec.Code)
	}
}

// TestContentBlockedTriggersDegradedRetry passthrough 模式下首请求 400（11128 文案）
// → 降级重试（Degraded）→ 200，客户端无感。验证第二次出站 body 为 Degraded。
func TestContentBlockedTriggersDegradedRetry(t *testing.T) {
	// 记录每次出站请求体，断言第二次为 Degraded 文本。
	var bodies [][]byte
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			bodies = append(bodies, raw)
			// 首次（body 含原始 system）返回 400 内容拦截；后续返回 200。
			if len(bodies) == 1 {
				return &http.Response{
					StatusCode: 400,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"code":11128,"msg":"blocked by security policy"}`)),
				}, nil
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"原始指纹"},{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (want 200 after degraded retry)", rec.Code, rec.Body)
	}
	if len(bodies) != 2 {
		t.Fatalf("want 2 upstream calls (first 400 + retry), got %d", len(bodies))
	}
	// 第二次出站 body 的 messages 头部 system 内容应为 Degraded 文本。
	if !strings.Contains(string(bodies[1]), prompt.Degraded) {
		t.Errorf("second body should contain Degraded prompt: %s", bodies[1])
	}
	if strings.Contains(string(bodies[1]), "原始指纹") {
		t.Errorf("second body should not contain original system: %s", bodies[1])
	}
}

// TestContentBlockedStickyDegraded 降级后新请求直达 Degraded（不再先撞 400）。
func TestContentBlockedStickyDegraded(t *testing.T) {
	var firstCall bool
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if !firstCall {
			firstCall = true
			return 400, `{"code":11128,"msg":"blocked by security policy"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	// 首请求触发降级 → 200。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"x"},{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("first req code=%d", rec.Code)
	}
	// 降级粘性：新请求 Active()=true，body 已被 Rewrite(Degraded)，上游首字节即 200。
	// 但 fake 上游只对 firstCall 返回 400，之后都 200，无法区分"直达"与"重试"。
	// 用 degrade.Active() 直接断言粘性生效。
	if !h.degrade.Active() {
		t.Fatal("degrade should be active after trigger")
	}
}

// TestContentBlockedCustomModeDoesNotDegrade custom 模式不触发降级重试
// （custom 已用自有提示词替换，不应再有 system 来源误报；若仍 400 走既有错误路径）。
func TestContentBlockedCustomModeDoesNotDegrade(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11128,"msg":"blocked by security policy"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "custom", PromptText: "SYS"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"system","content":"old"},{"role":"user","content":"hi"}]}`)))
	// custom 模式下 400 直接返回 503（所有账号轮转失败），不降级重试。
	if rec.Code != 503 {
		t.Fatalf("code=%d want 503 (custom does not degrade)", rec.Code)
	}
	if h.degrade.Active() {
		t.Error("degrade should NOT be active in custom mode")
	}
}

// TestContentBlockedDoesNotPenalizeAccount ErrContentBlocked 不罚账号（无冷却/熔断/NoteError）。
func TestContentBlockedDoesNotPenalizeAccount(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11128,"msg":"blocked by security policy"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	// 熔断阈值 1：若误罚 NoteError 一次即熔断；content_blocked 不应喂熔断。
	p.SetBreaker(1, time.Hour, time.Hour)
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	st, _ := p.Status("u1")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 {
		t.Fatalf("ErrContentBlocked should not penalize account: %+v", st)
	}
}

// TestCustomModeFingerprintSanitizePreserved custom 端到端：user 消息含 Claude Code
// 指纹句（PR39 fixture 串）→ Rewrite 注入自有 system → 经 sanitize → 出站 body 中
// 该指纹被改写、system 为自有提示词。证明两层（提示词替换 + 清洗）叠加工作。
//
// 两层各自职责（互不替代）：
//   - prompt.Rewrite 替换 system/developer 消息（消灭 system 来源指纹）；
//   - sanitizeMessages 改写 user/assistant 消息中残留的指纹串（兜底用户上下文）。
func TestCustomModeFingerprintSanitizePreserved(t *testing.T) {
	// 捕获出站 body（prepareBody 已强制 stream + 归一 + 清洗后）。
	var sentBody []byte
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			sentBody = raw
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:           "https://fake.example",
		SanitizeFingerprints: true, // 开启清洗层（与生产一致）
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	const customSys = "我是网关自有提示词"
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "custom", PromptText: customSys})

	// user 消息含 Claude Code 指纹句（PR39 的 fixture 串，逐字精确指纹）。
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{
		"model":"glm-5.2",
		"stream":true,
		"messages":[
			{"role":"system","content":"You are Claude Code, Anthropic's official CLI for Claude."},
			{"role":"user","content":"You are Claude Code, Anthropic's official CLI for Claude. Main branch (you will usually use this for PRs)"}
		]
	}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	out := string(sentBody)
	// 1) system 为自有提示词，旧 system 内容零残留。
	if !strings.Contains(out, customSys) {
		t.Errorf("out body should contain custom system prompt: %s", out)
	}
	if strings.Contains(out, "official CLI for Claude.") && strings.Contains(out, "You are Claude Code, Anthropic's") {
		// 旧 system 原文（含句点）不应以 system 角色出现；但 sanitize 把它改写为
		// "...official CLI tool for Claude."，所以原文 fingerprint 串应消失。
	}
	// 原始指纹串（逐字精确匹配）在出站 body 中应被改写：
	// "official CLI for Claude." → "official CLI tool for Claude."
	// "Main branch (" → "Default branch ("
	if strings.Contains(out, "official CLI for Claude.") {
		t.Errorf("identity fingerprint not rewritten by sanitize in user msg: %s", out)
	}
	if strings.Contains(out, "Main branch (you will usually use this for PRs)") {
		t.Errorf("branch fingerprint not rewritten by sanitize in user msg: %s", out)
	}
	// 改写后的痕迹应在（证明 sanitize 层生效，不是"全删了"）。
	if !strings.Contains(out, "official CLI tool for Claude.") {
		t.Errorf("sanitized identity rewrite missing: %s", out)
	}
	if !strings.Contains(out, "Default branch (you will usually use this for PRs)") {
		t.Errorf("sanitized branch rewrite missing: %s", out)
	}
	// 2) messages 头部恰好一条 system = 自有提示词（Rewrite 已删旧 system）。
	var obj map[string]any
	if err := json.Unmarshal(sentBody, &obj); err != nil {
		t.Fatalf("out body not json: %v %s", err, out)
	}
	msgs := obj["messages"].([]any)
	var systemCount int
	for _, m := range msgs {
		mm := m.(map[string]any)
		if mm["role"] == "system" {
			systemCount++
			if mm["content"] != customSys {
				t.Errorf("system content=%v want %q", mm["content"], customSys)
			}
		}
	}
	if systemCount != 1 {
		t.Errorf("want exactly 1 system message, got %d (all=%v)", systemCount, msgs)
	}
}

// cnAuth / globalAuth 构造两区账号，便于区域路由用例。
func cnAuth(uid, at string) *auth.Auth {
	return &auth.Auth{UID: uid, AccessToken: at, ExpiresAt: 9999999999, Domain: "copilot.tencent.com"}
}

func globalAuth(uid, at string) *auth.Auth {
	return &auth.Auth{UID: uid, AccessToken: at, ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"}
}

// TestChatRoutesModelToOwningRegion 请求应只发给支持该模型的区域账号。
// 注意：实测 global 可用的 13 个模型在 CN 均存在，因此「区域限制」实际只表现为
// 「CN 独有模型不得发给 global 账号」；反向由 TestChatNoMatchingRegionReturns503 覆盖。
func TestChatRoutesModelToOwningRegion(t *testing.T) {
	// 注意：global 的模型列表接口恒 500，其上可能还有未探测到的独有模型，
	// 因此路由表只收录已证实的映射，未知模型一律不限制。
	cases := []struct {
		model    string
		wantAuth string // 期望被调用的账号 AccessToken；空 = 两区皆可，不做断言
	}{
		{"deepseek-v4-pro", "Bearer at-cn"}, // CN 独有（global 调它报 11102）
		{"hunyuan-chat", "Bearer at-cn"},    // CN 独有
		{"deepseek-v4.1-flash", ""},         // 两区都有
		{"glm-5.2", ""},                     // 两区都有
	}
	for _, c := range cases {
		t.Run(c.model, func(t *testing.T) {
			var got []string
			up := newFakeUpstream(t, func(authz string) (int, string, bool) {
				got = append(got, authz)
				return 200, sseOK, true
			})
			p := testPoolWith(
				cnAuth("cn", "at-cn"),
				globalAuth("intl", "at-intl"),
			)
			h := NewHandler(Config{Pool: p, Upstream: up})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
				strings.NewReader(`{"model":"`+c.model+`","messages":[{"role":"user","content":"hi"}]}`)))
			if rec.Code != 200 {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
			}
			if len(got) == 0 {
				t.Fatal("no upstream call")
			}
			if c.wantAuth != "" && got[0] != c.wantAuth {
				t.Fatalf("upstream called with %v, want first=%s (model %s 必须路由到其所属区域)",
					got, c.wantAuth, c.model)
			}
			// CN 独有模型绝不能被发给 global 账号（否则上游 11102）。
			if c.wantAuth == "Bearer at-cn" {
				for _, a := range got {
					if a == "Bearer at-intl" {
						t.Fatalf("CN 独有模型 %s 被发给了 global 账号: %v", c.model, got)
					}
				}
			}
		})
	}
}

// TestChatNoMatchingRegionReturns503 池中无该区域账号时应返回 503 且说明原因，
// 而不是退化去调用错误区域的账号。
func TestChatNoMatchingRegionReturns503(t *testing.T) {
	called := false
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		called = true
		return 200, sseOK, true
	})
	// 池中只有 global 账号，却请求 CN 独有模型。
	p := testPoolWith(globalAuth("intl", "at-intl"))
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 body=%s", rec.Code, rec.Body)
	}
	if called {
		t.Error("must not call upstream when no account in the model's region")
	}
	if !strings.Contains(rec.Body.String(), "deepseek-v4-pro") {
		t.Errorf("503 body should name the model: %s", rec.Body)
	}
}

// TestChatUnknownModelNotRegionRestricted 未收录的模型不作区域限制（向前兼容上游新增模型）。
func TestChatUnknownModelNotRegionRestricted(t *testing.T) {
	var got []string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		got = append(got, authz)
		return 200, sseOK, true
	})
	// 只有 global 账号，模型表未收录 → 应照常服务。
	p := testPoolWith(globalAuth("intl", "at-intl"))
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"brand-new-model-9000","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d want 200 body=%s", rec.Code, rec.Body)
	}
	if len(got) != 1 || got[0] != "Bearer at-intl" {
		t.Errorf("upstream calls=%v want [Bearer at-intl]", got)
	}
}

// TestModelsListIsUnionOfPoolRegions /v1/models 应返回池中两区模型的并集：
// 仅 global 账号时列出 global 模型（不列 CN 独有），混池时两区并集去重。
func TestModelsListIsUnionOfPoolRegions(t *testing.T) {
	modelIDs := func(rec *httptest.ResponseRecorder) map[string]bool {
		var resp map[string]any
		json.Unmarshal(rec.Body.Bytes(), &resp)
		ids := map[string]bool{}
		for _, m := range resp["data"].([]any) {
			ids[m.(map[string]any)["id"].(string)] = true
		}
		return ids
	}
	// 让动态接口失败，走静态表以便断言确定内容。
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 500, `boom`, false
	})

	// 只有 CN 账号 → 只有 CN 静态表，不含 global 独有的 hy4-preview-x。
	resetModelsCache()
	h := NewHandler(Config{Pool: testPoolWith(cnAuth("cn", "at-cn")), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	ids := modelIDs(rec)
	if !ids["deepseek-v4-pro"] {
		t.Error("CN 账号池应含 deepseek-v4-pro")
	}
	if ids["hy4-preview-x"] {
		t.Error("只挂 CN 账号时不应列出 global 独有的 hy4-preview-x")
	}

	// 混池 → 两区并集。
	resetModelsCache()
	h = NewHandler(Config{Pool: testPoolWith(cnAuth("cn", "at-cn"), globalAuth("intl", "at-intl")), Upstream: up})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	ids = modelIDs(rec)
	if !ids["deepseek-v4-pro"] || !ids["hy4-preview-x"] {
		t.Errorf("混池应含两区模型: deepseek-v4-pro=%v hy4-preview-x=%v", ids["deepseek-v4-pro"], ids["hy4-preview-x"])
	}

	// 只有 global 账号 → 不含 CN 独有的 deepseek-v4-pro。
	resetModelsCache()
	h = NewHandler(Config{Pool: testPoolWith(globalAuth("intl", "at-intl")), Upstream: up})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	ids = modelIDs(rec)
	if ids["deepseek-v4-pro"] {
		t.Error("只挂 global 账号时不应列出 CN 独有的 deepseek-v4-pro")
	}
	if !ids["hy4-preview-x"] {
		t.Error("global 账号池应含 hy4-preview-x")
	}
}

// TestRegionsForModel 路由表与未知模型回退的单元断言。
func TestRegionsForModel(t *testing.T) {
	if got := regionsForModel(""); got != nil {
		t.Errorf("empty model → %v want nil (不限制)", got)
	}
	if got := regionsForModel("totally-unknown"); got != nil {
		t.Errorf("unknown model → %v want nil (不限制)", got)
	}
	if got := regionsForModel("deepseek-v4-pro"); len(got) != 1 || got[0] != auth.RegionCN {
		t.Errorf("deepseek-v4-pro → %v want [cn]", got)
	}
	// 实测：global 可用的模型在 CN 均存在（hy4-preview-x 即两区共有），
	// 故此处断言两区，而非仅 global。
	if got := regionsForModel("hy4-preview-x"); len(got) != 2 {
		t.Errorf("hy4-preview-x → %v want 两区共有", got)
	}
	// 两区共有：cn 在前（升序），且不得重复。
	got := regionsForModel("deepseek-v4.1-flash")
	if len(got) != 2 || got[0] != auth.RegionCN || got[1] != auth.RegionGlobal {
		t.Errorf("deepseek-v4.1-flash → %v want [cn global]", got)
	}
	if pred := regionAllowed(nil); pred != nil {
		t.Error("regionAllowed(nil) 应为 nil = 不过滤")
	}
}

// ── API key 绑定区域 ──────────────────────────────────────────────

// TestConstrainRegions 单元断言四种组合（密钥不限区域 / 密钥绑定 + 模型不限 /
// 密钥绑定 + 模型有限取交集 / 无交集）。
func TestConstrainRegions(t *testing.T) {
	cnOnly := []auth.Region{auth.RegionCN}
	both := []auth.Region{auth.RegionCN, auth.RegionGlobal}
	cnKey := &APIKeySpec{Key: "k", Region: auth.RegionCN}
	globalKey := &APIKeySpec{Key: "g", Region: auth.RegionGlobal}
	freeKey := &APIKeySpec{Key: "f"}

	cases := []struct {
		name   string
		model  []auth.Region
		key    *APIKeySpec
		want   []auth.Region
		wantOK bool
	}{
		{"密钥不限区域+模型有限 → 原样", cnOnly, freeKey, cnOnly, true},
		{"密钥不限区域+模型未知 → 仍不限制", nil, freeKey, nil, true},
		{"无密钥(不鉴权)+模型未知 → 不限制", nil, nil, nil, true},
		{"密钥CN+模型未知 → 收敛为CN", nil, cnKey, []auth.Region{auth.RegionCN}, true},
		{"密钥CN+模型CN → 交集CN", cnOnly, cnKey, []auth.Region{auth.RegionCN}, true},
		{"密钥CN+模型两区 → 交集CN", both, cnKey, []auth.Region{auth.RegionCN}, true},
		{"密钥CN+模型仅global → 无交集", []auth.Region{auth.RegionGlobal}, cnKey, nil, false},
		{"密钥global+模型仅CN → 无交集", cnOnly, globalKey, nil, false},
		{"密钥global+模型两区 → 交集global", both, globalKey, []auth.Region{auth.RegionGlobal}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := constrainRegions(c.model, c.key)
			if ok != c.wantOK {
				t.Fatalf("ok=%v want %v (got=%v)", ok, c.wantOK, got)
			}
			if len(got) != len(c.want) {
				t.Fatalf("regions=%v want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("regions=%v want %v", got, c.want)
				}
			}
		})
	}
}

// keysPool 构造 CN + global 双账号池，并把 global 账号权重拉高：
// 若区域约束失效，global 账号会因权重最高而被优先选中，从而暴露问题。
func keysPool() *pool.Pool {
	p := testPoolWith(
		cnAuth("cn", "at-cn"),
		globalAuth("intl", "at-intl"),
	)
	p.SetCredits("intl", 999999)
	p.SetCredits("cn", 1)
	return p
}

// TestAPIKeyRegionBindsAccount 绑定的密钥必须只用本区账号：
// 即便另一区账号权重远高，也必须被排除。
func TestAPIKeyRegionBindsAccount(t *testing.T) {
	cases := []struct {
		name     string
		key      string
		wantAuth string
	}{
		{"cn 密钥", "k-cn", "Bearer at-cn"},
		{"global 密钥", "k-global", "Bearer at-intl"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			up := newFakeUpstream(t, func(authz string) (int, string, bool) {
				got = append(got, authz)
				return 200, sseOK, true
			})
			h := NewHandler(Config{Pool: keysPool(), Upstream: up, APIKeys: []APIKeySpec{
				{Key: "k-cn", Region: auth.RegionCN, Name: "cn"},
				{Key: "k-global", Region: auth.RegionGlobal, Name: "global"},
			}})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/v1/chat/completions",
				strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("Authorization", "Bearer "+c.key)
			h.ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
			}
			if len(got) != 1 || got[0] != c.wantAuth {
				t.Fatalf("upstream calls=%v want [%s]（密钥区域约束失效，可能跑到了另一区）", got, c.wantAuth)
			}
		})
	}
}

// TestAPIKeyRegionRejectsModelFromOtherRegion 区域密钥请求不属于其区域的模型 → 404，
// 且不得调用上游（该模型对此密钥根本不存在）。
func TestAPIKeyRegionRejectsModelFromOtherRegion(t *testing.T) {
	called := false
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		called = true
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: keysPool(), Upstream: up, APIKeys: []APIKeySpec{
		{Key: "k-global", Region: auth.RegionGlobal},
	}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}]}`)) // 仅 CN 可用
	req.Header.Set("Authorization", "Bearer k-global")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d want 404 body=%s", rec.Code, rec.Body)
	}
	if called {
		t.Error("must not call upstream: 该模型对此密钥不存在")
	}
	// 错误体不得泄露区域结构。
	if body := rec.Body.String(); strings.Contains(body, "region") || strings.Contains(body, "cn") {
		t.Errorf("404 body 不应泄露区域信息: %s", body)
	}
}

// TestAPIKeyRegionFiltersModels 区域密钥的 /v1/models 只含本区模型。
func TestAPIKeyRegionFiltersModels(t *testing.T) {
	modelIDs := func(key string) map[string]bool {
		resetModelsCache()
		up := newFakeUpstream(t, func(authz string) (int, string, bool) {
			return 500, `boom`, false // 走静态表，内容确定
		})
		h := NewHandler(Config{Pool: keysPool(), Upstream: up, APIKeys: []APIKeySpec{
			{Key: "k-cn", Region: auth.RegionCN},
			{Key: "k-global", Region: auth.RegionGlobal},
		}})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("code=%d", rec.Code)
		}
		var resp map[string]any
		json.Unmarshal(rec.Body.Bytes(), &resp)
		ids := map[string]bool{}
		for _, m := range resp["data"].([]any) {
			ids[m.(map[string]any)["id"].(string)] = true
		}
		return ids
	}

	cn := modelIDs("k-cn")
	if !cn["deepseek-v4-pro"] {
		t.Error("cn 密钥应含 deepseek-v4-pro")
	}
	if cn["hy4-preview-x"] {
		t.Error("cn 密钥不应含 global 独有模型")
	}

	global := modelIDs("k-global")
	if !global["hy4-preview-x"] {
		t.Error("global 密钥应含 hy4-preview-x")
	}
	if global["deepseek-v4-pro"] {
		t.Error("global 密钥不应含 CN 独有模型 deepseek-v4-pro")
	}
}

// TestAPIKeyRegionFiltersStatus 区域密钥的 /status 只看到本区账号，计数口径同步。
func TestAPIKeyRegionFiltersStatus(t *testing.T) {
	statusUIDs := func(key string) (ids map[string]bool, total float64) {
		up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
		h := NewHandler(Config{Pool: keysPool(), Upstream: up, APIKeys: []APIKeySpec{
			{Key: "k-cn", Region: auth.RegionCN},
			{Key: "k-global", Region: auth.RegionGlobal},
			{Key: "k-any"}, // 不限区域
		}})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/status", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("code=%d", rec.Code)
		}
		var resp map[string]any
		json.Unmarshal(rec.Body.Bytes(), &resp)
		ids = map[string]bool{}
		for _, a := range resp["accounts"].([]any) {
			ids[a.(map[string]any)["uid"].(string)] = true
		}
		return ids, resp["total"].(float64)
	}

	cn, cnTotal := statusUIDs("k-cn")
	if len(cn) != 1 || !cn["cn"] || cnTotal != 1 {
		t.Errorf("cn 密钥 status: accounts=%v total=%v want 仅 cn/1", cn, cnTotal)
	}
	global, globalTotal := statusUIDs("k-global")
	if len(global) != 1 || !global["intl"] || globalTotal != 1 {
		t.Errorf("global 密钥 status: accounts=%v total=%v want 仅 intl/1", global, globalTotal)
	}
	// 不限区域密钥仍看到全部（改造前行为）。
	all, allTotal := statusUIDs("k-any")
	if len(all) != 2 || allTotal != 2 {
		t.Errorf("不限区域密钥 status: accounts=%v total=%v want 2 个/2", all, allTotal)
	}
}

// TestUnknownAPIKeyRejected 未登记的密钥仍 401。
func TestUnknownAPIKeyRejected(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{Pool: keysPool(), Upstream: up, APIKeys: []APIKeySpec{
		{Key: "k-cn", Region: auth.RegionCN},
	}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	req.Header.Set("Authorization", "Bearer nope")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d want 401", rec.Code)
	}
}

// fillReader 无限产出固定字节，避免为超限测试真的分配 32MB。
type fillReader struct{}

func (fillReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

// TestReadBodyRejectsOversize 锁定「超限要报错，不能静默截断」。
// 静默截断是 2026-09-13 那次 503 的根因：截断后的残体被原样转发上游，上游回 400
// code=11101 unexpected EOF，网关只能对调用方报 503，排查时会一路怀疑账号与限流。
func TestReadBodyRejectsOversize(t *testing.T) {
	const limit = 1 << 20

	// 超过上限：必须报错。
	over := &http.Request{Body: io.NopCloser(io.LimitReader(fillReader{}, limit+1))}
	if _, err := readBody(over, limit); err == nil {
		t.Fatal("body > limit: got nil error, want error")
	}

	// 正好等于上限：必须放行（readBody 多读 1 字节用于判定，别把边界一起拒了）。
	exact := &http.Request{Body: io.NopCloser(io.LimitReader(fillReader{}, limit))}
	body, err := readBody(exact, limit)
	if err != nil {
		t.Fatalf("body == limit: unexpected error %v", err)
	}
	if int64(len(body)) != limit {
		t.Fatalf("body == limit: got %d bytes, want %d", len(body), limit)
	}
}

// TestReadBodyOrFailTooLarge 锁定超限时的响应形态：413 + 可辨识错误码。
// 该错误在网关侧判出，不打上游、不罚账号、不轮转，调用方据此知道要调大
// server.max_body_mb，而不是去查账号或限流。
func TestReadBodyOrFailTooLarge(t *testing.T) {
	const limit = 1 << 20
	h := &Handler{cfg: Config{MaxBodyBytes: limit}}

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", io.LimitReader(fillReader{}, limit+1))
	if _, ok := h.readBodyOrFail(rec, r, false); ok {
		t.Fatal("oversize body: got ok=true, want false")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status: got %d, want 413", rec.Code)
	}
	var got struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal error body: %v", err)
	}
	if got.Error.Code != "request_body_too_large" {
		t.Fatalf("error code: got %q, want request_body_too_large", got.Error.Code)
	}

	// Anthropic 协议同一边界，但错误体结构不同（type=error + error.type）。
	rec = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodPost, "/v1/messages", io.LimitReader(fillReader{}, limit+1))
	if _, ok := h.readBodyOrFail(rec, r, true); ok {
		t.Fatal("anthropic oversize body: got ok=true, want false")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("anthropic status: got %d, want 413", rec.Code)
	}
	var aerr struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &aerr); err != nil {
		t.Fatalf("unmarshal anthropic error body: %v", err)
	}
	if aerr.Type != "error" || aerr.Error.Type != "request_too_large" {
		t.Fatalf("anthropic error shape: got %+v, want type=error / error.type=request_too_large", aerr)
	}
}
