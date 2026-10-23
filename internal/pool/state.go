// 账号状态演进与查询：禁用/12153 连续计数判定、成功与错误入账、复活解冻，
// 以及状态查询（Status/AvailableUIDs/PickByUID/CountsDetailed/ServableNow/List）。
package pool

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
)

// Disable 禁用账号（自动禁用：session 死亡等故障态）。
// 人工禁用请用 DisableManual —— 二者在「测试通过能否自动解除」上语义不同。
func (p *Pool) Disable(uid, reason string) {
	p.disableWith(uid, reason, false)
}

// DisableManual 人工禁用（面板 toggle）：标记来源为人工，
// 使 NoteTestOK 不会在「测试连接」通过时把它自动解除。
//
// 为什么人工禁用要区别对待：「测试通过」只证明**凭证/session 有效**，
// 而人工禁用表达的是**运维决定**（比如这个号要留作他用、或正在排查问题）。
// 二者不在同一维度，测试结果不构成推翻人工决定的依据。
// 解除人工禁用只有两条显式路径：面板 toggle 启用、或「重置」勾选解除禁用。
func (p *Pool) DisableManual(uid, reason string) {
	p.disableWith(uid, reason, true)
}

// disableWith 两种禁用的共用实现。manual 决定来源标记。
func (p *Pool) disableWith(uid, reason string, manual bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = true
		e.reason = reason
		e.disabledByManual = manual
		p.dirty.Store(true)
	}
}

// NoteSessionDead 记录一次 ErrSessionDead（12153）——**不立即禁用**。
// 旧行为一次 12153 即 Disable，但 12153 会被临时性触发（网络抖动/上游闪断/refresh
// 竞态），一次失败就永久杀号会误杀健康账号（P0-1 侦察：13 个 disabled 号全部 refresh
// 成功，是历史误判的受害者）。改为连续 sessionDeadThreshold 次才禁用：
// 计数 +1，达到阈值 → Disable（reason=12153 session dead）并清计数；
// refresh 成功 / 任意成功 / 手工复活 → ClearSessionDead 清计数。
// 返回 true 表示本次已达阈值并完成禁用。
// 即使账号已 disabled，计数仍累计并返回 false 前 N-1 次——但 keepalive 会跳过
// disabled 号，实际只有「已 disabled 后复活且计数未清」这类场景才会走到这里。
func (p *Pool) NoteSessionDead(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.sessionDeadFails++
	if e.sessionDeadFails < sessionDeadThreshold {
		return false
	}
	e.disabled = true
	e.reason = sessionDeadReason
	e.sessionDeadFails = 0
	p.dirty.Store(true)
	return true
}

// ClearSessionDead 清连续 12153 计数——账号被证明未死的任何时刻调用：
// refresh 成功（RunKeepaliveNow）、chat 成功（NoteSuccess）、手工复活（ReviveDisabled）。
func (p *Pool) ClearSessionDead(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.sessionDeadFails = 0
	}
}

// NoteSafetyBanned 记录一次 ErrSafetyBanned（403 + 11140 + request illegal）连续计数。
//
// **为什么不一次就上长冷却**：上游该响应的 displayMsg 是「内容未通过安全审核」，
// 而内容审核同时存在「按账号判」与「按内容判」两种触发面。2026-09-27 实测证明前者
// 存在（同一 body 换健康账号即 200、最小探针同样 403、重登不解除）；但后者同样可能
// ——若某条用户内容对所有账号都触发审核，一次即长冷却会让**一个用户**在几分钟内
// 把整个池逐个罚停（每次请求可轮换到 MaxRotate 个账号）。
// 故与 sessionDeadFails 同构：连续累计到 safetyBannedThreshold 才判为账号级终态，
// 期间用短冷却把该号暂时移出轮换，既不放大请求量，也不因单条内容误伤整池。
//
// 返回 true 表示本次已达阈值（调用方据此施加长冷却）；阈值达成后计数清零，
// 下一轮从零重新累计（若长冷却到期后仍持续命中，会再次快速触顶）。
func (p *Pool) NoteSafetyBanned(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.safetyBannedFails++
	if e.safetyBannedFails < safetyBannedThreshold {
		return false
	}
	e.safetyBannedFails = 0
	return true
}

