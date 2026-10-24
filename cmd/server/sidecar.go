// sidecar.go 订阅 → 本地出口端口 → proxy_slots → 账号绑定的编排层。
//
// 职责分层（避免把网络/文件/业务耦合在一起）：
//   - internal/mihomosub：纯 mihomo 侧（解析订阅、生成配置、起进程、探活）。
//   - 本文件：把 mihomo 的 N 个 listener 翻译成网关的 proxy_slots，并把 N 个
//     账号粘性摊到 M 个槽位上；负责拉订阅、落盘、接线 Registry、重启后恢复。
//   - internal/proxyreg：运行期槽位/绑定注册表（唯一事实来源）。
//
// 关键不变量：**账号 → 槽位 的绑定是粘性的**（proxyreg.Resolve 按 uid 查）。
// 只要订阅节点集合不变，BuildBindings 的「按 identity 排序 + port=base+i」保证
// 每个节点永远拿到同一个端口、同一个代理名 → 账号出口 IP 恒定。机场侧重排顺序
// 不会导致端口漂移，因而不会让已绑定的账号换出口 IP。
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/mihomosub"
	"workbuddy2api/internal/upstream"
)

// sidecarHTTPTimeout 拉订阅/探活的超时。订阅通常几百 KB，30s 足够。
const sidecarHTTPTimeout = 30 * time.Second

// sidecarDefaultBasePort / sidecarDefaultMaxNodes / sidecarDefaultPrefix 缺省值。
// base 21080 沿用社区约定，刻意避开网关 7863 与 FlClash 控制口 9090。
const (
	sidecarDefaultBasePort      = 21080
	sidecarDefaultMaxNodes      = 10
	sidecarDefaultPrefix        = "sidecar-"
	sidecarDefaultSyncIntervalS = 3600
)

// Sidecar 编排订阅展开与账号分配。
type Sidecar struct {
	mu      sync.Mutex
	engine  *mihomosub.Engine
	cfg     ProxySidecarConfig
	dataDir string
	// lastSync 最近一次同步结果（供面板展示）。
	lastSync    time.Time
	lastErr     string
	nodeCount   int
	slots       []ProxySlot // 本次同步生成的槽位（未含手工槽位）
	subFetched  time.Time
	stopCh      chan struct{}
	stopped     bool
	httpClient  *http.Client
	resolvePath func(string) string // 相对路径 → 绝对路径（注入便于测试）
}

// NewSidecar 构造编排器。dataDir 为空时按 state_file 同级推导。
func NewSidecar(cfg ProxySidecarConfig, stateFile string, abs func(string) string) *Sidecar {
	if abs == nil {
		abs = func(p string) string { return p }
	}
	cfg = normalizeSidecar(cfg)
	dataDir := cfg.DataDir
	if dataDir == "" {
		base := filepath.Dir(stateFile)
		if base == "" || base == "." {
			base = "data"
		}
		dataDir = filepath.Join(base, "proxy-sidecar")
	}
	// binary 解析：空 → 先找网关同目录 mihomo.exe，否则交给引擎按 PATH 兜底。
	bin := strings.TrimSpace(cfg.Binary)
	if bin == "" {
		if cand := abs("mihomo.exe"); cand != "" {
			bin = cand
		}
	}
	s := &Sidecar{
		engine:  mihomosub.NewEngine(bin, abs(dataDir)),
		cfg:     cfg,
		dataDir: abs(dataDir),
		stopCh:  make(chan struct{}),
		httpClient: &http.Client{
			Timeout: sidecarHTTPTimeout,
			// 显式不走环境变量代理：订阅站常要求直连，且环境里的代理会时通时断。
			Transport: &http.Transport{Proxy: nil},
		},
		resolvePath: abs,
	}
	return s
}

