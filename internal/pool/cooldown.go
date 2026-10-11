// 冷却与熔断：Cooldown/CooldownSoftForModel（软退避指数升级）、软冷却封顶、
// 熔断失败累计、签到解冻（ReenableIfCredits/reviveCoolingLocked）。
package pool

import (
	"time"
)

func (p *Pool) SetCredits(uid string, credits int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = credits
		p.dirty.Store(true)
	}
}

// Cooldown 施加冷却至 now+d。model 为空 = 账号级（作用于所有模型）；非空 = 仅该模型。
//
// 冷却入口同时是熔断器的失败信号：喂入 fails，达到阈值按指数退避熔断（与 until 正交）。
// 熔断器仍是**账号级**的——它表达"这个号整体病态"，与模型维的冷却记账正交。
//
// CoolSoft 额外做**连续软限流指数退避**：同一 (账号, 模型) 连续触发软冷却时，实际时长按
// d << (streak-1) 逐次翻倍（封顶 softRateMax，见 softDurationLocked）。
// 首 streak=1 → 实际时长 = d，单次调用语义与旧行为一致。
// 这就是与熔断器并存的双重升级，且是**有意为之**：软退避管"近期被限流"，
// 在 600s 起按分钟~小时级放大；熔断管"病态反复失败"，按 30m→1h→2h→6h 长期封禁。
// 二者喂入路径共用本入口但计数器独立（softStreak vs fails），互不污染。
//
// streak 的归属随 model 走：账号级用 entry.softStreak，模型级用该模型的 modelCool.streak。
// 在 A 模型被限流不应放大 B 模型的退避——这是本函数按模型分账的直接收益。
func (p *Pool) Cooldown(uid string, kind CoolKind, d time.Duration, reason, model string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	now := time.Now()
	if kind == CoolSoft {
		streak := e.softStreak + 1
		if model != "" {
			// 该模型的首次软冷却从 1 起算，不继承账号级的 streak
			// （否则一次账号级限流会把所有模型的退避基数一起抬高）。
			streak = 1
			if mc := e.modelCools[model]; mc != nil {
				streak = mc.streak + 1
			}
		}
		d = p.softDurationLocked(d, streak)
	}
	if model == "" {
		e.until = now.Add(d)
		e.coolKind = kind
		e.reason = reason
		if kind == CoolSoft {
			e.softStreak++
		}
	} else {
		p.setModelCoolLocked(e, model, kind, now.Add(d), reason)
	}
	p.recordBreakerFailureLocked(e) // 冷却入口也是熔断器的失败信号
	p.dirty.Store(true)
}

// setModelCoolLocked 写入/更新一条模型级冷却记账。调用方必须已持 p.mu。
// kind == CoolSoft 时递增该模型自己的 streak（指数退避的指数）。
func (p *Pool) setModelCoolLocked(e *entry, model string, kind CoolKind, until time.Time, reason string) {
	if e.modelCools == nil {
		e.modelCools = map[string]*modelCool{}
	}
	mc := e.modelCools[model]
	if mc == nil {
		mc = &modelCool{}
		e.modelCools[model] = mc
	}
	if kind == CoolSoft {
		mc.streak++
	}
	mc.until = until
	mc.kind = kind
	mc.reason = reason
}

// clearModelCoolsLocked 清空全部模型级冷却记账。调用方必须已持 p.mu。
//
// 所有「账号已被证明恢复」的出口共用（签到解冻、面板测试/重置、账号级长冷却覆盖）：
// 余额恢复或人工确认后，没有理由再按模型逐个拦着。
func (p *Pool) clearModelCoolsLocked(e *entry) {
	if len(e.modelCools) == 0 {
		return
	}
	e.modelCools = nil
}

