package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   ErrKind
	}{
		{402, ``, ErrHardCredit},
		{400, `{"code":1,"msg":"余额不足"}`, ErrHardCredit},
		{403, `insufficient credits`, ErrHardCredit},
		{200, `{"code":10001,"msg":"积分不足，请充值"}`, ErrHardCredit},
		{400, `{"code":1,"msg":"额度用尽"}`, ErrHardCredit},
		// 上游 14018 原文是复数 "Credits exhausted"（真实 body）。hardMarkers 走子串
		// 匹配，词中间的 's' 让 "credit exhausted" 对不上，会落进下面的 429 兜底被
		// 判成 soft_rate：余额耗尽的号只软冷却十分钟就回池反复失败，而不是停到次日 04:00。
		{429, `{"error":{"data":{"code":14018,"msg":"Credits exhausted. Please visit the link below to purchase add-on packs and get more credits: https://www.codebuddy.ai/profile/usage","requestId":"x"}}}`, ErrHardCredit},
		{429, ``, ErrSoftRate},
		// 限流文案（issue #28）：状态码不是 429 时也必须识别为软限流，
		// 否则账号不会被冷却，下次请求仍会被选中。
		{200, `{"code":11140,"msg":"The model provider is rate-limiting requests. Please wait a moment and try again."}`, ErrSoftRate},
		{400, `rate limit`, ErrSoftRate},
		{403, `usage limit reached`, ErrSoftRate},
		// "model usage limit exceeded" 不是余额语义（无 credit/quota/积分/额度 等计费词），
		// 属于模型侧用量节流 → 短冷却（误判为硬冷却会把有余量的号停到次日 04:00）。
		{200, `{"code":1,"msg":"model usage limit exceeded"}`, ErrSoftRate},
		{200, `{"code":1,"msg":"too many requests"}`, ErrSoftRate},
		{500, `rate-limited upstream`, ErrSoftRate}, // 限流文案优先于 5xx 分类
		// 内容策略拦截（HTTP 400 + 审核文案）：误报信号，不罚账号，走降级重试。
		{400, `Illegal API invocation from an unapproved channel`, ErrContentBlocked},
		{400, `{"code":11128,"msg":"blocked by security policy"}`, ErrContentBlocked},
		{400, `unapproved channel`, ErrContentBlocked},
		// 通用 4xx（非审核文案）：仍判 ErrClient，只换号不罚。
		{400, `bad request`, ErrClient},
		// ErrBadParams：请求体解析失败（HTTP 400 + Unmarshal chat params failed / code 11101）。
		// 这是"发给上游的 body 有问题"（网关截断已由 413 消灭，剩余为客户端畸形 JSON），
		// 换了账号也一样 400，不罚号。具体词优先于通用 4xx。
		{400, `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF"}`, ErrBadParams},
		{400, `Unmarshal chat params failed`, ErrBadParams},
		{400, `{"code":11101,"msg":"x"}`, ErrBadParams},
		{200, `quota exceeded`, ErrHardCredit},
		// session 死亡优先于限流文案（401+12153 需人工重登，短冷却无意义）。
		{401, `{"code":12153,"msg":"Offline user session not found, rate limit"}`, ErrSessionDead},
		{401, `Offline user session not found`, ErrSessionDead},
		{401, `{"code":12153,"msg":"Offline user session not found"}`, ErrSessionDead},
		{401, `{"code":9999,"msg":"bad token"}`, ErrClient},
		{500, `boom`, ErrServer},
		{503, `unavailable`, ErrServer},
		{200, ``, ErrNone},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("Classify(%d,%q)=%v want %v", c.status, c.body, got, c.want)
		}
	}
}

// TestIsModelRateLimit 判断 429 body 是否明确指向模型级限流（code 6004）。
func TestIsModelRateLimit(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		// 6004：模型级限流（issue #31 的核心场景）。
		{`{"code":6004,"msg":"将在 2026-09-11 18:33:27 UTC+8 重置"}`, true},
		{`{"code": 6004,"msg":"x"}`, true},
		// 其他 code（非模型级限流）→ 不算。
		{`{"code":11140,"msg":"The model provider is rate-limiting requests."}`, false},
		{`{"code":1,"msg":"429 rate limit"}`, false},
	}
	for _, c := range cases {
		if got := IsModelRateLimit(c.body); got != c.want {
			t.Errorf("IsModelRateLimit(%q)=%v want %v", c.body, got, c.want)
		}
	}
}

