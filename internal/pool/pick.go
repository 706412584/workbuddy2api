// 选号：Pick 簇（healthy 四因子加权 + 近期失败让位 + Top5 短名单 + 加权随机
// + 全冷却兜底 + 在途占满过滤）。
package pool

import (
	"log"
	"math/rand/v2"
	"sort"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
)

func (p *Pool) Pick() *auth.Auth {
	return p.PickExcluding(nil)
}

// PickExcluding 同上，但跳过 tried 中的 uid（请求级轮换）。
// 挑选策略：healthy 账号中按四因子权重取前 5 名（近期刚失败的整档让位），
// 再在 Top5 内按同一权重加权随机抽签，意图是打散热点，避免永远打同一个账号。
func (p *Pool) PickExcluding(tried map[string]bool) *auth.Auth {
	return p.pick(tried, "", nil)
}

// PickExcludingForModel 模型感知选号：等同 PickExcluding，但对「6004 模型级冷却中的
// 账号」进行模型豁免——请求模型与其 trigger 模型不同时视为可用（issue #31）。
// reqModel 为空时即普通 PickExcluding（不影响既有调用语义）。
func (p *Pool) PickExcludingForModel(tried map[string]bool, reqModel string) *auth.Auth {
	return p.pick(tried, reqModel, nil)
}

// PickExcludingWhere 在 PickExcluding 基础上只考虑 pred 为真的账号（pred==nil 等同不过滤）。
// 典型用途：按账号区域过滤，使请求只用支持其模型的上游区域账号。
// 谓词同时作用于常规候选集与全冷却兜底路径——否则全员冷却时会兜底到不合格的账号。
func (p *Pool) PickExcludingWhere(tried map[string]bool, pred func(*auth.Auth) bool) *auth.Auth {
	return p.pick(tried, "", pred)
}

// PickExcludingForModelWhere 同时叠加模型豁免与谓词过滤。
// 两个维度正交：reqModel 决定「哪些冷却算可用」（6004 模型级豁免），
// pred 决定「哪些账号在本次请求的允许集合内」（区域路由）。
// 只挂一个维度就够的调用点用上面两个更窄的函数，语义更清楚。
func (p *Pool) PickExcludingForModelWhere(tried map[string]bool, reqModel string, pred func(*auth.Auth) bool) *auth.Auth {
	return p.pick(tried, reqModel, pred)
}

