// Package headers 构造三类上游请求头（common / chat / billing / refresh）。
// 规则来自 docs/api-reference.md §0/§4/§6。
package upstream

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"workbuddy2api/internal/auth"
)

const (
	// defaultClientVersion 出站 WorkBuddy 客户端版本段（UA 的 `WorkBuddy/<ver>` 与
	// 白名单头组的 X-IDE-Version）。对齐官方 WorkBuddy Desktop 分发包版本
	// （resources/install-manifest.json 的 appVersion，以及 app.asar 内
	// package.json 的 version；2026-10-15 分发包 = 5.8.4）。
	// config upstream.client_version 可覆盖（空 = 内置默认）。
	//
	// 版本滞后本身不是封禁判据（2026-10-21 实测：同账号分别用 5.5.4 与 5.8.4 的 UA
	// 请求，200/403 结果完全一致），但对齐当前分发包可减少"旧客户端"归因面。
	defaultClientVersion = "5.8.4"
	// defaultCliVersion 出站 UA 中 `CLI/<ver>` 段版本。对齐官方内置 CLI
	// （同分发包 app.asar.unpacked/cli/dist/*.mjs 内嵌 package.json 的
	// publishConfig.customPackage.version = 2.161.0）。
	// config upstream.cli_version 可覆盖（空 = 内置默认）。
	defaultCliVersion = "2.161.0"

	originRefererCN   = "https://www.codebuddy.cn"
	originRefererGlob = "https://www.workbuddy.ai"
)

func originRefererFor(a *auth.Auth) string {
	if a != nil && a.Region() == auth.RegionGlobal {
		return originRefererGlob
	}
	return originRefererCN
}

// clientVersion 生效的 WorkBuddy 客户端版本：Client.ClientVersion 非空则取之，
// 否则内置默认 defaultClientVersion。
func (c *Client) clientVersion() string {
	if c != nil && c.ClientVersion != "" {
		return c.ClientVersion
	}
	return defaultClientVersion
}

// cliVersion 生效的 CLI 版本：Client.CliVersion 非空则取之，否则内置默认 defaultCliVersion。
func (c *Client) cliVersion() string {
	if c != nil && c.CliVersion != "" {
		return c.CliVersion
	}
	return defaultCliVersion
}

// defaultWorkBuddyUA 组装默认客户端出站 UA（官方桌面端 RestOperations 层形状）：
// `WorkBuddy/<clientVersion> WorkBuddy/<clientVersion> CLI/<cliVersion>`
// （step1 §1.3：applicationName/version + platform/version + CLI/<cliVersion>；
// SaaS 默认形态下 applicationName 与 platform 同为 WorkBuddy，两段相同）。
// 与旧值 `CLI/2.63.2 CodeBuddy/2.63.2` 的差异：CodeBuddy 老品牌换为 WorkBuddy 三段式，
// CLI 版本对齐官方内置 2.137.1。官方无任何 UA 随机化（step1 §4），故默认确定性。
func (c *Client) defaultWorkBuddyUA() string {
	return "WorkBuddy/" + c.clientVersion() + " WorkBuddy/" + c.clientVersion() + " CLI/" + c.cliVersion()
}

// userAgent 返回当前出站 UA（客户端出站路径：chat/refresh/FetchModels）。
// 优先级：Client.UserAgent（config user_agent）显式覆盖 > 默认 WorkBuddy 三段式。
// 显式覆盖兼容既有覆盖逻辑：用户配了即以用户值为准（自定义品牌/版本），
// 未配则走官方桌面端默认形态（本任务 A 段核心变更）。
func (c *Client) userAgent() string {
	if c != nil && c.UserAgent != "" {
		return c.UserAgent
	}
	return c.defaultWorkBuddyUA()
}

// billingUA 白名单类（billing/checkin/banner）出站 UA。
// 语义对齐官方 application-manifest.js:27590-27601（banner 白名单接口显式头组）：
// 这类接口用单段 `WorkBuddy/<clientVersion>`（不带 CLI 段——官方 banner 显式覆写 UA
// 为 `WorkBuddy/<pkgVer>`，RestOperations 层的 CLI 扩展段被业务层固化覆盖掉）。
// 仅当 client_name 配置（非空）才生效；未配保持现状（BillingHeaders 不设 UA，Go 默认 UA）。
func (c *Client) billingUA() string {
	if c == nil || c.ClientName == "" {
		return ""
	}
	return "WorkBuddy/" + c.clientVersion()
}

