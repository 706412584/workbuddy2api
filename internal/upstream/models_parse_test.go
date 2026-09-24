package upstream

import (
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// 以下 fixture 的模型条目**逐字取自生产响应**（2026-09-25 实测 CN /console/... 与
// global /v3/config），仅裁剪到 3 个模型 + cli agent。按记忆里的教训：
// 测试 body 必须抄生产原文，自己编的 body 会让「字段名对不上」这类 bug 逃过测试。
const cnModelsFixture = `{"code":0,"data":{"models":[
{"id":"hy3","credits":"x0.00 credits","name":"HY3","maxInputTokens":192000,"maxOutputTokens":32000,"vendor":"f","supportsToolCall":true},
{"id":"glm-5.3","credits":"x0.79","name":"GLM-5.3","maxInputTokens":1000000,"maxOutputTokens":64000,"vendor":"f","supportsReasoning":true,"canDisableThinking":true},
{"id":"deepseek-v4.1-flash","credits":"x0.11 credits","name":"Deepseek-V4.1-Flash","descriptionZh":"DeepSeek 旗舰模型","maxInputTokens":1000000,"maxOutputTokens":128000,"vendor":"f","supportsImages":true,"supportsReasoning":true}
],"agents":[{"name":"cli","models":["hy3","glm-5.3","deepseek-v4.1-flash"]}]}}`

const globalModelsFixture = `{"code":0,"data":{"models":[
{"id":"deepseek-v4.1-flash","credits":"x0.00","name":"Deepseek-V4.1-Flash","maxInputTokens":1000000,"maxOutputTokens":128000,"vendor":"f","supportsImages":true},
{"id":"hy3","credits":"x0.00","name":"HY3","maxInputTokens":192000,"maxOutputTokens":32000,"vendor":"f"},
{"id":"glm-5.3","credits":"x0.79","name":"GLM-5.3","maxInputTokens":1000000,"maxOutputTokens":64000,"vendor":"f"}
],"agents":[{"name":"cli","models":["deepseek-v4.1-flash","hy3","glm-5.3"]}]}}`

// TestParseModelsCreditsPerRegion 同名模型两区倍率不同必须如实解析
// （实测 deepseek-v4.1-flash：CN x0.11 / global x0.00）。
func TestParseModelsCreditsPerRegion(t *testing.T) {
	cn, err := parseModels([]byte(cnModelsFixture))
	if err != nil {
		t.Fatalf("CN parse: %v", err)
	}
	gl, err := parseModels([]byte(globalModelsFixture))
	if err != nil {
		t.Fatalf("global parse: %v", err)
	}
	find := func(ms []ModelInfo, id string) *ModelInfo {
		for i := range ms {
			if ms[i].ID == id {
				return &ms[i]
			}
		}
		return nil
	}
	cnDS := find(cn, "deepseek-v4.1-flash")
	glDS := find(gl, "deepseek-v4.1-flash")
	if cnDS == nil || glDS == nil {
		t.Fatal("deepseek-v4.1-flash 必须两区都存在")
	}
	if cnDS.Credits != "x0.11 credits" {
		t.Errorf("CN credits=%q want \"x0.11 credits\"", cnDS.Credits)
	}
	if glDS.Credits != "x0.00" {
		t.Errorf("global credits=%q want \"x0.00\"", glDS.Credits)
	}
	// 两区同名不同倍率 —— 这正是必须保留 region 字段的理由。
	if cnDS.Credits == glDS.Credits {
		t.Error("两区倍率应当不同（CN x0.11 vs global x0.00），测试 fixture 或解析有误")
	}
}

// TestParseModelsFieldPassthrough 展示字段全量透出（能力旗标/描述/vendor）。
func TestParseModelsFieldPassthrough(t *testing.T) {
	out, err := parseModels([]byte(cnModelsFixture))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, m := range out {
		if m.ID != "deepseek-v4.1-flash" {
			continue
		}
		if !m.SupportsImages || !m.SupportsReasoning {
			t.Errorf("能力旗标丢失: images=%v reasoning=%v", m.SupportsImages, m.SupportsReasoning)
		}
		if m.Vendor != "f" {
			t.Errorf("vendor=%q want f", m.Vendor)
		}
		if m.Description != "DeepSeek 旗舰模型" {
			t.Errorf("description=%q want 中文描述", m.Description)
		}
		if m.ContextWindow != 1000000 || m.MaxTokens != 128000 {
			t.Errorf("尺寸 ctx=%d max=%d", m.ContextWindow, m.MaxTokens)
		}
		return
	}
	t.Fatal("未找到 deepseek-v4.1-flash")
}

// TestParseModelsCreditsAbsentIsNotEmpty 缺失 credits ≠ 免费（上游 panel 强调的坑）：
// 字段不下发时保持空串，展示侧据此省略，绝不能当 0。
func TestParseModelsCreditsAbsentIsNotEmpty(t *testing.T) {
	raw := `{"code":0,"data":{"models":[
		{"id":"gpt-image-2.5-sunburst","name":"Image","maxInputTokens":1000,"maxOutputTokens":1000}
	],"agents":[{"name":"cli","models":["gpt-image-2.5-sunburst"]}]}}`
	out, err := parseModels([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 model, got %d", len(out))
	}
	if out[0].Credits != "" {
		t.Errorf("credits=%q want 空串（缺失≠免费）", out[0].Credits)
	}
}

// TestModelsPathForPerRegion 两域路径分叉：global 走 /v3/config
// （实测 /console/... 在 global 是 HTTP 500 APISIX 错误页，这是 intl 侧动态模型
// 长期不可用、只能靠静态表兜底的根因）。
func TestModelsPathForPerRegion(t *testing.T) {
	c := New()
	cn := &auth.Auth{UID: "cn", Domain: ""}
	gl := &auth.Auth{UID: "g", Domain: "www.workbuddy.ai"}
	if got := c.modelsPathFor(cn); !strings.Contains(got, "/console/") {
		t.Errorf("CN path=%q want /console/...", got)
	}
	if got := c.modelsPathFor(gl); got != "/v3/config" {
		t.Errorf("global path=%q want /v3/config", got)
	}
}