// pick 在 healthy 候选集中按四因子权重加权随机选出账号，并记录 lastUsed（防并发撞号）。
// 候选集是 top5 近似：先按 (成本层, 近期是否失败, 四因子权重) 排序取前 5（credits 只是权重的一个因子，
// 闲置补偿与成功率同样决定谁进短名单），再在 top5 内做防撞号过滤。
// 并发防雪崩：跳过 lastUsed 距今 < minPickGap 的账号（除非 top5 全部刚被用过，
// 此时退回最近最少使用 LRU 账号），迫使高并发请求发散，而不是全部撞同一高分账号。
// minPickGap=0（测试用）时过滤恒通过，退化为纯加权随机。
// reqModel 非空时把健康口径换成 healthyForModel（6004 模型豁免生效；PickExcluding 传 ""）。
// 注意：模型豁免只进 normal 选号（候选 healthy 判定）；全冷却兜底不参与模型豁免——
// 兜底本来就是在"无任何 direct 可用"时的降级，切模型可用性已在 normal 阶段体现。
// pred（可 nil）则**同时**作用于 normal 与兜底：区域受限的请求在任何路径都不得越区。
func (p *Pool) pick(tried map[string]bool, reqModel string, pred func(*auth.Auth) bool) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	healthyOf := func(e *entry) bool { return e.healthy(now) }
	if reqModel != "" {
		healthyOf = func(e *entry) bool { return e.healthyForModel(now, reqModel) }
	}
	var cands []*entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if !healthyOf(e) {
			continue
		}
		if p.inFlightFull(e) {
			continue // 在途占满：跳过（max=0 不限时不触发）
		}
		if pred != nil && !pred(e.a) {
			continue
		}
		cands = append(cands, e)
	}
	if len(cands) == 0 {
		// 全冷却兜底：无 healthy 候选时，从冷却账号里选指定模型上 until 最早到期的一个
		// （熔断/冷却共用 expiry 口径，取较早截止者）。禁用的账号永不参与兜底。
		return p.pickEarliestExpiryLocked(tried, pred, now, reqModel)
	}
	// top5 短名单按四因子权重降序截断（而非 credits 单纯降序）：否则闲置补偿 + 成功率
	// 根本进不了短名单决策，低 credits 但高成功率/久置的账号会永远排不进 top5。
	var maxCredits int64
	for _, e := range cands {
		if e.credits > maxCredits {
			maxCredits = e.credits
		}
	}
	// 权重只算一次：顶 5 截断要排序，若在 sort 比较器里现算 weightOf 会翻成 O(n log n) 次
	// 冗余浮点计算（46 账号约 500 次）。先做 O(n) 预计算，再按 (权重, uid) 排序。
	type weighted struct {
		e *entry
		w float64
	}
	// 成本分层：reqModel 非空时，按该模型的实测扣费把候选分层，只保留最优层。
	//   0 = 已实测免费（限免期/夜间免费的号，最强偏好）
	//   1 = 无观测（含观测过期）
	//   2 = 已实测收费
	// 为什么"无观测"排在"已实测收费"之前：新号的限免状态只能靠实测发现，
	// 若已知收费的号恒压过未知号，那台免费的号永远轮不到，也就永远学不到。
	// 为什么用硬过滤而非仅排序：pickWeighted 会在候选内加权随机，只排序的话
	// 收费号仍有机会抽中，达不到"优先免费"的语义。
	costTier := func(e *entry) (int, float64) {
		mc, ok := e.modelCostOf(reqModel, now)
		if !ok {
			return 1, 0
		}
		if mc.CostPer1k <= 0 {
			return 0, 0
		}
		return 2, mc.CostPer1k
	}
	bestTier := 2
	for _, e := range cands {
		if ti, _ := costTier(e); ti < bestTier {
			bestTier = ti
		}
	}
	ws := make([]weighted, 0, len(cands))
	for _, e := range cands {
		if ti, _ := costTier(e); ti == bestTier {
			ws = append(ws, weighted{e: e, w: p.weightOf(e, maxCredits, now, reqModel)})
		}
	}
	// 排序键（优先级从高到低）：
	//   0. 成本层（只保留最优层，见上）
	//   1. **近期是否失败**：刚失败的排到干净账号之后
	//   2. 权重降序
	//   3. uid（保证确定性，便于测试与复现）
	//
	// 第 1 层是本轮调度修复的核心。原先只有权重排序，而权重里的失败信号：
	//   - credits 项 ×10 量纲过大，把其余因子全压住；
	//   - lifetime 成功率对"近期连续失败"近乎无感（9 次失败 / 7479 次成功 = 0.1%）。
	// 实测后果：2026-10-15 上游 4.1 返 500 时，同一批号以 ~62s 周期被反复撞，
	// 每轮 MaxRotate×60s ≈ 186s 后 503。
	//
	// 用"整档让位"而非"权重相乘"：单纯相乘压不动 14 vs 9 的差距，档位切换才
	// 立刻生效。这是**降权**不是**惩罚**——账号仍留在候选集（failEWMA 衰减后自动
	// 回到干净档），不暂停、不冷却、不影响熔断器，与 sub2api 把 report 与 pause
	// 解耦的做法同构。
	dirtyOf := func(e *entry) bool { return e.failEWMANow(reqModel, now) > failEWMAFreshThreshold }
	sort.Slice(ws, func(i, j int) bool {
		_, ci := costTier(ws[i].e)
		_, cj := costTier(ws[j].e)
		if ci != cj {
			return ci < cj // 同层且收费时：单价低的在前
		}
		if di, dj := dirtyOf(ws[i].e), dirtyOf(ws[j].e); di != dj {
			return !di // 干净账号（未近期失败）优先
		}
		if ws[i].w != ws[j].w {
			return ws[i].w > ws[j].w
		}
		return ws[i].e.a.UID < ws[j].e.a.UID
	})
	cands = cands[:0]
	for _, c := range ws {
		cands = append(cands, c.e)
	}
	// 截断为 Top-N 主候选：加权随机只在主候选内进行，保证"优先高分号"的语义。
	//
	// 保留截断的理由：不加限制地让全部 22 个号参与加权随机，会把"优先限免/便宜号"
	// 的语义稀释掉（成本分层只保证同层，层内仍应偏向高分号）。
	// 截断的安全网在下面一层：主候选全被 tried 排除或全在 minPickGap 内时，
	// 会回落到 LRU（而今 LRU 按"近期失败优先排除"的新序取，故也会自然避开失败号）。
	if len(cands) > pickTopN {
		cands = cands[:pickTopN]
	}
	eligible := make([]*entry, 0, len(cands))
	for _, e := range cands {
		if now.Sub(e.lastUsed) >= minPickGap {
			eligible = append(eligible, e)
		}
	}
	var e *entry
	if len(eligible) == 0 {
		// top5 全部刚被用过：LRU 兜底，维持发散且不 starve 任一候选。
		e = cands[0]
		for _, c := range cands[1:] {
			if c.lastUsed.Before(e.lastUsed) {
				e = c
			}
		}
	} else {
		e = p.pickWeighted(eligible, reqModel) // eligible 保序 = top5 降序子集
	}
	e.lastUsed = time.Now()
	return e.a
}

