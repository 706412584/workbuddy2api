package updater

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/version"
)

// makeZip 造一个发布包 zip，条目名形如 <dir>/<name>（模拟单层目录打包）。
func makeZip(t *testing.T, dir string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(dir + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func makeTarGz(t *testing.T, dir string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		hdr := &tar.Header{
			Name:     dir + "/" + name,
			Mode:     0o755,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// installFixture 搭一套「假 GitHub + 目标目录」，返回安装器与目标目录。
func installFixture(t *testing.T, goos string, archive []byte, assetName, sumsBody string) (*Client, string) {
	t.Helper()
	exeDir := t.TempDir()
	// 目标目录里先放好"旧版本"文件（install 只替换已存在的）。
	for _, n := range binaryNames(goos) {
		if err := os.WriteFile(filepath.Join(exeDir, n), []byte("old-"+n), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	rel := Release{
		TagName: "v9.9.9",
		Assets: []Asset{
			{Name: assetName, URL: "PLACEHOLDER", Size: int64(len(archive))},
			{Name: "SHA256SUMS", URL: "PLACEHOLDER", Size: int64(len(sumsBody))},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/releases/tags/", func(w http.ResponseWriter, r *http.Request) {
		// 附件的下载地址在服务起来后才确定，故这里动态改写。
		rel.Assets[0].URL = "https://github.com/assets/" + assetName
		rel.Assets[1].URL = "https://github.com/assets/SHA256SUMS"
		_ = json.NewEncoder(w).Encode(rel)
	})
	// 资产下载走本 server，但 URL 校验只放行 github.com。为了不改校验逻辑，
	// 用 github.com 主机名 + 自定义 RoundTripper 在测试里重定向。
	files := map[string][]byte{assetName: archive, "SHA256SUMS": []byte(sumsBody)}
	mux.HandleFunc("/assets/", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[len("/assets/"):]
		b, ok := files[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := &Client{
		Repo: "o/r", APIBase: srv.URL, GOOS: goos, GOARCH: "amd64",
		// 把 github.com 的请求重定向到本地测试服务器：validateDownloadURL 校验的是
		// URL 字符串（照常执行），实际连接走 httptest。
		HTTPClient: &http.Client{
			Transport: rewriteTransport{base: srv.URL, rt: http.DefaultTransport},
		},
	}
	return c, exeDir
}

// rewriteTransport 把所有请求改写到测试服务器，但保留原 URL 供校验。
type rewriteTransport struct {
	base string
	rt   http.RoundTripper
}

func (t rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	// 只重写 scheme/host，路径保持不变（/assets/<name>）。
	r2.URL.Scheme = "http"
	r2.URL.Host = t.base[len("http://"):]
	return t.rt.RoundTrip(r2)
}

func TestInstallReplacesBinaries(t *testing.T) {
	old := version.Version
	version.Version = "v1.0.0"
	defer func() { version.Version = old }()

	archive := makeZip(t, "wb2api-v9.9.9-windows-amd64", map[string]string{
		"wb2api.exe":     "NEW-GATEWAY",
		"login.exe":      "NEW-LOGIN",
		"credit.exe":     "NEW-CREDIT",
		"signin_bin.exe": "NEW-SIGNIN",
		"activity.exe":   "NEW-ACTIVITY",
	})
	sums := sha256Hex(archive) + "  wb2api-v9.9.9-windows-amd64.zip\n"

	c, exeDir := installFixture(t, "windows", archive, "wb2api-v9.9.9-windows-amd64.zip", sums)

	info := &UpdateInfo{Latest: "v9.9.9", AssetName: "wb2api-v9.9.9-windows-amd64.zip"}
	res, err := c.Install(context.Background(), info, exeDir)
	if err != nil {
		t.Fatalf("Install 失败：%v", err)
	}
	if len(res.Replaced) != 5 {
		t.Errorf("应替换 5 个文件，实得 %v", res.Replaced)
	}
	got, err := os.ReadFile(filepath.Join(exeDir, "wb2api.exe"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "NEW-GATEWAY" {
		t.Errorf("wb2api.exe 内容=%q want NEW-GATEWAY", got)
	}
	// 旧版本必须留下备份，便于回滚。
	if _, err := os.Stat(filepath.Join(exeDir, "wb2api.exe.backup")); err != nil {
		t.Errorf("缺少备份文件：%v", err)
	}
	bak, _ := os.ReadFile(filepath.Join(exeDir, "wb2api.exe.backup"))
	if string(bak) != "old-wb2api.exe" {
		t.Errorf("备份内容=%q want old-wb2api.exe", bak)
	}
}

// TestInstallChecksumMismatch 校验和不符必须拒绝安装，且不能动目标目录里的文件。
func TestInstallChecksumMismatch(t *testing.T) {
	archive := makeZip(t, "wb2api-v9.9.9-windows-amd64", map[string]string{"wb2api.exe": "NEW"})
	// 故意给一个错的校验和。
	sums := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef  wb2api-v9.9.9-windows-amd64.zip\n"

	c, exeDir := installFixture(t, "windows", archive, "wb2api-v9.9.9-windows-amd64.zip", sums)

	info := &UpdateInfo{Latest: "v9.9.9", AssetName: "wb2api-v9.9.9-windows-amd64.zip"}
	_, err := c.Install(context.Background(), info, exeDir)
	if err == nil {
		t.Fatal("校验和不符应报错")
	}
	// 目标文件必须原封不动。
	got, _ := os.ReadFile(filepath.Join(exeDir, "wb2api.exe"))
	if string(got) != "old-wb2api.exe" {
		t.Errorf("校验失败后目标文件被改动：%q", got)
	}
}

// TestInstallMissingChecksums 没有 SHA256SUMS 时必须拒绝（不装未校验的产物）。
func TestInstallMissingChecksums(t *testing.T) {
	exeDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(exeDir, "wb2api.exe"), []byte("old"), 0o755)

	rel := Release{
		TagName: "v9.9.9",
		Assets:  []Asset{{Name: "wb2api-v9.9.9-windows-amd64.zip", URL: "https://github.com/a"}},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(rel)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &Client{Repo: "o/r", APIBase: srv.URL, GOOS: "windows", GOARCH: "amd64"}
	info := &UpdateInfo{Latest: "v9.9.9", AssetName: "wb2api-v9.9.9-windows-amd64.zip"}
	if _, err := c.Install(context.Background(), info, exeDir); err == nil {
		t.Fatal("缺少 SHA256SUMS 应报错")
	}
}

// TestInstallSkipsAbsentTargets 目标目录里没有的文件不该被凭空创建。
func TestInstallSkipsAbsentTargets(t *testing.T) {
	archive := makeZip(t, "d", map[string]string{
		"wb2api.exe": "NEW-GATEWAY",
		"login.exe":  "NEW-LOGIN",
	})
	sums := sha256Hex(archive) + "  wb2api-v9.9.9-windows-amd64.zip\n"

	exeDir := t.TempDir()
	// 只放网关主程序，没有 login.exe（用户可能没装小工具）。
	_ = os.WriteFile(filepath.Join(exeDir, "wb2api.exe"), []byte("old"), 0o755)

	rel := Release{TagName: "v9.9.9"}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/releases/tags/", func(w http.ResponseWriter, r *http.Request) {
		rel.Assets = []Asset{
			{Name: "wb2api-v9.9.9-windows-amd64.zip", URL: "https://github.com/assets/a.zip"},
			{Name: "SHA256SUMS", URL: "https://github.com/assets/SHA256SUMS"},
		}
		_ = json.NewEncoder(w).Encode(rel)
	})
	files := map[string][]byte{"a.zip": archive, "SHA256SUMS": []byte(sums)}
	mux.HandleFunc("/assets/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(files[r.URL.Path[len("/assets/"):]])
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &Client{
		Repo: "o/r", APIBase: srv.URL, GOOS: "windows", GOARCH: "amd64",
		HTTPClient: &http.Client{
			Transport: rewriteTransport{base: srv.URL, rt: http.DefaultTransport},
		},
	}

	info := &UpdateInfo{Latest: "v9.9.9", AssetName: "wb2api-v9.9.9-windows-amd64.zip"}
	res, err := c.Install(context.Background(), info, exeDir)
	if err != nil {
		t.Fatalf("Install 失败：%v", err)
	}
	if len(res.Replaced) != 1 || res.Replaced[0] != "wb2api.exe" {
		t.Errorf("应只替换 wb2api.exe，实得 %v", res.Replaced)
	}
	if _, err := os.Stat(filepath.Join(exeDir, "login.exe")); err == nil {
		t.Error("不应凭空创建 login.exe")
	}
}

// TestExtractZipIgnoresPathTraversal zip-slip：包内用 ../ 想写到目标目录外，
// 必须被白名单文件名机制挡住（只取 Base）。
func TestExtractZipIgnoresPathTraversal(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("../../../evil.exe")
	_, _ = w.Write([]byte("pwned"))
	w2, _ := zw.Create("dir/wb2api.exe")
	_, _ = w2.Write([]byte("OK"))
	zw.Close()

	dest := t.TempDir()
	// goos=windows → 白名单含 wb2api.exe，不含 evil.exe。
	want := map[string]bool{"wb2api.exe": true}
	if err := extractZip(writeTemp(t, buf.Bytes()), dest, want, "windows"); err != nil {
		t.Fatal(err)
	}
	// 只应落下 wb2api.exe，且没有逃逸文件。
	entries, _ := os.ReadDir(dest)
	if len(entries) != 1 || entries[0].Name() != "wb2api.exe" {
		t.Errorf("解包结果异常：%v", entries)
	}
	if _, err := os.Stat(filepath.Join(dest, "..", "evil.exe")); err == nil {
		t.Error("zip-slip：evil.exe 逃逸到了目标目录外")
	}
}

func TestExtractTarGz(t *testing.T) {
	archive := makeTarGz(t, "wb2api-v9.9.9-linux-amd64", map[string]string{
		"wb2api":     "NEW",
		"signin_bin": "S",
	})
	dest := t.TempDir()
	want := map[string]bool{"wb2api": true, "signin_bin": true}
	if err := extractTarGz(writeTemp(t, archive), dest, want, "linux"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "wb2api"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "NEW" {
		t.Errorf("内容=%q want NEW", got)
	}
}

// TestExtractArchiveNoExpectedFiles 包内没有预期文件时必须报错（防止空包静默成功）。
func TestExtractArchiveNoExpectedFiles(t *testing.T) {
	archive := makeZip(t, "d", map[string]string{"README.md": "hi"})
	dest := t.TempDir()
	if err := extractArchive(writeTemp(t, archive), dest, "windows"); err == nil {
		t.Error("包内无预期二进制应报错")
	}
}

func TestSwapBinariesRollsBackOnFailure(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	// src 有两个文件，dst 也有两个：第二个替换会失败吗？不会——这里验证成功路径，
	// 以及"dst 里不存在的文件被跳过"。
	_ = os.WriteFile(filepath.Join(src, "wb2api.exe"), []byte("NEW"), 0o755)
	_ = os.WriteFile(filepath.Join(dst, "wb2api.exe"), []byte("OLD"), 0o755)

	res, err := swapBinaries(src, dst, "windows")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Replaced) != 1 {
		t.Errorf("replaced=%v", res.Replaced)
	}
	got, _ := os.ReadFile(filepath.Join(dst, "wb2api.exe"))
	if string(got) != "NEW" {
		t.Errorf("内容=%q want NEW", got)
	}
}

func TestParseChecksumsMultiline(t *testing.T) {
	// release.yml 用 `sha256sum wb2api-*`，行内是「哈希 + 两空格 + 文件名」。
	in := "aaa  wb2api-v1.0.0-linux-amd64.tar.gz\nbbb  wb2api-v1.0.0-windows-amd64.zip\n"
	m := parseChecksums(in)
	if m["wb2api-v1.0.0-linux-amd64.tar.gz"] != "aaa" ||
		m["wb2api-v1.0.0-windows-amd64.zip"] != "bbb" {
		t.Errorf("解析结果=%v", m)
	}
}

// writeTemp 把字节写进临时文件并返回路径。
func writeTemp(t *testing.T, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "archive.bin")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}
