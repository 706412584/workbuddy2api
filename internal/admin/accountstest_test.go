package admin

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/upstream"
)

// newTestUpstream 造一个指向上游替身的 Client。handler 决定它回什么、卡多久。
func newTestUpstream(handler http.HandlerFunc) (*upstream.Client, func()) {
	srv := httptest.NewServer(handler)
	c := upstream.New()
	// 账号 domain 为空 → Region()=cn → chatBase 走 ChatBaseCN，钉到替身即可。
	c.ChatBaseCN = srv.URL
	return c, srv.Close
}

func writeTestAuth(t *testing.T, dir, uid string) {
	t.Helper()
	raw := `{"account":{"uid":"` + uid + `","nickname":"t"},"auth":{"accessToken":"tok","expiresAt":9999999999}}`
	if err := os.WriteFile(filepath.Join(dir, "workbuddy-"+uid+".json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

// shrinkBudget 把总闸缩短，用例不必真等 40s。
func shrinkBudget(t *testing.T, d time.Duration) {
	t.Helper()
	old := accountTestBudget
	accountTestBudget = d
	t.Cleanup(func() { accountTestBudget = old })
}

// TestAccountTestTotalBudget 上游收下请求后永不响应：
// ChatStream 的头超时是 120s，没有外层总闸时测试会挂到那里；
// 有总闸时必须在预算内返回明确的超时结果（这正是「测试按钮没超时」的修复点）。
func TestAccountTestTotalBudget(t *testing.T) {
	shrinkBudget(t, 300*time.Millisecond)
	// 清理顺序（LIFO）：后注册的 close(hang) 先跑，放掉挂起的 handler，
	// 然后 closeSrv 才安全 —— httptest.Server.Close 会等在途请求，反序即死锁。
	hang := make(chan struct{})
	up, closeSrv := newTestUpstream(func(w http.ResponseWriter, r *http.Request) {
		<-hang // 永远不回：模拟上游黑洞
	})
	t.Cleanup(closeSrv)
	t.Cleanup(func() { close(hang) })

	dir := t.TempDir()
	writeTestAuth(t, dir, "uid-hang")
	h := New(Config{AuthDir: dir, Upstream: up})

	started := time.Now()
	res := h.runAccountTest("uid-hang", "deepseek-v4.1-flash")
	elapsed := time.Since(started)

	if res.OK {
		t.Error("上游无响应不应判通")
	}
	if elapsed > 3*time.Second {
		t.Errorf("耗时 %v：外层总闸没生效（预算 300ms）", elapsed)
	}
	if !strings.Contains(res.Message, "上限") {
		t.Errorf("Message = %q，应为总闸超时文案", res.Message)
	}
}

// TestAccountTestSuccessStillWorks 总闸不能误伤快路径：上游秒回 SSE 时测试照常通过。
func TestAccountTestSuccessStillWorks(t *testing.T) {
	shrinkBudget(t, 5*time.Second)
	up, closeSrv := newTestUpstream(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
	})
	t.Cleanup(closeSrv)

	dir := t.TempDir()
	writeTestAuth(t, dir, "uid-ok")
	h := New(Config{AuthDir: dir, Upstream: up})

	res := h.runAccountTest("uid-ok", "deepseek-v4.1-flash")
	if !res.OK {
		t.Errorf("应判通，实际 Message=%q", res.Message)
	}
}
