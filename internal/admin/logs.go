package admin

import (
	"io"
	"math"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ── 日志尾部 ────────────────────────────────────────────────────────

// tailLines 读日志尾部若干行。
//
// 只读文件末尾一块而非整读：日志随运行无限增长，整读既慢又可能吃掉几百 MB 内存，
// 而我们只关心最近这些行。代价是截断点可能落在某行中间，故丢弃第一行
// （除非确实读到了文件头）。
func tailLines(path string, lines int, maxBytes int64) (out []string, truncated bool, size int64, mtime string) {
	fi, err := os.Stat(path)
	if err != nil {
		return []string{}, false, 0, ""
	}
	size = fi.Size()
	start := size - maxBytes
	if start < 0 {
		start = 0
	}
	f, err := os.Open(path)
	if err != nil {
		return []string{}, false, size, ""
	}
	defer f.Close()

	buf := make([]byte, size-start)
	if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
		return []string{}, false, size, ""
	}
	text := string(buf)
	if start > 0 {
		// 从中间截断时，开头多半是半行（甚至是半个多字节字符）——丢掉它
		i := strings.IndexByte(text, '\n')
		if i < 0 {
			text = ""
		} else {
			text = text[i+1:]
		}
	}
	all := strings.Split(text, "\n")
	if n := len(all); n > 0 && all[n-1] == "" {
		all = all[:n-1] // 末尾换行产生的空串
	}
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return all, start > 0, size, isoUTC(fi.ModTime())
}

// logs 日志页数据。lines 夹到 1..5000：要能看趋势，但也不能让一次请求把浏览器卡住。
func (h *Handler) logs(w http.ResponseWriter, r *http.Request) {
	lines := clampInt(r.URL.Query().Get("lines"), 1, 5000, 500)
	ls, truncated, size, mtime := tailLines(h.cfg.LogPath, lines, 512*1024)
	var mtimeAny any
	if mtime != "" {
		mtimeAny = mtime
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":      h.cfg.LogPath,
		"lines":     ls,
		"truncated": truncated,
		"size":      size,
		"mtime":     mtimeAny,
	})
}

// ── 请求统计（解析日志里的结构化行）────────────────────────────────

// chatRow 日志里一条结构化的请求记录。
//
// 网关把每个请求打成一行（internal/server/logging.go 的 logChatRow）：
//
//	| #462 | 15:03:08 | deepseek-v4 | stream | 200 | uid=249b4f55 | TTFB=4326ms | tok=94 | 19.9tok/s | total=4.7s |
//
// 两个格式限制来自 logChatRow 本身，不是解析器的问题：
//  1. 时间只有 HH:MM:SS，没有日期 —— 日期得靠同文件里带 YYYY/MM/DD 的 log.Printf 行
//     重建，所以解析必须按行序推进，不能按时间戳排序。
//  2. 模型名被截到 11 字符，deepseek-v4 实为 deepseek-v4.1-flash。故只能按截断名分组。
//
// `#N` 是进程内的原子计数，重启即从 1 重来，同一文件里会出现多个 #001，不能当唯一标识。
type chatRow struct {
	ts       string // 完整时间戳（日期锚点 + 行内时间）；无锚点时为 HH:MM:SS
	model    string
	mode     string
	status   int
	uid      string
	ttfbMs   *int
	tok      *int
	totalSec float64
}

var (
	chatRowRE = regexp.MustCompile(
		`^\|\s*#\d+\s*\|\s*(\d{2}:\d{2}:\d{2})\s*\|\s*(\S+)\s*\|\s*(\S+)\s*\|\s*(\d{3})\s*\|` +
			`\s*uid=(\S+)\s*\|\s*TTFB=(\S+)\s*\|\s*tok=(\S+)\s*\|\s*(\S+)tok/s\s*\|\s*total=([\d.]+)s\s*\|`)
	// dateAnchorRE 带日期的 log.Printf 行，用于给后续 chat 行补日期。
	dateAnchorRE = regexp.MustCompile(`^(\d{4})/(\d{2})/(\d{2}) (\d{2}:\d{2}:\d{2})`)
	ttfbRE       = regexp.MustCompile(`^(\d+)ms$`)
)