// resolveDeviceToken 解析本次请求的 X-Device-Token 取值。
// 优先级：auth.Auth.DeviceToken（每号）> Client.DeviceToken（config 全局）> 文件兜底。
// 三者皆空/读失败则返回空串（调用方不注入该头，优雅降级）。
// 为什么不放进 CommonHeaders：鉴权/刷新类头（refresh / FetchModels）给设备 token
// 无意义且可能被上游风控误判为异常客户端；只在 chat/billing 业务请求注入。
func (c *Client) resolveDeviceToken(a *auth.Auth) string {
	if a != nil && a.DeviceToken != "" {
		return a.DeviceToken
	}
	if c != nil && c.DeviceToken != "" {
		return c.DeviceToken
	}
	if c != nil && c.DeviceTokenFile != "" {
		return readDeviceTokenFile(c.DeviceTokenFile)
	}
	return ""
}

// injectDeviceToken 在 req 注入 X-Device-Token 头（仅当取到非空 token）。
func (c *Client) injectDeviceToken(req *http.Request, a *auth.Auth) {
	if tok := c.resolveDeviceToken(a); tok != "" {
		req.Header.Set("X-Device-Token", tok)
	}
}

// deriveAccountStableID 按 uid + 用途盐稳定派生 36 hex 设备/会话标识。
// 跨重启稳定（固定盐 "wb2a:"，不随进程换——这是与 session 包 deriveSalt 的本质差异：
// 那是会话键维度的进程级随机盐，重启换新；本函数是账号维度，必须跨重启恒定）、
// 账号间互异（uid 不同则不同）、同 uid 同用途恒同值（幂等）。对齐 hub
// wb_fingerprint.py:derive_id 的 md5(salt:uid)[:36] 语义。
// 用 sha256（非 md5）——项目既有派生（session/ids.go、cache_key.go）全用 sha256，
// 保持一致；截 36 hex 与 hub 的 [:36] 同形态（提供更长熵）。
//
// 两个用途：
//   - purpose="machine" → X-Machine-ID（设备级，跨会话稳定）
//   - purpose="session" → X-Session-ID（账号固定会话，跨重启稳定——hub 语义）
func deriveAccountStableID(uid, purpose string) string {
	sum := sha256.Sum256([]byte("wb2a:" + purpose + ":" + uid))
	return hex.EncodeToString(sum[:18]) // 36 hex chars
}

// injectAccountStableHeaders 在 req 注入 X-Machine-ID / X-Session-ID：按 uid 稳定
// 派生，跨重启固定、账号间互异。对齐官方桌面端「每账号一台固定虚拟设备」语义，
// 防多号被上游按设备指纹缺失/漂移关联风控。uid 为空时不注入（匿名请求无设备
// 标识，上游不要求）。
func (c *Client) injectAccountStableHeaders(req *http.Request, a *auth.Auth) {
	if a == nil || a.UID == "" {
		return
	}
	req.Header.Set("X-Machine-ID", deriveAccountStableID(a.UID, "machine"))
	req.Header.Set("X-Session-ID", deriveAccountStableID(a.UID, "session"))
}

// CommonHeaders 设置所有 API 共享的请求头。
func (c *Client) CommonHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	origin := originRefererFor(a)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", c.userAgent())
	// X-Machine-ID / X-Session-ID：按 uid 稳定派生的账号级设备头（见
	// injectAccountStableHeaders）。注入在 CommonHeaders 而非 ChatHeaders——hub 在
	// 所有出站路径的公共 headers() 注入（wb_accounts.py:250-251），本网关同样全覆盖
	// （chat 经 ChatHeaders 叠加 CommonHeaders 天然继承；billing 域另行注入）。
	c.injectAccountStableHeaders(req, a)
}

