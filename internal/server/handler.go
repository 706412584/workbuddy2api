// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/prompt"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

// APIKeySpec 一个 API key 及其绑定的上游区域。
// Region 为空表示不限区域（等价于旧的单一 api_key 行为）；绑定后该密钥的请求
// 只会使用对应区域的账号，其 /v1/models 与 /status 也按该区域过滤。
type APIKeySpec struct {
	Key    string      `json:"key"`
	Region auth.Region `json:"region"`
	Name   string      `json:"name"` // 可选，用于日志/排查标识
}

// Config handler 依赖。
type Config struct {
	Pool     *pool.Pool
	Upstream *upstream.Client
	// APIKey 旧版单一密钥（空 = 不鉴权）。保留以兼容既有配置；
	// 语义等价于 APIKeys 中 Region 为空的一项（不限区域）。
	APIKey string
	// APIKeys 多密钥列表，每项可绑定区域。与 APIKey 合并生效；
	// 两者皆空 = 不鉴权。
	APIKeys []APIKeySpec
	// Protocol 协议适配配置（Anthropic /v1/messages、OpenAI /v1/responses 的模型名映射）。
	Protocol  ProtocolConfig
	MaxRotate int // 单请求最多换号次数，默认 3
	// Session 会话粘性路由器（可选；nil = 关闭粘性，纯 Pick 轮换）。
	Session *session.Router
	// StickyCount 返回当前粘性会话绑定数（供 /status）；nil 时报告 0。
	// 注意：该计数是全局的，不随密钥区域过滤（router 未按区域记账）。
	StickyCount func() int
	// RedisMode 观测字段（"upstash" / "noop"），供 /status 透出。
	RedisMode    string
	SoftCooldown time.Duration // 429/限流文案软冷却基数，默认 600s（连续触发指数退避，封顶 soft_rate_max）
	RefreshSkew  time.Duration // token 提前刷新窗口，默认 10m
	// MaxBodyBytes 请求体大小上限；<=0 兜底 defaultMaxBodyBytes。
	// 超限直接 413 request_body_too_large，不再静默截断后喂给上游。
	MaxBodyBytes int64
	// PromptMode "custom"（网关用自有提示词替换 system）/ "passthrough"（透传）。
	PromptMode string
	// PromptText custom 模式下注入的系统提示词文本（来自 config.PromptText）。
	PromptText string
	// Web 未命中 API 路由时的兜底处理器（管理面板的静态资源）。nil = 不提供面板。
	Web http.Handler
	// Admin 管理接口，挂在 /__admin/ 前缀下；自身负责本机来源校验。nil = 不提供。
	Admin http.Handler
	// LoopGuard 思考死循环判据与重试上限。Enabled=false 时完全不检测。
	LoopGuard LoopGuardConfig
}

// LoopGuardConfig 思考死循环的检测与重试参数。
type LoopGuardConfig struct {
	// Enabled 关闭时完全不检测，保持原有行为。
	Enabled bool
	// MinThinkChars 思考字符数下限（默认 3 万）：样本太小不下判断。
	// 是下限而非主判据 —— 设得过大反而会屏蔽停滞判据。
	MinThinkChars int
	// MaxDistinctRatio 唯一块占比上限（默认 0.15）：低于此值判为重复文本。
	MaxDistinctRatio float64
	// MinElapsedSeconds 最早可判定的耗时秒数（默认 30），给正常长思考留空间。
	MinElapsedSeconds int
	// MaxRetries 命中后最多重试几次，用尽则回错误（默认 maxLoopRetries）。
	MaxRetries int
	// MaxStaleChunks 停滞窗口（块数，默认 2000 = 48KB）：连续这么多块没有新内容
	// 即判空转。主判据，与循环周期长度无关。
	MaxStaleChunks int
}

// toLoopGuard 转成运行时判据。
func (c LoopGuardConfig) toLoopGuard() *LoopGuard {
	if !c.Enabled {
		return nil
	}
	g := DefaultLoopGuard()
	if c.MinThinkChars > 0 {
		g.MinThinkChars = c.MinThinkChars
	}
	if c.MaxDistinctRatio > 0 {
		g.MaxDistinctRatio = c.MaxDistinctRatio
	}
	if c.MinElapsedSeconds > 0 {
		g.MinElapsed = time.Duration(c.MinElapsedSeconds) * time.Second
	}
	if c.MaxStaleChunks > 0 {
		g.MaxStaleChunks = c.MaxStaleChunks
	}
	g.MaxRetries = c.MaxRetries
	if g.MaxRetries <= 0 {
		g.MaxRetries = maxLoopRetries
	}
	return &g
}

// maxLoopRetries 思考死循环命中后的重试上限（不含首次尝试）。
// 取 2：实测同一账号上循环会连续复现（见 loopguard.go 的背景数据），
// 给两次「追加提示 + 换号」的机会；仍不行说明是模型侧稳定故障，继续重试只是
// 让客户端多等，不如尽快回明确错误。
const maxLoopRetries = 2

// notFoundCooldown 上游 404 的固定短冷却时长。
// 与 SoftCooldown 分流的原因：404 是上游**偶发**路径缺失，不是"本账号在限流"，
// 若共用 soft_rate（600s 起 + 指数升级），一次偶发 404 会把好账号罚 10 分钟并逐次加倍。
// 故固定 60s 防雪崩即可，不随 soft_rate 配置、也不参与软退避指数。
const notFoundCooldown = 60 * time.Second

// ServiceName 网关身份标识。经 /healthz 响应体 service 字段与 X-Service 头同时透出：
// 宿主（如 workbuddy-switch 托管网关子进程）探测同端口的旧服务/其他服务时，对方即使
// 返回 2xx 也不带本标识，宿主据此可识别"假成功"。
const ServiceName = "workbuddy2api"

