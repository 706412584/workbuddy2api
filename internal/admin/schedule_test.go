package admin

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 一段最小日志：启动块声明四类排程，之后无执行行。
// 首行必须是 "loaded N account(s)" —— afterLastStartup 以它为进程起点，
// 缺了它整段日志会被判定为「无启动行」而回落到 config。
const schedLog = `2026/09/14 11:17:08 loaded 19 account(s) from ./auths
2026/09/14 11:17:08 签到已启用：[9 21] 点（签到 + 余额查询解冻）
2026/09/14 11:17:08 猫猫旅行已启用：[9 21] 点（独立排程：领养 / 派出 / 领奖）
2026/09/14 11:17:08 活跃上报已启用：[10] 点（每号 5 条，点亮连登 + 补满领猫对话门槛）
2026/09/14 11:17:08 token 保活已启用：[22] 点
2026/09/14 11:17:08 workbuddy2api listening on :7863
`

func newSchedHandler(t *testing.T) *Handler {
	t.Helper()
	dir := t.TempDir()
	lp := filepath.Join(dir, "gateway.log")
	if err := os.WriteFile(lp, []byte(schedLog), 0o600); err != nil {
		t.Fatal(err)
	}
	// ConfigPath 指向不存在的文件：本用例只走日志分支，不该回落到 config。
	return New(Config{LogPath: lp, ConfigPath: filepath.Join(dir, "nonexistent.json")})
}

// TestCollectScheduleCarriesManualRun 排程响应必须带上 manual 字段。
// 面板靠它显示「手动执行」列与执行中状态 —— 少了它按钮点下去就永远没反馈。
func TestCollectScheduleCarriesManualRun(t *testing.T) {
	h := newSchedHandler(t)
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.Local)

	// 未手动执行过：manual 为 nil。
	got := h.collectSchedule(now)
	tasks := got["tasks"].([]map[string]any)
	if len(tasks) != 4 {
		t.Fatalf("tasks=%d want 4", len(tasks))
	}
	for _, task := range tasks {
		if task["manual"] != nil {
			t.Errorf("%v 未执行过时 manual 应为 nil，got %v", task["key"], task["manual"])
		}
	}

	// 手动跑一趟（同步完成）后：manual 应带摘要且不在 running。
	if !h.runs.begin("checkin") {
		t.Fatal("begin 应成功")
	}
	h.runs.finish("checkin", "19 个账号：成功 12 · 已签到 5 · 跳过 2 · 失败 0")

	got = h.collectSchedule(now)
	for _, task := range got["tasks"].([]map[string]any) {
		m, _ := task["manual"].(*taskRun)
		switch task["key"] {
		case "checkin":
			if m == nil {
				t.Fatal("checkin 应带 manual")
			}
			if m.Running {
				t.Error("已跑完，running 应为 false")
			}
			if m.Summary == "" {
				t.Error("应带结果摘要")
			}
		default:
			if m != nil {
				t.Errorf("%v 未执行过，manual 应为 nil", task["key"])
			}
		}
	}
}

// TestCollectScheduleNextRunFromLogHours 时点表取自启动日志，下次运行按它推算。
// 12:00 时点下：签到/旅行 [9,21] → 当天 21:00；活跃 [10] → 次日 10:00；保活 [22] → 当天 22:00。
func TestCollectScheduleNextRunFromLogHours(t *testing.T) {
	h := newSchedHandler(t)
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.Local)
	got := h.collectSchedule(now)

	want := map[string]string{
		"checkin":   "2026-09-14T13:00:00.000Z", // 21:00 CST = 13:00 UTC
		"travel":    "2026-09-14T13:00:00.000Z",
		"activity":  "2026-09-15T02:00:00.000Z", // 次日 10:00 CST
		"keepalive": "2026-09-14T14:00:00.000Z", // 22:00 CST
	}
	for _, task := range got["tasks"].([]map[string]any) {
		k := task["key"].(string)
		if task["source"] != "log" {
			t.Errorf("%v source=%v want log（日志里有启动行）", k, task["source"])
		}
		if got, ok := task["nextRun"].(string); !ok || got != want[k] {
			t.Errorf("%v nextRun=%v want %s", k, task["nextRun"], want[k])
		}
	}
}
