// proxyhealth.go 出站代理（含 mihomo sidecar 本地端口）的健康登记与探测。
//
// 要解决的问题（2026-10-21 发现）：账号绑定的代理不可达时，ChatStream 会返回传输层
// 错误，而调用方把它当「网络抖动」处理 —— 只换号不罚账号（这点是对的），但：
//
//  1. **换号≠换代理**：同一槽位可能被多个账号共用（实测一个 sidecar 槽位挂 4 个号），
//     轮换会把它们逐个试一遍，全撞同一个死代理，白耗 MaxRotate 名额。
//  2. **无槽位健康状态**：死槽位不被标记，下次请求照选不误。
//  3. **无法区分**：代理挂了 vs 上游网络抖动，二者都是 transport error。
//
// 本文件提供三样东西：
//   - 按代理 URL 记账的连续失败计数（NoteProxyResult）。
//   - 达阈值后**主动 TCP 探测**确认（Probe），避免把上游抖动误判成代理故障。
//   - 不健康名单（Unhealthy），供选号排除「走坏槽位的账号」。
//
// 与 pool 的账号冷却是**正交**的两层：pool 记「这个号是不是坏了」，本层记
// 「这个号走的出口是不是坏了」。故意不复用 pool：一个槽位对应多个账号，
// 按槽位记账才能一次摘掉全部受害账号，而不是逐个罚。
package upstream

import (
	"net"
	"strings"
	"sync"
	"time"
)

// 代理健康参数。
const (
	// proxyFailThreshold 连续失败多少次才触发主动探测。
	// 首次失败不探测（只换号，代价可接受），避免给每个偶发抖动都加一次 RTT。
	proxyFailThreshold = 2
	// proxyProbeTimeout 探测代理端口可连性的超时。
	// 只做 TCP 连通（不建完整代理会话）——目的是区分「进程/端口没了」与
	// 「代理活着但上游抖动」，TCP 够用且快。
	proxyProbeTimeout = 1500 * time.Millisecond
	// proxyUnhealthyTTL 判定不健康后的冷却时长，到期自动重新探测（给自愈出口）。
	proxyUnhealthyTTL = 60 * time.Second
)

// proxyHealth 按代理 URL 记账的健康登记表。
type proxyHealth struct {
	mu sync.Mutex
	m  map[string]*proxyEntry
}

type proxyEntry struct {
	// fails 连续失败计数（成功即清零）。
	fails int
	// unhealthyUntil 判定不健康后的冷却截止；零值 = 健康。
	unhealthyUntil time.Time
	// lastErr 最近一次失败原因（供面板展示）。
	lastErr string
	// probedAt 最近一次主动探测时刻（避免并发请求同时探测同一代理）。
	probedAt time.Time
}

var proxyHealthReg = &proxyHealth{m: map[string]*proxyEntry{}}

// NoteProxyResult 记一次经该代理的出站结果。success=true 会立即清除不健康状态
// （代理自愈最可靠的信号就是「真的有请求穿过去了」）。
func NoteProxyResult(proxyURL string, success bool, errMsg string) {
	if strings.TrimSpace(proxyURL) == "" {
		return // 直连不记账
	}
	proxyHealthReg.mu.Lock()
	defer proxyHealthReg.mu.Unlock()
	e := proxyHealthReg.m[proxyURL]
	if e == nil {
		e = &proxyEntry{}
		proxyHealthReg.m[proxyURL] = e
	}
	if success {
		e.fails = 0
		e.unhealthyUntil = time.Time{}
		e.lastErr = ""
		return
	}
	e.fails++
	e.lastErr = errMsg
}

// ProxyUnhealthy 报告该代理当前是否被判定为不健康（供选号排除）。
// 冷却到期后自动视为健康（下次经它的请求会重新验证）。
func ProxyUnhealthy(proxyURL string) bool {
	if strings.TrimSpace(proxyURL) == "" {
		return false
	}
	proxyHealthReg.mu.Lock()
	defer proxyHealthReg.mu.Unlock()
	e := proxyHealthReg.m[proxyURL]
	if e == nil || e.unhealthyUntil.IsZero() {
		return false
	}
	if time.Now().After(e.unhealthyUntil) {
		return false // 冷却到期，给自愈机会
	}
	return true
}

// ProxyFails 返回该代理的连续失败计数（供测试与诊断）。
func ProxyFails(proxyURL string) int {
	proxyHealthReg.mu.Lock()
	defer proxyHealthReg.mu.Unlock()
	if e := proxyHealthReg.m[proxyURL]; e != nil {
		return e.fails
	}
	return 0
}

// ProxyHealthSnapshot 导出健康快照（供面板 /__admin/proxy/* 展示）。
type ProxyHealthSnapshot struct {
	URL        string `json:"url"`
	Fails      int    `json:"fails"`
	Unhealthy  bool   `json:"unhealthy"`
	LastErr    string `json:"last_error"`
	CooldownMs int64  `json:"cooldown_ms"`
}

