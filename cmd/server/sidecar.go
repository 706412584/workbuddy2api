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
	bindings, err := mihomosub.BuildBindings(
		nodes, s.cfg.NamePrefix, "127.0.0.1", "mixed",
		s.cfg.BasePort, s.cfg.MaxNodes, nil, nil,
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

	slots := make([]ProxySlot, 0, len(bindings))
	for _, b := range bindings {
		slots = append(slots, ProxySlot{
			ID:      b.Name, // 用「节点 identity 派生的稳定名」作槽位 id：同名即同节点
			Name:    b.Node.Name,
			URL:     fmt.Sprintf("http://127.0.0.1:%d", b.Port),
			Enabled: true,
		})
	}

	res := &SyncResult{Slots: slots, NodeCount: len(nodes), FetchedAt: time.Now()}
	if len(nodes) > len(bindings) {
		res.Warnings = append(res.Warnings,
			fmt.Sprintf("订阅含 %d 个节点，按 max_nodes=%d 只展开前 %d 个", len(nodes), s.cfg.MaxNodes, len(bindings)))
	}

	s.mu.Lock()
	s.slots = slots
	s.nodeCount = len(nodes)
	s.lastSync = time.Now()
	s.lastErr = ""
	s.mu.Unlock()

	// 注册表接线与落盘由 admin 层负责（见 admin.persistSidecarSlots）：
	// 本层只管「订阅 → mihomo → 槽位定义」，不碰 config.json，避免两层都写配置。
	log.Printf("[sidecar] 同步完成：%d 个节点 → %d 个出口端口（%d..%d），dataDir=%s",
		len(nodes), len(bindings), s.cfg.BasePort, s.cfg.BasePort+len(bindings)-1, s.dataDir)
	return res, nil
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
