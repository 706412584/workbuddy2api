package admin

import (
	"testing"
	"time"
)

// 真实的 chat 行（取自 data/gateway.log 里的实际输出）。
const sampleRow = `| #462 | 15:03:08 | deepseek-v4 | stream | 200 | uid=249b4f55 | TTFB=4326ms | tok=94 | 19.9tok/s | total=4.7s |`

func TestParseChatRow(t *testing.T) {
	r := parseChatRow(sampleRow, "2026-09-12")
	if r == nil {
		t.Fatal("没解析出来")
	}
	if r.ts != "2026-09-12T15:03:08" {
		t.Errorf("ts=%q", r.ts)
	}
	if r.model != "deepseek-v4" || r.mode != "stream" || r.status != 200 || r.uid != "249b4f55" {
		t.Errorf("字段不符: %+v", r)
	}
	if r.ttfbMs == nil || *r.ttfbMs != 4326 {
		t.Errorf("ttfb=%v", r.ttfbMs)
	}
	if r.tok == nil || *r.tok != 94 {
		t.Errorf("tok=%v", r.tok)
	}
	if r.totalSec != 4.7 {
		t.Errorf("total=%v", r.totalSec)
	}

	// 无日期锚点时退化为 HH:MM:SS（日志被截断时会遇到）
	r2 := parseChatRow(sampleRow, "")
	if r2 == nil || r2.ts != "15:03:08" {
		t.Errorf("无锚点: %+v", r2)
	}

	// 非 chat 行返回 nil，不得误判
	for _, line := range []string{
		"2026/09/12 21:32:32 pool: fallback_earliest_expiry uid=x until=y kind=soft",
		"",
		"| 表头 | 不是 | chat | 行 |",
	} {
		if parseChatRow(line, "2026-09-12") != nil {
			t.Errorf("误判为 chat 行: %q", line)
		}
	}
}

// TestParseChatRowMissingMetrics `-` 表示该项无值（非流式无 TTFB、上游未给 usage）。
func TestParseChatRowMissingMetrics(t *testing.T) {
	line := `| #2 | 21:32:32 | hy4-preview | stream | 503 | uid=4c8b4acf | TTFB=- | tok=- | -tok/s | total=11.65s |`
	r := parseChatRow(line, "2026-09-12")
	if r == nil {
		t.Fatal("没解析出来")
	}
	if r.ttfbMs != nil {
		t.Errorf("TTFB=- 应为 nil，得到 %v", *r.ttfbMs)
	}
	if r.tok != nil {
		t.Errorf("tok=- 应为 nil，得到 %v", *r.tok)
	}
	if r.status != 503 {
		t.Errorf("status=%d", r.status)
	}
}

func TestPercentile(t *testing.T) {
	s := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	cases := []struct {
		p    float64
		want float64
	}{
		{50, 5}, {95, 10}, {100, 10}, {1, 1},
	}
	for _, c := range cases {
		if got := percentile(s, c.p); got != c.want {
			t.Errorf("percentile(p=%v)=%v want %v", c.p, got, c.want)
		}
	}
	if percentile(nil, 50) != nil {
		t.Error("空样本应返回 nil")
	}
}

func TestParseWindow(t *testing.T) {
	if parseWindow("all") != nil || parseWindow("") != nil || parseWindow("0") != nil {
		t.Error("all/空/0 都应为不限窗口")
	}
	if got := parseWindow("3"); got == nil || *got != 3 {
		t.Errorf("3 → %v", got)
	}
	if got := parseWindow("9999"); got == nil || *got != 720 {
		t.Errorf("超上限应夹到 720，得到 %v", got)
	}
	if got := parseWindow("abc"); got == nil || *got != 1 {
		t.Errorf("解析不出来应落到下限 1，得到 %v", got)
	}
}

