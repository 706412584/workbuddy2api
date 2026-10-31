// video.go 视频生成域：POST {chatBase}/v2/videos/generations（提交）与
// POST {chatBase}/v2/videos/tasks（轮询）。
//
// 来源（2026-10-31 逆向客户端 5.9.3 / 37.10.3-43 的 cli/dist/codebuddy-lite-wb.mjs
// VideoService，并用健康账号实测校验）：
//   - 提交   POST /v2/videos/generations {model, prompt, watermark:true, seconds?,
//     image_url?, last_image_url?, reference_images?[{image_url}],
//     reference_videos?[{video_url}], reference_audios?[{audio_url}],
//     resolution?, aspect_ratio?, enable_audio?} → data.id = taskId
//   - 轮询   POST /v2/videos/tasks {task_id} → data.status=completed|failed|...
//     （客户端按 completed/failed 二分，其余继续轮）
//
// 实测错误码（健康 global 账号直连）：
//   14407 = 视频模型路由不存在（400，换号无意义）
//   14410 = 任务不存在（400，轮询假 id）
//   10001 = 参数缺失（"requires prompt"，400）
// 均为请求体属性而非账号属性 → Submit/Poll 不走账号轮换错误策略，由 server 层
// 直接把上游信封透传给客户端（与 ErrBadRequest「不轮换」同一语义）。
//
// 请求头与官方 VideoService 对齐：Conversation 三头 + 身份头族（runIdentityHeaders）
// + X-Product/X-IDE-*/X-Private-Data + User-Id。chat 路径由 ChatHeaders 承担，
// 这里用 ChatHeaders 复用全套指纹（其差异仅在 body 不做 chat 专属改写）。
package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"workbuddy2api/internal/auth"
)

const (
	videoGenerationsPath = "/v2/videos/generations"
	videoTasksPath       = "/v2/videos/tasks"
)

// VideoSubmit 提交视频生成任务，返回上游 taskId（data.id）。
// body 为客户端原始 JSON（透传，不做 chat 专属改写；上游自行校验参数）。
// 非 2xx 或业务 code != 0 时返回 *Error（Kind 已分类；server 层直接透传信封）。
func (c *Client) VideoSubmit(a *auth.Auth, body []byte, convID string) (json.RawMessage, error) {
	return c.videoJSON(a, http.MethodPost, videoGenerationsPath, body, convID)
}

// VideoPoll 查询视频任务状态。body 为 {"task_id": "..."}。
func (c *Client) VideoPoll(a *auth.Auth, body []byte, convID string) (json.RawMessage, error) {
	return c.videoJSON(a, http.MethodPost, videoTasksPath, body, convID)
}

// videoJSON 发 chat 域视频请求并解信封；语义与 doJSON 一致（错误分类共用 Classify），
// 但请求头走 ChatHeaders（视频与 chat 同为 chatBase 业务面，官方同样带会话三头 +
// 身份头族；BillingHeaders 不带 x-codebuddy-request 等指纹头，不匹配）。
func (c *Client) videoJSON(a *auth.Auth, method, path string, body []byte, convID string) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.chatBase(a)+path, rdr)
	if err != nil {
		return nil, err
	}
	c.ChatHeaders(req, a, "", convID)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.clientFor(a, c.HTTP).Do(req)
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
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 160))
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

// VideoTaskState 轮询响应的常用字段（server 层判定 completed/failed 用）。
type VideoTaskState struct {
	Status string          `json:"status"`
	Raw    json.RawMessage `json:"-"`
}

// ParseVideoTaskState 从轮询 data 中取 status；缺省视为 pending（继续轮）。
// 客户端官方语义：completed / failed 二分，其余一律继续轮（VideoService.pollTask）。
func ParseVideoTaskState(data json.RawMessage) string {
	var st VideoTaskState
	if err := json.Unmarshal(data, &st); err != nil {
		return ""
	}
	return st.Status
}

// videoPollInterval 官方 pollTask 的固定间隔（sleep(pp)）。未在 dist 里拿到常量名
// 对应值前取 5s：与多数视频任务粒度匹配，也低于客户端超时（ph=10min）。
const videoPollInterval = 5 * time.Second

// PollInterval 返回轮询间隔（导出供 server 层使用，避免两处硬编码漂移）。
func PollInterval() time.Duration { return videoPollInterval }
