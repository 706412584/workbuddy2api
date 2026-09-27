// install.go 下载并安装一次发布包：下载 → 校验 SHA256 → 解包 → 原子替换。
//
// 替换策略（Windows 实测约束，见 cmd/server 的交接实现注释）：
//   - 运行中的 exe **可以**被重命名（加载器以 FILE_SHARE_DELETE 打开映像），
//     但**不可以**被删除（"Access is denied"）。故替换走「改名让路 + 落新文件」，
//     而不是「先删再写」。
//   - 五个二进制是一套，全部替换或全部不动：中途失败会回滚已替换的，
//     避免出现「新 wb2api 配旧 login」这种版本分叉。
package updater

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"workbuddy2api/internal/version"
)

// maxDownloadSize 单个下载上限。发布包实测 36MB，留足余量但挡住异常响应。
const maxDownloadSize = 500 << 20

// downloadTimeout 发布包下载总时长上限。
const downloadTimeout = 10 * time.Minute

// InstallResult 一次安装的结果，直接作为接口响应体。
type InstallResult struct {
	// Version 安装到的版本。
	Version string `json:"version"`
	// Asset 使用的发布包文件名。
	Asset string `json:"asset"`
	// ExeDir 被替换二进制所在目录。
	ExeDir string `json:"exe_dir"`
	// Replaced 实际替换的文件名列表。
	Replaced []string `json:"replaced"`
	// Skipped 包内不存在于目标目录、因而跳过的文件（仅报告，不算失败）。
	Skipped []string `json:"skipped"`
	// Backups 备份文件名（"原名.backup"），失败时人工回滚用。
	Backups []string `json:"backups"`
}

// binaryNames 发布包内的二进制名（按 GOOS 加扩展名）。
// 与 .github/workflows/release.yml 的 Build binaries 步骤逐字对应。
func binaryNames(goos string) []string {
	ext := ""
	if goos == "windows" {
		ext = ".exe"
	}
	return []string{
		"wb2api" + ext,
		"login" + ext,
		"credit" + ext,
		"signin_bin" + ext,
		"activity" + ext,
	}
}

// Install 下载并安装 rel 中与本平台匹配的发布包，替换 exeDir 下的同名二进制。
//
// exeDir 为空的约定见 InstallToExeDir：由调用方传入当前可执行文件所在目录。
// 返回前不会删除 exeDir 下的任何文件；旧版本一律改名为 .backup 保留，便于回滚。
func (c *Client) Install(ctx context.Context, info *UpdateInfo, exeDir string) (*InstallResult, error) {
	if info == nil || info.AssetName == "" {
		return nil, fmt.Errorf("没有与本平台匹配的发布包")
	}
	if exeDir == "" {
		return nil, fmt.Errorf("未指定安装目录")
	}

	// 目录可写性预检：这一步能挡住「exe 装在 Program Files / 只读挂载」这类
	// 注定失败的场景，避免下了 36MB 才报错。
	if err := checkWritable(exeDir); err != nil {
		return nil, err
	}

	rel, err := c.fetchReleaseByTag(ctx, info.Latest)
	if err != nil {
		return nil, err
	}

	asset := findAsset(rel.Assets, info.AssetName)
	if asset == nil {
		return nil, fmt.Errorf("Release %s 中找不到附件 %s", info.Latest, info.AssetName)
	}
	sumsAsset := findAssetByName(rel.Assets, "SHA256SUMS")
	if sumsAsset == nil {
		return nil, fmt.Errorf("Release %s 缺少 SHA256SUMS，拒绝安装未校验的产物", info.Latest)
	}

	// 临时目录放在 exeDir 内：与目标同文件系统，rename 才是原子的（跨盘 rename 会失败）。
	tmpDir, err := os.MkdirTemp(exeDir, ".wb2api-update-*")
	if err != nil {
		return nil, fmt.Errorf("创建临时目录失败：%w", err)
	}
	defer os.RemoveAll(tmpDir)

	sums, err := c.downloadText(ctx, sumsAsset.URL)
	if err != nil {
		return nil, fmt.Errorf("下载校验和失败：%w", err)
	}
	wantSum, ok := parseChecksums(sums)[asset.Name]
	if !ok {
		return nil, fmt.Errorf("SHA256SUMS 中没有 %s 的记录", asset.Name)
	}

	archivePath := filepath.Join(tmpDir, asset.Name)
	if err := c.downloadFile(ctx, asset.URL, archivePath); err != nil {
		return nil, fmt.Errorf("下载发布包失败：%w", err)
	}
	gotSum, err := sha256File(archivePath)
	if err != nil {
		return nil, fmt.Errorf("计算校验和失败：%w", err)
	}
	if !strings.EqualFold(gotSum, wantSum) {
		return nil, fmt.Errorf("发布包校验和不匹配（期望 %s，实得 %s），已放弃安装", wantSum, gotSum)
	}

	extractDir := filepath.Join(tmpDir, "extract")
	if err := os.MkdirAll(extractDir, 0o755); err != nil {
		return nil, err
	}
	if err := extractArchive(archivePath, extractDir, c.goos()); err != nil {
		return nil, fmt.Errorf("解包失败：%w", err)
	}

	res, err := swapBinaries(extractDir, exeDir, c.goos())
	if err != nil {
		return nil, err
	}
	res.Version = info.Latest
	res.Asset = asset.Name
	res.ExeDir = exeDir
	return res, nil
}

