// responses.go OpenAI Responses API 协议适配（Codex CLI 等客户端）。
//
// 与 anthropic.go 同构：上游只提供 chat completions，故做 Responses → chat → Responses
// 的双向转换，转换本身由 internal/apicompat 提供（移植自 sub2api）。
// 本文件只负责：解析请求、映射模型、复用 forwardChat、渲染响应。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"workbuddy2api/internal/apicompat"
)

// responses POST /v1/responses
//
// 错误体沿用 OpenAI 格式：Responses API 的 error 结构与 chat completions 相同
// （{"error":{"message","type","code"}}），Codex 按此解析，无需另写一套。
func (h *Handler) responses(w http.ResponseWriter, r *http.Request, key *APIKeySpec) {
	body, err := readBody(r)
	if err != nil {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "invalid_request", "read body: "+err.Error())
		return
	}
	var rreq apicompat.ResponsesRequest
	if err := json.Unmarshal(body, &rreq); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid JSON: "+err.Error())
		return
	}

	st := newChatStat(time.Now(), body, rreq.Stream)
	defer st.done()

	if rreq.Model == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "model is required")
		st.status = http.StatusBadRequest
		return
	}

	upstreamModel := mapModel(rreq.Model, h.cfg.Protocol)
	allowedRegions, ok := h.resolveRegions(w, upstreamModel, key, writeOpenAIError)
	if !ok {
		st.status = http.StatusNotFound
		return
	}

	creq, err := apicompat.ResponsesToChatCompletionsRequest(&rreq)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "convert request: "+err.Error())
		st.status = http.StatusBadRequest
		return
	}
	creq.Model = upstreamModel
	chatBody, err := json.Marshal(creq)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "api_error", "marshal request: "+err.Error())
		st.status = http.StatusInternalServerError
		return
	}

	rc := h.forwardChat(w, chatBody, forwardOpt{
		pickPred:       regionAllowed(allowedRegions),
		sessKey:        h.sessionKey(body),
		key:            key,
		st:             st,
		allowedRegions: allowedRegions,
		model:          rreq.Model,
	})
	if rc == nil {
		return // 失败：forwardChat 已回错（OpenAI 格式）
	}
	defer rc.Close()

	if rreq.Stream {
		h.streamAsResponses(w, rc, rreq.Model, st)
		return
	}
	h.bufferAsResponses(w, rc, rreq.Model, &rreq, st)
}

// streamAsResponses 把上游 chat completions 的 SSE 流转换为 Responses 事件流写回客户端。
//
// 事件顺序对 Codex 是硬要求：delta 必须在其 output_item.added 之后到达，
// 且 sequence_number 需单调递增——这些由 apicompat 的状态机保证，此处只负责转发。
func (h *Handler) streamAsResponses(w http.ResponseWriter, rc io.ReadCloser, clientModel string, st *chatStat) {
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("Connection", "keep-alive")
	hdr.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	// 与 OpenAI 透传路径一致：开始写流即视为 200（响应头一旦发出便无法改状态）。
	st.status = http.StatusOK

	state := apicompat.NewChatCompletionsToResponsesStreamState(clientModel)
	readers := newChatChunkReader(rc)

	emit := func(evts []apicompat.ResponsesStreamEvent) bool {
		for _, e := range evts {
			sse, err := apicompat.ResponsesEventToSSE(e)
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
		if !emit(apicompat.ChatCompletionsChunkToResponsesEvents(ch, state)) {
			return
		}
	}
	// 即便中途断流也要发 response.completed：否则 Codex 认为本轮未结束，
	// 会把未完成的工具调用持久化进历史，毒化后续请求。
	emit(apicompat.FinalizeChatCompletionsResponsesStream(state))
}

// bufferAsResponses 非流式：聚合上游后转成 Responses 响应。
// 需要先按请求里的 tools 声明分类出 custom / function / namespace 工具，
// 转换器据此还原工具调用的条目类型（Codex 按类型路由，判错会认不出工具）。
func (h *Handler) bufferAsResponses(w http.ResponseWriter, rc io.ReadCloser,
	clientModel string, rreq *apicompat.ResponsesRequest, st *chatStat) {
	resp, err := aggregateAsChatResponse(rc)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", "aggregate upstream: "+err.Error())
		st.status = http.StatusBadGateway
		return
	}
	tools, _ := apicompat.EffectiveResponsesTools(rreq)
	out := apicompat.ChatCompletionsResponseToResponses(resp, clientModel,
		apicompat.CustomToolNames(tools),
		apicompat.FunctionToolNames(tools),
		apicompat.HasToolSearchTool(tools),
		apicompat.NamespaceToolNames(tools),
	)
	writeJSON(w, http.StatusOK, out)
	st.status = http.StatusOK
	if resp.Usage != nil {
		st.toks = resp.Usage.CompletionTokens
	}
}
