// appeal.go 11140 申诉端点：POST /__admin/accounts/appeal。
//
// 背景：上游把 11140（账号级风控封控）的官方申诉出口做在应用内反馈
// POST /v2/feedback（见 upstream/feedback.go 与客户端 5.9.3 错误码表
// "errorCode.11140": "Submit feedback to appeal"）。面板在「测试连接」测出
// 11140 后，可一键以该账号身份提交申诉——账号自身凭证即所有权证明。
//
// 安全边界：写操作（对外提交内容），走 admin 既有鉴权（本机放行/非本机要 token）。
// 支持 dry_run=1：只组装并回显将要提交的内容，不发上游（前端按钮默认先 dry-run
// 预览，用户确认后再真发）。
package admin

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/upstream"
)

// appealResult 申诉提交结果。
type appealResult struct {
	OK         bool   `json:"ok"`
	UID        string `json:"uid"`
	Nickname   string `json:"nickname,omitempty"`
	FeedbackID string `json:"feedbackId,omitempty"`
	// DryRun true = 仅预览未提交。
	DryRun bool   `json:"dryRun"`
	Detail string `json:"detail,omitempty"`
	// Payload dry_run 时回显将要提交的表单字段。
	Payload map[string]string `json:"payload,omitempty"`
}

// appeal POST /__admin/accounts/appeal
// 入参 {"uid":"...", "content":"...", "platform":"windows", "dry_run":true}
func (h *Handler) appeal(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UID      string `json:"uid"`
		Content  string `json:"content"`
		Platform string `json:"platform"`
		DryRun   bool   `json:"dry_run"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	uid := strings.TrimSpace(body.UID)
	if uid == "" {
		fail(w, 400, "缺少 uid")
		return
	}
	content := strings.TrimSpace(body.Content)
	if content == "" {
		content = defaultAppealContent(uid)
	}

	file := "workbuddy-" + uid + ".json"
	if !authFileRE.MatchString(file) {
		fail(w, 400, "uid 含非法字符：%s", uid)
		return
	}
	raw, err := os.ReadFile(filepath.Join(h.cfg.AuthDir, file))
	if err != nil {
		fail(w, 404, "读取凭证失败：%v", err)
		return
	}
	acct, err := auth.Parse(raw)
	if err != nil {
		fail(w, 400, "凭证解析失败：%v", err)
		return
	}

	res := appealResult{UID: uid, Nickname: acct.Nickname, DryRun: body.DryRun}
	req := upstream.FeedbackRequest{
		Content:  content,
		Platform: body.Platform,
	}
	if body.DryRun {
		res.Payload = map[string]string{
			"content":     req.Content,
			"client_type": "workbuddy",
			"platform":    platformOr(req.Platform),
			"X-User-Id":   acct.UID,
		}
		writeJSON(w, 200, res)
		return
	}
	if h.cfg.Upstream == nil {
		fail(w, 503, "上游客户端未接线")
		return
	}
	// 反馈提交与探测一样是短 RPC；给 30s 上限防面板挂死。
	type out struct {
		data json.RawMessage
		err  error
	}
	ch := make(chan out, 1)
	go func() {
		d, e := h.cfg.Upstream.FeedbackSubmit(acct, req)
		ch <- out{d, e}
	}()
	select {
	case o := <-ch:
		if o.err != nil {
			res.Detail = o.err.Error()
			writeJSON(w, 200, res) // 失败也是结果：前端原样展示原因
			return
		}
		res.OK = true
		res.FeedbackID = extractFeedbackID(o.data)
		res.Detail = string(o.data)
		log.Printf("[admin] appeal uid=%s: submitted feedbackId=%s", logfmt.UID8(uid), res.FeedbackID)
	case <-time.After(30 * time.Second):
		res.Detail = "提交超过 30s 无响应（上游未答复）"
	}
	writeJSON(w, 200, res)
}

// defaultAppealContent 默认申诉正文：事实陈述 + requestId 占位。
// 不编造使用场景——上游按账号维度风控，如实描述最小探针被拒的事实即可。
func defaultAppealContent(uid string) string {
	return fmt.Sprintf(
		"My account (uid %s) has been returning HTTP 403 {\"code\":11140,\"msg\":\"request illegal\"} "+
			"on chat requests since 2026-10-23, including minimal probes (one system + one \"hi\" message), "+
			"and re-login does not lift it. Credits remain positive. "+
			"Could you please review this restriction and let me know whether it is temporary, "+
			"and what triggered it so I can correct it. Thank you.", uid)
}

// platformOr 归一平台展示值。
func platformOr(p string) string {
	if v := strings.TrimSpace(p); v != "" {
		return v
	}
	return "windows"
}

// extractFeedbackID 从上游信封 data 里挖 feedback id（字段名两种形态都试）。
func extractFeedbackID(data json.RawMessage) string {
	if len(data) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return ""
	}
	for _, k := range []string{"feedback_id", "feedbackId", "id"} {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
			if f, ok := v.(float64); ok {
				return fmt.Sprintf("%.0f", f)
			}
		}
	}
	return ""
}