// ChatHeaders 在 common 之上加 chat 专属的账号头。
// 缺省字段用 X-No-* 约定（与 CodeBuddy 官方 CLI 一致）。
// clientIP 为本次请求的客户端 IP（按参数传递，不读共享字段——避免并发串扰）；
// PassthroughIP=false 或 clientIP 为空时不注入 IP 头。
// convID 为本次会话标识（见 injectConversationHeaders），空则由本函数生成一个。
func (c *Client) ChatHeaders(req *http.Request, a *auth.Auth, clientIP, convID string) {
	c.CommonHeaders(req, a)
	// AccessToken 加锁快照：keepalive 定时刷新会在 a.mu 内改写它，锁外直读构成数据竞争
	// （见 auth.AccessTokenValue 注释）。
	if at := a.AccessTokenValue(); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		// X-Tenant-Id：官方 CLI 恒与 X-Enterprise-Id 成对发（axiosToFetchAdapter 与
		// /main/server.js 的 buildVlmGatewayHeaders 均如此），企业身份的两个同义头。
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	// X-Private-Data：官方 YN 常量组的常开项（applyGatewayHeaders 无条件套用，
	// 仅自定义模型请求会删掉），表达"允许把私有代码上下文发给模型"。
	req.Header.Set("X-Private-Data", "true")
	// 安全红线：绝不在 chat 请求里携带 X-Refresh-Token。
	if d := a.DomainValue(); d != "" {
		req.Header.Set("X-Domain", d)
	} else {
		req.Header.Set("X-No-Department-Info", "1")
	}
	// 用量归属头：真实桌面端发 X-Agent-Purpose="conversation" + X-IDE-Name/Type/X-Product
	// 识别 client，避免上游用量统计里 client/agentPurpose 为空。来源 xiaofan6ya/converter.py。
	// 默认（ClientName 空）保持 X-Product="SaaS" 兼容现状，不设 X-IDE-*（不突变归因）；
	// 配 ClientName（如 "WorkBuddy"）则四头跟随该值，对齐官方桌面端。
	c.injectAttribution(req)
	// 客户端指纹头：官方 CLI 对**所有**出站请求无条件注入的一批头
	// （x-codebuddy-request / X-Agent-Intent / trace 家族等，见 injectClientFingerprint）。
	// 我们此前完全不发这批头，是"请求不像官方客户端"最明显的缺口。
	c.injectClientFingerprint(req)
	// 会话三头：上游 2026-10-16 起强制校验（见 injectConversationHeaders）。
	c.injectConversationHeaders(req, convID)
	// 客户端 IP 透传：仅当 PassthroughIP=true 且本次请求 clientIP 参数非空（见 handler 设置）。
	// 缺省 false（反代安全边界：不把内网/代理 IP 暴露给上游）。
	c.injectClientIP(req, clientIP)
	// 设备风控头：auth 每号 > config 全局 > 文件兜底；空则不注入（见 resolveDeviceToken）。
	c.injectDeviceToken(req, a)
}

// injectClientFingerprint 注入官方 WorkBuddy CLI 对所有出站请求无条件携带的
// 「客户端指纹」头组。
//
// 来源（2026-10-21 从客户端 5.8.4 分发包逆向，两处独立证据）：
//
//  1. app.asar 内 /main/server.js 的 applyGatewayHeaders()（VLM 网关路径）：
//     x-codebuddy-request=1 / X-Agent-Intent=craft / acp-connection-id /
//     X-Request-ID / traceparent + b3 + X-B3-* + X-Trace-ID。
//  2. app.asar.unpacked/cli/dist/*.mjs 的 axiosToFetchAdapter()（CLI 对
//     **每一个** axios 请求的拦截器，即 chat 主路径）：
//     X-Agent-Intent=craft（或 session meta 的 codebuddy.ai/mode）、
//     X-Agent-Type=main|subagent|team|auxiliary|compaction、X-Request-ID、
//     X-Root-Request-ID、X-Compaction / X-Context-Compacted。
//     另 runIdentityHeaders() 注入 X-Auth-Method / X-Id-Source / X-Userinfo。
//
// 与 injectAttribution 的分工：归属头（X-Product/X-IDE-*）是**业务用量归因**，
// 官方只在配了 client_name 时全量发；本函数这批是 CLI 的**统一请求拦截器**，
// 官方**无条件**发，与 client_name 无关，故分开放。
//
// 实测边界（2026-10-21，25 个账号 A/B）：补齐这批头**不能**解除已发生的
// 403（账号级终态，换出口 IP 与换版本号 UA 同样无效），故它是**预防性对齐**，
// 不是解封手段。取确定性值而非随机值，避免引入新的"特征漂移"。
func (c *Client) injectClientFingerprint(req *http.Request) {
	// x-codebuddy-request=1：CLI 的 withSecurityHeader() 给所有请求打的标记，
	// 官方 worker 反代白名单里显式透传（只放行 authorization/cookie/x-codebuddy-request）。
	req.Header.Set("x-codebuddy-request", "1")
	// X-Agent-Intent：官方取自 session meta["codebuddy.ai/mode"]，普通会话回落 "craft"。
	req.Header.Set("X-Agent-Intent", "craft")
	// X-Agent-Type：网关是单会话直答，等价 resolveSessionAgentType 的 "main" 分支。
	req.Header.Set("X-Agent-Type", "main")
	// X-Request-ID：官方用本次 message id（UUID 打掉连字符）。这里独立生成，
	// 上游不校验其与 X-Conversation-* 的关系（见 injectConversationHeaders 的实测）。
	req.Header.Set("X-Request-ID", strings.ReplaceAll(newConversationID(), "-", ""))
	// 不需要 X-Root-Request-ID / X-Compaction / X-Context-Compacted：前者是子代理
	// 链路溯源（我们无子代理），后两者表达"本请求是否压缩轮"（网关不压缩上下文，
	// 发假值反而是错误信号）。
	injectTraceHeaders(req)
}

