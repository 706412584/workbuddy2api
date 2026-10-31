// video.go 视频生成端点：POST /v1/videos/generations 与 POST /v1/videos/tasks。
//
// 与 chat 不同，视频是「提交 → 轮询」两段式任务接口，同一次生成的提交与轮询
// 必须落在**同一个账号**上（task_id 归属账号，跨账号轮询上游按 14410 task not
// found 拒绝）。因此这里不复用 forwardChat 的逐请求轮换，而是：
//
//   - 提交：走一次轻量轮换（复用 keyPickPred / 健康代理过滤），成功后把
//     task_id → uid 记入进程内绑定表，并把 uid 以 X-Video-Account 响应头回显；
//   - 轮询：优先按 task_id 查绑定号直连该号；查不到（网关重启丢失 / 外部导入的
//     task）回落普通轮换。
//
// 绑定表是进程内存态（重启即失）：任务粒度以分钟计，重启后客户端重试成本低，
// 不值得为此引入落盘状态。
//
// 错误透传：上游对视频参数/任务不存在返回 400 业务信封（14407/14410/10001），
// 均为请求体属性而非账号属性 → 与 chat 的 ErrBadRequest 同语义「不轮换」，把
// 上游信封原样回给客户端（不做 OpenAI 包装，客户端 VideoService 按 code/msg 解析）。
package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/upstream"
)

// videoTasks task_id → (uid, model) 绑定表。
type videoTasks struct {
	mu     sync.RWMutex
	m      map[string]string
	models map[string]string // task_id → 提交时的 model（轮询时补区域路由用）
	ttl    map[string]time.Time
	ttlD   time.Duration
}

func newVideoTasks() *videoTasks {
	return &videoTasks{m: map[string]string{}, models: map[string]string{}, ttl: map[string]time.Time{}, ttlD: 24 * time.Hour}
}

func (v *videoTasks) bind(taskID, uid, model string) {
	if taskID == "" || uid == "" {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.m[taskID] = uid
	if model != "" {
		v.models[taskID] = model
	}
	v.ttl[taskID] = time.Now().Add(v.ttlD)
	// 顺手清理过期项（低频路径，O(n) 可接受）。
	now := time.Now()
	for k, t := range v.ttl {
		if now.After(t) {
			delete(v.ttl, k)
			delete(v.m, k)
			delete(v.models, k)
		}
	}
}

func (v *videoTasks) owner(taskID string) string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if t, ok := v.ttl[taskID]; ok && time.Now().After(t) {
		return ""
	}
	return v.m[taskID]
}

// modelOf 取提交时记录的模型名；无记录返回空串。
func (v *videoTasks) modelOf(taskID string) string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if t, ok := v.ttl[taskID]; ok && time.Now().After(t) {
		return ""
	}
	return v.models[taskID]
}

// videoHandlerDef 视频端点公共体：提交与轮询共享。
type videoHandlerDef struct {
	submit bool // true = /generations，false = /tasks
}

// videoGenerations POST /v1/videos/generations
func (h *Handler) videoGenerations(w http.ResponseWriter, r *http.Request, key *APIKeySpec) {
	h.video(w, r, key, true)
}

// videoTasks POST /v1/videos/tasks —— 原生形态（上游同构）。
// Grok 客户端走 GET /v1/videos/{id}（videoTaskGet），本端点保留给直连客户端。
func (h *Handler) videoTasks(w http.ResponseWriter, r *http.Request, key *APIKeySpec) {
	h.video(w, r, key, false)
}

// videoTaskGetPath GET /v1/videos/{id} 的路由适配：从路径取任务 id。
func (h *Handler) videoTaskGetPath(w http.ResponseWriter, r *http.Request, key *APIKeySpec) {
	h.videoTaskGet(w, r, key, r.PathValue("id"))
}

