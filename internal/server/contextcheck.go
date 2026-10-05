// contextcheck.go 上下文预算的**本地估算**：用于把上游那个含糊的 400 11133
// 区分为「上下文超限」还是「参数错误」。
//
// 背景（2026-10-06 直连两区上游实测，逐字节对比）：
//
//	**两区行为不同**，这是问题的根源：
//
//	- CN（copilot.tencent.com）：输入超窗口时回 400 + code=**11115**，body 里直接
//	  带精确计数与官方话术 —— "prompt is too long: 1252240 tokens > 1048576
//	  maximum"，extError.code=invalid_request_error。**无需本地估算**，网关的
//	  ErrContextExceeded 分支已原样透传该文案（见 upstream.ContextExceededMessage）。
//
//	- global（www.workbuddy.ai）：输入超窗口时**不返回 11115**，而是回 400 +
//	  code=**11133** + extError.code="model_param_invalid" —— 与「tool schema 畸形」
//	  「tool 名非法」等真正的参数错误**逐字节同形**（实测：超限与坏 tool 名两个请求
//	  的 body 完全一致，均无任何 token 计数或提示）。于是 global 侧唯一可用的判据
//	  就是**本地估算**。
//
//	若一律按参数错误回泛化文案（「请求参数不符合当前模型要求，请调整后重试。」），
//	客户端读不出「该压缩上下文了」这个信号，自动压缩永不触发 —— 与 issue「503 被
//	读成 overloaded_error 导致 15 分钟假死」是同一类失效，只是换了层皮。
//
// 做法：**不预先拒绝**（本地估算有误差，提前拒绝可能误伤上游本可接受的请求），
// 而是在上游已返回 11133 之后，用本地估算判断「这个请求是不是本就超了窗口」：
//
//   - 估算越界 → 按上下文超限回 Anthropic 同款 "prompt is too long …"（触发压缩）；
//   - 估算远未越界 → 真正的参数错误，回泛化参数文案。
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
// 判据：本地估算输入 > 窗口 * underEstimateSlack。
//
// 为什么 slack 远小于 1（而不是「估到接近窗口才算」）：
//
//	estimateTokens 用固定的 0.25 token/字符估 ASCII，而真实 BPE 对**代码/JSON 这类
//	符号密集文本**切得更碎。2026-10-06 用真实上游 usage.prompt_tokens 标定（经网关
//	发同样内容，读回上游计数）：
//
//	    Go 源码   真实/估算 = 1.30
//	    Markdown  真实/估算 = 1.42
//	    英文散文  真实/估算 = 0.83
//	    中文      真实/估算 = 1.05
//
//	即结构化内容**低估最多 ~1.4 倍**。若阈值取 0.95×窗口，则真实 token 要到
//	~1.35×窗口 才判超限 —— 而真实超限发生在 1.0×窗口，于是 est 落在
//	[0.65×窗口, 0.95×窗口) 的真实超限请求全被漏判成「参数错误」（正是线上现象）。
//
//	取 underEstimateSlack = 0.65 ≈ 1/1.54：给实测最坏低估（1.42）留了余量，
//	真实超限（real = 窗口）在**最坏低估**下 est 仍有 ~0.70×窗口，稳在阈值之上。
//	覆盖代码 / Markdown / minified JSON（实测比率 1.23~1.42），**盖不住**极端高密度
//	内容 —— base64 实测 2.87、hex 2.34（随机字母数字几乎不可压缩，1 字符≈1 token）。
//	这类内容真实超限时 est 仅 ~0.35×窗口，仍会被漏判成参数错误。**不做更低阈值**
//	是刻意的：阈值越低，越容易把「中等体积的真实参数错误」误判成超限，而若该参数
//	错误持续存在，客户端会反复压缩却始终看不到真正的报错原因（比漏判更难排查）。
//	两害相权取保守值。
//
// 宁可略过报，不可漏报：走到本函数时上游**已经**以 11133 拒绝了本次请求，
// 不存在「把成功请求误判成失败」的风险 —— 唯一的差别是**文案**。而两种错的代价不对称：
//
//   - 漏报（超限却说参数错误）→ 客户端读不出压缩信号，原样重发 → 永远失败（本次 bug）；
//   - 过报（参数错误却说超限）→ 客户端压缩后重发；若真是参数问题，压缩后 est 变小、
//     再次 11133 时即落回泛化文案，客户端最终仍能看到真实原因（自纠正，代价有界）。
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
	// 见上：0.65 ≈ 1 / 1.54，给实测最坏低估（1.42）留余量，保证真实超限不漏判。
	const underEstimateSlack = 0.65
	limit := int64(float64(window) * underEstimateSlack)
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