// Handler 主路由。
type Handler struct {
	cfg Config
	mux *http.ServeMux
	// keyMu 保护 keys：管理面板能在运行期换密钥（保存后热重载），而每个请求都要读它。
	keyMu sync.RWMutex
	keys  []APIKeySpec // 合并后的生效密钥（旧 APIKey → 不限区域项）；空 = 不鉴权
	// degrade 内容拦截误报的降级闸门（00:00 CST 每日重置，见 degrade.go）。
	degrade degradeGate
	// loopGuard 思考死循环判据；nil = 不检测（配置关闭时）。
	loopGuard *LoopGuard
	// loopLog 近期空转命中的环形缓冲，经 /status 暴露（免去翻日志）。
	loopLog *loopLog
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 600 * time.Second // 软限流基数（连续触发按指数退避放大）
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	if cfg.PromptMode == "" {
		cfg.PromptMode = "custom" // 缺省 custom：网关自有提示词
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = defaultMaxBodyBytes
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux(), loopLog: &loopLog{}}
	h.loopGuard = cfg.LoopGuard.toLoopGuard()
	// 生效判据落一行启动日志：这几项只能从配置推导，出问题时（误杀/漏判）第一件
	// 事就是确认实际生效的阈值，没有这行只能去翻配置文件猜。
	if g := h.loopGuard; g != nil {
		log.Printf("[server] 思考循环判据：停滞窗口=%d 块(%dKB) 占比<%.2f 字符下限=%d 重试=%d",
			g.MaxStaleChunks, g.MaxStaleChunks*loopChunkLen>>10, g.MaxDistinctRatio, g.MinThinkChars, g.maxRetries())
	} else {
		log.Printf("[server] 思考循环判据：关闭")
	}
	h.SetKeys(cfg.APIKey, cfg.APIKeys)
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	// 协议适配：同一账号池同时服务 Anthropic（Claude Code）与 Responses（Codex）客户端。
	h.mux.HandleFunc("POST /v1/messages", h.withAuth(h.anthropicMessages))
	h.mux.HandleFunc("POST /v1/messages/count_tokens", h.withAuth(h.countTokens))
	h.mux.HandleFunc("POST /v1/responses", h.withAuth(h.responses))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	if cfg.Admin != nil {
		h.mux.Handle("/__admin/", cfg.Admin)
	}
	// "/" 是兜底模式：只有比它更具体的、上面那些 API 路径都未命中时才会走到，
	// 因此面板不会抢走任何 API 路由。
	if cfg.Web != nil {
		h.mux.Handle("/", cfg.Web)
	}
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 面板统一走 /api 前缀：dev 下它是 vite 反代规则的锚点，单二进制下由这里剥掉。
	// 两种部署共用同一份前端产物，不必区分。
	// 剥离后重新派发（而非 302 重定向）：重定向对 POST 会丢方法语义。
	if p, ok := strings.CutPrefix(r.URL.Path, "/api/"); ok {
		r = r.Clone(r.Context())
		r.URL.Path = "/" + p
	}
	h.mux.ServeHTTP(w, r)
}

// authedHandler 需要知道「哪个密钥发起了请求」的处理器。
// key 为 nil 表示未鉴权（未配置任何密钥）。
type authedHandler func(w http.ResponseWriter, r *http.Request, key *APIKeySpec)

// SetKeys 换用新的密钥表（密钥管理面板保存后调用）。立即生效，无需重启进程。
func (h *Handler) SetKeys(legacy string, keys []APIKeySpec) {
	h.keyMu.Lock()
	defer h.keyMu.Unlock()
	var merged []APIKeySpec
	// 旧 api_key 视为「不限区域」的一项，与 api_keys 列表合并。
	if legacy != "" {
		merged = append(merged, APIKeySpec{Key: legacy, Name: "legacy"})
	}
	h.keys = append(merged, keys...)
}

// withAuth 校验 Bearer 密钥并把命中的 spec 传给处理器。
// 多密钥（每把可绑区域）逐把比对；比较用 ConstantTimeCompare（发现 7）：
// == 的短路时序随前缀长度变化，公网暴露下理论上可逐字节探测 key 前缀。
// 注意这里只对「长度相等」的候选走常量时间比较 —— 长度本身会泄漏，
// 但不泄漏内容，且 length 无法在不填充的前提下隐藏。
func (h *Handler) withAuth(next authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.keyMu.RLock()
		keys := h.keys
		h.keyMu.RUnlock()
		if len(keys) == 0 {
			next(w, r, nil) // 未配置密钥 = 不鉴权、不限区域
			return
		}
		authz := r.Header.Get("Authorization")
		if !strings.HasPrefix(authz, "Bearer ") {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		token := strings.TrimPrefix(authz, "Bearer ")
		for i := range keys {
			if subtle.ConstantTimeCompare([]byte(keys[i].Key), []byte(token)) == 1 {
				// 传指针而非副本：调用方需要读到 Region/Name。keys 是本地切片头，
				// 但底层数组与 h.keys 共享 —— 热重载换掉 h.keys 后旧数组依然存活，
				// 本次请求手里的指针始终有效。
				next(w, r, &keys[i])
				return
			}
		}
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	total, healthy, _, _, _ := h.cfg.Pool.CountsDetailed()
	// 用 ServableNow 判定：healthy>0 但全占满在途时 chat 会 503，探活必须同口径，
	// 否则负载均衡器会把流量持续打进无法受理的实例。
	status := http.StatusOK
	if !h.cfg.Pool.ServableNow() {
		status = http.StatusServiceUnavailable
	}
	// 恒无鉴权（负载均衡/编排探活只需 2xx/503 语义），身份靠 service 字段 + X-Service 头双保险。
	w.Header().Set("X-Service", ServiceName)
	writeJSON(w, status, map[string]any{
		"healthy": healthy,
		"total":   total,
		"service": ServiceName,
	})
}

// status 返回账号池观测信息。绑定区域的密钥只看到该区域的账号与计数；
// 未绑定区域的密钥看到全部（改造前行为）。
// sticky_sessions 例外：该计数来自 router 的全局记账，不随区域过滤。
//
// thinking_loop_total / thinking_loops 同样是全局的（跨区域、重启清零）：
// 空转是模型侧现象而非账号侧（实测 7 个不同账号都中过、全部同一模型），
// 按密钥区域切开只会让形态更难看清。要看跨重启的长期趋势请查 gateway.log。
func (h *Handler) status(w http.ResponseWriter, r *http.Request, key *APIKeySpec) {
	pred := keyRegionPred(key)
	total, healthy, cooling, disabled, inFlightFull := h.cfg.Pool.CountsDetailedWhere(pred)
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}
	redisMode := h.cfg.RedisMode
	if redisMode == "" {
		redisMode = "noop"
	}
	loopTotal, loopHits := h.loopLog.snapshot()
	if loopHits == nil {
		// nil 切片会被编码成 null，面板要额外处理；空数组更省事。
		loopHits = []LoopHit{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":        h.cfg.Pool.ListWhere(pred),
		"total":           total,
		"healthy":         healthy,
		"cooling":         cooling,
		"disabled":        disabled,
		"in_flight_full":  inFlightFull,
		"sticky_sessions": sticky,
		"redis_mode":      redisMode,

		// 思考空转观测：近期命中次数与各自的上下文大小，
		// 用于判断是否集中在长上下文（见 LoopHit 注释）。
		"thinking_loop_total": loopTotal,
		"thinking_loops":      loopHits,
	})
}

// keyRegionPred 把密钥绑定的区域编译成账号谓词；未绑定区域返回 nil（=不过滤）。
// 复用 regionAllowed，使「密钥区域」与「模型区域」共用同一套判定语义。
func keyRegionPred(key *APIKeySpec) func(*auth.Auth) bool {
	if key == nil || key.Region == "" {
		return nil
	}
	return regionAllowed([]auth.Region{key.Region})
}

