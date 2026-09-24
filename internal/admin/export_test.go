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

// writeFullAuth 写一个带完整 token 的凭证（导出用例需要真 token 才验得出剥离与否）。
func writeFullAuth(t *testing.T, dir, uid, at, rt string) {
	t.Helper()
	raw := `{"account":{"uid":"` + uid + `","nickname":"n-` + uid + `","enterpriseId":"e1"},` +
		`"auth":{"accessToken":"` + at + `","refreshToken":"` + rt + `","expiresAt":9999999999,"domain":"www.codebuddy.cn"}}`
	if err := os.WriteFile(filepath.Join(dir, "workbuddy-"+uid+".json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

func doExport(t *testing.T, h *Handler, query string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/__admin/accounts/export"+query, nil)
	req.RemoteAddr = "127.0.0.1:12345" // admin 有本机限制
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("export %s: code=%d body=%s", query, rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("export 响应非 JSON: %v", err)
	}
	return out
}

// TestExportOmitsTokensByDefault 默认导出**不含 token** ——
// 这是安全底线：点一下导出不该把账号接管凭据泄到磁盘。
func TestExportOmitsTokensByDefault(t *testing.T) {
	dir := t.TempDir()
	writeFullAuth(t, dir, "u1", "SECRET_AT_1", "SECRET_RT_1")
	writeFullAuth(t, dir, "u2", "SECRET_AT_2", "SECRET_RT_2")
	h := New(Config{AuthDir: dir})

	body := doExport(t, h, "")
	if body["includeTokens"] != false {
		t.Errorf("includeTokens=%v want false", body["includeTokens"])
	}
	if n := body["count"].(float64); n != 2 {
		t.Errorf("count=%v want 2", n)
	}
	// 原始响应体里绝不能出现 token 明文
	raw, _ := json.Marshal(body)
	for _, secret := range []string{"SECRET_AT_1", "SECRET_RT_1", "SECRET_AT_2", "SECRET_RT_2"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("默认导出泄漏了 token 明文：%s", secret)
		}
	}
	// 元信息仍要有
	accts := body["accounts"].([]any)
	first := accts[0].(map[string]any)
	if first["account"].(map[string]any)["uid"] == nil {
		t.Error("元信息 uid 缺失")
	}
	if _, has := first["auth"]; has {
		t.Error("默认导出不应有 auth 段（含 token）")
	}
}

// TestExportWithTokens 显式 include_tokens=true 才带完整凭证。
func TestExportWithTokens(t *testing.T) {
	dir := t.TempDir()
	writeFullAuth(t, dir, "u1", "SECRET_AT_1", "SECRET_RT_1")
	h := New(Config{AuthDir: dir})

	body := doExport(t, h, "?include_tokens=true")
	if body["includeTokens"] != true {
		t.Errorf("includeTokens=%v want true", body["includeTokens"])
	}
	raw, _ := json.Marshal(body)
	if !strings.Contains(string(raw), "SECRET_AT_1") {
		t.Error("显式导出应含 accessToken")
	}
	if !strings.Contains(string(raw), "SECRET_RT_1") {
		t.Error("显式导出应含 refreshToken")
	}
}

// TestExportSingleFile 带 file 参数只导一个。
func TestExportSingleFile(t *testing.T) {
	dir := t.TempDir()
	writeFullAuth(t, dir, "u1", "AT1", "RT1")
	writeFullAuth(t, dir, "u2", "AT2", "RT2")
	h := New(Config{AuthDir: dir})

	body := doExport(t, h, "?file=workbuddy-u1.json&include_tokens=true")
	if n := body["count"].(float64); n != 1 {
		t.Fatalf("count=%v want 1", n)
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "AT2") {
		t.Error("只导 u1，不该含 u2 的 token")
	}
}

// TestExportRejectsBadFilename 非法文件名拒绝（防路径穿越）。
func TestExportRejectsBadFilename(t *testing.T) {
	dir := t.TempDir()
	writeFullAuth(t, dir, "u1", "AT1", "RT1")
	h := New(Config{AuthDir: dir})

	for _, bad := range []string{"../config.json", "..\\config.json", "/etc/passwd", "workbuddy-x.txt"} {
		req := httptest.NewRequest(http.MethodGet, "/__admin/accounts/export?file="+bad, nil)
		req.RemoteAddr = "127.0.0.1:12345"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Errorf("file=%q 应被拒绝，却返回 200", bad)
		}
	}
}

// TestExportImportRoundTrip 导出 → 导入 round-trip：
// 导出格式与 auths/ 文件同构，故可直接导回（含 token 时账号可立即使用）。
func TestExportImportRoundTrip(t *testing.T) {
	src := t.TempDir()
	writeFullAuth(t, src, "u1", "AT_RT_1", "RT_RT_1")
	writeFullAuth(t, src, "u2", "AT_RT_2", "RT_RT_2")
	hSrc := New(Config{AuthDir: src})
	body := doExport(t, hSrc, "?include_tokens=true")
	blob, _ := json.Marshal(body)

	// 导入到全新目录
	dst := t.TempDir()
	hDst := New(Config{AuthDir: dst, ReloadAccounts: func() (int, error) { return 2, nil }})
	req := httptest.NewRequest(http.MethodPost, "/__admin/accounts/import",
		strings.NewReader(`{"json":`+jsonStr(blob)+`}`))
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	hDst.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("批量导入失败 code=%d body=%s", rec.Code, rec.Body)
	}
	var res map[string]any
	json.Unmarshal(rec.Body.Bytes(), &res)
	if n := len(res["imported"].([]any)); n != 2 {
		t.Errorf("imported=%d want 2（failed=%v）", n, res["failed"])
	}
	// 落盘文件必须与原文件同构且 token 保留
	for _, uid := range []string{"u1", "u2"} {
		raw, err := os.ReadFile(filepath.Join(dst, "workbuddy-"+uid+".json"))
		if err != nil {
			t.Fatalf("导入后文件缺失 %s: %v", uid, err)
		}
		if !strings.Contains(string(raw), "AT_RT_"+uid[1:]) {
			t.Errorf("%s 的 accessToken 未保留: %s", uid, raw)
		}
		// 导出的 file 元数据不该被写进凭证
		if strings.Contains(string(raw), `"file"`) {
			t.Errorf("%s 落盘文件混入了导出元数据 file 键: %s", uid, raw)
		}
	}
}

