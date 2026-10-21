// engine_mgr.go mihomo sidecar 的进程管理（订阅源 → N 个本地出口端口）。
//
// 设计来源：sub2api-custom 的 internal/service/mihomo_engine.go（照搬并做站点适配）。
// 生命周期粒度为**订阅源**（sourceID），不是账号：
//
//	1 个订阅源 → 1 个 mihomo 进程 → N 个 mixed listener 端口 → N 个 proxy_slots
//
// 账号只**粘性绑定**到槽位（见 internal/proxyreg），故增删账号/改绑定**不会**重启
// mihomo，不会造成其它账号的出口抖动。只有订阅节点集合本身变化才重启。
//
// 与参考实现的四点差异（均为踩坑加固，参考项目未做）：
//  1. 写配置前对每个 listener 端口做 net.Listen 预检 —— mihomo 端口被占时**不退出**，
//     只记 bind 错误继续跑；不预检就会陷入「5s 探活失败 → 重启 → 仍失败」的死循环。
//  2. bindAddr 强制 loopback —— mixed 端口无鉴权，绑非 loopback 等于给本机开开放代理。
//  3. mihomo stdout/stderr 逐行转发到 log（带 source 前缀），避免多进程日志混流。
//  4. dataDir 由调用方给绝对路径（Windows 双击启动时 CWD 可能是 System32）。
package mihomosub

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// listenVerifyTimeout 等 listener 端口进入监听的上限。
// 参考值 5s（≈25 次 200ms 重试）——够几十个端口用。
const listenVerifyTimeout = 5 * time.Second

// Engine 管理每个订阅源的 mihomo 子进程。
type Engine struct {
	mu      sync.Mutex
	binary  string
	dataDir string
	procs   map[string]*proc
}

type proc struct {
	runner     *Runner
	configHash string
	lastError  string
	running    bool
}

// NewEngine 构造引擎。binary 可空（后续经 PATH 解析）；dataDir 必须是**绝对路径**。
func NewEngine(binary, dataDir string) *Engine {
	return &Engine{
		binary:  binary,
		dataDir: dataDir,
		procs:   make(map[string]*proc),
	}
}

// ResolveBinary 返回可用的 mihomo 可执行文件路径。
// 顺序：配置的显式路径（os.Stat 绝对路径）→ PATH 查找该名 → PATH 查找 mihomo/clash-meta。
// Windows 的 LookPath 会按 PATHEXT 自动补 .exe，故 "mihomo" 能命中 mihomo.exe。
func (e *Engine) ResolveBinary() (string, bool) {
	if e == nil {
		return "", false
	}
	if e.binary != "" {
		if st, err := os.Stat(e.binary); err == nil && !st.IsDir() {
			return e.binary, true
		}
		if path, err := exec.LookPath(e.binary); err == nil {
			return path, true
		}
		return e.binary, false
	}
	for _, name := range []string{"mihomo", "clash-meta"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, true
		}
	}
	return "mihomo", false
}

// DataDir 返回配置/工作目录根。
func (e *Engine) DataDir() string {
	if e == nil {
		return ""
	}
	return e.dataDir
}

// configPath / workDir 按 source 分目录。
func (e *Engine) configPath(sourceID string) string {
	return filepath.Join(e.dataDir, "source-"+sourceID, "config.yaml")
}

func (e *Engine) workDir(sourceID string) string {
	return filepath.Join(e.dataDir, "source-"+sourceID, "workdir")
}

// ConfigPathFor 供外部（面板/诊断）取配置路径。
func (e *Engine) ConfigPathFor(sourceID string) string {
	return e.configPath(sourceID)
}