func parseChatRow(line, date string) *chatRow {
	m := chatRowRE.FindStringSubmatch(line)
	if m == nil {
		return nil
	}
	status, _ := strconv.Atoi(m[4])
	total, _ := strconv.ParseFloat(m[9], 64)
	r := &chatRow{model: m[2], mode: m[3], status: status, uid: m[5], totalSec: total}
	if date != "" {
		r.ts = date + "T" + m[1]
	} else {
		r.ts = m[1]
	}
	if mm := ttfbRE.FindStringSubmatch(m[6]); mm != nil {
		if n, err := strconv.Atoi(mm[1]); err == nil {
			r.ttfbMs = &n
		}
	}
	// `-` = 上游未给 usage
	if m[7] != "-" {
		if n, err := strconv.Atoi(m[7]); err == nil {
			r.tok = &n
		}
	}
	return r
}

// parseRowTS 解析 chat 行的完整时间戳。行里没有时区标记，而网关与本面板同机，
// 故按本地时区解释（与 JS 的 new Date("YYYY-MM-DDTHH:MM:SS") 一致）。
func parseRowTS(ts string) (time.Time, bool) {
	t, err := time.ParseInLocation("2006-01-02T15:04:05", ts, time.Local)
	return t, err == nil
}

// percentile 升序分位数（最近邻法，样本少时也稳定）。
func percentile(sorted []float64, p float64) any {
	if len(sorted) == 0 {
		return nil
	}
	i := int(math.Ceil(p / 100 * float64(len(sorted))))
	i--
	if i < 0 {
		i = 0
	}
	if i > len(sorted)-1 {
		i = len(sorted) - 1
	}
	return sorted[i]
}

type statGroup struct {
	key          string
	requests     int
	ok           int
	tokens       int64
	tokSamples   int
	ttfbSum      int64
	ttfbSamples  int
	tokpsSum     float64
	tokpsSamples int
}

func (g *statGroup) accumulate(r *chatRow) {
	g.requests++
	if r.status == 200 {
		g.ok++
	}
	if r.tok != nil {
		g.tokens += int64(*r.tok)
		g.tokSamples++
		if r.totalSec > 0 {
			g.tokpsSum += float64(*r.tok) / r.totalSec
			g.tokpsSamples++
		}
	}
	if r.ttfbMs != nil {
		g.ttfbSum += int64(*r.ttfbMs)
		g.ttfbSamples++
	}
}

func (g *statGroup) finalize() map[string]any {
	var avgTtfb, avgTokps any
	if g.ttfbSamples > 0 {
		avgTtfb = int(math.Round(float64(g.ttfbSum) / float64(g.ttfbSamples)))
	}
	if g.tokpsSamples > 0 {
		avgTokps = g.tokpsSum / float64(g.tokpsSamples)
	}
	rate := 0.0
	if g.requests > 0 {
		rate = float64(g.ok) / float64(g.requests)
	}
	return map[string]any{
		"key":         g.key,
		"requests":    g.requests,
		"ok":          g.ok,
		"errors":      g.requests - g.ok,
		"successRate": rate,
		"tokens":      g.tokens,
		"avgTtfbMs":   avgTtfb,
		"avgTokps":    avgTokps,
	}
}

// stats 扫描日志尾部并按窗口聚合。
//
// windowHours 为 nil 表示不限时间（用上全部读到的行）。窗口判定依赖行里的时间戳，
// 而时间戳只有 HH:MM:SS —— 无日期锚点时无法判断跨天，此时退化为统计全部读到的行，
// 并在 windowNote 里如实告知。
func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.collectStats(parseWindow(r.URL.Query().Get("hours"))))
}

// parseWindow 解析 hours 参数：0 / all / 缺省 = 不限窗口；否则夹到 1..720 小时（30 天）。
func parseWindow(raw string) *float64 {
	if raw == "" || raw == "0" || raw == "all" {
		return nil
	}
	n, err := strconv.ParseFloat(raw, 64)
	if err != nil || n == 0 {
		n = 1 // 与原先 Math.max(Number(raw) || 0, 1) 一致：解析不出来就当下限
	}
	if n < 1 {
		n = 1
	}
	if n > 720 {
		n = 720
	}
	return &n
}

