// groups.go 账号分组的管理端点：读分组表 / 保存分组 / 批量设置账号分组。
//
// 分组的用途（当前动机）：把被上游风控封禁（403 + 11140）的账号放进「受限」分组，
// 使普通 API key 不会再选中它们；管理员可定期对它们做连通性测试看是否解封，
// 解封后移回正常分组。
//
// 落盘：写 config.json 的 account_groups（uid → 分组名）。比照 proxy_slots 的做法
// ——读原文件、原地改一个顶层键、一致性断言 + 备份 + 原子替换、再热重载注册表。
package admin

import (
	"net/http"
	"os"
	"strings"
)

// GroupsController 网关侧的分组能力（由 cmd/server 注入）。
type GroupsController interface {
	// Snapshot 返回 uid → 分组名。
	Snapshot() map[string]string
	// SetAll 整体替换分组表并热重载。
	SetAll(m map[string]string)
	// Names 返回出现过的分组名（供面板下拉）。
	Names() []string
	// GroupOf 返回某账号的分组名（空 = 未分组）。
	GroupOf(uid string) string
}

// getGroups GET /__admin/groups —— 分组表与分组名列表。
func (h *Handler) getGroups(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Groups == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"groups":    map[string]string{},
			"names":     []string{},
			"available": false,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"groups":    h.cfg.Groups.Snapshot(),
		"names":     h.cfg.Groups.Names(),
		"available": true,
	})
}

// postGroups POST /__admin/groups —— 整体保存分组表。
//
// 入参 {"groups": {"<uid>": "<分组名>"}}（空分组名 = 移出分组）。
// 整体替换语义（与槽位/密钥保存一致）：界面提交完整表，服务端校验后一次写入。
//
// 分组名不做白名单限制（用户可自取），但拒绝会破坏语义的字符：
//   - 空串 → 视为移出分组（删除该条），不报错；
//   - 含路径分隔/控制字符 → 拒绝（虽然只存 JSON，但避免后续导出/日志混乱）。
func (h *Handler) postGroups(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Groups == nil {
		fail(w, http.StatusServiceUnavailable, "分组功能未接线")
		return
	}
	var body struct {
		Groups map[string]string `json:"groups"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	clean := make(map[string]string, len(body.Groups))
	for uid, g := range body.Groups {
		uid = strings.TrimSpace(uid)
		g = strings.TrimSpace(g)
		if uid == "" {
			fail(w, http.StatusBadRequest, "账号 uid 不能为空")
			return
		}
		if g == "" {
			continue // 空分组 = 移出分组：不落盘该键
		}
		if strings.ContainsAny(g, "\r\n\t/\\") {
			fail(w, http.StatusBadRequest, "分组名 %q 含非法字符（不允许换行/制表/斜杠）", g)
			return
		}
		if len([]rune(g)) > 40 {
			fail(w, http.StatusBadRequest, "分组名 %q 过长（上限 40 字）", g)
			return
		}
		clean[uid] = g
	}

	raw, err := os.ReadFile(h.cfg.ConfigPath)
	if err != nil {
		fail(w, http.StatusInternalServerError, "读取 config.json 失败：%v", err)
		return
	}
	text, err := setTopLevelValue(string(raw), "account_groups", clean)
	if err != nil {
		fail(w, http.StatusInternalServerError, "写入 account_groups 失败：%v", err)
		return
	}
	if _, err := h.persistConfigEdits(raw, text, map[string]any{
		"account_groups": toAny(clean),
	}); err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	h.cfg.Groups.SetAll(clean)
	logf("账号分组已保存：%d 个账号有分组", len(clean))
	ok(w, nil)
}

// postGroupSet POST /__admin/groups/set —— 给一批账号设置同一个分组。
//
// 入参 {"uids": [...], "group": "受限"}（group 为空 = 移出分组）。
// 这是「把封禁号批量移入受限组」的便捷入口 —— 逐个改太繁琐。
func (h *Handler) postGroupSet(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Groups == nil {
		fail(w, http.StatusServiceUnavailable, "分组功能未接线")
		return
	}
	var body struct {
		UIDs  []string `json:"uids"`
		Group string   `json:"group"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	g := strings.TrimSpace(body.Group)
	if g != "" && strings.ContainsAny(g, "\r\n\t/\\") {
		fail(w, http.StatusBadRequest, "分组名含非法字符")
		return
	}
	if len(body.UIDs) == 0 {
		fail(w, http.StatusBadRequest, "缺少 uids")
		return
	}
	m := h.cfg.Groups.Snapshot()
	n := 0
	for _, uid := range body.UIDs {
		uid = strings.TrimSpace(uid)
		if uid == "" {
			continue
		}
		if g == "" {
			delete(m, uid)
		} else {
			m[uid] = g
		}
		n++
	}
	// 复用整体保存路径
	raw, err := os.ReadFile(h.cfg.ConfigPath)
	if err != nil {
		fail(w, http.StatusInternalServerError, "读取 config.json 失败：%v", err)
		return
	}
	// 排序输出保证确定性（便于 diff/备份对比）
	text, err := setTopLevelValue(string(raw), "account_groups", m)
	if err != nil {
		fail(w, http.StatusInternalServerError, "写入 account_groups 失败：%v", err)
		return
	}
	if _, err := h.persistConfigEdits(raw, text, map[string]any{
		"account_groups": toAny(m),
	}); err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	h.cfg.Groups.SetAll(m)
	logf("批量分组：%d 个账号 → 分组 %q", n, g)
	ok(w, map[string]any{"updated": n, "group": g})
}