// ClearSafetyBanned 清连续 11140 计数——账号被证明 chat 通道可用的时刻调用：
// chat 成功（NoteSuccess）、面板测试通过（NoteTestOK）、面板重置（Reset）、
// 人工复活（ReviveDisabled）。**不含** token refresh：刷新只证明凭证有效，
// 与内容安全风控无关（签到/余额接口对该形态全程正常，见 ReenableIfCredits 注释）。
func (p *Pool) ClearSafetyBanned(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.safetyBannedFails = 0
	}
}

// ReviveDisabled 人工/端点复活入口：清除 disabled + reason + 连续 12153 计数 + 连续 11140 计数，
// 账号回到池子（若无其他冷却/熔断则立即可选，健康检查自然接管）。
// **不改** Disabled 在选号/状态端点的既有语义：disabled 号依然不参与选号，
// 直到被本方法复活。不存在的 uid 为空操作。
func (p *Pool) ReviveDisabled(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok && e.disabled {
		e.disabled = false
		e.disabledByManual = false
		e.reason = ""
		e.sessionDeadFails = 0
		e.safetyBannedFails = 0
		p.dirty.Store(true)
	}
}

// reviveCoolingLocked 只清冷却（until/coolKind/reason/softStreak）并更新 credits，不动熔断器
// （fails/retryCount/breakerUntil）。签到解冻走这里：签到成功只证明余额恢复与
// billing 通道健康，不证明 chat 通道健康，熔断（连续 5xx 信号）不应被签到覆盖。
// softStreak 属**冷却域**（与 until/coolKind 同域），故随冷却一并清零——与"解冻只清冷却
// 不清熔断"的既有 C5 语义一致；硬冷却（CoolHard）本就不参与 streak，这里清的是历史软冷却累积。
//
// **CoolSafety 例外**：内容安全封控（403 + 11140）与余额无关，签到/余额接口全程正常
// 也可能持续被拒（2026-09-27 实测该号签到正常、chat 连续 403 达 15 小时）。若在此
// 一并清掉，签到任务每天 09:00/21:00 会把封控号重新放回池中，恰好制造它最擅长的
// 那类失败。故保留其冷却，等它自然到期或由面板重置/测试连接通过解除。
// 调用方必须已持有 p.mu。
func (p *Pool) ReenableIfCredits(uid string, remain int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		stillCooling := e.coolKind == CoolSafety && !e.until.IsZero() && time.Now().Before(e.until)
		if remain > 0 && !e.disabled && !stillCooling {
			p.reviveCoolingLocked(e, remain)
		} else {
			e.credits = remain
		}
		p.dirty.Store(true)
	}
}

// NoteError 记录一次错误：喂入唯一的连续失败计数器 fails + 累计错误 errTotal。
// 达到 breakerThreshold 触发熔断（指数退避），连续失败语义整体并入熔断器（不再有独立的 err 冷却）。
func (p *Pool) NoteError(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errTotal++
		e.lastErr = time.Now()
		p.recordBreakerFailureLocked(e)
		p.dirty.Store(true)
	}
}

