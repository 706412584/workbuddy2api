// Package updater 从本仓库的 GitHub Releases 检查并安装新版本。
//
// 设计取舍（参考 sub2api 的自更新实现，但按本仓库的约束裁剪）：
//
//  1. **不做 hdiff 增量补丁。** 实测本仓库发布包：全量 5 个二进制共 36MB，而
//     网关主程序 wb2api.exe 单文件 9.7MB。对两次真实构建做内容定义分块（CDC）
//     差分的结果是：相隔 6 天的两次构建只省 1.1x（几乎全量重传），即使只改两行
//     代码的热修也只省 1.8x。原因是 -ldflags="-s -w" + 静态链接下，Go 二进制里
//     任何一处改动都会让其后所有函数的地址偏移，块边界整体漂移。增量补丁在这种
//     构建配置下收益极低，却要额外维护补丁产物矩阵与打补丁工具链。故只做全量替换。
//
//  2. **更新的是整包，不只网关主程序。** 发布包里的 login/credit/signin_bin/activity
//     与网关是同一套源码编出来的，版本必须一致；只换主程序会让面板「添加账号」
//     调用旧版 login.exe，行为分叉且极难排查。
//
//  3. **只做检查与下载，不做自动重启。** 重启由调用方（admin 接口）在替换完成后
//     触发，因为「怎么重启才不掐断在途请求」是 server 侧的知识（见 cmd/server 的
//     零停机交接），不是本包该知道的。
package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/version"
)

// defaultRepo 本仓库的 GitHub 全名（owner/repo）。
// 可用 WB2A_UPDATE_REPO 覆盖，便于 fork 自建发布。
const defaultRepo = "706412584/workbuddy2api"

// defaultAPIBase GitHub REST API 根地址。测试用 httptest 替换它。
const defaultAPIBase = "https://api.github.com"

// checkCacheTTL 检查结果的内存缓存时长。
// GitHub 未认证 API 限 60 次/小时，面板打开就查一次会把额度烧光；20 分钟足够
// 「刚发了新版，用户点一下就能看到」。
const checkCacheTTL = 20 * time.Minute

// apiTimeout 单次 GitHub API 请求上限。
const apiTimeout = 30 * time.Second

// Asset 一个 Release 附件。
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// Release GitHub Release 的裁剪视图（只取本包用得到的字段）。
type Release struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	PublishedAt time.Time `json:"published_at"`
	Body        string    `json:"body"`
	Prerelease  bool      `json:"prerelease"`
	Draft       bool      `json:"draft"`
	Assets      []Asset   `json:"assets"`
}

// UpdateInfo 一次检查的结果，直接作为接口响应体。
type UpdateInfo struct {
	// Current 当前运行的版本（编译期注入，见 internal/version）。
	Current string `json:"current"`
	// Latest 远端最新版本 tag。
	Latest string `json:"latest"`
	// Available 是否存在可更新的新版本。
	Available bool `json:"available"`
	// DevBuild 当前是否为开发版（未经发布流程构建）。此时版本无法比较，
	// Available 恒为 false，面板据此提示「开发版不参与更新检查」而不是「已是最新」。
	DevBuild bool `json:"dev_build"`
	// PublishedAt 最新版本的发布时间。
	PublishedAt time.Time `json:"published_at"`
	// Notes Release 说明（changelog）。
	Notes string `json:"notes"`
	// AssetName 与本机平台匹配的发布包文件名；无匹配为空。
	AssetName string `json:"asset_name"`
	// AssetSize 该发布包字节数，供面板显示下载量。
	AssetSize int64 `json:"asset_size"`
	// Downloadable 本平台是否有可自动安装的发布包。
	// 与 Available 分开：可能有新版但没有本平台包（例如只发了 linux），
	// 此时「可更新」但「不可自动安装」，面板要给不同提示。
	Downloadable bool `json:"downloadable"`
	// CheckedAt 本次检查时刻；Cached 表示是否来自缓存。
	CheckedAt time.Time `json:"checked_at"`
	Cached    bool      `json:"cached"`
	// ReleaseURL 该 Release 的网页地址，供无自动包时手动下载。
	ReleaseURL string `json:"release_url"`
}

