// system.go 更新检查与安装接口（/__admin/system/*）。
//
// 三条路径分工：
//   - GET  /__admin/system/version        当前版本（编译期注入，零成本）
//   - GET  /__admin/system/check-updates  查 GitHub Releases 有无新版（带 20 分钟缓存）
//   - POST /__admin/system/update         下载 + 校验 + 替换，然后零停机交接重启
//
// 安装成功后不在这里重启，而是调 cfg.RequestRestart 投递信号 —— 重启是 server 侧
// 的知识（见 cmd/server/handoff.go），admin 只负责"把新二进制放到位"。
package admin

import (
	"net/http"
	"runtime"

	"workbuddy2api/internal/updater"
	"workbuddy2api/internal/version"
)

// systemVersion GET /__admin/system/version —— 当前版本与构建信息。
func (h *Handler) systemVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":    version.Version,
		"commit":     version.Commit,
		"build_time": version.BuildTime,
		"dev_build":  version.IsDev(),
		"goos":       goosName(),
		"goarch":     goarchName(),
		// can_self_update 表示本进程是否具备零停机交接能力。false 时更新接口
		// 仍可替换文件，但不会自动重启（面板据此提示"需手工重启"）。
		"can_self_update": h.cfg.RequestRestart != nil,
	})
}

// systemCheckUpdates GET /__admin/system/check-updates —— 检查有无新版本。
//
// 查询参数 force=1 强制走网络（面板"重新检查"按钮）；否则 20 分钟内复用缓存，
// 避免频繁刷新把 GitHub 未认证限额（60 次/小时）烧光。
func (h *Handler) systemCheckUpdates(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Updater == nil {
		fail(w, http.StatusNotImplemented, "本进程未接线更新检查")
		return
	}
	force := r.URL.Query().Get("force") == "1"
	info, err := h.cfg.Updater.CheckUpdate(r.Context(), force)
	if err != nil {
		// 502：错误来自上游（GitHub），不是本进程的问题。
		fail(w, http.StatusBadGateway, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// systemUpdate POST /__admin/system/update —— 下载并安装最新版，然后触发交接重启。
//
// 请求体（可空）：
//
//	{"version": "v1.2.3"}  指定版本；空 = 用最新版
//	{"check_only": true}   只校验能否安装，不落盘（预演）
//
// 同步返回：下载 + 校验 + 替换是分钟级操作，但远短于浏览器超时，故同步做完再返回，
// 用户能立刻看到成功/失败。只有"重启"是异步的（投递信号后本进程稍后退出，
// 若同步等重启完成，响应根本发不出去）。
func (h *Handler) systemUpdate(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Updater == nil {
		fail(w, http.StatusNotImplemented, "本进程未接线更新功能")
		return
	}
	var body struct {
		Version   string `json:"version"`
		CheckOnly bool   `json:"check_only"`
	}
	if !decodeBody(w, r, &body) {
		return
	}

	ctx := r.Context()
	info, err := h.cfg.Updater.CheckUpdate(ctx, true)
	if err != nil {
		fail(w, http.StatusBadGateway, "%v", err)
		return
	}
	// 指定了版本就核对一下：版本不匹配直接拒绝，免得"以为装了 A 实际装了 B"。
	if body.Version != "" && body.Version != info.Latest {
		fail(w, http.StatusConflict, "请求的版本 %s 与最新版 %s 不一致（本接口只安装最新版）",
			body.Version, info.Latest)
		return
	}
	if !info.Available {
		if info.DevBuild {
			fail(w, http.StatusConflict, "当前为开发版（%s），不参与版本比较；如需更新请指定发布版本",
				info.Current)
			return
		}
		ok(w, map[string]any{"msg": "已是最新版本 " + info.Current, "latest": info.Latest})
		return
	}
	if !info.Downloadable {
		fail(w, http.StatusConflict, "最新版 %s 没有本平台的发布包，请到 %s 手动下载",
			info.Latest, info.ReleaseURL)
		return
	}
	if body.CheckOnly {
		ok(w, map[string]any{
			"msg":   "检查通过，可安装 " + info.Latest,
			"asset": info.AssetName,
			"size":  info.AssetSize,
		})
		return
	}

	exeDir, err := updater.ExeDir()
	if err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	logf("开始更新：%s → %s（%s，%d 字节）", info.Current, info.Latest, info.AssetName, info.AssetSize)
	res, err := h.cfg.Updater.Install(ctx, info, exeDir)
	if err != nil {
		logf("更新失败：%v", err)
		fail(w, http.StatusInternalServerError, "更新失败：%v", err)
		return
	}
	logf("更新完成：已替换 %v（备份 %v），目录 %s", res.Replaced, res.Backups, res.ExeDir)

	// 触发交接重启。不能同步等：本进程随后会退出，响应就发不出去了。
	// 投递后立即返回，用户看到"已更新，正在重启"，前端稍后重新轮询即可。
	restarting := false
	if h.cfg.RequestRestart != nil {
		h.cfg.RequestRestart("更新到 " + info.Latest)
		restarting = true
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"version":    res.Version,
		"asset":      res.Asset,
		"replaced":   res.Replaced,
		"skipped":    res.Skipped,
		"backups":    res.Backups,
		"exe_dir":    res.ExeDir,
		"restarting": restarting,
		"msg":        restartMsg(restarting),
	})
}

// systemRestart POST /__admin/system/restart —— 不更新，只做一次零停机交接重启。
//
// 为什么单独留这个接口（而非只让更新接口重启）：换了 config.json 里需要重启才生效的
// 项（如 prompt.file、server.* 上限）、或想验证交接链路时，都需要一次干净的重启。
// 它比「杀进程再起」强的唯一一点就是**不掐断在途请求**，其余语义一致。
func (h *Handler) systemRestart(w http.ResponseWriter, r *http.Request) {
	if h.cfg.RequestRestart == nil {
		fail(w, http.StatusNotImplemented, "本进程不支持自动重启（未接线交接），请手工重启")
		return
	}
	logf("收到零停机交接重启请求")
	h.cfg.RequestRestart("手工触发重启")
	ok(w, map[string]any{
		"restarting": true,
		"msg":        "正在零停机交接重启（新进程接管同一端口，在途请求会跑完）",
	})
}

// restartMsg 给用户一句准确的结果描述。
// 不能统一说"已重启"：没有交接能力时文件已换但进程还是旧的，必须让人知道去手工重启。
func restartMsg(restarting bool) string {
	if restarting {
		return "更新完成，正在零停机交接重启（新进程接管同一端口，在途请求会跑完）"
	}
	return "更新完成，但本进程不支持自动重启：请手工重启网关以启用新版本"
}

// 平台名透出给面板显示；直接用 runtime 的字符串，避免另建一套常量。
func goosName() string   { return runtime.GOOS }
func goarchName() string { return runtime.GOARCH }
