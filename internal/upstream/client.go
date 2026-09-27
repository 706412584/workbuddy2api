// Package upstream 封装对 CodeBuddy 上游（chat / billing / auth）的全部 HTTP 调用，
// 以及错误分类（驱动 pool 冷却状态机）。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
)

// ErrKind 错误分类，pool 据此决定冷却时长。
type ErrKind int

const (
	ErrNone           ErrKind = iota // 成功
	ErrHardCredit                    // 余额不足（402 或 body 关键词）→ 长冷却
	ErrSoftRate                      // 429 软限流 → 短冷却
	ErrSessionDead                   // 401 + 12153 offline session 失效 → 禁用
	ErrNotFound                      // 404 上游偶发 → 短冷却，不累计错误计数（防雪崩）
	ErrServer                        // 5xx 上游故障
	ErrContentBlocked                // 内容策略拦截（400 + 审核文案）→ 不罚账号，走降级重试
	ErrBadParams                     // 请求体解析失败（400 + Unmarshal chat params failed / 11101）→ 不罚账号，仍轮转
	// ErrContextExceeded 上下文超限（400 + context_length_exceeded / 11115）：请求体本身
	// 超过模型窗口。**不轮换**——换账号改变不了请求体，且窗口是模型属性而非账号属性，
	// 每个账号都会得到一模一样的结果。必须原样透传 400 给客户端，否则见下方注释。
	ErrContextExceeded
	// ErrModelNotFound 模型不存在 / 当前不提供（400 + code 11102）：同样是请求侧问题。
	// 与 ErrContextExceeded 的区别是**仍然轮换**——模型清单是区域级的，而网关的静态表
	// 可能滞后于上游；对未收录的模型（不做区域限制）轮换能撞出真正提供它的区域。
	// 但轮换耗尽后必须回 404 而非 503：模型名不对不是「网关没有可用账号」。
	ErrModelNotFound
	// ErrWafBlock 403 + 非业务信封体（APISIX WAF 拦截页/空体/纯文本）→ 账号软冷却
	// + 轮转退避。判据是「无业务信封」：带 code/msg 的 403 走各自的业务分类
	// （如 11140 request illegal → ErrSafetyBanned），不受影响。此前该形态落
	// ErrClient 兜底 → 只换号不罚 → 连环 403 放大请求量。
	ErrWafBlock
	// ErrSafetyBanned 403 + code 11140 + msg "request illegal"（displayMsg 为
	// 「内容未通过安全审核」）→ 账号级内容安全封控，长冷却。
	//
	// 与 ErrContentBlocked 的分工（二者都带「审核」语义，方向相反，不可混淆）：
	//   - ErrContentBlocked 是 HTTP 400 + 逐字指纹误报，**请求侧**问题，换号即可绕过，
	//     故不罚账号、走降级重试；
	//   - ErrSafetyBanned 是 HTTP 403 + **账号侧**终态。2026-09-27 实测：同一 body
	//     换健康账号即 200、最小探针（system+「hi」）同样 403、重新登录（新 session）
	//     不解除、持续 15h 以上。不罚的话死号会一直留在池中被反复选中（实测 50 次），
	//     每个请求吃掉一个 MaxRotate 名额，把正常请求拖成 503。
	//
	// 判据必须同时要求 code 与 msg：11140 是复用码，限流文案也用它
	// （"The model provider is rate-limiting requests." → ErrSoftRate），
	// 只看 code 会把限流误判成封禁。
	ErrSafetyBanned
	ErrClient // 其他 4xx / 业务错误
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrContentBlocked:
		return "content_blocked"
	case ErrBadParams:
		return "bad_params"
	case ErrContextExceeded:
		return "context_exceeded"
	case ErrModelNotFound:
		return "model_not_found"
	case ErrWafBlock:
		return "waf_block"
	case ErrSafetyBanned:
		return "safety_banned"
	case ErrClient:
		return "client"
	default:
		return "none"
	}
}

