package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
)

// ── config.json 读写 ────────────────────────────────────────────────
//
// 密钥存在 config.json 里，而那个文件是人在维护的：有注释般的分组、有自己习惯的
// 缩进、可能还有别的键。所以这里刻意**不做 JSON 往返**（parse → 改 → stringify）——
// 那会重排整个文件、丢掉原始格式，还可能把大整数压成浮点。改为只在文本上替换
// api_key / api_keys 两个值的区间，其余字节原样保留。
//
// 写入前还会把结果重新解析并与预期做深比较，不一致就放弃写入：
// config.json 写坏会让网关直接起不来，宁可这次保存失败。

// skipString 跳过一段字符串字面量，返回结束引号之后的下标。s[i] 必须是开引号。
func skipString(s string, i int) int {
	i++
	for i < len(s) {
		if s[i] == '\\' {
			i += 2
			continue
		}
		if s[i] == '"' {
			return i + 1
		}
		i++
	}
	return i
}

// scanValueEnd 从 start 起吃掉一个完整 JSON 值，返回其结束下标（不含）。
func scanValueEnd(s string, start int) int {
	if start >= len(s) {
		return len(s)
	}
	c := s[start]
	if c == '"' {
		return skipString(s, start)
	}
	if c == '[' || c == '{' {
		open := c
		closer := byte('}')
		if c == '[' {
			closer = ']'
		}
		depth := 0
		for i := start; i < len(s); {
			ch := s[i]
			if ch == '"' {
				i = skipString(s, i)
				continue
			}
			if ch == open {
				depth++
			} else if ch == closer {
				depth--
				if depth == 0 {
					return i + 1
				}
			}
			i++
		}
		return len(s)
	}
	// 数字 / true / false / null：读到分隔符为止
	i := start
	for i < len(s) && !isSpace(s[i]) && !strings.ContainsRune(",}]", rune(s[i])) {
		i++
	}
	return i
}

// depthAt 下标 pos 处的嵌套深度（根对象的成员 = 1）。
func depthAt(s string, pos int) int {
	depth := 0
	for i := 0; i < pos; {
		ch := s[i]
		if ch == '"' {
			i = skipString(s, i)
			continue
		}
		if ch == '{' || ch == '[' {
			depth++
		} else if ch == '}' || ch == ']' {
			depth--
		}
		i++
	}
	return depth
}

// findTopLevelValue 定位顶层键的值区间 [start, end)；键不存在返回 false。
func findTopLevelValue(s, key string) (start, end int, found bool) {
	needle := `"` + key + `"`
	for idx := strings.Index(s, needle); idx >= 0; {
		// 只认顶层成员：深度须为 1，且紧跟冒号（借此排除同名的字符串值）
		if depthAt(s, idx) == 1 {
			i := idx + len(needle)
			for i < len(s) && isSpace(s[i]) {
				i++
			}
			if i < len(s) && s[i] == ':' {
				i++
				for i < len(s) && isSpace(s[i]) {
					i++
				}
				return i, scanValueEnd(s, i), true
			}
		}
		next := strings.Index(s[idx+1:], needle)
		if next < 0 {
			break
		}
		idx = idx + 1 + next
	}
	return 0, 0, false
}

// reindent 把 JSON 值序列化成多行文本，续行按 indent 缩进，行尾用 eol。
//
// 必须用 Encoder 并关掉 HTML 转义：Go 的 MarshalIndent 默认把 < > & 转成
// \u003c 之类，而 JS 的 JSON.stringify 不转。一旦转义，写入前的一致性断言就会
// 判定不一致、整个保存被放弃 —— 表现为「改密钥永远失败」，且原因极难看出。
func reindent(v any, indent, eol string) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) == 1 {
		return lines[0], nil
	}
	for i := 1; i < len(lines); i++ {
		lines[i] = indent + lines[i]
	}
	return strings.Join(lines, eol), nil
}

