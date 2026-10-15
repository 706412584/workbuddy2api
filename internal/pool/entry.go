// Package pool 账号池：单一状态机（健康/冷却/熔断）+ 在途租约 + 三因子加权挑选 + state.json 持久化。
package pool

import (
	"sync/atomic"
	"time"

	"workbuddy2api/internal/auth"
)

type CoolKind int

const (
	CoolHard CoolKind = iota // 余额不足 → 冷却到次日 04:00（等签到恢复）
	CoolSoft                 // 429 → 短冷却
	// CoolSafety 403 + 11140 内容安全封控（ErrSafetyBanned）→ 长冷却。
	//
	// 与 CoolHard 的共性是「长且不参与指数退避」，但两者的**恢复出口不同**：
	//   - CoolHard 由签到解冻（ReenableIfCredits）：余额恢复 = 账号恢复；
	//   - CoolSafety 不被签到解冻——余额与内容安全风控无关，签到成功不能证明
	//     该号 chat 通道已解封。恢复出口只剩「冷却到期」「面板重置」「测试连接通过」。
	// 同样不参与全冷却兜底（pickEarliestExpiryLocked 与 CoolHard 一同排除）：
	// 兜底选到它只会再吃一个 403，白费一轮并放大请求量。
	CoolSafety
)

func (k CoolKind) String() string {
	switch k {
	case CoolHard:
		return "hard_credit"
	case CoolSoft:
		return "soft_rate"
	case CoolSafety:
		return "safety_banned"
	}
	return "unknown"
}

// Status 单个账号对外暴露的状态（脱敏）。
type Status struct {
	UID             string    `json:"uid"`
	Region          string    `json:"region,omitempty"` // 账号所属上游区域（"cn"/"global"），由凭证 domain 推导
	Nickname        string    `json:"nickname,omitempty"`
	Credits         int64     `json:"credits"`
	Cooling         bool      `json:"cooling"`
	CoolKind        string    `json:"cool_kind,omitempty"`
	CoolRemaining   int64     `json:"cool_remaining_sec,omitempty"`
	Until           time.Time `json:"until,omitempty"`
	Reason          string    `json:"reason,omitempty"`
	SoftStreak      int       `json:"soft_streak,omitempty"` // 连续软冷却次数（指数退避指数，见 entry.softStreak）
	// ModelCools 仍在生效的**模型级**冷却明细（见 entry.modelCools）。
	// 账号级冷却只反映在 Until/CoolKind 上，模型级冷却不写 Until —— 面板若只看
	// Cooling/Until 会把「只在该模型被限流」的账号显示成正常，故单列出来供展示。
	ModelCools      []ModelCoolStatus `json:"model_cools,omitempty"`
	Disabled        bool              `json:"disabled"`
	DisabledReason  string    `json:"disabled_reason,omitempty"` // 仅 disabled 账号：禁用原因（运维可见）
	SuccessCount    int64     `json:"success_count,omitempty"`
	ErrTotal        int64     `json:"err_total,omitempty"`
	LastSuccessTime time.Time `json:"last_success,omitempty"`
	LastErrTime     time.Time `json:"last_err,omitempty"`
	// 运行态（不持久化）：在途请求数 + 熔断器状态。
	InFlight     int       `json:"in_flight"`
	BreakerFails int       `json:"breaker_fails"`
	BreakerUntil time.Time `json:"breaker_until,omitempty"`
}

// ModelCoolStatus 单个模型级冷却的对外展示形态（脱敏）。
type ModelCoolStatus struct {
	Model         string    `json:"model"`
	Kind          string    `json:"kind"`
	Reason        string    `json:"reason,omitempty"`
	Until         time.Time `json:"until"`
	RemainingSec  int64     `json:"remaining_sec"`
	SoftStreak    int       `json:"soft_streak,omitempty"`
}

