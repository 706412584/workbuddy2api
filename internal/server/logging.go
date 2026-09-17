// logging.go 请求级表格日志：每个 /v1/chat/completions 请求结束后打印一行到 stdout。
package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// chatSeq 进程级请求序号。
var chatSeq atomic.Int64

// chatLogEnabled 聊天表格日志总开关。生产恒 true；
// 测试包经 TestMain 置 false 关闭 stdout 噪音，需要断言行输出的测试用 withChatLog 临时开启（R5）。
var chatLogEnabled = true

// chatStat 单个 chat 请求的日志统计；handler 挂 defer，请求出口后落一行。
type chatStat struct {
	start  time.Time
	model  string
	mode   string // "stream" | "sync"
	uid    string // 完整 uid，展示时只取前 8 位
	ttfb   time.Duration
	toks   int // <0 表示 usage 缺失 → 显示 "-"
	status int

	logged bool
}

// newChatStat 以请求进入 handler 的时刻为起点构造统计对象；toks 默认 -1（usage 缺失）。
func newChatStat(now time.Time, body []byte, stream bool) *chatStat {
	mode := "sync"
	if stream {
		mode = "stream"
	}
	return &chatStat{start: now, model: parseModelFromBody(body), mode: mode, toks: -1}
}

// done 幂等落一行表格日志。
func (s *chatStat) done() {
	if s.logged {
		return
	}
	s.logged = true
	logChatRow(s.ttfb, time.Since(s.start), s.model, s.mode, s.uid, s.status, s.toks)
}

// chatStatsReader 在流式透传时抓取 SSE 末帧的 usage.completion_tokens 精确值，
// 并记录首个 data 帧的 TTFB；原始字节原样返回给下游透传。
// 注意：不做 rune 估算，token 数一律采信上游 usage。
type chatStatsReader struct {
	br       *bufio.Reader
	start    time.Time
	ttfb     time.Duration
	seen     bool // 已见过首个 data 帧（TTFB 只记一次）
	hasUsage bool // 末帧是否带 usage
	tokens   int
	credit   float64 // 末帧 usage.credit（本次真实扣费，供成本账本）
	prompt   int     // 末帧 usage.prompt_tokens（与 completion 合计折算单价）
	pend     []byte // 已读未返回的行缓存

	// 思考循环观测（只统计，不影响转发）。
	// 背景：deepseek-v4 实测出现过 thinking 空转 —— 客户端记录 25 万个 thinking_delta、
	// 正文零输出，上游 usage 报 383008 输出 token，耗时 901s。
	// 网关是逐帧透传、不看内容，所以此前没有任何办法发现这种情况。
	thinkChars int                 // reasoning_content 累计字符
	textChars  int                 // content 累计字符
	chunkSeen  map[string]struct{} // thinking 切出的唯一块集合（去重）
	chunkTotal int                 // 已切出的块总数
	// lastNovel 最近一次出现新块时的 chunkTotal。当前总块数与之之差即「停滞块数」：
	// 循环文本跑完一遍周期后不再产生新块，该差值单调增长，是空转的本质信号。
	lastNovel int
	chunkBuf  strings.Builder

	// guard 思考死循环判据；nil = 不检测。loopTripped 保证只报一次。
	guard       *LoopGuard
	loopTripped bool
}

// loopChunkLen 思考循环检测的切块长度（字节）。
//
// 判据用「唯一块数 / 总块数」而不是「重复块出现几次」：循环文本的周期长度通常
// 不是 24 的整数倍，固定边界切出的块会逐轮漂移（周期 56 字节 + 块长 24 时，
// 相位每 7 块轮转一次）。所以单个块可能只出现几次，但**唯一块的总数**始终极小 ——
// 这个比值对边界漂移不敏感，正是可靠的判据。
const loopChunkLen = 24