// normalizeSidecar 补齐缺省值并做区间校验（只纠正明显非法值，不报错中断启动）。
func normalizeSidecar(c ProxySidecarConfig) ProxySidecarConfig {
	if c.BasePort < 1024 || c.BasePort > 65000 {
		c.BasePort = sidecarDefaultBasePort
	}
	if c.MaxNodes <= 0 {
		c.MaxNodes = sidecarDefaultMaxNodes
	}
	if c.MaxNodes > 500 {
		c.MaxNodes = 500
	}
	if c.BasePort+c.MaxNodes-1 > 65535 {
		c.MaxNodes = 65535 - c.BasePort + 1
	}
	if strings.TrimSpace(c.NamePrefix) == "" {
		c.NamePrefix = sidecarDefaultPrefix
	}
	if c.SyncIntervalSec == 0 {
		c.SyncIntervalSec = sidecarDefaultSyncIntervalS
	}
	return c
}

// Enabled 报告是否启用（需要 URL + Enabled）。
func (s *Sidecar) Enabled() bool {
	return s != nil && s.cfg.Enabled && strings.TrimSpace(s.cfg.SubscriptionURL) != ""
}

// Engine 暴露引擎（供 main 关停时 StopAll）。
func (s *Sidecar) Engine() *mihomosub.Engine {
	if s == nil {
		return nil
	}
	return s.engine
}

// SyncResult 一次同步的结果。
type SyncResult struct {
	Slots     []ProxySlot       `json:"slots"` // 本次生成的槽位
	Binds     map[string]string `json:"binds"` // 建议的 uid→slotID 分配（仅未绑定的账号）
	NodeCount int               `json:"node_count"`
	FetchedAt time.Time         `json:"fetched_at"`
	Warnings  []string          `json:"warnings"`
}

// Sync 拉订阅 → 解析 → 起 mihomo → 生成槽位。**不**改账号绑定（由 Assign 或面板决定）。
func (s *Sidecar) Sync() (*SyncResult, error) {
	if !s.Enabled() {
		return nil, fmt.Errorf("sidecar 未启用（需 proxy_sidecar.enabled=true 且填 subscription_url）")
	}
	body, err := s.fetchSubscription()
	if err != nil {
		s.setErr(err)
		return nil, err
	}
	return s.applySubscription(body)
}

// ApplySubscriptionBody 用**外部提供的订阅内容**（如面板粘贴/上传）执行同步。
// 便于订阅 URL 不可直达（需代理）时，让用户手动贴订阅文本。
func (s *Sidecar) ApplySubscriptionBody(body []byte) (*SyncResult, error) {
	if !s.cfg.Enabled {
		return nil, fmt.Errorf("sidecar 未启用（需 proxy_sidecar.enabled=true）")
	}
	return s.applySubscription(body)
}