// injectTraceHeaders 注入 W3C + B3 双链路追踪头。
// 形状对齐官方 applyTraceHeaders() 与 injectOtelSpanHeaders()：16 字节 traceId、
// 8 字节 spanId（hex），traceparent 固定 sampled=01。
// 每次请求独立生成——追踪头的语义就是"本次请求"，复用反而错误。
func injectTraceHeaders(req *http.Request) {
	// traceId 16 字节（32 hex）、spanId 8 字节（16 hex）——W3C traceparent 的定长，
	// 与官方 randomBytes(16)/randomBytes(8) 一致。
	var tid [16]byte
	var sid [8]byte
	if _, err := rand.Read(tid[:]); err != nil {
		// crypto/rand 在受支持平台不会失败；退化固定值也好过缺头
		// （官方每个请求都带，缺失是特征差异）。
		copy(tid[:], "wb2a-trace-id-000")
	}
	if _, err := rand.Read(sid[:]); err != nil {
		copy(sid[:], "wb2a-spn")
	}
	traceID := hex.EncodeToString(tid[:])
	spanID := hex.EncodeToString(sid[:])
	req.Header.Set("traceparent", "00-"+traceID+"-"+spanID+"-01")
	req.Header.Set("b3", traceID+"-"+spanID+"-1")
	req.Header.Set("X-B3-TraceId", traceID)
	req.Header.Set("X-B3-SpanId", spanID)
	req.Header.Set("X-B3-Sampled", "1")
	req.Header.Set("X-Trace-ID", traceID)
}

// injectConversationHeaders 注入上游强制要求的三个会话头。
//
// 背景（2026-10-16 01:21 起全池 400 code=11-128）：上游给 /v2/chat/completions
// 新增了会话头校验，**三个头缺一不可**，且判定顺序分两档：
//
//	无 X-Conversation-ID          → msg="conversation id missing"
//	有 ID 但缺另外两个            → msg="request illegal"
//	三头齐全                      → 200
//
// 实测（2026-10-16，global 账号直连上游 A/B/C 三组）：
//
//	A. 不带头               → 400 conversation id missing
//	B. 仅 X-Conversation-ID → 400 request illegal
//	C. 三头齐全             → 200，且响应体的 id 字段回显 X-Conversation-Message-ID
//
// 只校验**存在性**，不校验格式与唯一性：UUID / "wb2api-xxx" / 纯数字 / 三个头同值
// 全部 200（实测 4 组）。因此这里只需保证非空即可，取值自由度留给上层
// （handler 传会话键派生值，使同一会话在上游看到稳定 ID，见 forwardChat）。
//
// convID 为空（如面板「测试连接」的一次性探测、scheduler 的上报路径）时现场生成
// 随机 UUID：上游要的只是"有值"，一次性请求没有会话连续性可言。
func (c *Client) injectConversationHeaders(req *http.Request, convID string) {
	if convID == "" {
		convID = newConversationID()
	}
	req.Header.Set("X-Conversation-ID", convID)
	// Request-ID 标识"这一轮请求"，Message-ID 标识"这一条消息"。
	// 上游不校验二者与 Conversation-ID 的关系，各自独立生成即可。
	req.Header.Set("X-Conversation-Request-ID", newConversationID())
	req.Header.Set("X-Conversation-Message-ID", newConversationID())
}

