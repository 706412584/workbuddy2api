package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

// authFileRE 凭证文件名必须严格匹配，杜绝 '../' 之类的路径穿越。
var authFileRE = regexp.MustCompile(`^workbuddy-[A-Za-z0-9._-]+\.json$`)

// authFileView 单个凭证文件对外暴露的视图。刻意不返回完整凭证，只给元信息 ——
// 面板只需要辨认「这是哪个账号」，多给一个字节的 token 都是多余的暴露面。
type authFileView struct {
	File         string `json:"file"`
	UID          string `json:"uid"`
	Nickname     string `json:"nickname"`
	EnterpriseID string `json:"enterpriseId"`
	Domain       string `json:"domain"`
	Region       string `json:"region"`
	ExpiresAt    int64  `json:"expiresAt"`
	Expired      bool   `json:"expired"`
	TokenHint    string `json:"tokenHint"`
	// Disabled 池中该账号是否被禁用（含自动禁用与人工禁用）。**来源是池状态而非文件**：
	// disabled 是运行期调度状态，凭证文件里没有这个字段。
	// UID 不在池中时（尚未加载/文件坏）为 false，且 InPool=false 让面板不误显示「启用中」。
	Disabled bool `json:"disabled"`
	// DisabledReason 禁用原因（"12153 session dead" / 人工禁用等），仅 disabled 时非空。
	DisabledReason string `json:"disabledReason,omitempty"`
	// InPool 该账号是否已在网关池中。false 时 Disabled/DisabledReason 无意义。
	InPool bool `json:"inPool"`
}

// viewOf 解析单个凭证文件为视图。解析失败（含缺 accessToken）返回 false，
// 由调用方跳过 —— 一个坏文件不该让整张账号表打不开。
func viewOf(file, path string) (authFileView, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return authFileView{}, false
	}
	a, err := auth.Parse(raw)
	if err != nil {
		return authFileView{}, false
	}
	return authFileView{
		File:         file,
		UID:          a.UID,
		Nickname:     a.Nickname,
		EnterpriseID: a.EnterpriseID,
		Domain:       a.Domain,
		Region:       string(a.Region()),
		ExpiresAt:    a.ExpiresAt,
		Expired:      a.ExpiresAt > 0 && a.ExpiresAt*1000 < time.Now().UnixMilli(),
		TokenHint:    tokenHint(a.AccessToken),
	}, true
}

// tokenHint 只露 token 前 6 位，仅供肉眼区分。
func tokenHint(token string) string {
	if len(token) > 6 {
		token = token[:6]
	}
	return token + "…"
}

// listAccounts 列出凭证目录下的账号。目录不存在视为「没有账号」而非错误。
func listAccounts(dir string) []authFileView {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []authFileView{}
	}
	out := make([]authFileView, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !authFileRE.MatchString(name) {
			continue
		}
		if v, ok := viewOf(name, filepath.Join(dir, name)); ok {
			out = append(out, v)
		}
	}
	return out
}

// mergePoolState 把池中的禁用状态合进磁盘视图。
//
// 磁盘（auths/*.json）与池是两份数据：前者是凭证，后者是调度状态。面板要同时显示
// 「这是哪个账号」与「它现在能不能被调度」，就得在出接口前合并一次。
// 查不到的 uid（文件有、池里没有）保留 InPool=false，面板据此区分「未加载」与「已启用」。
func mergePoolState(p *pool.Pool, views []authFileView) {
	if p == nil {
		return
	}
	byUID := make(map[string]pool.Status, len(views))
	for _, s := range p.List() {
		byUID[s.UID] = s
	}
	for i := range views {
		s, ok := byUID[views[i].UID]
		if !ok {
			continue
		}
		views[i].InPool = true
		views[i].Disabled = s.Disabled
		views[i].DisabledReason = s.DisabledReason
	}
}

func (h *Handler) listAccounts(w http.ResponseWriter, r *http.Request) {
	views := listAccounts(h.cfg.AuthDir)
	mergePoolState(h.cfg.Pool, views)
	writeJSON(w, http.StatusOK, map[string]any{
		"authDir":  h.cfg.AuthDir,
		"accounts": views,
	})
}