// Error 带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
	// RetryAfter 上游明示的等待时长（Retry-After 秒 / retry-after-ms /
	// x-ratelimit-reset 头解析，见 ParseRetryAfter）。零值 = 上游未明示，
	// 冷却时长回落调用方计算值。挂载点选在 Error 信封：Kind 决定「罚不罚」，
	// RetryAfter 决定「罚多久」，同为上游响应的一等公民。
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// hardMarkers 余额不足关键词（小写比较 + 中文原文比较双通道）。
var hardMarkers = []string{
	// 单复数必须分别列出：Contains 是子串匹配，词中间插了 's' 就对不上。
	// "insufficient credit" / "no credit" / "out of credit" 恰好是复数形式的前缀，
	// 能直接命中；唯独 "credit exhausted" 的 's' 在词中间，漏了它会让上游的
	// "Credits exhausted"（HTTP 429 + code=14018）落进 429 兜底被判成 soft_rate：
	// 账号只软冷却十分钟就回到池中反复失败，而不会停到次日 04:00 等签到恢复。
	// 2026-09-13 实测：日志里 241 次 14018 全部被误判为 soft_rate。
	"insufficient credit", "no credit", "credit exhausted", "credits exhausted",
	"out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}

// softRateMarkers 限流/节流关键词（小写比较 + 中文原文比较双通道）。
// 上游在状态码非 429 时也会返回限流语义（如 200 + code 11140
// "The model provider is rate-limiting requests."、400 + "rate limit"），
// 此类响应若不识别，账号既不被冷却也不喂熔断，下次请求仍会被选中（issue #28）。
//
// 词表按子串匹配，宁缺毋滥：只收录明确指向「请求速率/模型用量被节流」的措辞。
// 连字符形式（rate-limiting / rate-limited）需单列——Contains 不跨 '-'。
// "too many" 会命中 "too many tokens" 这类客户端参数错误，代价是该号被软冷却
// 一个 SoftCooldown（默认 60s）后自愈，远小于漏判限流导致反复选中同一号的代价。
var softRateMarkers = []string{
	"rate limit", // rate limit / rate limits / rate limiting
	"rate-limiting",
	"rate-limited",
	"too many requests",
	"too many",
	"usage limit", // usage limit reached / model usage limit exceeded（用量节流，非计费余额）
	"请求过于频繁", "限流",
}

var sessionDeadMarkers = []string{"Offline user session not found", "12153"}

// contentBlockedMarkers 内容策略拦截关键词（大小写不敏感子串匹配）。
//
// 定位：上游按逐字精确指纹审核，system 来源的模板句（如 Claude Code/Codex
// 注入指令）触发 HTTP 400 + 以下文案。这是「误报」（合法流量被审核误杀），
// 非账号问题——该账号余额健康、未限流、session 未死，故 ErrContentBlocked
// 在 applyErrorPolicy 中不罚账号（无冷却/熔断/NoteError），改由网关降级重试。
var contentBlockedMarkers = []string{
	"blocked by security policy",
	"unapproved channel",
	"illegal api invocation",
}

// safetyBanned 账号级内容安全封控的识别标记（生产实测形态，2026-09-27）：
//
//	{"code":11140,"msg":"request illegal","requestId":"...",
//	 "displayMsg":{"en":"The content did not pass the safety review. Please adjust and retry.",
//	               "zh":"内容未通过安全审核，请调整后重试"}}
//
// **两个标记必须同时命中**：11140 是复用码——模型级限流也用它
// （{"code":11140,"msg":"The model provider is rate-limiting requests."}，
// 由 softRateMarkers 先接住判为 ErrSoftRate）。只看 code 会把限流误判成封禁，
// 把一个十分钟自愈的号罚成长期停用。
const safetyBannedBizCode = `"code":11140`
const safetyBannedMsg = "request illegal"

// IsSafetyBanned 报告 403 响应是否为账号级内容安全封控（11140 + request illegal）。
// 与 IsWafBlocked 互补：后者要求「无业务信封」，本形态带完整业务信封，故不冲突。
func IsSafetyBanned(status int, body string) bool {
	if status != http.StatusForbidden {
		return false
	}
	return strings.Contains(body, safetyBannedBizCode) &&
		strings.Contains(strings.ToLower(body), safetyBannedMsg)
}

// badParamsMarkers 请求体解析失败关键词（issue #41 连带）：HTTP 400 + 上游
// "Unmarshal chat params failed..."（code 11101）。这是"发给上游的 body 有问题"，
// 与账号健康无关——不罚号，但仍轮转（commit B）。
var badParamsMarkerMsg = "Unmarshal chat params failed"
var badParamsMarkerCode = `"code":11101`

// contextExceededMarker 上下文超限的识别标记（生产实测形态）：
//
//	{"code":11115,"msg":"prompt is too long: 1049589 tokens > 1048576 maximum",
//	 "extError":{"code":"context_length_exceeded","message":"..."}}
//
// 三个标记任一命中即归类。用 extError.code 作首选（结构化、不受文案语言影响），
// 另两个是冗余保险 —— 上游改文案时至少还能命中一个。
const contextExceededCode = "context_length_exceeded"
const contextExceededBizCode = `"code":11115`
const contextExceededMsg = "prompt is too long"

// contextExceededPattern 从上游 body 里提取可直接透传给客户端的错误消息。
//
// 只取 "prompt is too long: N tokens > M maximum" 这一段（上游原话），
// 因为下游客户端（Claude Code 等）正是按这句话识别"该压缩上下文了"。
// Anthropic 官方 API 的原生文案也是这个格式，故透传即可被识别。
var contextExceededPattern = regexp.MustCompile(`prompt is too long: [^"\\]{0,120}`)

// ContextExceededMessage 返回可直接回给客户端的上下文超限消息。
// 解析失败时回退到固定文案（不含上游 requestId 等噪声，那些对客户端无意义）。
func ContextExceededMessage(body string) string {
	if m := contextExceededPattern.FindString(body); m != "" {
		return strings.TrimSpace(m)
	}
	return "prompt is too long: this request exceeds the model's maximum context length"
}

// IsContextExceeded 报告上游 body 是否为上下文超限错误。
func IsContextExceeded(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, contextExceededCode) ||
		strings.Contains(body, contextExceededBizCode) ||
		strings.Contains(lower, contextExceededMsg)
}

// modelNotFoundMarker 模型不存在 / 当前区域不提供该模型（生产实测形态）：
//
//	{"code":11102,"msg":"model [deepseek-v4] service info not found",
//	 "displayMsg":{"en":"The requested model is not available. Please switch to another model."}}
//
// 与上下文超限同类：这是**请求**的问题（模型名不对/该区不提供），不是网关或账号的问题。
// 原先进 ErrClient → 换号重试 → 503 no_healthy_account，客户端读作「网关故障」。
const modelNotFoundBizCode = `"code":11102`
const modelNotFoundMarker = "service info not found"

// IsModelNotFound 报告上游 body 是否为「模型不存在」。
func IsModelNotFound(body string) bool {
	return strings.Contains(body, modelNotFoundBizCode) ||
		strings.Contains(strings.ToLower(body), modelNotFoundMarker)
}

// hasBusinessEnvelope 报告错误 body 是否携带上游业务信封形态（JSON 且含
// `"code":` 或 `"msg":` 字段）。WAF 403 判定（IsWafBlocked）用「无业务信封」
// 区分 APISIX WAF 拦截页（HTML/空体/纯文本）与上游业务层 403（带 code/msg
// 信封，正常走既有分类）。不做 JSON 解析：信封存在性只需字段名命中——
// 畸形 JSON 但含 `"msg":` 字样仍按业务响应保守处理（宁漏判 WAF 也不误罚
// 业务 403，后者有各自的权威分类）。
func hasBusinessEnvelope(body string) bool {
	return strings.Contains(body, `"code":`) || strings.Contains(body, `"msg":`)
}

// IsWafBlocked 报告 403 响应是否为 WAF 拦截形态：HTTP 403 且 body 无业务信封
// （无 `"code":`/`"msg":` JSON 字段——HTML 拦截页、空体、纯文本均命中）。
// 带业务信封的 403（11140 request illegal → ErrSafetyBanned 等）不在此列，
// 由 Classify 在下方按各自标记分类。
func IsWafBlocked(status int, body string) bool {
	return status == http.StatusForbidden && !hasBusinessEnvelope(body)
}

// retryAfterHeaderCandidates 冷却时长优先解析的响应头候选序列：
// retry-after（秒，RFC 7231）/ retry-after-ms（毫秒）/ x-ratelimit-reset
// （epoch 秒或毫秒，取 now+ 剩余量）。大小写不敏感（http.Header.Get 已归一）。
var retryAfterHeaderCandidates = []string{"Retry-After", "Retry-After-Ms", "X-Ratelimit-Reset"}

// retryAfterSanity 解析结果的上限（超过视为上游异常值丢弃，回落本地计算），
// 与 pool 的 softRateMax 默认 2h 同量级（上游不该明示比冷却封顶更长的等待）。
const retryAfterSanity = 2 * time.Hour

