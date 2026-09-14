//go:build !windows

package admin

import "os/exec"

// hideWindow 在非 Windows 平台是空操作：没有「控制台窗口」这个概念，
// 子进程的 stdout/stderr 由调用方用管道或缓冲区接管即可。
func hideWindow(cmd *exec.Cmd) {}