// detectEOL 原文件使用的换行符。必须沿用：序列化只会产出 \n，直接插入会把 CRLF
// 文件写成混合行尾（语义没问题，但字节级不再一致，后续编辑器的行为也变得不可预期）。
func detectEOL(s string) string {
	if strings.Contains(s, "\r\n") {
		return "\r\n"
	}
	return "\n"
}

// indentAt 值所在行的前导空白，用于让替换后的缩进与原文件一致。
func indentAt(s string, at int) string {
	if at > len(s) {
		at = len(s)
	}
	lineStart := strings.LastIndex(s[:at], "\n") + 1
	seg := s[lineStart:at]
	i := 0
	for i < len(seg) && isSpace(seg[i]) {
		i++
	}
	return seg[:i]
}

// setTopLevelValue 替换顶层键的值；键不存在时追加到根对象末尾。返回新文本。
func setTopLevelValue(s, key string, value any) (string, error) {
	eol := detectEOL(s)
	if start, end, found := findTopLevelValue(s, key); found {
		text, err := reindent(value, indentAt(s, start), eol)
		if err != nil {
			return "", err
		}
		return s[:start] + text + s[end:], nil
	}
	// 键不存在：插到根对象的右花括号之前
	rootStart := strings.IndexByte(s, '{')
	if rootStart < 0 {
		return "", fmt.Errorf("config.json 结构异常：找不到根对象")
	}
	rootEnd := scanValueEnd(s, rootStart) - 1
	if rootEnd < rootStart {
		return "", fmt.Errorf("config.json 结构异常：根对象未闭合")
	}
	body := strings.TrimRight(s[rootStart+1:rootEnd], " \t\r\n\f\v")
	sep := ","
	if body == "" || strings.HasSuffix(body, ",") {
		sep = ""
	}
	text, err := reindent(value, "  ", eol)
	if err != nil {
		return "", err
	}
	chunk := sep + eol + "  " + strconv.Quote(key) + ": " + text
	return s[:rootEnd] + chunk + eol + s[rootEnd:], nil
}

func isSpace(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	}
	return false
}