func TestParseHours(t *testing.T) {
	if got := parseHours("2026/09/12 07:10:44 签到已启用：[9 21] 点（签到 + 余额查询解冻）"); len(got) != 2 || got[0] != 9 || got[1] != 21 {
		t.Errorf("got %v", got)
	}
	// 禁用行不带方括号
	if got := parseHours("2026/09/12 07:10:44 签到已禁用（schedule.checkin_enabled=false）"); len(got) != 0 {
		t.Errorf("禁用行不该解出时点: %v", got)
	}
	// 乱序输入要排好
	if got := parseHours("[21 9 15] 点"); len(got) != 3 || got[0] != 9 || got[1] != 15 || got[2] != 21 {
		t.Errorf("未排序: %v", got)
	}
}

func TestAfterLastStartup(t *testing.T) {
	lines := []string{
		"2026/09/12 06:00:00 loaded 5 account(s) from ./auths",
		"2026/09/12 06:00:00 签到已启用：[9 21] 点",
		"2026/09/12 06:00:01 workbuddy2api listening on :7863",
		"2026/09/12 06:30:00 checkin 14da1d4c-x: 余额 100",
		"2026/09/12 21:00:00 loaded 6 account(s) from ./auths",
		"2026/09/12 21:00:00 签到已启用：[9 21] 点",
		"2026/09/12 21:00:01 workbuddy2api listening on :7863",
	}
	got, found := afterLastStartup(lines)
	if !found {
		t.Error("应找到启动块")
	}
	// 必须从「loaded」切，而不是从「listening on」——否则开关行会被丢掉
	if len(got) != 2 || got[0] != "2026/09/12 21:00:00 签到已启用：[9 21] 点" {
		t.Errorf("切分错误（很可能从 listening on 切了）: %#v", got)
	}

	// 日志被截断：只剩 listening 行 → found=false，且从它的下一行开始
	truncated := []string{"2026/09/12 21:00:01 workbuddy2api listening on :7863", "2026/09/12 21:05:00 travel x: 派出"}
	got, found = afterLastStartup(truncated)
	if found {
		t.Error("只剩 listening 行时不该报 found")
	}
	if len(got) != 1 {
		t.Errorf("got %#v", got)
	}
}

func TestNextRunOf(t *testing.T) {
	at := func(h, m int) time.Time {
		return time.Date(2026, 9, 12, h, m, 0, 0, time.Local)
	}
	// 当天还有时点 → 取最近的
	if got := nextRunOf([]int{9, 21}, at(15, 3)); got.Hour() != 21 || got.Day() != 12 {
		t.Errorf("15:03 之后应等当天 21:00，得到 %v", got)
	}
	// 当天都过了 → 取明天第一个
	if got := nextRunOf([]int{9, 21}, at(22, 30)); got.Hour() != 9 || got.Day() != 13 {
		t.Errorf("22:30 之后应等次日 09:00，得到 %v", got)
	}
	// 恰好在整点上 → 该时点已过（严格大于），取下一个
	if got := nextRunOf([]int{9, 21}, at(9, 0)); got.Hour() != 21 {
		t.Errorf("09:00 整点应算已过，得到 %v", got)
	}
	// 单时点
	if got := nextRunOf([]int{10}, at(11, 0)); got.Hour() != 10 || got.Day() != 13 {
		t.Errorf("单时点跨天失败: %v", got)
	}
}

// TestISOUTCFormat 前端按 JS 的 Date 语义解析这些字符串，格式必须与 toISOString 同形。
func TestISOUTCFormat(t *testing.T) {
	got := isoUTC(time.Date(2026, 9, 12, 21, 0, 0, 0, time.Local))
	if len(got) != 24 || got[10] != 'T' || got[23] != 'Z' {
		t.Errorf("格式不对（应形如 2026-09-12T13:00:00.000Z）: %s", got)
	}
	if _, err := time.Parse("2006-01-02T15:04:05.000Z", got); err != nil {
		t.Errorf("无法解析: %v", err)
	}
}

// TestClampInt lines 参数必须夹到 1..5000，否则一次请求可能把整个日志读进内存。
func TestClampInt(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"500", 500}, {"0", 500}, {"", 500}, {"abc", 500},
		{"99999", 5000}, {"-5", 1}, {"1", 1},
	}
	for _, c := range cases {
		if got := clampInt(c.in, 1, 5000, 500); got != c.want {
			t.Errorf("clampInt(%q)=%d want %d", c.in, got, c.want)
		}
	}
}