// ExeDir 返回当前可执行文件所在目录（解析符号链接后的真实路径）。
// 安装目标必须是它：用户可能用 PATH 里的软链启动，替换软链指向的文件才有效。
func ExeDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("定位当前可执行文件失败：%w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe), nil
}

// fetchReleaseByTag 按 tag 拉取 Release（安装前重取一次，拿到附件的真实下载地址）。
func (c *Client) fetchReleaseByTag(ctx context.Context, tag string) (*Release, error) {
	apiURL := fmt.Sprintf("%s/repos/%s/releases/tags/%s", c.apiBase(), c.repo(), url.PathEscape(tag))
	reqCtx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", version.UserAgent())
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("查询 Release %s 失败：%w", tag, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("查询 Release %s 失败：HTTP %s", tag, resp.Status)
	}
	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("解析 Release 响应失败：%w", err)
	}
	return &rel, nil
}

// downloadFile 流式下载到 path，边下边算长度上限。
func (c *Client) downloadFile(ctx context.Context, rawURL, path string) error {
	if err := validateDownloadURL(rawURL); err != nil {
		return err
	}
	reqCtx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", version.UserAgent())
	if c.Token != "" {
		// 私有 fork 的 Release 资产同样需要凭证；对公开仓库无害。
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	if resp.ContentLength > maxDownloadSize {
		return fmt.Errorf("发布包过大（%d 字节，上限 %d）", resp.ContentLength, maxDownloadSize)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	// 用 LimitReader 兜住「Content-Length 撒谎/分块传输」的情形。
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxDownloadSize+1))
	if err != nil {
		return err
	}
	if n > maxDownloadSize {
		return fmt.Errorf("发布包超过 %d 字节上限", maxDownloadSize)
	}
	return f.Sync()
}

// downloadText 下载小文本（SHA256SUMS）。
func (c *Client) downloadText(ctx context.Context, rawURL string) (string, error) {
	if err := validateDownloadURL(rawURL); err != nil {
		return "", err
	}
	reqCtx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", version.UserAgent())
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %s", resp.Status)
	}
	// 校验和文件只有几百字节，1MB 上限绰绰有余。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// validateDownloadURL 只允许从 GitHub 下载，挡住「响应被篡改后指向任意主机」的路径。
// 与 sub2api 同一口径：HTTPS + github.com（含子域）/ objects.githubusercontent.com。
func validateDownloadURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("下载地址非法：%w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("下载地址必须为 HTTPS，实得 %q", u.Scheme)
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "github.com", strings.HasSuffix(host, ".github.com"):
	case host == "objects.githubusercontent.com", strings.HasSuffix(host, ".githubusercontent.com"):
	default:
		return fmt.Errorf("下载地址主机 %q 不在允许列表（github.com / githubusercontent.com）", host)
	}
	return nil
}

// parseChecksums 解析 SHA256SUMS（`<hex>  <name>` 每行一条）。
func parseChecksums(s string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*") // 二进制模式的 `*name`
		name = strings.TrimPrefix(name, "./")
		out[name] = fields[0]
	}
	return out
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// findAsset 在附件里按名字精确查找。
func findAsset(assets []Asset, name string) *Asset { return findAssetByName(assets, name) }

func findAssetByName(assets []Asset, name string) *Asset {
	for i := range assets {
		if assets[i].Name == name {
			return &assets[i]
		}
	}
	return nil
}

// extractArchive 解包 zip / tar.gz，只落 binaryNames 里列出的文件。
//
// 只认白名单文件名，天然免疫 zip-slip（路径穿越）：包内形如
// `wb2api-v1.0.0-windows-amd64/wb2api.exe` 的条目取 Base 后匹配，
// 目录条目一律忽略。
func extractArchive(archivePath, destDir, goos string) error {
	want := map[string]bool{}
	for _, n := range binaryNames(goos) {
		want[n] = true
	}
	if strings.HasSuffix(archivePath, ".zip") {
		return extractZip(archivePath, destDir, want, goos)
	}
	return extractTarGz(archivePath, destDir, want, goos)
}

