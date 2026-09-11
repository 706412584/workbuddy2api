// anthropic.go Anthropic Messages 协议适配（Claude Code 等客户端）。
//
// 本网关上游只提供 OpenAI chat completions，故此处做 Anthropic → chat → Anthropic 的
// 双向转换，转换本身由 internal/apicompat 提供（移植自 sub2api）。
// 本文件只负责：解析请求、映射模型、复用 forwardChat 的选号/轮换/错误策略、渲染响应。
package server

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"time"
	"unicode"

	"workbuddy2api/internal/apicompat"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// writeAnthropicError 以 Anthropic 的错误格式回错。
// Claude Code 按 Anthropic 协议解析错误体，回 OpenAI 格式它只能显示为未知错误。
func writeAnthropicError(w http.ResponseWriter, status int, errType, msg string) {
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": msg,
		},
	})
}

// anthropicErrWriter 适配 forwardChat 的错误写出签名。
func anthropicErrWriter() func(http.ResponseWriter, int, string, string) {
	return func(w http.ResponseWriter, status int, code, msg string) {
		writeAnthropicError(w, status, anthropicErrType(status), msg)
	}
}

// anthropicErrType 把 HTTP 状态映射为 Anthropic 的错误类型。
func anthropicErrType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

// anthropicMessages POST /v1/messages
func (h *Handler) anthropicMessages(w http.ResponseWriter, r *http.Request, key *APIKeySpec) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	var areq apicompat.AnthropicRequest
	if err := json.Unmarshal(body, &areq); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}

	// 请求级统计：出口即打一行表格日志。
	st := newChatStat(time.Now(), body, areq.Stream)
	defer st.done()

	if areq.Model == "" {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		st.status = http.StatusBadRequest
		return
	}

	// 区域路由用的是**上游**模型名：真正决定哪个区域能服务的是它，
	// 而非客户端发来的 claude-* 名字。
	upstreamModel := mapModel(areq.Model, h.cfg.Protocol)
	allowedRegions, ok := h.resolveRegions(w, upstreamModel, key, anthropicErrWriter())
	if !ok {
		st.status = http.StatusNotFound
		return
	}

	creq, err := apicompat.AnthropicToChatCompletionsRequest(&areq)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "convert request: "+err.Error())
		st.status = http.StatusBadRequest
		return
	}
	creq.Model = upstreamModel
	chatBody, err := json.Marshal(creq)
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "marshal request: "+err.Error())
		st.status = http.StatusInternalServerError
		return
	}

	rc := h.forwardChat(w, chatBody, forwardOpt{
		pickPred:       regionAllowed(allowedRegions),
		sessKey:        h.sessionKey(body),
		key:            key,
		st:             st,
		allowedRegions: allowedRegions,
		model:          areq.Model,
		writeErr:       anthropicErrWriter(),
	})
	if rc == nil {
		return // 失败：forwardChat 已回错
	}
	defer rc.Close()

	if areq.Stream {
		h.streamAsAnthropic(w, rc, areq.Model, st)
		return
	}
	h.bufferAsAnthropic(w, rc, areq.Model, st)
}

// resolveRegions 计算本次请求允许的账号区域（模型区域 ∩ 密钥区域）。
// 无交集时按客户端协议回 404 并返回 false —— 该模型对调用方而言不存在。
func (h *Handler) resolveRegions(w http.ResponseWriter, model string, key *APIKeySpec,
	writeErr func(http.ResponseWriter, int, string, string)) ([]auth.Region, bool) {
	regions, ok := constrainRegions(regionsForModel(model), key)
	if !ok {
		writeErr(w, http.StatusNotFound, "model_not_found",
			"model "+model+" does not exist")
		return nil, false
	}
	return regions, true
}