func (h *Handler) collectStats(windowHours *float64) map[string]any {
	lines, truncated, size, mtime := tailLines(h.cfg.LogPath, 5000, 4*1024*1024)

	var rows []*chatRow
	date := ""
	for _, line := range lines {
		if m := dateAnchorRE.FindStringSubmatch(line); m != nil {
			date = m[1] + "-" + m[2] + "-" + m[3]
			continue
		}
		if r := parseChatRow(line, date); r != nil {
			rows = append(rows, r)
		}
	}

	// 窗口裁剪：以最后一个带完整时间戳的行为基准往前推。
	// ts 只有 HH:MM:SS 的行（出现在第一个日期锚点之前，截断读取时会发生）无法定位日期，
	// 这类行只能排除在窗口之外，并如实告知条数。
	windowed := rows
	windowNote := ""
	undated := 0
	for _, r := range rows {
		if !strings.Contains(r.ts, "T") {
			undated++
		}
	}
	if windowHours != nil && len(rows) > 0 {
		if last, ok := parseRowTS(rows[len(rows)-1].ts); ok {
			cutoff := last.Add(-time.Duration(*windowHours * float64(time.Hour)))
			windowed = make([]*chatRow, 0, len(rows))
			for _, r := range rows {
				if t, ok := parseRowTS(r.ts); ok && !t.Before(cutoff) {
					windowed = append(windowed, r)
				}
			}
			if undated > 0 {
				windowNote = "另有 " + strconv.Itoa(undated) + " 行因日志被截断、无法定位日期而未计入"
			}
		} else {
			// 整段都没有日期锚点：只能统计全部读到的行，跨天会失准
			windowNote = "日志里没有日期行，无法按小时窗口裁剪，已改为统计读到的全部行"
		}
	}

	var sortedTtfb []float64
	var sortedTokps []float64
	for _, r := range windowed {
		if r.ttfbMs != nil {
			sortedTtfb = append(sortedTtfb, float64(*r.ttfbMs))
		}
		if r.tok != nil && r.totalSec > 0 {
			sortedTokps = append(sortedTokps, float64(*r.tok)/r.totalSec)
		}
	}
	sort.Float64s(sortedTtfb)
	sort.Float64s(sortedTokps)

	byModel := map[string]*statGroup{}
	byAccount := map[string]*statGroup{}
	statusCounts := map[int]int{}
	var tokensTotal int64
	okCount := 0
	for _, r := range windowed {
		if r.status == 200 {
			okCount++
		}
		if r.tok != nil {
			tokensTotal += int64(*r.tok)
		}
		statusCounts[r.status]++
		gm := byModel[r.model]
		if gm == nil {
			gm = &statGroup{key: r.model}
			byModel[r.model] = gm
		}
		gm.accumulate(r)
		ga := byAccount[r.uid]
		if ga == nil {
			ga = &statGroup{key: r.uid}
			byAccount[r.uid] = ga
		}
		ga.accumulate(r)
	}

	var ttfbAvg, ttfbMax any
	var ttfbSum float64
	for _, v := range sortedTtfb {
		ttfbSum += v
	}
	if len(sortedTtfb) > 0 {
		ttfbAvg = int(math.Round(ttfbSum / float64(len(sortedTtfb))))
		ttfbMax = sortedTtfb[len(sortedTtfb)-1]
	}
	var tokpsAvg any
	if len(sortedTokps) > 0 {
		var s float64
		for _, v := range sortedTokps {
			s += v
		}
		tokpsAvg = s / float64(len(sortedTokps))
	}

	var mtimeAny any
	if mtime != "" {
		mtimeAny = mtime
	}
	var firstTs, lastTs any
	if len(windowed) > 0 {
		firstTs = windowed[0].ts
		lastTs = windowed[len(windowed)-1].ts
	}

	return map[string]any{
		"windowHours":   windowHours,
		"windowNote":    windowNote,
		"truncated":     truncated,
		"logSize":       size,
		"logMtime":      mtimeAny,
		"totalRequests": len(windowed),
		"ok":            okCount,
		"errors":        len(windowed) - okCount,
		"successRate":   rate(len(windowed), okCount),
		"ttfb": map[string]any{
			"samples": len(sortedTtfb),
			"avg":     ttfbAvg,
			"p50":     percentile(sortedTtfb, 50),
			"p95":     percentile(sortedTtfb, 95),
			"max":     ttfbMax,
		},
		"tokps": map[string]any{
			"samples": len(sortedTokps),
			"avg":     tokpsAvg,
			"p50":     percentile(sortedTokps, 50),
			"p95":     percentile(sortedTokps, 95),
		},
		"tokensTotal":  tokensTotal,
		"statusCounts": statusCountList(statusCounts),
		"byModel":      groupList(byModel),
		"byAccount":    groupList(byAccount),
		"firstTs":      firstTs,
		"lastTs":       lastTs,
	}
}

