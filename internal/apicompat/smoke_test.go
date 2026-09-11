package apicompat

import (
	"encoding/json"
	"strings"
	"testing"
)

// 本文件是移植后的冒烟测试：证明包内纯函数与流式状态机在 workbuddy2api 内可用。
// 只覆盖适配层实际会用到的主链路，不追求上游测试的覆盖度（上游测试未随包移植）。

// ── Anthropic → chat completions ────────────────────────────────

func TestSmokeAnthropicToChatRequest(t *testing.T) {
	raw := `{
		"model":"claude-sonnet-4-5-20250929",
		"max_tokens":1024,
		"system":"你是助手",
		"messages":[{"role":"user","content":"你好"}],
		"tools":[{"name":"get_weather","description":"查天气","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}]
	}`
	var req AnthropicRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := AnthropicToChatCompletionsRequest(&req)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(out.Messages) < 2 {
		t.Fatalf("messages=%d want ≥2（system + user）", len(out.Messages))
	}
	// 顶层 system 必须变成 messages 里的一条 role=system，且在最前。
	if out.Messages[0].Role != "system" {
		t.Errorf("首条 role=%q want system（Anthropic 顶层 system 需前置为消息）", out.Messages[0].Role)
	}
	if got := chatMsgText(out.Messages[0]); !strings.Contains(got, "你是助手") {
		t.Errorf("system 内容丢失: %q", got)
	}
	// 工具须转成嵌套 function 形态。
	if len(out.Tools) != 1 || out.Tools[0].Type != "function" || out.Tools[0].Function == nil {
		t.Fatalf("tools=%+v want 1 个嵌套 function", out.Tools)
	}
	if out.Tools[0].Function.Name != "get_weather" {
		t.Errorf("tool name=%q", out.Tools[0].Function.Name)
	}
}

// TestSmokeAnthropicToolResultPairing tool_use / tool_result 的配对是 Claude Code 的核心路径。
func TestSmokeAnthropicToolResultPairing(t *testing.T) {
	raw := `{
		"model":"claude-sonnet-4-5",
		"max_tokens":1024,
		"messages":[
			{"role":"user","content":"北京天气"},
			{"role":"assistant","content":[
				{"type":"text","text":"我来查"},
				{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"北京"}}
			]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"晴 25 度"}]}
		]
	}`
	var req AnthropicRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := AnthropicToChatCompletionsRequest(&req)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var sawAssistantToolCall, sawToolMsg bool
	for _, m := range out.Messages {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			sawAssistantToolCall = true
			if m.ToolCalls[0].ID != "toolu_1" || m.ToolCalls[0].Function.Name != "get_weather" {
				t.Errorf("tool_call=%+v", m.ToolCalls[0])
			}
		}
		if m.Role == "tool" {
			sawToolMsg = true
			if m.ToolCallID != "toolu_1" {
				t.Errorf("tool message ToolCallID=%q want toolu_1", m.ToolCallID)
			}
		}
	}
	if !sawAssistantToolCall {
		t.Error("tool_use 未转成 assistant.tool_calls")
	}
	if !sawToolMsg {
		t.Error("tool_result 未转成 role=tool 消息")
	}
}

// ── Anthropic 流式状态机 ────────────────────────────────────────

func TestSmokeAnthropicStreamEventSequence(t *testing.T) {
	state := NewChatCompletionsToAnthropicStreamState("claude-sonnet-4-5")

	var types []string
	collect := func(evts []AnthropicStreamEvent) {
		for _, e := range evts {
			types = append(types, e.Type)
			// 每个事件都必须能序列化成 SSE（适配层逐条写回客户端）。
			if sse, err := ResponsesAnthropicEventToSSE(e); err != nil || sse == "" {
				t.Errorf("SSE 序列化失败 type=%s err=%v", e.Type, err)
			}
		}
	}

	content := "你好"
	finish := "stop"
	collect(ChatCompletionsChunkToAnthropicEvents(&ChatCompletionsChunk{
		ID: "c1", Model: "glm-5.2",
		Choices: []ChatChunkChoice{{Index: 0, Delta: ChatDelta{Role: "assistant", Content: &content}}},
	}, state))
	collect(ChatCompletionsChunkToAnthropicEvents(&ChatCompletionsChunk{
		ID: "c1", Model: "glm-5.2",
		Choices: []ChatChunkChoice{{Index: 0, Delta: ChatDelta{}, FinishReason: &finish}},
		Usage:   &ChatUsage{CompletionTokens: 2},
	}, state))
	collect(FinalizeChatCompletionsAnthropicStream(state))

	// Claude Code 要求的最小事件链。
	for _, want := range []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"} {
		if !containsStr(types, want) {
			t.Errorf("缺少事件 %q\n实际序列: %v", want, types)
		}
	}
	// message_start 必须最先发出。
	if len(types) == 0 || types[0] != "message_start" {
		t.Errorf("首个事件=%v want 以 message_start 开头", types)
	}
	// message_stop 必须最后。
	if types[len(types)-1] != "message_stop" {
		t.Errorf("末个事件=%q want message_stop", types[len(types)-1])
	}
}

// ── Responses → chat completions ────────────────────────────────

