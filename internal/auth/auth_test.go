package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseNested(t *testing.T) {
	raw := []byte(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1753600000,"domain":""},"account":{"uid":"u1","enterpriseId":"e1","nickname":"n1"}}`)
	sa, err := Parse(raw)
	if err != nil {
		t.Fatalf("nested parse err: %v", err)
	}
	if sa.AccessToken != "at" || sa.RefreshToken != "rt" || sa.ExpiresAt != 1753600000 {
		t.Errorf("tokens: %+v", sa)
	}
	if sa.UID != "u1" || sa.EnterpriseID != "e1" || sa.Nickname != "n1" {
		t.Errorf("account: %+v", sa)
	}
}

func TestParseFlat(t *testing.T) {
	raw := []byte(`{"accessToken":"at","refreshToken":"rt","expiresAt":1753600000,"uid":"u2","nickname":"n2"}`)
	sa, err := Parse(raw)
	if err != nil || sa.UID != "u2" || sa.AccessToken != "at" {
		t.Fatalf("flat: %+v %v", sa, err)
	}
}

func TestParseMissingToken(t *testing.T) {
	if _, err := Parse([]byte(`{"uid":"u3"}`)); err == nil {
		t.Fatal("want error for missing accessToken")
	}
}

func TestSaveAtomicRoundtrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u1.json")
	a := &Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1753600000,
		UID: "u1", EnterpriseID: "e1", Nickname: "n1", FilePath: fp}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(fp + ".tmp"); !os.IsNotExist(err) {
		t.Error("tmp file should not remain")
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	b, err := Parse(raw)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if b.AccessToken != "at" || b.UID != "u1" || b.EnterpriseID != "e1" {
		t.Errorf("roundtrip: %+v", b)
	}
}

// TestLoadDirLoadsAllValid 不再按 region 过滤：所有可解析的 auth 文件都被加载，
// 解析失败的文件静默跳过。
func TestLoadDirLoadsAllValid(t *testing.T) {
	dir := t.TempDir()
	cn := `{"auth":{"accessToken":"at1","refreshToken":"r","expiresAt":1,"domain":""},"account":{"uid":"cn1"}}`
	other := `{"auth":{"accessToken":"at2","refreshToken":"r","expiresAt":1,"domain":"example.com"},"account":{"uid":"u2"}}`
	bad := `not json`
	os.WriteFile(filepath.Join(dir, "workbuddy-cn1.json"), []byte(cn), 0o600)
	os.WriteFile(filepath.Join(dir, "workbuddy-u2.json"), []byte(other), 0o600)
	os.WriteFile(filepath.Join(dir, "workbuddy-bad.json"), []byte(bad), 0o600)

	list, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 valid accounts, got %+v", list)
	}
	for _, a := range list {
		if a.FilePath == "" {
			t.Error("FilePath not set")
		}
	}
}

func TestNeedsRefresh(t *testing.T) {
	a := &Auth{ExpiresAt: 0}
	if !a.NeedsRefresh(0) {
		t.Error("zero expiry should need refresh")
	}
	a.ExpiresAt = 9999999999
	if a.NeedsRefresh(0) {
		t.Error("far future should not need refresh")
	}
}

func TestRegion(t *testing.T) {
	cases := []struct {
		domain string
		want   Region
	}{
		{"copilot.tencent.com", RegionCN},
		{"www.codebuddy.cn", RegionCN},
		{"", RegionCN},                       // 空 domain 向后兼容为 CN
		{"example.com", RegionCN},            // 未知域名归 CN
		{"www.workbuddy.ai", RegionGlobal},   // 国外版
		{"workbuddy.ai", RegionGlobal},       // 裸域名（无前导点）
		{"api.workbuddy.ai", RegionGlobal},   // 子域
		{"WWW.WorkBuddy.AI", RegionGlobal},   // 大小写不敏感
		{" www.workbuddy.ai ", RegionGlobal}, // 前后空白
	}
	for _, c := range cases {
		a := &Auth{Domain: c.domain}
		if got := a.Region(); got != c.want {
			t.Errorf("Region(domain=%q) = %q, want %q", c.domain, got, c.want)
		}
	}
}

// TestParseDeviceToken 嵌套形与扁平形 auth 文件的顶层 device_token 键均被解析。
func TestParseDeviceToken(t *testing.T) {
	nested := []byte(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1,"domain":""},"account":{"uid":"u1"},"device_token":"dev-tok-nested"}`)
	sa, err := Parse(nested)
	if err != nil {
		t.Fatalf("nested parse: %v", err)
	}
	if sa.DeviceToken != "dev-tok-nested" {
		t.Errorf("nested DeviceToken = %q want %q", sa.DeviceToken, "dev-tok-nested")
	}

	flat := []byte(`{"accessToken":"at","refreshToken":"rt","expiresAt":1,"uid":"u2","device_token":"dev-tok-flat"}`)
	fa, err := Parse(flat)
	if err != nil {
		t.Fatalf("flat parse: %v", err)
	}
	if fa.DeviceToken != "dev-tok-flat" {
		t.Errorf("flat DeviceToken = %q want %q", fa.DeviceToken, "dev-tok-flat")
	}
}

// TestSaveAtomicPreservesDeviceToken SaveAtomic 写回后顶层 device_token 被保留并重新解析回来。
func TestSaveAtomicPreservesDeviceToken(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-dt.json")
	a := &Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1,
		UID: "u1", DeviceToken: "persisted-tok", FilePath: fp}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	b, err := Parse(raw)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if b.DeviceToken != "persisted-tok" {
		t.Errorf("roundtrip DeviceToken = %q want %q", b.DeviceToken, "persisted-tok")
	}
}

// TestCreatedAtRoundTrip created_at 必须能解析、落盘保留、且缺省为 0（不伪造）。
//
// 背景（2026-10-21）：上游不暴露账号真实注册时间，而 auths/*.json 的文件时间会被
// token 刷新整份重写覆盖，因此「账号何时入库」无法从任何既有来源回溯。加此字段后：
//   - 首次落盘（登录/导入）记一次；
//   - 之后每次 SaveAtomic 整份重写都**保留**它；
//   - 旧凭证（无该字段）解析为 0 = 未知，**不猜测**。
func TestCreatedAtRoundTrip(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "workbuddy-u1.json")
	a := &Auth{
		AccessToken: "at", RefreshToken: "rt", ExpiresAt: 123, Domain: "www.workbuddy.ai",
		UID: "u1", Nickname: "n1", FilePath: f, CreatedAt: 1700000000,
	}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "created_at") {
		t.Errorf("落盘文件应含 created_at：%s", raw)
	}
	got, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.CreatedAt != 1700000000 {
		t.Errorf("created_at=%d want 1700000000（应原样保留）", got.CreatedAt)
	}

	// 旧凭证无该字段 → 0（未知），且 SaveAtomic 不写空键。
	b := &Auth{AccessToken: "at2", UID: "u2", FilePath: filepath.Join(dir, "workbuddy-u2.json")}
	if err := b.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	raw2, _ := os.ReadFile(b.FilePath)
	if strings.Contains(string(raw2), "created_at") {
		t.Errorf("CreatedAt=0 时不应写 created_at 键：%s", raw2)
	}
	if p2, _ := Parse(raw2); p2.CreatedAt != 0 {
		t.Errorf("无该字段应解析为 0，得到 %d", p2.CreatedAt)
	}
}
