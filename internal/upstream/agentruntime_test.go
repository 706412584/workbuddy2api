package upstream

import (
	"net/http"
	"testing"
)

// TestInjectClientFingerprintAgentRuntime 断言指纹头组包含 X-Agent-Runtime：
// 官方 buildConversationHeaders（5.9.3 dist）对所有请求恒发该头，取值域
// local-agent|cloud-agent|agent-agent，无上下文时回落 "other"。缺失是可区分特征。
func TestInjectClientFingerprintAgentRuntime(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://example.com", nil)
	c := &Client{}
	c.injectClientFingerprint(req)
	if got := req.Header.Get("X-Agent-Runtime"); got != "other" {
		t.Fatalf("X-Agent-Runtime=%q, want other（官方无 baggage/meta/env 时的回落值）", got)
	}
}