func rate(total, ok int) float64 {
	if total == 0 {
		return 0
	}
	return float64(ok) / float64(total)
}

// statusCountList 状态码分布，按出现次数降序。
func statusCountList(m map[int]int) []map[string]any {
	out := make([]map[string]any, 0, len(m))
	for status, count := range m {
		out = append(out, map[string]any{"status": status, "count": count})
	}
	sort.Slice(out, func(i, j int) bool {
		ci, cj := out[i]["count"].(int), out[j]["count"].(int)
		if ci != cj {
			return ci > cj
		}
		return out[i]["status"].(int) < out[j]["status"].(int) // 次数相同按状态码升序，保证输出稳定
	})
	return out
}

// groupList 分组结果，按请求数降序。
func groupList(m map[string]*statGroup) []map[string]any {
	out := make([]map[string]any, 0, len(m))
	for _, g := range m {
		out = append(out, g.finalize())
	}
	sort.Slice(out, func(i, j int) bool {
		ri, rj := out[i]["requests"].(int), out[j]["requests"].(int)
		if ri != rj {
			return ri > rj
		}
		return out[i]["key"].(string) < out[j]["key"].(string) // 同上，消除 map 遍历的随机顺序
	})
	return out
}

// ── 调度状态 ────────────────────────────────────────────────────────
//
// 网关不暴露调度状态，这里只能推算，且推算的基准必须可靠：
//
//  1. 时点表：优先用日志里最后一次启动打的「签到已启用：[9 21] 点」。为什么不用
//     config.json？网关启动时读一次配置就再也不看 —— 改了文件没重启，配置已经不是
//     进程实际在跑的东西，拿它推算会给出一个看似精确的错答案。
//  2. 已跑过：用「最后一次启动之后」的任务行判定，避免把重启前那趟算进来。
//
// 已知局限（UI 里如实标注）：进程跑满 24h 后日志只剩末尾一块，可能读不到启动行，
// 此时时点表回落 config.json 并标出 source=config。

var (
	// 网关启动块的开头（cmd/server/main.go 的 "loaded N account(s)"）
	startupBlockRE = regexp.MustCompile(`loaded \d+ account\(s\)`)
	// 兜底：日志被截断时可能只剩这一行
	startupTailRE = regexp.MustCompile(`listening on `)
	// 带完整日期的行「2026/09/12 07:10:44」。网关与本面板同机，按本地时区解释。
	logTSRE = regexp.MustCompile(`^(\d{4})/(\d{2})/(\d{2}) (\d{2}):(\d{2}):(\d{2})`)
	// 调度器打的执行行，如「2026/09/12 13:02:01 checkin 14da1d4c-...: ...」
	taskLineRE = regexp.MustCompile(`^(?:checkin|travel|activity|keepalive) [0-9a-f]{8}`)
	// 启动时的开关行，启用时带「：[9 21] 点」
	enabledLineRE = regexp.MustCompile(`(签到|猫猫旅行|活跃上报|token 保活)已(启用|禁用)(：\[[\d\s]+\]\s*点)?`)
	hoursRE       = regexp.MustCompile(`\[([\d\s]+)\]\s*点`)
)

// taskKeys 四个排程任务的固定顺序（也决定了界面上的顺序）。
var taskKeys = []string{"checkin", "travel", "activity", "keepalive"}

var nameToKey = map[string]string{
	"签到":       "checkin",
	"猫猫旅行":     "travel",
	"活跃上报":     "activity",
	"token 保活": "keepalive",
}

type taskSlot struct {
	hours    []int
	enabled  bool
	lastRun  *time.Time
	declared bool // 开关/时点是否由运行中的进程在启动日志里声明过
}

// parseHours 从「签到已启用：[9 21] 点」里取回时点表；禁用行不带方括号，返回 nil。
func parseHours(line string) []int {
	m := hoursRE.FindStringSubmatch(line)
	if m == nil {
		return nil
	}
	var out []int
	for _, f := range strings.Fields(m[1]) {
		if h, err := strconv.Atoi(f); err == nil && h >= 0 && h <= 23 {
			out = append(out, h)
		}
	}
	sort.Ints(out)
	return out
}