// ParseRetryAfter 从限流/拦截响应头解析上游明示的等待时长：依次尝试
// Retry-After（整数秒）→ retry-after-ms（整数毫秒）→ x-ratelimit-reset
// （纯数字按 epoch 秒/毫秒推断）。任一头缺失/非法/非正/超上限则尝试下一头；
// 全部不可用返回 false（调用方回落既有计算值，绝不臆造等待时长）。
func ParseRetryAfter(h http.Header) (time.Duration, bool) {
	for _, name := range retryAfterHeaderCandidates {
		v := strings.TrimSpace(h.Get(name))
		if v == "" {
			continue
		}
		if !isAllDigits(v) {
			continue // 非纯数字（如 HTTP-Date）不解析，宁缺毋滥
		}
		n, ok := parseRetryNumber(v, name)
		if !ok {
			continue
		}
		if n <= 0 || n > retryAfterSanity {
			continue // 非正/异常大：丢弃（回落本地计算）
		}
		return n, true
	}
	return 0, false
}

// isAllDigits 报告 s 是否为纯数字（前置快筛，免 strconv 之后再判语义）。
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// parseRetryNumber 按头名口径把纯数字串折算成时长。x-ratelimit-reset 是
// epoch 时刻而非时长：秒口径（10 位）与毫秒口径（13 位）都按「now+ 该时刻
// 的剩余量」折算，已在过去则不可用。位数不足（8 位以下）无法判定 epoch
// 语义的丢弃（宁缺毋滥：该族实践发 epoch，短串多半是序号之类的误用头）。
func parseRetryNumber(v, headerName string) (time.Duration, bool) {
	// 上限 16 位防 int64 溢出（超过 epoch 毫秒的现实量级必非法）。
	if len(v) > 16 {
		return 0, false
	}
	var n int64
	for _, r := range v {
		n = n*10 + int64(r-'0')
	}
	switch headerName {
	case "Retry-After":
		return time.Duration(n) * time.Second, true
	case "Retry-After-Ms":
		return time.Duration(n) * time.Millisecond, true
	default: // X-Ratelimit-Reset：epoch → 剩余量
		sec := n
		if len(v) >= 12 { // 毫秒口径（13 位）；11 位边界按秒（误判代价是多算 1000 倍）
			sec = n / 1000
		}
		remain := time.Until(time.Unix(sec, 0))
		return remain, true
	}
}

// ModelNotFoundMessage 返回可直接回给客户端的模型不存在消息。
//
// 优先用上游的中英双语 displayMsg（对用户友好）；缺失时回退到 msg 字段；
// 都取不到才用固定文案。不含 requestId 等内部字段。
func ModelNotFoundMessage(body string) string {
	if m := reModelNotFoundDisplay.FindStringSubmatch(body); len(m) > 1 {
		return m[1]
	}
	if m := reModelNotFoundMsg.FindStringSubmatch(body); len(m) > 1 {
		return m[1]
	}
	return "the requested model is not available; please switch to another model"
}

var (
	reModelNotFoundDisplay = regexp.MustCompile(`"displayMsg":\{"en":"([^"]{1,200})"`)
	reModelNotFoundMsg     = regexp.MustCompile(`"msg":"(model \[[^"]{0,120}service info not found)"`)
)

// alreadyCheckinMarkers "今天已签到"关键词（上游对重复签到返回 code!=0，
// 实测 code=10001/14001 "今天已签到"/"今日已签到"）。只对 *Error.Msg 做包含匹配，
// 网络层/解析层错误不在此识别（见 IsAlreadyCheckin）。
var alreadyCheckinMarkers = []string{"已签到", "already"}

// softRateResetLoc 上游 429 6004 文案中的重置时间固定按 UTC+8 解释（上游文案如此，
// 与容器时区无关）。
var softRateResetLoc = time.FixedZone("UTC+8", 8*60*60)

// SoftRateResetLoc 暴露重置时间的固定时区（供测试构造/断言同一时区口径）。
func SoftRateResetLoc() *time.Location { return softRateResetLoc }

// modelRateLimitCode 明确指向「模型级 429 限流」的业务 code。
// 上游用它表达"该模型的使用量超限"（code 6004，msg 带重置时间），
// 而不是账号整体被限流——账号健康，只是这个模型此刻被限（issue #31）。
const modelRateLimitCode = "6004"

// refreshTokenExpiresInMax 可接受的 refresh 响应 expiresIn 量级上限（10 年）。
// 上游实测恒为 5184000（60d），超量级值只会是脏数据——照写会把 ExpiresAt 推到
// 荒谬未来，NeedsRefresh 永假 → token 永不刷新反而真过期失效。
const refreshTokenExpiresInMax = 10 * 365 * 24 * time.Hour

// softRateResetPattern 匹配 6004 文案里的重置时刻，捕获时间串。
//
// 两种语言的实测形态（**必须都覆盖**）：
//   - 英文（生产实测唯一形态，intl 与 cn 均如此）：
//     "...your usage will reset at 2026-09-18 14:04:42 UTC+8, alternatively, ..."
//   - 中文："使用量已超出频率限制，将在 2026-09-12 18:08:12 UTC+8 重置"
//
// 直接用时间格式匹配而非「抓分隔符之间的任意串」：分隔符在两种语言里不同
// （中文用「 重置」、英文用逗号），而时间格式是同一套；且英文文案的时间后可能
// 跟逗号/句点/直接结束，靠分隔符会漏。这也是曾出过的 bug —— 原来的模式只写了
// 中文 `将在 (.+?) 重置`，而生产日志里 6004 文案**全是英文**，于是 ResetAt 恒解析
// 失败、模型级冷却（CooldownSoftForModel）从未生效，账号被按「全模型」冷却了，
// 而上游明说 "you can switch to the other models to continue using it"。
// 前缀 (?i) 不可省：上游大小写不保证（实测见过 "Your Usage Will RESET AT …"），
// 无该标志时大小写变体漏匹配 → 退回指数退避而非对齐上游重置墙钟。
const softRateResetPattern = `(?i)(?:将在 |will reset at )(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}(?: UTC\+8)?)`

// 限流判定正则预编译为包级 var（发现 8）：IsModelRateLimit / ParseRateReset
// 在每次错误分类、每个限流 body 上调用，函数体内 MustCompile 是纯浪费；
// 错误风暴（429 轰炸）时尤甚。模式串均为纯常量，与 sanitize.go 的包级
// 预编译先例保持一致。regexp 并发安全（匹配只读），无需额外锁。
var (
	reModelRateLimit = regexp.MustCompile(`"code"\s*:\s*"?` + modelRateLimitCode + `"?`)
	reSoftRateReset  = regexp.MustCompile(softRateResetPattern)
)

