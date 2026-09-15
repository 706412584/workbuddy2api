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

func (h *Handler) listAccounts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"authDir":  h.cfg.AuthDir,
		"accounts": listAccounts(h.cfg.AuthDir),
	})
}

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

// importAccount 手填/粘贴凭证：字段名沿用网关的嵌套形。
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
	// 先按网关的解析器验一遍：能解析才落盘，否则写进去也是个起不来的账号
	a, err := auth.Parse([]byte(body.JSON))
	if err != nil {
		fail(w, http.StatusBadRequest, "JSON 解析失败：%v", err)
		return
	}
	if strings.TrimSpace(a.UID) == "" {
		fail(w, http.StatusBadRequest, "凭证缺少 uid")
		return
	}
	file, err := h.writeAuthFile(a.UID, []byte(body.JSON))
	if err != nil {
		fail(w, http.StatusInternalServerError, "写入凭证失败：%v", err)
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
	writeJSON(w, http.StatusOK, map[string]any{
		"cn":        cn,
		"global":    global,
		"reachable": true,
		"degraded":  len(cn) == 0 || len(global) == 0,
	})
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
func parseUpstreamError(raw []byte) (code int, message string) {
	var e map[string]any
	if json.Unmarshal(raw, &e) != nil {
		return 0, truncate(string(raw), 300)
	}
	errObj, _ := e["error"].(map[string]any)
	data, _ := errObj["data"].(map[string]any)
	return firstInt(data["code"], e["code"]), firstStr(data["message"], errObj["message"], e["message"])
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