// modelCool 单个 (账号, 模型) 的冷却记账。
//
// 上游的错误大多**只对一个模型成立**：14018（该模型付不起）、6004（该模型限流）、
// WAF 403（该模型的频控）。把这类失败记成账号级会把「跑不起 hy4-preview 但
// 跑得动 deepseek-v4.1-flash」的账号整体封停，进而让没出问题的模型也 503。
// 故按 (账号, 模型) 二元组记账——与 sub2api 的 account+model transient 同构。
type modelCool struct {
	until  time.Time
	kind   CoolKind
	reason string
	// streak 该模型上的连续软限流次数（指数退避）。按模型独立计数：
	// 在 A 模型上被限流不应放大 B 模型的退避时长。
	streak int
}
type entry struct {
	a            *auth.Auth
	credits      int64
	successCount int64     // 累计成功
	errTotal     int64     // 累计错误（供成功率权重 successRate = successCount/(successCount+errTotal)，不清零）
	lastErr      time.Time // 最近一次错误时间
	lastSuccess  time.Time // 最近一次成功时间
	coolKind     CoolKind
	until        time.Time // 冷却截止（即时冷却：CoolSoft 429 / CoolHard 余额耗尽）
	disabled     bool
	reason       string
	lastUsed     time.Time // 最近被选中时刻（防并发撞号）
	// breakerUntil / fails / retryCount 为熔断器运行态（不持久化）。
	// fails 是唯一的"连续失败"计数器：任何错误喂入，达到 breakerThreshold 触发熔断（指数退避），
	// 跨入口累计，成功/熔断/统一复活时清零（保留 retryCount 驱动退避指数）。
	breakerUntil time.Time // 熔断截止（指数退避）
	fails        int       // 连续失败计数（熔断用，唯一权威）
	retryCount   int       // 已熔断次数（指数退避的指数）
	// softStreak 连续软冷却次数（CoolSoft），独立于熔断器 fails 的**冷却域**计数器：
	// fails 会被熔断触发清零、且被 hard 冷却与 NoteError 污染，无法表达"连续软限流"。
	// 重置点只有两处（都是账号被证明恢复的时刻）：NoteSuccess、reviveCoolingLocked。
	// 持久化（stateAccount.SoftStreak）：重启后软限流仍在退避，不因重启回到基数。
	softStreak int
	// modelCools 模型级冷却记账：(模型 → 记账)。键存在即代表该模型在冷却中。
	//
	// 覆盖 14018（付不起）、6004（限流）、WAF 403、404 等**模型/路由维**失败。
	// 账号级 until/coolKind 只留给「账号真的坏了」的形态（11140 内容封控）；
	// 账号级冷却一旦施加，模型级记账整体作废（见 CooldownSafetyBanned）。
	//
	// 运行态语义（不持久化）：重启清零，退化为「全部模型可用」，靠真实请求重新学习。
	// 这在两难里选了 fail-open 一侧——多打一次注定失败的请求，好过重启后把
	// 明明能跑的模型也一并封住（旧实现把 6004 的 until 持久化成账号级，正是后者）。
	modelCools map[string]*modelCool
	// sessionDeadFails 连续 12153（ErrSessionDead）计数。12153 在真实环境会被临时性触发
	// （网络抖动/上游闪断/refresh 竞态），一次失败就永久禁用太粗暴——连续达到阈值才判死。
	// 运行态语义（不持久化，与 inFlight 同语义）：重启清零可接受——重启后首个 keepalive
	// 成功即清计数，误判号不会因重启前的历史累积被继续追杀。
	sessionDeadFails int
	// safetyBannedFails 连续 ErrSafetyBanned（403 + 11140 + request illegal）计数，
	// 语义与 sessionDeadFails 同构：内容审核可能按账号判（账号级终态，该罚），
	// 也可能被某条用户内容触发（对所有账号都命中）——后者若一次即长冷却，
	// 一个用户就能在几分钟内把整池账号逐个罚停。故连续 safetyBannedThreshold
	// 次才判为账号级。运行态语义（不持久化，与 inFlight/sessionDeadFails 同）。
	// 由 pool.NoteSafetyBanned 累计，见 state.go。
	safetyBannedFails int
	// failEWMA 近期失败率（EWMA，alpha=failEWMAAlpha，运行态不持久化），**按模型**记账。
	// 初值 0 = "尚未观测到失败" = 不降权，与 sub2api 的 openAIAccountRuntimeStat
	// 从零值起步一致（首次失败经 alpha 混合后为 0.2，而非 1.0——避免一次抖动就把
	// 账号打到最低权重）。
	//
	// 与熔断器 fails 的分工：fails 是"连续失败计数"，达阈值即熔断（惩罚）；failEWMA
	// 是"近期失败占比"，只用于**降权**（soft demotion），永不暂停账号。二者刻意分离
	// ——上游整体故障时把健康号逐批熔断是错的（见 handler.applyErrorPolicy 的 ErrServer
	// 分支），但"这次失败"仍是真实的选号信号，丢掉它会让同一个号被反复撞。
	//
	// 关键设计：**每次尝试都上报**（成功上报 1、失败上报 0），与是否记账冷却无关。
	// 这正是 sub2api openai_account_scheduler.go 的 openAIAccountRuntimeStats.report
	// 口径：降权与惩罚解耦。500/transport error 这类"不罚号"的分类也照常降权。
	//
	// **为什么按模型记账**（与 modelCools / modelCost 同一哲学）：上游失败绝大多数
	// 只对一个模型成立。若记成账号级，在 glm-5.3 上被 6004 限流的号会被连带到
	// hy3-x 上一起降权——而上游明说 "you can switch to the other models"。
	// 实测回归：TestChat6004ModelResetCoolsToParsedTime 断言换模型后该号应被优先
	// 选中，账号级降权会让它被错误跳过。
	//
	// 键不存在 = 该模型无观测（不降权）；model 为空（无模型信息的路径，如 models
	// 拉取/探活）时退化到 "" 键，即"账号级"语义，与既有无模型路径口径一致。
	//
	// 不持久化：重启清零可接受——重启后头几次请求无偏好，靠真实流量重新学习
	// （与 modelCost 同语义）。
	failEWMA map[string]float64
	// failEWMATime 各模型最近一次上报时刻，供时间衰减计算（与 failEWMA 同键）。
	failEWMATime map[string]time.Time
	// inFlight 单账号在途请求数（运行态，不持久化）。用 atomic 避免 Pick 热路径拿写锁。
	inFlight atomic.Int64

	// modelCost 实测扣费账本：model → 观测（运行态，不持久化）。
	// 由每次成功请求的 usage.credit 折算而来（上游没有"按模型的用量"接口，
	// get-user-resource 只给套餐级积分汇总，只能实测）。选号时据此把
	// 「该模型上免费/便宜的号」排在前面。
	modelCost map[string]modelCostEntry
}