// ProxyHealth 导出全部代理健康快照。
func ProxyHealth() []ProxyHealthSnapshot {
	proxyHealthReg.mu.Lock()
	defer proxyHealthReg.mu.Unlock()
	out := make([]ProxyHealthSnapshot, 0, len(proxyHealthReg.m))
	now := time.Now()
	for u, e := range proxyHealthReg.m {
		s := ProxyHealthSnapshot{URL: sanitizeProxyURL(u), Fails: e.fails, LastErr: e.lastErr}
		if !e.unhealthyUntil.IsZero() && now.Before(e.unhealthyUntil) {
			s.Unhealthy = true
			s.CooldownMs = e.unhealthyUntil.Sub(now).Milliseconds()
		}
		out = append(out, s)
	}
	return out
}

// ResetProxyHealth 清空健康登记（代理配置变更时调用——旧 URL 的记录已无意义）。
func ResetProxyHealth() {
	proxyHealthReg.mu.Lock()
	proxyHealthReg.m = map[string]*proxyEntry{}
	proxyHealthReg.mu.Unlock()
}

// MaybeProbeProxy 判断是否需要对该代理做主动探测，需要则探测并在失败时标记不健康。
//
// 返回是否把该代理标记成了不健康（调用方据此决定「这个请求要不要换槽位重试」）。
//
// 探测节流：同一代理在探活窗口内只探一次（probedAt），避免一个坏槽位上的
// N 个账号并发请求各探一次（N 次 1.5s RTT 的浪费）。
//
// 调用时机：**仅在传输层失败且失败计数达阈值时**调用（见 clientFor 的用法），
// 故正常路径零开销。
func MaybeProbeProxy(proxyURL string) bool {
	if strings.TrimSpace(proxyURL) == "" {
		return false
	}
	proxyHealthReg.mu.Lock()
	e := proxyHealthReg.m[proxyURL]
	if e == nil {
		proxyHealthReg.mu.Unlock()
		return false
	}
	if e.fails < proxyFailThreshold {
		proxyHealthReg.mu.Unlock()
		return false
	}
	// 已在冷却中：直接答不健康，不必重复探测。
	if !e.unhealthyUntil.IsZero() && time.Now().Before(e.unhealthyUntil) {
		proxyHealthReg.mu.Unlock()
		return true
	}
	// 节流：1.5s 内已探过就不重复探（返回上次结论）。
	now := time.Now()
	if now.Sub(e.probedAt) < proxyProbeTimeout {
		marked := !e.unhealthyUntil.IsZero() && now.Before(e.unhealthyUntil)
		proxyHealthReg.mu.Unlock()
		return marked
	}
	e.probedAt = now
	proxyHealthReg.mu.Unlock()

	// 探测在锁外做（网络 IO 不能持锁）。
	ok := probeProxyReachable(proxyURL)

	proxyHealthReg.mu.Lock()
	defer proxyHealthReg.mu.Unlock()
	e2 := proxyHealthReg.m[proxyURL]
	if e2 == nil {
		return false
	}
	if ok {
		// 端口通 → 是上游抖动而非代理故障，清计数（不标不健康）。
		e2.fails = 0
		e2.unhealthyUntil = time.Time{}
		e2.lastErr = ""
		return false
	}
	e2.unhealthyUntil = time.Now().Add(proxyUnhealthyTTL)
	if e2.lastErr == "" {
		e2.lastErr = "代理端口不可达"
	}
	return true
}

// probeProxyReachable 对代理地址做一次 TCP 连通探测。
// 只测「端口能不能连上」：代理进程/端口消失是最常见也最确定的故障形态；
// 完整代理会话验证成本高且易被上游抖动干扰，不适合放在请求路径上。
func probeProxyReachable(proxyURL string) bool {
	pc, err := parseProxy(proxyURL)
	if err != nil {
		return false // URL 本身非法 → 视为不可用
	}
	host := pc.u.Host
	if !strings.Contains(host, ":") {
		// 缺端口时按协议默认端口补（http 代理常用 8080，但这里给不出可靠默认，
		// 故只在有端口时探测；无端口视为不可确认 → 不判不健康，宁可放过）。
		return true
	}
	conn, err := net.DialTimeout("tcp", host, proxyProbeTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// ProxyHealthByURL 按**槽位原始 URL** 查健康（未记账返回 false）。
//
// 键用原始 URL：admin 展示槽位时拿的是 config 里的原文，而记账键来自
// Registry.Resolve（同样是原文），二者天然一致，不需要再做规范化。
func ProxyHealthByURL(rawURL string) (ProxyHealthSnapshot, bool) {
	if strings.TrimSpace(rawURL) == "" {
		return ProxyHealthSnapshot{}, false
	}
	proxyHealthReg.mu.Lock()
	defer proxyHealthReg.mu.Unlock()
	e := proxyHealthReg.m[rawURL]
	if e == nil {
		return ProxyHealthSnapshot{}, false
	}
	now := time.Now()
	s := ProxyHealthSnapshot{URL: sanitizeProxyURL(rawURL), Fails: e.fails, LastErr: e.lastErr}
	if !e.unhealthyUntil.IsZero() && now.Before(e.unhealthyUntil) {
		s.Unhealthy = true
		s.CooldownMs = e.unhealthyUntil.Sub(now).Milliseconds()
	}
	return s, true
}
