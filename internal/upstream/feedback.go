// feedback.go 用户反馈/申诉通道：POST {chatBase}/v2/feedback（multipart）。
//
// 来源（2026-10-31 逆向客户端 5.9.3 app.asar 的 createFeedbackFacade）：
// 上游把 11140（账号级风控封控）的官方申诉出口做在**应用内反馈**——错误码表里
// 11140 的处置就是「Submit feedback to appeal」。该接口以账号自身身份提交，
// 天然证明账号所有权（比找客服邮箱可靠）。
//
// 官方 facade 形状（multipart/form-data）：
//   字段：content / client_type=workbuddy / platform=windows|macos|linux /
//         client_version / prestage? / feedback_id? / images[]? / log?
//   请求头：X-Machine-Id（客户端 machineId）、X-User-Id、Authorization、
//           X-Enterprise-Id + X-Tenant-Id（企业号）、X-Domain
// 响应：{code:0, data:{feedback_id|id}} 或 {success:true, feedbackId}
//
// 网关侧 machineId 复用账号稳定派生（headers.go deriveAccountStableID "machine"），
// 与 X-Machine-ID 出站语义一致（同一账号恒定一台虚拟设备）。
package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"

	"workbuddy2api/internal/auth"
)

const feedbackPath = "/v2/feedback"

// FeedbackRequest 申诉内容。
type FeedbackRequest struct {
	Content     string // 申诉正文（必填；空则调用方先拒）
	ClientType  string // 官方恒 "workbuddy"（resolveClientType desktop→workbuddy）
	Platform    string // windows / macos / linux；空 = windows（网关部署面）
	Prestage    string // 可选，官方传 "true"/"false" 字符串
	FeedbackRef string // 可选，跟进已有 feedback_id
}

// FeedbackSubmit 提交反馈/申诉，返回上游 data（含 feedback id）。
// 复用 videoJSON 的信封语义（chatBase 业务面 + ChatHeaders 指纹头 + Classify）；
// 官方此路径不走 conversation 三头，但 ChatHeaders 带上无害（上游只校验存在性）。
func (c *Client) FeedbackSubmit(a *auth.Auth, req FeedbackRequest) (json.RawMessage, error) {
	contentType, body, err := buildFeedbackBody(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequest(http.MethodPost, c.chatBase(a)+feedbackPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// 头对齐官方 facade：X-Machine-Id（注意官方该头是 Id 不是 ID）+ 身份族由
	// ChatHeaders 承担；这里补 facade 专属的三个。
	c.ChatHeaders(httpReq, a, "", "")
	httpReq.Header.Set("Content-Type", contentType)
	if tok := c.resolveDeviceToken(a); tok != "" {
		// 签到/反馈同属 billing 语义的设备校验面（官方 buildHeadersWithTuringToken）。
		httpReq.Header.Set("X-Device-Token", tok)
	}
	resp, err := c.clientFor(a, c.HTTP).Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 300)}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		// 官方还有 {success:true, feedbackId} 非 code 信封形态，兜底原样返回。
		return json.RawMessage(raw), nil
	}
	if env.Code != 0 {
		kind := Classify(resp.StatusCode, env.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 200))}
	}
	return env.Data, nil
}

// buildFeedbackBody 组 multipart；字段名与官方 facade 逐一对齐。
func buildFeedbackBody(req FeedbackRequest) (string, []byte, error) {
	platform := strings.TrimSpace(req.Platform)
	if platform == "" {
		platform = "windows"
	}
	clientType := strings.TrimSpace(req.ClientType)
	if clientType == "" {
		clientType = "workbuddy"
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("content", req.Content)
	_ = w.WriteField("client_type", clientType)
	_ = w.WriteField("platform", platform)
	if v := strings.TrimSpace(req.Prestage); v != "" {
		_ = w.WriteField("prestage", v)
	}
	if v := strings.TrimSpace(req.FeedbackRef); v != "" {
		_ = w.WriteField("feedback_id", v)
	}
	if err := w.Close(); err != nil {
		return "", nil, err
	}
	return w.FormDataContentType(), buf.Bytes(), nil
}
