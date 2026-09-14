// runtask.go 定时任务的「立即执行」：手动触发一次，并跟踪其运行态。
//
// 为什么是异步而不是同步返回结果：这四类任务都是逐账号串行打上游，且账号间有
// 防风控间隔（活跃上报每号 5 条 × 1.5s + 账号间 0.8s，19 个号约 130 秒；旅行与
// 签到也在数十秒级）。同步等完会让面板转圈一两分钟，还可能撞上浏览器/反代的
// 请求超时，最后既没结果也没进度。
//
// 于是：POST 立即返回「已开始」，任务在后台跑；运行态并进 GET /__admin/schedule
// 的响应（面板本来就在轮询它），跑完显示摘要。不新增状态端点，也不引入持久化 ——
// 这是进程内的一次性运行记录，重启即忘。
package admin

import (
	"net/http"
	"sync"
	"time"
)

// taskRun 一类任务的手动执行记录。
//
// 只记「手动触发」的，不掺入定时那趟：定时路径不经过本包，硬凑会让这里显示的
// 时间与 /__admin/schedule 里按日志推算的 lastRun 互相矛盾（两个来源、两套口径）。
// 面板上两者分列展示，各自标注来源。
type taskRun struct {
	Running  bool      `json:"running"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	// Summary 完成后的结果摘要（由调度器给出，如「19 个账号：成功 12 · 已签到 5…」）。
	// 失败也走这里 —— 唯一可预期的失败是「与定时那趟撞车」，摘要本身就是
	// 「未执行：…」，无需再分一个字段让前端拼文案。
	Summary string `json:"summary"`
}

// runTracker 手动执行的运行态表。
// 互斥范围仅覆盖表本身；任务本体在锁外跑（跑几分钟，不能占着锁）。
type runTracker struct {
	mu   sync.Mutex
	runs map[string]*taskRun
}

func newRunTracker() *runTracker {
	return &runTracker{runs: map[string]*taskRun{}}
}

// begin 尝试把任务标记为「执行中」。已有同名任务在跑则返回 false —— 这是防重复
// 触发的唯一闸门（面板连点、多人同时点、或点完不等就跑第二次）。
//
// 它挡不住「定时那趟正在跑」：调度器内部只有签到自己带互斥（CheckinAll 的 ErrBusy），
// 另外三类没有。真撞上的后果是同一趟活干两遍（对上游而言这些操作按自然日幂等），
// 可接受；为此在调度器里加一套全局锁不划算。
func (t *runTracker) begin(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.runs[key]
	if r != nil && r.Running {
		return false
	}
	// 保留上一次的 Summary/Finished 供对照，只覆盖本轮的开始态。
	if r == nil {
		r = &taskRun{}
		t.runs[key] = r
	}
	r.Running = true
	r.Started = time.Now()
	r.Summary = ""
	r.Finished = time.Time{}
	return true
}

// finish 记下结果。任务 panic 时也要走到这里（见 runScheduleTask 的 recover），
// 否则 Running 会永远挂在 true，该任务再也没法手动触发。
func (t *runTracker) finish(key, summary string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.runs[key]
	if r == nil {
		return
	}
	r.Running = false
	r.Finished = time.Now()
	r.Summary = summary
}

// snapshot 返回指定任务的运行态；从未手动执行过返回 nil。
func (t *runTracker) snapshot(key string) *taskRun {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.runs[key]
	if r == nil {
		return nil
	}
	// 拷一份出去：调用方要序列化它，不能让它继续被后台任务改写。
	cp := *r
	return &cp
}

// runScheduleTask POST /__admin/schedule/run —— 立即执行一次指定任务。
//
// 只负责触发与记状态，不等待执行完（理由见文件头）。
func (h *Handler) runScheduleTask(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Task string `json:"task"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if h.cfg.RunTask == nil {
		fail(w, http.StatusNotImplemented, "本进程未接线调度器，无法立即执行")
		return
	}
	label, known := taskLabel[body.Task]
	if !known {
		fail(w, http.StatusBadRequest, "未知任务 %q", body.Task)
		return
	}
	if !h.runs.begin(body.Task) {
		fail(w, http.StatusConflict, "%s 正在执行中，等它跑完再试", label)
		return
	}

	logf("手动执行 %s（%s）", label, body.Task)
	task := body.Task
	go func() {
		// 兜住 panic：任务里任何一处下标越界/空指针都会让这个 goroutine 直接死掉，
		// 那样 Running 永远停在 true —— 用户看到「执行中」不动，且再也点不动该任务。
		// 把 panic 记成摘要，至少让人知道发生了什么、能再点一次。
		defer func() {
			if p := recover(); p != nil {
				logf("手动执行 %s panic: %v", task, p)
				h.runs.finish(task, "执行出错（panic），详见网关日志")
			}
		}()
		summary := h.cfg.RunTask(task)
		h.runs.finish(task, summary)
		logf("手动执行 %s 完成：%s", task, summary)
	}()

	ok(w, map[string]any{"task": task, "label": label, "msg": "已开始执行"})
}
