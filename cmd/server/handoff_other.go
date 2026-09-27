//go:build !windows

// handoff_other.go Unix 的监听句柄复制与子进程继承。
//
// Unix 上套接字天生是文件描述符，TCPListener.File() 无 Windows 那种
// 「Accept 之后就拿不到」的限制，语义也更直白：FileListener 在新进程里
// dup 出独立 fd，旧进程关闭自己的 fd 不影响新进程。
package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
)

// dupListener 复制监听套接字为 *os.File。
// 该文件在 spawnReplacement 里作为 ExtraFiles[0] 传给子进程，子进程侧对应 fd 3。
//
// Unix 上 File() 不会破坏父进程的关闭路径（那是 Windows DisassociateIOCP 的
// 特有副作用），但本实现统一不调用 Shutdown（见 handoff.go 约束），
// 两条平台走同一套排空逻辑，避免"只在某个平台验证过"的分叉。
func dupListener(ln net.Listener) (*os.File, error) {
	tl, ok := ln.(*net.TCPListener)
	if !ok {
		return nil, fmt.Errorf("监听器类型 %T 不支持句柄复制", ln)
	}
	return tl.File()
}

// inheritListener 新进程侧：ExtraFiles 的第 0 项在子进程里固定是 fd 3。
func inheritListener() (net.Listener, bool, error) {
	raw := os.Getenv(handoffEnv)
	if raw == "" {
		return nil, false, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return nil, true, fmt.Errorf("%s=%q 不是合法 fd：%w", handoffEnv, raw, err)
	}
	f := os.NewFile(uintptr(n), "inherited-listener")
	if f == nil {
		return nil, true, fmt.Errorf("fd %d 无效", n)
	}
	ln, err := net.FileListener(f)
	if err != nil {
		return nil, true, fmt.Errorf("接管继承的监听套接字失败：%w", err)
	}
	_ = f.Close()
	return ln, true, nil
}

// spawnReplacement 起新进程：监听文件作为 ExtraFiles[0]（子进程侧 fd 3），
// 就绪管道写端作为 ExtraFiles[1]（子进程侧 fd 4）。
func spawnReplacement(f *os.File) (spawnResult, error) {
	if f == nil {
		return spawnResult{}, fmt.Errorf("监听文件为空")
	}
	exe, err := os.Executable()
	if err != nil {
		return spawnResult{}, err
	}

	// 就绪管道：新进程接管监听后往 fd 4 写 1 字节。Unix 上 ExtraFiles 传的句柄
	// 天然可继承，无需额外设置标志。
	pr, pw, err := os.Pipe()
	if err != nil {
		return spawnResult{}, fmt.Errorf("创建就绪管道失败：%w", err)
	}

	cmd := exec.Command(exe, os.Args[1:]...)
	// ExtraFiles[0] → 子进程 fd 3（监听）；ExtraFiles[1] → 子进程 fd 4（就绪）。
	cmd.Env = append(os.Environ(), handoffEnv+"=3", readyEnv+"=4")
	cmd.Dir = mustGetwd()
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{f, pw}
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return spawnResult{}, err
	}
	// 父进程关掉写端副本：新进程一旦退出，读端会得到 EOF（可靠的就绪判据）。
	pw.Close()
	return spawnResult{pid: cmd.Process.Pid, proc: cmd.Process, ready: pr}, nil
}

// inheritReady 新进程侧：就绪管道写端固定为 fd 4（见 spawnReplacement 的 ExtraFiles）。
func inheritReady() *os.File {
	if os.Getenv(handoffEnv) == "" {
		return nil
	}
	f := os.NewFile(4, "ready-pipe")
	if f == nil {
		return nil
	}
	return f
}
