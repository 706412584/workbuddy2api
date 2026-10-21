// sidecar_persist.go sidecar 同步结果的落盘与热重载。
//
// 与 proxy.go 的 saveProxySlots 共用同一套机制（读原 config → 原地改顶层键 →
// 一致性断言 + 备份 + 原子替换 → 刷新 Registry + 清连接池缓存），差别只在
// **合并语义**：
//   - 槽位：sidecar 只**增改**自己前缀的槽位，**手工槽位原样保留**（用户手工配的
//     代理不该被一次订阅同步抹掉）；已消失的 sidecar 槽位删除。
//   - 绑定：只**合并**新增绑定，不动已有绑定（粘性）。
package admin

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"workbuddy2api/internal/proxyreg"
)

// jsonUnmarshalLenient 宽松 JSON 解码（空/空白视为无操作，由调用方先判空）。
func jsonUnmarshalLenient(b []byte, v any) error {
	return json.Unmarshal(b, v)
}

// persistSidecarSlots 把同步结果里的槽位合并进 config.json 并热重载。
func (h *Handler) persistSidecarSlots(res SidecarSyncResult) error {
	if h.cfg.Registry == nil {
		return fmt.Errorf("代理注册表不可用")
	}
	cur := h.cfg.Registry.Slots()
	binds := h.cfg.Registry.Binds()

	merged := mergeSlots(cur, res.Slots)
	// 清掉指向已消失槽位的绑定（否则界面显示「已失效绑定」）
	valid := map[string]bool{}
	for _, s := range merged {
		valid[s.ID] = true
	}
	for uid, sid := range binds {
		if !valid[sid] {
			delete(binds, uid)
		}
	}

	return h.writeProxyConfig(merged, binds)
}

// persistSidecarBinds 把新增绑定合并进 config.json 并热重载。
func (h *Handler) persistSidecarBinds(added map[string]string) error {
	if h.cfg.Registry == nil {
		return fmt.Errorf("代理注册表不可用")
	}
	slots := h.cfg.Registry.Slots()
	binds := h.cfg.Registry.Binds()
	for uid, sid := range added {
		binds[uid] = sid
	}
	return h.writeProxyConfig(slots, binds)
}

// mergeSlots 用 incoming 覆盖/新增，保留其它（手工）槽位；incoming 未出现的
// **旧 sidecar 槽位**（同前缀且不在 incoming 里）删除——那是订阅里已消失的节点。
func mergeSlots(cur []proxyreg.Slot, incoming []SidecarSlot) []proxyreg.Slot {
	prefix := sidecarSlotPrefix(cur, incoming)
	// 以 cur 顺序为基底，保持界面稳定
	byID := map[string]proxyreg.Slot{}
	for _, s := range cur {
		byID[s.ID] = s
	}
	for _, in := range incoming {
		byID[in.ID] = proxyreg.Slot{ID: in.ID, Name: in.Name, URL: in.URL, Enabled: in.Enabled}
	}
	inSet := map[string]bool{}
	for _, in := range incoming {
		inSet[in.ID] = true
	}
	out := make([]proxyreg.Slot, 0, len(byID))
	for _, s := range cur {
		if prefix != "" && strings.HasPrefix(s.ID, prefix) && !inSet[s.ID] {
			continue // 已消失的 sidecar 节点
		}
		out = append(out, byID[s.ID])
	}
	// 追加本次新增且不在 cur 里的
	for _, in := range incoming {
		found := false
		for _, s := range cur {
			if s.ID == in.ID {
				found = true
				break
			}
		}
		if !found {
			out = append(out, byID[in.ID])
		}
	}
	return out
}

// sidecarSlotPrefix 推断 sidecar 槽位前缀（取 incoming 第一个 id 里最后一个 '-' 前的
// 部分不足以判定，这里改用一个保守做法：只要 incoming 非空，就用 id 的公共形态判断）。
// 简化实现：从 incoming[0].ID 里取「sidecar-」这类已知前缀；取不到则返回空（不删任何旧槽位）。
func sidecarSlotPrefix(_ []proxyreg.Slot, incoming []SidecarSlot) string {
	if len(incoming) == 0 {
		return ""
	}
	id := incoming[0].ID
	if i := strings.Index(id, "-"); i > 0 {
		return id[:i+1] // e.g. "sidecar-"
	}
	return ""
}

// writeProxyConfig 读 config.json → 改 proxy_slots/account_proxies → 一致性断言 +
// 备份 + 原子替换 → 刷新注册表 + 清连接池缓存。与 saveProxySlots 同强度。
func (h *Handler) writeProxyConfig(slots []proxyreg.Slot, binds map[string]string) error {
	raw, err := os.ReadFile(h.cfg.ConfigPath)
	if err != nil {
		return fmt.Errorf("读取 config.json 失败：%w", err)
	}
	text, err := setTopLevelValue(string(raw), "proxy_slots", slots)
	if err != nil {
		return fmt.Errorf("写入 proxy_slots 失败：%w", err)
	}
	text, err = setTopLevelValue(text, "account_proxies", binds)
	if err != nil {
		return fmt.Errorf("写入 account_proxies 失败：%w", err)
	}
	if _, err := h.persistConfigEdits(raw, text, map[string]any{
		"proxy_slots":     toAny(slots),
		"account_proxies": toAny(binds),
	}); err != nil {
		return err
	}
	h.cfg.Registry.Set(slots, binds)
	if h.cfg.ResetProxyTransports != nil {
		h.cfg.ResetProxyTransports()
	}
	return nil
}