// softRateTimeLayout 上游重置时间的格式（无时区后缀；时区固定 UTC+8）。
const softRateTimeLayout = "2006-01-02 15:04:05"

// IsModelRateLimit 报告 429 body 是否明确指向模型级限流（业务 code 6004）。
// 用于区分"账号级软限流"（按账号冷却）与"模型级用量限流"（切模型即可用）。
func IsModelRateLimit(body string) bool {
	// `"code":6004` / `"code": 6004` / `"code":"6004"` 均可命中（JSON 空格容差）。
	return reModelRateLimit.MatchString(body)
}

// ParseSoftRateReset 从 429 body 解析重置时刻（上游 UTC+8 文案，中英文均可）。
// 成功返回解析出的**墙钟时刻**（按 UTC+8 解释），失败返回零值 + false。
// 内部先判 IsModelRateLimit：非模型级限流（非 6004）即使带"重置"字样也不返回——该重置
// 无冷却语义（如 11140 的通用限流提示），解析出来反而会错误收窄冷却。
// 正则用包级预编译的 reSoftRateReset（发现 8）：本函数在每次错误分类上调用，
// 函数体内 MustCompile 是纯浪费（模式串为常量，语义零变更）。
func ParseSoftRateReset(body string) (time.Time, bool) {
	if !IsModelRateLimit(body) {
		return time.Time{}, false
	}
	m := reSoftRateReset.FindStringSubmatch(body)
	if len(m) < 2 {
		return time.Time{}, false
	}
	ts := strings.TrimSpace(m[1])
	ts = strings.TrimSuffix(ts, " UTC+8") // 去掉后缀，固定按 softRateResetLoc 解释
	t, err := time.ParseInLocation(softRateTimeLayout, ts, softRateResetLoc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// Classify 按 HTTP 状态码 + body 判定错误类别。
//
// 判定顺序自「严」到「宽」，每层的先后都有语义依据：
//  1. 402 / hardMarkers —— 计费额度耗尽，最严、最不可自愈，必须最先判。
//     "quota exceeded" 语义跨计费/限流两界，历史归 hard_credit，本次保持不变
//     （issue #28 已记录该反向误判风险，待上游原始响应确认后再定）。
//  2. sessionDeadMarkers —— 需要人工重登的终态。若 401 body 同时含 "12153" 与
//     "rate limit"（如网关错误页混排），归 session_dead：短冷却救不活失效 session，
//     误判为限流会让该死号留在池中反复被选中；且此层 marker 是精确词（12153 等），
//     比限流层的大范围子串更具体，具体优先于宽泛。
//  3. softRateMarkers —— 非 429 状态码携带限流文案（issue #28 修复点）。
//     位于此处可覆盖 200/400/403/5xx 各状态码；429 且 body 含文案时在此短路，
//     结果同为 soft_rate，与下一层一致。
//  4. status==429 —— body 无文案时的兜底识别。
//  5. 404 / 5xx / 其他 4xx —— 与限流无关的常规分类。
//
// 403 的三个分流（都在下方 4xx 层之前）：无业务信封 → WAF；带 11140+request illegal
// → 账号级安全封控；其余带信封 403 → 按既有 marker 链分类。
func Classify(status int, body string) ErrKind {
	if status == http.StatusPaymentRequired {
		return ErrHardCredit
	}
	lower := strings.ToLower(body)
	for _, m := range hardMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrHardCredit
		}
	}
	for _, m := range sessionDeadMarkers {
		if strings.Contains(body, m) {
			return ErrSessionDead
		}
	}
	for _, m := range softRateMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrSoftRate
		}
	}
	if status == http.StatusTooManyRequests {
		return ErrSoftRate
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 500 {
		return ErrServer
	}
	// WAF 403（无业务信封的拦截形态）：判在内容策略/参数错误/通用 4xx 之前——
	// 这些层只认带文案的 body，WAF 空体/HTML 永远不会命中它们的 marker，
	// 但落 ErrClient 兜底的代价是「只换号不罚」，连环 403 会放大请求量。
	// 带信封的 403 在上方各层已有权威分类，不受影响。
	if IsWafBlocked(status, body) {
		return ErrWafBlock
	}
	// 账号级内容安全封控（403 + 11140 + request illegal）：判在内容策略之前——
	// 二者都含「审核」语义但方向相反，且 contentBlockedMarkers 并不匹配本形态，
	// 不显式分流就会一路落进 ErrClient 兜底（只换号不罚），死号留在池中被反复选中。
	if IsSafetyBanned(status, body) {
		return ErrSafetyBanned
	}
	// 内容策略拦截（HTTP 400 + 审核文案）：判在通用 ErrClient 之前。
	// 这是误报信号，不罚账号，由网关降级重试处理（见 handler.applyErrorPolicy）。
	if status >= 400 {
		for _, m := range contentBlockedMarkers {
			if strings.Contains(lower, m) {
				return ErrContentBlocked
			}
		}
		// 请求体解析失败（HTTP 400 + Unmarshal chat params failed / code 11101）：
		// 这是"发给上游的 body 有问题"。网关侧截断已由 413 消灭（issue #41 commit A），
		// 剩余来源是客户端 JSON 本身畸形——换了账号照样 400，不该罚号（白白冷却好号）。
		// 归 ErrBadParams：不冷却/不熔断/不计错，但**仍然轮转**（不同账号可能有不同的
		// 模型权限，值得再试一次）。
		if strings.Contains(body, badParamsMarkerMsg) || strings.Contains(body, badParamsMarkerCode) {
			return ErrBadParams
		}
		// 上下文超限：**必须原样透传 400**，不能吞成通用错误。
		//
		// 这里踩过一个坑（2026-09-18 实测）：该错误原先进 default→ErrClient，
		// 网关换 3 个号重试后回 503，Anthropic 侧又被映射成 "overloaded_error"。
		// 客户端（Claude Code）把 overloaded 读作「服务过载，稍后重试」，于是原样
		// 重发同一份超长上下文 —— 日志里同一请求每 10 秒重发一次、token 数一字不差、
		// 连续数十次，自动压缩永不触发（压缩的触发信号是 400 invalid_request_error）。
		// 换号毫无意义：窗口是模型属性，每个账号都会得到完全相同的 400。
		if IsContextExceeded(body) {
			return ErrContextExceeded
		}
		// 模型不存在（11102）：归类但**仍然轮换**——见 ErrModelNotFound 的注释。
		// 判在 ErrClient 之前，好让轮换耗尽时能回 404 而不是 503。
		if IsModelNotFound(body) {
			return ErrModelNotFound
		}
		return ErrClient
	}
	// HTTP 200 但业务 code 非 0 且含余额关键词的情况已被上面 hardMarkers 捕获。
	return ErrNone
}

