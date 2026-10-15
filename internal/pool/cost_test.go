package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestNoteModelCostAndPreferFree 成本账本的核心验收：同一模型上已实测免费的号
// 必须优先于已实测收费的号，且这个优先级要压过积分因子。
func TestNoteModelCostAndPreferFree(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "free"})
	p.Add(&auth.Auth{UID: "paid"})
	// paid 积分远高于 free：若只看积分，paid 必胜；成本分层必须压过积分。
	p.SetCredits("free", 1)
	p.SetCredits("paid", 1_000_000)

	p.NoteModelCost("free", "hy4-preview", 0, 1000)   // 免费
	p.NoteModelCost("paid", "hy4-preview", 2.9, 1000) // 收费

	for i := 0; i < 50; i++ {
		a := p.PickExcludingForModel(nil, "hy4-preview")
		if a == nil || a.UID != "free" {
			t.Fatalf("第 %d 次选中 %v，want free（免费号应优先于高积分收费号）", i, a)
		}
	}
}

// TestModelCostCheaperPaidWins 同为收费时，单价低的优先。
func TestModelCostCheaperPaidWins(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "cheap"})
	p.Add(&auth.Auth{UID: "pricey"})
	p.NoteModelCost("cheap", "hy4-preview", 0.3, 1000)
	p.NoteModelCost("pricey", "hy4-preview", 5.0, 1000)
	for i := 0; i < 50; i++ {
		a := p.PickExcludingForModel(nil, "hy4-preview")
		if a == nil || a.UID != "cheap" {
			t.Fatalf("选中 %v, want cheap（单价低的优先）", a)
		}
	}
}

// TestModelCostUnknownBeatsKnownPaid 无观测的号优先于已知收费的号。
// 这是学习闭环的前提：新号的限免状态只能靠实测发现，若已知收费的号恒压过未知号，
// 那台免费的号永远轮不到，也就永远学不到"它其实是免费的"。
func TestModelCostUnknownBeatsKnownPaid(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "unknown"})
	p.Add(&auth.Auth{UID: "paid"})
	p.SetCredits("unknown", 1)
	p.SetCredits("paid", 1_000_000)
	p.NoteModelCost("paid", "hy4-preview", 2.9, 1000)

	for i := 0; i < 50; i++ {
		a := p.PickExcludingForModel(nil, "hy4-preview")
		if a == nil || a.UID != "unknown" {
			t.Fatalf("选中 %v, want unknown（未知号需有机会被实测）", a)
		}
	}
}

// TestModelCostFreeBeatsUnknown 已确认免费的号压过未观测的号。
func TestModelCostFreeBeatsUnknown(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "knownfree"})
	p.Add(&auth.Auth{UID: "unknown"})
	p.SetCredits("knownfree", 1)
	p.SetCredits("unknown", 1_000_000)
	p.NoteModelCost("knownfree", "hy4-preview", 0, 1000)

	for i := 0; i < 50; i++ {
		a := p.PickExcludingForModel(nil, "hy4-preview")
		if a == nil || a.UID != "knownfree" {
			t.Fatalf("选中 %v, want knownfree（已确认免费 > 未知）", a)
		}
	}
}

// TestModelCostStaleIgnored 过期观测失效：防止"夜间免费"在白天仍被当作免费
// （否则会一直用那个已开始收费的号）。
func TestModelCostStaleIgnored(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteModelCost("u1", "hy4-preview", 0, 1000)
	p.mu.Lock()
	mc := p.byUID["u1"].modelCost["hy4-preview"]
	mc.LastSeen = time.Now().Add(-2 * modelCostTTL) // 手工做旧
	p.byUID["u1"].modelCost["hy4-preview"] = mc
	p.mu.Unlock()

	p.mu.RLock()
	_, ok := p.byUID["u1"].modelCostOf("hy4-preview", time.Now())
	p.mu.RUnlock()
	if ok {
		t.Error("过期的成本观测应失效（夜间免费白天不该仍算免费）")
	}
}