func (s *Sidecar) applySubscription(body []byte) (*SyncResult, error) {
	nodes, err := mihomosub.ParseSubscription(body)
	if err != nil {
		s.setErr(err)
		return nil, fmt.Errorf("订阅解析失败：%w", err)
	}

	// 探测候选池必须**大于** max_nodes：否则「先取前 N 个再探测」会把
	// 「前 N 个恰好都失效」变成「同步失败」，而订阅后面明明还有能用的节点。
	// 上限取 min(节点数, max(max_nodes*3, max_nodes+8))，兼顾「够补」与「别探太久」。
	probeCap := s.probeCap(len(nodes))
	bindings, err := mihomosub.BuildBindings(
		nodes, s.cfg.NamePrefix, "127.0.0.1", "mixed",
		s.cfg.BasePort, probeCap, nil, nil,
	)
	if err != nil {
		s.setErr(err)
		return nil, fmt.Errorf("节点选择失败：%w", err)
	}
	if len(bindings) == 0 {
		err := fmt.Errorf("订阅里没有可用节点")
		s.setErr(err)
		return nil, err
	}

	sourceID := "main"
	if err := s.engine.EnsureRunning(sourceID, s.cfg.NamePrefix, "127.0.0.1", bindings, ""); err != nil {
		s.setErr(err)
		return nil, fmt.Errorf("启动 mihomo 失败：%w", err)
	}

	// ── 出口健康探测（2026-10-24 新增）────────────────────────────────
	//
	// 为什么必须在同步时探测：槽位是从订阅生成的，而机场会**轮换节点**。
	// 实测（2026-10-24）：订阅里 5 个 lxyun 域名节点全部不可用，展开成槽位后
	// 经它们访问 generate_204 一律 502/超时，但面板健康列**全绿** —— 因为健康
	// 账只记「真实出站」，而这些槽位从没被用过。用户把账号绑上去才发现全废。
	//
	// 语义选择（按用户「按你建议」的决定）：
	//   - 探测不通的节点**丢弃**，并用探测通的节点补足到 max_nodes；
	//   - **全部不通** → 返回错误且**不落盘**（保留原配置）。
	//     账号失去出口比报错严重得多：宁可这次同步失败，让用户看到原因。
	//
	// 顺序必须先启动后探测：探测要经本地 mixed 端口发出，而端口是 mihomo 起的。
	// 探测用的端口就是候选池的端口（basePort..basePort+probeCap-1），
	// 与最终槽位端口一致（BuildBindings 的 port = base+i 由 identity 排序决定），
	// 故探测的 URL 与最终槽位 URL 逐字相同 —— 健康账的键天然对齐。
	alive, dead := s.probeBindings(bindings)
	if len(alive) == 0 {
		err := fmt.Errorf("订阅里 %d 个候选节点全部探测不通（经出口访问 %s 均失败）——"+
			"订阅可能已失效，或节点已被机场轮换。本次同步**未改动**原槽位配置",
			len(bindings), upstream.ProbeURL)
		s.setErr(err)
		return nil, err
	}
	// 截断到 max_nodes（探测通过的按稳定顺序取前 N 个）
	alive = selectAlive(alive, s.cfg.MaxNodes)
	// 探测筛选后集合变化 → 用存活节点重写配置并重启（端口随之压缩到 base..base+len-1）
	if len(alive) != len(bindings) {
		if err := s.engine.EnsureRunning(sourceID, s.cfg.NamePrefix, "127.0.0.1", alive, ""); err != nil {
			s.setErr(err)
			return nil, fmt.Errorf("按可用节点重启 mihomo 失败：%w", err)
		}
	}

	slots := make([]ProxySlot, 0, len(alive))
	for _, b := range alive {
		slots = append(slots, ProxySlot{
			ID:      b.Name, // 用「节点 identity 派生的稳定名」作槽位 id：同名即同节点
			Name:    b.Node.Name,
			URL:     fmt.Sprintf("http://127.0.0.1:%d", b.Port),
			Enabled: true,
		})
	}

	res := &SyncResult{Slots: slots, NodeCount: len(nodes), FetchedAt: time.Now()}
	res.Warnings = append(res.Warnings,
		fmt.Sprintf("探测 %d 个候选出口：%d 个可用、%d 个不可用（已丢弃）",
			len(bindings), len(alive), len(dead)))
	for _, d := range dead {
		res.Warnings = append(res.Warnings,
			fmt.Sprintf("丢弃 %s：%s", d.Name, firstLine(d.Err)))
	}
	if len(nodes) > probeCap {
		res.Warnings = append(res.Warnings,
			fmt.Sprintf("订阅共 %d 个节点，本次只探测了前 %d 个（够补足 %d 个可用出口即停）",
				len(nodes), probeCap, s.cfg.MaxNodes))
	}

	s.mu.Lock()
	s.slots = slots
	s.nodeCount = len(nodes)
	s.lastSync = time.Now()
	s.lastErr = ""
	s.mu.Unlock()

	// 注册表接线与落盘由 admin 层负责（见 admin.persistSidecarSlots）：
	// 本层只管「订阅 → mihomo → 槽位定义」，不碰 config.json，避免两层都写配置。
	log.Printf("[sidecar] 同步完成：%d 个节点 → 探测 %d 个候选 → 保留 %d 个可用出口（%d..%d），dataDir=%s",
		len(nodes), len(bindings), len(alive), s.cfg.BasePort, s.cfg.BasePort+len(alive)-1, s.dataDir)
	return res, nil
}