// apiEnvelope 上游统一信封。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// Client 上游 HTTP 客户端。Base 字段可覆盖便于测试。
type Client struct {
	HTTP *http.Client

	// ChatHTTP 聊天 SSE 专用 client：无总时长上限（Timeout=0），首字节由
	// Transport.ResponseHeaderTimeout 约束，流中空闲由 IdleTimeout 约束。
	// 与 HTTP 共享同一个 *http.Transport 实例，连接池不重复。
	ChatHTTP *http.Client

	// HeaderTimeout 聊天 SSE 首字节前（响应头）超时；<=0 表示未设置（回落 HTTP.Timeout）。
	HeaderTimeout time.Duration
	// IdleTimeout 聊天 SSE 流中空闲超时；<=0 表示禁用空闲监控。
	IdleTimeout time.Duration

	// effortsMu/efforts 缓存各模型 supportedEfforts（FetchModels 刷新），供请求体 effort 降级。
	// 按区域分槽：两个区域同一模型的 effort 档位可能不同，且 FetchModels 会整体替换
	// 所查区域的数据，共用一份会互相抹掉。无 supportedEfforts 的模型不入槽。
	effortsMu sync.RWMutex
	efforts   map[auth.Region]map[string][]string

	// SanitizeFingerprints 出站请求体黑名单指纹脱敏开关（默认 true；false 完全还原）。
	SanitizeFingerprints bool

	// UserAgent 出站 User-Agent 显式覆盖（非空时全路径生效，优先于默认 WorkBuddy
	// 三段式与 billingUA 单段式）。空 = 默认官方形态：chat/refresh/FetchModels 走
	// `WorkBuddy/<ver> WorkBuddy/<ver> CLI/<cliVer>`；billing/checkin 走 `WorkBuddy/<ver>`
	// （仅当 client_name 非空，见 billingUA）。
	// issue #42 深挖：官网「使用端」列基于出站请求的 UA/X-Product 服务端归因，
	// 官方 WorkBuddy 桌面 UA 见 defaultWorkBuddyUA。默认值已对齐官方（A 段变更），
	// 用户仍可显式配置完全自定义的 UA。
	UserAgent string

	// DeviceToken 设备风控 Token（X-Device-Token 头）兜底来源：config upstream.device_token。
	// 仅当 auth.Auth.DeviceToken 为空时才取此值；两者皆空则不注入该头。
	// 容器内无桌面端 Turing SDK，这是把外部（宿主/桌面端）生成的 token 注入的入口。
	// 另见 DeviceTokenFile 缓存读取：宿主可把 token 落 /app/data/device_token 共用。
	DeviceToken string

	// DeviceTokenFile 宿主落盘的 device token 文件路径（可选，空 = 不读文件）。
	// 读取频率限 5 分钟一次缓存（见 device_token.go），>1KB 或读失败则忽略。
	// 解析优先级：auth.Auth.DeviceToken > DeviceToken（config）> DeviceTokenFile（文件）。
	DeviceTokenFile string

	// ClientName 用量归属头取值（X-Product / X-IDE-Name / X-IDE-Type / X-IDE-Version）。
	// 空 = 旧行为：X-Product="SaaS"，不设 X-IDE-*（向后兼容，不突变归因）。
	// 非空（如 "WorkBuddy"）则四头跟随，对齐官方桌面端 client 识别。
	ClientName string

	// ClientVersion WorkBuddy 客户端版本段（出站 UA 的 `WorkBuddy/<ver>` + B 段的
	// X-IDE-Version）。空 = 内置默认 defaultClientVersion（对齐官方 5.5.4 分发包）。
	// config upstream.client_version 覆盖。
	ClientVersion string

	// CliVersion 出站 UA 中 `CLI/<ver>` 段版本。空 = 内置默认 defaultCliVersion
	// （对齐官方内置 CLI 2.137.1）。config upstream.cli_version 覆盖。
	CliVersion string

	// PassthroughIP 是否透传客户端 IP 给上游（X-Forwarded-For/X-Real-IP 首段）。
	// 缺省 false（反代安全边界）；handler 在 chat 路径按请求把 clientIP 参数传入 ChatStream，
	// 由 ChatHeaders 注入（不再挂共享字段，杜绝并发串扰）。
	PassthroughIP bool

	// 双区域 base：区域由凭证的 domain 决定（见 auth.Region / chatBase / billingBase）。
	// 上游只有 CN 一对字段，本网关额外维护 global 一对。
	ChatBaseCN     string
	BillingBaseCN  string
	ChatBaseGlobal string
	BillingBaseGl  string

	// proxyMu 保护 proxyFor：面板可在运行期改绑定（热重载），而每个出站请求都读它。
	proxyMu sync.RWMutex
	// proxyFor 账号 → 代理 URL 的解析函数（见 SetProxyResolver）。nil = 全部直连。
	proxyFor func(*auth.Auth) string
}

// labelOf 日志用的账号标识（uid 前 8 位）。nil 安全。
func labelOf(a *auth.Auth) string {
	if a == nil {
		return "<nil>"
	}
	return logfmt.UID8(a.UID)
}

// newDialer 构造出站拨号器（参数集中于此，供测试回读断言）。
// Timeout 10s：对端 SYN 不回时应答时快速失败轮转换号，不干等系统 TCP 重传窗口。
// KeepAlive 15s（原 30s）：默认 Dialer 2h 才发首个探测，NAT 黑洞里连接半死仍会被
// 复用；15s 周期让死连接在 15~30s 内被内核掐掉（RST/ETIMEDOUT），复用侧立即感知。
func newDialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 15 * time.Second,
	}
}