func extractZip(archivePath, destDir string, want map[string]bool, goos string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer zr.Close()
	found := 0
	for _, f := range zr.File {
		name := filepath.Base(f.Name)
		if f.FileInfo().IsDir() || !want[name] {
			continue
		}
		if err := writeFromReader(filepath.Join(destDir, name), f.Open, f.UncompressedSize64); err != nil {
			return err
		}
		found++
	}
	if found == 0 {
		return fmt.Errorf("压缩包内没有预期的二进制（%v）", binaryNames(goos))
	}
	return nil
}

func extractTarGz(archivePath, destDir string, want map[string]bool, goos string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	found := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.Base(hdr.Name)
		if hdr.Typeflag != tar.TypeReg || !want[name] {
			continue
		}
		if err := writeFromReader(filepath.Join(destDir, name), func() (io.ReadCloser, error) {
			return io.NopCloser(tr), nil
		}, uint64(hdr.Size)); err != nil {
			return err
		}
		found++
	}
	if found == 0 {
		return fmt.Errorf("压缩包内没有预期的二进制（%v）", binaryNames(goos))
	}
	return nil
}

// writeFromReader 把 src 写进 path，可执行权限，大小超限即报错。
func writeFromReader(path string, open func() (io.ReadCloser, error), size uint64) error {
	if size > maxDownloadSize {
		return fmt.Errorf("%s 解包后过大（%d 字节）", filepath.Base(path), size)
	}
	rc, err := open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, io.LimitReader(rc, maxDownloadSize+1)); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// swapBinaries 把 srcDir 里的二进制原子替换到 dstDir 同名文件。
//
// 只有 dstDir 里**已存在**的同名文件才替换（Skipped 报告其余）：用户可能只用
// 网关主程序、没有安装四个小工具，凭空多出几个 exe 不是更新该做的事。
//
// 全部成功才算成功；任一失败即回滚已完成的替换，不留半新半旧的状态。
func swapBinaries(srcDir, dstDir, goos string) (*InstallResult, error) {
	res := &InstallResult{Replaced: []string{}, Skipped: []string{}, Backups: []string{}}
	type swap struct{ target, backup string }
	var done []swap

	rollback := func() {
		for i := len(done) - 1; i >= 0; i-- {
			s := done[i]
			_ = os.Remove(s.target)         // 移走半成品（新文件）
			_ = os.Rename(s.backup, s.target) // 旧文件复位
		}
	}

	for _, name := range binaryNames(goos) {
		src := filepath.Join(srcDir, name)
		if _, err := os.Stat(src); err != nil {
			continue // 包内没有（不该发生，解包已保证至少一个）
		}
		target := filepath.Join(dstDir, name)
		if _, err := os.Stat(target); err != nil {
			res.Skipped = append(res.Skipped, name)
			continue
		}
		backup := target + ".backup"
		// 旧备份先清掉：上一次更新留下的，不清会让 rename 失败（Windows 上
		// rename 不覆盖已存在的目标）。
		if err := os.Remove(backup); err != nil && !os.IsNotExist(err) {
			// 旧备份正被占用（如上次没重启完）——不致命，换一个带时间戳的名字。
			backup = fmt.Sprintf("%s.backup.%d", target, time.Now().Unix())
		}
		if err := os.Rename(target, backup); err != nil {
			rollback()
			return nil, fmt.Errorf("备份 %s 失败（文件被占用？）：%w", name, err)
		}
		if err := os.Rename(src, target); err != nil {
			// 复位这一个，再回滚其余。
			_ = os.Rename(backup, target)
			rollback()
			return nil, fmt.Errorf("写入 %s 失败：%w", name, err)
		}
		done = append(done, swap{target: target, backup: backup})
		res.Replaced = append(res.Replaced, name)
		res.Backups = append(res.Backups, filepath.Base(backup))
	}

	if len(res.Replaced) == 0 {
		return nil, fmt.Errorf("目标目录 %s 下没有可替换的二进制", dstDir)
	}
	return res, nil
}

// checkWritable 预检目录可写（写一个临时文件再删）。
func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".wb2api-writecheck-*")
	if err != nil {
		return fmt.Errorf("目录 %s 不可写：%w", dir, err)
	}
	name := f.Name()
	f.Close()
	_ = os.Remove(name)
	return nil
}