// 静态 CN 模型表（api-reference §5，动态接口失败时的回退）。
// context_length 取自 2026-09-13 直连上游 /console/enterprises/personal/models 的
// maxInputTokens 实测值；此前统一硬编码 131072，会让客户端误以为只有 128K 而提前截断。
// hy3-preview / hy3-preview-agent 在上游实测表中无对应条目，保留 131072 兜底。
var staticModelsCN = []map[string]any{
	{"id": "glm-5.2", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 1000000},
	{"id": "glm-5.1", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 200000},
	{"id": "glm-5v-turbo", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 200000},
	{"id": "kimi-k2.7", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 256000},
	{"id": "minimax-m3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 512000},
	{"id": "hy3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 192000},
	{"id": "hy3-preview", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "hy3-preview-agent", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "deepseek-v4-pro", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 1000000},
	{"id": "deepseek-v4-flash", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 1000000},
}

// 静态 global（workbuddy.ai）模型表。
// 该表必须存在且准确：intl 的动态模型接口 /console/enterprises/personal/models
// 实测恒 500（重试 3/3，2026-09-13 复测仍 500），因此 intl 必然回退到本表。
// 模型名单为 2026-09 用真实 intl 凭证逐个模型实测「可用」的结果；
// context_length 按同 id 取 CN 上游实测的 maxInputTokens —— 两区是同一套 API 的两次部署，
// 但 global 侧无法实测验证，属外推值，可能偏高。
// 注意 global 不含 deepseek-v4-flash / deepseek-v4-pro（调用报 code=11102 service info not found）。
var staticModelsGlobal = []map[string]any{
	{"id": "glm-5.3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 1000000},
	{"id": "glm-5.2", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 1000000},
	{"id": "glm-5.1", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 200000},
	{"id": "glm-5v-turbo", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 200000},
	{"id": "kimi-k2.7", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 256000},
	{"id": "kimi-k2.6", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 256000},
	{"id": "kimi-k2.5", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 164000},
	{"id": "minimax-m3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 512000},
	{"id": "hy3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 192000},
	{"id": "hy4-preview", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 1000000},
	{"id": "hy4-preview-x", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 1000000},
	{"id": "deepseek-v4.1-flash", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 1000000},
	{"id": "auto", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 168000},
}

// modelRegions 模型 → 支持它的区域集合（区域路由表）。
// 来源：2026-09 实测。CN 取自动态接口的 cli agent 列表；global 为逐个模型实调结果。
// 存在的意义：两区模型阵容不同，把请求发给不支持的域名会拿到 401 / code=11102。
var modelRegions = func() map[string]map[auth.Region]bool {
	m := map[string]map[auth.Region]bool{}
	add := func(r auth.Region, ids ...string) {
		for _, id := range ids {
			if m[id] == nil {
				m[id] = map[auth.Region]bool{}
			}
			m[id][r] = true
		}
	}
	add(auth.RegionCN,
		"auto", "default",
		"deepseek-v3-2-volc", "deepseek-v4-flash", "deepseek-v4-pro", "deepseek-v4.1-flash",
		"glm-4.6", "glm-4.6v", "glm-4.7", "glm-5.0", "glm-5.1", "glm-5.2", "glm-5.3", "glm-5.3-flash", "glm-5v-turbo",
		"hunyuan-2.0-thinking", "hunyuan-chat", "hunyuan-image-v3.0",
		"hy3", "hy3-x", "hy4-preview", "hy4-preview-x",
		"kimi-k2-thinking", "kimi-k2.5", "kimi-k2.6", "kimi-k2.7", "kimi-k3-1",
		"minimax-m2.5", "minimax-m3",
	)
	add(auth.RegionGlobal,
		"auto", "deepseek-v4.1-flash",
		"glm-5.1", "glm-5.2", "glm-5.3", "glm-5v-turbo",
		"hy3", "hy4-preview", "hy4-preview-x",
		"kimi-k2.5", "kimi-k2.6", "kimi-k2.7",
		"minimax-m3",
	)
	return m
}()

// regionsForModel 返回支持该模型的区域；未知模型返回 nil，语义为「不作限制」
// （向前兼容上游新增模型：宁可让它按普通轮换试一次，也不要因表未收录而直接拒绝）。
func regionsForModel(model string) []auth.Region {
	if model == "" {
		return nil
	}
	set := modelRegions[model]
	if len(set) == 0 {
		return nil
	}
	out := make([]auth.Region, 0, len(set))
	for r := range set {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// constrainRegions 把「模型允许的区域」与「密钥绑定的区域」取交集。
// 返回的第二个值为 false 表示无交集——该模型对此密钥不存在，调用方应报 404。
//
// 四种组合：
//   - 密钥不限区域 → 原样返回模型区域（nil 仍为 nil，即不限制）
//   - 密钥绑定 + 模型不限区域（nil）→ 收敛为密钥区域
//   - 密钥绑定 + 模型有区域 → 求交集
//   - 交集为空 → (nil, false)
func constrainRegions(modelRegions []auth.Region, key *APIKeySpec) ([]auth.Region, bool) {
	if key == nil || key.Region == "" {
		return modelRegions, true
	}
	if len(modelRegions) == 0 {
		return []auth.Region{key.Region}, true
	}
	out := make([]auth.Region, 0, len(modelRegions))
	for _, r := range modelRegions {
		if r == key.Region {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// regionAllowed 把允许区域编译成选号谓词；allowed 为空（模型未知）时返回 nil = 不过滤。
func regionAllowed(allowed []auth.Region) func(*auth.Auth) bool {
	if len(allowed) == 0 {
		return nil
	}
	set := make(map[auth.Region]bool, len(allowed))
	for _, r := range allowed {
		set[r] = true
	}
	return func(a *auth.Auth) bool { return set[a.Region()] }
}

// modelsSlot 单区域的动态模型缓存。
type modelsSlot struct {
	ids      []upstream.ModelInfo
	fetched  time.Time // 最近一次成功拉取时间
	lastFail time.Time // 最近一次拉取失败时间（负缓存）
}

// modelsCache 动态模型缓存，按区域分槽。
// 分区域的原因：两区模型阵容不同，且 global 的动态接口恒 500。
// 共用一份会让先成功的一区覆盖另一区，也会让 global 的失败把 CN 一起拖进负缓存。
type modelsCache struct {
	mu     sync.RWMutex
	cn     modelsSlot
	global modelsSlot
}

func (m *modelsCache) slot(r auth.Region) *modelsSlot {
	if r == auth.RegionGlobal {
		return &m.global
	}
	return &m.cn
}

// dynamicModelsCache 动态模型缓存（进程级）。
var dynamicModelsCache = &modelsCache{}

const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
)

// models 返回模型列表：优先动态（缓存 1h），失败回退静态表。
func (h *Handler) models(w http.ResponseWriter, r *http.Request, key *APIKeySpec) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.modelList(key),
	})
}

// modelList 返回该密钥可见的模型列表（按 id 去重，CN 在前）。
// 绑定区域的密钥只看到该区域的模型——与其请求被路由到的账号集合一致，
// 因此列表里出现的模型一定可用，反之请求不在列表里的模型会得到 404。
// 未绑定区域的密钥看到池中实际存在区域的并集：只挂 CN 账号时不列出仅 global
// 可用的模型，故单区域部署的列表与改造前一致。
func (h *Handler) modelList(key *APIKeySpec) []map[string]any {
	regions := h.cfg.Pool.Regions()
	if key != nil && key.Region != "" {
		regions = []auth.Region{key.Region}
	}
	if len(regions) == 0 {
		regions = []auth.Region{auth.RegionCN} // 空池：给出默认一区，避免返回空列表
	}
	seen := make(map[string]bool)
	out := make([]map[string]any, 0, len(regions)*8)
	for _, r := range regions {
		for _, m := range h.regionModels(r) {
			id, _ := m["id"].(string)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, m)
		}
	}
	return out
}

// ModelsForRegion 返回该区域可见的模型 id 列表。
// 供管理面板「测试连接」的模型下拉框用：面板因此不必自己抄一份模型表，
// 也不必反过来查本进程的 /v1/models。
func (h *Handler) ModelsForRegion(r auth.Region) []string {
	out := []string{}
	for _, m := range h.regionModels(r) {
		if id, ok := m["id"].(string); ok && id != "" {
			out = append(out, id)
		}
	}
	return out
}

// regionModels 单个区域的模型列表：动态接口优先，失败回落该区域的静态表。
func (h *Handler) regionModels(r auth.Region) []map[string]any {
	if infos := h.fetchDynamicModels(r); len(infos) > 0 {
		out := make([]map[string]any, 0, len(infos))
		for _, mi := range infos {
			entry := map[string]any{
				"id":                mi.ID,
				"object":            "model",
				"created":           1753600000,
				"owned_by":          "workbuddy",
				"context_length":    mi.ContextWindow,
				"max_output_tokens": mi.MaxTokens,
			}
			if mi.ContextWindow == 0 {
				entry["context_length"] = 131072 // 兜底
			}
			out = append(out, entry)
		}
		return out
	}
	if r == auth.RegionGlobal {
		return staticModelsGlobal
	}
	return staticModelsCN
}

// fetchDynamicModels 从池中该区域的一个健康账号拉模型列表（含 contextWindow/maxTokens），缓存 1h。
// 拉取失败记录时间戳进入 5min 负缓存，冷却期内直接用静态表，避免反复打上游。
// 缓存与负缓存均按区域独立：global 的动态接口恒 500，不得因此让 CN 也走静态表。
func (h *Handler) fetchDynamicModels(r auth.Region) []upstream.ModelInfo {
	dynamicModelsCache.mu.Lock()
	slot := dynamicModelsCache.slot(r)
	if len(slot.ids) > 0 && time.Since(slot.fetched) < dynamicModelsTTL {
		out := slot.ids
		dynamicModelsCache.mu.Unlock()
		return out
	}
	// 失败负缓存：冷却期内不再请求上游。
	if !slot.lastFail.IsZero() && time.Since(slot.lastFail) < modelsFetchFailCooldown {
		dynamicModelsCache.mu.Unlock()
		return nil
	}
	dynamicModelsCache.mu.Unlock()

	acct := h.cfg.Pool.PickExcludingWhere(nil, regionAllowed([]auth.Region{r}))
	if acct == nil {
		return nil
	}
	infos, err := h.cfg.Upstream.FetchModels(acct)
	if err != nil || len(infos) == 0 {
		// 拉取失败只进负缓存（5min lastFail），不 NoteError（P1-6/发现 6）：
		// NoteError 喂的是 chat 熔断器，models 端点偶发 5xx 跨界惩罚 chat 通道
		// 健康的账号；models 拉取失败 ≠ 账号 chat 不可用。
		// 负缓存按区域分槽（本地改造）：两区的失败互不影响对方的 5min 退避。
		dynamicModelsCache.mu.Lock()
		dynamicModelsCache.slot(r).lastFail = time.Now()
		dynamicModelsCache.mu.Unlock()
		return nil
	}
	dynamicModelsCache.mu.Lock()
	slot = dynamicModelsCache.slot(r)
	slot.ids = infos
	slot.fetched = time.Now()
	slot.lastFail = time.Time{} // 成功则清空负缓存
	dynamicModelsCache.mu.Unlock()
	return infos
}

// forwardOpt 转发所需的请求级上下文，由各协议的 handler 组装后交给 forwardChat。
type forwardOpt struct {
	// pickPred 账号谓词（模型区域 ∩ 密钥区域）；nil = 不过滤。
	pickPred func(*auth.Auth) bool
	// sessKey 会话键（取自客户端原始请求体，未加区域前缀）；空 = 不走粘性。
	sessKey string
	// key 本次请求命中的密钥；用于给会话键加区域前缀（nil = 未鉴权）。
	key *APIKeySpec
	// st 请求级统计；成功与失败都会写入 uid/status，由调用方负责收尾输出。
	st *chatStat
	// allowedRegions 允许的区域，仅用于失败文案。
	allowedRegions []auth.Region
	// model 客户端请求的模型名，仅用于失败文案。
	model string
	// clientIP 客户端 IP（仅在 PassthroughIP 开启时由调用方填充，否则留空）。
	// 按请求经本字段传入而非在 forwardChat 里读 *http.Request：三协议入口的
	// request 各不相同，且这样可让 forwardChat 不依赖 HTTP 细节。
	clientIP string
	// writeErr 失败时的错误响应写出函数；nil = OpenAI 格式（默认）。
	// 各协议的客户端只认自己协议的错误体（Claude Code 读 Anthropic 的
	// {"type":"error",...}），故允许调用方覆盖。
	writeErr func(w http.ResponseWriter, status int, code, msg string)
	// stream 客户端是否要流式。流式的转发在 forwardChat 内完成（思考死循环需要
	// 「读了若干帧后」才判得出，交给调用方转发就没有重试机会了）。
	stream bool
	// relay 流式转发函数；nil = OpenAI SSE 透传（upstream.Stream）。
	// Anthropic / Responses 两种协议要把 chat completions 的 SSE 转成自己的事件流，
	// 故由调用方注入；返回 ErrThinkingLoop 时 forwardChat 会追加提示并换号重试。
	relay func(w http.ResponseWriter, r io.Reader) error
}

// dumpReqPath WB2A_DUMP_REQ 调试开关的落盘路径。
// 容器内 /app/data 是数据卷；本地直跑时该目录通常不存在，写失败只记一条日志、不影响请求。
const dumpReqPath = "/app/data/last_request.json"

// clientIPFor 在开启 IP 透传时提取客户端 IP，否则返回空串（不注入任何 IP 头）。
// 三个协议入口共用：取值只依赖 *http.Request，与协议无关。
func clientIPFor(up *upstream.Client, r *http.Request) string {
	if up == nil || !up.PassthroughIP {
		return ""
	}
	return upstream.ExtractClientIP(r)
}

// sessionKey 提取会话键；未启用粘性时返回空，省去一次无谓的 JSON 解析。
func (h *Handler) sessionKey(body []byte) string {
	if h.cfg.Session == nil {
		return ""
	}
	return session.ExtractKey(body)
}

// forwardChat 执行「选号 → 轮换 → 调上游」，成功返回上游响应流（调用方负责 Close）。
// 全部尝试失败时返回 nil —— 此时已按错误策略处置过账号，且 HTTP 错误响应已写好。
//
// 三种协议（OpenAI chat completions / Anthropic messages / OpenAI responses）共用本函数：
// 它们的差异只在请求体构造与响应渲染，而选号、在途租约、token 刷新、错误分类处置、
// 粘性重绑这些语义必须完全一致，故集中于此，避免三处各写一份而产生行为漂移。
func (h *Handler) forwardChat(w http.ResponseWriter, body []byte, o forwardOpt) io.ReadCloser {
	// 会话粘性：解析绑定号（找不到/无效则 stickyUID 为空，走普通轮换）。
	sessKey := ""
	stickyUID := ""
	if h.cfg.Session != nil {
		sessKey = o.sessKey
		// 区域密钥给会话键加区域前缀，使两区的粘性绑定互不干扰：
		// 同一 conversationId 分别用 cn / global 密钥时不应争抢同一个绑定。
		// 未绑定区域的密钥不加前缀，行为与改造前一致。
		if sessKey != "" && o.key != nil && o.key.Region != "" {
			sessKey = string(o.key.Region) + "|" + sessKey
		}
		if sessKey != "" {
			// 用 peek.Model（缺省为空串）而非 st.model（缺省为 "-"）：
			// 模型名参与成本账本与选号过滤，"-" 会污染成不存在的模型键。
			if uid, ok := h.cfg.Session.ResolveForModel(sessKey, o.model); ok {
				stickyUID = uid
			}
		}
	}

	st := o.st
	tried := map[string]bool{}
	var lastErr error
	// loopRetries 本轮请求因思考死循环而重试的次数（不计入 MaxRotate 的账号轮换预算：
	// 它换的是「同一个请求的再次尝试」，不是「换一个账号」）。
	loopRetries := 0
	// maxRetries / maxStale 思考循环的重试上限与停滞窗口（判据未装配时用默认）。
	maxRetries := maxLoopRetries
	maxStale := DefaultLoopGuard().MaxStaleChunks
	if h.loopGuard != nil {
		maxRetries = h.loopGuard.maxRetries()
		maxStale = h.loopGuard.MaxStaleChunks
	}
	// origBody 客户端原始请求体（后续的提示词改写与循环提示都返回新切片，不改这一份），
	// 仅在命中思考循环时用于诊断「上下文是否接近占满」。开销放在命中路径上 ——
	// estimateTokens 是 O(len) 的逐字符扫描，不能每条请求都付。
	origBody := body

	// 在途租约：成功选中即占名额；函数出口（含成功 return 与 panic）统一释放。
	var heldUID string
	defer func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	releaseHeld := func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
			heldUID = ""
		}
	}
	// unbindSticky 解绑当前会话粘性号（stickyUID 非空时）。供「粘性号不可用/被抢」与 fail 共用。
	// 幂等：stickyUID 已空则空操作；不会误解绑其他轮的绑定。仅当 Session != nil 时 stickyUID 才会非空。
	unbindSticky := func() {
		if stickyUID != "" {
			h.cfg.Session.Unbind(sessKey)
			stickyUID = ""
		}
	}
	// fail 在轮转失败分支统一：释放租约 + 若失败号正是粘性号则解绑（下次请求重新分配）。
	fail := func(uid string) {
		releaseHeld()
		if stickyUID != "" && uid == stickyUID {
			unbindSticky()
		}
	}

	// 系统提示词改写（出站前、轮转前；每个请求一次）。
	//   - custom：用自有提示词替换客户端 system/developer（从源头消灭 system 指纹误报）。
	//   - passthrough + 降级期：换 Degraded 中性提示词直达，不再先撞 400。
	//   - passthrough 非降级期：透传客户端原始 system（不改写）。
	degradedApplied := false
	if h.cfg.PromptMode == "custom" && h.cfg.PromptText != "" {
		body = prompt.Rewrite(body, h.cfg.PromptText)
	} else if h.cfg.PromptMode == "passthrough" && h.degrade.Active() {
		body = prompt.Rewrite(body, prompt.Degraded)
		degradedApplied = true
	}

	for i := 0; i < h.cfg.MaxRotate; i++ {
		// 选号：粘性号优先，否则普通轮换。
		// 两个约束维度同时生效：o.model 启用 6004 模型级冷却豁免（该模型被限额的号
		// 在其他模型上仍可用），o.pickPred 限定本次允许的账号集合（区域路由）。
		var acct *auth.Auth
		if stickyUID != "" {
			acct = h.cfg.Pool.PickByUIDForModelWhere(stickyUID, o.model, o.pickPred)
			if acct == nil {
				// 粘性号在当前模型/区域不可用（冷却/占满/6004 限额/区域不符）→ 解绑，回落普通轮换。
				unbindSticky()
			}
		}
		if acct == nil {
			acct = h.cfg.Pool.PickExcludingForModelWhere(tried, o.model, o.pickPred)
		}
		if acct == nil {
			st.status = http.StatusServiceUnavailable
			break
		}
		st.uid = acct.UID
		tried[acct.UID] = true

		// 占用在途名额：Pick 已跳过满额账号，此处 CAS 兜底并发抢名额的竞态。
		if !h.cfg.Pool.Acquire(acct.UID) {
			// 若被抢的正是粘性号，立即解绑并回落普通轮换，避免下一轮仍撞同一个
			// 满载粘性号再浪费一次粘性命中往返（语义与 fail()/粘性命中-nil 的解绑一致）。
			if stickyUID != "" && acct.UID == stickyUID {
				unbindSticky()
			}
			continue // 最后一个名额被并发抢走 → 换号
		}
		heldUID = acct.UID

		// token 临近过期 → 先 refresh（失败冷却换号）
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					h.cfg.Pool.Disable(acct.UID, "refresh session dead")
				} else {
					h.cfg.Pool.NoteError(acct.UID)
				}
				fail(acct.UID)
				continue
			}
			if err := acct.SaveAtomic(); err != nil {
				// 刷新成功但落盘失败：下次启动会用旧 token，必须暴露
				log.Printf("ERR: [server] chat refresh uid=%s: save auth failed: %v", logfmt.UID8(acct.UID), err)
			}
		}

		// 客户端 IP 透传（仅 PassthroughIP 开启）：由调用方按请求取首段放进 o.clientIP，
		// 不读写共享字段——并发请求各自携带独立 IP，互不串扰（issue：ClientIP 竞态）。
		rc, status, respBody, terr := h.cfg.Upstream.ChatStream(acct, body, o.clientIP)
		if terr != nil {
			// 网络层抖动：只换号，不喂熔断计数（传输层错误对连续失败连坐熔断过于严苛）。
			// 上游 client 已打 transport error 日志。
			st.status = http.StatusServiceUnavailable
			lastErr = terr
			fail(acct.UID)
			continue
		}
		if status >= 400 {
			st.status = status
			kind := upstream.Classify(status, string(respBody))
			// 上下文超限：立即回 400，**不轮换**。
			//
			// 两个理由，缺一都会造成客户端行为错误：
			//  1. 换号毫无意义 —— 窗口是模型属性不是账号属性，每个账号都会返回
			//     一字不差的 400。轮换只是让客户端白等 MaxRotate 倍的往返时间
			//     （实测 3 × 9s ≈ 28s）后才拿到错误。
			//  2. 状态码必须是 4xx。此前它走通用路径被转成 503，Anthropic 侧又映射为
			//     "overloaded_error" —— 客户端读作「服务过载，稍后重试」，于是原样重发
			//     同一份超长上下文，自动压缩永不触发。压缩的触发信号是 400。
			//
			// 消息透传上游原话（"prompt is too long: N tokens > M maximum"）：
			// 这是 Anthropic 官方同款文案，客户端按它识别「该压缩了」。
			if kind == upstream.ErrContextExceeded {
				writeErrFn := o.writeErr
				if writeErrFn == nil {
					writeErrFn = writeOpenAIError
				}
				msg := upstream.ContextExceededMessage(string(respBody))
				log.Printf("WARN: [server] context exceeded uid=%s model=%s: %s",
					logfmt.UID8(acct.UID), o.model, msg)
				writeErrFn(w, http.StatusBadRequest, "context_length_exceeded", msg)
				st.status = http.StatusBadRequest
				releaseHeld()
				return nil
			}
			// 内容拦截误报（passthrough 模式首遇）：判定为 system 指纹误报，
			// 触发降级到次日 00:00 CST，换 Degraded 中性提示词同请求内重试。
			// 第二次仍被拦（用户内容本身触发审核）→ 走既有错误路径返回客户端。
			// 内容问题非账号问题：applyErrorPolicy 不罚账号（见 ErrContentBlocked 分支）。
			if kind == upstream.ErrContentBlocked && h.cfg.PromptMode == "passthrough" && !degradedApplied {
				h.degrade.Trigger()
				body = prompt.Rewrite(body, prompt.Degraded)
				degradedApplied = true
				delete(tried, acct.UID) // 单账号池也能拿到重试机会（降级重试占一次名额）
				releaseHeld()
				log.Printf("WARN: [server] content-blocked (likely fingerprint false positive) -> degraded prompt retry")
				continue
			}
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
			h.applyErrorPolicy(acct.UID, kind, string(respBody), o.model)
			fail(acct.UID)
			continue
		}
		h.cfg.Pool.NoteSuccess(acct.UID)

		// 流式：转发必须在轮换循环内完成 —— 思考死循环只能靠「读了若干帧之后」
		// 才判得出来，若交给调用方转发，这里就没有重试的机会了。
		if o.stream {
			hdr := w.Header()
			hdr.Set("Content-Type", "text/event-stream")
			hdr.Set("Cache-Control", "no-cache")
			hdr.Set("Connection", "keep-alive")
			hdr.Set("X-Accel-Buffering", "no")
			// 与既有语义一致：开始写流即视为 200（响应头一旦发出便无法改状态）。
			st.status = http.StatusOK

			relay := o.relay
			if relay == nil {
				relay = upstream.Stream
			}
			stats := newChatStatsReaderSince(rc, st.start)
			stats.SetLoopGuard(h.loopGuard)
			serr := relay(w, stats)
			st.ttfb = stats.TTFB()
			st.toks, _ = stats.Tokens()
			if credit, ok := stats.Credit(); ok {
				h.cfg.Pool.NoteModelCost(acct.UID, o.model, credit, stats.TotalTokens())
			}
			rc.Close()

			if errors.Is(serr, ErrThinkingLoop) {
				// 命中思考空转：切断上游流并换号重试，同时在请求体追加一条提示，
				// 要求模型停止重复、直接给结论。
				//
				// 注意 Stream 命中时提前返回、不写 [DONE] —— 若写了，客户端会认为
				// 本轮已结束并停止读取，重试的内容就送不到了。
				//
				// 已知代价：流式下思考内容已实时发给客户端，重试会让客户端看到两段
				// 思考拼接。这是刻意取舍 —— 拼接的观感问题远小于挂满 15 分钟。
				body = InjectLoopNudge(body)
				loopRetries++
				lastErr = ErrThinkingLoop
				fail(acct.UID)
				thinkChars, _, distinct, total, stale := stats.LoopSignal()
				ratio := 0.0
				if total > 0 {
					ratio = float64(distinct) / float64(total)
				}
				// req_kb / req_est_tokens 用于验证「上下文满了容易触发空转」的假设：
				// 与 /v1/models 里该模型的 context_length 对照即可判断占满程度。
				// 估算一次即可 —— estimateTokens 是 O(len) 逐字符扫描，日志与
				// /status 观测共用同一个值，不要各算一遍。
				estTokens := estimateTokens(string(origBody))
				log.Printf("WARN: [server] thinking loop uid=%s model=%s think_chars=%d ratio=%.3f stale=%d/%d retry=%d/%d req_kb=%d req_est_tokens=%d",
					logfmt.UID8(acct.UID), o.model, thinkChars, ratio, stale, maxStale, loopRetries, maxRetries,
					len(origBody)>>10, estTokens)
				// 同时进环形缓冲，供 /status 查看：
				// 「近期空转次数 + 各自上下文大小」一眼可见，不用翻日志。
				h.loopLog.record(LoopHit{
					At:           time.Now(),
					UID:          acct.UID,
					Model:        o.model,
					ThinkChars:   thinkChars,
					Ratio:        ratio,
					StaleChunks:  stale,
					ReqEstTokens: estTokens,
					Retry:        loopRetries,
				})
				if loopRetries > maxRetries {
					// 重试预算用尽：跳出轮换循环，由出口统一回错误。
					// 出口判 lastErr 决定文案 —— 预算可能因「账号池小」先被
					// MaxRotate 耗尽，那样也必须是 thinking_loop 而非「无可用账号」。
					break
				}
				continue
			}
			if serr != nil {
				// 其余流错误（客户端断连、上游中断）：无法重试（响应已开始），如实记日志。
				log.Printf("WARN: [server] stream uid=%s: %v", logfmt.UID8(acct.UID), serr)
			}
			// 粘性绑定与租约：流已写完，绑定成功号（多轮对话下一跳不再随机抽）。
			if sessKey != "" && h.cfg.Session != nil {
				h.cfg.Session.Bind(sessKey, acct.UID)
			}
			return nil
		}

		// 粘性跟随最终成功号：本轮成功的账号成为该会话的粘性绑定（覆盖旧绑定）。
		// 若 sticky 号失败、轮换到别的号成功，这里把会话重绑到新号，多轮对话下一跳不再随机抽。
		if sessKey != "" && h.cfg.Session != nil {
			h.cfg.Session.Bind(sessKey, acct.UID)
		}
		// 租约不在此释放：由调用方在读尽响应流后统一归还（defer）。
		// 成本账本随流式/非流式在此收尾——见下方调用方（读尽 stats 后才有 usage）。
		return rc
	}

	writeErr := o.writeErr
	if writeErr == nil {
		writeErr = writeOpenAIError
	}

	// 思考循环用尽重试预算：必须优先于下面的通用文案。
	// 轮换预算（MaxRotate）可能先于重试预算耗尽（账号池小），若不在此分流，
	// 客户端拿到的会是「无可用账号」—— 把人引去查账号池，而真正的故障在模型侧空转。
	if errors.Is(lastErr, ErrThinkingLoop) {
		writeErr(w, http.StatusServiceUnavailable, "thinking_loop",
			"model is stuck in a thinking loop (repeated reasoning with no output); "+
				"retried "+strconv.Itoa(maxRetries)+" times without success")
		st.status = http.StatusServiceUnavailable
		return nil
	}

	// 模型不存在：轮换过了（撞区域，见 ErrModelNotFound 注释）仍没人能服务它，
	// 回 404 而不是 503。503 的语义是「网关没有可用账号」，会把排查方向引向账号池；
	// 而真相是模型名不对 / 该模型上游已下线。404 也是 OpenAI 对未知模型的标准语义
	// （网关在模型已知时已按此回 404，这里是模型未知、只能靠上游告知的分支）。
	if ue := (*upstream.Error)(nil); errors.As(lastErr, &ue) && ue.Kind == upstream.ErrModelNotFound {
		msg := upstream.ModelNotFoundMessage(ue.Msg)
		log.Printf("WARN: [server] model not found model=%s: %s", o.model, msg)
		writeErr(w, http.StatusNotFound, "model_not_found", msg)
		st.status = http.StatusNotFound
		return nil
	}

	// 轮换耗尽有两种截然不同的原因，必须分开表述，否则会把排查方向带偏：
	//   - lastErr == nil：真的没有可尝试的账号（区域无账号 / 全冷却 / 全禁用）。
	//   - lastErr != nil：账号是好的，但每次尝试都被上游拒绝（如 400 消息格式错误）。
	//     此时若仍断言「账号不可用」，会让调用方去查账号，而真正的问题在请求体。
	regionNote := ""
	if len(o.allowedRegions) > 0 {
		regions := make([]string, 0, len(o.allowedRegions))
		for _, rr := range o.allowedRegions {
			regions = append(regions, string(rr))
		}
		regionNote = " in region " + strings.Join(regions, "/")
	}
	msg := "no attempt succeeded" + regionNote + " for model " + o.model
	if lastErr == nil {
		msg = "no available account" + regionNote + " for model " + o.model +
			" (账号不可用：不存在 / 冷却中 / 已禁用)"
	} else {
		msg += " (账号可用，但上游拒绝了每次尝试): " + lastErr.Error()
	}
	writeErr(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
	st.status = http.StatusServiceUnavailable
	return nil
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request, key *APIKeySpec) {
	body, ok := h.readBodyOrFail(w, r, false)
	if !ok {
		return
	}
	// 调试开关：设置 WB2A_DUMP_REQ 即把上游侧收到的原始请求体落盘，供离线二分定位指纹命中行。
	// 仅在排查上游指纹拦截时开启；不设置时零开销、不落盘。
	// 只落"大请求"（超过上限一半）：小探针（{"input":"hi"} 之类）会覆盖掉真正要看的对话请求。
	if os.Getenv("WB2A_DUMP_REQ") != "" && len(body)*2 >= int(h.cfg.MaxBodyBytes) {
		if err := os.WriteFile(dumpReqPath, body, 0o600); err != nil {
			log.Printf("ERR: [server] dump req: %v", err)
		}
	}
	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)

	// 请求级统计：出口即打一行表格日志（任何路径都会走到）。
	st := newChatStat(time.Now(), body, peek.Stream)
	defer st.done()

	// 区域路由：按模型确定支持它的区域，本次请求只在该区域的账号中轮换。
	// 两区模型阵容不同，把请求发给不支持的域名会拿到 401 / code=11102。
	// 模型未收录时 allowedRegions 为 nil → 不作限制（向前兼容上游新增模型）。
	allowedRegions := regionsForModel(peek.Model)

	// 再叠加密钥绑定的区域约束（两者取交集）。密钥绑定后，其可见模型集合
	// 与可用账号集合都限定在该区域，因此请求该区域的模型必然可用。
	regions, ok := constrainRegions(allowedRegions, key)
	if !ok {
		// 无交集：该模型在此密钥的可见列表里不存在，按 OpenAI 语义报 404。
		// 不回显另一区域的任何信息，避免泄露内部区域结构。
		writeOpenAIError(w, http.StatusNotFound, "model_not_found",
			"model "+peek.Model+" does not exist")
		st.status = http.StatusNotFound
		return
	}
	allowedRegions = regions
	pickPred := regionAllowed(allowedRegions)

	rc := h.forwardChat(w, body, forwardOpt{
		pickPred:       pickPred,
		sessKey:        h.sessionKey(body),
		key:            key,
		st:             st,
		allowedRegions: allowedRegions,
		model:          peek.Model,
		clientIP:       clientIPFor(h.cfg.Upstream, r),
		stream:         peek.Stream,
	})
	if rc == nil {
		return // 失败或流式已完成：forwardChat 已按错误策略处置账号并写好响应
	}

	resp, err := upstream.Aggregate(rc)
	rc.Close()
	if err != nil {
		// 上游流解析失败：客户端还没看到任何输出，回 502 并告知原因。
		writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
		st.status = http.StatusBadGateway
		return
	}
	writeJSON(w, http.StatusOK, resp)
	st.status = http.StatusOK
	st.toks = completionTokens(resp)
	// 成本账本（非流式）：从聚合响应的 usage 取 credit 与 token 总数。
	if credit, total, ok := usageCreditTotal(resp); ok {
		h.cfg.Pool.NoteModelCost(st.uid, peek.Model, credit, total)
	}
}

