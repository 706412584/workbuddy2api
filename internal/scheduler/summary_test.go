package scheduler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// 四个 Run*Now 的返回值是面板「立即执行」唯一的结果来源（服务端不等执行完，
// 摘要就是回执）。这些用例锁定摘要非空且能区分成败 —— 摘要为空会让面板显示
// 「执行中…」结束后一片空白，用户不知道到底跑没跑。

// TestRunActivityNowSummary 摘要要反映各档计数，且带上每号条数。
func TestRunActivityNowSummary(t *testing.T) {
	fastActivity(t)
	stub := &reportStub{}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "ok", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "dis", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "notoken", AccessToken: "", RefreshToken: "", ExpiresAt: 9999999999})
	p.Disable("dis", "test")
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, ActivityReportCount: 1})

	got := s.RunActivityNow()
	// 2 个跳过（disabled + 无 token）、1 个完成、0 失败。
	for _, want := range []string{"上报完成 1", "失败 0", "跳过 2", "每号 1 条"} {
		if !strings.Contains(got, want) {
			t.Errorf("摘要 %q 缺 %q", got, want)
		}
	}
}

// TestRunActivityNowSummaryCountsFailure 上游报错要计入「失败」，不能算成完成。
func TestRunActivityNowSummaryCountsFailure(t *testing.T) {
	fastActivity(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", 500)
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, ActivityReportCount: 1})

	got := s.RunActivityNow()
	if !strings.Contains(got, "失败 1") || !strings.Contains(got, "上报完成 0") {
		t.Errorf("摘要 %q 应报 1 失败 0 完成", got)
	}
}

// TestRunKeepaliveNowSummary 保活摘要区分刷新成功/失败/跳过。
func TestRunKeepaliveNowSummary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "refresh") {
			w.Write([]byte(`{"code":0,"data":{"accessToken":"new","refreshToken":"new","expiresAt":9999999999}}`))
			return
		}
		http.Error(w, "not found", 404)
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "ok", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "nort", AccessToken: "at", RefreshToken: "", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	got := s.RunKeepaliveNow()
	for _, want := range []string{"刷新成功 1", "失败 0", "跳过 1"} {
		if !strings.Contains(got, want) {
			t.Errorf("摘要 %q 缺 %q", got, want)
		}
	}
	// 无新增禁用时不该出现那一项（避免摘要里挂个恒为 0 的噪音）。
	if strings.Contains(got, "新增禁用") {
		t.Errorf("无禁用却报了禁用: %q", got)
	}
}

// TestRunTravelNowSummaryNoAccounts 空池的摘要要说明「没有可巡检的账号」，
// 而不是空串 —— 空串在面板上会显示成什么都没发生。
func TestRunTravelNowSummaryNoAccounts(t *testing.T) {
	p := pool.New("")
	up := &upstream.Client{HTTP: http.DefaultClient}
	s := New(Config{Pool: p, Upstream: up})

	got := s.RunTravelNow()
	if got == "" {
		t.Fatal("空池摘要不应为空")
	}
	if !strings.Contains(got, "没有可巡检") {
		t.Errorf("摘要=%q 应说明没有账号", got)
	}
}

// TestTravelSkipsGlobalAdopt global 区的 buddy/agreement 实测恒 500（CN 同请求正常），
// 调度器必须跳过领养而不是每趟白打 18 次必然失败的请求、报 18 条「失败」。
func TestTravelSkipsGlobalAdopt(t *testing.T) {
	var agreementCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "buddy/agreement") {
			atomic.AddInt32(&agreementCalls, 1)
			http.Error(w, `{"code":500,"msg":"internal server error"}`, 500)
			return
		}
		if strings.Contains(r.URL.Path, "buddy/info") {
			w.Write([]byte(`{"code":0,"data":{"buddy":null}}`)) // 无猫
			return
		}
		http.Error(w, "not found", 404)
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL,
		ChatBaseGlobal: srv.URL, BillingBaseGl: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	got := s.RunTravelNow()
	if n := atomic.LoadInt32(&agreementCalls); n != 0 {
		t.Errorf("global 账号不应调用 agreement，实际调用 %d 次", n)
	}
	if !strings.Contains(got, "跳过 1") {
		t.Errorf("摘要 %q 应把 global 账号算作跳过", got)
	}
	if strings.Contains(got, "失败") {
		t.Errorf("摘要 %q 不应出现失败（不是账号故障）", got)
	}
}