func TestSmokeResponsesToChatRequest(t *testing.T) {
	raw := `{
		"model":"gpt-5-codex",
		"instructions":"你是编码助手",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"写个函数"}]}],
		"max_output_tokens":2048
	}`
	var req ResponsesRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := ResponsesToChatCompletionsRequest(&req)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(out.Messages) < 2 {
		t.Fatalf("messages=%d want ≥2（instructions + user）", len(out.Messages))
	}
	if out.Messages[0].Role != "system" {
		t.Errorf("首条 role=%q want system（instructions 需前置）", out.Messages[0].Role)
	}
	if got := chatMsgText(out.Messages[0]); !strings.Contains(got, "你是编码助手") {
		t.Errorf("instructions 内容丢失: %q", got)
	}
	// input 文本要落到 user 消息上。
	var userText string
	for _, m := range out.Messages {
		if m.Role == "user" {
			userText += chatMsgText(m)
		}
	}
	if !strings.Contains(userText, "写个函数") {
		t.Errorf("input 文本丢失: %q", userText)
	}
}

// TestSmokeResponsesStreamItemBeforeDelta 是最关键的 Codex 兼容点：
// delta 之前必须先发 output_item.added，否则 Codex 会丢弃该 delta。
func TestSmokeResponsesStreamItemBeforeDelta(t *testing.T) {
	state := NewChatCompletionsToResponsesStreamState("gpt-5-codex")

	var order []string
	collect := func(evts []ResponsesStreamEvent) {
		for _, e := range evts {
			order = append(order, e.Type)
			if sse, err := ResponsesEventToSSE(e); err != nil || sse == "" {
				t.Errorf("SSE 序列化失败 type=%s err=%v", e.Type, err)
			}
		}
	}

	content := "hi"
	finish := "stop"
	collect(ChatCompletionsChunkToResponsesEvents(&ChatCompletionsChunk{
		ID: "c1", Model: "glm-5.2",
		Choices: []ChatChunkChoice{{Index: 0, Delta: ChatDelta{Role: "assistant", Content: &content}}},
	}, state))
	collect(ChatCompletionsChunkToResponsesEvents(&ChatCompletionsChunk{
		ID: "c1", Model: "glm-5.2",
		Choices: []ChatChunkChoice{{Index: 0, Delta: ChatDelta{}, FinishReason: &finish}},
	}, state))
	collect(FinalizeChatCompletionsResponsesStream(state))

	idxAdded := indexOfStr(order, "response.output_item.added")
	idxDelta := indexOfStr(order, "response.output_text.delta")
	if idxAdded < 0 || idxDelta < 0 {
		t.Fatalf("缺少关键事件（added=%d delta=%d）: %v", idxAdded, idxDelta, order)
	}
	if idxAdded > idxDelta {
		t.Errorf("output_item.added 必须早于 output_text.delta（Codex 严格校验）\n实际: %v", order)
	}
	if !containsStr(order, "response.created") {
		t.Errorf("缺少 response.created: %v", order)
	}
	if !containsStr(order, "response.completed") {
		t.Errorf("缺少 response.completed: %v", order)
	}
}

// TestSmokeResponsesSequenceNumberMonotonic Codex 要求 sequence_number 单调递增。
func TestSmokeResponsesSequenceNumberMonotonic(t *testing.T) {
	state := NewChatCompletionsToResponsesStreamState("gpt-5-codex")
	content := "x"
	finish := "stop"

	var seqs []int
	collect := func(evts []ResponsesStreamEvent) {
		for _, e := range evts {
			seqs = append(seqs, e.SequenceNumber)
		}
	}
	collect(ChatCompletionsChunkToResponsesEvents(&ChatCompletionsChunk{
		ID: "c1", Choices: []ChatChunkChoice{{Index: 0, Delta: ChatDelta{Content: &content}}},
	}, state))
	collect(ChatCompletionsChunkToResponsesEvents(&ChatCompletionsChunk{
		ID: "c1", Choices: []ChatChunkChoice{{Index: 0, Delta: ChatDelta{}, FinishReason: &finish}},
	}, state))
	collect(FinalizeChatCompletionsResponsesStream(state))

	if len(seqs) < 3 {
		t.Fatalf("事件过少: %v", seqs)
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("sequence_number 非单调递增: %v", seqs)
		}
	}
}

// ── 工具分类 helper（非流式 Responses 转换需要）────────────────

func TestSmokeToolClassifiers(t *testing.T) {
	raw := `{
		"model":"gpt-5-codex",
		"input":"hi",
		"tools":[
			{"type":"function","name":"shell","parameters":{"type":"object"}},
			{"type":"custom","name":"apply_patch"}
		]
	}`
	var req ResponsesRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tools, err := EffectiveResponsesTools(&req)
	if err != nil {
		t.Fatalf("EffectiveResponsesTools: %v", err)
	}
	fn := FunctionToolNames(tools)
	ct := CustomToolNames(tools)
	if !fn["shell"] {
		t.Errorf("function tools=%v want 含 shell", fn)
	}
	if !ct["apply_patch"] {
		t.Errorf("custom tools=%v want 含 apply_patch", ct)
	}
}

// ── 小工具 ──────────────────────────────────────────────────────

// chatMsgText 取出 chat 消息的文本内容（Content 可能是字符串或 parts 数组）。
func chatMsgText(m ChatMessage) string {
	if len(m.Content) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	return string(m.Content)
}

func containsStr(xs []string, want string) bool { return indexOfStr(xs, want) >= 0 }

func indexOfStr(xs []string, want string) int {
	for i, x := range xs {
		if x == want {
			return i
		}
	}
	return -1
}