// probeCap 返回本次要展开并探测的候选节点数。
//
// 必须 > max_nodes：给「前 N 个失效」留补位余量。上限同时受节点总数与
// probeCeiling 约束 —— 每个候选要占一个本地端口并起一条 mihomo listener，
// 探太多既慢又浪费端口。
func (s *Sidecar) probeCap(total int) int {
	if total <= 0 {
		return 0
	}
	want := s.cfg.MaxNodes*3 + 8
	if want > probeCeiling {
		want = probeCeiling
	}
	if want > total {
		want = total
	}
	return want
}

// probeCeiling 候选探测数的硬上限。
const probeCeiling = 40

// selectAlive 从探测通过的候选里取前 max 个（候选已按 identity 稳定排序，
// 故取前 N 个是确定性的：同一份订阅、同一批可用节点 → 同一组槽位）。
func selectAlive(alive []mihomosub.Binding, max int) []mihomosub.Binding {
	if max <= 0 || len(alive) <= max {
		return alive
	}
	return alive[:max]
}

// probeDead 一个探测失败的候选。
type probeDead struct {
	Name string
	Err  string
}

// probeBindings 并发探测每个候选出口，返回（存活、失败）。
//
// 并发度固定 probeConcurrency：串行 40 个 × 最坏 12s = 8 分钟不可接受；
// 全并发又会对本机 mihomo 与上游节点同时发起几十条连接。8 路约一到两轮。
func (s *Sidecar) probeBindings(bindings []mihomosub.Binding) (alive []mihomosub.Binding, dead []probeDead) {
	type outcome struct {
		b   mihomosub.Binding
		ok  bool
		err string
	}
	sem := make(chan struct{}, probeConcurrency)
	results := make([]outcome, len(bindings))
	var wg sync.WaitGroup

	// 每路探测的总时长上限（dial 5s + 转发 8s 已在 ProbeProxyURL 内各自封顶；
	// 这里再给一个更紧的整体上限，避免某个节点把整轮拖满）。
	ctx, cancel := context.WithTimeout(context.Background(), probeRoundTimeout)
	defer cancel()

	for i, b := range bindings {
		wg.Add(1)
		go func(i int, b mihomosub.Binding) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			url := fmt.Sprintf("http://127.0.0.1:%d", b.Port)
			r := probeFn(ctx, url)
			results[i] = outcome{b: b, ok: r.OK, err: r.Error}
		}(i, b)
	}
	wg.Wait()

	for _, r := range results {
		if r.ok {
			alive = append(alive, r.b)
			continue
		}
		dead = append(dead, probeDead{Name: r.b.Node.Name, Err: r.err})
	}
	return alive, dead
}

// probeConcurrency 探测并发度。
const probeConcurrency = 8

// probeFn 实际执行一次出口探测。抽成变量是为了让测试注入桩 ——
// 探测要真起 mihomo 并经它出网，单测里做不到（也不该做）。
var probeFn = upstream.ProbeProxyURL

// probeRoundTimeout 一轮探测的整体时长上限。
const probeRoundTimeout = 45 * time.Second

// firstLine 取错误首行（探测错误的模板很长，面板里只放一行够用）。
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Assign 把池中账号**粘性**摊到 sidecar 槽位上。
//
// 粘性口径：已绑定到**在册槽位**的账号**不改**（保持出口恒定）；只有未绑定（或
// 绑定指向已消失槽位）的账号才分配。这样反复点「自动分配」不会让已有账号换出口。
func (s *Sidecar) Assign(uids []string) (map[string]string, error) {
	s.mu.Lock()
	slots := append([]ProxySlot(nil), s.slots...)
	s.mu.Unlock()
	if len(slots) == 0 {
		return nil, fmt.Errorf("尚无 sidecar 槽位，请先同步订阅")
	}
	sort.Strings(uids)
	// 现有绑定从运行期注册表取（唯一事实来源；config 只是它的持久化形态）。
	existing := map[string]string{}
	if proxyRegistry != nil {
		existing = proxyRegistry.Binds()
	}

	// 槽位负载计数（已绑定的算入，尽量摊平）
	load := map[string]int{}
	for _, sid := range existing {
		load[sid]++
	}
	valid := map[string]bool{}
	for _, sl := range slots {
		valid[sl.ID] = true
	}
	out := map[string]string{}
	for _, uid := range uids {
		if uid == "" {
			continue
		}
		if sid, ok := existing[uid]; ok && valid[sid] {
			continue // 已粘性绑定到有效槽位 → 不动
		}
		// 选当前负载最小的槽位（并列时取 id 最小，保证确定性）
		best := ""
		bestLoad := 1 << 30
		for _, sl := range slots {
			if load[sl.ID] < bestLoad || (load[sl.ID] == bestLoad && sl.ID < best) {
				best, bestLoad = sl.ID, load[sl.ID]
			}
		}
		if best == "" {
			continue
		}
		out[uid] = best
		load[best]++
	}
	return out, nil
}