// newBaseTransport 构造出站 Transport 骨架（不含代理）。proxy 非空时挂代理
// （http/https 走 Transport.Proxy，socks5 走自定义 DialContext）。
//
// New() 与代理池共用本函数，保证「直连」与「走代理」的超时/连接池参数**逐字一致**
// ——否则两种形态会有两套不同的超时行为，排查时极易误判。
func newBaseTransport(proxy *proxyConfig) *http.Transport {
	tr := &http.Transport{
		// 显式配置 DialContext：零值只有 KeepAlive、没有拨号超时，
		// 对端 SYN 不回时应答时 TCP 层可挂数分钟。
		DialContext: newDialer().DialContext,
		// 上游走 h2（错误文案 "http2: timeout awaiting response headers"）；
		// 自定义 DialContext 会关掉自动 h2，必须显式强制开启。
		//
		// 注：上游 fork 的 3d9a4cc 主张「禁 h2」（置空 TLSNextProto，理由是半死
		// h2 流复用）。本地与之结论相反且已在生产跑通，故**保持开启**——两边都
		// 自称有实证，本地日志从未出现该错误，不做无证据的反向切换。
		ForceAttemptHTTP2: true,
		// ResponseHeaderTimeout 在请求体写完后才起算，握手不在其保护范围内；
		// 缺这条则 TLS 握手黑洞可以无限挂（ChatHTTP 无总时长兜底）。
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		// IdleConnTimeout 90s→30s：上游常态性掐闲置连接，90s 池里的连接多半
		// 已死；复用侧仍有 15s keepalive 兜底识别。
		IdleConnTimeout: 30 * time.Second,
		// 聊天 SSE 首字节前硬上限（对短 RPC 无实际影响：其总时长 120s 更先到期）。
		// 注意：本值在 main.go 会被 config `header_timeout_seconds` 覆盖，此处
		// 仅为未配置时的默认；生产取值以 config.json 为准。
		ResponseHeaderTimeout: 120 * time.Second,
	}
	if proxy != nil {
		proxy.apply(tr)
	}
	return tr
}

// New 生产默认值。配置连接池减少 TLS 握手。
func New() *Client {
	tr := newBaseTransport(nil)
	return &Client{
		HTTP:                 &http.Client{Timeout: 120 * time.Second, Transport: tr},
		ChatHTTP:             &http.Client{Timeout: 0, Transport: tr}, // 无总时长；首字节由 ResponseHeaderTimeout 管
		SanitizeFingerprints: true,
		ChatBaseCN:           "https://copilot.tencent.com",
		BillingBaseCN:        "https://www.codebuddy.cn",
		ChatBaseGlobal:       "https://www.workbuddy.ai",
		BillingBaseGl:        "https://www.workbuddy.ai",
	}
}

// chatHTTP 返回聊天专用 client；未设置（如测试只注入 HTTP）时回落 HTTP。
func (c *Client) chatHTTP() *http.Client {
	if c.ChatHTTP != nil {
		return c.ChatHTTP
	}
	return c.HTTP
}

func (c *Client) chatBase(a *auth.Auth) string {
	if a != nil && a.Region() == auth.RegionGlobal {
		return c.ChatBaseGlobal
	}
	return c.ChatBaseCN
}

// prepareBody 组装出站请求体（脱敏开关由 Client.SanitizeFingerprints 控制）。
// 末尾注入 prompt_cache_key（P0 费用优化）：按账号隔离的稳定缓存键，让同一客户端
// 对同一账号的连续请求命中上游前缀缓存。会话段从 body 自带的 conversation_id /
// conversationId 取（本地未解析 X-Conversation-ID 头，故只走 body 源）。
func (c *Client) prepareBody(a *auth.Auth, body []byte) []byte {
	body = PrepareBodyOptWithEfforts(body, c.SanitizeFingerprints, c.effortsSnapshot(a))
	uid := ""
	if a != nil {
		uid = a.UID
	}
	return InjectPromptCacheKey(body, uid, "")
}

// effortsSnapshot 返回该账号所在区域的 effort 能力缓存副本；nil 表示未知（透传不降级）。
func (c *Client) effortsSnapshot(a *auth.Auth) map[string][]string {
	c.effortsMu.RLock()
	defer c.effortsMu.RUnlock()
	src := c.efforts[a.Region()]
	if len(src) == 0 {
		return nil
	}
	cp := make(map[string][]string, len(src))
	for k, v := range src {
		cp[k] = v
	}
	return cp
}

func (c *Client) billingBase(a *auth.Auth) string {
	if a != nil && a.Region() == auth.RegionGlobal {
		return c.BillingBaseGl
	}
	return c.BillingBaseCN
}

// billing 域端点路径（billingBase + path）。balance/checkin 与 report（report.go）同域，
// 统一走 billingJSON 发请求。
const (
	billingMeterPath = "/v2/billing/meter/get-user-resource"
	dailyCheckinPath = "/v2/billing/meter/daily-checkin"
)

// doJSON 发请求并解信封；HTTP 非 2xx 或业务 code != 0 时返回带 body 片段的 *Error。
// body 读失败（连接中断/空闲掐流/截断）返回普通错误（非 *Error）——半截 body 不进
// Classify，不参与账号惩罚（传输层故障不该喂熔断误罚号）。
//
// a 为该请求所属账号，用于选取绑定的代理（nil = 直连，与调用方无账号的场景兼容）。
func (c *Client) doJSON(a *auth.Auth, req *http.Request) (json.RawMessage, error) {
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
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}
	if env.Code != 0 {
		kind := Classify(resp.StatusCode, env.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))}
	}
	return env.Data, nil
}

