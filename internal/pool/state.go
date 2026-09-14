// 账号状态演进与查询：禁用/12153 连续计数判定、成功与错误入账、复活解冻，
// 以及状态查询（Status/AvailableUIDs/PickByUID/CountsDetailed/ServableNow/List）。
package pool

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
)

func (p *Pool) Disable(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = true
		e.reason = reason
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

// ReviveDisabled 人工/端点复活入口：清除 disabled + reason + 连续 12153 计数，
// 账号回到池子（若无其他冷却/熔断则立即可选，健康检查自然接管）。
// **不改** Disabled 在选号/状态端点的既有语义：disabled 号依然不参与选号，
// 直到被本方法复活。不存在的 uid 为空操作。
func (p *Pool) ReviveDisabled(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok && e.disabled {
		e.disabled = false
		e.reason = ""
		e.sessionDeadFails = 0
		p.dirty.Store(true)
	}
}

// reviveCoolingLocked 只清冷却（until/coolKind/reason/softStreak）并更新 credits，不动熔断器
// （fails/retryCount/breakerUntil）。签到解冻走这里：签到成功只证明余额恢复与
// billing 通道健康，不证明 chat 通道健康，熔断（连续 5xx 信号）不应被签到覆盖。
// softStreak 属**冷却域**（与 until/coolKind 同域），故随冷却一并清零——与"解冻只清冷却
// 不清熔断"的既有 C5 语义一致；硬冷却（CoolHard）本就不参与 streak，这里清的是历史软冷却累积。
// 调用方必须已持有 p.mu。
func (p *Pool) ReenableIfCredits(uid string, remain int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		if remain > 0 && !e.disabled {
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
// 二进制模型：清 fails + retryCount + breakerUntil；不碰 until/coolKind（那些是即时冷却，各自到期）。
// 额外清 softStreak：成功是账号已恢复的最强证据，连续软限流计数就此归零、退避回到基数。
// 同样清 sessionDeadFails：成功证明 session 未死（与 ClearSessionDead 语义一致）。
func (p *Pool) NoteSuccess(uid string) {
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
	e.reason = ""
	e.softStreak = 0
	e.breakerUntil = time.Time{}
	e.fails = 0
	e.retryCount = 0
	e.sessionDeadFails = 0
	if includeDisabled {
		e.disabled = false
	}
	p.dirty.Store(true)
	return before, "已重置"
}

// NoteTestOK 面板「测试连接」成功后写回池中状态：清冷却/退避/熔断，并解除禁用。
//
// 为什么要单独一个方法，而不是复用 NoteSuccess：
//   - 测试是诊断，不该计入 successCount/lastSuccess —— 那些字段描述的是**真实请求**
//     的调度表现，把测试混进去会污染 /status 的「最近活动」与成功率权重。
//   - 测试必须解除禁用：disabled 只在 session 死亡时设置，而测试通过恰好证明
//     session 活着，二者直接矛盾。且 upsertLocked 重新导入凭证时保留旧状态，
//     所以「重新登录 → 导入 → 测试通过」若不在此解除，就会永远卡在「已禁用」。
//
// 只改池中状态，不碰凭证文件。
func (p *Pool) NoteTestOK(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.byUID[uid]
	if e == nil {
		return
	}
	e.disabled = false
	e.until = time.Time{}
	e.reason = ""
	e.coolKind = 0
	e.softStreak = 0
	e.breakerUntil = time.Time{}
	e.fails = 0
	e.retryCount = 0
	e.sessionDeadFails = 0
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
// 模型级豁免（issue #31 的探活侧补齐）：6004 模型级软冷却中的账号（modelExempt 形态）
// 对触发模型不可用、对其他模型仍可选——chat 的 healthyForModel 已按此放行切模型请求，
// 探活必须同口径，否则"全号被 v4.1 限流但 glm 可用"时 chat 实际 200 而 /healthz 误报 503。
// /healthz 无请求模型上下文，取"存在可服务模型"的存在性语义（与 chat 可达性等价）。
func (p *Pool) ServableNow() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for _, e := range p.byUID {
		if p.inFlightFull(e) {
			continue
		}
		if e.healthy(now) || e.modelExempt() {
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
		// 冷却剩余秒数（向上取整，避免 0 显示为已到期）。
		st.CoolRemaining = int64(time.Until(e.until).Seconds() + 0.999)
		if st.CoolRemaining < 0 {
			st.CoolRemaining = 0
		}
		st.CoolKind = e.coolKind.String()
	}
	return st
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------
