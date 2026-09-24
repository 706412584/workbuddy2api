package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestWafSoftCooldownBounds WAF 403 的账号级软冷却时长落 [45s,75s]
// （60s 基数经 ±25% 抖动），且不 Disable、不喂熔断。
func TestWafSoftCooldownBounds(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 403, "<html>403 Forbidden</html>", false // WAF 拦截形态（HTML 无信封）
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 1})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	st, _ := p.Status("u1")
	if st.Disabled {
		t.Fatalf("WAF 403 must not disable the account (频控非账号故障): %+v", st)
	}
	if !st.Cooling {
		t.Fatalf("WAF 403 must soft-cool the account: %+v", st)
	}
	// CoolRemaining 单位秒；抖动区间 [45,75] 留 1s 余量。
	rem := time.Duration(st.CoolRemaining) * time.Second
	if rem < 44*time.Second || rem > 76*time.Second {
		t.Fatalf("WAF cooldown=%v want [45s,75s] (60s base ±25%% jitter)", rem)
	}
}
