package main

import (
	"os"
	"testing"
	"time"
)

// TestWaitReady 就绪握手的三条判据：收到 1 字节才算就绪；写端关闭（EOF）与超时都不算。
//
// 这是「spawn 成功 ≠ 能服务」这一教训的回归测试：换成不支持交接的二进制时，子进程会
// 重新 bind 失败并退出，父进程必须据此判定「未就绪」而不是照常退出。
func TestWaitReady(t *testing.T) {
	t.Run("收到1字节即就绪", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		go func() { _, _ = w.Write([]byte{1}); w.Close() }()
		if !waitReady(r, 2*time.Second) {
			t.Error("写入 1 字节后应判为就绪")
		}
	})

	t.Run("写端关闭即EOF不算就绪", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		w.Close() // 子进程已退出、父进程也关了写端副本 → EOF
		if waitReady(r, 2*time.Second) {
			t.Error("EOF 不应判为就绪")
		}
	})

	t.Run("超时不算就绪", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		defer w.Close()
		start := time.Now()
		if waitReady(r, 150*time.Millisecond) {
			t.Error("无信号时不应判为就绪")
		}
		if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
			t.Errorf("应等待到超时，实际只等了 %v", elapsed)
		}
	})

	t.Run("非1字节不算就绪", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		go func() { _, _ = w.Write([]byte{0}); w.Close() }()
		if waitReady(r, 2*time.Second) {
			t.Error("字节值非 1 不应判为就绪")
		}
	})
}