// TestTravelCNStillAdopts CN 账号不受影响：仍会走 agreement 并领养。
// 这条是上一条的对照 —— 守卫只该拦 global。
func TestTravelCNStillAdopts(t *testing.T) {
	var agreementCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "buddy/agreement"):
			atomic.AddInt32(&agreementCalls, 1)
			w.Write([]byte(`{"code":0,"msg":"OK"}`))
		case strings.Contains(r.URL.Path, "buddy/first"):
			w.Write([]byte(`{"code":0,"msg":"OK"}`))
		case strings.Contains(r.URL.Path, "buddy/info"):
			w.Write([]byte(`{"code":0,"data":{"buddy":null}}`)) // 无猫 → 走领养
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "c1", AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: 9999999999, Domain: "copilot.tencent.com"})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL,
		ChatBaseGlobal: srv.URL, BillingBaseGl: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	got := s.RunTravelNow()
	if n := atomic.LoadInt32(&agreementCalls); n != 1 {
		t.Errorf("CN 账号应调用 agreement 1 次，实际 %d", n)
	}
	if !strings.Contains(got, "领养 1") {
		t.Errorf("摘要 %q 应报领养成功", got)
	}
}

// TestActivitySkipsGlobalStreakCheck global 区不做 streak 自检（该区端点恒 500，
// 调了必失败，只会给每个号刷一条 WARN 噪音）。
func TestActivitySkipsGlobalStreakCheck(t *testing.T) {
	fastActivity(t)
	var streakCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/report":
			w.Write([]byte(`{"code":0,"msg":"OK"}`))
		case strings.Contains(r.URL.Path, "growth/streak"):
			atomic.AddInt32(&streakCalls, 1)
			http.Error(w, `{"code":500,"msg":"internal server error"}`, 500)
		case strings.Contains(r.URL.Path, "buddy/info"):
			w.Write([]byte(`{"code":0,"data":{"buddy":null}}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL,
		ChatBaseGlobal: srv.URL, BillingBaseGl: srv.URL}
	s := New(Config{Pool: p, Upstream: up, ActivityReportCount: 1})

	s.RunActivityNow()
	if n := atomic.LoadInt32(&streakCalls); n != 0 {
		t.Errorf("global 账号不应调用 growth/streak，实际 %d 次", n)
	}
}

// TestRunCheckinNowSummaryCountsGlobalSkipped global 区账号在签到摘要里算「跳过」
// 而非「失败」—— 它本就没有签到活动，报失败会让运维去查一个不存在的问题。
func TestRunCheckinNowSummaryCountsGlobalSkipped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 余额查询（签到解冻用）返回正常值，让流程走完。
		if strings.Contains(r.URL.Path, "get-user-resource") {
			w.Write([]byte(`{"code":0,"data":{"remain":100}}`))
			return
		}
		http.Error(w, "not found", 404)
	}))
	defer srv.Close()

	p := pool.New("")
	// 只有 global 账号：签到跳过，但余额查询照走。
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"})
	// global 域走 BillingBaseGl（见 billingBase），两个 base 都要指向桩，
	// 否则余额查询打到空 URL 变 404，账号会被记成「失败」而掩盖了本用例要验的跳过语义。
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL,
		ChatBaseGlobal: srv.URL, BillingBaseGl: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	got := s.RunCheckinNow()
	if !strings.Contains(got, "跳过 1") {
		t.Errorf("摘要 %q 应把 global 账号算作跳过", got)
	}
	if !strings.Contains(got, "失败 0") {
		t.Errorf("摘要 %q 不应把 global 跳过算成失败", got)
	}
}
