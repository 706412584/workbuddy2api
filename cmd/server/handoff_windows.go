//go:build windows

// handoff_windows.go Windows 的监听句柄复制与子进程继承。
//
// **必须在 Accept 之前复制**：实测本机 Windows，net.TCPListener.File() 在
// 尚无 Accept 时 20/20 成功，一旦有并发 Accept 就 0/20 失败
// （报 "A device attached to the system is not functioning"）。
// 故 main 在起 Serve 之前先调 dupListener 复制句柄，再开始服务。
package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"syscall"
)

// dupListener 复制监听套接字句柄。
// 返回的 *os.File 由调用方（handoffState）长期持有，spawnReplacement 用它的句柄号。
//
// **用 File() 而不是 DuplicateHandle**：实测 DuplicateHandle(bInheritHandle=TRUE)
// 能保住父进程的 Shutdown，但子进程 net.FileListener 接不住（"参数错误"）——
// Go 的 dup 不止是 DuplicateHandle，还要 DisassociateIOCP + WSASocket 重建，
// 子进程拿到裸 DuplicateHandle 句柄无法还原成 socket。File() 两条都满足，
// 代价是父进程的 Shutdown/Close 会永久卡住（见 handoff.go 文件头约束）。
func dupListener(ln net.Listener) (*os.File, error) {
	tl, ok := ln.(*net.TCPListener)
	if !ok {
		return nil, fmt.Errorf("监听器类型 %T 不支持句柄复制", ln)
	}
	return tl.File()
}

// inheritListener 新进程侧：若环境里有继承来的句柄号，则接管它作为监听器。
// 返回 (listener, true, nil) 表示本次为交接启动。
func inheritListener() (net.Listener, bool, error) {
	raw := os.Getenv(handoffEnv)
	if raw == "" {
		return nil, false, nil
	}
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return nil, true, fmt.Errorf("%s=%q 不是合法句柄号：%w", handoffEnv, raw, err)
	}
	f := os.NewFile(uintptr(n), "inherited-listener")
	if f == nil {
		return nil, true, fmt.Errorf("句柄 %d 无效", n)
	}
	ln, err := net.FileListener(f)
	if err != nil {
		return nil, true, fmt.Errorf("接管继承的监听套接字失败：%w", err)
	}
	// FileListener 已 dup 出独立的 fd，原始句柄可关。
	_ = f.Close()
	return ln, true, nil
}

// spawnReplacement 以「继承监听句柄 + 就绪管道」的方式启动一份新的自身。
//
// 日志/标准输出不重定向：新进程继承旧进程的 stdio，而旧进程是被 start.cmd
// 重定向到 data\gateway.log 启动的，故新进程的日志自然接到同一个文件。
func spawnReplacement(f *os.File) (spawnResult, error) {
	if f == nil {
		return spawnResult{}, fmt.Errorf("监听文件为空")
	}
	h := syscall.Handle(f.Fd())
	exe, err := os.Executable()
	if err != nil {
		return spawnResult{}, err
	}

	// 就绪管道：新进程接管监听后往写端写 1 字节。写端必须显式标记可继承
	// （os.Pipe 建的句柄默认不带 HANDLE_FLAG_INHERIT），否则子进程拿到的是
	// 一个无效句柄 —— 实测会静默写失败，父进程就只能等超时。
	pr, pw, err := os.Pipe()
	if err != nil {
		return spawnResult{}, fmt.Errorf("创建就绪管道失败：%w", err)
	}
	wh := syscall.Handle(pw.Fd())
	if err := syscall.SetHandleInformation(wh, syscall.HANDLE_FLAG_INHERIT, syscall.HANDLE_FLAG_INHERIT); err != nil {
		pr.Close()
		pw.Close()
		return spawnResult{}, fmt.Errorf("设置就绪管道可继承失败：%w", err)
	}

	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Env = append(os.Environ(),
		handoffEnv+"="+strconv.FormatUint(uint64(h), 10),
		readyEnv+"="+strconv.FormatUint(uint64(wh), 10),
	)
	cmd.Dir = mustGetwd()
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:                 true,
		AdditionalInheritedHandles: []syscall.Handle{h, wh},
	}
	// 双保险：既让调用方持有引用，也在这里挡一次 GC —— Start 内部要读 f 的句柄号，
	// 此刻被 finalizer 关掉就会报 "The parameter is incorrect"（实测踩过）。
	runtime.KeepAlive(f)
	runtime.KeepAlive(pw)
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return spawnResult{}, err
	}
	// 父进程关掉写端副本：这样新进程一旦退出，读端会得到 EOF（可靠的就绪判据）。
	pw.Close()
	// 不 Wait：新进程要独立存活。Start 已复制句柄，子进程与旧进程无父子生命周期绑定。
	return spawnResult{pid: cmd.Process.Pid, proc: cmd.Process, ready: pr}, nil
}

// inheritReady 新进程侧：取出继承来的就绪管道写端。未接线时返回 nil。
func inheritReady() *os.File {
	raw := os.Getenv(readyEnv)
	if raw == "" {
		return nil
	}
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return nil
	}
	return os.NewFile(uintptr(n), "ready-pipe")
}
