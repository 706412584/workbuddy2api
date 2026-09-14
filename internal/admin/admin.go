// Package admin 提供管理面板的服务端接口（/__admin/*）。
//
// 这套接口原本是前端 vite 的中间件（frontend/vite.admin.ts），迁进网关进程后
// 有了本质差别：它能直接读写网关自己的状态 —— 账号池、凭证目录、配置、日志，
// 于是不必再通过 HTTP 回调自己（原实现要读 config.json 取全部密钥、逐个问
// /status 再取并集），也不必靠重启进程来让改动生效。
//
// 安全边界：这些接口能写文件、能起进程，而网关默认监听 0.0.0.0:7863（局域网可达），
// 所以每一条都必须先过 isLocal。
package admin

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sync"

	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// KeyEntry 一条 API 密钥。Region 为空表示不限区域。
type KeyEntry struct {
	Key    string `json:"key"`
	Region string `json:"region"`
	Name   string `json:"name"`
}

// Config admin 的依赖。
type Config struct {
	Pool     *pool.Pool
	Upstream *upstream.Client
	// AuthDir 凭证目录；账号的增删查都落在这里。
	AuthDir string
	// ConfigPath config.json 路径（密钥的读写）。
	ConfigPath string
	// LogPath 网关日志；日志/统计/调度三页都靠解析它。
	LogPath string
	// LoginBin 设备码登录工具（login.exe）。
	LoginBin string
	// RepoRoot 仓库根目录；login 工具的工作目录（它的 state 路径是盘符相对的，
	// 生成链接与查询结果两次调用必须同目录）。
	RepoRoot string
	// ReloadAccounts 让网关重新扫描凭证目录，返回加载到的账号数。
	ReloadAccounts func() (int, error)
	// ReloadKeys 让网关换用新的密钥表。
	ReloadKeys func(legacy string, keys []KeyEntry)
	// ModelsByRegion 返回两区各自可见的模型 id，供「测试连接」的模型下拉框用。
	ModelsByRegion func() (cn, global []string)
	// RunTask 立即执行一次指定任务（key 见 taskKeys），阻塞到跑完并返回一句摘要。
	// nil = 本进程未接线调度器，「立即执行」不可用。
	//
	// 约定为阻塞式：调用方在后台 goroutine 里跑它，好在跑的过程中把「执行中」透给面板。
	// 摘要即结果（失败也写在摘要里，见 taskRun.Summary）。
	RunTask func(key string) string
}

// Handler 管理接口路由。
type Handler struct {
	cfg Config
	mux *http.ServeMux
	// login poll 是单次性操作（成功即消费掉 state），并发调用会互相抢，故串行化。
	pollMu sync.Mutex
	// runs 「立即执行」的运行态（进程内，重启即忘）。
	runs *runTracker
}

// New 构建管理接口。
func New(cfg Config) *Handler {
	h := &Handler{cfg: cfg, mux: http.NewServeMux(), runs: newRunTracker()}

	h.mux.HandleFunc("GET /__admin/accounts", h.listAccounts)
	h.mux.HandleFunc("POST /__admin/accounts/import", h.importAccount)
	h.mux.HandleFunc("POST /__admin/accounts/delete", h.deleteAccount)
	h.mux.HandleFunc("POST /__admin/accounts/test", h.testAccount)
	h.mux.HandleFunc("GET /__admin/loaded", h.loaded)

	h.mux.HandleFunc("POST /__admin/login/start", h.loginStart)
	h.mux.HandleFunc("POST /__admin/login/poll", h.loginPoll)

	h.mux.HandleFunc("POST /__admin/pool/reload", h.poolReload)
	h.mux.HandleFunc("POST /__admin/pool/reset", h.poolReset)

	h.mux.HandleFunc("GET /__admin/logs", h.logs)
	h.mux.HandleFunc("GET /__admin/stats", h.stats)
	h.mux.HandleFunc("GET /__admin/schedule", h.schedule)
	h.mux.HandleFunc("POST /__admin/schedule/run", h.runScheduleTask)
	h.mux.HandleFunc("GET /__admin/models", h.regionModels)

	h.mux.HandleFunc("GET /__admin/apikeys", h.getKeys)
	h.mux.HandleFunc("POST /__admin/apikeys", h.saveKeys)

	return h
}

// ServeHTTP 只做一件事：把非本机的请求挡在外面。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !isLocal(r) {
		fail(w, http.StatusForbidden, "账号管理仅允许本机访问")
		return
	}
	h.mux.ServeHTTP(w, r)
}

// isLocal 判定请求是否来自本机。
// 网关默认绑 0.0.0.0，这层判断是「能写文件」与「任何人都能写文件」之间的唯一屏障。
func isLocal(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	switch host {
	case "127.0.0.1", "::1", "::ffff:127.0.0.1":
		return true
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		// 只在响应结构体写错时发生；此时头还没发出去，还能改成 500
		http.Error(w, `{"error":"响应序列化失败"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// fail 按前端约定的错误体形状回错：{"error": "..."}（client.ts 的 readError 读它）。
func fail(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, map[string]any{"error": fmt.Sprintf(format, args...)})
}

// ok 回一个通用成功体，省掉各处的 map 字面量。
func ok(w http.ResponseWriter, extra map[string]any) {
	if extra == nil {
		extra = map[string]any{}
	}
	extra["ok"] = true
	writeJSON(w, http.StatusOK, extra)
}

// decodeBody 读并解析请求体。body 为空按空对象处理（与前端 `|| '{}'` 一致）。
// 解析失败时已写好 400 响应，返回 false。
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		fail(w, http.StatusBadRequest, "读取请求体失败：%v", err)
		return false
	}
	if len(raw) == 0 {
		return true
	}
	if err := json.Unmarshal(raw, v); err != nil {
		fail(w, http.StatusBadRequest, "请求体不是合法 JSON：%v", err)
		return false
	}
	return true
}

// readConfig 读并解析 config.json。
func (h *Handler) readConfig(v any) error {
	raw, err := os.ReadFile(h.cfg.ConfigPath)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

// logf 管理接口的内部日志。改动账号池/配置这类操作值得留痕，
// 出问题时能对上「谁在什么时候动了什么」。
func logf(format string, args ...any) {
	log.Printf("[admin] "+format, args...)
}