// TestModelCostEMASmoothing 观测按 EMA 平滑，单次异常不主导决策。
func TestModelCostEMASmoothing(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteModelCost("u1", "m", 1.0, 1000) // 首次：直接取实测
	p.mu.RLock()
	first := p.byUID["u1"].modelCost["m"].CostPer1k
	p.mu.RUnlock()
	if first != 1.0 {
		t.Fatalf("首次观测 CostPer1k=%v want 1.0", first)
	}
	p.NoteModelCost("u1", "m", 3.0, 1000) // 第二次：EMA 拉向 3.0 但不等于 3.0
	p.mu.RLock()
	second := p.byUID["u1"].modelCost["m"].CostPer1k
	samples := p.byUID["u1"].modelCost["m"].Samples
	p.mu.RUnlock()
	if second <= 1.0 || second >= 3.0 {
		t.Errorf("EMA 后 CostPer1k=%v, want 介于 1.0 与 3.0 之间", second)
	}
	if samples != 2 {
		t.Errorf("Samples=%d want 2", samples)
	}
}

// TestModelCostIgnoresInvalidInput 无法折算单价的输入不应写入账本（防污染）。
func TestModelCostIgnoresInvalidInput(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteModelCost("u1", "m", 5.0, 0)  // tokens=0
	p.NoteModelCost("", "m", 5.0, 1000) // 空 uid
	p.NoteModelCost("u1", "", 5.0, 1000)
	p.mu.RLock()
	n := len(p.byUID["u1"].modelCost)
	p.mu.RUnlock()
	if n != 0 {
		t.Errorf("非法输入不应写入成本账本，len=%d", n)
	}
}

// TestModelCostEmptyModelUnaffected 空模型名（PickExcluding 路径）不触发成本分层，
// 保持原三因子选号行为。
func TestModelCostEmptyModelUnaffected(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteModelCost("u1", "hy4-preview", 5.0, 1000)
	if a := p.PickExcludingForModel(nil, ""); a == nil {
		t.Error("空模型名不应被成本分层影响（应仍能选出账号）")
	}
}

// TestPickByUIDForModel 模型级可用性：绑定号在被限额的模型上返回 nil，
// 在其他模型上照常返回——handler 据此决定解绑还是沿用。
func TestPickByUIDForModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// 触发一次带解析时间的 6004 冷却：记入 modelCools["hy4-preview"]，
	// 该账号对 hy4-preview 冷却、对其他模型豁免（issue #31 语义）。
	reset := time.Now().Add(time.Hour)
	p.CooldownSoftForModel("u1", 600*time.Second, reset, "hy4-preview", "6004 model rate limit")

	// 先确认冷却真的落上了（否则后面两个断言是假阳性）。
	p.mu.RLock()
	mc := p.byUID["u1"].modelCools["hy4-preview"]
	p.mu.RUnlock()
	if mc == nil || mc.kind != CoolSoft {
		t.Fatalf("modelCools[hy4-preview]=%+v want 一条 CoolSoft 记账（6004 模型级冷却未记录）", mc)
	}

	if a := p.PickByUIDForModel("u1", "hy4-preview"); a != nil {
		t.Error("被限额的模型上，PickByUIDForModel 应返回 nil（让 handler 解绑轮换）")
	}
	if a := p.PickByUIDForModel("u1", "glm-5.3"); a == nil {
		t.Error("其他模型应照常可用（模型豁免生效）")
	}
}

// TestAvailableUIDsForModel 按模型过滤可用账号：被该模型限额的号不列入，
// 被其他模型限额的号照常列入。
func TestAvailableUIDsForModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.CooldownSoftForModel("u1", 600*time.Second, time.Now().Add(time.Hour), "hy4-preview", "6004")

	uids := p.AvailableUIDsForModel("hy4-preview")
	if len(uids) != 1 || uids[0] != "u2" {
		t.Errorf("AvailableUIDsForModel(hy4-preview)=%v want [u2]", uids)
	}
	all := p.AvailableUIDsForModel("glm-5.3")
	if len(all) != 2 {
		t.Errorf("AvailableUIDsForModel(glm-5.3)=%v want 两个账号（u1 对其他模型豁免）", all)
	}
}

// ---------------------------------------------------------------------------
// 近期失败率降权（EWMA）
// ---------------------------------------------------------------------------

// entryWeightForModel 与 entryWeight 同，但可指定模型（读该模型上的失败率降权）。
func (p *Pool) entryWeightForModel(uid, model string) float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e := p.byUID[uid]
	var maxCredits int64
	for _, x := range p.byUID {
		if x.credits > maxCredits {
			maxCredits = x.credits
		}
	}
	return p.weightOf(e, maxCredits, time.Now(), model)
}