// pickEarliestExpiryLocked 全冷却兜底：在非禁用的冷却/熔断账号中选指定模型上截止最早的一个。
// 分级：disabled 永不参与；CoolSafety（账号级内容安全封控号）同样排除——调了必 403，
// 白白浪费轮换并产生噪音日志。**其余冷却一律放行**（含模型级 14018/6004 冷却与熔断号）：
// 冷却只是"上次失败"的记录，不是"此刻必然失败"的证明，全池无候选时值得用一个请求去证伪。
//
// 历史上 CoolHard（余额耗尽）也在此排除，理由是"调了必 402"——2026-10-11 实测证伪：
// 返 14018 的账号在免费模型上是 200，被排除后直接导致该模型全池 503。既然 14018 现已
// 按模型记账，这条排除自然消失（见 modelCool）。
//
// 被 tried 排除、在途占满的账号同样跳过（维持请求级轮换 + 租约语义）。无任何可用返回 nil。
// pred（可 nil）同样作用于兜底：区域受限请求在全员冷却时不得越区兜底。
func (p *Pool) pickEarliestExpiryLocked(tried map[string]bool, pred func(*auth.Auth) bool, now time.Time, reqModel string) *auth.Auth {
	var best *entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if e.disabled {
			continue // 禁用的账号永不参与兜底
		}
		if e.coolKind == CoolSafety && !e.until.IsZero() && now.Before(e.until) {
			continue // 内容安全封控号不参与兜底：调了必 403
		}
		if p.inFlightFull(e) {
			continue
		}
		if pred != nil && !pred(e.a) {
			continue
		}
		exp := e.expiry(now, reqModel)
		if exp.IsZero() {
			continue
		}
		if best == nil || exp.Before(best.expiry(now, reqModel)) {
			best = e
		}
	}
	if best == nil {
		return nil
	}
	log.Printf("WARN: [pool] fallback_earliest_expiry uid=%s until=%s kind=%s",
		logfmt.UID8(best.a.UID), best.expiry(now, reqModel).Format(time.RFC3339), best.fallbackKind(now, reqModel))
	best.lastUsed = time.Now()
	return best.a
}

// inFlightFull 报告账号是否已占满在途名额（max=0 不限 → 恒 false）。
// 调用方需已持 p.mu（读锁或写锁均可，本方法只读 p.maxInFlight）。
func (p *Pool) inFlightFull(e *entry) bool {
	if p.maxInFlight <= 0 {
		return false
	}
	return e.inFlight.Load() >= int64(p.maxInFlight)
}

// pickTopN 主候选短名单长度：排序后只在前 N 名内做加权随机抽签。
// 保持 5（既有行为）：分数最高的几个号本来就是"优先用"的对象，改动它们的相对概率
// 不是本次目标；本次修的是"这 N 个里若有人刚失败，不该继续优先撞它"。
// 用 var 而非 const：测试需要临时调整（如只放 1 个候选做确定性断言）。
var pickTopN = 5

// minPickGap 防并发撞号窗口：同一账号在该窗口内不重复被选中（除非 top5 全部刚被用过）。
// 生产默认 100ms；纯加权分布测试可临时置 0 关闭防撞号。
var minPickGap = 100 * time.Millisecond

