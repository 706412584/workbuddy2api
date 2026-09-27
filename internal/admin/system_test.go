package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/updater"
	"workbuddy2api/internal/version"
)

// newSystemHandler 造一个只接线更新功能的 admin handler。
// 走 ServeHTTP（本机请求无条件放行），因此不需要口令。
func newSystemHandler(cfg Config) *Handler { return New(cfg) }

func getJSON(t *testing.T, h *Handler, path string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func postJSON(t *testing.T, h *Handler, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// TestSystemVersion 版本端点必须如实报出编译期注入的值与自更新能力。
func TestSystemVersion(t *testing.T) {
	oldV, oldC := version.Version, version.Commit
	version.Version, version.Commit = "v1.2.3", "abc1234"
	defer func() { version.Version, version.Commit = oldV, oldC }()

	h := newSystemHandler(Config{RequestRestart: func(string) {}})
	code, body := getJSON(t, h, "/__admin/system/version")
	if code != http.StatusOK {
		t.Fatalf("status=%d", code)
	}
	if body["version"] != "v1.2.3" || body["commit"] != "abc1234" {
		t.Errorf("版本信息不对：%v", body)
	}
	if body["dev_build"] != false {
		t.Errorf("v1.2.3 不应标记 dev_build：%v", body)
	}
	if body["can_self_update"] != true {
		t.Errorf("接线了 RequestRestart 时 can_self_update 应为 true：%v", body)
	}
}

// TestSystemVersionDevBuild 未经发布流程构建的版本必须被标为开发版。
func TestSystemVersionDevBuild(t *testing.T) {
	old := version.Version
	version.Version = "dev"
	defer func() { version.Version = old }()

	h := newSystemHandler(Config{})
	_, body := getJSON(t, h, "/__admin/system/version")
	if body["dev_build"] != true {
		t.Errorf("dev 应标记 dev_build：%v", body)
	}
	if body["can_self_update"] != false {
		t.Errorf("未接线 RequestRestart 时 can_self_update 应为 false：%v", body)
	}
}

// TestSystemCheckUpdatesNotWired 未接线更新器时必须明确报错，而不是静默返回空。
func TestSystemCheckUpdatesNotWired(t *testing.T) {
	h := newSystemHandler(Config{})
	code, body := getJSON(t, h, "/__admin/system/check-updates")
	if code != http.StatusNotImplemented {
		t.Fatalf("status=%d want 501: %v", code, body)
	}
	if body["error"] == nil {
		t.Error("应返回 error 字段")
	}
}

// TestSystemCheckUpdatesUpstreamError 上游（GitHub）出错时回 502，不是 500 ——
// 错误来自上游而非本进程，面板据此给出不同的排查提示。
func TestSystemCheckUpdatesUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	h := newSystemHandler(Config{Updater: &updater.Client{Repo: "o/r", APIBase: srv.URL}})
	code, body := getJSON(t, h, "/__admin/system/check-updates?force=1")
	if code != http.StatusBadGateway {
		t.Fatalf("status=%d want 502: %v", code, body)
	}
}

// TestSystemUpdateNotWired 未接线时明确拒绝。
func TestSystemUpdateNotWired(t *testing.T) {
	h := newSystemHandler(Config{})
	code, _ := postJSON(t, h, "/__admin/system/update", "{}")
	if code != http.StatusNotImplemented {
		t.Fatalf("status=%d want 501", code)
	}
}

// TestSystemRestartNotWired 无交接能力时重启接口必须拒绝，并提示手工重启 ——
// 不能假装重启成功（文件换了但进程还是旧的，是更难排查的情形）。
func TestSystemRestartNotWired(t *testing.T) {
	h := newSystemHandler(Config{})
	code, body := postJSON(t, h, "/__admin/system/restart", "{}")
	if code != http.StatusNotImplemented {
		t.Fatalf("status=%d want 501: %v", code, body)
	}
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "手工重启") {
		t.Errorf("应提示手工重启：%q", msg)
	}
}

// TestSystemRestartTriggersHandoff 接线后重启接口必须投递交接请求，并如实回 restarting。
func TestSystemRestartTriggersHandoff(t *testing.T) {
	var got string
	h := newSystemHandler(Config{RequestRestart: func(reason string) { got = reason }})
	code, body := postJSON(t, h, "/__admin/system/restart", "{}")
	if code != http.StatusOK {
		t.Fatalf("status=%d: %v", code, body)
	}
	if body["restarting"] != true {
		t.Errorf("restarting 应为 true：%v", body)
	}
	if got == "" {
		t.Error("未投递交接请求")
	}
}