// afterLastStartup 返回日志里最后一次启动之后的部分。
// 必须从启动块的**开头**切：开关行打在 "listening on" 之前，从后者切会把它们丢掉。
// 找不到块首（日志被截断）时退回 listening 行，并让调用方标注。
func afterLastStartup(lines []string) (out []string, found bool) {
	for i := len(lines) - 1; i >= 0; i-- {
		if startupBlockRE.MatchString(lines[i]) {
			return lines[i+1:], true
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if startupTailRE.MatchString(lines[i]) {
			return lines[i+1:], false
		}
	}
	return lines, false
}

// nextRunOf 下一次运行时间。与网关的排程同规则：只看时钟，不看上一趟跑没跑成功
// （纯 cron，到点就跑，跑失败也照样等下一个整点）。当天剩下的时点里取最近的；
// 都过了就取明天的第一个。
func nextRunOf(hours []int, now time.Time) time.Time {
	sorted := append([]int(nil), hours...)
	sort.Ints(sorted)
	for _, h := range sorted {
		if h > now.Hour() {
			return time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		}
	}
	return time.Date(now.Year(), now.Month(), now.Day(), sorted[0], 0, 0, 0, now.Location()).AddDate(0, 0, 1)
}

// schedule 推算排程状态。now 由调用方给，便于测试。
func (h *Handler) schedule(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.collectSchedule(time.Now()))
}