// modelCostOf 返回该账号在指定 model 上的有效成本观测；无观测或观测过期返回 ok=false。
func (e *entry) modelCostOf(model string, now time.Time) (modelCostEntry, bool) {
	if model == "" {
		return modelCostEntry{}, false
	}
	mc, ok := e.modelCost[model]
	if !ok || mc.LastSeen.IsZero() {
		return modelCostEntry{}, false
	}
	if now.Sub(mc.LastSeen) > modelCostTTL {
		return modelCostEntry{}, false // 过期：时段性优惠（夜间免费）不得跨时段生效
	}
	return mc, true
}

// healthy 报告账号当前是否可选（未禁用、未处于任一**账号级**冷却/熔断期）。
// 模型级冷却不在此判定：它对部分模型可用，由 healthyForModel 收口。
func (e *entry) healthy(now time.Time) bool {
	if e.disabled {
		return false
	}
	if !e.until.IsZero() && now.Before(e.until) {
		return false
	}
	if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) {
		return false
	}
	return true
}

// coolingOnModel 报告账号在指定模型上是否处于冷却中。
// model 为空表示调用方没有模型信息，此时不做模型维过滤（返回 false）——
// 无模型信息的路径（models 拉取、探活）不该因某个模型的限额而失去候选。
func (e *entry) coolingOnModel(model string, now time.Time) bool {
	if model == "" {
		return false
	}
	mc := e.modelCools[model]
	return mc != nil && !mc.until.IsZero() && now.Before(mc.until)
}

// healthyForModel 报告账号对指定 model 是否可选：
// 账号级健康（未禁用/未账号级冷却/未熔断）**且**该模型未在冷却中。
//
// 模型级豁免的由来（issue #31，本函数原为 6004 专用，现覆盖全部模型维失败）：
// 上游明说 "you can switch to the other models to continue using it"——
// 在 A 模型被限流/付不起，不代表账号在 B 模型不可用。空 reqModel / 未记录模型
// → 退化为 healthy(now)。
func (e *entry) healthyForModel(now time.Time, reqModel string) bool {
	return e.healthy(now) && !e.coolingOnModel(reqModel, now)
}

