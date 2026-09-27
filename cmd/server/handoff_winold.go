//go:build windows && !go1.25

// handoff_winold.go 旧工具链的硬失败闸门（仅在 Windows + Go < 1.25 时参与编译）。
//
// 为什么需要它：Windows 的零停机交接依赖 net.TCPListener.File() 与 net.FileListener，
// 而这两者在 Windows 上直到 **Go 1.25** 才实现（1.22~1.24 的 net/file_windows.go 里
// 是 `return nil, syscall.EWINDOWS` 的 TODO 桩）。用旧工具链构建不会报任何错，但产物
// 在交接时只能静默降级为「无法复制监听句柄」—— 更新永远落到手工重启，且从二进制本身
// 看不出原因（实测踩过：CI 用 go.mod 的 1.22.5 构建，本地用 1.26.5 测试全绿）。
//
// 故这里故意引用一个不存在的符号，把「静默降级」变成**构建期硬失败**，
// 错误信息本身就是排查指引：
//
//	undefined: WindowsZeroDowntimeHandoffRequiresGo1_25_or_later
//
// go.mod 已声明 `go 1.25.0`，正常情况下工具链会自动升到 1.25+，本文件不参与编译；
// 它兜住的是 GOTOOLCHAIN=local 之类的强制降级场景。
package main

var _ = WindowsZeroDowntimeHandoffRequiresGo1_25_or_later