// Client 更新检查与安装器。
type Client struct {
	// Repo owner/repo。空 = defaultRepo 或 WB2A_UPDATE_REPO。
	Repo string
	// APIBase GitHub API 根；空 = 官方地址。测试注入 httptest 用。
	APIBase string
	// Token 可选的 GitHub token（提高 API 限额；私有 fork 也靠它）。
	Token string
	// GOOS/GOARCH 目标平台；空 = runtime 实际值。测试注入用。
	GOOS, GOARCH string
	// HTTPClient 出站客户端；nil = http.DefaultClient。测试注入用，
	// 也留给将来需要走代理的场景。
	HTTPClient *http.Client

	mu     sync.Mutex
	cached *UpdateInfo
	// checkedAt 上次实际发起网络请求的时刻（缓存新鲜度只看它）。
	checkedAt time.Time
}

// httpClient 返回出站客户端。
func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

// New 用环境变量构造一个 Client（WB2A_UPDATE_REPO / WB2A_UPDATE_TOKEN / GITHUB_TOKEN）。
func New() *Client {
	token := os.Getenv("WB2A_UPDATE_TOKEN")
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}
	return &Client{
		Repo:  os.Getenv("WB2A_UPDATE_REPO"),
		Token: token,
	}
}

func (c *Client) repo() string {
	if c.Repo != "" {
		return c.Repo
	}
	return defaultRepo
}

func (c *Client) apiBase() string {
	if c.APIBase != "" {
		return strings.TrimRight(c.APIBase, "/")
	}
	return defaultAPIBase
}

func (c *Client) goos() string {
	if c.GOOS != "" {
		return c.GOOS
	}
	return runtime.GOOS
}

func (c *Client) goarch() string {
	if c.GOARCH != "" {
		return c.GOARCH
	}
	return runtime.GOARCH
}

// CurrentVersion 当前运行版本。
func (c *Client) CurrentVersion() string { return version.Version }

// CheckUpdate 查询最新 Release 并与当前版本比较。
//
// force=false 时优先返回 20 分钟内的缓存，避免面板刷新把 GitHub 限额烧光；
// force=true 强制走网络（面板「重新检查」按钮）。
func (c *Client) CheckUpdate(ctx context.Context, force bool) (*UpdateInfo, error) {
	c.mu.Lock()
	if !force && c.cached != nil && time.Since(c.checkedAt) < checkCacheTTL {
		cp := *c.cached
		cp.Cached = true
		c.mu.Unlock()
		return &cp, nil
	}
	c.mu.Unlock()

	rel, err := c.fetchLatestRelease(ctx)
	if err != nil {
		return nil, err
	}

	cur := c.CurrentVersion()
	dev := version.IsDev()
	info := &UpdateInfo{
		Current:     cur,
		Latest:      rel.TagName,
		DevBuild:    dev,
		PublishedAt: rel.PublishedAt,
		Notes:       rel.Body,
		CheckedAt:   time.Now(),
		ReleaseURL:  fmt.Sprintf("https://github.com/%s/releases/tag/%s", c.repo(), rel.TagName),
	}
	if !dev {
		info.Available = CompareVersions(rel.TagName, cur) > 0
	}
	if a := selectAsset(rel.Assets, c.goos(), c.goarch()); a != nil {
		info.AssetName = a.Name
		info.AssetSize = a.Size
		info.Downloadable = true
	}
	// 只有真有新版才把 Downloadable 当回事：没新版时"有包"不构成可更新。
	if !info.Available {
		info.Downloadable = false
	}

	c.mu.Lock()
	c.cached = info
	c.checkedAt = time.Now()
	c.mu.Unlock()

	return info, nil
}

// fetchLatestRelease 拉取最新 Release。
//
// 用 /releases/latest 而非 /releases?per_page=1：前者由 GitHub 保证跳过 draft 与
// prerelease，正是「最新正式版」的语义；后者会拿到刚推的预发布 tag。
func (c *Client) fetchLatestRelease(ctx context.Context) (*Release, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/latest", c.apiBase(), c.repo())
	reqCtx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", version.UserAgent()) // GitHub 对无 UA 请求直接 403
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("查询 GitHub 失败：%w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, fmt.Errorf("仓库 %s 尚无 Release（或不可见）", c.repo())
	case http.StatusForbidden, http.StatusTooManyRequests:
		return nil, fmt.Errorf("GitHub API 限流（未认证 60 次/小时）；稍后再试或配置 WB2A_UPDATE_TOKEN")
	default:
		return nil, fmt.Errorf("GitHub API 返回 %s", resp.Status)
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("解析 Release 响应失败：%w", err)
	}
	if rel.TagName == "" {
		return nil, fmt.Errorf("Release 缺少 tag_name，响应格式异常")
	}
	return &rel, nil
}