// noteThinking 累计 reasoning 文本并切块计数。
func (s *chatStatsReader) noteThinking(t string) {
	s.thinkChars += len(t)
	if s.chunkSeen == nil {
		s.chunkSeen = map[string]struct{}{}
	}
	s.chunkBuf.WriteString(t)
	for s.chunkBuf.Len() >= loopChunkLen {
		full := s.chunkBuf.String()
		// 必须复制：full 别名于 chunkBuf 的内部缓冲，紧随其后的 WriteString 会就地
		// 改写它 —— 直接把 full[:loopChunkLen] 当 map 键，键的字节会随缓冲被覆盖而
		// 改变，唯一块统计（以及依赖它的停滞判据）随之失真。
		blk := strings.Clone(full[:loopChunkLen])
		s.chunkTotal++
		if _, seen := s.chunkSeen[blk]; !seen {
			s.chunkSeen[blk] = struct{}{}
			s.lastNovel = s.chunkTotal // 记账在自增之后：刚出现新块时停滞数为 0
		}
		s.chunkBuf.Reset()
		s.chunkBuf.WriteString(full[loopChunkLen:])
	}
}

// LoopSignal 返回思考循环的观测指标：thinking 字符数、正文（content）字符数、
// 唯一块数、总块数、停滞块数（自上次出现新块以来累积的块数）。
//
// 判读：总块数大而唯一块数极小 → 文本在重复（循环），正常行文的唯一块数应与
// 总块数同量级；停滞块数持续增长 → 已跑完一遍循环周期，正在原地打转。
func (s *chatStatsReader) LoopSignal() (thinkChars, textChars, distinctChunks, totalChunks, staleChunks int) {
	return s.thinkChars, s.textChars, len(s.chunkSeen), s.chunkTotal, s.chunkTotal - s.lastNovel
}

// SetLoopGuard 装配思考循环判据；nil 表示不检测（默认）。
// 由 handler 在转发前注入 —— 判据来自配置，stats reader 本身不读配置。
func (s *chatStatsReader) SetLoopGuard(g *LoopGuard) { s.guard = g }

// LoopGuardErr 在累计思考内容后判定是否命中死循环；命中返回非 nil。
// 由 Stream 的读循环逐帧调用：一旦命中即中断上游读取，不必等流自然结束
// （实测空转能跑 900s，等结束就失去了拦截意义）。
func (s *chatStatsReader) LoopGuardErr() error {
	if s.guard == nil || s.loopTripped {
		return nil
	}
	distinct, total := len(s.chunkSeen), s.chunkTotal
	// 停滞块数：自上次出现新块以来累积的块数。lastNovel 记录的是「发现新块那一刻的
	// 总块数」，而总块数只增不减，故该差值恒 >= 0。
	stale := total - s.lastNovel
	if s.guard.Detect(s.thinkChars, s.textChars, stale, distinct, total, time.Since(s.start)) {
		s.loopTripped = true
		return ErrThinkingLoop
	}
	return nil
}

// newChatStatsReaderSince 以 since 为 TTFB 计时起点（通常是请求进入 handler 的时刻）。
func newChatStatsReaderSince(r io.Reader, since time.Time) *chatStatsReader {
	return &chatStatsReader{br: bufio.NewReaderSize(r, 64*1024), start: since}
}

// TTFB 返回首个 data 帧到达耗时；无帧时为 0。
func (s *chatStatsReader) TTFB() time.Duration { return s.ttfb }

// Tokens 返回末帧 usage.completion_tokens 与是否缺失；无 usage 时 ok=false。
func (s *chatStatsReader) Tokens() (int, bool) { return s.tokens, s.hasUsage }

// Credit 返回末帧 usage.credit（本次真实扣费）；无 usage 时 ok=false。
func (s *chatStatsReader) Credit() (float64, bool) { return s.credit, s.hasUsage }

// TotalTokens 返回本次请求总 token 数（prompt + completion），供成本单价折算。
func (s *chatStatsReader) TotalTokens() int { return s.prompt + s.tokens }