func (h *Handler) collectSchedule(now time.Time) map[string]any {
	lines, logTruncated, _, _ := tailLines(h.cfg.LogPath, 5000, 4*1024*1024)
	scoped, startupFound := afterLastStartup(lines)

	slots := map[string]*taskSlot{}
	for _, k := range taskKeys {
		slots[k] = &taskSlot{}
	}

	for _, line := range scoped {
		m := logTSRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		rest := strings.TrimLeft(line[len(m[0]):], " \t")

		// 开关行只在启动时打，出现即代表进程当前的排程
		if en := enabledLineRE.FindStringSubmatch(rest); en != nil {
			if key, ok := nameToKey[en[1]]; ok {
				slots[key].enabled = en[2] == "启用"
				slots[key].declared = true
				// 禁用行不带时点，保留 config 回落值用于展示「配的是几点」
				if hours := parseHours(rest); len(hours) > 0 {
					slots[key].hours = hours
				}
			}
		}

		// 执行行记下最后一趟的时间。注意：调度器只在**出错或跳过**时打日志
		// （成功路径静默），所以这只是「最后一次有记录的执行」，不代表任务没跑过。
		if taskLineRE.MatchString(rest) {
			key := strings.SplitN(rest, " ", 2)[0]
			if s := slots[key]; s != nil {
				y, _ := strconv.Atoi(m[1])
				mo, _ := strconv.Atoi(m[2])
				d, _ := strconv.Atoi(m[3])
				hh, _ := strconv.Atoi(m[4])
				mi, _ := strconv.Atoi(m[5])
				ss, _ := strconv.Atoi(m[6])
				t := time.Date(y, time.Month(mo), d, hh, mi, ss, 0, time.Local)
				s.lastRun = &t
			}
		}
	}

	// 日志里没声明过的任务（进程跑了很久、日志只剩末尾一块）回落 config.json。
	// 但配置改了没重启就不是进程实际在跑的排程，故逐条标注来源。
	needFallback := false
	for _, k := range taskKeys {
		if !slots[k].declared {
			needFallback = true
			break
		}
	}
	if needFallback {
		var fc struct {
			Schedule struct {
				CheckinHours     []int `json:"checkin_hours"`
				TravelHours      []int `json:"travel_hours"`
				ActivityHours    []int `json:"activity_hours"`
				KeepaliveHours   []int `json:"keepalive_hours"`
				CheckinEnabled   *bool `json:"checkin_enabled"`
				TravelEnabled    *bool `json:"travel_enabled"`
				ActivityEnabled  *bool `json:"activity_enabled"`
				KeepaliveEnabled *bool `json:"keepalive_enabled"`
			} `json:"schedule"`
		}
		if err := h.readConfig(&fc); err == nil {
			s := fc.Schedule
			// 键缺席即 true / 回落默认，与 Go 侧 Default()+Unmarshal 的语义一致
			hours := map[string][]int{
				"checkin":   orHours(s.CheckinHours, 9, 21),
				"travel":    orHours(s.TravelHours, 9, 21),
				"activity":  orHours(s.ActivityHours, 10),
				"keepalive": orHours(s.KeepaliveHours, 22),
			}
			enabled := map[string]bool{
				"checkin":   s.CheckinEnabled == nil || *s.CheckinEnabled,
				"travel":    s.TravelEnabled == nil || *s.TravelEnabled,
				"activity":  s.ActivityEnabled == nil || *s.ActivityEnabled,
				"keepalive": s.KeepaliveEnabled == nil || *s.KeepaliveEnabled,
			}
			for _, k := range taskKeys {
				// 时点：禁用的任务启动行不带方括号，仍从 config 补上「配的是几点」供展示
				if len(slots[k].hours) == 0 {
					slots[k].hours = hours[k]
				}
				// 开关：进程声明过就以进程为准（config 可能改了没重启）
				if !slots[k].declared {
					slots[k].enabled = enabled[k]
				}
			}
		}
	}

	tasks := make([]map[string]any, 0, len(taskKeys))
	for _, k := range taskKeys {
		s := slots[k]
		hours := s.hours
		if hours == nil {
			hours = []int{} // JSON 里给 []，别给 null
		}
		var nextRun, lastRun any
		if s.enabled && len(s.hours) > 0 {
			nextRun = isoUTC(nextRunOf(s.hours, now))
		}
		if s.lastRun != nil {
			lastRun = isoUTC(*s.lastRun)
		}
		source := "config"
		if s.declared {
			source = "log"
		}
		// manual 是「立即执行」的运行态；从未手动跑过为 null。
		// 与 lastRun 是两个来源：lastRun 从日志推算（含定时那趟），manual 只记手动。
		//
		// 显式判空而非直接塞 snapshot(k)：后者返回的是 *taskRun，nil 指针装进 any
		// 会得到一个「非 nil 的接口包着 nil 指针」，`v != nil` 为真 —— 调用方与测试
		// 都会误判成「跑过」。JSON 序列化虽然都出 null，但 Go 侧语义已经错了。
		var manual any
		if mr := h.runs.snapshot(k); mr != nil {
			manual = mr
		}
		tasks = append(tasks, map[string]any{
			"key":     k,
			"label":   taskLabel[k],
			"note":    taskNote[k],
			"enabled": s.enabled,
			"hours":   hours,
			"source":  source,
			"lastRun": lastRun,
			// 成功时不打日志，lastRun 因此偏旧，UI 需说明
			"quiet":   taskQuiet[k],
			"nextRun": nextRun,
			"manual":  manual,
		})
	}

	return map[string]any{
		"now":          isoUTC(now),
		"startupFound": startupFound, // false = 时点表没有运行中进程背书
		"logTruncated": logTruncated,
		"tasks":        tasks,
	}
}

var (
	taskLabel = map[string]string{
		"checkin":   "签到",
		"travel":    "猫猫旅行",
		"activity":  "活跃上报",
		"keepalive": "token 保活",
	}
	taskNote = map[string]string{
		"checkin":   "非 global 账号签到 + 查余额解冻（global 区无此活动）",
		"travel":    "独立排程：领养 / 派出 / 领奖，一趟只做一个动作",
		"activity":  "每个账号上报一次对话活跃，点亮连登",
		"keepalive": "刷新所有账号 token；session 死亡的会被禁用",
	}
	// 调度器只在出错/跳过时打日志，成功路径静默 —— 这几个任务的「上次」天然偏旧
	taskQuiet = map[string]bool{
		"checkin":   true,
		"travel":    false,
		"activity":  true,
		"keepalive": true,
	}
)

func orHours(v []int, def ...int) []int {
	if v == nil {
		return def
	}
	return v
}

// isoUTC 与 JS 的 Date#toISOString 同形（毫秒精度、UTC、Z 结尾）。
func isoUTC(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// clampInt 解析整数并夹到 [lo, hi]；解析失败或为 0 时用 def。
func clampInt(s string, lo, hi, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n == 0 {
		n = def
	}
	if n < lo {
		n = lo
	}
	if n > hi {
		n = hi
	}
	return n
}
