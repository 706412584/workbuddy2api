// modelmap.go 客户端模型名 → 上游模型名的映射。
//
// 背景：Anthropic /v1/messages 与 OpenAI /v1/responses 的客户端发的是它们各自的
// 模型名（Claude Code 发 claude-sonnet-4-5-*，Codex 发 gpt-5*），而本网关的上游
// 只有 glm-* / deepseek-* / kimi-* 等。不做映射直接转发会得到
// code=11102（model service info not found），客户端完全不可用。
package server

import "strings"

// ProtocolConfig 协议适配配置（由 cmd/server 从 config.json 注入）。
type ProtocolConfig struct {
	// DefaultModel 未命中映射表时的上游模型；空 = 不映射，原样透传客户端模型名。
	DefaultModel string
	// ModelMapping 客户端模型名 → 上游模型名。
	ModelMapping map[string]string
}

// mapModel 把客户端模型名解析为上游模型名。
//
// 解析顺序：
//  1. 精确匹配 ModelMapping
//  2. 前缀匹配（取最长 key），要求命中处是名字边界——即剩余部分是空、或以下一字符
//     为 '-' / '.' 开头。这样 claude-sonnet-4-5 能命中 claude-sonnet-4-5-20250929，
//     而 glm-5 不会命中 glm-5x 这类无关名字。
//  3. DefaultModel（若配置）
//  4. 原样返回客户端模型名（既无映射也无默认 = 透传，由上游报错）
func mapModel(clientModel string, cfg ProtocolConfig) string {
	if clientModel == "" {
		return cfg.DefaultModel
	}
	if m, ok := cfg.ModelMapping[clientModel]; ok && m != "" {
		return m
	}
	// 最长 key 优先：避免短 key（如 claude-sonnet-4）抢走更长 key 的匹配。
	var bestKey, bestVal string
	for k, v := range cfg.ModelMapping {
		if k == "" || v == "" || len(k) <= len(bestKey) {
			continue
		}
		if !strings.HasPrefix(clientModel, k) {
			continue
		}
		if rest := clientModel[len(k):]; rest != "" && rest[0] != '-' && rest[0] != '.' {
			continue // 非名字边界，不算命中
		}
		bestKey, bestVal = k, v
	}
	if bestKey != "" {
		return bestVal
	}
	if cfg.DefaultModel != "" {
		return cfg.DefaultModel
	}
	return clientModel
}
