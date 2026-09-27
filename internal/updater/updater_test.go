package updater

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/version"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v1.2.3", "v1.2.3", 0},
		{"1.2.3", "v1.2.3", 0}, // v 前缀不参与比较
		{"v1.2.4", "v1.2.3", 1},
		{"v1.2.3", "v1.2.4", -1},
		{"v1.3.0", "v1.2.9", 1},
		{"v2.0.0", "v1.99.99", 1},
		{"v1.2", "v1.2.0", 0},  // 缺位补 0
		{"v1.2.0", "v1.2", 0},
		{"v1.10.0", "v1.9.0", 1}, // 数字段按数值而非字典序
		// semver 预发布：有预发布段 < 无预发布段
		{"v1.2.3-rc.1", "v1.2.3", -1},
		{"v1.2.3", "v1.2.3-rc.1", 1},
		{"v1.2.3-rc.2", "v1.2.3-rc.1", 1},
		{"v1.2.3-rc.10", "v1.2.3-rc.9", 1}, // 预发布段里的数字也按数值
		{"v1.2.3-alpha", "v1.2.3-beta", -1},
		{"v1.2.3-rc.1", "v1.2.3-rc.1.1", -1}, // 缺位视为更小
		{"v1.2.3+build.1", "v1.2.3", 0},       // build 元数据不参与排序
		// 开发版：主体 0，永远小于任何真实版本
		{"v1.0.0", "dev-abc1234", 1},
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
		// 反对称性：反过来必须取反。
		if got := CompareVersions(c.b, c.a); got != -c.want {
			t.Errorf("CompareVersions(%q,%q)=%d want %d (antisymmetry)", c.b, c.a, got, -c.want)
		}
	}
}

func TestSelectAsset(t *testing.T) {
	assets := []Asset{
		{Name: "wb2api-v1.2.3-linux-amd64.tar.gz"},
		{Name: "wb2api-v1.2.3-linux-arm64.tar.gz"},
		{Name: "wb2api-v1.2.3-windows-amd64.zip"},
		{Name: "SHA256SUMS"},
	}
	cases := []struct {
		goos, goarch, want string
	}{
		{"windows", "amd64", "wb2api-v1.2.3-windows-amd64.zip"},
		{"linux", "amd64", "wb2api-v1.2.3-linux-amd64.tar.gz"},
		{"linux", "arm64", "wb2api-v1.2.3-linux-arm64.tar.gz"},
		{"darwin", "arm64", ""}, // 未发布平台
	}
	for _, c := range cases {
		got := selectAsset(assets, c.goos, c.goarch)
		if c.want == "" {
			if got != nil {
				t.Errorf("selectAsset(%s/%s)=%q want nil", c.goos, c.goarch, got.Name)
			}
			continue
		}
		if got == nil {
			t.Fatalf("selectAsset(%s/%s)=nil want %q", c.goos, c.goarch, c.want)
		}
		if got.Name != c.want {
			t.Errorf("selectAsset(%s/%s)=%q want %q", c.goos, c.goarch, got.Name, c.want)
		}
	}
}

// TestSelectAssetDevVersion 版本号里含 '-'（dev-abc1234）时后缀匹配仍须正确。
// 这是刻意用后缀而非按 '-' 切分解析的原因。
func TestSelectAssetDevVersion(t *testing.T) {
	assets := []Asset{{Name: "wb2api-dev-abc1234-windows-amd64.zip"}}
	got := selectAsset(assets, "windows", "amd64")
	if got == nil {
		t.Fatal("dev 版本号未被正确匹配")
	}
}

func TestParseChecksums(t *testing.T) {
	in := "abc123  wb2api-v1.0.0-linux-amd64.tar.gz\n" +
		"def456 *wb2api-v1.0.0-windows-amd64.zip\n" + // 二进制模式带 *
		"ghi789  ./wb2api-v1.0.0-linux-arm64.tar.gz\n" + // 带 ./ 前缀
		"\n" +
		"malformed-line-without-two-fields\n"
	got := parseChecksums(in)
	want := map[string]string{
		"wb2api-v1.0.0-linux-amd64.tar.gz":  "abc123",
		"wb2api-v1.0.0-windows-amd64.zip":   "def456",
		"wb2api-v1.0.0-linux-arm64.tar.gz":  "ghi789",
	}
	if len(got) != len(want) {
		t.Fatalf("parseChecksums 条目数=%d want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("parseChecksums[%q]=%q want %q", k, got[k], v)
		}
	}
}

func TestValidateDownloadURL(t *testing.T) {
	ok := []string{
		"https://github.com/706412584/workbuddy2api/releases/download/v1/x.zip",
		"https://objects.githubusercontent.com/foo",
		"https://release-assets.githubusercontent.com/bar",
	}
	for _, u := range ok {
		if err := validateDownloadURL(u); err != nil {
			t.Errorf("validateDownloadURL(%q) 应通过，实得 %v", u, err)
		}
	}
	bad := []string{
		"http://github.com/x.zip",          // 非 HTTPS
		"https://evil.com/x.zip",           // 非白名单主机
		"https://github.com.evil.com/x.zip", // 后缀伪装
		"ftp://github.com/x.zip",
		"://nonsense",
	}
	for _, u := range bad {
		if err := validateDownloadURL(u); err == nil {
			t.Errorf("validateDownloadURL(%q) 应被拒绝", u)
		}
	}
}

