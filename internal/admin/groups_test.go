// groups_test.go 账号分组端点的行为守则。
//
// 重点不是「能不能存进去」，而是两条容易漏掉的**联动**：
//   - 重命名分组必须同时改写 key 里存的组名，否则 key 指向一个不存在的组、
//     行为从「用该组账号」变成「组内无账号」而全失败；
//   - 删除分组必须解绑引用它的 key（同上），且**不删账号**。
//
// 另外守住 saveKeys 的整表替换不抹掉 Group —— 这个 bug 实测踩到过。
package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGroups 内存版 GroupsController。
type fakeGroups struct {
	m     map[string]string
	extra []string
}

func (f *fakeGroups) Snapshot() map[string]string {
	out := make(map[string]string, len(f.m))
	for k, v := range f.m {
		out[k] = v
	}
	return out
}
func (f *fakeGroups) SetAll(m map[string]string) {
	f.m = make(map[string]string, len(m))
	for k, v := range m {
		f.m[k] = v
	}
}
func (f *fakeGroups) Names() []string {
	seen := map[string]bool{}
	var out []string
	for _, g := range f.m {
		if g != "" && !seen[g] {
			seen[g] = true
			out = append(out, g)
		}
	}
	for _, g := range f.extra {
		if g != "" && !seen[g] {
			seen[g] = true
			out = append(out, g)
		}
	}
	return out
}
func (f *fakeGroups) GroupOf(uid string) string { return f.m[uid] }
func (f *fakeGroups) EmptyNames() []string      { return append([]string{}, f.extra...) }
func (f *fakeGroups) SetEmpty(names []string)   { f.extra = append([]string{}, names...) }

// writeConfig 落一份最小 config.json（含密钥与分组），返回 Handler。
func groupsTestHandler(t *testing.T, cfgText string, g *fakeGroups) (*Handler, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(cfgText), 0o600); err != nil {
		t.Fatal(err)
	}
	var reloaded struct {
		legacy string
		keys   []KeyEntry
	}
	h := New(Config{
		ConfigPath: path,
		Groups:     g,
		ReloadKeys: func(legacy string, keys []KeyEntry) {
			reloaded.legacy, reloaded.keys = legacy, keys
		},
	})
	return h, path
}

func postAdmin(t *testing.T, h *Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// readConfigFile 把落盘结果解析成泛型 map，便于断言。
func readConfigFile(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("config.json 已被写坏: %v\n%s", err, raw)
	}
	return out
}

const groupsTestConfig = `{
  "api_key": "",
  "api_keys": [
    {"key": "k1", "region": "cn", "name": "普通", "group": ""},
    {"key": "k2", "region": "", "name": "受限", "group": "受限"}
  ],
  "account_groups": {"u1": "受限", "u2": "受限", "u3": "其它"}
}`