// toggleAccount POST /__admin/accounts/toggle —— 人工启用/禁用单个账号。
//
// 只改池中调度状态，**不碰凭证文件**：禁用是「暂时别用它」，删除才是「不要它了」。
// 这一点很重要 —— 误禁用可一键恢复，误删除要重新登录。
//
// 人工禁用与自动禁用（12153 session dead）共用同一个 disabled 字段，只靠 reason 文案区分。
// 因此「解除禁用」不止本端点一条路径，界面上必须说清楚：
//   - 签到（ReenableIfCredits）**不会**解除：它只管余额与冷却；
//   - keepalive（refresh 成功）**不会**解除：只清连续 12153 计数；
//   - 面板「测试连接」成功**会**解除（NoteTestOK：测试通过即证明 session 活着）；
//   - 面板「重置」勾选解除禁用**会**解除（Reset includeDisabled）。
// 后两条都是用户在面板上的显式动作，不算"偷偷放回"，但文案不能宣称禁用是绝对粘性的。
func (h *Handler) toggleAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UID      string `json:"uid"`
		Disabled bool   `json:"disabled"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	uid := strings.TrimSpace(body.UID)
	if uid == "" {
		fail(w, http.StatusBadRequest, "缺少 uid")
		return
	}
	// 与 mergePoolState / testAccount 同样的防御：Pool 未接线时给明确错误，
	// 而不是空指针 panic 变成 500。
	if h.cfg.Pool == nil {
		fail(w, http.StatusNotImplemented, "本进程未接线账号池，无法切换账号状态")
		return
	}
	if _, ok := h.cfg.Pool.Status(uid); !ok {
		fail(w, http.StatusNotFound, "账号 %s 不在池中（可能尚未加载，或凭证文件已被删除）", uid)
		return
	}
	if body.Disabled {
		h.cfg.Pool.Disable(uid, manualDisableReason)
	} else {
		// 启用走 ReviveDisabled：清 disabled + reason + 两个连续计数，**不动冷却/熔断**
		// （那是「重置」的职责，启用只负责把号放回轮换）。
		// 副作用：若该号本就在冷却/熔断中，启用后仍不会被选中 —— 面板状态列会显示
		// 「已加载」而非「已禁用」，别让用户以为点了没生效。
		h.cfg.Pool.ReviveDisabled(uid)
	}
	h.cfg.Pool.Flush()
	action := "启用"
	if body.Disabled {
		action = "禁用"
	}
	logf("账号 %s 已%s（人工）", uid, action)
	ok(w, map[string]any{"uid": uid, "disabled": body.Disabled, "action": action})
}

// manualDisableReason 人工禁用的 reason 文案，供面板区分于自动禁用。
const manualDisableReason = "面板人工禁用"

// loaded 网关此刻到底加载了哪些账号。
//
// 面板原先只能拿密钥去问 /status 再取并集 —— 因为绑定区域的密钥只看得见本区域账号
// （global 密钥看到 5 个、cn 密钥看到 1 个、不限区域看到全部 6 个），拿单把密钥判断
// 「已加载」会把被过滤掉的账号误报成未加载。现在跑在网关进程内，直接问池子即可，
// 既是全量也不会被区域过滤。
func (h *Handler) loaded(w http.ResponseWriter, r *http.Request) {
	list := h.cfg.Pool.List()
	uuids := make([]string, 0, len(list))
	for _, s := range list {
		uuids = append(uuids, s.UID)
	}
	keys, _, _ := h.readKeys()
	writeJSON(w, http.StatusOK, map[string]any{
		"reachable": true, // 进程内查询，恒可达
		"uuids":     uuids,
		"total":     len(uuids),
		"keyCount":  len(keys),
	})
}

// exportAccounts 导出凭证。
//
// 两种粒度：带 file 参数导单个；不带则导全部。
// **两种内容**由 include_tokens 决定（默认 false）：
//   - false：仅元信息（uid/nickname/domain/区域/到期），**不含 token**，可安全分享
//   - true：完整凭证（accessToken/refreshToken 明文），文件即可直接导入使用
//
// 安全设计：默认不导出 token，必须显式 `include_tokens=true` 才带 —— 凭证文件里的
// accessToken/refreshToken 是账号的完全接管凭据（global 有效期 365 天），
// 默认导出即等于「点一下就把所有号泄到磁盘上」。前端据此加二次确认。
//
// 导出格式与 auths/workbuddy-<uid>.json **逐字同构**（{"auth":{...},"account":{...}}），
// 故导出文件可直接被本端点 import 回去，无需任何转换。
func (h *Handler) exportAccounts(w http.ResponseWriter, r *http.Request) {
	includeTokens := r.URL.Query().Get("include_tokens") == "true"
	onlyFile := strings.TrimSpace(r.URL.Query().Get("file"))

	entries, err := os.ReadDir(h.cfg.AuthDir)
	if err != nil {
		fail(w, http.StatusInternalServerError, "读取凭证目录失败：%v", err)
		return
	}
	if onlyFile != "" && !authFileRE.MatchString(onlyFile) {
		fail(w, http.StatusBadRequest, "非法文件名")
		return
	}

	type exported struct {
		File string         `json:"file"`
		Auth map[string]any `json:"auth,omitempty"`
		Acct map[string]any `json:"account"`
	}
	out := make([]exported, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !authFileRE.MatchString(name) {
			continue
		}
		if onlyFile != "" && name != onlyFile {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(h.cfg.AuthDir, name))
		if err != nil {
			continue // 单个文件读失败不拖累整体导出
		}
		a, err := auth.Parse(raw)
		if err != nil {
			continue // 坏文件跳过（与 listAccounts 同口径）
		}
		item := exported{
			File: name,
			Acct: map[string]any{
				"uid":          a.UID,
				"nickname":     a.Nickname,
				"enterpriseId": a.EnterpriseID,
			},
		}
		if includeTokens {
			item.Auth = map[string]any{
				"accessToken":  a.AccessTokenValue(),
				"refreshToken": a.RefreshTokenValue(),
				"expiresAt":    a.ExpiresAt,
				"domain":       a.DomainValue(),
			}
		}
		out = append(out, item)
	}
	if onlyFile != "" && len(out) == 0 {
		fail(w, http.StatusNotFound, "未找到该凭证文件：%s", onlyFile)
		return
	}
	logf("导出账号 %d 个（含 token=%v）", len(out), includeTokens)
	writeJSON(w, http.StatusOK, map[string]any{
		"exportedAt":    time.Now().Format(time.RFC3339),
		"includeTokens": includeTokens,
		"count":         len(out),
		"accounts":      out,
	})
}

// importAccount 导入凭证。接受两种形态：
//
//  1. **单个凭证**（原有行为）：`{"auth":{...},"account":{...}}` 嵌套形，
//     或手写扁平形 `{"accessToken":...,"uid":...}`。字段名沿用网关解析器。
//  2. **导出批量格式**（与 exportAccounts 输出同构，支持 round-trip）：
//     `{"accounts":[{"file","auth","account"}, ...]}`。
//     逐条导入，**部分成功不中断**——一个坏条目不该让整批失败。
//
// 判定方式：JSON 顶层含 `accounts` 数组即按批量处理，否则按单条（向后兼容）。
func (h *Handler) importAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		JSON string `json:"json"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.JSON) == "" {
		fail(w, http.StatusBadRequest, "缺少凭证 JSON")
		return
	}

	// 批量格式探测：顶层有 accounts 数组即按批处理。
	var probe struct {
		Accounts []json.RawMessage `json:"accounts"`
	}
	if json.Unmarshal([]byte(body.JSON), &probe) == nil && len(probe.Accounts) > 0 {
		h.importBatch(w, probe.Accounts)
		return
	}

	// 单条（原有路径）
	file, a, err := h.importOne([]byte(body.JSON))
	if err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	loaded, err := h.reloadAccounts()
	if err != nil {
		fail(w, http.StatusInternalServerError, "凭证已写入，但重新加载失败：%v", err)
		return
	}
	logf("导入账号 uid=%s file=%s（池中现有 %d 个）", a.UID, file, loaded)
	ok(w, map[string]any{"file": file, "account": h.viewOfFallback(file, a)})
}