// EnsureRunning 写配置并在 hash 变化时重启进程。
//
// bindings 非空 → 重新生成配置（hash 由写入内容决定）；
// bindings 为空且 configHash 非空 → **信任落盘配置**，不重写（冷启动恢复路径，
// 不依赖外部订阅源可达）。
func (e *Engine) EnsureRunning(sourceID, namePrefix, bindAddr string, bindings []Binding, configHash string) error {
	if e == nil {
		return fmt.Errorf("mihomo 引擎未配置")
	}
	if !isLoopback(bindAddr) {
		return fmt.Errorf("bind 地址 %q 不是 loopback：mixed 端口无鉴权，绑非 loopback 会给本机开一个开放代理出口", bindAddr)
	}
	bin, ok := e.ResolveBinary()
	if !ok {
		return fmt.Errorf("找不到 mihomo 可执行文件（请把 mihomo.exe 放到网关同目录，或在 config 里配 proxy_sidecar.binary 指向绝对路径）")
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	cfgPath := e.configPath(sourceID)
	workDir := e.workDir(sourceID)
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return err
	}

	hash := configHash
	if len(bindings) > 0 {
		// 端口预检：mihomo 被占用端口不会退出，必须先自己确认端口空闲，
		// 否则会「启动成功 → 探活失败 → 重启 → 仍失败」空转。
		if err := preflightPorts(bindings); err != nil {
			return err
		}
		h, err := WriteMihomoConfig(cfgPath, bindAddr, namePrefix, bindings)
		if err != nil {
			return err
		}
		hash = h
	}
	if hash == "" {
		return fmt.Errorf("source %s 既无 bindings 也无 configHash，无法确定配置", sourceID)
	}

	p := e.procs[sourceID]
	// 已在运行且配置未变：仍要确认端口真在监听。mihomo 端口被占时不退出，
	// 若上次实际 bind 失败（如上一实例的孤儿占着端口），这里必须能发现并重启，
	// 否则会因 hash 未变而永久短路 —— 面板显示 running 但代理全废。
	if p != nil && p.running && p.configHash == hash && p.runner != nil {
		if err := p.runner.VerifyListening(listenVerifyTimeout); err == nil {
			return nil
		} else {
			p.lastError = err.Error()
			_ = p.runner.Stop()
			p.running = false
			p.runner = nil
		}
	}
	if p != nil && p.runner != nil {
		_ = p.runner.Stop()
	}

	runner := &Runner{Binary: bin, ConfigPath: cfgPath, DataDir: workDir, sourceID: sourceID}
	if err := runner.Start(); err != nil {
		e.procs[sourceID] = &proc{configHash: hash, lastError: err.Error()}
		return fmt.Errorf("启动 mihomo 失败：%w", err)
	}
	if err := runner.VerifyListening(listenVerifyTimeout); err != nil {
		_ = runner.Stop()
		// configHash 置空 → 下一轮不会因「hash 未变」跳过重启。
		e.procs[sourceID] = &proc{lastError: err.Error()}
		return fmt.Errorf("mihomo listener 未就绪：%w", err)
	}
	e.procs[sourceID] = &proc{runner: runner, configHash: hash, running: true}
	log.Printf("[mihomo] source=%s 已就绪：%d 个 listener，binary=%s", sourceID, len(bindings), filepath.Base(bin))
	return nil
}

// StopSource 停一个源的进程。
func (e *Engine) StopSource(sourceID string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if p, ok := e.procs[sourceID]; ok && p != nil && p.runner != nil {
		_ = p.runner.Stop()
		p.running = false
		p.runner = nil
	}
}

// StopAll 停所有进程（进程退出时调用）。
func (e *Engine) StopAll() {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, p := range e.procs {
		if p != nil && p.runner != nil {
			_ = p.runner.Stop()
		}
		delete(e.procs, id)
	}
}

// Status 一个源的运行态（供面板展示）。
type Status struct {
	ID         string `json:"id"`
	Running    bool   `json:"running"`
	ConfigHash string `json:"config_hash"`
	LastError  string `json:"last_error"`
}

// StatusSnapshot 返回各源运行态快照。
func (e *Engine) StatusSnapshot() []Status {
	out := []Status{}
	if e == nil {
		return out
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, p := range e.procs {
		if p == nil {
			continue
		}
		out = append(out, Status{ID: id, Running: p.running, ConfigHash: p.configHash, LastError: p.lastError})
	}
	return out
}

// preflightPorts 逐个试绑 listener 端口，任一失败即返回错误（含占用者提示）。
// 试绑后立刻释放——真正的绑由 mihomo 完成；这里只回答「这个端口现在能不能用」。
func preflightPorts(bindings []Binding) error {
	for _, b := range bindings {
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(b.Port))
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("端口 %d 被占用或不可绑：%w（换个 base_port，或先清掉占用该端口的进程）", b.Port, err)
		}
		_ = ln.Close()
	}
	return nil
}

// isLoopback 报告 bind 地址是否为 loopback（127.0.0.1 / localhost / ::1）。
func isLoopback(host string) bool {
	h := strings.TrimSpace(strings.ToLower(host))
	if h == "" {
		return false
	}
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// pumpLog 把子进程输出逐行转发到标准日志（带 source 前缀）。
// 参考项目直接 r.cmd.Stdout = os.Stdout，多进程时日志混流无法区分。
func pumpLog(r io.ReadCloser, sourceID string, wg *sync.WaitGroup) {
	defer wg.Done()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 256*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r\n")
		if line != "" {
			log.Printf("[mihomo %s] %s", sourceID, line)
		}
	}
}
