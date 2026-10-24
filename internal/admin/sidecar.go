// sidecar.go admin 侧 mihomo sidecar 端点：状态查询 / 拉取同步 / 一键分配。
//
// 与 internal/admin/proxy.go 的分工：proxy.go 管**手工**槽位与绑定的增删改查；
// 本文件管**订阅驱动**的自动化（拉订阅 → 展开出口端口 → 写槽位 → 摊账号）。
//
// 落盘与热重载复用 proxy.go 的既有机制（setTopLevelValue + persistConfigEdits +
// Registry.Set + ResetProxyTransports），保持「除被改的键外逐字段相同」的一致性断言。
package admin

import (
	"io"
	"net/http"
	"strings"
)

// SidecarController 网关侧 sidecar 的能力面（由 cmd/server 注入实现）。
//
// 用接口而非直连具体类型：admin 不该依赖 cmd/server 的编排结构，
// 也便于测试注入桩。
type SidecarController interface {
	// Enabled 是否启用（配置了订阅且 enabled=true）。
	Enabled() bool
	// Status 返回状态快照（节点数/槽位数/二进制可用性/mihomo 进程态）。
	Status() SidecarStatus
	// Sync 拉取订阅并展开为出口槽位，返回本次生成的槽位。
	// body 非空时用该内容作为订阅（面板粘贴/上传路径），否则按配置的 URL 拉取。
	Sync(body []byte) (SidecarSyncResult, error)
	// Assign 把给定账号粘性摊到 sidecar 槽位上，返回新增的绑定（已绑定的不动）。
	Assign(uids []string) (map[string]string, error)
	// AccountUIDs 返回当前池内账号 uid 列表（供「全部账号自动分配」）。
	AccountUIDs() []string
}

// ProxyHealthInfo 一个出口代理的健康快照（与 upstream.ProxyHealthSnapshot 同形，
// 独立声明以免 admin 依赖 upstream 的其它部分）。
type ProxyHealthInfo struct {
	Unhealthy  bool   `json:"unhealthy"`
	Fails      int    `json:"fails"`
	LastErr    string `json:"last_error"`
	CooldownMs int64  `json:"cooldown_ms"`
	// ProbedAt 最近一次记账时刻（RFC3339）。供面板显示「多久没探过」。
	ProbedAt string `json:"probed_at,omitempty"`
}

// SidecarStatus 状态快照（与 cmd/server 的同名字段对齐；此处独立声明以免 admin
// 反向依赖 cmd/server）。
type SidecarStatus struct {
	Enabled    bool             `json:"enabled"`
	NodeCount  int              `json:"node_count"`
	SlotCount  int              `json:"slot_count"`
	LastSync   string           `json:"last_sync"`
	LastError  string           `json:"last_error"`
	BasePort   int              `json:"base_port"`
	MaxNodes   int              `json:"max_nodes"`
	BinaryOK   bool             `json:"binary_ok"`
	BinaryPath string           `json:"binary_path"`
	Engines    []map[string]any `json:"engines"`
}

// SidecarSyncResult 一次同步的结果。
type SidecarSyncResult struct {
	Slots     []SidecarSlot `json:"slots"`
	NodeCount int           `json:"node_count"`
	FetchedAt string        `json:"fetched_at"`
	Warnings  []string      `json:"warnings"`
}

// SidecarSlot 展开出的一个出口槽位（与 proxyreg.Slot 同形）。
type SidecarSlot struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	URL     string `json:"url"`
	Enabled bool   `json:"enabled"`
}

// getSidecar GET /__admin/proxy/sidecar —— 状态查询。
func (h *Handler) getSidecar(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Sidecar == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled": false,
			"message": "sidecar 未接线（config 的 proxy_sidecar.enabled 或本进程未启用）",
		})
		return
	}
	writeJSON(w, http.StatusOK, h.cfg.Sidecar.Status())
}

// postSidecarSync POST /__admin/proxy/sidecar/sync —— 拉取订阅并展开槽位。
//
// 两种入参（互斥，优先 body）：
//   - {"subscription": "<订阅原文>"}：面板粘贴/上传，用于订阅站无法直连时。
//   - 空 body：按 config 的 proxy_sidecar.subscription_url 拉取。
//
// 同步后**写盘**（proxy_slots 只增改 sidecar 前缀的项，手工槽位保留）并热重载。
func (h *Handler) postSidecarSync(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Sidecar == nil {
		fail(w, http.StatusServiceUnavailable, "sidecar 未接线")
		return
	}
	var body struct {
		Subscription string `json:"subscription"`
	}
	// 允许空 body / 非 JSON：此时按配置 URL 拉取。
	_ = decodeBodyOptional(r, &body)

	var raw []byte
	if s := strings.TrimSpace(body.Subscription); s != "" {
		raw = []byte(s)
	}
	// raw 为 nil → 按配置的订阅 URL 拉取；非 nil（含空串粘贴）→ 用该内容。
	res, err := h.cfg.Sidecar.Sync(raw)
	if err != nil {
		fail(w, http.StatusBadGateway, "%v", err)
		return
	}

	// 落盘 + 热重载（复用 proxy.go 的槽位保存路径）
	if err := h.persistSidecarSlots(res); err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	logf("sidecar 同步：%d 个节点 → %d 个槽位", res.NodeCount, len(res.Slots))
	writeJSON(w, http.StatusOK, res)
}

// postSidecarAssign POST /__admin/proxy/sidecar/assign —— 把账号粘性摊到槽位。
//
// 入参 {"uids": [...]} 省略/为空 = 全部池内账号。
// **已绑定到有效槽位的账号不动**（保持出口 IP 恒定），只分配未绑定/绑定失效的。
func (h *Handler) postSidecarAssign(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Sidecar == nil {
		fail(w, http.StatusServiceUnavailable, "sidecar 未接线")
		return
	}
	var body struct {
		UIDs []string `json:"uids"`
	}
	_ = decodeBodyOptional(r, &body)
	uids := body.UIDs
	if len(uids) == 0 {
		uids = h.cfg.Sidecar.AccountUIDs()
	}
	// 前置防护：若槽位尚未落盘（config 里 proxy_slots 为空），直接分配会写出
	// 「绑定指向不存在的槽位」的坏配置。先要求同步一次。
	if h.cfg.Registry == nil || len(h.cfg.Registry.Slots()) == 0 {
		fail(w, http.StatusConflict, "尚无槽位：请先执行「同步订阅」把出口写成槽位，再分配账号")
		return
	}
	added, err := h.cfg.Sidecar.Assign(uids)
	if err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	if len(added) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"assigned": 0,
			"message":  "所有账号均已绑定到有效槽位（粘性绑定保持不变）",
		})
		return
	}
	// 合并进现有绑定表后落盘 + 热重载
	if err := h.persistSidecarBinds(added); err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	logf("sidecar 分配：%d 个账号新增绑定", len(added))
	writeJSON(w, http.StatusOK, map[string]any{"assigned": len(added), "binds": added})
}

// decodeBodyOptional 宽松解码：允许空 body 或非 JSON（视为全零值），供「可选入参」端点用。
func decodeBodyOptional(r *http.Request, v any) error {
	if r == nil || r.Body == nil {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	return jsonUnmarshalLenient(b, v)
}
