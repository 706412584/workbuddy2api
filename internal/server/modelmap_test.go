package server

import "testing"

func TestMapModel(t *testing.T) {
	cfg := ProtocolConfig{
		DefaultModel: "deepseek-v4.1-flash",
		ModelMapping: map[string]string{
			"claude-sonnet-4-5": "deepseek-v4.1-flash",
			"claude-opus-4-1":   "glm-5.2",
			"gpt-5-codex":       "glm-5.2",
		},
	}
	cases := []struct {
		name  string
		model string
		want  string
	}{
		{"精确匹配", "claude-sonnet-4-5", "deepseek-v4.1-flash"},
		{"带日期后缀走前缀匹配", "claude-sonnet-4-5-20250929", "deepseek-v4.1-flash"},
		{"另一个前缀", "claude-opus-4-1-20250805", "glm-5.2"},
		{"Codex 精确", "gpt-5-codex", "glm-5.2"},
		{"Codex 变体走前缀", "gpt-5-codex-mini", "glm-5.2"},
		{"未命中回落默认", "claude-3-5-haiku", "deepseek-v4.1-flash"},
		{"完全不认识的模型也回落默认", "some-random-model", "deepseek-v4.1-flash"},
		{"空模型名给默认", "", "deepseek-v4.1-flash"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mapModel(c.model, cfg); got != c.want {
				t.Errorf("mapModel(%q)=%q want %q", c.model, got, c.want)
			}
		})
	}
}

// TestMapModelPrefixRequiresNameBoundary 前缀匹配必须落在名字边界上，
// 否则 glm-5 会错误命中 glm-5x 这类无关模型名。
func TestMapModelPrefixRequiresNameBoundary(t *testing.T) {
	cfg := ProtocolConfig{
		ModelMapping: map[string]string{"glm-5": "target-model"},
	}
	// 边界情形：- 与 . 开头算命中。
	for _, m := range []string{"glm-5", "glm-5-2025", "glm-5.2"} {
		if got := mapModel(m, cfg); got != "target-model" {
			t.Errorf("%q 应命中前缀（名字边界），got %q", m, got)
		}
	}
	// 非边界：glm-5x 不该命中。
	if got := mapModel("glm-5x", cfg); got != "glm-5x" {
		t.Errorf("glm-5x 不该命中前缀，got %q want 原样透传", got)
	}
}

// TestMapModelLongestPrefixWins 多个 key 都能前缀命中时取最长者，
// 避免短 key 抢走更精确的匹配。
func TestMapModelLongestPrefixWins(t *testing.T) {
	cfg := ProtocolConfig{
		ModelMapping: map[string]string{
			"claude-sonnet-4":   "short-target",
			"claude-sonnet-4-5": "long-target",
		},
	}
	if got := mapModel("claude-sonnet-4-5-20250929", cfg); got != "long-target" {
		t.Errorf("got %q want long-target（最长 key 优先）", got)
	}
	if got := mapModel("claude-sonnet-4-1", cfg); got != "short-target" {
		t.Errorf("got %q want short-target", got)
	}
}

// TestMapModelNoMappingPassesThrough 未配置映射与默认时原样透传，
// 由上游报错而不是网关擅自替换模型。
func TestMapModelNoMappingPassesThrough(t *testing.T) {
	empty := ProtocolConfig{}
	if got := mapModel("glm-5.2", empty); got != "glm-5.2" {
		t.Errorf("空配置应透传，got %q", got)
	}
	// 只有映射表、没有默认值：未命中同样透传。
	only := ProtocolConfig{ModelMapping: map[string]string{"a": "b"}}
	if got := mapModel("glm-5.2", only); got != "glm-5.2" {
		t.Errorf("未命中且无默认应透传，got %q", got)
	}
}

// TestMapModelSkipsEmptyEntries 空 key/空值是配置错误（启动时已拒绝），
// 此处确认即便存在也不会被选中，避免把模型名映射成空串。
func TestMapModelSkipsEmptyEntries(t *testing.T) {
	cfg := ProtocolConfig{
		DefaultModel: "fallback",
		ModelMapping: map[string]string{"": "x", "claude": ""},
	}
	if got := mapModel("claude-sonnet", cfg); got != "fallback" {
		t.Errorf("got %q want fallback（空值项应跳过）", got)
	}
}