// TestParseSoftRateReset 解析上游 429 6004 msg 里的「将在 … 重置」时间（## UTC+8）。
func TestParseSoftRateReset(t *testing.T) {
	future := time.Now().Add(35 * time.Minute)
	ts := future.In(softRateResetLoc).Format("2006-01-02 15:04:05")
	cases := []struct {
		name string
		body string
		ok   bool
	}{
		{"6004 带时间+UTC+8 后缀", `{"code":6004,"msg":"将在 ` + ts + ` UTC+8 重置"}`, true},
		{"6004 带时间无后缀", `{"code":6004,"msg":"将在 ` + ts + ` 重置"}`, true},
		{"6004 无时间文案", `{"code":6004,"msg":"model usage limit exceeded"}`, false},
		{"非 6004 但带时间（不是模型级）", `{"code":11140,"msg":"将在 ` + ts + ` UTC+8 重置"}`, false},
		{"非法时间格式", `{"code":6004,"msg":"将在 明天 重置"}`, false},
		{"空 body", ``, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ParseSoftRateReset(c.body)
			if ok != c.ok {
				t.Fatalf("ok=%v want %v (body=%s)", ok, c.ok, c.body)
			}
			if ok {
				// 解析结果 = ts 在 UTC+8 解释下的墙钟（截断到分钟），应与 future 相差 ±2 分钟。
				if d := got.Sub(future); d < -2*time.Minute || d > 2*time.Minute {
					t.Errorf("parsed=%v want ~%v (diff %v)", got, future, d)
				}
				if got.Location() != time.UTC {
					// 不同指针的 FixedZone 实例相等性按 offset 判，这里只断言 offset。
					if _, off := got.Zone(); off != 8*60*60 {
						t.Errorf("zone offset=%d want +08:00", off)
					}
				}
			}
		})
	}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func testClient(fn rtFunc) *Client {
	return &Client{
		HTTP:          &http.Client{Transport: fn},
		ChatBaseCN:    "https://chat.example",
		BillingBaseCN: "https://billing.example",
	}
}

func TestRefreshSuccess(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/plugin/auth/token/refresh") {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		if r.Header.Get("X-Refresh-Token") != "oldrt" {
			return nil, errors.New("missing X-Refresh-Token")
		}
		return jsonResp(200, `{"code":0,"msg":"ok","data":{"accessToken":"newat","refreshToken":"newrt","expiresIn":3600}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "oldrt", ExpiresAt: 1}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.AccessToken != "newat" || a.RefreshToken != "newrt" {
		t.Errorf("tokens not updated: %+v", a)
	}
	if a.ExpiresAt <= 1 {
		t.Errorf("expiresAt not advanced: %d", a.ExpiresAt)
	}
}

func TestRefreshPreservesExpiryWhenOmitted(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"accessToken":"newat"}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1753600000}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.ExpiresAt != 1753600000 {
		t.Errorf("expiresAt should be preserved, got %d", a.ExpiresAt)
	}
	if a.RefreshToken != "rt" {
		t.Errorf("refreshToken should be preserved, got %s", a.RefreshToken)
	}
}

func TestRefreshSessionDead(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 401,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"code":12153,"msg":"Offline user session not found"}`)),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}
	err := c.RefreshToken(a)
	if err == nil {
		t.Fatal("want error")
	}
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("want *Error, got %T %v", err, err)
	}
	if ue.Kind != ErrSessionDead {
		t.Errorf("kind=%v want ErrSessionDead", ue.Kind)
	}
}

func TestChatStreamSendsHeadersAndStreamTrue(t *testing.T) {
	var gotAuth, gotUID, gotProduct string
	var gotBody []byte
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotAuth = r.Header.Get("Authorization")
		gotUID = r.Header.Get("X-User-Id")
		gotProduct = r.Header.Get("X-Product")
		gotBody, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", EnterpriseID: "e1"}
	rc, status, respBody, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","messages":[]}`), "")
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	if respBody != nil {
		t.Errorf("200 response should carry nil body, got %q", respBody)
	}
	rc.Close()
	if gotAuth != "Bearer at" || gotUID != "u1" || gotProduct != "SaaS" {
		t.Errorf("headers: auth=%q uid=%q product=%q", gotAuth, gotUID, gotProduct)
	}
	if !bytes.Contains(gotBody, []byte(`"stream":true`)) {
		t.Errorf("stream not forced: %s", gotBody)
	}
}

