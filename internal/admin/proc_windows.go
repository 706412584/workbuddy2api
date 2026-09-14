//go:build windows

package admin

import (
	"os/exec"
	"syscall"
)

// hideWindow 抑制子进程弹出控制台窗口。
//
// 非可选优化：网关自身是被 detach 启动的（stdio 重定向到日志文件），手里没有可继承的
// 控制台，于是 Windows 会给每个子控制台程序**新建一个窗口**。面板登录要反复调
// login.exe（轮询是每几秒一次），不隐藏的话屏幕上就会不停弹黑框。
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