// applyErrorPolicy 按错误分类对账号施加冷却/禁用/熔断策略（最终版状态机）。
// kind 是唯一权威分类（来自 upstream.Classify），此处不再按原始 status 二次判断。
// 仅在 chatCompletions 轮转循环内调用：调用方已准备好 lastErr 并打算 continue 换号。
//
// 七条路径，各司其职：
//   - ErrHardCredit → CooldownUntilTomorrow4AM：即时硬冷却到次日 04:00（等签到恢复）。
//   - ErrSoftRate → 默认 Cooldown(CoolSoft, soft_rate) 连续触发指数退避（封顶 soft_rate_max）；
//     若上游 body 为模型级 6004 且带重置时间 → CooldownSoftForModel（until=重置墙钟，
//     封顶 soft_rate_max，记录触发模型供切模型豁免）。
//   - ErrNotFound → Cooldown(CoolSoft, notFoundCooldown 固定 60s)：短冷却防雪崩，不随 soft_rate 退避。
//   - ErrSessionDead → Disable：session 死亡，永久禁用（需人工重登）。
//   - ErrContentBlocked → 不罚账号（无冷却/熔断/NoteError），passthrough 模式走降级重试。
//   - ErrBadParams → 不罚账号（无冷却/熔断/NoteError，同 ErrContentBlocked 待遇），但仍轮转。
//   - ErrServer → 不罚账号（无冷却/熔断/NoteError，同 ErrClient 待遇），但仍轮转。
//     上游 5xx 是「上游此刻病了」而非「这个号坏了」：喂熔断会把健康号逐批误杀。
//   - 其他（default：ErrClient/ErrNone）→ 只换号不罚（防雪崩），不喂熔断。
//
// ErrContextExceeded 不在此表：调用方在进入轮换前就把它拦下并直接回 400
// （换号改变不了请求体，窗口是模型属性），故本函数不会收到该分类。
//
// body 仅在 ErrSoftRate 分支用于识别上游 6004 模型级限流并解析重置时间；model 为请求
// 携带的模型名（触发 6004 时记录以便后续切模型豁免）。
//
// 恢复出口：CoolSoft/CoolHard 各自到期自动恢复；熔断按其指数退避截止到期；
// 成功（NoteSuccess）清 fails/熔断；签到解冻（ReenableIfCredits→reviveCoolingLocked）只清冷却，不动熔断。
func (h *Handler) applyErrorPolicy(uid string, kind upstream.ErrKind, body, model string) {
	switch kind {
	case upstream.ErrHardCredit:
		// 402 + 余额关键词即积分耗尽：同步冷却到次日 04:00（签到任务 09/21 点恢复），
		// 不需要异步核查（冗余）。立即换号。
		h.cfg.Pool.CooldownUntilTomorrow4AM(uid, "余额不足")
	case upstream.ErrSoftRate:
		// 模型级 6004 且带「将在 … 重置」时间（issue #31）：冷却到上游明说的重置墙钟
		// （封顶 soft_rate_max），记录触发模型 → 该账号对**其他模型**请求可豁免冷却。
		// 解析失败（无时间文案 / 非 6004）→ 退回既有 600s 基数 + 指数退避现况。
		if upstream.IsModelRateLimit(body) {
			if resetAt, ok := upstream.ParseSoftRateReset(body); ok {
				h.cfg.Pool.CooldownSoftForModel(uid, h.cfg.SoftCooldown, resetAt, model, "6004 model rate limit")
				return
			}
		}
		// 其余 soft_rate：软冷却基数来自 soft_rate（默认 600s）；同一账号连续触发时
		// pool 内部按 softStreak 指数退避并封顶 soft_rate_max。
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.cfg.SoftCooldown, "429 rate limit")
	case upstream.ErrSessionDead:
		h.cfg.Pool.Disable(uid, "12153 session dead")
	case upstream.ErrNotFound:
		// 404 短冷却（软冷却），防雪崩。固定 notFoundCooldown，不随 soft_rate 退避：
		// 偶发路径缺失不是限流信号，不该按限流惩罚升级。
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, notFoundCooldown, "upstream 404")
	case upstream.ErrServer:
		// 5xx 上游故障：只换号不罚账号。
		// 上游整体故障（APISIX 502/504、网关超时）时每个账号都会失败——喂熔断计数
		// 会把健康号逐批熔断（实测上游一次故障就把 16/22 打进冷却，healthy 掉到 4），
		// 候选池塌缩后剩下几个号被反复选中、更快撞够阈值，恶性循环；上游恢复后
		// 还要等最长数小时熔断到期才恢复满血。
		// 判据：5xx 是「上游此刻病了」而不是「这个号坏了」——与 ErrClient 同待遇。
		// 单号真坏（如对某模型持续 5xx）由 ErrSoftRate/ErrNotFound 的冷却路径兜底。
	case upstream.ErrContentBlocked:
		// 内容策略拦截（误报）：内容问题非账号问题，不罚账号（无冷却/熔断/NoteError）。
		// passthrough 模式由 chatCompletions 内降级重试处理；custom 模式本不会到此分支。
	case upstream.ErrBadParams:
		// 请求体解析失败（400 + Unmarshal chat params failed / 11101）：发给上游的 body
		// 有问题（网关截断已由 413 消灭，剩余为客户端畸形 JSON）。换了账号照样 400，
		// 不罚账号（无冷却/熔断/NoteError，同 ErrContentBlocked 待遇）；但**仍然轮转**
		// ——不同账号可能有不同的模型权限，值得换号再试一次。
	default:
		// 其余（ErrClient/ErrNone）：只换号不罚（防雪崩），不喂熔断。
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// defaultMaxBodyBytes 请求体上限兜底值（32MB），仅在配置未提供时使用。
// base64 图片膨胀 4/3，一张几 MB 的图就能把请求顶到 8MB 以上，故不能设太小。
const defaultMaxBodyBytes = 32 << 20

