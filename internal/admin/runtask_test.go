package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// newRunHandler 造一个只接线了 RunTask 的 handler，供「立即执行」用例共用。
func newRunHandler(run func(string) string) *Handler {
	return New(Config{RunTask: run})
}

func postRun(h *Handler, task string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/__admin/schedule/run",
		strings.NewReader(`{"task":"`+task+`"}`))
	req.RemoteAddr = "127.0.0.1:12345" // 绕开 isLocal 拦截
	h.ServeHTTP(rec, req)
	return rec
}

// TestRunScheduleTaskStartsAsync POST 只表示「已开始」，不等任务跑完。
// 这是刻意的：一趟全量要几十秒到两分钟，同步等会让面板转圈并可能撞上请求超时。
func TestRunScheduleTaskStartsAsync(t *testing.T) {
	release := make(chan struct{})
	done := make(chan struct{})
	h := newRunHandler(func(string) string {
		<-release // 卡住任务，模拟"跑了两分钟"
		close(done)
		return "完成"
	})

	start := time.Now()
	rec := postRun(h, "checkin")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// 任务仍卡着，但请求已经返回 —— 这就是异步的证据。
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("POST 阻塞了 %v，应当立即返回", elapsed)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["ok"] != true || resp["task"] != "checkin" || resp["label"] != "签到" {
		t.Errorf("响应不符: %v", resp)
	}

	// 期间运行态应为 running，且没有摘要。
	snap := h.runs.snapshot("checkin")
	if snap == nil || !snap.Running || snap.Summary != "" {
		t.Fatalf("执行中状态不符: %+v", snap)
	}

	close(release)
	<-done
	// 等 finish 落定（它在任务函数返回后才执行）。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s := h.runs.snapshot("checkin"); s != nil && !s.Running {
			if s.Summary != "完成" {
				t.Errorf("summary=%q want 完成", s.Summary)
			}
			if s.Finished.IsZero() {
				t.Error("finished 应被填充")
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("任务结束后 running 未复位")
}

// TestRunScheduleTaskRejectsConcurrent 同一任务执行中不得重复触发。
// 闸门是唯一防重复的手段：面板连点、多人同点都靠它挡。
func TestRunScheduleTaskRejectsConcurrent(t *testing.T) {
	release := make(chan struct{})
	h := newRunHandler(func(string) string {
		<-release
		return "完成"
	})

	if rec := postRun(h, "travel"); rec.Code != http.StatusOK {
		t.Fatalf("首次触发应成功，code=%d", rec.Code)
	}
	rec := postRun(h, "travel")
	if rec.Code != http.StatusConflict {
		t.Fatalf("并发触发应 409，code=%d body=%s", rec.Code, rec.Body)
	}
	// 报错要说清是哪个任务，否则用户不知道该等谁。
	if !strings.Contains(rec.Body.String(), "猫猫旅行") {
		t.Errorf("409 文案应含任务名: %s", rec.Body)
	}
	close(release)
}

// TestRunScheduleTaskAllowsSequential 上一趟跑完后可以再跑（不是一次性闸门）。
func TestRunScheduleTaskAllowsSequential(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	h := newRunHandler(func(string) string {
		mu.Lock()
		calls++
		mu.Unlock()
		return "完成"
	})

	for i := 0; i < 3; i++ {
		postRun(h, "keepalive")
		// 等这一趟结束（空任务瞬间完成，但要给 finish 一点时间）
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if s := h.runs.snapshot("keepalive"); s != nil && !s.Running {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 3 {
		t.Errorf("calls=%d want 3", calls)
	}
}

// TestRunScheduleTaskUnknownKey 未知任务名要 400，且不能占住运行态。
func TestRunScheduleTaskUnknownKey(t *testing.T) {
	h := newRunHandler(func(string) string { return "不该被调用" })
	rec := postRun(h, "nope")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", rec.Code)
	}
	if h.runs.snapshot("nope") != nil {
		t.Error("未知任务不应写入运行态")
	}
}

// TestRunScheduleTaskNoScheduler RunTask 未接线（nil）时报 501，而不是空跑。
func TestRunScheduleTaskNoScheduler(t *testing.T) {
	h := New(Config{})
	if rec := postRun(h, "checkin"); rec.Code != http.StatusNotImplemented {
		t.Fatalf("code=%d want 501", rec.Code)
	}
}

// TestRunScheduleTaskRecoversPanic 任务 panic 后 running 必须复位。
// 否则该任务永远显示「执行中」，且再也点不动 —— 一次越界就废掉一个按钮。
func TestRunScheduleTaskRecoversPanic(t *testing.T) {
	h := newRunHandler(func(string) string { panic("boom") })
	postRun(h, "activity")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s := h.runs.snapshot("activity"); s != nil && !s.Running {
			if !strings.Contains(s.Summary, "panic") {
				t.Errorf("panic 应写进摘要供人察觉，got %q", s.Summary)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("panic 后 running 未复位")
}

// TestRunTrackerSnapshotIsCopy 快照必须是副本：调用方要序列化它，
// 不能让它继续被后台任务改写（否则响应与状态可能不一致）。
func TestRunTrackerSnapshotIsCopy(t *testing.T) {
	tr := newRunTracker()
	if !tr.begin("checkin") {
		t.Fatal("begin 应成功")
	}
	snap := tr.snapshot("checkin")
	tr.finish("checkin", "完成")

	if !snap.Running {
		t.Error("快照被后续 finish 改写了")
	}
	if snap.Summary != "" {
		t.Errorf("快照摘要被改写: %q", snap.Summary)
	}
}