// NoteAttempt 上报一次**尝试结果**到近期失败率（EWMA），用于选号降权。
//
// 与 NoteError 的分工：NoteError 是"记账惩罚"（累计 errTotal + 喂熔断），只在
// 该分类确实该罚账号时调用；NoteAttempt 是"学习信号"，**每次尝试都该调用**，
// 包括那些刻意不罚号的分类（5xx 上游故障、400 参数错误、内容拦截误报）。
//
// 为什么必须分开：applyErrorPolicy 对 ErrServer 明确"不罚账号"（上游故障时熔断
// 健康号是错的，见该分支注释），但代价是连"这个号刚刚失败过"的信号都丢了——
// 于是同一批号被反复撞。实测 2026-10-15：4.1 返 500 时 22 个号里只有 5 个有积分
// 因而进得了候选，这 5 个被逐轮撞满，请求卡 184~190s 后 503。
//
// model 参与记账键（与 modelCools / modelCost 同口径）：上游失败绝大多数只对一个
// 模型成立，在 A 模型失败不该连带到 B 模型——上游文案明说 "you can switch to the
// other models"。model 为空时退化到 "" 键（账号级语义，与无模型信息的路径一致）。
//
// success 语义：true = 本次尝试成功（上报 0），false = 失败（上报 1）。
// 与 sub2api openai_account_scheduler.go 的 openAIAccountRuntimeStats.report 同口径。
func (p *Pool) NoteAttempt(uid, model string, success bool) {
	if uid == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	if e.failEWMA == nil {
		e.failEWMA = make(map[string]float64)
		e.failEWMATime = make(map[string]time.Time)
	}
	sample := 1.0
	if success {
		sample = 0.0
	}
	// 从零值起步直接混合（与 sub2api updateEWMAAtomic 同口径）：首次失败 → 0.2，
	// 首次成功 → 仍为 0。不做"首样本直接取 1.0"的特例——一次偶发抖动不该把账号
	// 直接压到最低权重，EWMA 的意义正是让单次样本只有 alpha 的份量。
	e.failEWMA[model] = failEWMAAlpha*sample + (1-failEWMAAlpha)*e.failEWMA[model]
	e.failEWMATime[model] = time.Now()
}

// failEWMANow 返回该账号在指定模型上的近期失败率经时间衰减后的**生效值**
// （纯读取，不修改 e）。model 为空时读 "" 键（账号级语义，与 NoteAttempt 记账键一致）。
//
// 为什么需要衰减：EWMA 没有时间维，失败率只能靠后续成功冲淡。若一个号失败后被冷却
// 或长期闲置，failEWMA 会一直停在低位，等它重新回到候选集时仍背着陈年失败率——
// 而当时的失败原因（上游瞬时故障/限流）多半早已消失。按经过时间做一步半衰期衰减，
// 让"很久以前的失败"自然失效。
//
// 为什么是纯函数而非就地写：weightOf 在 pick 的写锁内、以及测试 helper 的读锁内
// 都会被调用。就地写会让"只读路径改状态"，破坏锁纪律。衰减只影响**本次打分**，
// 下一次 NoteAttempt 会用真实样本覆盖，无需持久化回去。
//
// 调用方需已持 p.mu（读或写均可）。now 由调用方统一取一次，避免热路径多次 time.Now。
func (e *entry) failEWMANow(model string, now time.Time) float64 {
	v, ok := e.failEWMA[model]
	if !ok || v <= 0 {
		return failEWMANeutral
	}
	ref := e.failEWMATime[model]
	if ref.IsZero() {
		return v
	}
	elapsed := now.Sub(ref)
	if elapsed <= 0 {
		return v
	}
	// 半衰期衰减：0.5^(elapsed/halfLife)
	v *= math.Pow(0.5, elapsed.Seconds()/failEWMADecayHalfLife.Seconds())
	if v < failEWMANeutral {
		return failEWMANeutral
	}
	return v
}