// videoTaskGet GET /v1/videos/{id} —— Grok 协议兼容（AndroidCodeStudio
// VideoGenerationTool.isGrokProtocol 走这条）。
//
// 差异翻译（其余字段原样透传）：
//   - 路径参数 id → 上游 body {"task_id": id}，走与 POST /tasks 相同的账号绑定
//   - status 映射：上游 queued/in_progress/processing → "in_progress"（Grok 客户端
//     只认 done/failed 之外的"继续轮"）；completed → "done"；failed → "failed"
//   - 结果 URL：上游 data[0].url → video.url（Grok 客户端读 video.url）
//   - id 回显：request_id = id
//
// 上游的 completed_at/created_at/usage 等字段保留，多给不伤。
func (h *Handler) videoTaskGet(w http.ResponseWriter, r *http.Request, key *APIKeySpec, taskID string) {
	if taskID == "" {
		writeOpenAIError(w, http.StatusNotFound, "missing_task_id", "video task id is required")
		return
	}
	body, _ := json.Marshal(map[string]string{"task_id": taskID})
	data := h.videoPollRaw(w, r, key, body, taskID)
	if data == nil {
		return // 错误已写出
	}
	writeJSON(w, http.StatusOK, grokVideoStatus(data, taskID))
}

// grokVideoStatus 把上游任务信封整形成 Grok 协议形态。
// 上游形态（实测）：{"id":...,"status":"completed","created_at":...,
// "completed_at":...,"data":[{"url":"..."}],"usage":{...}}
func grokVideoStatus(data json.RawMessage, taskID string) map[string]any {
	var up struct {
		ID          string `json:"id"`
		Status      string `json:"status"`
		CreatedAt   int64  `json:"created_at"`
		CompletedAt int64  `json:"completed_at"`
		Data        []struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	_ = json.Unmarshal(data, &up)
	id := up.ID
	if id == "" {
		id = taskID
	}
	out := map[string]any{
		"request_id": id,
		"id":         id,
		"status":     grokStatusOf(up.Status),
		"created_at": up.CreatedAt,
	}
	if up.CompletedAt > 0 {
		out["completed_at"] = up.CompletedAt
	}
	if len(up.Data) > 0 && up.Data[0].URL != "" {
		out["video"] = map[string]string{"url": up.Data[0].URL}
		// 顶层 url 也给一份：FrameBaker/Agnes 系客户端读顶层。
		out["url"] = up.Data[0].URL
	}
	// usage 原样回传（credit 计费可见）。
	var full map[string]any
	if json.Unmarshal(data, &full) == nil {
		if u, ok := full["usage"]; ok {
			out["usage"] = u
		}
	}
	return out
}

// grokStatusOf 上游状态 → Grok 客户端状态。
// Grok 客户端二分：done 成功 / failed|expired 失败 / 其余继续轮。
func grokStatusOf(s string) string {
	switch s {
	case "completed":
		return "done"
	case "failed":
		return "failed"
	case "":
		return "unknown"
	default: // queued / in_progress / processing 等
		return "in_progress"
	}
}

// videoPollRaw 执行一次轮询并返回上游 data；失败时已写响应并返回 nil。
// 供 POST /tasks 与 GET /videos/{id} 两条路径共用选号/绑定/错误整形。
func (h *Handler) videoPollRaw(w http.ResponseWriter, r *http.Request, key *APIKeySpec, body []byte, taskID string) json.RawMessage {
	var peek struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)
	// model 可由客户端带上（Grok 客户端不带）——查绑定表定区域；无绑定则不限区域。
	peek.Model = h.videoModelForTask(taskID, peek.Model)

	var convID = "wb2api-video-" + taskID

	var pred []auth.Region
	if peek.Model != "" {
		pred = regionsForModel(peek.Model)
	}
	regions, ok2 := constrainRegions(pred, key)
	if !ok2 {
		writeOpenAIError(w, http.StatusNotFound, "model_not_found", "model "+peek.Model+" does not exist")
		return nil
	}
	pickPred := h.pickPredWithHealthyProxy(h.keyPickPred(key, regions))

	var acct *auth.Auth
	if uid := h.videoT.owner(taskID); uid != "" {
		acct = h.cfg.Pool.PickByUIDForModelWhere(uid, peek.Model, pickPred)
		if acct == nil {
			writeOpenAIError(w, http.StatusConflict, "video_account_unavailable",
				"task belongs to an account that is currently cooling/disabled; retry later")
			return nil
		}
	}
	if acct == nil {
		for i := 0; i < h.cfg.MaxRotate; i++ {
			a := h.cfg.Pool.PickExcludingForModelWhere(nil, peek.Model, pickPred)
			if a == nil {
				break
			}
			if a.NeedsRefresh(h.cfg.RefreshSkew) {
				if err := h.cfg.Upstream.RefreshToken(a); err != nil {
					log.Printf("WARN: [server] video refresh uid=%s: %v", logfmt.UID8(a.UID), err)
					continue
				}
				_ = a.SaveAtomic()
			}
			acct = a
			break
		}
	}
	if acct == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", "no available account for video request")
		return nil
	}
	data, err := h.cfg.Upstream.VideoPoll(acct, body, convID)
	if err != nil {
		h.videoWriteUpstreamErr(w, acct, err, false)
		return nil
	}
	return data
}

