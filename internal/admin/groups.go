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
	// Names 返回出现过的分组名（供面板下拉）。含显式创建的空组。
	Names() []string
	// GroupOf 返回某账号的分组名（空 = 未分组）。
	GroupOf(uid string) string
	// EmptyNames 返回显式创建、但当前没有任何账号的分组名。
	EmptyNames() []string
	// SetEmpty 整体替换空组名列表。
	SetEmpty(names []string)
}

// getGroups GET /__admin/groups —— 分组表与分组名列表。
func (h *Handler) getGroups(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Groups == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"groups":     map[string]string{},
			"names":      []string{},
			"emptyNames": []string{},
			"available":  false,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"groups":     h.cfg.Groups.Snapshot(),
		"names":      h.cfg.Groups.Names(),
		"emptyNames": h.cfg.Groups.EmptyNames(),
		"available":  true,
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

	// 空组名里凡是这次拿到了成员的，就不必再单独记着（映射本身已代表它）
	if err := h.writeGroups(clean, minusUsed(h.cfg.Groups.EmptyNames(), clean)); err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	h.cfg.Groups.SetAll(clean)
	logf("账号分组已保存：%d 个账号有分组", len(clean))
	ok(w, nil)
}

// postGroupCreate POST /__admin/groups/create —— 新建一个（可能还没有账号的）分组。
//
// 入参 {"name":"受限"}。为什么需要独立端点：分组的唯一数据是 uid→名 的映射，
// 没有成员的组在数据上不存在。要支持「先建组、再把账号放进去」就必须显式记下
// 组名（config 的 account_group_names）。
//
// 已存在（有账号或已建过）时幂等返回，不报错。
func (h *Handler) postGroupCreate(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Groups == nil {
		fail(w, http.StatusServiceUnavailable, "分组功能未接线")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		fail(w, http.StatusBadRequest, "分组名不能为空")
		return
	}
	if strings.ContainsAny(name, "\r\n\t/\\") {
		fail(w, http.StatusBadRequest, "分组名 %q 含非法字符（不允许换行/制表/斜杠）", name)
		return
	}
	if len([]rune(name)) > 40 {
		fail(w, http.StatusBadRequest, "分组名 %q 过长（上限 40 字）", name)
		return
	}
	for _, n := range h.cfg.Groups.Names() {
		if n == name {
			ok(w, map[string]any{"name": name, "created": false})
			return
		}
	}
	extra := append(append([]string{}, h.cfg.Groups.EmptyNames()...), name)
	if err := h.writeGroups(h.cfg.Groups.Snapshot(), extra); err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	h.cfg.Groups.SetEmpty(extra)
	logf("新建分组 %q", name)
	ok(w, map[string]any{"name": name, "created": true})
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
	// 复用整体保存路径。目标组若是新建的，顺手从「空组名」里摘掉（它现在有成员了）。
	extra := dropName(h.cfg.Groups.EmptyNames(), g)
	if err := h.writeGroups(m, extra); err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	h.cfg.Groups.SetAll(m)
	h.cfg.Groups.SetEmpty(extra)
	logf("批量分组：%d 个账号 → 分组 %q", n, g)
	ok(w, map[string]any{"updated": n, "group": g})
}

// minusUsed 返回 names 中「没有被 m 里任何账号使用」的部分。
func minusUsed(names []string, m map[string]string) []string {
	used := make(map[string]bool, len(m))
	for _, g := range m {
		used[g] = true
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n != "" && !used[n] {
			out = append(out, n)
		}
	}
	return out
}

// dropName 返回去掉 name 后的副本（name 为空时原样返回副本）。
func dropName(names []string, name string) []string {
	if name == "" {
		return append([]string{}, names...)
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n != name {
			out = append(out, n)
		}
	}
	return out
}

