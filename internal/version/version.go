// Package version 携带构建期注入的版本信息，供更新检查与面板展示。
//
// 三个变量都靠 -ldflags -X 在编译时注入（见 .github/workflows/release.yml 与
// README「源码构建」）。源码里刻意不写死版本号：写死就意味着每发一版都要改代码，
// 而「改了代码忘了改版本号」是发布流程里最常见的一类事故。默认值只用于
// `go build` 直接编出来的开发版，让它在面板上明确显示为「开发版」而不是某个假版本。
package version

import (
	"fmt"
	"runtime"
	"strings"
)

// 构建期由 ldflags 覆盖。默认值语义 = 未经发布流程构建的开发版。
var (
	// Version 发布版本，形如 v1.2.3（取自 git tag）或 dev-abc1234（手动构建）。
	Version = "dev"
	// Commit 构建所基于的 commit 短 sha。
	Commit = "unknown"
	// BuildTime 构建时刻（UTC，RFC3339）。
	BuildTime = "unknown"
)

// IsDev 报告当前是否为未经发布流程构建的开发版。
// 更新检查据此提示「无法比较版本」而不是编造一个「已是最新」。
func IsDev() bool {
	return Version == "" || Version == "dev" || strings.HasPrefix(Version, "dev-")
}

// String 一行式版本描述，用于启动日志与面板。
func String() string {
	var b strings.Builder
	b.WriteString(Version)
	b.WriteString(" (")
	b.WriteString(runtime.GOOS)
	b.WriteByte('/')
	b.WriteString(runtime.GOARCH)
	if Commit != "" && Commit != "unknown" {
		b.WriteString(", ")
		b.WriteString(Commit)
	}
	if BuildTime != "" && BuildTime != "unknown" {
		b.WriteString(", built ")
		b.WriteString(BuildTime)
	}
	b.WriteByte(')')
	return b.String()
}

// UserAgent 更新检查请求的 User-Agent。
// GitHub API 对无 UA 的请求直接 403，故不能省。
func UserAgent() string {
	return fmt.Sprintf("workbuddy2api/%s (%s/%s)", Version, runtime.GOOS, runtime.GOARCH)
}