// TestNoteAttemptDropsWeight 一次失败即压低权重，成功把它拉回。
func TestNoteAttemptDropsWeight(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	base := p.entryWeightForModel("u1", "m")

	p.NoteAttempt("u1", "m", false)
	afterFail := p.entryWeightForModel("u1", "m")
	if afterFail >= base {
		t.Fatalf("一次失败后权重应下降: base=%v afterFail=%v", base, afterFail)
	}

	// 连续成功应把失败率摊薄回接近原始值（alpha=0.2，需若干次）。
	for i := 0; i < 20; i++ {
		p.NoteAttempt("u1", "m", true)
	}
	if got := p.entryWeightForModel("u1", "m"); got < base*0.99 {
		t.Errorf("连续成功后权重应回到基线: base=%v got=%v", base, got)
	}
}

// TestNoteAttemptScopedToModel 失败率按 (账号, 模型) 记账：
// 在 A 模型失败不应连带到 B 模型——上游明说 "switch to the other models"。
// 回归的是 TestChat6004ModelResetCoolsToParsedTime 暴露的问题。
func TestNoteAttemptScopedToModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	p.NoteAttempt("u1", "glm-5.3", false)
	if got := p.byUID["u1"].failEWMANow("glm-5.3", time.Now()); got <= 0 {
		t.Fatalf("glm-5.3 上应记到失败率, got=%v", got)
	}
	if got := p.byUID["u1"].failEWMANow("hy3-x", time.Now()); got != 0 {
		t.Errorf("hy3-x 未失败，失败率应为 0（模型级隔离），got=%v", got)
	}
}

// TestPickPrefersCleanAccountAfterFailure 刚失败的号应让位给干净号：
// 失败号即使积分高得多，也不该被优先撞——这正是本次修复的核心诉求。
func TestPickPrefersCleanAccountAfterFailure(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "rich"})   // 高积分，原本必被优先选中
	p.Add(&auth.Auth{UID: "clean"})  // 零积分
	p.SetCredits("rich", 1000)
	p.NoteAttempt("rich", "m", false) // rich 刚在这个模型上失败

	// 注入 r=0（选权重最高者）：干净号应已排到 rich 之前。
	p.SetRandomSource(func(n int64) int64 { return 0 })
	got := p.PickExcludingForModel(nil, "m")
	if got == nil || got.UID != "clean" {
		t.Fatalf("pick=%v, want clean（rich 刚失败应让位）", got)
	}
}

// TestPickFailureDoesNotBlockOtherModel 在 A 模型失败不影响 B 模型的选号顺序。
func TestPickFailureDoesNotBlockOtherModel(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "rich"})
	p.Add(&auth.Auth{UID: "clean"})
	p.SetCredits("rich", 1000)
	p.NoteAttempt("rich", "glm-5.3", false) // 只记在 glm-5.3

	p.SetRandomSource(func(n int64) int64 { return 0 })
	got := p.PickExcludingForModel(nil, "hy3-x") // 换模型：rich 仍是最高权重
	if got == nil || got.UID != "rich" {
		t.Fatalf("pick=%v, want rich（其他模型不应受 glm-5.3 的失败影响）", got)
	}
}

// TestFailEWMADecaysOverTime 失败率随半衰期衰减：很久以前的失败不该继续压制权重。
func TestFailEWMADecaysOverTime(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteAttempt("u1", "m", false)

	now := time.Now()
	soon := p.byUID["u1"].failEWMANow("m", now)
	// 手工把上报时刻做旧 4 个半衰期（10min × 4 = 40min）→ 衰减到约 1/16。
	p.mu.Lock()
	p.byUID["u1"].failEWMATime["m"] = now.Add(-40 * time.Minute)
	fresh := p.byUID["u1"].failEWMA["m"]
	p.mu.Unlock()

	later := p.byUID["u1"].failEWMANow("m", now)
	if later >= soon {
		t.Fatalf("失败率应随时间衰减: soon=%v later=%v", soon, later)
	}
	if later > fresh/8 {
		t.Errorf("40 分钟（4 个半衰期）后应衰减到不足 1/8: fresh=%v later=%v", fresh, later)
	}
}

// TestNoteAttemptEmptyUIDIgnored 空 uid 不应 panic（models 拉取等路径可能传空）。
func TestNoteAttemptEmptyUIDIgnored(t *testing.T) {
	p := New("")
	p.NoteAttempt("", "m", false)
	p.NoteAttempt("not-exist", "m", false)
}