// postGroupRename POST /__admin/groups/rename —— 重命名分组（批量改归属）。
//
// 入参 {"from":"旧名","to":"新名"}。to 为空 = 拒绝（要解散请用 delete 或逐个清空）。
// 账号归属按 from→to 批量改写；不影响 key 绑定 —— key 里存的是组名，故**同时**
// 把绑定了 from 的 key 改成 to（否则 key 会指向一个已不存在的组，行为变成"组内无账号"）。
func (h *Handler) postGroupRename(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Groups == nil {
		fail(w, http.StatusServiceUnavailable, "分组功能未接线")
		return
	}
	var body struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	from := strings.TrimSpace(body.From)
	to := strings.TrimSpace(body.To)
	if from == "" || to == "" {
		fail(w, http.StatusBadRequest, "from/to 都不能为空")
		return
	}
	if from == to {
		ok(w, map[string]any{"accounts": 0, "keys": 0})
		return
	}
	if strings.ContainsAny(to, "\r\n\t/\\") || len([]rune(to)) > 40 {
		fail(w, http.StatusBadRequest, "分组名 %q 非法", to)
		return
	}
	// 账号归属
	m := h.cfg.Groups.Snapshot()
	n := 0
	for uid, g := range m {
		if g == from {
			m[uid] = to
			n++
		}
	}
	// 空组名里若记着 from，一并改名（否则旧名残留成幽灵组）
	extra := h.cfg.Groups.EmptyNames()
	for i := range extra {
		if extra[i] == from {
			extra[i] = to
		}
	}
	extra = append(extra, to)
	extra = uniqStrings(extra)
	if err := h.writeGroups(m, extra); err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	h.cfg.Groups.SetAll(m)
	h.cfg.Groups.SetEmpty(extra)
	// key 绑定同步改写（否则 key 指向不存在的组）
	kn, err := h.rewriteKeyGroups(from, to)
	if err != nil {
		fail(w, http.StatusInternalServerError, "账号已改名，但 key 绑定改写失败：%v", err)
		return
	}
	logf("分组重命名：%q → %q（%d 个账号，%d 个密钥）", from, to, n, kn)
	ok(w, map[string]any{"accounts": n, "keys": kn})
}

// postGroupDelete POST /__admin/groups/delete —— 删除分组（解散）。
//
// 入参 {"name":"分组名"}。语义：**只解除归属，不删账号**（账号变回未分组）。
// 绑定了该组的 key 会被解绑（Group 置空）—— 否则它们会指向不存在的组、
// 变成「组内无账号」而全部失败。这是显式选择：删组后这些 key 回到未分组语义。
func (h *Handler) postGroupDelete(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Groups == nil {
		fail(w, http.StatusServiceUnavailable, "分组功能未接线")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		fail(w, http.StatusBadRequest, "缺少分组名")
		return
	}
	m := h.cfg.Groups.Snapshot()
	n := 0
	for uid, g := range m {
		if g == name {
			delete(m, uid)
			n++
		}
	}
	extra := dropName(h.cfg.Groups.EmptyNames(), name)
	if err := h.writeGroups(m, extra); err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	h.cfg.Groups.SetAll(m)
	h.cfg.Groups.SetEmpty(extra)
	kn, err := h.rewriteKeyGroups(name, "")
	if err != nil {
		fail(w, http.StatusInternalServerError, "分组已删除，但 key 解绑失败：%v", err)
		return
	}
	logf("删除分组 %q：解除 %d 个账号归属，解绑 %d 个密钥", name, n, kn)
	ok(w, map[string]any{"accounts": n, "keys": kn})
}

// writeGroups 把分组表 + 空组名一起写进 config.json（一次原子替换）。
//
// 两个键必须同一次落盘：分成两次写的话，中间态会出现「组已删但空组名还在」
// 这类不一致；而且第二次失败会让第一次的结果留在盘上、内存却没更新。
func (h *Handler) writeGroups(m map[string]string, extra []string) error {
	raw, err := os.ReadFile(h.cfg.ConfigPath)
	if err != nil {
		return err
	}
	text, err := setTopLevelValue(string(raw), "account_groups", m)
	if err != nil {
		return err
	}
	text, err = setTopLevelValue(text, "account_group_names", extra)
	if err != nil {
		return err
	}
	_, err = h.persistConfigEdits(raw, text, map[string]any{
		"account_groups":      toAny(m),
		"account_group_names": toAny(extra),
	})
	return err
}

// uniqStrings 去重并丢弃空串（保持首次出现顺序）。
func uniqStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// rewriteKeyGroups 把 api_keys 里 Group==from 的项改成 to（to 为空 = 解绑）。
// 返回改动条数。空 from 不做任何事。
func (h *Handler) rewriteKeyGroups(from, to string) (int, error) {
	if from == "" {
		return 0, nil
	}
	keys, legacy, err := h.readKeys()
	if err != nil {
		return 0, err
	}
	n := 0
	for i := range keys {
		if strings.TrimSpace(keys[i].Group) == from {
			keys[i].Group = to
			n++
		}
	}
	if n == 0 {
		return 0, nil
	}
	raw, err := os.ReadFile(h.cfg.ConfigPath)
	if err != nil {
		return 0, err
	}
	text, err := setTopLevelValue(string(raw), "api_keys", keys)
	if err != nil {
		return 0, err
	}
	if legacy != "" {
		if text, err = setTopLevelValue(text, "api_key", legacy); err != nil {
			return 0, err
		}
	}
	if _, err := h.persistConfigEdits(raw, text, map[string]any{
		"api_keys": toAny(keys),
	}); err != nil {
		return 0, err
	}
	// 热重载密钥表（让 server 侧立刻看到新 Group）
	if h.cfg.ReloadKeys != nil {
		h.cfg.ReloadKeys(legacy, keys)
	}
	return n, nil
}