func TestFetchModelsEffortsDriveBodyDowngrade(t *testing.T) {
	var outbound []byte
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models"):
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"glm-5.2","name":"GLM-5.2","maxInputTokens":131072,"maxOutputTokens":8192,"reasoning":{"effort":"high","supportedEfforts":["low","high"]}}
			],"agents":[{"name":"cli","models":["glm-5.2"]}]}}`), nil
		default:
			outbound, _ = io.ReadAll(r.Body)
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
			}, nil
		}
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	infos, err := c.FetchModels(a)
	if err != nil {
		t.Fatalf("fetch models: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("infos=%+v", infos)
	}
	// ModelInfo.Efforts 应携带 supportedEfforts
	if len(infos[0].Efforts) != 2 || infos[0].Efforts[0] != "low" {
		t.Errorf("infos[0].Efforts=%v", infos[0].Efforts)
	}

	// glm-5.2 只支持 low/high，请求 max → 降级为 high
	rc, status, _, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","reasoning_effort":"max","messages":[]}`), "")
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	rc.Close()
	var m map[string]any
	if err := json.Unmarshal(outbound, &m); err != nil {
		t.Fatalf("outbound unmarshal: %v (%s)", err, outbound)
	}
	if got, _ := m["reasoning_effort"].(string); got != "high" {
		t.Errorf("reasoning_effort=%v want high (outbound=%s)", m["reasoning_effort"], outbound)
	}
}

func TestChatStreamHardCreditError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(402, `{"code":1,"msg":"余额不足"}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	_, status, respBody, err := c.ChatStream(a, []byte(`{}`), "")
	if status != 402 {
		t.Errorf("status=%d", status)
	}
	if err != nil {
		t.Fatalf("hard credit should return body via status, not err: %v", err)
	}
	// caller classifies via returned body
	if Classify(status, string(respBody)) != ErrHardCredit {
		t.Errorf("body=%q not classified hard credit", respBody)
	}
}

// TestChatStreamReadsMultipleChunksOverRealTransport 走真实 net/http 传输层，
// 回归 defer cancel() 导致第二块起 body Read 返回 context canceled 的断流 bug。
func TestChatStreamReadsMultipleChunksOverRealTransport(t *testing.T) {
	const frames = 6
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("http.ResponseWriter does not implement http.Flusher")
			return
		}
		for i := 1; i <= frames; i++ {
			if _, err := fmt.Fprintf(w, "data: chunk-%d\n\n", i); err != nil {
				return
			}
			flusher.Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer srv.Close()

	c := New()
	c.ChatBaseCN = srv.URL
	c.IdleTimeout = 5 * time.Second

	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	rc, status, _, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","messages":[]}`), "")
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	defer rc.Close()

	buf := make([]byte, 1)
	var got string
	for i := 0; i < frames; i++ {
		if _, err := io.ReadFull(rc, buf); err != nil {
			t.Fatalf("read %d: %v (real transport body must not be cut)", i, err)
		}
		got += string(buf)
	}
	if strings.Contains(got, "context canceled") {
		t.Fatalf("body read hit context canceled, got %q", got)
	}
}

func TestUserResourceAggregation(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/get-user-resource") {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		if r.Method != http.MethodPost {
			return nil, errors.New("want POST")
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"ProductCode":"p_tcaca"`)) {
			return nil, errors.New("missing ProductCode: " + string(body))
		}
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"TotalCount":2,"TotalDosage":3000,"Accounts":[
			{"PackageName":"签到包","CapacitySize":2000,"CapacityRemain":1200,"CapacityUsed":800,"CycleCapacitySize":2000,"CycleCapacityRemain":1200,"CycleCapacityUsed":800},
			{"PackageName":"体验包","CapacitySize":1000,"CapacityRemain":300,"CapacityUsed":700,"CycleCapacitySize":1000,"CycleCapacityRemain":300,"CycleCapacityUsed":700}
		]}}}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	remain, err := c.UserResource(a)
	if err != nil {
		t.Fatalf("resource: %v", err)
	}
	if remain != 1500 {
		t.Errorf("remain=%d want 1500", remain)
	}
}

func TestUserResourceNegativeClamped(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[
			{"PackageName":"p","CycleCapacitySize":100,"CycleCapacityRemain":-50,"CycleCapacityUsed":150}
		]}}}}`), nil
	})
	remain, err := c.UserResource(&auth.Auth{AccessToken: "at"})
	if err != nil || remain != 0 {
		t.Errorf("remain=%d err=%v, want 0 (clamped)", remain, err)
	}
}

func TestDailyCheckinAlready(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/daily-checkin") {
			return nil, errors.New("wrong path")
		}
		return jsonResp(200, `{"code":14001,"msg":"今日已签到"}`), nil
	})
	err := c.DailyCheckin(&auth.Auth{AccessToken: "at"})
	if err == nil || !strings.Contains(err.Error(), "已签到") {
		t.Errorf("err=%v", err)
	}
}