// expiry 返回账号当前仍在生效的最近冷却/熔断截止时间（账号级与指定模型级取较早者）；
// 不在冷却期返回零值。供全冷却兜底选取"最早到期"账号用。
func (e *entry) expiry(now time.Time, model string) time.Time {
	var t time.Time
	consider := func(cand time.Time) {
		if cand.IsZero() || !now.Before(cand) {
			return
		}
		if t.IsZero() || cand.Before(t) {
			t = cand
		}
	}
	if !e.until.IsZero() {
		consider(e.until)
	}
	if !e.breakerUntil.IsZero() {
		consider(e.breakerUntil)
	}
	if mc := e.modelCools[model]; mc != nil {
		consider(mc.until)
	}
	return t
}

// fallbackKind 报告兜底账号属于哪一类冷却（soft：即时软冷却；breaker：熔断期）。
// 只对参与兜底的账号调用（CoolSafety 已被 pickEarliestExpiryLocked 排除）。判定口径：
// 若熔断截止是当前生效的最近截止（含"仅有熔断无软冷却"），记为 breaker；否则记为 soft。
func (e *entry) fallbackKind(now time.Time, model string) string {
	exp := e.expiry(now, model)
	if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) &&
		(exp.IsZero() || !exp.Before(e.breakerUntil)) {
		return "breaker"
	}
	return "soft"
}

// stateAccount 单个账号的持久化状态（JSON tag 全小写下划线，向后兼容：缺字段零值）。
type stateAccount struct {
	Credits      int64     `json:"credits"`
	Disabled     bool      `json:"disabled"`
	Reason       string    `json:"reason,omitempty"`
	Until        time.Time `json:"until,omitempty"`
	CoolKind     CoolKind  `json:"cool_kind"`
	SuccessCount int64     `json:"success_count,omitempty"`
	// err_total 累计错误计数。旧版 err_count（连续错误）仍可读：加载时映射到 err_total，
	// 仅作一次性迁移，不再回写 err_count。
	ErrTotal    int64     `json:"err_total,omitempty"`
	ErrCount    int       `json:"err_count,omitempty"` // 兼容旧文件的迁移源，仅读取
	LastSuccess time.Time `json:"last_success,omitempty"`
	LastErr     time.Time `json:"last_err,omitempty"`
	// SoftStreak 连续软冷却次数（软退避指数）。旧 state.json 缺此字段 → 零值，
	// 退避从基数重新开始（向后兼容）。
	SoftStreak int `json:"soft_streak,omitempty"`

	// ModelCost 实测扣费账本（model → 观测）。仅内存态，重启后重新学习：
	// 成本会随上游活动（限免期/夜间免费/折扣）变化，持久化旧值反而是脏数据。
	ModelCost map[string]stateModelCost `json:"-"`
}

// stateModelCost 单个 (账号, 模型) 的实测成本观测。
type stateModelCost struct {
	// CostPer1k 每千 token 的 credit 消耗（EMA 平滑）。0 = 免费。
	// 用"每千 token"归一而非"单次 credit"：扣费随请求长度变化，
	// 不同长度的请求之间不可比。
	CostPer1k float64
	// LastSeen 最近观测时刻，超过 modelCostTTL 视为失效。
	LastSeen time.Time
	// Samples 观测次数（供排查）。
	Samples int
}

// modelCostTTL 成本观测的有效期。取 6 小时：既覆盖"夜间免费"这类时段性优惠的
// 单次会话，又不至于让昨天的价格决定今天的选择——过期的免费观测若永久有效，
// 白天会把已开始收费的号继续当成免费。
const modelCostTTL = 6 * time.Hour

// modelCostEntry 运行时成本账本（与 stateModelCost 同构，独立于持久化结构，
// 避免账本污染 state.json）。
type modelCostEntry struct {
	CostPer1k float64
	LastSeen  time.Time
	Samples   int
}

// stateFile 持久化格式。
type stateFile struct {
	Accounts map[string]stateAccount `json:"accounts"`
}

// flushInterval 后台落盘周期。
const (
	defaultBreakerThreshold   = 3
	defaultBreakerCooldown    = 30 * time.Minute
	defaultBreakerCooldownMax = 6 * time.Hour
)

// defaultSoftRateMax 软冷却指数退避的默认封顶：softRateMax 未注入（<=0）时按此值算，
// 避免测试/裸用池时退避无上限。
const defaultSoftRateMax = 2 * time.Hour