// Status 供面板展示的同步状态。
type SidecarStatus struct {
	Enabled    bool               `json:"enabled"`
	NodeCount  int                `json:"node_count"`
	SlotCount  int                `json:"slot_count"`
	LastSync   string             `json:"last_sync"`
	LastError  string             `json:"last_error"`
	BasePort   int                `json:"base_port"`
	MaxNodes   int                `json:"max_nodes"`
	BinaryOK   bool               `json:"binary_ok"`
	BinaryPath string             `json:"binary_path"`
	Engines    []mihomosub.Status `json:"engines"`
}

// Status 返回当前状态快照。
func (s *Sidecar) Status() SidecarStatus {
	st := SidecarStatus{Enabled: s.Enabled()}
	if s == nil {
		return st
	}
	bin, ok := s.engine.ResolveBinary()
	st.BinaryOK, st.BinaryPath = ok, bin
	st.BasePort, st.MaxNodes = s.cfg.BasePort, s.cfg.MaxNodes
	st.Engines = s.engine.StatusSnapshot()
	s.mu.Lock()
	st.NodeCount = s.nodeCount
	st.SlotCount = len(s.slots)
	if !s.lastSync.IsZero() {
		st.LastSync = s.lastSync.Format(time.RFC3339)
	}
	st.LastError = s.lastErr
	s.mu.Unlock()
	return st
}

// StartAutoSync 起后台定时刷新（SyncIntervalSec<=0 时不刷）。
func (s *Sidecar) StartAutoSync() {
	if !s.Enabled() || s.cfg.SyncIntervalSec <= 0 {
		return
	}
	iv := time.Duration(s.cfg.SyncIntervalSec) * time.Second
	go func() {
		t := time.NewTicker(iv)
		defer t.Stop()
		for {
			select {
			case <-s.stopCh:
				return
			case <-t.C:
				if _, err := s.Sync(); err != nil {
					log.Printf("[sidecar] 定时同步失败：%v", err)
				}
			}
		}
	}()
	log.Printf("[sidecar] 自动同步已启用，间隔 %s", iv)
}

// Stop 停后台同步与 mihomo 子进程。
func (s *Sidecar) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	s.mu.Unlock()
	close(s.stopCh)
	s.engine.StopAll()
}

// fetchSubscription 拉订阅，自动处理 gzip。
func (s *Sidecar) fetchSubscription() ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, s.cfg.SubscriptionURL, nil)
	if err != nil {
		return nil, fmt.Errorf("订阅 URL 非法：%w", err)
	}
	// 与社区一致：伪装 Clash 客户端，很多机场据此返回 Clash YAML 而非 base64。
	req.Header.Set("User-Agent", "clash.meta/1.19.0")
	req.Header.Set("Accept", "*/*")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("拉取订阅失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("拉取订阅失败：HTTP %d", resp.StatusCode)
	}
	// 限长：订阅通常几百 KB；8MB 足够且防被喂超大响应。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("读取订阅失败：%w", err)
	}
	if len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("订阅 gzip 解压失败：%w", err)
		}
		defer zr.Close()
		if raw, err = io.ReadAll(io.LimitReader(zr, 8<<20)); err != nil {
			return nil, fmt.Errorf("订阅 gzip 解压读取失败：%w", err)
		}
	}
	return raw, nil
}

func (s *Sidecar) setErr(err error) {
	s.mu.Lock()
	s.lastErr = err.Error()
	s.mu.Unlock()
	log.Printf("[sidecar] %v", err)
}