// TestIsAlreadyCheckin "今天已签到"判定为幂等成功（中文/英文 markers 均命中）。
func TestIsAlreadyCheckin(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":10001,"msg":"今天已签到"}`), nil
	})
	if !IsAlreadyCheckin(c.DailyCheckin(&auth.Auth{AccessToken: "at"})) {
		t.Error("今天已签到 应判为 already")
	}

	c2 := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":10001,"msg":"Already checked in today"}`), nil
	})
	if !IsAlreadyCheckin(c2.DailyCheckin(&auth.Auth{AccessToken: "at"})) {
		t.Error("already(英文) 应判为 already")
	}
}

// TestIsAlreadyCheckinRejectsNonUpstream 网络层/解析层错误不得当作幂等成功。
// 误判会让当天实际未签到的账号被标成正常（停机补签遇到抖动时尤其危险）。
func TestIsAlreadyCheckinRejectsNonUpstream(t *testing.T) {
	if IsAlreadyCheckin(errors.New("dial tcp: connection refused")) {
		t.Error("网络错误不得判为 already")
	}
	if IsAlreadyCheckin(errors.New("parse failed: unexpected EOF")) {
		t.Error("解析错误不得判为 already")
	}
	// 非"已签到"语义的上游业务错误（如余额不足）也不得判为 already。
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":10002,"msg":"积分不足，请充值"}`), nil
	})
	if IsAlreadyCheckin(c.DailyCheckin(&auth.Auth{AccessToken: "at"})) {
		t.Error("余额不足不得判为 already")
	}
	// nil 不判为 already。
	if IsAlreadyCheckin(nil) {
		t.Error("nil 不得判为 already")
	}
}

// TestBasesByRegion 上游 base 与 Origin 按账号区域选择：
// workbuddy.ai 域走全局 base，其余（含空 domain）走 CN base。
func TestBasesByRegion(t *testing.T) {
	c := testClient(nil)
	c.ChatBaseGlobal = "https://chat.global"
	c.BillingBaseGl = "https://billing.global"

	cn := &auth.Auth{Domain: ""}
	unknown := &auth.Auth{Domain: "example.com"}
	global := &auth.Auth{Domain: "www.workbuddy.ai"}

	if c.chatBase(cn) != "https://chat.example" || c.billingBase(cn) != "https://billing.example" {
		t.Errorf("cn bases wrong: %s %s", c.chatBase(cn), c.billingBase(cn))
	}
	// 未知域名归 CN。
	if c.chatBase(unknown) != c.chatBase(cn) || c.billingBase(unknown) != c.billingBase(cn) {
		t.Error("unknown domain must fall back to CN bases")
	}
	// 全局账号走全局 base。
	if c.chatBase(global) != "https://chat.global" || c.billingBase(global) != "https://billing.global" {
		t.Errorf("global bases wrong: %s %s", c.chatBase(global), c.billingBase(global))
	}

	// Origin/Referer 同源按区域切换。
	if got := originRefererFor(global); got != originRefererGlob {
		t.Errorf("global origin=%q want %q", got, originRefererGlob)
	}
	if got := originRefererFor(cn); got != originRefererCN {
		t.Errorf("cn origin=%q want %q", got, originRefererCN)
	}
	if got := originRefererFor(nil); got != originRefererCN {
		t.Errorf("nil auth origin=%q want CN", got)
	}
}