// importBatch 逐条导入批量格式。返回成功/失败清单，部分失败不中断整批。
func (h *Handler) importBatch(w http.ResponseWriter, accounts []json.RawMessage) {
	type failItem struct {
		Index int    `json:"index"`
		Error string `json:"error"`
	}
	imported := make([]string, 0, len(accounts))
	failed := make([]failItem, 0)
	for i, raw := range accounts {
		// 导出条目形如 {"file","auth","account"}——多一个 file 键。剥掉 file 后
		// 即为标准凭证形态（auth.Parse 忽略未知键，故 file 在也不影响解析；
		// 这里显式剥离是为了落盘文件干净、不把导出元数据写进 auths/）。
		var item struct {
			File    string          `json:"file"`
			Auth    json.RawMessage `json:"auth"`
			Account json.RawMessage `json:"account"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			failed = append(failed, failItem{i, "条目解析失败：" + err.Error()})
			continue
		}
		var cred []byte
		switch {
		case len(item.Auth) > 0:
			// 重组为嵌套形，丢弃 file 键
			cred, _ = json.Marshal(map[string]any{
				"auth":    json.RawMessage(item.Auth),
				"account": json.RawMessage(item.Account),
			})
		default:
			cred = raw // 扁平形：原样交给解析器
		}
		file, a, err := h.importOne(cred)
		if err != nil {
			failed = append(failed, failItem{i, err.Error()})
			continue
		}
		imported = append(imported, file)
		_ = a
	}
	// 有成功项才重扫；全失败时池子没变，不必扰动
	loaded := 0
	if len(imported) > 0 {
		var err error
		loaded, err = h.reloadAccounts()
		if err != nil {
			fail(w, http.StatusInternalServerError, "已写入 %d 个，但重新加载失败：%v", len(imported), err)
			return
		}
	}
	logf("批量导入：成功 %d / 失败 %d（池中现有 %d 个）", len(imported), len(failed), loaded)
	writeJSON(w, http.StatusOK, map[string]any{
		"imported": imported,
		"failed":   failed,
		"loaded":   loaded,
	})
}

// importOne 导入单条凭证（校验 + 落盘），返回 (文件名, 解析结果, 错误)。
// 供单条导入与批量导入共用。
func (h *Handler) importOne(raw []byte) (string, *auth.Auth, error) {
	// 先按网关的解析器验一遍：能解析才落盘，否则写进去也是个起不来的账号
	a, err := auth.Parse(raw)
	if err != nil {
		return "", nil, fmt.Errorf("JSON 解析失败：%w", err)
	}
	if strings.TrimSpace(a.UID) == "" {
		return "", nil, errors.New("凭证缺少 uid")
	}
	file, err := h.writeAuthFile(a.UID, raw)
	if err != nil {
		return "", nil, fmt.Errorf("写入凭证失败：%w", err)
	}
	return file, a, nil
}

// deleteAccount 删除凭证文件。只删本地文件，随后立即重扫目录让池子同步。
func (h *Handler) deleteAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		File string `json:"file"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if !authFileRE.MatchString(body.File) {
		fail(w, http.StatusBadRequest, "非法文件名")
		return
	}
	target := filepath.Join(h.cfg.AuthDir, body.File)
	// 双保险：即使正则被绕过，也要求解析后的路径确实落在 auth 目录内
	if filepath.Dir(filepath.Clean(target)) != filepath.Clean(h.cfg.AuthDir) {
		fail(w, http.StatusBadRequest, "目标不在 auth 目录内")
		return
	}
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		fail(w, http.StatusInternalServerError, "删除失败：%v", err)
		return
	}
	loaded, err := h.reloadAccounts()
	if err != nil {
		fail(w, http.StatusInternalServerError, "文件已删除，但重新加载失败：%v", err)
		return
	}
	logf("删除账号 file=%s（池中剩余 %d 个）", body.File, loaded)
	ok(w, nil)
}

// writeAuthFile 原子写凭证（先写 .tmp 再 rename），避免网关读到半截 JSON。
//
// 缩进必须与 internal/auth 的 SaveAtomic 一致（2 空格）：凭证文件有两个写入方 ——
// 面板写导入/登录，网关在 token 刷新后写回。两者格式不同的话，文件会在每次刷新后
// 悄悄换一种排版，diff 全是噪音。
//
// 重新序列化而非原样落盘：粘贴进来的 JSON 可能带 BOM、注释或多余空白，过一遍解析器
// 能保证落盘的一定是干净可读的形态。解析用 any 而非结构体，故凭证里的额外字段会保留。
func (h *Handler) writeAuthFile(uid string, raw []byte) (string, error) {
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", err
	}
	text, err := json.MarshalIndent(parsed, "", "  ")
	if err != nil {
		return "", err
	}
	file := "workbuddy-" + uid + ".json"
	if !authFileRE.MatchString(file) {
		return "", fmt.Errorf("uid 含非法字符，拒绝写入：%s", uid)
	}
	if err := os.MkdirAll(h.cfg.AuthDir, 0o755); err != nil {
		return "", err
	}
	final := filepath.Join(h.cfg.AuthDir, file)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, text, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, final); err != nil {
		return "", err
	}
	return file, nil
}