// CooldownSoftForModel 429 的**模型级**软冷却入口（issue #31）。
// 区别于 Cooldown：当上游 6004 明说「将在 … 重置」时，把冷却截止精确设为
// resetAt（上游给定时间，不再靠固定基数+指数退避猜测），记入该模型的记账。
//
// 收窄规则：
//   - resetAt 非零（6004 带解析时间）→ until = min(resetAt, now+softRateMax)，
//     指数退避**不适用**：重置时间已是上游权威，再指数放大会无视它明说的恢复时刻
//     （这恰是本 issue 的核心痛点）。
//   - resetAt 零值（6004 无时间文案 / 非 6004 的 soft）→ 完全退回 Cooldown 现状
//     （soft_streak 指数退避 + 封顶 soft_rate_max）。
//
// 熔断信号照旧喂入（冷却与熔断正交，行为与 Cooldown 一致）；该模型的 streak 仍递增
// （无论是否命中解析时间），后续无时间的 6004 从当前 streak 继续退避。
func (p *Pool) CooldownSoftForModel(uid string, base time.Duration, resetAt time.Time, model, reason string) {
	if model == "" {
		// 无模型信息时退化为账号级软冷却（调用方本不该如此，但不静默丢弃惩罚）。
		p.Cooldown(uid, CoolSoft, base, reason, "")
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	streak := 1
	if mc := e.modelCools[model]; mc != nil {
		streak = mc.streak + 1
	}
	d := p.softDurationLocked(base, streak)
	if !resetAt.IsZero() {
		// 有上游重置时间：直接取 min(重置墙钟, now+softRateMax)，不做指数放大。
		now := time.Now()
		cap := now.Add(p.softRateMaxOr())
		switch {
		case resetAt.After(cap):
			d = cap.Sub(now)
		case resetAt.After(now):
			d = resetAt.Sub(now)
		default:
			// 重置时间已过（时钟偏移/文案过期）：冷却极短，立即恢复。
			d = time.Millisecond
		}
	}
	p.setModelCoolLocked(e, model, CoolSoft, time.Now().Add(d), reason)
	p.recordBreakerFailureLocked(e) // 冷却入口也是熔断器的失败信号
	p.dirty.Store(true)
}

// softRateMaxOr 返回生效的 softRateMax（未注入时按默认 2h），供封顶计算。
// 调用方必须已持有 p.mu。
func (p *Pool) softRateMaxOr() time.Duration {
	if p.softRateMax > 0 {
		return p.softRateMax
	}
	return defaultSoftRateMax
}

// softDurationLocked 按连续软冷却次数把基数 d 指数放大：d << (streak-1)，封顶 softRateMax。
// softRateMax 未注入（<=0）时按 defaultSoftRateMax 算。streak<=1 时原样返回 d。
// 左移位数受 softStreakShiftMax 限制，避免 streak 极大时移位溢出。
// 调用方必须已持有 p.mu。
func (p *Pool) softDurationLocked(d time.Duration, streak int) time.Duration {
	if streak <= 1 {
		return d
	}
	shift := streak - 1
	if shift > softStreakShiftMax {
		shift = softStreakShiftMax
	}
	d <<= shift
	max := p.softRateMax
	if max <= 0 {
		max = defaultSoftRateMax
	}
	if d > max || d <= 0 { // d<=0：左移溢出成负数/零，同样按封顶兜底
		d = max
	}
	return d
}

// recordBreakerFailureLocked 累计一次熔断失败；达到阈值则按指数退避熔断。
// 熔断与冷却（until）解耦：冷却按错误类别给固定时长，熔断则对"反复失败"逐次加长封禁。
// 调用方必须已持有 p.mu。
func (p *Pool) recordBreakerFailureLocked(e *entry) {
	e.fails++
	if e.fails < p.breakerThreshold {
		return
	}
	d := p.breakerCooldown
	for i := 0; i < e.retryCount; i++ {
		d *= 2
		if d >= p.breakerCooldownMax {
			d = p.breakerCooldownMax
			break
		}
	}
	// 触发熔断：重置失败计数供下一轮重新累计；retryCount 递增放大退避指数。
	e.fails = 0
	e.retryCount++
	e.breakerUntil = time.Now().Add(d)
}

// CooldownUntilTomorrow4AM 冷却到下一个 04:00（本地时区）。
// 用于 ErrHardCredit 场景：积分耗尽账号等签到任务（09:00/21:00）恢复。
// model 非空时**只封该模型**——14018 是「这个模型付不起」，同一账号的免费模型照常可用
// （2026-10-11 实测：hy4-preview 返 14018 的账号，deepseek-v4.1-flash 与 hy3 均为 200）。
func (p *Pool) CooldownUntilTomorrow4AM(uid, reason, model string) {
	now := time.Now()
	p.Cooldown(uid, CoolHard, nextDay4AM(now).Sub(now), reason, model)
}

// CooldownSafetyBanned 账号级内容安全封控（403 + 11140 + request illegal）的专用冷却入口。
//
// 与 Cooldown 的两点差异，都是刻意为之：
//  1. **不走指数退避**。该形态不是「频率/用量」信号，退避放大没有意义；上游也没有
//     明示恢复时刻，任何时长都是猜。故按调用方给定的固定 d 冷却，并复用 jitterDur
//     抖动打散多账号同相位（风控往往是批量命中，齐步走到期会再次聚团撞墙）。
//  2. **不发熔断信号**。熔断器的指数升级（30m→1h→2h→6h）是给「反复失败的病态号」
//     设计的，而本形态一次触发就已经是终态——再叠一层数小时的熔断只会让恢复出口
//     更难走（熔断不被签到/重置之外的路径清）。账号已经在长冷却里，无需第二把锁。
//
// 清空模型级记账：账号级长冷却覆盖所有模型，留着模型级记录只会让
// 「账号级已封控但某模型不冷却」的矛盾状态漏进选号判定。
func (p *Pool) CooldownSafetyBanned(uid string, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Now().Add(d)
		e.coolKind = CoolSafety
		e.reason = reason
		p.clearModelCoolsLocked(e)
		p.dirty.Store(true)
	}
}

// nextDay4AM 返回 now 之后最近的一个 04:00（与 now 同一时区）。
// now 在当天 04:00 之前（凌晨 00:00~04:00）时返回当天 04:00——此时签到尚未执行，
// 该窗内触发的硬冷却等当天签到即可恢复；返回次日会白冷约一天。
// 04:00 整及之后返回次日 04:00。
// time.Date 对日溢出自动进位（月末→下月 1 号、年末→下年 1 号），天然覆盖跨日/跨月/跨年。
func nextDay4AM(now time.Time) time.Time {
	if now.Hour() < 4 {
		return time.Date(now.Year(), now.Month(), now.Day(), 4, 0, 0, 0, now.Location())
	}
	return time.Date(now.Year(), now.Month(), now.Day()+1, 4, 0, 0, 0, now.Location())
}

// Disable 永久禁用（session 死亡），需人工重登后手工恢复或文件替换。
func (p *Pool) reviveCoolingLocked(e *entry, credits int64) {
	e.credits = credits
	e.until = time.Time{}
	e.coolKind = 0
	e.reason = ""
	e.softStreak = 0
	p.clearModelCoolsLocked(e) // 余额恢复 = 账号恢复，模型级记账一并作废
}

// ReenableIfCredits 签到后解冻：仅当 remain > 0 且账号非禁用时，清冷却（余额恢复）。
// 注意：不碰熔断器——熔断到期（breakerUntil 过期）或下次 chat 成功（NoteSuccess）才恢复。