// RefreshToken 刷新 access token；成功时更新 a 的字段（缺省值保留旧值），
// 调用方负责 SaveAtomic。
//
// 两段式（网络 IO 移出锁）：旧实现全程持 a.mu 跨 HTTP 往返，30s 网络超时期间
// 所有出站请求头构造（ChatHeaders/CommonHeaders 都要经 Region()/AccessTokenValue()
// 取值）会被该锁堵住，一个账号刷新即拖住整池并发。现改为「锁内读快照 →
// 锁外 IO → 锁内写回」，与 auth.mu 契约（只保护字段读写、不覆盖 IO）一致。
func (c *Client) RefreshToken(a *auth.Auth) error {
	// 第 1 段（锁内）：读快照。
	a.Lock()
	rtSnapshot := a.RefreshToken
	atBefore := a.AccessToken
	a.Unlock()
	if strings.TrimSpace(rtSnapshot) == "" {
		return fmt.Errorf("no refreshToken")
	}
	// chatBase 经 Region() 取值——此刻未持锁，安全。
	url := c.chatBase(a) + "/v2/plugin/auth/token/refresh"
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	// RefreshHeaders 读 a 的 domain/uid 等字段注入请求头：锁内取一份逐字段拷贝
	// （不拷贝 sync.Mutex，避免 vet copylocks），用该副本构造头。
	a.Lock()
	hdrSnapshot := auth.Auth{
		AccessToken:  a.AccessToken,
		RefreshToken: rtSnapshot,
		ExpiresAt:    a.ExpiresAt,
		Domain:       a.Domain,
		UID:          a.UID,
		EnterpriseID: a.EnterpriseID,
		Nickname:     a.Nickname,
		DeviceToken:  a.DeviceToken,
	}
	a.Unlock()
	c.RefreshHeaders(req, &hdrSnapshot)

	// 网络 IO（锁外）。
	data, err := c.doJSON(a, req)
	if err != nil {
		return err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return fmt.Errorf("refresh_failed: no accessToken in response — re-login required")
	}
	// 第 2 段（锁内）：校验快照一致后写回。
	a.Lock()
	defer a.Unlock()
	// 锁外期间另一刷新已完成（两 token 同时 rotate）→ 新 token 已生效，本次结果
	// 不必再写，避免无意义覆盖与 ExpiresAt 抖动。
	if a.AccessToken != atBefore && a.RefreshToken != rtSnapshot {
		return nil
	}
	a.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		a.Domain = tok.Domain
	}
	// preserveExpiry：响应缺 expiresIn 时保留旧过期时间，避免刷新风暴。
	// 超过 10 年的 expiresIn 按脏值处理保留旧值：照写会把 ExpiresAt 推到荒谬未来
	// → NeedsRefresh 永假 → token 永不刷新反而真过期失效。
	if tok.ExpiresIn > 0 && time.Duration(tok.ExpiresIn)*time.Second < refreshTokenExpiresInMax {
		a.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	return nil
}

// ChatStream 发 chat 请求并返回原始 SSE body 流（调用方负责 Close）。
// clientIP 为本次请求的客户端 IP（PassthroughIP=true 时注入；空串表示不透传）。
// 按**请求传递**而非读共享字段：避免并发请求交叉污染对方 IP（issue：ClientIP 竞态）。
// 非 2xx 时 rc 为 nil、body 为上游响应体（供调用方 Classify(status, string(body))）、err 为 nil；
// 只有传输层失败才返回 err。
func (c *Client) ChatStream(a *auth.Auth, body []byte, clientIP string) (rc io.ReadCloser, status int, respBody []byte, err error) {
	url := c.chatBase(a) + "/v2/chat/completions"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(c.prepareBody(a, body)))
	if err != nil {
		return nil, 0, nil, err
	}
	c.ChatHeaders(req, a, clientIP)
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)
	resp, err := c.clientFor(a, c.chatHTTP()).Do(req)
	if err != nil {
		cancel()
		log.Printf("ERR: [upstream] chat_stream uid=%s: transport error: %v", logfmt.UID8(a.UID), err)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		// 读失败时 raw 可能为半截：不把它交给 Classify（半截 body 可能命中
		// 余额不足等 marker，把传输层故障误判成账号问题）。
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()
		if readErr != nil {
			log.Printf("ERR: [upstream] chat_stream uid=%s: read error body: %v", logfmt.UID8(a.UID), readErr)
			return nil, resp.StatusCode, nil, readErr
		}
		kind := Classify(resp.StatusCode, string(raw))
		log.Printf("WARN: [upstream] chat_stream uid=%s: upstream %d %s body=%s",
			logfmt.UID8(a.UID), resp.StatusCode, kind, truncate(string(raw), 200))
		return nil, resp.StatusCode, raw, nil
	}
	// 成功分支：cancel 所有权交给 monitorBody（其 Close 会 cancel）；
	// IdleTimeout<=0 时 monitorBody 原样返回底流、无人调 cancel——可接受：
	// ctx 无 deadline 无 goroutine，连接由 resp.Body.Close 正常清理。
	return monitorBody(resp.Body, c.IdleTimeout, cancel), resp.StatusCode, nil, nil
}

// ModelInfo 动态模型信息（含 maxInputTokens/maxOutputTokens 与展示用元数据）。
//
// 除 ID/尺寸外全部为**展示字段**，不参与选号与路由决策：
// 上游两域下发同一套字段（credits/description/vendor/tags/能力旗标），
// 此前只取 4 个，其余丢弃；现全量透出供客户端与面板展示。
type ModelInfo struct {
	ID            string
	Name          string
	ContextWindow int64    // = maxInputTokens
	MaxTokens     int64    // = maxOutputTokens
	Efforts       []string // reasoning.supportedEfforts（空=未知/固定档）

	// Credits 积分倍率原文（如 "x0.11 credits" / "x0.00"），**仅展示不参与选号**。
	// 空串 = 上游未下发（非对话模型如图像/视频），与 "x0.00"（真免费）语义不同，
	// 展示侧必须区分：空 → 不显示，x0.00 → 显示免费。
	Credits string
	// Description 模型描述（优先中文 descriptionZh，回落英文 descriptionEn）。
	Description string
	// Vendor 供应商标识（如 "f"），仅展示。
	Vendor string
	// Tags 模型标签（如 ["craft"]），仅展示。
	Tags []string
	// IsDefault 是否为该区默认模型。
	IsDefault bool

	// 能力旗标：供客户端按需选择（多模态/推理/工具调用）。
	SupportsImages    bool
	SupportsReasoning bool
	SupportsToolCall  bool
	// CanDisableThinking 推理模型是否允许关闭思考（false 时思考恒开）。
	CanDisableThinking bool
}

// modelsPathFor 按账号区域返回模型目录端点路径。
//
// 两域**路径不同**（2026-09 实测）：
//   - CN  → /console/enterprises/personal/models
//   - global → /v3/config（同一路径 /console/... 在 global 是 HTTP 500 APISIX 错误页
//     ——这是 intl 侧动态模型长期不可用的根因，此前靠 staticModelsGlobal 兜底，
//     代价是拿不到 credits 等元数据）
//
// 两域响应结构同构（data.models[] + data.agents[]，agent name 均为 "cli"），
// 故解析链共用，仅路径分叉。
func (c *Client) modelsPathFor(a *auth.Auth) string {
	if a != nil && a.Region() == auth.RegionGlobal {
		return "/v3/config"
	}
	return "/console/enterprises/personal/models"
}