// viewOfFallback 写入成功后回显账号信息。文件刚写过，正常一定能解析；
// 解析不出来时用已知的 uid 拼一个最小视图，免得整个响应失败。
func (h *Handler) viewOfFallback(file string, a *auth.Auth) authFileView {
	if v, ok := viewOf(file, filepath.Join(h.cfg.AuthDir, file)); ok {
		return v
	}
	return authFileView{File: file, UID: a.UID, Nickname: a.Nickname, Region: string(a.Region())}
}

// reloadAccounts 让网关重扫凭证目录。热重载的入口之一：
// 账号增删立即生效，不需要重启进程。
func (h *Handler) reloadAccounts() (int, error) {
	if h.cfg.ReloadAccounts == nil {
		return 0, errors.New("当前未启用账号热重载")
	}
	return h.cfg.ReloadAccounts()
}

// poolReload 手工重扫凭证目录。用于有人直接改了 auths/ 目录、
// 或想确认池子与磁盘一致时。
func (h *Handler) poolReload(w http.ResponseWriter, r *http.Request) {
	n, err := h.reloadAccounts()
	if err != nil {
		fail(w, http.StatusInternalServerError, "重新扫描失败：%v", err)
		return
	}
	logf("手工重扫凭证目录：池中 %d 个账号", n)
	ok(w, map[string]any{"loaded": n})
}