// newFakeGitHub 起一个假的 GitHub API + 资产服务器。
func newFakeGitHub(t *testing.T, rel Release, files map[string][]byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			http.Error(w, "no UA", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(rel)
	})
	mux.HandleFunc("/repos/o/r/releases/tags/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(rel)
	})
	mux.HandleFunc("/assets/", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[len("/assets/"):]
		b, ok := files[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	})
	return httptest.NewServer(mux)
}

func TestCheckUpdateAvailable(t *testing.T) {
	srv := newFakeGitHub(t, Release{
		TagName:     "v9.9.9",
		PublishedAt: time.Now(),
		Body:        "notes",
		Assets: []Asset{
			{Name: "wb2api-v9.9.9-windows-amd64.zip", URL: "https://github.com/x", Size: 123},
			{Name: "SHA256SUMS", URL: "https://github.com/y", Size: 10},
		},
	}, nil)
	defer srv.Close()

	old := version.Version
	version.Version = "v1.0.0"
	defer func() { version.Version = old }()

	c := &Client{Repo: "o/r", APIBase: srv.URL, GOOS: "windows", GOARCH: "amd64"}
	info, err := c.CheckUpdate(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Available {
		t.Errorf("应有更新：current=%s latest=%s", info.Current, info.Latest)
	}
	if !info.Downloadable || info.AssetName != "wb2api-v9.9.9-windows-amd64.zip" {
		t.Errorf("资产选择错误：%+v", info)
	}
	if info.Cached {
		t.Error("首次检查不应标记为缓存")
	}

	// 第二次不带 force：应命中缓存。
	info2, err := c.CheckUpdate(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !info2.Cached {
		t.Error("20 分钟内的第二次检查应走缓存")
	}
	// force 必须绕过缓存。
	info3, err := c.CheckUpdate(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if info3.Cached {
		t.Error("force=true 不应走缓存")
	}
}

// TestCheckUpdateDevBuild 开发版不参与版本比较：Available 恒 false，
// 但 DevBuild 为真，让面板能提示"开发版"而不是撒谎说"已是最新"。
func TestCheckUpdateDevBuild(t *testing.T) {
	srv := newFakeGitHub(t, Release{TagName: "v9.9.9"}, nil)
	defer srv.Close()

	old := version.Version
	version.Version = "dev"
	defer func() { version.Version = old }()

	c := &Client{Repo: "o/r", APIBase: srv.URL, GOOS: "linux", GOARCH: "amd64"}
	info, err := c.CheckUpdate(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if info.Available {
		t.Error("开发版不应报告有更新")
	}
	if !info.DevBuild {
		t.Error("应标记 DevBuild")
	}
	if info.Downloadable {
		t.Error("无更新时 Downloadable 应为 false")
	}
}

// TestCheckUpdateNoPlatformAsset 有新版但无本平台包：Available 真、Downloadable 假。
func TestCheckUpdateNoPlatformAsset(t *testing.T) {
	srv := newFakeGitHub(t, Release{
		TagName: "v9.9.9",
		Assets:  []Asset{{Name: "wb2api-v9.9.9-linux-amd64.tar.gz"}},
	}, nil)
	defer srv.Close()

	old := version.Version
	version.Version = "v1.0.0"
	defer func() { version.Version = old }()

	c := &Client{Repo: "o/r", APIBase: srv.URL, GOOS: "windows", GOARCH: "amd64"}
	info, err := c.CheckUpdate(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Available {
		t.Error("有新版应 Available")
	}
	if info.Downloadable {
		t.Error("无 windows 包时 Downloadable 应为 false")
	}
}

func TestCheckUpdateNotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &Client{Repo: "o/r", APIBase: srv.URL, GOOS: "linux", GOARCH: "amd64"}
	if _, err := c.CheckUpdate(context.Background(), true); err == nil {
		t.Error("404 应报错")
	}
}

func TestCheckUpdateRateLimited(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusForbidden)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &Client{Repo: "o/r", APIBase: srv.URL, GOOS: "linux", GOARCH: "amd64"}
	_, err := c.CheckUpdate(context.Background(), true)
	if err == nil {
		t.Fatal("403 应报错")
	}
	// 报错要能提示"限流"，否则用户不知道该怎么办。
	if !strings.Contains(err.Error(), "限流") {
		t.Errorf("限流错误提示不明确：%v", err)
	}
}

func TestArchiveExt(t *testing.T) {
	if archiveExt("windows") != "zip" {
		t.Error("windows 应为 zip")
	}
	if archiveExt("linux") != "tar.gz" {
		t.Error("linux 应为 tar.gz")
	}
}

func TestBinaryNames(t *testing.T) {
	w := binaryNames("windows")
	if len(w) != 5 || w[0] != "wb2api.exe" || w[3] != "signin_bin.exe" {
		t.Errorf("windows 二进制名不对：%v", w)
	}
	l := binaryNames("linux")
	if l[0] != "wb2api" || l[3] != "signin_bin" {
		t.Errorf("linux 二进制名不对：%v", l)
	}
}
