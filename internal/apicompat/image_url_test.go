package apicompat

import (
	"encoding/json"
	"strings"
	"testing"
)

// 本文件锁定「畸形图片 URL」的处理（2026-09-29 生产事故）。
//
// 事故链：MCP 工具 mcp__taptap-maker__generate_test_qrcode 返回的 image 块里，
// URL 尾部被拼进一段带引号的说明文字：
//
//	{"type":"image","source":{"type":"url","url":"https://…/m_qwe3.png \"扫描此二维码测试游戏\""}}
//
// 该串在 JSON 里完全合法，但原样转发给上游会被判为非法参数（400 code=11133）。
// 网关两条路径必须都「安全降级」而不是把非法参数透传上去：
//   - chat 路径（cc-haha 走这条）：降级成一条文字说明；
//   - responses 路径：同样降级成文字说明。

const malformedImgURL = `https://tapcode-sce.spark.xd.com/qrcode/m_qwe3_1790588966355.png "扫描此二维码测试游戏"`
const cleanImgURL = `https://tapcode-sce.spark.xd.com/qrcode/m_qwe3_1790588966355.png`

// TestIsForwardableImageURL 合法性判据：http(s) + 有 host + 无空白/控制字符。
func TestIsForwardableImageURL(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{cleanImgURL, true},
		{"http://example.com/a.png", true},
		{"https://example.com/a.png?x=1&y=2", true},
		// 事故形态：URL 后带空格 + 中文说明。
		{malformedImgURL, false},
		{"https://a/b.png x", false},
		{"https://a/b.png\n", false},
		{"https://a/b.png\t", false},
		{"", false},
		// 非 http(s) 不转发（data: 走 base64 分支，其余协议上游不认）。
		{"data:image/png;base64,AAAA", false},
		{"ftp://example.com/a.png", false},
		{"not-a-url", false},
		{"https:///nohost.png", false},
	}
	for _, c := range cases {
		if got := isForwardableImageURL(c.url); got != c.want {
			t.Errorf("isForwardableImageURL(%q)=%v want %v", c.url, got, c.want)
		}
	}
}

// TestAnthropicImageToDataURI_URL 合法 URL 原样转发；畸形 URL 返回空（由调用方降级）。
func TestAnthropicImageToDataURI_URL(t *testing.T) {
	if got := anthropicImageToDataURI(&AnthropicImageSource{Type: "url", URL: cleanImgURL}); got != cleanImgURL {
		t.Errorf("合法 URL 应原样返回, got %q", got)
	}
	if got := anthropicImageToDataURI(&AnthropicImageSource{Type: "url", URL: malformedImgURL}); got != "" {
		t.Errorf("畸形 URL 应返回空（不得透传非法参数）, got %q", got)
	}
	// base64 分支不受影响。
	if got := anthropicImageToDataURI(&AnthropicImageSource{Type: "base64", MediaType: "image/png", Data: "AAAA"}); got != "data:image/png;base64,AAAA" {
		t.Errorf("base64 分支被破坏: %q", got)
	}
	// base64 且 MediaType 缺失 → 默认 image/png（原有行为）。
	if got := anthropicImageToDataURI(&AnthropicImageSource{Type: "base64", Data: "AAAA"}); got != "data:image/png;base64,AAAA" {
		t.Errorf("MediaType 缺省应回落 image/png: %q", got)
	}
	if got := anthropicImageToDataURI(nil); got != "" {
		t.Errorf("nil 源应为空: %q", got)
	}
}

// TestChatBridgeDropsMalformedURLImage chat 路径（cc-haha 实际走的路径）：
// 畸形 URL 不得出现在序列化结果里（否则上游 11133），且必须留下降级说明。
func TestChatBridgeDropsMalformedURLImage(t *testing.T) {
	raw := json.RawMessage(`[{"type":"image","source":{"type":"url","url":` +
		jsonString(malformedImgURL) + `}},{"type":"text","text":"看这张图"}]`)
	msgs, err := anthropicUserToChatMessages(raw)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	serialized := mustJSON(t, msgs)
	if strings.Contains(serialized, "tapcode-sce") {
		t.Errorf("畸形 URL 被透传给上游（会触发 400 code=11133）: %s", serialized)
	}
	if strings.Contains(serialized, "扫描此二维码") {
		t.Errorf("畸形 URL 的说明文字不应作为 URL 透传: %s", serialized)
	}
	if !strings.Contains(serialized, "image omitted") {
		t.Errorf("应留下降级说明，不能让图片无声消失: %s", serialized)
	}
	if !strings.Contains(serialized, "看这张图") {
		t.Errorf("同级 text 块丢失: %s", serialized)
	}
}

// TestChatBridgeForwardsCleanURLImage 合法 URL 图片必须原样转发（不能因噎废食）。
func TestChatBridgeForwardsCleanURLImage(t *testing.T) {
	raw := json.RawMessage(`[{"type":"image","source":{"type":"url","url":` +
		jsonString(cleanImgURL) + `}}]`)
	msgs, err := anthropicUserToChatMessages(raw)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	serialized := mustJSON(t, msgs)
	if !strings.Contains(serialized, cleanImgURL) {
		t.Errorf("合法 URL 图片未转发: %s", serialized)
	}
	if strings.Contains(serialized, "image omitted") {
		t.Errorf("合法 URL 不该降级: %s", serialized)
	}
}

// TestResponsesBridgeDropsMalformedURLImage responses 路径同样安全降级。
func TestResponsesBridgeDropsMalformedURLImage(t *testing.T) {
	raw := json.RawMessage(`[{"type":"image","source":{"type":"url","url":` +
		jsonString(malformedImgURL) + `}}]`)
	items, err := anthropicUserToResponses(raw)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	serialized := mustJSON(t, items)
	if strings.Contains(serialized, "tapcode-sce") {
		t.Errorf("畸形 URL 被透传: %s", serialized)
	}
	if !strings.Contains(serialized, "image omitted") {
		t.Errorf("responses 路径也应留下降级说明: %s", serialized)
	}
}

// TestMalformedURLImageInsideToolResult tool_result 内的畸形图片（事故实际形态：
// 工具返回的 content 数组里同时有 image 和 text）不得让 URL 漏到上游。
func TestMalformedURLImageInsideToolResult(t *testing.T) {
	raw := json.RawMessage(`[{"type":"tool_result","tool_use_id":"t1","content":[` +
		`{"type":"image","source":{"type":"url","url":` + jsonString(malformedImgURL) + `}},` +
		`{"type":"text","text":"二维码已生成"}]}]`)
	msgs, err := anthropicUserToChatMessages(raw)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	serialized := mustJSON(t, msgs)
	if strings.Contains(serialized, "tapcode-sce") {
		t.Errorf("tool_result 内的畸形 URL 被透传: %s", serialized)
	}
	if !strings.Contains(serialized, "二维码已生成") {
		t.Errorf("tool_result 的文本输出丢失: %s", serialized)
	}
}

// jsonString 把一个 Go 字符串编码成 JSON 字符串字面量（含引号），便于拼测试 body。
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