// NoteModelCost 记录一次实测扣费观测，更新该 (账号, 模型) 的成本账本。
// credit 为上游 usage.credit（本次真实扣费），tokens 为本次请求的 token 总数
// （prompt+completion，用于折算单位成本）。tokens<=0 时不记录：无法折算单价，
// 记进去会污染账本。
//
// 用 EMA 平滑（alpha=0.3，约 5 次观测收敛）：单次异常值不主导选号决策。
// 账本仅内存态——成本随上游活动（限免期/夜间免费/折扣）变化，持久化旧值
// 反而是脏数据；重启后重新学习，代价只是前几次请求无偏好。
func (p *Pool) NoteModelCost(uid, model string, credit float64, tokens int) {
	if uid == "" || model == "" || tokens <= 0 {
		return
	}
	// 单价按每千 token 归一，消除请求长度差异。
	per1k := credit / float64(tokens) * 1000
	if per1k < 0 {
		per1k = 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	if e.modelCost == nil {
		e.modelCost = make(map[string]modelCostEntry)
	}
	const alpha = 0.3
	prev, seen := e.modelCost[model]
	if !seen {
		e.modelCost[model] = modelCostEntry{CostPer1k: per1k, LastSeen: time.Now(), Samples: 1}
	} else {
		e.modelCost[model] = modelCostEntry{
			CostPer1k: prev.CostPer1k*(1-alpha) + per1k*alpha,
			LastSeen:  time.Now(),
			Samples:   prev.Samples + 1,
		}
	}
}

// NoteSuccess 成功请求累加成功计数、刷新 lastSuccess，并清空连续失败与熔断运行态。
// 二进制模型：清 fails + retryCount + breakerUntil；不碰账号级 until/coolKind
// （那些是即时冷却，各自到期）。
// 额外清 softStreak：成功是账号已恢复的最强证据，连续软限流计数就此归零、退避回到基数。
// 同样清 sessionDeadFails：成功证明 session 未死（与 ClearSessionDead 语义一致）。
//
// model 非空时**额外清除该 (账号, 模型) 的模型级冷却**：一次真实成功是该模型可用的
// 最强证据（sub2api 的 recordSuccess 直接删掉 account+model 记录，同口径）。
// 这是打破「冷却 → 永不被选中 → 永远没机会证明自己」死锁的关键一环：
// 除了签到与人工重置，真实流量本身也能解冻。
//
// 账号级 until/coolKind 不动：账号级冷却（11140）表达的故障与单次成功无关。
func (p *Pool) NoteSuccess(uid, model string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.successCount++
		e.lastSuccess = time.Now()
		e.fails = 0
		e.retryCount = 0
		e.breakerUntil = time.Time{}
		e.softStreak = 0
		e.sessionDeadFails = 0
		e.safetyBannedFails = 0
		if model != "" {
			delete(e.modelCools, model)
		}
		p.dirty.Store(true)
	}
}

// Reset 清除账号的冷却、软退避与熔断状态，可选一并解除禁用。
// 返回重置前的人类可读描述与结果，供管理面板如实展示。
//
// 熔断器（breakerUntil / fails / retryCount）是非持久化的运行态：改造前面板靠
// 「停进程再起进程」把它清掉，现在进程不停了，必须在这里显式清。
// 冷却域（until / reason / softStreak）清空后由调用方 Flush() 落盘。
// 不动 successCount / errTotal：那是累计统计，不是状态。
func (p *Pool) Reset(uid string, includeDisabled bool) (before, after string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.byUID[uid]
	if e == nil {
		return "无记录", "无需重置"
	}
	now := time.Now()
	var parts []string
	if !e.until.IsZero() && now.Before(e.until) {
		parts = append(parts, "冷却至 "+e.until.Local().Format("2006-01-02 15:04:05"))
	}
	if e.softStreak > 0 {
		parts = append(parts, fmt.Sprintf("退避 ×%d", e.softStreak))
	}
	if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) {
		parts = append(parts, "熔断中")
	}
	// 禁用标记一律如实报出：账上带着它却说「本来就没状态」，会让人以为这次重置
	// 把什么都清了，实际标记还在。没清掉时额外注明，免得报了个寂寞。
	if e.disabled {
		if includeDisabled {
			parts = append(parts, "已禁用")
		} else {
			parts = append(parts, "已禁用（本次未解除）")
		}
	}
	before = "本来就没状态"
	if len(parts) > 0 {
		before = strings.Join(parts, "，")
	}

	e.until = time.Time{}
	e.coolKind = 0
	e.reason = ""
	e.softStreak = 0
	p.clearModelCoolsLocked(e)
	e.breakerUntil = time.Time{}
	e.fails = 0
	e.retryCount = 0
	e.sessionDeadFails = 0
	e.safetyBannedFails = 0
	if includeDisabled {
		// 显式勾选解除禁用：人工禁用也一并解除（这是用户本人的显式动作，
		// 与 NoteTestOK 的"测试通过顺带解禁"不同）。
		e.disabledByManual = false
		e.disabled = false
	}
	p.dirty.Store(true)
	return before, "已重置"
}

