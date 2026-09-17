// loopguard.go 思考死循环的检测与处置。
//
// 背景（实测 2026-09-15/16，19 次，全部是 deepseek-v4.1-flash）：
// 模型陷入思考空转 —— 连续吐 reasoning_content 但正文零输出，内容高度重复。
// 单次请求实测 900s（15 分钟），最坏 2637s（44 分钟），思考字符 130 万–147 万，
// 唯一块占比 0.005–0.034（正常行文接近 1.0）。代价：真实消耗额度、产出为零，
// 且占住一个在途名额（max_in_flight=3）十几分钟，3 个并发就能把池子堵死。
//
// 为什么此前没有任何机制能拦住它：唯一的兜底是 idle_timeout（空闲超时），
// 而空转并非空闲 —— 它在持续吐 token，空闲监控看它"很活跃"，永不触发。
//
// 处置策略（分档）：
//  1. 命中判据 → 切断上游流，在请求体追加一条 system 提示（要求停止重复、直接给结论），
//     换号重新发起。三种协议（chat completions / Anthropic / Responses）共用同一处置，
//     因为「读了几帧才发现」这个时机只存在于转发过程中，故转发被移进了轮换循环。
//  2. 重试次数用尽仍命中 → 回明确的 thinking_loop 错误，不再让客户端干等。
//     注意出口按 lastErr 分流：账号池小时轮换预算会先耗尽，那时也必须报
//     thinking_loop 而非「无可用账号」—— 否则会把排查方向引向账号池。
//
// 已知限制：流式请求的思考内容已逐帧实时转发给客户端（normalizeFrame 白名单含
// reasoning_content），故重试会让客户端收到两段思考拼接。这是刻意的取舍 ——
// 拼接的观感问题远小于挂 15 分钟，且客户端能拿到最终正文。
package server

import (
	"encoding/json"
	"errors"
	"time"
)

// ErrThinkingLoop 思考死循环命中信号。由 chatStatsReader.LoopGuardErr 判定，
// 经两条读循环冒泡回 forwardChat 的重试判定：
//   - upstream.Stream（OpenAI 透传路径，经 LoopAborter 接口回调）；
//   - chatChunkReader.next（Anthropic / Responses 协议适配路径）。
//
// 两条路径都要接：前者服务 /v1/chat/completions，后两者服务 /v1/messages 与
// /v1/responses —— 只接一条的话，另外两个协议的守卫会静默失效。
var ErrThinkingLoop = errors.New("thinking loop detected")

// loopNudgeText 追加给模型的提示。措辞直接，针对「反复检查同一件事」的典型形态。
const loopNudgeText = "你刚才陷入了重复思考：反复检查同一件事却没有给出结论。" +
	"请立即停止重复，直接基于已有信息给出最终答案。不要再次检查，不要再罗列步骤。"

// LoopGuard 思考循环判据。
//
// 三个条件同时满足才算命中（缺一不可）：
//   - 思考字符数超过阈值：区分「长思考」与「空转」。正常请求 thinking 通常几千字符。
//   - 正文为 0：核心判据 —— 想了半天不产出，这正是要救的场景。
//   - 唯一块占比低：确认是重复文本，而非大篇幅的正常推理。
//
// 阈值取实测异常的保守下界：异常样本最小 20.8 万字符，正常请求几千，
// 默认 40 万落在两者之间，留足余量避免误杀真正需要长思考的请求。
type LoopGuard struct {
	// Enabled 关闭时完全不检测（保留原有行为）。
	Enabled bool
	// MinThinkChars 思考字符数阈值。
	MinThinkChars int
	// MaxDistinctRatio 唯一块占比上限；占比低于此值判为重复文本。
	MaxDistinctRatio float64
	// MinElapsed 最早可判定的耗时：给正常长思考留空间，避免刚开跑就误判。
	MinElapsed time.Duration
	// MaxRetries 命中后的重试上限（不含首次尝试）；<=0 表示用 maxLoopRetries。
	// 放这里而非 handler 的常量：测试需要构造「命中即用尽」的判据来验证终态错误。
	MaxRetries int
}

// DefaultLoopGuard 默认判据（用户选定 40 万 / 0.15）。
func DefaultLoopGuard() LoopGuard {
	return LoopGuard{
		Enabled:          true,
		MinThinkChars:    400_000,
		MaxDistinctRatio: 0.15,
		MinElapsed:       30 * time.Second,
		MaxRetries:       maxLoopRetries,
	}
}

// maxRetries 返回生效的重试上限（<=0 回落到默认）。
func (g LoopGuard) maxRetries() int {
	if g.MaxRetries <= 0 {
		return maxLoopRetries
	}
	return g.MaxRetries
}

// Detect 判断给定的流式统计是否命中思考死循环。
func (g LoopGuard) Detect(thinkChars, textChars, distinctChunks, totalChunks int, elapsed time.Duration) bool {
	if !g.Enabled {
		return false
	}
	if thinkChars < g.MinThinkChars {
		return false
	}
	// 正文有输出说明模型在正常工作（哪怕同时在想），不干预。
	if textChars > 0 {
		return false
	}
	if totalChunks == 0 {
		return false
	}
	if elapsed < g.MinElapsed {
		return false
	}
	ratio := float64(distinctChunks) / float64(totalChunks)
	return ratio < g.MaxDistinctRatio
}

// InjectLoopNudge 在请求体末尾追加一条 system 提示，要求模型停止重复思考。
//
// 为什么追加 system 而不是改写已有消息：不改动用户上下文，重试的请求与原始请求
// 除末尾提示外逐字相同，便于对照排查。上游约束是「首条必须是 system」（否则
// 400 code=11-128），追加在末尾不影响该约束。
//
// body 不可解析时原样返回（坏 body 不二次错误化，与 prepareBody 同口径）。
func InjectLoopNudge(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, _ := obj["messages"].([]any)
	msgs = append(msgs, map[string]any{
		"role":    "system",
		"content": loopNudgeText,
	})
	obj["messages"] = msgs
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}