// TestGroupRenameRewritesKeyBindings 改名分组必须同步改写 key 里的组名。
func TestGroupRenameRewritesKeyBindings(t *testing.T) {
	g := &fakeGroups{m: map[string]string{"u1": "受限", "u2": "受限", "u3": "其它"}}
	h, path := groupsTestHandler(t, groupsTestConfig, g)

	rec := postAdmin(t, h, "/__admin/groups/rename", `{"from":"受限","to":"封禁"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename: code=%d body=%s", rec.Code, rec.Body)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["accounts"].(float64) != 2 || body["keys"].(float64) != 1 {
		t.Errorf("accounts/keys = %v/%v，期望 2/1", body["accounts"], body["keys"])
	}

	// 内存表
	if g.m["u1"] != "封禁" || g.m["u3"] != "其它" {
		t.Errorf("账号归属未按预期改写: %v", g.m)
	}
	// 落盘：账号归属与 key 绑定都要变
	cfg := readConfigFile(t, path)
	ag := cfg["account_groups"].(map[string]any)
	if ag["u1"] != "封禁" {
		t.Errorf("account_groups 未落盘: %v", ag)
	}
	keys := cfg["api_keys"].([]any)
	if keys[1].(map[string]any)["group"] != "封禁" {
		t.Errorf("key 绑定未跟着改名: %v", keys[1])
	}
	// 未绑定分组的 key 不能被顺带改掉
	if _, has := keys[0].(map[string]any)["group"]; has && keys[0].(map[string]any)["group"] != "" {
		t.Errorf("未绑定的 key 被误改: %v", keys[0])
	}
}

// TestGroupDeleteUnbindsKeysAndKeepsAccounts 删组只解除归属，不删账号文件。
func TestGroupDeleteUnbindsKeysAndKeepsAccounts(t *testing.T) {
	// 内存表必须与 config 一致：端点以 Snapshot() 为准整体回写，
	// 少了 u3 就等于「删组顺带把别的组也清了」（测试夹具不一致，不是实现问题）。
	g := &fakeGroups{m: map[string]string{"u1": "受限", "u2": "受限", "u3": "其它"}}
	h, path := groupsTestHandler(t, groupsTestConfig, g)

	rec := postAdmin(t, h, "/__admin/groups/delete", `{"name":"受限"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: code=%d body=%s", rec.Code, rec.Body)
	}
	if _, still := g.m["u1"]; still {
		t.Errorf("账号仍留在已删除的分组里: %v", g.m)
	}
	cfg := readConfigFile(t, path)
	if ag := cfg["account_groups"].(map[string]any); len(ag) != 1 || ag["u3"] != "其它" {
		t.Errorf("account_groups 未按预期清空该组: %v", ag)
	}
	keys := cfg["api_keys"].([]any)
	// 绑定该组的 key 被解绑（group 置空/缺省），否则它会指向不存在的组
	if got := keys[1].(map[string]any)["group"]; got != nil && got != "" {
		t.Errorf("绑定已删分组的 key 未被解绑: %v", got)
	}
}

// TestSaveKeysKeepsGroup 整表替换密钥时不能抹掉分组绑定。
//
// 回归用：saveKeys 曾在组装 KeyEntry 时漏掉 Group 字段，用户每保存一次密钥
// 就把所有分组绑定静默清零。
func TestSaveKeysKeepsGroup(t *testing.T) {
	g := &fakeGroups{m: map[string]string{"u1": "受限"}}
	h, path := groupsTestHandler(t, groupsTestConfig, g)

	body := `{"legacyKey":"","keys":[
		{"key":"k1","region":"cn","name":"普通","group":"受限"},
		{"key":"k2","region":"","name":"受限","group":"受限"}
	]}`
	rec := postAdmin(t, h, "/__admin/apikeys", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("saveKeys: code=%d body=%s", rec.Code, rec.Body)
	}
	cfg := readConfigFile(t, path)
	for i, raw := range cfg["api_keys"].([]any) {
		if got := raw.(map[string]any)["group"]; got != "受限" {
			t.Errorf("第 %d 把密钥的分组丢了: %v", i+1, got)
		}
	}
}

// TestGroupSetBatch 批量设置：空组名 = 移出分组。
func TestGroupSetBatch(t *testing.T) {
	g := &fakeGroups{m: map[string]string{"u1": "受限"}}
	h, _ := groupsTestHandler(t, groupsTestConfig, g)

	rec := postAdmin(t, h, "/__admin/groups/set", `{"uids":["u2","u3"],"group":"受限"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("set: code=%d body=%s", rec.Code, rec.Body)
	}
	if g.m["u2"] != "受限" || g.m["u3"] != "受限" || g.m["u1"] != "受限" {
		t.Errorf("批量设置结果不符: %v", g.m)
	}

	rec = postAdmin(t, h, "/__admin/groups/set", `{"uids":["u1"],"group":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("set 空组: code=%d body=%s", rec.Code, rec.Body)
	}
	if _, still := g.m["u1"]; still {
		t.Errorf("空组名应移出分组: %v", g.m)
	}
}

// TestGroupRejectsIllegalName 分组名里的换行/斜杠会让导出与日志混乱，必须拒绝。
func TestGroupRejectsIllegalName(t *testing.T) {
	g := &fakeGroups{m: map[string]string{}}
	h, _ := groupsTestHandler(t, groupsTestConfig, g)
	for _, name := range []string{"a/b", "a\\b", "a\nb", "a\tb"} {
		rec := postAdmin(t, h, "/__admin/groups/set",
			`{"uids":["u1"],"group":`+strconvQuote(name)+`}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("分组名 %q 未被拒绝: code=%d", name, rec.Code)
		}
	}
}

// TestGroupCreateEmptySurvives 新建的空组必须能被读回、并落进 config。
//
// 分组的唯一数据是 uid→名；没有成员的组若不单独记名，建完就从下拉里消失，
// 「先建组、再放号」这个操作顺序做不到。
func TestGroupCreateEmptySurvives(t *testing.T) {
	g := &fakeGroups{m: map[string]string{"u1": "受限"}}
	h, path := groupsTestHandler(t, groupsTestConfig, g)

	rec := postAdmin(t, h, "/__admin/groups/create", `{"name":"待分配"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: code=%d body=%s", rec.Code, rec.Body)
	}
	names := g.Names()
	if !contains(names, "待分配") {
		t.Errorf("新建的空组没出现在 Names(): %v", names)
	}
	cfg := readConfigFile(t, path)
	raw, _ := json.Marshal(cfg["account_group_names"])
	if !strings.Contains(string(raw), "待分配") {
		t.Errorf("空组名未落盘: %v", cfg["account_group_names"])
	}

	// 幂等：再建一次不报错、不重复
	rec = postAdmin(t, h, "/__admin/groups/create", `{"name":"待分配"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("重复 create 应幂等: code=%d", rec.Code)
	}
	if n := len(g.EmptyNames()); n != 1 {
		t.Errorf("空组名重复: %v", g.EmptyNames())
	}

	// 给账号设成这个组后，它不再是空组
	rec = postAdmin(t, h, "/__admin/groups/set", `{"uids":["u2"],"group":"待分配"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("set: code=%d body=%s", rec.Code, rec.Body)
	}
	if contains(g.EmptyNames(), "待分配") {
		t.Errorf("已有成员的组仍被记为空组: %v", g.EmptyNames())
	}
	if !contains(g.Names(), "待分配") {
		t.Errorf("有成员后组名消失: %v", g.Names())
	}
}

// TestGroupDeleteRemovesEmptyName 删组要把空组名一起清掉，否则会留下幽灵组。
func TestGroupDeleteRemovesEmptyName(t *testing.T) {
	g := &fakeGroups{m: map[string]string{}, extra: []string{"空组"}}
	h, path := groupsTestHandler(t, groupsTestConfig, g)

	rec := postAdmin(t, h, "/__admin/groups/delete", `{"name":"空组"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: code=%d body=%s", rec.Code, rec.Body)
	}
	if contains(g.Names(), "空组") {
		t.Errorf("删除后组名仍在: %v", g.Names())
	}
	cfg := readConfigFile(t, path)
	raw, _ := json.Marshal(cfg["account_group_names"])
	if strings.Contains(string(raw), "空组") {
		t.Errorf("空组名未从 config 清掉: %s", raw)
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// strconvQuote 用 encoding/json 的转义规则引一个字符串（避免手写 \n 时出错）。
func strconvQuote(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}