// resetOutcome 一个账号的重置前后对照，给界面展示。
type resetOutcome struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Before   string `json:"before"`
	After    string `json:"after"`
}

// poolReset 重置账号状态。
//
// 与 Node 版的根本差别：那时必须先停进程再改 state.json —— 网关运行中改文件会被
// 下一次 flush 原样盖掉（这正是「重启了但冷却还在」的成因）。现在直接改内存再落盘，
// 既即时生效，也顺带把不持久化的熔断器一并清掉，全程无停机。
func (h *Handler) poolReset(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UIDs            []string `json:"uids"`
		IncludeDisabled bool     `json:"includeDisabled"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	names := map[string]string{}
	for _, v := range listAccounts(h.cfg.AuthDir) {
		names[v.UID] = v.Nickname
	}
	outcomes := make([]resetOutcome, 0, len(body.UIDs))
	for _, uid := range body.UIDs {
		before, after := h.cfg.Pool.Reset(uid, body.IncludeDisabled)
		outcomes = append(outcomes, resetOutcome{
			UID:      uid,
			Nickname: names[uid],
			Before:   before,
			After:    after,
		})
	}
	h.cfg.Pool.Flush()
	logf("重置账号状态：%d 个（含禁用=%v）", len(body.UIDs), body.IncludeDisabled)
	ok(w, map[string]any{"outcomes": outcomes})
}

// regionModels 按区域列出可测模型，供「测试连接」的下拉框用。
//
// 面板不再自己抄一份模型表，也不反过来问自己的 /v1/models：网关内部已按区域维护
// 两套表（动态接口优先，失败回落各自静态表），直接取那份即可。
// degraded 表示某一区没拿到模型 —— 界面据此说明「这一区暂时测不了」。
func (h *Handler) regionModels(w http.ResponseWriter, r *http.Request) {
	if h.cfg.ModelsByRegion == nil {
		fail(w, http.StatusServiceUnavailable, "模型表不可用")
		return
	}
	cn, global := h.cfg.ModelsByRegion()
	resp := map[string]any{
		"cn":        cn,
		"global":    global,
		"reachable": true,
		"degraded":  len(cn) == 0 || len(global) == 0,
	}
	// 完整条目（含 credits 倍率等展示字段）：面板据此并排展示两区差异。
	// 与 cn/global 的 id 数组并存而非替换——「测试连接」下拉框仍用 id 数组。
	if h.cfg.ModelDetailsByRegion != nil {
		dcn, dglobal := h.cfg.ModelDetailsByRegion()
		resp["details"] = map[string]any{"cn": dcn, "global": dglobal}
	}
	writeJSON(w, http.StatusOK, resp)
}

// ── 单账号连通性测试 ────────────────────────────────────────────────
//
// 网关的 chat 端点按权重自己挑号，没法指定账号，所以「测某个账号」必须绕开网关直连
// 上游。这里复用 upstream.Client.ChatStream —— 它的请求头与线上流量逐字一致，
// 手拼一份的话少一个 X-Product 或 Origin 都可能被上游拒绝，届时测出来的失败是假的。

// testResult 一次连通性测试的结果。
type testResult struct {
	OK         bool   `json:"ok"`
	UID        string `json:"uid"`
	Nickname   string `json:"nickname"`
	Region     string `json:"region"`
	Model      string `json:"model"`
	HTTPStatus int    `json:"httpStatus"`
	// Code 上游业务错误码，0 表示没有
	Code int `json:"code"`
	// Message 成功时为「链路正常」，失败时为诊断文案
	Message   string `json:"message"`
	LatencyMs int64  `json:"latencyMs"`
	// FirstEvent 收到的首个 SSE 事件类型，成功时用于证明链路真的通了
	FirstEvent string `json:"firstEvent,omitempty"`
}

func (h *Handler) testAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UID   string `json:"uid"`
		Model string `json:"model"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.UID) == "" {
		fail(w, http.StatusBadRequest, "缺少 uid")
		return
	}
	// 不传模型时用 4.1-flash：两区都有，测的是账号而不是模型可用性
	if strings.TrimSpace(body.Model) == "" {
		body.Model = "deepseek-v4.1-flash"
	}
	res := h.runAccountTest(body.UID, body.Model)
	// 连通成功即把池中状态一并修正（清冷却/退避/熔断 + 解除禁用）并落盘。
	// 测试本身是诊断、绕开池子跑，若不写回，界面会一直显示「已禁用/冷却中」，
	// 而账号实际是好的 —— 用户看到的就是「测试通过但状态不变成正常」。
	// 失败不写回：一次失败不构成「账号可用」的证据，池子该保留自己的判断。
	if res.OK && h.cfg.Pool != nil {
		h.cfg.Pool.NoteTestOK(body.UID)
		h.cfg.Pool.Flush()
	}
	writeJSON(w, http.StatusOK, res)
}

