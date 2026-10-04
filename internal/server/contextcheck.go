// contextcheck.go 上下文预算的**本地估算**：用于把上游那个含糊的 400 11133
// 区分为「上下文超限」还是「参数错误」。
//
// 背景（2026-10-03 实测）：
//
//	上游在「输入超出模型上下文窗口」时**不返回 11115（context_length_exceeded）**，
//	而是返回 400 + code=11133 + extError.code="model_param_invalid" —— 与「tool
//	schema 畸形」「图片 URL 非法」等真正的参数错误**逐字节同形**。实测同一个 1M
//	上下文模型：输入 ~975K token → 200，~1.47M token → 400 11133。
//
//	若一律按参数错误回泛化文案（「请求参数不符合当前模型要求，请调整后重试。」），
//	客户端读不出「该压缩上下文了」这个信号，自动压缩永不触发 —— 与 issue「503 被
//	读成 overloaded_error 导致 15 分钟假死」是同一类失效，只是换了层皮。
//
// 做法：**不预先拒绝**（本地估算有误差，提前拒绝可能误伤上游本可接受的请求），
// 而是在上游已返回 11133 之后，用本地估算判断「这个请求是不是本就超了窗口」：
//
//   - 估算已越界 → 这是上下文超限，回 Anthropic 官方同款 "prompt is too long:
//     N tokens > M maximum"（客户端按此触发压缩）；
//   - 估算远未越界 → 是真正的参数错误，回泛化参数文案。
//
// 只在「上游已拒绝」这个前提下做判断，故估算误差只会影响**文案**，不会改变
// 「请求是否被拒绝」—— 代价被压到最小。
package server

import (
	"encoding/json"
	"strconv"

	"workbuddy2api/internal/auth"
)

// contextCheckProbe 从上游 chat completions 请求体里挑出估算需要的字段。
// messages/tools 保留原始 JSON（json.RawMessage）—— 估算只需它们的文本，不必
// 反序列化成具体类型，省一次全量解析。
type contextCheckProbe struct {
	Model    string          `json:"model"`
	Messages json.RawMessage `json:"messages"`
	Tools    json.RawMessage `json:"tools"`
}

// estimateUpstreamInputTokens 估算上游请求体的输入 token 数，并返回其中的上游
// 模型名（body.model）。
//
// 模型名取自 **body** 而非 forwardOpt.model：Anthropic / Responses 入口的
// o.model 是客户端模型名（claude-* / gpt-*），只有 body.model 才是映射后的上游
// 模型名（deepseek-*），而上下文窗口是按上游模型查的。
//
// 只对 messages + tools 的文本做估算，忽略 JSON 结构开销（保守偏低）。与
// /v1/messages/count_tokens 的估算口径一致（同用 estimateTokens），避免两处
// 对同一请求给出互相矛盾的 token 数。解析失败返回 (0, "")（= 无法判断，调用方跳过）。
func estimateUpstreamInputTokens(body []byte) (int, string) {
	var p contextCheckProbe
	if err := json.Unmarshal(body, &p); err != nil {
		return 0, ""
	}
	n := 0
	if len(p.Messages) > 0 {
		n += estimateTokens(string(p.Messages))
	}
	if len(p.Tools) > 0 {
		n += estimateTokens(string(p.Tools))
	}
	return n, p.Model
}

// contextWindowForModel 返回模型在给定区域集合下的上下文窗口（token）。
//
// 取「请求可路由到的区域」里该模型窗口的**最小值**：请求可能落到任一允许区域的
// 账号，用最小值才能保证「按此判不超限 → 实际任一账号都不超限」。动态模型缓存优先
// （上游实时值），回落静态表；都查不到返回 0（= 未知，调用方跳过）。
func (h *Handler) contextWindowForModel(model string, regions []auth.Region) int64 {
	var best int64
	set := func(w int64) {
		if w <= 0 {
			return
		}
		if best == 0 || w < best {
			best = w
		}
	}
	// 动态缓存：上游实时下发的 maxInputTokens（权威值）。
	dynamicModelsCache.mu.RLock()
	for _, slot := range []*modelsSlot{&dynamicModelsCache.cn, &dynamicModelsCache.global} {
		for _, mi := range slot.ids {
			if mi.ID == model {
				set(mi.ContextWindow)
			}
		}
	}
	dynamicModelsCache.mu.RUnlock()

	// 静态表：动态缺失时的权威回落（global 动态接口恒 500，必走此路）。
	regionSet := map[auth.Region]bool{}
	for _, r := range regions {
		regionSet[r] = true
	}
	lookupStatic := func(tbl []map[string]any) {
		for _, m := range tbl {
			if id, _ := m["id"].(string); id != model {
				continue
			}
			switch w := m["context_length"].(type) {
			case int:
				set(int64(w))
			case int64:
				set(w)
			case float64:
				set(int64(w))
			}
		}
	}
	// regions 为空 = 不限区域（regionsForModel 对未收录模型的语义）→ 两区都查。
	if len(regionSet) == 0 || regionSet[auth.RegionCN] {
		lookupStatic(staticModelsCN)
	}
	if len(regionSet) == 0 || regionSet[auth.RegionGlobal] {
		lookupStatic(staticModelsGlobal)
	}
	return best
}

// contextOverflowLikely 判断「上游以 11133 拒绝的这个请求，是否本就超出了上下文窗口」。
//
// 判据：本地估算输入 > 窗口 * (1 - margin)。留 margin 是因为 estimateTokens 对代码/
// JSON 这类符号密集文本会**低估**（真实 BPE 在符号串上切得更碎）：低估时 est 偏小，
// 少判成超限 → 退回泛化参数文案（不误伤，只是信号弱一点）；估准或略高估时按超限处理。
// 取 5% 余量覆盖实测误差量级。
//
// 返回 (isOverflow, estTokens, window)。窗口未知或无法估算时 isOverflow=false。
func (h *Handler) contextOverflowLikely(body []byte, regions []auth.Region) (bool, int, int64) {
	est, model := estimateUpstreamInputTokens(body)
	if est == 0 || model == "" {
		return false, 0, 0
	}
	window := h.contextWindowForModel(model, regions)
	if window <= 0 {
		return false, est, 0
	}
	const margin = 0.05
	limit := int64(float64(window) * (1 - margin))
	return int64(est) > limit, est, window
}

// contextLengthExceededMessage 组装上下文超限文案。
//
// 刻意对齐 Anthropic 官方格式（"prompt is too long: N tokens > M maximum"）：
// Claude Code 等客户端正是按这句话识别「该压缩上下文了」。N 用本地估算值（带 "~"
// 标明是估算，不冒充上游精确计数），M 是模型真实窗口。
func contextLengthExceededMessage(est int, window int64) string {
	return "prompt is too long: ~" + strconv.Itoa(est) + " tokens > " + strconv.FormatInt(window, 10) + " maximum"
}