// FetchModels 调上游动态模型接口（按账号区域自动选路径，见 modelsPathFor）。
// 字段名与上游实际返回对齐：maxInputTokens（非 contextWindow）、maxOutputTokens（非 maxTokens）。
func (c *Client) FetchModels(a *auth.Auth) ([]ModelInfo, error) {
	url := c.chatBase(a) + c.modelsPathFor(a)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	c.CommonHeaders(req, a) // 复用共享请求头（Origin/Referer/UA/Accept/Content-Type）
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	resp, err := c.clientFor(a, c.HTTP).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models api status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	out, err := parseModels(raw)
	if err != nil {
		return nil, err
	}
	// 刷新 effort 能力缓存（供请求体降级；无 supportedEfforts 的模型不入缓存）。
	// 只替换该账号所在区域的槽位，另一区域的缓存保持不变。
	cache := make(map[string][]string, len(out))
	for _, mi := range out {
		if len(mi.Efforts) > 0 {
			cache[mi.ID] = mi.Efforts
		}
	}
	c.effortsMu.Lock()
	if c.efforts == nil {
		c.efforts = make(map[auth.Region]map[string][]string, 2)
	}
	c.efforts[a.Region()] = cache
	c.effortsMu.Unlock()
	return out, nil
}

// modelsEnvelope 上游模型目录响应信封（CN /console/... 与 global /v3/config 同构）。
type modelsEnvelope struct {
	Code int `json:"code"`
	Data struct {
		Models []modelEntry `json:"models"`
		Agents []struct {
			Name   string   `json:"name"`
			Models []string `json:"models"`
		} `json:"agents"`
	} `json:"data"`
}

// modelEntry 上游单个模型条目。两域字段同名同义；零值即未下发（展示侧据此省略，
// **不编造**——尤其 Credits 缺失≠免费）。
type modelEntry struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	Credits            string   `json:"credits"`
	DescriptionZh      string   `json:"descriptionZh"`
	DescriptionEn      string   `json:"descriptionEn"`
	Vendor             string   `json:"vendor"`
	Tags               []string `json:"tags"`
	IsDefault          bool     `json:"isDefault"`
	MaxInputTokens     int64    `json:"maxInputTokens"`
	MaxOutputTokens    int64    `json:"maxOutputTokens"`
	Disabled           bool     `json:"disabled"`
	SupportsImages     bool     `json:"supportsImages"`
	SupportsReasoning  bool     `json:"supportsReasoning"`
	SupportsToolCall   bool     `json:"supportsToolCall"`
	CanDisableThinking bool     `json:"canDisableThinking"`
	Reasoning          struct {
		Effort           string   `json:"effort"`
		SupportedEfforts []string `json:"supportedEfforts"`
	} `json:"reasoning"`
}

// parseModels 解析模型目录响应（两域共用）：
// 以 cli agent 的 models 名单为准（该名单是客户端实际可用的对话模型面），
// 逐 id 从 data.models 取元数据；disabled 条目剔除；名单外的模型不透出
// （global 实测 25 个 models 中仅 22 个在 cli 名单内，其余非对话面）。
func parseModels(raw []byte) ([]ModelInfo, error) {
	var env modelsEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("models api code=%d", env.Code)
	}
	var cliIDs []string
	for _, ag := range env.Data.Agents {
		if ag.Name == "cli" {
			cliIDs = ag.Models
			break
		}
	}
	if len(cliIDs) == 0 {
		return nil, fmt.Errorf("no cli agent models found")
	}
	byID := make(map[string]modelEntry, len(env.Data.Models))
	for _, m := range env.Data.Models {
		byID[m.ID] = m
	}
	out := make([]ModelInfo, 0, len(cliIDs))
	for _, id := range cliIDs {
		m, ok := byID[id]
		if !ok || m.Disabled {
			continue
		}
		// 描述优先中文（面板/客户端面向中文用户），回落英文；都空则留空不编造。
		desc := m.DescriptionZh
		if strings.TrimSpace(desc) == "" {
			desc = m.DescriptionEn
		}
		out = append(out, ModelInfo{
			ID:                 m.ID,
			Name:               m.Name,
			ContextWindow:      m.MaxInputTokens,
			MaxTokens:          m.MaxOutputTokens,
			Efforts:            m.Reasoning.SupportedEfforts,
			Credits:            m.Credits,
			Description:        desc,
			Vendor:             m.Vendor,
			Tags:               m.Tags,
			IsDefault:          m.IsDefault,
			SupportsImages:     m.SupportsImages,
			SupportsReasoning:  m.SupportsReasoning,
			SupportsToolCall:   m.SupportsToolCall,
			CanDisableThinking: m.CanDisableThinking,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	return out, nil
}

// UserResource 查询账号当前可花费积分余额（所有套餐 CycleCapacity 聚合，负值钳 0）。
func (c *Client) UserResource(a *auth.Auth) (remain int64, err error) {
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	}
	data, err := c.billingJSON(a, http.MethodPost, billingMeterPath, body)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Response struct {
			Data struct {
				Accounts []struct {
					PackageName         string `json:"PackageName"`
					CapacitySize        int64  `json:"CapacitySize"`
					CapacityRemain      int64  `json:"CapacityRemain"`
					CapacityUsed        int64  `json:"CapacityUsed"`
					CycleCapacitySize   int64  `json:"CycleCapacitySize"`
					CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, fmt.Errorf("resource parse: %w", err)
	}
	for _, acct := range resp.Response.Data.Accounts {
		var r int64
		switch {
		case acct.CycleCapacitySize > 0:
			r = acct.CycleCapacityRemain
		case acct.CycleCapacityRemain > 0 || acct.CycleCapacityUsed > 0:
			r = acct.CycleCapacityRemain
		default:
			r = acct.CapacityRemain
		}
		if r < 0 {
			r = 0
		}
		remain += r
	}
	return remain, nil
}

// DailyCheckin 执行每日签到。已签到（业务 code 非 0）也返回错误，调用方按 msg 区分。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	_, err := c.billingJSON(a, http.MethodPost, dailyCheckinPath, map[string]any{})
	return err
}

// IsAlreadyCheckin 报告 err 是否表示"今天已签到"（上游幂等拒绝重复签到）。
// 只认带分类的 *Error（业务 code 或 HTTP 错误）：网络层/解析层错误不得当作幂等成功，
// 否则停机补签遇到抖动会误记为 already，账号当天实际未签到却被判定正常。
func IsAlreadyCheckin(err error) bool {
	var ue *Error
	if !errors.As(err, &ue) {
		return false
	}
	for _, m := range alreadyCheckinMarkers {
		if strings.Contains(ue.Msg, m) || strings.Contains(strings.ToLower(ue.Msg), strings.ToLower(m)) {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	return logfmt.Truncate(s, n)
}
