// proxy.go 出站代理槽位与账号绑定的管理接口（/__admin/proxy/*）。
//
// 持久化：写回 config.json 的 `proxy_slots` 与 `account_proxies` 两个顶层键，
// 沿用 keys.go 的 setTopLevelValue（原地改文本区间，保留其余字节与排版）。
//
// 热重载：保存后立即 SetRegistry 刷新进程内注册表 + 清空代理连接池缓存，
// **不重启进程**（与密钥保存同口径）。
package admin

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"workbuddy2api/internal/proxyreg"
	"workbuddy2api/internal/upstream"
)

// proxyStatus 槽位 + 绑定 + 使用计数的界面视图。
type proxyStatus struct {
	Slots []proxySlotView `json:"slots"`
	// Binds 完整绑定表（uid → slotID）。界面据此回显每个账号当前绑了哪个槽位
	// ——缺了它界面只能全部显示「直连」，用户看不到已配置的绑定。
	Binds map[string]string `json:"binds"`
	// OrphanBinds 绑定了不存在的槽位的账号（uid → 失效的槽位 id）。
	// 界面据此提示「该绑定已失效」，而不是静默当作直连。
	OrphanBinds map[string]string `json:"orphanBinds,omitempty"`
}

type proxySlotView struct {
	proxyreg.Slot
	// Usage 有多少账号绑定了本槽位（界面显示「N 个账号在用」）。
	Usage int `json:"usage"`
	// 出口健康（按代理 URL 记账，见 upstream.ProxyHealth）。
	// Healthy 为 false 时选号会排除绑定本槽位的账号（轮换改用别的出口）。
	Healthy bool `json:"healthy"`
	// HealthKnown 该出口是否有过任何探测/出站记录。
	//
	// **必须与 Healthy 分开**：健康账只记「真实出站」与「手动测试」的结果，
	// 从没被用过的槽位没有任何记录。旧实现把「无记录」默认成 Healthy=true，
	// 于是面板把「从未使用」显示成「正常」—— 2026-10-24 实测：8 个槽位经出口
	// 访问 generate_204 全部返回 502，面板却全绿，误导成「出口都健康」。
	// 界面据此显示第三态「未探测」，而不是谎报正常。
	HealthKnown bool   `json:"health_known"`
	ProbedAt    string `json:"probed_at,omitempty"`
	FailCount   int    `json:"fail_count"`
	LastErr     string `json:"last_error,omitempty"`
	CooldownMs  int64  `json:"cooldown_ms,omitempty"`
}

// getProxySlots GET /__admin/proxy/slots —— 列出槽位与绑定概览。
func (h *Handler) getProxySlots(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Registry == nil {
		fail(w, http.StatusServiceUnavailable, "代理注册表不可用")
		return
	}
	slots := h.cfg.Registry.Slots()
	usage := h.cfg.Registry.UsageCount()
	views := make([]proxySlotView, 0, len(slots))
	for _, s := range slots {
		// 健康按**槽位 URL 原文**查（与 upstream 记账用的键一致 —— 它用
		// proxyForAccount 的返回值，即 Registry.Resolve 给出的原始 URL）。
		v := proxySlotView{Slot: s, Usage: usage[s.ID], Healthy: true}
		if h.cfg.ProxyHealth != nil {
			if hp, ok := h.cfg.ProxyHealth(s.URL); ok {
				v.HealthKnown = true
				v.Healthy = !hp.Unhealthy
				v.FailCount = hp.Fails
				v.LastErr = hp.LastErr
				v.CooldownMs = hp.CooldownMs
				v.ProbedAt = hp.ProbedAt
			}
		}
		views = append(views, v)
	}
	// 失效绑定：绑定指向了不存在的槽位（槽位被删但绑定还在）。
	binds := h.cfg.Registry.Binds()
	orphan := map[string]string{}
	for uid, sid := range binds {
		if _, ok := h.cfg.Registry.SlotByID(sid); !ok {
			orphan[uid] = sid
		}
	}
	writeJSON(w, http.StatusOK, proxyStatus{Slots: views, Binds: binds, OrphanBinds: orphan})
}