// videoModelForTask 轮询时补齐 model：优先客户端带的；否则从提交时的绑定记录
// 取当时用的模型（videoT 记 uid 时顺带记 model）。都没有则空（不限区域）。
func (h *Handler) videoModelForTask(taskID, hint string) string {
	if hint != "" {
		return hint
	}
	return h.videoT.modelOf(taskID)
}

func (h *Handler) video(w http.ResponseWriter, r *http.Request, key *APIKeySpec, submit bool) {
	body, ok := h.readBodyOrFail(w, r, false)
	if !ok {
		return
	}
	// Grok 协议兼容（AndroidCodeStudio / xAI 客户端）：时长字段名是 duration
	// （Grok 1-15s），上游 kling 用 seconds。客户端带了 duration 且未带 seconds
	// 时翻译过去，OpenAI 风格客户端不受影响。
	if obj := parseJSONObj(body); obj != nil {
		if _, has := obj["seconds"]; !has {
			if d, ok := obj["duration"].(float64); ok && d > 0 {
				obj["seconds"] = int(d)
				if nb, err := json.Marshal(obj); err == nil {
					body = nb
				}
			}
		}
	}
	var peek struct {
		Model  string `json:"model"`
		TaskID string `json:"task_id"`
	}
	_ = json.Unmarshal(body, &peek)

	var convID string
	if submit {
		convID = "" // 每次提交独立会话（ChatHeaders 现场生成）
	} else {
		convID = "wb2api-video-" + peek.TaskID // 同任务的轮询复用同一会话标识，降低会话抖动
	}

	// 选号：
	//   提交 → 普通轮换（模型区域 ∩ 密钥约束）。
	//   轮询 → task_id 绑定号优先；无绑定（重启丢失/外部 task）回落普通轮换。
	var pred []auth.Region
	if peek.Model != "" {
		pred = regionsForModel(peek.Model)
	}
	regions, ok2 := constrainRegions(pred, key)
	if !ok2 {
		writeOpenAIError(w, http.StatusNotFound, "model_not_found", "model "+peek.Model+" does not exist")
		return
	}
	pickPred := h.keyPickPred(key, regions)
	pickPred = h.pickPredWithHealthyProxy(pickPred)

	var acct *auth.Auth
	if !submit && peek.TaskID != "" {
		if uid := h.videoT.owner(peek.TaskID); uid != "" {
			acct = h.cfg.Pool.PickByUIDForModelWhere(uid, peek.Model, pickPred)
			if acct == nil {
				// 绑定号此刻不可用（冷却/禁用）：视频任务必须回原号，不换号轮询
				// ——换号拿到的必然是 14410。直接把绑定号不可用报给客户端。
				writeOpenAIError(w, http.StatusConflict, "video_account_unavailable",
					"task belongs to an account that is currently cooling/disabled; retry later")
				return
			}
		}
	}
	if acct == nil {
		for i := 0; i < h.cfg.MaxRotate; i++ {
			a := h.cfg.Pool.PickExcludingForModelWhere(nil, peek.Model, pickPred)
			if a == nil {
				break
			}
			if a.NeedsRefresh(h.cfg.RefreshSkew) {
				if err := h.cfg.Upstream.RefreshToken(a); err != nil {
					log.Printf("WARN: [server] video refresh uid=%s: %v", logfmt.UID8(a.UID), err)
					continue
				}
				_ = a.SaveAtomic()
			}
			acct = a
			break
		}
	}
	if acct == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", "no available account for video request")
		return
	}

	data, err := func() (json.RawMessage, error) {
		if submit {
			return h.cfg.Upstream.VideoSubmit(acct, body, convID)
		}
		return h.cfg.Upstream.VideoPoll(acct, body, convID)
	}()
	if err != nil {
		h.videoWriteUpstreamErr(w, acct, err, submit)
		return
	}

	if submit {
		// 提交成功：绑定 task_id → (uid, model)，并回显账号（客户端可记录以便排查）。
		var submitted struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(data, &submitted) == nil && submitted.ID != "" {
			h.videoT.bind(submitted.ID, acct.UID, peek.Model)
			w.Header().Set("X-Video-Account", acct.UID)
			// Grok 协议客户端读 request_id —— 补一个别名（同值，无副作用）。
			// data 是上游原文切片，重建 map 而非改切片。
			var full map[string]any
			if json.Unmarshal(data, &full) == nil {
				if _, has := full["request_id"]; !has {
					full["request_id"] = submitted.ID
					writeJSON(w, http.StatusOK, full)
					return
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, data)
}

// parseJSONObj 宽松解出 body 对象；非对象返回 nil。
func parseJSONObj(body []byte) map[string]any {
	var m map[string]any
	if json.Unmarshal(body, &m) == nil {
		return m
	}
	return nil
}

// videoWriteUpstreamErr 把上游错误回给客户端。
//
// 判据：*upstream.Error 且 Kind 属于「请求体属性」（ErrBadRequest 类 400 / ErrNotFound）
// 或任何 4xx 业务信封 → 透传状态码与原信封文案，不轮换不罚号；传输层错误 /
// 5xx → 502/503，同样不罚号（视频任务与账号健康无关，罚号语义不在本路径）。
// 仅 ErrSafetyBanned（提交撞上被封号）按既有策略冷却该号并 503，换号由客户端重试。
func (h *Handler) videoWriteUpstreamErr(w http.ResponseWriter, acct *auth.Auth, err error, submit bool) {
	var ue *upstream.Error
	if !errors.As(err, &ue) {
		log.Printf("WARN: [server] video transport uid=%s: %v", logfmt.UID8(acct.UID), err)
		writeOpenAIError(w, http.StatusBadGateway, "upstream_transport", err.Error())
		return
	}
	if ue.Kind == upstream.ErrSafetyBanned {
		h.applyErrorPolicy(acct.UID, ue.Kind, ue.Msg, "")
		writeOpenAIError(w, http.StatusServiceUnavailable, "account_restricted",
			"selected video account is restricted (11140); retry to pick another account")
		return
	}
	// 其余：透传上游原文。提取信封里的 msg 使客户端能按 code 判定
	// （上游 body 形如 {"code":14407,"msg":"...","requestId":"..."}）。
	status := ue.Status
	if status == 0 {
		status = http.StatusBadGateway
	}
	writeJSON(w, status, map[string]any{"code": videoBizCode(ue.Msg), "msg": ue.Msg})
}

// videoBizCode 从 *upstream.Error 的 Msg 里还原业务码。两种形态：
//   - status>=400：Msg 是上游原始 JSON 信封 {"code":14407,"msg":...}
//   - 业务 code!=0（HTTP 200）：Msg 是 doJSON 拼的 "code=14407 msg=..."
//
// 提取失败回 -1（客户端按 msg 兜底）。
func videoBizCode(msg string) int {
	if i := strings.Index(msg, `"code":`); i >= 0 {
		rest := strings.TrimPrefix(msg[i:], `"code":`)
		rest = rest[:leadingIntLen(rest)]
		if n, err := strconv.Atoi(rest); err == nil {
			return n
		}
	}
	if i := strings.Index(msg, "code="); i >= 0 {
		rest := msg[i+5:]
		if j := strings.Index(rest, " "); j > 0 {
			rest = rest[:j]
		}
		if n, err := strconv.Atoi(strings.TrimSpace(rest)); err == nil {
			return n
		}
	}
	return -1
}

// leadingIntLen 返回前导整数（含符号/空白前缀）的长度；无数字返回 0。
func leadingIntLen(s string) int {
	i := 0
	for i < len(s) && (s[i] == '-' || s[i] == '+' || s[i] == ' ') {
		i++
	}
	digits := 0
	for i+digits < len(s) && s[i+digits] >= '0' && s[i+digits] <= '9' {
		digits++
	}
	if digits == 0 {
		return 0
	}
	return i + digits
}
