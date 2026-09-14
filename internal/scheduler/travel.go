// travel.go 猫猫旅行巡检状态机：随旅行时点（travel_hours，默认 09 点）对池内每个可用账号单趟推进一次。
// 无猫 → 同意协议 + 领养；有猫 → 按 travel/status 分派 派出 / 领奖 / 跳过。
package scheduler

import (
	"fmt"
	"log"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/upstream"
)

const (
	// travelLocationID 派出地点固定 4（古镇客栈）：4 个地点收益/时长区间完全相同，无最优解。
	travelLocationID = 4

	// travelStateIdle 空闲可派出；travelStateTraveling 在途；travelStateArrived 到站可领奖。
	travelStateIdle      = "idle"
	travelStateTraveling = "traveling"
	travelStateArrived   = "arrived"
)

// travelAccountDelay 账号间限速：全量账号约 40s，避免上游风控。测试可置 0。
var travelAccountDelay = 800 * time.Millisecond

// activityAccountDelay 活跃上报账号间限速：与旅行同口径，避免上游风控。测试可置 0。
var activityAccountDelay = 800 * time.Millisecond

// activityReportGap 同一账号内连续上报之间的间隔：5 连发模拟同一会话多轮对话，
// 秒发易触发风控，故 1.5s 一条。测试可置 0。
var activityReportGap = 1500 * time.Millisecond

// cstZone 上游每日重置按自然日 00:00 CST（Asia/Shanghai）。中国无夏令时，固定 +8 即可，
// 不依赖容器 tzdata。
var cstZone = time.FixedZone("CST", 8*60*60)

// travelDay 返回 t 所属的上游自然日（CST），格式 2006-01-02。
func travelDay(t time.Time) string {
	return t.In(cstZone).Format("2006-01-02")
}

// RunTravelNow 立即对池内所有可用账号执行一趟旅行巡检。
// 禁用账号跳过；401/查询失败只跳过该账号本轮（不强刷 token，交 22:00 keepalive）；
// 账号间限速 travelAccountDelay。
//
// 返回一句人类可读的摘要，供面板「立即执行」展示。
func (s *Scheduler) RunTravelNow() string {
	first := true
	acted := map[string]int{} // 动作 → 次数（动作名见 travelOne 返回值）
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			acted["跳过"]++
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			acted["跳过"]++
			continue
		}
		if !first {
			time.Sleep(travelAccountDelay)
		}
		first = false
		acted[s.travelOne(a)]++
	}
	// 固定顺序输出，避免 map 迭代顺序让每次摘要看起来都不一样。
	parts := make([]string, 0, 4)
	for _, k := range []string{"领养", "派出", "领奖", "跳过", "失败"} {
		if n := acted[k]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", k, n))
		}
	}
	if len(parts) == 0 {
		return "没有可巡检的账号"
	}
	return strings.Join(parts, " · ")
}

// travelOne 单账号单趟状态机：查有无猫 + 查状态 + 最多一个动作，不轮询不等待。
// 返回本趟的动作名（"领养"/"派出"/"领奖"/"跳过"/"失败"），供 RunTravelNow 汇总。
// 注意只归"动作"，不评价成败细节——成败原因看日志。
func (s *Scheduler) travelOne(a *auth.Auth) string {
	buddy, err := s.cfg.Upstream.BuddyInfo(a)
	if err != nil {
		log.Printf("travel %s: buddy-info: %v", logfmt.UID8(a.UID), err)
		return "失败"
	}
	if buddy == nil {
		return s.travelAdopt(a)
	}
	ts, err := s.cfg.Upstream.TravelStatus(a)
	if err != nil {
		log.Printf("travel %s: status: %v", logfmt.UID8(a.UID), err)
		return "失败"
	}
	switch ts.State {
	case travelStateArrived:
		return s.travelClaim(a, ts)
	case travelStateIdle:
		return s.travelDepart(a, ts)
	case travelStateTraveling:
		log.Printf("travel %s: skip (traveling record=%d)", logfmt.UID8(a.UID), ts.RecordID)
		return "跳过"
	default:
		log.Printf("travel %s: skip (unknown state %q)", logfmt.UID8(a.UID), ts.State)
		return "跳过"
	}
}

// travelDepart 空闲且未达当日上限时派出（每日 1 次，自然日 00:00 CST 重置）。
func (s *Scheduler) travelDepart(a *auth.Auth, ts *upstream.TravelState) string {
	if ts.DailyLimitReached {
		log.Printf("travel %s: skip (daily limit reached)", logfmt.UID8(a.UID))
		return "跳过"
	}
	if err := s.cfg.Upstream.TravelDepart(a, travelLocationID); err != nil {
		log.Printf("travel %s: depart: %v", logfmt.UID8(a.UID), err)
		return "失败"
	}
	log.Printf("travel %s: depart ok location=%d", logfmt.UID8(a.UID), travelLocationID)
	return "派出"
}