// newConversationID 生成一个会话标识（UUID v4 形态，与官方桌面端一致）。
// 用 crypto/rand：这些值会随请求出站，不应用可预测的伪随机源。
func newConversationID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 在受支持的平台上不会失败；真失败时退化为固定串也比空值好
		// （空值会被上游判 11-128，整请求失败）。
		return "wb2api-fallback-conversation-id"
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// injectAttribution 注入用量归属头（X-Agent-Purpose / X-IDE-* / X-Product）。
// 仅在 chat/completions 路径生效（ChatHeaders 调用）。
//
// 2026-10-21 修正：X-IDE-Name/Type 与 X-Agent-Purpose **改为无条件发送**。
// 官方客户端把它们放在 applyGatewayHeaders() 末尾的常量组里无条件套用
// （YN = [[X-IDE-Type,WorkBuddy],[X-IDE-Name,WorkBuddy],[X-IDE-Version,0.0.0],
// [X-Private-Data,true],[X-Product,SaaS]]，见 app.asar /main/server.js）。
// 此前我们只在配了 client_name 时才发，等于给上游一个"这批请求没带 IDE 标识"
// 的可区分特征——与本次 403 排查中"账号被单独标记"的方向一致，故对齐官方。
// ClientName 非空时仍以该值为准（自定义品牌/版本的能力保留）。
func (c *Client) injectAttribution(req *http.Request) {
	name := "WorkBuddy"
	product := "SaaS"
	if c != nil && c.ClientName != "" {
		name = c.ClientName
		product = c.ClientName
	}
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Name", name)
	req.Header.Set("X-IDE-Type", name)
	req.Header.Set("X-IDE-Version", c.clientVersion())
	req.Header.Set("X-Product", product)
}

// injectClientIP 在 PassthroughIP 开启时把 clientIP 参数透传给上游。
// 三个等价头（X-Forwarded-For/X-Real-IP/X-Client-IP）一并设，与桌面端透传一致。
// 按**参数传递**而非读共享字段：避免并发请求交叉污染对方 IP（issue：ClientIP 竞态）。
func (c *Client) injectClientIP(req *http.Request, clientIP string) {
	if c == nil || !c.PassthroughIP || clientIP == "" {
		return
	}
	req.Header.Set("X-Forwarded-For", clientIP)
	req.Header.Set("X-Real-IP", clientIP)
	req.Header.Set("X-Client-IP", clientIP)
}

// ExtractClientIP 从入站请求提取客户端 IP 首段（X-Forwarded-For 首段，回落 X-Real-IP）。
// 供 handler 在 PassthroughIP 开启时按请求取值后传入 ChatStream（chat 路径专属，不跨请求）。
// 取不到返回空串（handler 据此跳过透传）。
func ExtractClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for i := 0; i < len(xff); i++ {
			// 取逗号前首段并 trim 空白。
			if xff[i] == ',' {
				return strings.TrimSpace(xff[:i])
			}
		}
		return strings.TrimSpace(xff)
	}
	if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
		return real
	}
	return ""
}

// BillingHeaders billing 接口请求头。
// UA 语义（A 段对齐官方白名单头组，application-manifest.js:27590-27601）：
//  1. 显式配置 c.UserAgent 优先（用户自定义值，全路径生效）；
//  2. 未配但 client_name 非空 → 单段 `WorkBuddy/<clientVersion>`（官方 banner/check-in
//     显式覆写 UA 的形态，不带 CLI 段）；
//  3. 两者皆空 → 保持现状不设置（Go 客户端自带默认 UA）。
func (c *Client) BillingHeaders(req *http.Request, a *auth.Auth) {
	// AccessToken 加锁快照（同 ChatHeaders：keepalive 可在 a.mu 内改写）。
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if c != nil && c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	} else if ua := c.billingUA(); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if d := a.DomainValue(); d != "" {
		req.Header.Set("X-Domain", d)
	}
	// 设备风控头：billing 域（report/travel/balance/checkin）同样注入（见 resolveDeviceToken）。
	c.injectDeviceToken(req, a)
}

// RefreshHeaders refresh 端点专属头（X-Refresh-Token 只允许出现在这里）。
func (c *Client) RefreshHeaders(req *http.Request, a *auth.Auth) {
	c.CommonHeaders(req, a)
	req.Header.Set("X-Refresh-Token", a.RefreshToken)
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	}
	req.Header.Set("X-Auth-Refresh-Source", "workbuddy")
}