// pickWeighted 四因子加权随机（claude-api selectWeightedRandom 参考口径 + 近期失败率降权）：
//
//		weight = (credits 比例 × 10 + idleWeight + successRate × 3) × (1 - 近期失败率)
//
//	  - credits 比例 = 该号 credits / 候选集内最大 credits（避免量纲爆炸）
//	  - idleWeight = min(距 lastUsed 小时数 × idleWeightPerHour, idleWeightMax)；从未使用给满分
//	  - successRate = successCount/(successCount+errTotal)；无请求记录给 1.5（中性偏信任）
//	  - 近期失败率降权见 weightOf；reqModel 决定读该号在哪个模型上的失败率记账
//
// credits 全 0 时仍按 idle+successRate 加权（不退化均匀随机）。
// 权重为浮点，用 int64 定点（×1e6）抽签可保持确定性随机源注入（randInt64N 语义不变）。
// 随机源优先用 p.randInt64N（仅供测试注入确定性），nil 时回退 math/rand/v2 全局源。
func (p *Pool) pickWeighted(cands []*entry, reqModel string) *entry {
	now := time.Now()
	var maxCredits int64
	for _, e := range cands {
		if e.credits > maxCredits {
			maxCredits = e.credits
		}
	}
	const scale = 1_000_000 // 定点放大：int64 累加权重大整数抽签
	weights := make([]int64, len(cands))
	var total int64
	for i, e := range cands {
		w := p.weightOf(e, maxCredits, now, reqModel)
		weights[i] = int64(w * scale)
		total += weights[i]
	}
	rnd := rand.Int64N
	if p.randInt64N != nil {
		rnd = p.randInt64N
	}
	if total <= 0 {
		return cands[int(rnd(int64(len(cands))))]
	}
	r := rnd(total)
	var acc int64
	for i, e := range cands {
		acc += weights[i]
		if r < acc {
			return e
		}
	}
	return cands[len(cands)-1]
}

// weightOf 计算单个账号的四因子权重（原三因子 × 近期失败率降权）。
//
// 四因子 = (1 + credits 比例×10 + 闲置补偿 + 成功率×3) × 失败率降权因子
//
// 降权因子 = 1 - failEWMA ∈ [0,1]，**乘**在总分上而非加项：
// 这样它按比例压低已得分高的号（有积分的号原本分高，失败后应显著回落），
// 而不会像加项那样被量纲差 10 倍的 credits 项淹没——加项是这套公式原先的
// 结构性问题：idle 封顶 5.0 追不上 credits 的 10，零积分号结构性不可达。
//
// 参考 sub2api openai_account_scheduler.go:998-1039 的 errorFactor = 1-clamp01(errorRate)，
// 差异是那边做加权求和（各因子先归一化），这里沿用本仓库既有的乘法降权，
// 改动面更小且不破坏既有权重断言。
func (p *Pool) weightOf(e *entry, maxCredits int64, now time.Time, reqModel string) float64 {
	w := 1.0
	// 1. credits 比例 ×10（会计入 mid-credit 锚点，避免全员 0 时 credits 项为 0）。
	if maxCredits > 0 {
		w += float64(e.credits) / float64(maxCredits) * 10
	}
	// 2. 闲置补偿。
	if e.lastUsed.IsZero() {
		w += p.idleWeightMax // 从未使用 → 满分
	} else {
		hours := now.Sub(e.lastUsed).Hours()
		idleW := hours * p.idleWeightPerHour
		if idleW > p.idleWeightMax {
			idleW = p.idleWeightMax
		}
		if idleW < 0 {
			idleW = 0 // lastUsed 在未来（时钟回拨）时钳 0
		}
		w += idleW
	}
	// 3. 成功率 ×3。
	totalReq := e.successCount + e.errTotal
	if totalReq > 0 {
		w += float64(e.successCount) / float64(totalReq) * 3
	} else {
		w += 1.5 // 无请求记录 → 中性偏信任
	}
	// 4. 近期失败率降权（乘法，见函数头注释）。
	// 无观测 → 因子 1.0（不降权）；刚失败 → 接近 0.8；连续失败 → 快速趋近 0。
	if seen := e.failEWMANow(reqModel, now); seen > 0 {
		factor := 1 - clamp01(seen)
		// 下限 0.05：完全归零会让该号权重为 0，在加权抽签里彻底不可达；
		// 但"刚连续失败"的号也不该被立刻彻底排除（上游故障是暂时的，
		// 冷却/熔断才是"暂停"的正规手段，降权只负责"别优先撞它"）。
		if factor < 0.05 {
			factor = 0.05
		}
		w *= factor
	}
	return w
}

// clamp01 把值钳到 [0,1]。
func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// SetCredits 更新账号余额。