// NoteTestOK 面板「测试连接」成功后写回池中状态：清冷却/退避/熔断。
//
// 为什么要单独一个方法，而不是复用 NoteSuccess：
//   - 测试是诊断，不该计入 successCount/lastSuccess —— 那些字段描述的是**真实请求**
//     的调度表现，把测试混进去会污染 /status 的「最近活动」与成功率权重。
//   - 测试通过证明 session 活着，故**自动**禁用（12153 session dead）应被解除；
//     且 upsertLocked 重新导入凭证时保留旧状态，所以「重新登录 → 导入 → 测试通过」
//     若不在此解除，就会永远卡在「已禁用」。
//
// **不解除人工禁用**（disabledByManual）：那是运维决定，与凭证有效性无关。
// 旧行为无条件解禁，导致「手动禁用的号点一次测试就被放回池中」——
// 2026-10-16 用户报告的问题。解除人工禁用只有两条显式路径：
// 面板 toggle 启用、或「重置」勾选解除禁用。
//
// 只改池中状态，不碰凭证文件。
func (p *Pool) NoteTestOK(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.byUID[uid]
	if e == nil {
		return
	}
	if e.disabled && e.disabledByManual {
		// 人工禁用：保留 disabled 与 reason，只清冷却/熔断（测试确实证明了链路可用，
		// 清掉这些让它在被重新启用后能立即服务，而不是还背着旧冷却）。
		e.until = time.Time{}
		e.coolKind = 0
		e.softStreak = 0
		p.clearModelCoolsLocked(e)
		e.breakerUntil = time.Time{}
		e.fails = 0
		e.retryCount = 0
		e.sessionDeadFails = 0
		e.safetyBannedFails = 0
		p.dirty.Store(true)
		return
	}
	e.disabled = false
	e.disabledByManual = false
	e.until = time.Time{}
	e.reason = ""
	e.coolKind = 0
	e.softStreak = 0
	p.clearModelCoolsLocked(e)
	e.breakerUntil = time.Time{}
	e.fails = 0
	e.retryCount = 0
	e.sessionDeadFails = 0
	e.safetyBannedFails = 0
	p.dirty.Store(true)
}

// Status 查询单账号状态。
func (p *Pool) Status(uid string) (Status, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return Status{}, false
	}
	return p.statusOf(uid, e), true
}

// AuthByUID 返回账号的完整凭证（给调度器/运维接口用）。
func (p *Pool) AuthByUID(uid string) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.a
	}
	return nil
}

// AvailableUIDs 返回当前 healthy 且未占满在途名额的账号 UID 列表（按 UID 排序，稳定输出）。
// 供会话粘性路由（internal/session）做快路径命中校验 + 双段分配；无可用返回空切片。
func (p *Pool) AvailableUIDs() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if !e.healthy(now) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// AvailableUIDsForModel 同 AvailableUIDs，但把健康口径换成 healthyForModel：
// 在该模型上被 6004 限流的账号不列入，而在**其他模型**被限流的账号照常列入
// （issue #31 模型豁免）。
// 供会话粘性按模型分配与命中校验；model 为空时等价于 AvailableUIDs。
func (p *Pool) AvailableUIDsForModel(model string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// PickByUIDForModel 同 PickByUID，但用 healthyForModel 校验：绑定号在当前模型被
// 6004 限流时返回 nil，让调用方（handler）解绑并回落普通轮换。
// 这是粘性能"换得动"的关键：绑定只记 uid，若只按账号级 healthy 校验，
// 被模型级限额的号（账号整体仍健康）会被持续选中直到轮换次数耗尽。
func (p *Pool) PickByUIDForModel(uid, model string) *auth.Auth {
	return p.PickByUIDForModelWhere(uid, model, nil)
}

// PickByUIDForModelWhere 同时叠加模型豁免与谓词过滤的粘性直取。
// 两个维度正交：model 决定「该号在此模型上是否被 6004 限额」，pred 决定「是否属本次允许区域」。
func (p *Pool) PickByUIDForModelWhere(uid, model string, pred func(*auth.Auth) bool) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return nil
	}
	now := time.Now()
	if !e.healthyForModel(now, model) {
		return nil
	}
	if p.inFlightFull(e) {
		return nil
	}
	if pred != nil && !pred(e.a) {
		return nil
	}
	e.lastUsed = now
	return e.a
}

