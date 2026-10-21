// calls.go 账号级调用历史（环形缓冲 + 可选 JSONL 落盘）。
//
// 为什么需要（2026-10-21 排查受阻）：
//   - 失败**有**日志（`WARN: chat_stream uid=… upstream 429 …`，带时间戳可归因），
//     但**成功没有**：请求日志行 `| #NNN | …` **不带日期**且是批量刷盘的，file
//     位置不可靠。于是「这个号最近是被用了还是没被用」无法回答 —— 排查「被封号
//     是否因为闲置」时就卡在这里（实测被该字段误导过一次）。
//   - 账号级 last_err 只存**时间戳**，不存**原因**，看不出失败性质。
//
// 本文件给每个账号保留最近 N 次调用（时间 + 成功/失败 + 原因 + 耗时），
// 使「这个号最近发生了什么」可以一眼回答。
//
// 设计取舍：
//   - **环形缓冲**而非无限追加：内存有界（每号 20 条），不需要清理逻辑。
//   - 原因直接复用既有错误分类的**人类可读串**（由调用方传入），不重复判定。
//   - 落盘为可选 JSONL（按天滚动、保留 N 天）：内存重启即丢，而本功能的核心
//     用途恰恰是**回溯过去**，故默认落盘。
package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// CallHistorySize 每账号保留的调用条数。
// 20 条足够回答「最近有没有被用 / 一直失败还是一直成功」，且 26 号 × 20 条
// 内存占用可忽略（约 100KB 量级）。
const CallHistorySize = 20

// CallRecord 一次账号调用。
type CallRecord struct {
	// Time 调用时刻（RFC3339，本地时区）。
	Time time.Time `json:"t"`
	// OK 是否成功。
	OK bool `json:"ok"`
	// Reason 失败原因（人类可读，如「429 上游限流(code=14003)」）；成功时为空。
	Reason string `json:"reason,omitempty"`
	// Model 本次请求的模型（可为空 = 未知）。
	Model string `json:"model,omitempty"`
	// Ms 本次耗时（毫秒）。0 = 未测量。
	Ms int64 `json:"ms,omitempty"`
}

// callLog 全部账号的调用历史（受 mu 保护）。
type callLog struct {
	mu   sync.Mutex
	byID map[string][]CallRecord // uid → 最多 CallHistorySize 条，新→旧
	// sink 落盘目标（空 = 不落盘）。
	sink *callSink
}

// Add 追加一条调用记录（环形保留最近 CallHistorySize 条）。
func (l *callLog) Add(uid string, r CallRecord) {
	if l == nil || uid == "" {
		return
	}
	l.mu.Lock()
	buf := l.byID[uid]
	buf = append([]CallRecord{r}, buf...) // 新在前，便于面板直接取前 N
	if len(buf) > CallHistorySize {
		buf = buf[:CallHistorySize]
	}
	l.byID[uid] = buf
	sink := l.sink
	l.mu.Unlock()
	if sink != nil {
		sink.write(uid, r)
	}
}

// Get 返回某账号的调用历史副本（新→旧）。
func (l *callLog) Get(uid string) []CallRecord {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	buf := l.byID[uid]
	out := make([]CallRecord, len(buf))
	copy(out, buf)
	return out
}

// Snapshot 返回全部账号的调用历史（uid → 新→旧）。供面板批量展示。
func (l *callLog) Snapshot() map[string][]CallRecord {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string][]CallRecord, len(l.byID))
	for u, buf := range l.byID {
		c := make([]CallRecord, len(buf))
		copy(c, buf)
		out[u] = c
	}
	return out
}

// ── 落盘（可选）─────────────────────────────────────────────────────────
//
// 按天一个文件（calls-YYYYMMDD.jsonl），保留 keepDays 天。
// 用 append + 一行一条 JSON：崩溃时最多丢最后一行，且可 grep / 可被 jq 处理。
// 不做 fsync —— 调用日志不是账本，性能优先；重启丢最后几行可接受。
type callSink struct {
	mu       sync.Mutex
	dir      string
	keepDays int
	curDate  string
	f        *os.File
	// lastGC 上次清理日期，避免每次写都扫目录。
	lastGC string
}

func newCallSink(dir string, keepDays int) *callSink {
	if strings.TrimSpace(dir) == "" || keepDays <= 0 {
		return nil
	}
	return &callSink{dir: dir, keepDays: keepDays}
}

func (s *callSink) write(uid string, r CallRecord) {
	if s == nil {
		return
	}
	line, err := json.Marshal(struct {
		UID string `json:"uid"`
		CallRecord
	}{uid, r})
	if err != nil {
		return
	}
	line = append(line, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	day := time.Now().Format("20060102")
	if s.f == nil || s.curDate != day {
		if s.f != nil {
			_ = s.f.Close()
			s.f = nil
		}
		if err := os.MkdirAll(s.dir, 0o755); err != nil {
			return
		}
		f, err := os.OpenFile(filepath.Join(s.dir, "calls-"+day+".jsonl"),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return
		}
		s.f, s.curDate = f, day
	}
	_, _ = s.f.Write(line)

	if s.lastGC != day {
		s.lastGC = day
		s.gcLocked()
	}
}

// gcLocked 删除超过 keepDays 的 calls-*.jsonl。调用方须持锁。
func (s *callSink) gcLocked() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -s.keepDays)
	var names []string
	for _, e := range entries {
		n := e.Name()
		if !strings.HasPrefix(n, "calls-") || !strings.HasSuffix(n, ".jsonl") {
			continue
		}
		day := strings.TrimSuffix(strings.TrimPrefix(n, "calls-"), ".jsonl")
		t, err := time.ParseInLocation("20060102", day, time.Local)
		if err != nil {
			continue
		}
		if t.Before(cutoff) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		_ = os.Remove(filepath.Join(s.dir, n))
	}
}

// close 关闭当前文件句柄（进程退出时调用）。
func (s *callSink) close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f != nil {
		_ = s.f.Close()
		s.f = nil
	}
}