// accountTestBudget 单次连通性测试的总时长上限。
// ChatStream 不接收 context，其头超时（120s）+ 首帧（30s）串起来最坏 150s；
// 诊断用途等不起 —— 外层用同首帧的 goroutine+select 模式封顶。
// 超时后内层 goroutine 仍会跑到自己的超时才退出（最多占住一条连接，低频可接受）。
// 用 var：测试把上限缩短，否则用例要真等满预算。
var accountTestBudget = 10 * time.Second

// runAccountTest 跑一次测试并施加总时长上限。不返回错误：失败也是一种结果，界面要原样展示原因。
func (h *Handler) runAccountTest(uid, model string) testResult {
	started := time.Now()
	ch := make(chan testResult, 1)
	go func() { ch <- h.probeAccount(uid, model) }()
	select {
	case res := <-ch:
		return res
	case <-time.After(accountTestBudget):
		return testResult{
			UID:       uid,
			Model:     model,
			LatencyMs: time.Since(started).Milliseconds(),
			Message:   fmt.Sprintf("测试超过 %ds 上限：上游未在本连接上响应", int(accountTestBudget.Seconds())),
		}
	}
}

// probeAccount 实际探测；超时兜底由 runAccountTest 负责。
// 用命名返回值：LatencyMs 由 defer 在出口处才填，具名才能写进真正的返回槽。
func (h *Handler) probeAccount(uid, model string) (res testResult) {
	res = testResult{UID: uid, Model: model}
	started := time.Now()
	defer func() { res.LatencyMs = time.Since(started).Milliseconds() }()

	file := "workbuddy-" + uid + ".json"
	if !authFileRE.MatchString(file) {
		res.Message = "uid 含非法字符：" + uid
		return res
	}
	raw, err := os.ReadFile(filepath.Join(h.cfg.AuthDir, file))
	if err != nil {
		res.Message = "读取凭证失败：" + err.Error()
		return res
	}
	acct, err := auth.Parse(raw)
	if err != nil {
		res.Message = "该账号文件无法解析：" + err.Error()
		return res
	}
	res.Nickname = acct.Nickname
	res.Region = string(acct.Region())

	// 上游只接受流式；首条必须是 system，否则 400 code=11128。
	// user 轮不能省：只有 system 的请求会被部分模型（实测 glm-5.3）判为参数不合规，
	// 回 400 code=11133；deepseek-v4.1-flash 恰好容忍，所以缺 user 时「换个模型就失败」，
	// 会误判成账号故障。
	reqBody, _ := json.Marshal(map[string]any{
		"model":      model,
		"stream":     true,
		"max_tokens": 16,
		"messages": []map[string]string{
			{"role": "system", "content": "You are a helpful assistant."},
			{"role": "user", "content": "hi"},
		},
	})

	// 空 clientIP：测试连接是诊断，不透传客户端 IP（面板请求来自本机，透传无意义）。
	rc, status, respBody, err := h.cfg.Upstream.ChatStream(acct, reqBody, "")
	res.HTTPStatus = status
	if err != nil {
		res.Message = "请求上游失败：" + err.Error()
		return res
	}
	if rc == nil {
		res.Code, res.Message = parseUpstreamError(respBody)
		res.Message = diagnoseUpstreamCode(res.Code, res.Message)
		return res
	}
	defer rc.Close()

	// 读首帧加 30s 上限：上游已经回了 200 却迟迟不吐数据时，不能让面板一直转圈。
	// 用 channel + 计时器而不是给 ChatStream 传 context —— 它不接收 context，
	// 而其 header 超时（默认 120s）只覆盖到响应头为止。
	ch := make(chan frameResult, 1)
	go func() {
		firstEvent, message, ok := firstFrame(rc)
		ch <- frameResult{firstEvent, message, ok}
	}()
	select {
	case fr := <-ch:
		res.FirstEvent, res.Message, res.OK = fr.firstEvent, fr.message, fr.ok
		if res.OK {
			res.Message = "链路正常"
		}
	case <-time.After(30 * time.Second):
		rc.Close() // 中断读取，上面那个 goroutine 随之退出
		res.Message = "读取首帧超时（30s）"
	}
	return res
}

