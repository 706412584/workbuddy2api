package server

import (
	"encoding/json"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestRegionModelsCarriesCreditsAndRegion /v1/models 条目须带 credits 与 region。
// region 是必须的：同名模型两区倍率可能不同（实测 deepseek-v4.1-flash
// CN x0.11 / global x0.00），无 region 则倍率无法归属。
func TestRegionModelsCarriesCreditsAndRegion(t *testing.T) {
	resetModelsCache()
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[
			{"id":"deepseek-v4.1-flash","credits":"x0.11 credits","name":"DS","maxInputTokens":1000000,"maxOutputTokens":128000,"supportsImages":true,"supportsReasoning":true,"vendor":"f"}
		],"agents":[{"name":"cli","models":["deepseek-v4.1-flash"]}]}}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	got := h.regionModels(auth.RegionCN)
	if len(got) == 0 {
		t.Fatal("regionModels 返回空")
	}
	m := got[0]
	if m["credits"] != "x0.11" {
		t.Errorf("credits=%v want \"x0.11\"（去 credits 后缀）", m["credits"])
	}
	if m["region"] != "cn" {
		t.Errorf("region=%v want cn", m["region"])
	}
	if m["supports_images"] != true || m["supports_reasoning"] != true {
		t.Errorf("能力旗标缺失: %+v", m)
	}
	if m["vendor"] != "f" {
		t.Errorf("vendor=%v want f", m["vendor"])
	}
	// 既有字段不得回退
	for _, k := range []string{"id", "object", "created", "owned_by", "context_length", "max_output_tokens"} {
		if _, ok := m[k]; !ok {
			t.Errorf("既有字段 %s 丢失", k)
		}
	}
}

// TestRegionModelsOmitsAbsentCredits 上游未下发 credits 时字段整体省略
// （缺失 ≠ 免费：不能写成 "x0.00"，否则图像模型会被误标为免费）。
func TestRegionModelsOmitsAbsentCredits(t *testing.T) {
	resetModelsCache()
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[
			{"id":"gpt-image-2.5-sunburst","name":"Image","maxInputTokens":1000,"maxOutputTokens":1000}
		],"agents":[{"name":"cli","models":["gpt-image-2.5-sunburst"]}]}}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	got := h.regionModels(auth.RegionCN)
	if len(got) == 0 {
		t.Fatal("regionModels 返回空")
	}
	if v, ok := got[0]["credits"]; ok {
		t.Errorf("credits 应整体省略，却得到 %v", v)
	}
}

// TestModelListDedupeKeepsRegion /v1/models 跨区去重后仍保留 region 标记。
func TestModelListDedupeKeepsRegion(t *testing.T) {
	resetModelsCache()
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[
			{"id":"deepseek-v4.1-flash","credits":"x0.11 credits","name":"DS","maxInputTokens":1000000,"maxOutputTokens":128000}
		],"agents":[{"name":"cli","models":["deepseek-v4.1-flash"]}]}}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	list := h.modelList(nil)
	if len(list) != 1 {
		t.Fatalf("want 1（同名去重）, got %d", len(list))
	}
	if list[0]["region"] != "cn" {
		t.Errorf("region=%v want cn", list[0]["region"])
	}
	// 序列化后字段名符合预期
	b, _ := json.Marshal(list[0])
	if !strings.Contains(string(b), `"credits":"x0.11"`) {
		t.Errorf("序列化结果缺 credits: %s", b)
	}
}