// saveProxySlots POST /__admin/proxy/slots —— 整体保存槽位与绑定。
//
// 整体替换语义（与密钥保存一致）：界面提交完整列表，服务端校验后一次写入。
// 逐条增删改会让「并发保存」的合并语义变复杂，而代理配置天然是低频操作。
func (h *Handler) saveProxySlots(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Slots []proxyreg.Slot   `json:"slots"`
		Binds map[string]string `json:"binds"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	// 校验：id 非空且唯一、URL 合法、绑定只指向已存在的槽位。
	seen := map[string]bool{}
	for i := range body.Slots {
		s := &body.Slots[i]
		s.ID = strings.TrimSpace(s.ID)
		s.Name = strings.TrimSpace(s.Name)
		s.URL = strings.TrimSpace(s.URL)
		if s.ID == "" {
			fail(w, http.StatusBadRequest, "第 %d 个槽位缺少 id", i+1)
			return
		}
		if seen[s.ID] {
			fail(w, http.StatusBadRequest, "槽位 id 重复：%s", s.ID)
			return
		}
		seen[s.ID] = true
		if s.Name == "" {
			s.Name = s.ID // 名字空时回落 id，避免界面出现空白行
		}
		if s.URL == "" {
			fail(w, http.StatusBadRequest, "槽位 %s 缺少代理地址", s.ID)
			return
		}
		if bad := validateProxyURL(s.URL); bad != "" {
			fail(w, http.StatusBadRequest, "槽位 %s：%s", s.ID, bad)
			return
		}
	}
	for uid, sid := range body.Binds {
		sid = strings.TrimSpace(sid)
		if sid == "" {
			delete(body.Binds, uid) // 空绑定 = 直连，不持久化
			continue
		}
		if !seen[sid] {
			fail(w, http.StatusBadRequest, "账号 %s 绑定了不存在的槽位 %s", uid, sid)
			return
		}
		body.Binds[uid] = sid
	}

	// 写 config.json（原地改两个顶层键）
	raw, err := os.ReadFile(h.cfg.ConfigPath)
	if err != nil {
		fail(w, http.StatusInternalServerError, "读取 config.json 失败：%v", err)
		return
	}
	text, err := setTopLevelValue(string(raw), "proxy_slots", body.Slots)
	if err != nil {
		fail(w, http.StatusInternalServerError, "写入 proxy_slots 失败：%v", err)
		return
	}
	text, err = setTopLevelValue(text, "account_proxies", body.Binds)
	if err != nil {
		fail(w, http.StatusInternalServerError, "写入 account_proxies 失败：%v", err)
		return
	}
	// 与密钥保存同强度保护：一致性断言（除这两个键外逐字段相同）+ 备份 + 原子替换
	_, err = h.persistConfigEdits(raw, text, map[string]any{
		"proxy_slots":     toAny(body.Slots),
		"account_proxies": toAny(body.Binds),
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}

	// 热重载：刷新注册表 + 清连接池缓存（旧代理的连接池不该继续被复用）
	h.cfg.Registry.Set(body.Slots, body.Binds)
	if h.cfg.ResetProxyTransports != nil {
		h.cfg.ResetProxyTransports()
	}
	logf("代理配置已保存：%d 个槽位，%d 条绑定", len(body.Slots), len(body.Binds))
	ok(w, nil)
}

// testProxy POST /__admin/proxy/test —— 探测某个代理是否可用。
//
// 用「经该代理访问一个必然可达的地址」判定：只做 TCP+HTTP 连通性，
// **不打上游**（避免用真实账号流量做探测，也避免消耗额度）。
// 目标选 http://www.gstatic.com/generate_204（全球可达的 204 端点）；
// 失败时回落报告拨号阶段的错误，便于区分「代理连不上」与「代理连上但出不去」。
//
// 探测结果**同时写回健康登记**（NoteProxyResult），使面板的槽位健康列能反映
// 手动「测试」的结论 —— 否则健康列只认「真实出站」的账，从没被用过的坏槽位
// 会一直显示「正常」（2026-10-24 实测：8 个槽位实际全部 502，面板全绿）。
func (h *Handler) testProxy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	body.URL = strings.TrimSpace(body.URL)
	if body.URL == "" {
		fail(w, http.StatusBadRequest, "缺少代理地址")
		return
	}
	if bad := validateProxyURL(body.URL); bad != "" {
		fail(w, http.StatusBadRequest, "%s", bad)
		return
	}

	res := upstream.ProbeProxyURL(r.Context(), body.URL)
	// 写回健康登记：成功清账、失败记一笔（与真实出站共用同一本账）。
	// 这样面板的槽位健康列能反映手动「测试」的结论，而不是只认真实出站的账。
	if h.cfg.ProxyNoteResult != nil {
		h.cfg.ProxyNoteResult(body.URL, res.OK, res.Error)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": res.OK, "stage": res.Stage, "dialMs": res.DialMs,
		"elapsedMs": res.ElapsedMs, "status": res.Status,
		"error": res.Error,
	})
}

// validateProxyURL 校验代理 URL，返回空串表示合法。
func validateProxyURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "URL 解析失败：" + err.Error()
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return "不支持的协议（支持 http/https/socks5/socks5h）"
	}
	if u.Host == "" {
		return "缺少主机与端口"
	}
	if _, _, err := net.SplitHostPort(u.Host); err != nil {
		return "主机需带端口，如 " + u.Host + ":8080"
	}
	return ""
}
