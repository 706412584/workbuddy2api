package pool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestCallHistoryRingBuffer 环形缓冲：只保留最近 N 条，新在前。
func TestCallHistoryRingBuffer(t *testing.T) {
	p := New("")
	uid := "u1"
	for i := 0; i < CallHistorySize+5; i++ {
		ok := i%2 == 0
		reason := ""
		if !ok {
			reason = "err"
		}
		p.NoteCall(uid, ok, reason, "m", int64(i))
	}
	h := p.CallHistory(uid)
	if len(h) != CallHistorySize {
		t.Fatalf("保留 %d 条，want %d（环形上限）", len(h), CallHistorySize)
	}
	// 最后写入的是 i=CallHistorySize+4（偶数 → ok=true，Ms=该 i）
	if !h[0].OK || h[0].Ms != int64(CallHistorySize+4) {
		t.Errorf("首条应为最后一次调用（新在前），得到 %+v", h[0])
	}
	// 时间应单调不增
	for i := 1; i < len(h); i++ {
		if h[i].Time.After(h[i-1].Time) {
			t.Errorf("第 %d 条时间晚于前一条（应新→旧）", i)
		}
	}
}

// TestCallHistoryIsolatedPerAccount 各账号历史互不串扰。
func TestCallHistoryIsolatedPerAccount(t *testing.T) {
	p := New("")
	p.NoteCall("a", true, "", "m", 0)
	p.NoteCall("b", false, "429", "m", 0)
	if h := p.CallHistory("a"); len(h) != 1 || !h[0].OK {
		t.Errorf("a 的历史不对：%+v", h)
	}
	if h := p.CallHistory("b"); len(h) != 1 || h[0].OK || h[0].Reason != "429" {
		t.Errorf("b 的历史不对：%+v", h)
	}
	if h := p.CallHistory("nobody"); len(h) != 0 {
		t.Errorf("未知账号应返回空，得到 %d 条", len(h))
	}
}

// TestCallLogPersistsToJSONL 落盘：一行一条 JSON，含 uid 与原因。
func TestCallLogPersistsToJSONL(t *testing.T) {
	dir := t.TempDir()
	p := New("")
	p.EnableCallLog(dir, 7)
	defer p.CloseCallLog()
	p.NoteCall("u1", false, "403 账号封控(11140)", "glm-5.2", 1234)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("应有 1 个日志文件，得到 %d", len(entries))
	}
	name := entries[0].Name()
	if !strings.HasPrefix(name, "calls-") || !strings.HasSuffix(name, ".jsonl") {
		t.Errorf("文件名不符 calls-YYYYMMDD.jsonl：%s", name)
	}
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{`"uid":"u1"`, `"ok":false`, "11140", `"model":"glm-5.2"`} {
		if !strings.Contains(s, want) {
			t.Errorf("落盘内容缺 %s：%s", want, s)
		}
	}
}

// TestCallLogGCRemovesOldFiles 按天滚动清理：超期文件被删。
func TestCallLogGCRemovesOldFiles(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "calls-20200101.jsonl")
	if err := os.WriteFile(old, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	today := filepath.Join(dir, "calls-"+time.Now().Format("20060102")+".jsonl")
	if err := os.WriteFile(today, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New("")
	p.EnableCallLog(dir, 7)
	p.NoteCall("u1", true, "", "m", 0) // 触发一次 GC
	p.CloseCallLog()

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("超期文件应被删除：%s", old)
	}
	if _, err := os.Stat(today); err != nil {
		t.Errorf("当日文件不应被删除：%v", err)
	}
}

// TestCallLogDisabledByDefault 不调用 EnableCallLog 时不落盘（纯内存可用）。
func TestCallLogDisabledByDefault(t *testing.T) {
	p := New("")
	p.NoteCall("u1", true, "", "m", 0)
	if len(p.CallHistory("u1")) != 1 {
		t.Error("内存历史应仍可用")
	}
	if p.calls.sink != nil {
		t.Error("默认不应有落盘 sink")
	}
}

// TestNoteCallDoesNotTouchScheduling 调用历史**不参与**任何决策：
// 大量失败记录不应改变账号可用性/冷却（这是本函数的契约）。
func TestNoteCallDoesNotTouchScheduling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	for i := 0; i < 50; i++ {
		p.NoteCall("u1", false, "boom", "m", 0)
	}
	if st, ok := p.Status("u1"); ok && (st.Cooling || st.Disabled) {
		t.Errorf("NoteCall 不应改变调度状态：%+v", st)
	}
}
