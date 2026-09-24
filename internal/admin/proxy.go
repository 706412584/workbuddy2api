// proxy.go 出站代理槽位与账号绑定的管理接口（/__admin/proxy/*）。
//
// 持久化：写回 config.json 的 `proxy_slots` 与 `account_proxies` 两个顶层键，
// 沿用 keys.go 的 setTopLevelValue（原地改文本区间，保留其余字节与排版）。
//
// 热重载：保存后立即 SetRegistry 刷新进程内注册表 + 清空代理连接池缓存，
// **不重启进程**（与密钥保存同口径）。
package admin

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"workbuddy2api/internal/proxyreg"
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
		views = append(views, proxySlotView{Slot: s, Usage: usage[s.ID]})
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

	u, _ := url.Parse(body.URL)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// 第一步：能否连上代理本身（区分「代理不可达」与「代理可达但转发失败」）
	stage := "dial"
	start := time.Now()
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", u.Host)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "stage": stage, "elapsedMs": time.Since(start).Milliseconds(),
			"error": "连不上代理主机 " + u.Host + "：" + err.Error(),
		})
		return
	}
	conn.Close()
	dialMs := time.Since(start).Milliseconds()

	// 第二步：经代理发一次真实 HTTP 请求（用标准库的 Proxy 钩子，http 代理通用）
	stage = "request"
	reqStart := time.Now()
	client := &http.Client{
		Timeout: 8 * time.Second,
		Transport: &http.Transport{
			Proxy:             http.ProxyURL(u),
			DisableKeepAlives: true, // 探测用短连接，不留池
			ForceAttemptHTTP2: false,
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://www.gstatic.com/generate_204", nil)
	if err != nil {
		fail(w, http.StatusInternalServerError, "构造探测请求失败：%v", err)
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "stage": stage, "dialMs": dialMs,
			"elapsedMs": time.Since(reqStart).Milliseconds(),
			"error":     "代理可达但转发失败：" + err.Error(),
		})
		return
	}
	resp.Body.Close()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "stage": "ok", "dialMs": dialMs,
		"elapsedMs": time.Since(reqStart).Milliseconds(),
		"status":    resp.StatusCode,
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