// selectAsset 在附件里挑出与本平台匹配的发布包。
//
// 命名约定见 .github/workflows/release.yml：wb2api-<version>-<goos>-<goarch>.<ext>，
// 例如 wb2api-v1.0.0-windows-amd64.zip、wb2api-v1.0.0-linux-amd64.tar.gz。
// 用**后缀**匹配而非解析版本号：版本里本身含 '-'（v1.0.0、dev-abc1234），
// 按 '-' 切分会歧义，而后缀是唯一确定的。
func selectAsset(assets []Asset, goos, goarch string) *Asset {
	suffix := fmt.Sprintf("-%s-%s.%s", goos, goarch, archiveExt(goos))
	for i := range assets {
		a := &assets[i]
		if strings.HasPrefix(a.Name, "wb2api-") && strings.HasSuffix(a.Name, suffix) {
			return a
		}
	}
	return nil
}

// archiveExt 各平台的发布包扩展名（与 release.yml 的 matrix.fmt 一致）。
func archiveExt(goos string) string {
	if goos == "windows" {
		return "zip"
	}
	return "tar.gz"
}

// CompareVersions 比较两个版本号，返回 -1 / 0 / 1。
//
// 支持 "v1.2.3" 与 "1.2.3" 两种写法（本仓库 tag 带 v，但版本号来源未必都带），
// 以及 semver 预发布段（"1.2.3-rc.1"）。比较规则：
//   - 数字段逐位比较，缺位补 0（1.2 == 1.2.0）；
//   - 数字段相同时，**有预发布段 < 无预发布段**（1.2.3-rc.1 < 1.2.3），符合 semver；
//   - 两段都有预发布段则按标识符逐位比较（数字按数值，其余按字典序，数字 < 非数字）。
//
// 非数字的段（解析不出来）按字典序兜底，保证永不 panic、结果稳定。
func CompareVersions(a, b string) int {
	ac, ap := splitVersion(a)
	bc, bp := splitVersion(b)

	// 主体段逐位比较。
	n := len(ac)
	if len(bc) > n {
		n = len(bc)
	}
	for i := 0; i < n; i++ {
		var x, y int
		if i < len(ac) {
			x = ac[i]
		}
		if i < len(bc) {
			y = bc[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}

	// 主体相同：无预发布段 > 有预发布段。
	if ap == "" && bp == "" {
		return 0
	}
	if ap == "" {
		return 1
	}
	if bp == "" {
		return -1
	}
	return comparePrerelease(ap, bp)
}

// splitVersion 把版本串切成数字段 + 预发布段。
// "v1.2.3-rc.1" → ([1,2,3], "rc.1")；"dev-abc1234" → ([0], "abc1234")。
func splitVersion(v string) ([]int, string) {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimPrefix(v, "V")

	// 预发布段在第一个 '-' 之后（build 元数据 '+' 之后的部分对排序无意义，丢弃）。
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}
	pre := ""
	if i := strings.IndexByte(v, '-'); i >= 0 {
		pre = v[i+1:]
		v = v[:i]
	}

	parts := strings.Split(v, ".")
	nums := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			// 非数字段（如 dev 前缀已在上面的 '-' 切分中处理掉）：整体退化为 0，
			// 交给预发布段比较决定先后。
			n = 0
		}
		nums = append(nums, n)
	}
	if len(nums) == 0 {
		nums = []int{0}
	}
	return nums, pre
}

// comparePrerelease 比较预发布段（"rc.1" vs "beta.2"）。
func comparePrerelease(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		// 短的一方缺位视为「更小」：rc.1 < rc.1.1。
		if i >= len(as) {
			return -1
		}
		if i >= len(bs) {
			return 1
		}
		x, y := as[i], bs[i]
		xn, xerr := strconv.Atoi(x)
		yn, yerr := strconv.Atoi(y)
		switch {
		case xerr == nil && yerr == nil:
			if xn != yn {
				if xn < yn {
					return -1
				}
				return 1
			}
		case xerr == nil: // 数字标识符 < 非数字标识符（semver 规则）
			return -1
		case yerr == nil:
			return 1
		default:
			if x != y {
				if x < y {
					return -1
				}
				return 1
			}
		}
	}
	return 0
}