// TestSystemUpdateConflictVersionMismatch 指定了与最新版不符的版本时必须拒绝，
// 避免"以为装了 A 实际装了 B"。
func TestSystemUpdateConflictVersionMismatch(t *testing.T) {
	srv := newFakeReleaseServer(t, "v9.9.9", false)
	defer srv.Close()

	old := version.Version
	version.Version = "v1.0.0"
	defer func() { version.Version = old }()

	h := newSystemHandler(Config{
		Updater:        &updater.Client{Repo: "o/r", APIBase: srv.URL, GOOS: "windows", GOARCH: "amd64"},
		RequestRestart: func(string) {},
	})
	code, body := postJSON(t, h, "/__admin/system/update", `{"version":"v8.8.8"}`)
	if code != http.StatusConflict {
		t.Fatalf("status=%d want 409: %v", code, body)
	}
}

// TestSystemUpdateAlreadyLatest 已是最新时如实回报，不应触发任何安装动作。
func TestSystemUpdateAlreadyLatest(t *testing.T) {
	srv := newFakeReleaseServer(t, "v1.0.0", true)
	defer srv.Close()

	old := version.Version
	version.Version = "v1.0.0"
	defer func() { version.Version = old }()

	restarted := false
	h := newSystemHandler(Config{
		Updater:        &updater.Client{Repo: "o/r", APIBase: srv.URL, GOOS: "windows", GOARCH: "amd64"},
		RequestRestart: func(string) { restarted = true },
	})
	code, body := postJSON(t, h, "/__admin/system/update", "{}")
	if code != http.StatusOK {
		t.Fatalf("status=%d: %v", code, body)
	}
	if restarted {
		t.Error("已是最新时不应触发重启")
	}
	if !strings.Contains(body["msg"].(string), "已是最新") {
		t.Errorf("msg=%v", body["msg"])
	}
}

// TestSystemUpdateDevBuildRejected 开发版不参与版本比较，更新接口必须明确拒绝，
// 而不是含糊地报"已是最新"。
func TestSystemUpdateDevBuildRejected(t *testing.T) {
	srv := newFakeReleaseServer(t, "v9.9.9", true)
	defer srv.Close()

	old := version.Version
	version.Version = "dev"
	defer func() { version.Version = old }()

	h := newSystemHandler(Config{
		Updater:        &updater.Client{Repo: "o/r", APIBase: srv.URL, GOOS: "windows", GOARCH: "amd64"},
		RequestRestart: func(string) {},
	})
	code, body := postJSON(t, h, "/__admin/system/update", "{}")
	if code != http.StatusConflict {
		t.Fatalf("status=%d want 409: %v", code, body)
	}
	if !strings.Contains(body["error"].(string), "开发版") {
		t.Errorf("应说明是开发版：%v", body["error"])
	}
}

// TestSystemUpdateNoPlatformAsset 有新版但无本平台包时，必须回冲突并给出 Release 链接，
// 让用户能手动下载 —— 而不是报一个含糊的内部错误。
func TestSystemUpdateNoPlatformAsset(t *testing.T) {
	srv := newFakeReleaseServer(t, "v9.9.9", false)
	defer srv.Close()

	old := version.Version
	version.Version = "v1.0.0"
	defer func() { version.Version = old }()

	h := newSystemHandler(Config{
		Updater:        &updater.Client{Repo: "o/r", APIBase: srv.URL, GOOS: "windows", GOARCH: "amd64"},
		RequestRestart: func(string) {},
	})
	code, body := postJSON(t, h, "/__admin/system/update", "{}")
	if code != http.StatusConflict {
		t.Fatalf("status=%d want 409: %v", code, body)
	}
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "手动下载") {
		t.Errorf("应提示手动下载：%q", msg)
	}
}

// newFakeReleaseServer 起一个只回 releases/latest 的假 GitHub。
// withAsset 控制是否带 windows-amd64 附件。
func newFakeReleaseServer(t *testing.T, tag string, withAsset bool) *httptest.Server {
	t.Helper()
	rel := map[string]any{"tag_name": tag, "body": "notes"}
	if withAsset {
		rel["assets"] = []map[string]any{
			{"name": "wb2api-" + tag + "-windows-amd64.zip", "browser_download_url": "https://github.com/x", "size": 100},
		}
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(rel)
	}))
}