// readBody 读取请求体；超过 limit 时返回错误，而不是静默截断。
//
// 为什么要包一层：裸 io.LimitReader 到上限即返回 EOF，io.ReadAll 会拿到一个
// **被截断的 JSON 却无任何错误**。该残体被原样转发给上游，上游回 400
// code=11101 "Unmarshal chat params failed with error: unexpected EOF"，
// 网关对调用方只能报 503 —— 排查时会一路怀疑账号/限流，实际是本地截断。
// 2026-09-13 实测：一张图即触发；截断点常落在 messages 之后，连 model 字段都
// 读不到，于是日志 model 列与错误文案里的模型名都是空的。
// 多读 1 字节才能区分「正好等于上限」与「超过上限」。
func readBody(r *http.Request, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		// 报错前先把剩余请求体读掉再返回：服务端在客户端仍在上传时就关连接，
		// 客户端拿到的是连接重置而不是这个 413（curl 与 python urllib 均能复现，
		// 是否读到取决于时序）—— 那等于把真实原因又藏起来，正是本次要修的问题。
		// 排空只占带宽不占内存，且必须设上限：服务端只配了 ReadHeaderTimeout，
		// 没有 ReadTimeout，不设限的话恶意客户端可以一直流式上传把连接占住。
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, limit))
		return nil, fmt.Errorf("request body exceeds %d MB", limit>>20)
	}
	return body, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    openAIErrType(status),
			"code":    code,
		},
	})
}