// parseSSELine 解析一行 "data: {...}"：首帧记 TTFB，含 usage 时采信精确 completion_tokens。
func (s *chatStatsReader) parseSSELine(line string) {
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "data: ") {
		return
	}
	payload := strings.TrimPrefix(line, "data: ")
	if payload == "[DONE]" {
		return
	}
	if !s.seen {
		s.seen = true
		s.ttfb = time.Since(s.start)
	}
	var chunk struct {
		Usage *struct {
			CompletionTokens int     `json:"completion_tokens"`
			PromptTokens     int     `json:"prompt_tokens"`
			Credit           float64 `json:"credit"`
		} `json:"usage"`
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal([]byte(payload), &chunk) != nil {
		return
	}
	// 思考循环观测：累计 thinking 与正文长度。只统计，不改动 payload。
	for _, c := range chunk.Choices {
		if c.Delta.ReasoningContent != "" {
			s.noteThinking(c.Delta.ReasoningContent)
		}
		s.textChars += len(c.Delta.Content)
	}
	if chunk.Usage == nil {
		return
	}
	s.hasUsage = true
	s.tokens = chunk.Usage.CompletionTokens
	s.prompt = chunk.Usage.PromptTokens
	s.credit = chunk.Usage.Credit
}

// Read 返回原始数据，同时解析统计 TTFB/token。
func (s *chatStatsReader) Read(p []byte) (int, error) {
	if len(s.pend) > 0 {
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	line, err := s.br.ReadString('\n')
	if line != "" {
		s.parseSSELine(line)
		s.pend = []byte(line)
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	return 0, err
}

// parseModelFromBody 从请求 JSON 取 model 字段，缺省标 "-"。
func parseModelFromBody(body []byte) string {
	var obj struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &obj); err != nil || obj.Model == "" {
		return "-"
	}
	return obj.Model
}

// completionTokens 从 Aggregate 返回的响应中提取 usage.completion_tokens；缺失返回 -1。
func completionTokens(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}
	v, ok := u["completion_tokens"].(float64)
	if !ok {
		return -1
	}
	return int(v)
}

// usageCreditTotal 从聚合响应提取本次真实扣费与总 token 数（供成本账本）。
// ok=false 表示 usage 缺失或字段类型不符——此时不记录观测，避免污染账本。
func usageCreditTotal(resp map[string]any) (credit float64, total int, ok bool) {
	u, isMap := resp["usage"].(map[string]any)
	if !isMap {
		return 0, 0, false
	}
	c, hasCredit := u["credit"].(float64)
	pt, hasPrompt := u["prompt_tokens"].(float64)
	ct, hasCompletion := u["completion_tokens"].(float64)
	if !hasCredit || (!hasPrompt && !hasCompletion) {
		return 0, 0, false
	}
	return c, int(pt) + int(ct), true
}

// uidPrefix 只显示 uid 前 8 位；空 uid 显示 "-"。
func uidPrefix(uid string) string {
	if uid == "" {
		return "-"
	}
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

// logChatRow 打印一行请求级表格日志（直接输出 stdout，无 log 时间戳前缀）。
// toks<0 表示 usage 缺失，显示 "-"。
func logChatRow(ttfb, total time.Duration, model, mode, uid string, status int, toks int) {
	if !chatLogEnabled {
		return
	}
	seq := chatSeq.Add(1)
	if len(model) > 11 {
		model = model[:11]
	}
	tokField := "-"
	tokpsField := "-"
	if toks >= 0 {
		tokField = fmt.Sprintf("%d", toks)
		if total > 0 {
			tokpsField = fmt.Sprintf("%.1f", float64(toks)/total.Seconds())
		} else {
			tokpsField = "0.0"
		}
	}
	ttfbMS := "-"
	if ttfb > 0 {
		ttfbMS = fmt.Sprintf("%dms", ttfb.Milliseconds())
	}
	fmt.Fprintf(os.Stdout, "| #%03d | %s | %s | %s | %d | uid=%s | TTFB=%s | tok=%s | %stok/s | total=%.1fs |\n",
		seq,
		time.Now().Format("15:04:05"),
		model,
		mode,
		status,
		uidPrefix(uid),
		ttfbMS,
		tokField,
		tokpsField,
		total.Seconds(),
	)
}

// 思考死循环的处置已从「只记日志」升级为「切断 + 提示 + 换号重试」，
// 观测与拦截统一由 LoopGuard（loopguard.go）承担，命中时在 forwardChat 内打 WARN 行。