// streamAsAnthropic 把上游 chat completions 的 SSE 流转换为 Anthropic 事件流写回客户端。
// clientModel 用于在响应里回显客户端原始模型名（而非上游模型名）。
func (h *Handler) streamAsAnthropic(w http.ResponseWriter, rc io.ReadCloser, clientModel string, st *chatStat) {
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("Connection", "keep-alive")
	hdr.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	// 与 OpenAI 透传路径一致：开始写流即视为 200（响应头一旦发出便无法改状态）。
	st.status = http.StatusOK

	state := apicompat.NewChatCompletionsToAnthropicStreamState(clientModel)
	readers := newChatChunkReader(bufio.NewReaderSize(rc, 64*1024))

	emit := func(evts []apicompat.AnthropicStreamEvent) bool {
		for _, e := range evts {
			sse, err := apicompat.ResponsesAnthropicEventToSSE(e)
			if err != nil {
				continue
			}
			if _, werr := io.WriteString(w, sse); werr != nil {
				return false // 客户端断连
			}
		}
		if fl != nil {
			fl.Flush()
		}
		return true
	}

	for {
		ch, err := readers.next()
		if err != nil {
			break // io.EOF 或读错误：交给 finalize 收尾
		}
		// 上游在末帧携带 usage；据此填日志的 token 数，避免流式请求恒记 tok=-。
		if ch.Usage != nil {
			st.toks = ch.Usage.CompletionTokens
		}
		if !emit(apicompat.ChatCompletionsChunkToAnthropicEvents(ch, state)) {
			return
		}
	}
	// 即便中途断流也要发收尾事件：否则客户端会一直等 message_stop，
	// 表现为"卡住"而不是明确的失败。
	emit(apicompat.FinalizeChatCompletionsAnthropicStream(state))
}

// bufferAsAnthropic 非流式：读完上游整个流后聚合，再转成 Anthropic 响应。
func (h *Handler) bufferAsAnthropic(w http.ResponseWriter, rc io.ReadCloser, clientModel string, st *chatStat) {
	resp, err := aggregateAsChatResponse(rc)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "aggregate upstream: "+err.Error())
		st.status = http.StatusBadGateway
		return
	}
	out := apicompat.ChatCompletionsResponseToAnthropic(resp, clientModel)
	writeJSON(w, http.StatusOK, out)
	st.status = http.StatusOK
	if resp.Usage != nil {
		st.toks = resp.Usage.CompletionTokens
	}
}

// countTokens POST /v1/messages/count_tokens
//
// 上游没有 token 计数接口，故本地估算。Claude Code 用它做上下文预算，
// 估值有偏差可接受，但绝不能报错——该端点失败会让客户端提前压缩上下文。
func (h *Handler) countTokens(w http.ResponseWriter, r *http.Request, _ *APIKeySpec) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	var areq apicompat.AnthropicRequest
	if err := json.Unmarshal(body, &areq); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}

	n := 0
	n += estimateTokens(string(areq.System))
	for _, m := range areq.Messages {
		n += estimateTokens(string(m.Content))
	}
	for _, t := range areq.Tools {
		n += estimateTokens(t.Name) + estimateTokens(t.Description) + len(t.InputSchema)/4
	}
	if n < 1 {
		n = 1 // 上游对空输入也会算 1 个 token，返回 0 会让客户端困惑
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": n})
}

// estimateTokens 估算文本的 token 数。
// CJK 约 1.5 字符/token（≈0.67 token/字符），ASCII 约 4 字符/token（0.25 token/字符）。
// 不追求准确——精确计数需要与上游一致的分词器，而本项目拿不到。
func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	var n float64
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			n += 0.67
		} else {
			n += 0.25
		}
	}
	if n < 1 {
		return 1
	}
	return int(n + 0.5)
}

// aggregateAsChatResponse 把上游 SSE 流聚合后重新解析为 apicompat 的 chat 响应类型。
// upstream.Aggregate 产出的是 OpenAI 形状的 map，字段与 apicompat 的类型一一对应，
// 故经一次 JSON 往返即可，无需重复实现聚合。
func aggregateAsChatResponse(rc io.Reader) (*apicompat.ChatCompletionsResponse, error) {
	agg, err := upstream.Aggregate(rc)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(agg)
	if err != nil {
		return nil, err
	}
	var out apicompat.ChatCompletionsResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