type frameResult struct {
	firstEvent string
	message    string
	ok         bool
}

// firstFrame 从上游 SSE 流里读第一帧 data。
// 只读前 4KB：够拿到首帧即可，避免把整段流读完（一次连通性测试不该产生
// 一整个回复的费用和时延）。成功判据是「读到至少一个 data 帧」而不是只看状态码 ——
// 上游在 200 之后仍可能在流里发错误帧。
func firstFrame(rc io.Reader) (firstEvent, message string, ok bool) {
	var buf []byte
	tmp := make([]byte, 1024)
	for len(buf) < 4096 {
		n, err := rc.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if bytes.Contains(buf, []byte("\n")) {
				break
			}
		}
		if err != nil {
			break
		}
	}
	line := ""
	for _, l := range strings.Split(string(buf), "\n") {
		if strings.HasPrefix(l, "data:") {
			line = l
			break
		}
	}
	if line == "" {
		return "", "上游返回 200 但没有 SSE 数据帧", false
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" || payload == "[DONE]" {
		return payload, "", true
	}
	var c struct {
		Object  string `json:"object"`
		Choices []struct {
			Delta struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	// 首帧通常只有 role 没有 content，故按 role → content → object 依次取
	if err := json.Unmarshal([]byte(payload), &c); err != nil {
		return "chunk", "", true
	}
	if len(c.Choices) > 0 {
		d := c.Choices[0].Delta
		if d.Role != "" {
			return d.Role, "", true
		}
		if d.Content != "" {
			return d.Content, "", true
		}
	}
	if c.Object != "" {
		return c.Object, "", true
	}
	return "chunk", "", true
}

// parseUpstreamError 从上游错误体里取业务码与文案。
// 逐层取第一个非空值，对应原先的 e?.error?.data?.code ?? e?.code ?? 0。
//
// msg 与 message 都要取：上游业务信封用 `"msg"`（如 {"code":11140,"msg":"request illegal"}），
// 而 OAuth/billing 侧的报错用 `"message"`。只认 message 会让业务信封的文案丢失，
// 使 diagnoseUpstreamCode 拿不到判据（11140 的封控/限流两种形态正是靠 msg 区分的）。
func parseUpstreamError(raw []byte) (code int, message string) {
	var e map[string]any
	if json.Unmarshal(raw, &e) != nil {
		return 0, truncate(string(raw), 300)
	}
	errObj, _ := e["error"].(map[string]any)
	data, _ := errObj["data"].(map[string]any)
	return firstInt(data["code"], e["code"]),
		firstStr(data["message"], data["msg"], errObj["message"], errObj["msg"], e["message"], e["msg"])
}

func firstInt(vals ...any) int {
	for _, v := range vals {
		if f, ok := v.(float64); ok {
			return int(f)
		}
	}
	return 0
}

func firstStr(vals ...any) string {
	for _, v := range vals {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// diagnoseUpstreamCode 上游错误码 → 中文诊断。语义见 docs 与 IDE 多语言表。
func diagnoseUpstreamCode(code int, message string) string {
	switch code {
	case 14016:
		return "企业未激活（14016）。该企业主体还没开通服务。"
	case 14017:
		return "账号未激活（14017）。网关登录不会激活账号，需用 CodeBuddy IDE 登录一次（会多一步选择/开通），激活后再把凭证导入网关。"
	case 14018:
		return "Credits 已耗尽（14018）。等签到补充，或换账号。"
	case 14019:
		return "token 配额耗尽（14019）。本周期用量已达上限。"
	case 11102:
		return "模型不存在（11102）。该模型不在本账号所属区域的模型表里 —— 例如 intl 区没有 deepseek-v4-flash / deepseek-v4-pro。"
	case 11133:
		return "模型拒绝了请求参数（11133）。该模型对请求格式有额外要求，换一个模型通常就能通 —— 不代表账号有问题。"
	case 11140:
		// 11140 是复用码：限流与安全封控都用它，靠 msg 区分（见 upstream.IsSafetyBanned）。
		if strings.Contains(strings.ToLower(message), "request illegal") {
			return "账号级内容安全封控（11140 request illegal）。上游对**这个账号**的 chat 通道返回「内容未通过安全审核」，" +
				"与请求内容无关（最小探针同样被拒），重新登录也不会解除。该账号已被网关长冷却（默认 6 小时），" +
				"期间不再参与轮换；到期后自动回到池中，也可在面板「重置」或测试连通后立即恢复。" +
				"若持续被拒，请到上游官网提交申诉。"
		}
		return "上游限流（11140）。该模型此刻繁忙，账号本身没问题，稍后重试。"
	case 6004:
		return "上游限流（6004）。该模型此刻繁忙，账号本身没问题，稍后重试。"
	default:
		if code != 0 {
			if message != "" {
				return fmt.Sprintf("上游错误 %d：%s", code, message)
			}
			return fmt.Sprintf("上游错误 %d", code)
		}
		if message == "" {
			return "未知失败"
		}
		return message
	}
}