// PickByUID 若 uid 当前 healthy 且未占满在途名额，返回其凭证（记录 lastUsed 防撞号）；
// 否则返回 nil。供会话粘性路由命中校验与直取使用。
func (p *Pool) PickByUID(uid string) *auth.Auth {
	return p.PickByUIDWhere(uid, nil)
}

// PickByUIDWhere 同 PickByUID，但额外要求 pred 为真（pred==nil 等同不过滤）。
// 用于粘性路由：粘性号若不属于本次请求允许的区域，返回 nil，
// 调用方据此解绑并回落普通轮换。
func (p *Pool) PickByUIDWhere(uid string, pred func(*auth.Auth) bool) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return nil
	}
	now := time.Now()
	if !e.healthy(now) {
		return nil
	}
	if p.inFlightFull(e) {
		return nil
	}
	if pred != nil && !pred(e.a) {
		return nil
	}
	e.lastUsed = now
	return e.a
}

// CountsDetailed 返回 total/healthy/cooling/disabled/inFlightFull 五类计数。
// cooling 含常规冷却（until）与熔断期（breakerUntil）。
// 注意：healthy 口径不含 inFlight 维度（是状态机权威判定，只看 disabled/until/breakerUntil）；
// inFlightFull 是 healthy 的子集——healthy 里已达在途上限的账号数，供 /status 透出满载度。
// 与 ServableNow 的区别见该函数注释。
func (p *Pool) CountsDetailed() (total, healthy, cooling, disabled, inFlightFull int) {
	return p.CountsDetailedWhere(nil)
}

// CountsDetailedWhere 同 CountsDetailed，但只统计 pred 为真的账号（pred==nil 等同不过滤）。
// 谓词作用于账号凭证，与选号谓词同语义（如按区域过滤），使 /status 的计数与账号列表口径一致。
func (p *Pool) CountsDetailedWhere(pred func(*auth.Auth) bool) (total, healthy, cooling, disabled, inFlightFull int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for _, e := range p.byUID {
		if pred != nil && !pred(e.a) {
			continue
		}
		total++
		switch {
		case e.disabled:
			disabled++
		case !e.healthy(now):
			cooling++
		default:
			healthy++
			if p.inFlightFull(e) {
				inFlightFull++
			}
		}
	}
	return total, healthy, cooling, disabled, inFlightFull
}

// ServableNow 报告池当前是否可服务：存在至少一个（对任意模型）healthy 且未占满在途名额的账号。
// 与 CountsDetailed 的 healthy 口径不同：healthy 只看 disabled/until/breakerUntil（状态机权威判定），
// 不看 inFlight；ServableNow 额外叠加在途维度，与 chat 的真实可达性（Pick 会跳过 inFlightFull 账号）对齐。
// 专供 /healthz 用，避免"全账号 healthy 但都占满"时探活误报 200 而 chat 返回 503 的口径裂缝。
//
// 模型级冷却是"部分模型不可用"，不是"账号不可服务"——/healthz 无请求模型上下文，
// 取"存在可服务模型"的存在性语义（与 chat 可达性等价）：账号级 healthy 即视为可服务，
// 故此处不再需要 issue #31 时代的 modelExempt 特判。
func (p *Pool) ServableNow() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for _, e := range p.byUID {
		if p.inFlightFull(e) {
			continue
		}
		if e.healthy(now) {
			return true
		}
	}
	return false
}