// toAny 把值过一遍 JSON 往返，用于和 json.Unmarshal 出来的结构做深比较
// （两边必须是同一套类型，否则 []KeyEntry 与 []any 永远不相等）。
func toAny(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out any
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

// ── 密钥读写 ────────────────────────────────────────────────────────

func (h *Handler) readKeys() ([]KeyEntry, string, error) {
	var fc struct {
		APIKey  string     `json:"api_key"`
		APIKeys []KeyEntry `json:"api_keys"`
	}
	if err := h.readConfig(&fc); err != nil {
		return nil, "", err
	}
	if fc.APIKeys == nil {
		fc.APIKeys = []KeyEntry{}
	}
	return fc.APIKeys, fc.APIKey, nil
}

func (h *Handler) getKeys(w http.ResponseWriter, r *http.Request) {
	keys, legacy, err := h.readKeys()
	if err != nil {
		fail(w, http.StatusInternalServerError, "读取 config.json 失败：%v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configPath": h.cfg.ConfigPath,
		"legacyKey":  legacy,
		"keys":       keys,
	})
}

// validateKeys 镜像 cmd/server/config.go 的 validateAPIKeys。必须在写入前跑一遍：
// 写进非法配置会让网关下次启动直接失败。
func validateKeys(legacyKey string, keys []KeyEntry) string {
	seen := map[string]string{}
	if lk := strings.TrimSpace(legacyKey); lk != "" {
		seen[lk] = "顶层 api_key"
	}
	for i, k := range keys {
		key := strings.TrimSpace(k.Key)
		if key == "" {
			return fmt.Sprintf("第 %d 项：密钥不得为空", i+1)
		}
		region := strings.ToLower(strings.TrimSpace(k.Region))
		if region != "" && region != "cn" && region != "global" {
			return fmt.Sprintf("第 %d 项：区域只能是 cn 或 global", i+1)
		}
		if prev, dup := seen[key]; dup {
			return fmt.Sprintf("第 %d 项：密钥与「%s」重复（同一密钥不能绑定两个区域）", i+1, prev)
		}
		seen[key] = fmt.Sprintf("第 %d 项", i+1)
	}
	return ""
}

func (h *Handler) saveKeys(w http.ResponseWriter, r *http.Request) {
	var body struct {
		LegacyKey            string     `json:"legacyKey"`
		Keys                 []KeyEntry `json:"keys"`
		AllowUnauthenticated bool       `json:"allowUnauthenticated"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	legacyKey := strings.TrimSpace(body.LegacyKey)
	keys := make([]KeyEntry, 0, len(body.Keys))
	for _, k := range body.Keys {
		keys = append(keys, KeyEntry{
			Key:    strings.TrimSpace(k.Key),
			Region: strings.ToLower(strings.TrimSpace(k.Region)),
			Name:   strings.TrimSpace(k.Name),
		})
	}
	if bad := validateKeys(legacyKey, keys); bad != "" {
		fail(w, http.StatusBadRequest, "%s", bad)
		return
	}

	// 密钥清空 = 网关不再鉴权（handler.go 里 len(h.keys)==0 时直接放行）。
	// 这是一次安全降级，必须让用户显式确认，不能当普通保存处理。
	if legacyKey == "" && len(keys) == 0 && !body.AllowUnauthenticated {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":                       "保存后密钥列表为空，网关将不再校验任何密钥（任何人都能调用）。如确需如此，请再确认一次。",
			"needsUnauthenticatedConfirm": true,
		})
		return
	}

	raw, err := os.ReadFile(h.cfg.ConfigPath)
	if err != nil {
		fail(w, http.StatusInternalServerError, "读取 config.json 失败：%v", err)
		return
	}
	text, err := setTopLevelValue(string(raw), "api_key", legacyKey)
	if err != nil {
		fail(w, http.StatusInternalServerError, "编辑 config.json 失败：%v", err)
		return
	}
	if text, err = setTopLevelValue(text, "api_keys", keys); err != nil {
		fail(w, http.StatusInternalServerError, "编辑 config.json 失败：%v", err)
		return
	}

	// 一致性断言：结果须能解析，且除这两个键外与原配置逐字段相同。
	var after, before map[string]any
	if err := json.Unmarshal([]byte(text), &after); err != nil {
		fail(w, http.StatusInternalServerError, "内部错误：编辑结果不是合法 JSON，已放弃写入（%v）", err)
		return
	}
	if err := json.Unmarshal(raw, &before); err != nil {
		fail(w, http.StatusInternalServerError, "内部错误：原配置无法解析（%v）", err)
		return
	}
	expected := make(map[string]any, len(before)+2)
	for k, v := range before {
		expected[k] = v
	}
	expected["api_key"] = legacyKey
	expected["api_keys"] = toAny(keys)
	if !reflect.DeepEqual(after, expected) {
		fail(w, http.StatusInternalServerError, "内部错误：编辑结果与预期不一致，已放弃写入（config.json 未被修改）")
		return
	}

	backup := h.cfg.ConfigPath + ".bak"
	if err := os.WriteFile(backup, raw, 0o600); err != nil {
		fail(w, http.StatusInternalServerError, "写备份失败：%v", err)
		return
	}
	tmp := h.cfg.ConfigPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o600); err != nil {
		fail(w, http.StatusInternalServerError, "写入失败：%v", err)
		return
	}
	if err := os.Rename(tmp, h.cfg.ConfigPath); err != nil {
		fail(w, http.StatusInternalServerError, "替换 config.json 失败：%v", err)
		return
	}

	// 热重载：密钥立即生效，不需要重启进程
	if h.cfg.ReloadKeys != nil {
		h.cfg.ReloadKeys(legacyKey, keys)
	}
	n := len(keys)
	if legacyKey != "" {
		n++
	}
	logf("保存密钥：%d 把（备份 %s），已热重载", n, backup)
	ok(w, map[string]any{
		"configPath": h.cfg.ConfigPath,
		"backup":     backup,
		"keyCount":   n,
	})
}