// sessionDeadThreshold 连续 ErrSessionDead（12153）达到该次数才永久禁用。
// 12153 会被临时性触发（网络抖动/上游闪断/refresh 竞态），一次失败即禁用的旧行为
// 会误杀健康账号（P0-1：13 个 disabled 号全是误判）。3 次连续才判死：容忍偶发抖动，
// 又不会让真正的死 session 留在池里反复被选中。
const sessionDeadThreshold = 3

// sessionDeadReason 12153 判定为 session 死亡时的持久化 reason。
const sessionDeadReason = "12153 session dead"

// safetyBannedThreshold 连续 ErrSafetyBanned（403 + 11140 + request illegal）达到该次数
// 才判为账号级内容安全封控。取 3 与 sessionDeadThreshold 同口径：既容忍「某条用户内容
// 触发审核」这类请求侧命中（不因一两次误伤杀号），又能让真正的账号级封控在几次尝试内
// 被摘出池子。注意尝试次数不等于请求数——同一请求内 `tried` 会排除已失败的号，
// 故阈值 3 通常需跨 3 个请求达成（其间该号由 45~75s 的短冷却挡在轮换之外）。
const safetyBannedThreshold = 3

// SessionDeadThreshold 暴露连续 12153 的禁用阈值（供 scheduler 日志/运维文档引用）。
func SessionDeadThreshold() int { return sessionDeadThreshold }

// SafetyBannedThreshold 暴露连续 11140 的判定阈值（供 server 日志引用）。
func SafetyBannedThreshold() int { return safetyBannedThreshold }

// softStreakShiftMax 软冷却退避的最大左移位数（防 1<<streak 溢出成负数/零）。
// 无论 streak 累积多少，封顶逻辑总会先生效，此值只是溢出兜底。
const softStreakShiftMax = 16

// StoreSnapshotter 池状态快照镜像的最小接口（redisstore.Store 满足；Noop 空实现安全）。
// 与本地 state.json 并存，作启动恢复备份：快照比本地新才采用，否则本地优先。
const (
	defaultIdleWeightPerHour = 0.5
	defaultIdleWeightMax     = 5.0
)

// failEWMAAlpha 近期失败率的 EWMA 平滑系数。
// 取 0.2（对齐 sub2api openAIAccountRuntimeStats.report 的 alpha）：约 5 次尝试收敛，
// 单次抖动不主导决策，连续失败则快速压低权重。
const failEWMAAlpha = 0.2

// failEWMAWindow 降权衰减半衰期语义的时间常量：EWMA 不含时间维，故失败率靠
// **成功请求自然冲淡**（每次成功上报 0）。账号长期不被选中时 failEWMA 不会自动回落，
// 但那种情况下它本来也不该被选中——真回到候选集时，首次成功即把率拉低。
// 这里额外用 failEWMADecay 在每次选号时做一步时间衰减，避免"冷却到期后仍背着
// 陈年失败率"：按半衰期 10 分钟衰减。
const failEWMADecayHalfLife = 10 * time.Minute

// failEWMANeutral 无观测时的中性值。
// 取 0：降权因子 = 1 - failEWMA，无观测即不降权。刻意不取 0.5——
// 新号/重启后的号应被平等对待，而不是先背上一个"疑似一半会失败"的预设。
const failEWMANeutral = 0.0

// failEWMAFreshThreshold "仍然新鲜"的失败率上限。超过它视为**近期刚失败**，
// 在选号排序里整档排到干净账号之后（见 pick 的 sort 键）。
//
// 为什么用整档排序而不只靠权重相乘：本仓库的权重里 credits 项 ×10，
// 而失败降权最多 ×0.8（一次失败），压不动 14 vs 9 的差距——实测 2026-10-15，
// 同一批号在 62s 周期上被反复选中，9 次失败/7479 次成功 在 lifetime 成功率里
// 只占 0.1%，权重毫无变化。整档排序让"刚失败"立刻让位给没失败的账号，
// 与用户诉求一致：优先能调的可用账号，而不是一直撞同一批。
//
// 取 0.15：单次失败经 alpha=0.2 混合后为 0.2 > 0.15 → 立刻降档；
// 之后靠 failEWMADecayHalfLife 衰减，约 4 分钟跌破阈值自动回到干净档。
const failEWMAFreshThreshold = 0.15

// New 构建池；stateFp 非空时尝试加载旧状态，并启动后台周期性落盘 goroutine。