// travelClaim 到站领奖（必须带 record_id）。
func (s *Scheduler) travelClaim(a *auth.Auth, ts *upstream.TravelState) string {
	if ts.RecordID == 0 {
		log.Printf("travel %s: claim skipped (arrived but no record_id)", logfmt.UID8(a.UID))
		return "跳过"
	}
	reward, err := s.cfg.Upstream.TravelClaim(a, ts.RecordID)
	if err != nil {
		log.Printf("travel %s: claim record=%d: %v", logfmt.UID8(a.UID), ts.RecordID, err)
		return "失败"
	}
	log.Printf("travel %s: claim ok record=%d reward=%d", logfmt.UID8(a.UID), ts.RecordID, reward)
	return "领奖"
}

// travelAdopt 旅行巡检时领养：受 adoptTriedToday 当日防抖约束。
func (s *Scheduler) travelAdopt(a *auth.Auth) string {
	return s.adoptBuddy(a, false)
}

// travelAdoptForce 活跃上报补满对话量后领养：豁免 adoptTriedToday 当日防抖。
// 背景：旅行排程 09 点已领养且因对话量未达 skip，10 点活跃上报 5 连发把
// 对话量补满——此时是「门槛刚达成」的新状态，不算对上游重试轰炸，放行重试。
// 有猫账号 BuddyInfo 非空时直接跳过（不重复领养）。
func (s *Scheduler) travelAdoptForce(a *auth.Auth) {
	buddy, err := s.cfg.Upstream.BuddyInfo(a)
	if err != nil {
		log.Printf("activity %s: buddy-info: %v", logfmt.UID8(a.UID), err)
		return
	}
	if buddy != nil {
		return // 已有猫，无需领养
	}
	s.adoptBuddy(a, true) // force=true 豁免当日防抖
}

// adoptBuddy 无猫时领养：先同意协议（幂等）再 buddy/first。
// conversation 门槛未达标（HTTP 400 first_buddy task not completed yet）属预期行为，
// 记一次当日已试后静默跳过，不再重试。force=true 时豁免当日防抖（活跃上报补满对话量后重试）。
// 返回动作名供 RunTravelNow 汇总；调用方若只关心副作用可忽略。
func (s *Scheduler) adoptBuddy(a *auth.Auth, force bool) string {
	// global（workbuddy.ai）区的 buddy/agreement 实测恒 500，领养在该区不可用。
	//
	// 2026-09-14 用真实账号对照实测（同请求、同头，只换账号）：
	//   CN 账号：buddy/info OK · buddy/agreement OK · growth/streak OK(days=1)
	//   global ：buddy/info OK · buddy/agreement 500 · growth/streak 500
	// 即：不是账号问题，也不是请求形态问题（同区只读接口正常，写接口挂）；
	// 是该区写接口未开放或故障。同理 growth/streak 在该区也不可用。
	//
	// 故跳过而不报失败：这不是账号故障，报失败（"失败 18"）会让运维去查一个不存在的
	// 问题，并每趟白打 18 次必然 500 的请求。与「global 区无签到活动」同属区域能力差异。
	//
	// 若将来腾讯在该区开放 buddy，删掉这行即可恢复；判定依据随之失效，故在此留验。
	if a.Region() == auth.RegionGlobal {
		return "跳过"
	}
	if !force && s.adoptTriedToday(a.UID) {
		return "跳过" // 当日已判定门槛未达，不重试
	}
	if err := s.cfg.Upstream.BuddyAgreement(a); err != nil {
		log.Printf("travel %s: agreement: %v", logfmt.UID8(a.UID), err)
		return "失败"
	}
	err := s.cfg.Upstream.BuddyFirst(a)
	switch {
	case err == nil:
		log.Printf("travel %s: adopt ok (+300 credits)", logfmt.UID8(a.UID))
		return "领养"
	case upstream.IsBuddyTaskIncomplete(err):
		s.markAdoptTried(a.UID)
		log.Printf("travel %s: adopt skipped (conversation threshold not reached, retry tomorrow)", logfmt.UID8(a.UID))
		return "跳过"
	default:
		log.Printf("travel %s: adopt: %v", logfmt.UID8(a.UID), err)
		return "失败"
	}
}

// adoptTriedToday 该账号当日是否已判定领养门槛未达。
func (s *Scheduler) adoptTriedToday(uid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adoptTried[uid] == travelDay(time.Now())
}

// markAdoptTried 记录该账号当日已尝试领养且未过门槛。
func (s *Scheduler) markAdoptTried(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adoptTried[uid] = travelDay(time.Now())
}