// Regions 返回池中现存账号覆盖的区域（升序去重）。
// 供上层按区域产出模型列表：只挂单区账号时不应列出另一区独有的模型。
func (p *Pool) Regions() []auth.Region {
	p.mu.RLock()
	defer p.mu.RUnlock()
	seen := make(map[auth.Region]bool, 2)
	for _, e := range p.byUID {
		seen[e.a.Region()] = true
	}
	out := make([]auth.Region, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// List 返回所有账号状态（按 UID 排序，稳定输出）。
func (p *Pool) List() []Status {
	return p.ListWhere(nil)
}

// ListWhere 同 List，但只返回 pred 为真的账号（pred==nil 等同不过滤）。
// 谓词作用于账号凭证，与选号谓词同语义（如按区域过滤）。
func (p *Pool) ListWhere(pred func(*auth.Auth) bool) []Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if pred != nil && !pred(e.a) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	out := make([]Status, 0, len(uids))
	for _, uid := range uids {
		out = append(out, p.statusOf(uid, p.byUID[uid]))
	}
	return out
}
func (p *Pool) statusOf(uid string, e *entry) Status {
	now := time.Now()
	st := Status{
		UID:             uid,
		Region:          string(e.a.Region()),
		Nickname:        e.a.Nickname,
		Credits:         e.credits,
		Cooling:         now.Before(e.until) || now.Before(e.breakerUntil),
		Reason:          e.reason,
		Disabled:        e.disabled,
		SuccessCount:    e.successCount,
		ErrTotal:        e.errTotal,
		LastSuccessTime: e.lastSuccess,
		LastErrTime:     e.lastErr,
		Until:           e.until,
		SoftStreak:      e.softStreak,
		InFlight:        int(e.inFlight.Load()),
		BreakerFails:    e.fails,
		BreakerUntil:    e.breakerUntil,
	}
	if st.Disabled {
		// 禁用账号透出禁用原因（运维看不到为什么死）。
		st.DisabledReason = e.reason
	}
	if st.Cooling {
		// 剩余时间与类别必须按**真正生效的那个来源**报。
		//
		// Cooling 是 `until || breakerUntil` 的并集，但旧代码无论谁生效都只读 e.until：
		// 熔断期间（until 为零值）会算出 0 剩余，并透出 e.coolKind 里上一次冷却留下的
		// **陈旧类别** —— 实测面板显示「kind=hard_credit + 余额 557 + 熔断中」这种
		// 自相矛盾的状态（该号其实是 6004 限流触发的熔断，coolKind 是别的路径的残留）。
		//
		// 熔断不是冷却类别（它是「反复失败」的指数退避），故此时不给 CoolKind：
		// 面板已有独立的「熔断中 / 熔断剩 N」展示，不需要再编一个类别出来。
		if now.Before(e.until) {
			st.CoolRemaining = int64(time.Until(e.until).Seconds() + 0.999)
			st.CoolKind = e.coolKind.String()
		} else {
			st.CoolRemaining = int64(time.Until(e.breakerUntil).Seconds() + 0.999)
		}
		if st.CoolRemaining < 0 {
			st.CoolRemaining = 0
		}
	}
	// 模型级冷却单列：它们不写 until（账号对其他模型仍可用），只看 Cooling/Until
	// 会把「4.1-flash 冷却中」的账号显示成正常 —— 运维盲区。
	for model, mc := range e.modelCools {
		if mc.until.IsZero() || !now.Before(mc.until) {
			continue // 已到期：不再展示（条目由下一次写入或重启清理）
		}
		st.ModelCools = append(st.ModelCools, ModelCoolStatus{
			Model:        model,
			Kind:         mc.kind.String(),
			Reason:       mc.reason,
			Until:        mc.until,
			RemainingSec: int64(time.Until(mc.until).Seconds() + 0.999),
			SoftStreak:   mc.streak,
		})
	}
	// 输出稳定：map 遍历无序，面板按模型名排序才不会每次刷新都换位置。
	sort.Slice(st.ModelCools, func(i, j int) bool { return st.ModelCools[i].Model < st.ModelCools[j].Model })
	return st
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------