// TestEffortsCacheIsolatedByRegion 两个区域的 effort 缓存必须互不覆盖：
// FetchModels 只替换所查账号区域的槽位。
func TestEffortsCacheIsolatedByRegion(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models") {
			// 两区返回同一模型但档位不同，以区分来源。
			efforts := `["low","high"]`
			if strings.Contains(r.Header.Get("Referer"), "workbuddy.ai") {
				efforts = `["medium"]`
			}
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"glm-5.2","name":"GLM-5.2","maxInputTokens":131072,"maxOutputTokens":8192,"reasoning":{"supportedEfforts":`+efforts+`}}
			],"agents":[{"name":"cli","models":["glm-5.2"]}]}}`), nil
		}
		return jsonResp(200, `{"code":0}`), nil
	})
	c.ChatBaseGlobal = "https://chat.global"

	cn := &auth.Auth{AccessToken: "at", UID: "cn", Domain: ""}
	global := &auth.Auth{AccessToken: "at", UID: "g", Domain: "www.workbuddy.ai"}

	if _, err := c.FetchModels(cn); err != nil {
		t.Fatalf("fetch cn: %v", err)
	}
	if _, err := c.FetchModels(global); err != nil {
		t.Fatalf("fetch global: %v", err)
	}

	// 拉取 global 后，CN 的档位不得被覆盖。
	if got := c.effortsSnapshot(cn); len(got["glm-5.2"]) != 2 {
		t.Errorf("cn efforts=%v want 2 档（被 global 覆盖？）", got["glm-5.2"])
	}
	if got := c.effortsSnapshot(global); len(got["glm-5.2"]) != 1 || got["glm-5.2"][0] != "medium" {
		t.Errorf("global efforts=%v want [medium]", got["glm-5.2"])
	}
	// 反向：重新拉 CN 也不得抹掉 global。
	if _, err := c.FetchModels(cn); err != nil {
		t.Fatalf("refetch cn: %v", err)
	}
	if got := c.effortsSnapshot(global); len(got["glm-5.2"]) != 1 {
		t.Errorf("global efforts after cn refetch=%v want [medium]", got["glm-5.2"])
	}
}

func TestNewChatClientNoTotalTimeoutAndSharedTransport(t *testing.T) {
	c := New()
	if c.ChatHTTP == nil {
		t.Fatal("ChatHTTP should be initialized")
	}
	if c.ChatHTTP.Timeout != 0 {
		t.Errorf("ChatHTTP.Timeout=%v want 0 (no total cap)", c.ChatHTTP.Timeout)
	}
	// 共享同一个 Transport 实例，连接池不重复。
	if c.ChatHTTP.Transport != c.HTTP.Transport {
		t.Errorf("ChatHTTP and HTTP must share the same *http.Transport")
	}
	htr, ok := c.ChatHTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport type=%T", c.ChatHTTP.Transport)
	}
	if htr.ResponseHeaderTimeout != 120*time.Second {
		t.Errorf("ResponseHeaderTimeout=%v want 120s", htr.ResponseHeaderTimeout)
	}
}

func TestChatStreamRoutesToChatHTTP(t *testing.T) {
	// 显式注入 ChatHTTP（可辨识标记），验证 ChatStream 走它而非 HTTP。
	chatHit, httpHit := false, false
	c := testClient(func(*http.Request) (*http.Response, error) {
		httpHit = true
		return jsonResp(200, `{}`), nil
	})
	c.ChatHTTP = &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
		chatHit = true
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	rc, status, _, err := c.ChatStream(a, []byte(`{}`), "")
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	rc.Close()
	if !chatHit {
		t.Error("ChatStream should use ChatHTTP")
	}
	if httpHit {
		t.Error("ChatStream must not use HTTP")
	}
}

func TestChatHTTPNilFallsBackToHTTP(t *testing.T) {
	c := testClient(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})
	if c.chatHTTP() != c.HTTP {
		t.Error("chatHTTP() should fall back to HTTP when ChatHTTP is nil")
	}
}

// failingReader 在吐出 prefix 后返回读错误，模拟「上游回了 500 但 body 传输中断」。
type failingReader struct {
	prefix []byte
	done   bool
}

func (f *failingReader) Read(p []byte) (int, error) {
	if !f.done {
		f.done = true
		n := copy(p, f.prefix)
		return n, nil
	}
	return 0, errors.New("connection reset by peer")
}

func (f *failingReader) Close() error { return nil }

// TestDoJSONReadErrorIsNotUpstreamError body 读失败必须返回普通错误（非 *Error）：
// 半截 body 若进了 Classify，可能命中「余额不足」等 marker 把传输层故障
// 误判成账号问题（长冷却罚号）。回归 doJSON 的 raw, _ := io.ReadAll 吞错。
func TestDoJSONReadErrorIsNotUpstreamError(t *testing.T) {
	// 半截 body 刻意含 hard credit marker：若实现吞掉读错误，Classify 会判 ErrHardCredit。
	const half = `{"code":1,"msg":"余额不足`
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 500,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       &failingReader{prefix: []byte(half)},
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}
	err := c.RefreshToken(a)
	if err == nil {
		t.Fatal("want error")
	}
	var ue *Error
	if errors.As(err, &ue) {
		t.Fatalf("读失败应返回普通错误而非 *Error（半截 body 不得参与 Classify），得到 Kind=%v Status=%d Msg=%q",
			ue.Kind, ue.Status, ue.Msg)
	}
	if !strings.Contains(err.Error(), "read body") {
		t.Errorf("err = %q，应说明是 body 读取失败", err)
	}
}