// TestImportBatchPartialFailure 批量导入部分失败不中断：坏条目进 failed，
// 好条目照常落盘。
func TestImportBatchPartialFailure(t *testing.T) {
	dst := t.TempDir()
	h := New(Config{AuthDir: dst, ReloadAccounts: func() (int, error) { return 1, nil }})
	blob := `{"accounts":[
		{"file":"workbuddy-good.json","auth":{"accessToken":"AT_GOOD","expiresAt":9999999999},"account":{"uid":"good","nickname":"g"}},
		{"file":"bad.json","auth":{"refreshToken":"no-at"},"account":{"uid":"bad"}},
		{"account":{"uid":"noauth"}}
	]}`
	req := httptest.NewRequest(http.MethodPost, "/__admin/accounts/import",
		strings.NewReader(`{"json":`+jsonStr([]byte(blob))+`}`))
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var res map[string]any
	json.Unmarshal(rec.Body.Bytes(), &res)
	if n := len(res["imported"].([]any)); n != 1 {
		t.Errorf("imported=%d want 1", n)
	}
	if n := len(res["failed"].([]any)); n != 2 {
		t.Errorf("failed=%d want 2", n)
	}
	if _, err := os.Stat(filepath.Join(dst, "workbuddy-good.json")); err != nil {
		t.Errorf("好条目未落盘: %v", err)
	}
}

// jsonStr 把 JSON blob 包成 JSON 字符串字面量（用于构造 {"json":"..."} 请求体）。
func jsonStr(b []byte) string {
	s, _ := json.Marshal(string(b))
	return string(s)
}