// openAIErrType 把 HTTP 状态映射为 OpenAI 的错误类型（与 anthropicErrType 对称）。
//
// 为什么不能一律 api_error：客户端按 type 决定动作 —— 4xx 是「请求有问题，改请求」，
// 5xx 是「服务有问题，稍后重试」。上下文超限若报成 api_error，客户端会当成服务故障
// 原样重发，压缩永不触发（2026-09-18 线上实测）。
//
// 未列出的状态（含 5xx）保持 api_error：与改造前一致，不做无依据的变更。
func openAIErrType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		return "api_error"
	}
}

// writeBodyTooLarge 写 413 响应。
// 独立于通用读取错误：超限是**客户端**问题，在网关侧就判出，不打上游、不罚账号、不轮转，
// 因此错误码必须是可辨识的 request_body_too_large，让调用方知道要调大 server.max_body_mb
// 而不是去查账号或限流。
func writeBodyTooLarge(w http.ResponseWriter, limit int64, anthropic bool) {
	msg := fmt.Sprintf("请求体超过 %d MB 上限：请压缩内容或调大 server.max_body_mb 配置后重试", limit>>20)
	if anthropic {
		writeAnthropicError(w, http.StatusRequestEntityTooLarge, anthropicErrType(http.StatusRequestEntityTooLarge), msg)
		return
	}
	writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_body_too_large", msg)
}

// readBodyOrFail 读请求体，超限时写出 413 并返回 ok=false。
// 四个入口（chat/anthropic/count_tokens/responses）共用，避免各处重复判断与文案漂移。
func (h *Handler) readBodyOrFail(w http.ResponseWriter, r *http.Request, anthropic bool) ([]byte, bool) {
	body, err := readBody(r, h.cfg.MaxBodyBytes)
	if err != nil {
		// 读失败有两种：真的超限（readBody 的哨兵文案）与底层 IO 错误。前者给 413 + 可辨识错误码，
		// 后者给 400 —— 把 IO 错误也报成 413 会误导调用方去调上限。
		if strings.HasPrefix(err.Error(), "request body exceeds") {
			writeBodyTooLarge(w, h.cfg.MaxBodyBytes, anthropic)
		} else if anthropic {
			writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		} else {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		}
		return nil, false
	}
	return body, true
}
