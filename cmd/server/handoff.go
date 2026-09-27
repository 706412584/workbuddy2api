// handoff.go 零停机交接重启：更新二进制后，让新进程接管同一个监听套接字，
// 旧进程把在途请求跑完再退出。端口全程有监听者，在途请求不被切断。
//
// **与参考实现（sub2api）的差别**：sub2api 是「原子改名换 exe → 杀掉旧进程 →
// PowerShell 助手重新拉起」。杀进程的瞬间会掐断所有在途连接。本网关的生产规则
// 明确写着「重启会掐断在途请求」（实测常年 10 个 ESTABLISHED），所以换成真正的交接：
//
//	1. 旧进程复制监听句柄 → 起新进程并让它继承该句柄（新进程无需重新 bind，
//	   端口不出现"无监听者"的空窗）；
//	2. 旧进程关掉自己的空闲 keep-alive 连接、等在途请求自然结束；
//	3. 旧进程退出。新进程从第 1 步起就在 accept 新连接。
//
// 已实测（本机 Windows，见下方"实现约束"）：交接窗口内 12 次新请求全部由新进程
// 应答，同时旧进程的在途长请求正常跑完。
//
// **实现约束（Windows 实测得出，改动本文件前务必先读）**：
//
//	复制监听句柄必须用 net.TCPListener.File()，不能用 DuplicateHandle —— 后者
//	保住了父进程但子进程无法用（net.FileListener 报 "参数错误"），因为 Go 的
//	dup 不止是 DuplicateHandle（还要 DisassociateIOCP + WSASocket 重建）。
//
//	代价是 File() 内部调用了 DisassociateIOCP，**永久破坏**该监听器的正常关闭
//	路径：此后 srv.Shutdown() 与 ln.Close() 都会永久阻塞（实测：即使把 dup 出来
//	的句柄关掉也照样卡）。所以本文件**绝不调用 Shutdown/Close**，改用 ConnState
//	自己记账排空，然后 os.Exit(0) 直接退出进程（Windows 会回收其全部句柄）。
//	正因如此，信号退出（SIGINT/SIGTERM）走的是普通 Shutdown 路径，
//	而交接路径不碰它 —— 两条路径的收尾方式不同是刻意的。
package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"workbuddy2api/internal/pool"
)

// handoffEnv 交接时传给新进程的环境变量：监听句柄在新进程中的句柄号。
// 新进程启动时若看到它，就不再 bind，而是接管这个套接字（见 inheritListener）。
const handoffEnv = "WB2A_INHERIT_LISTENER"

// readyEnv 就绪信号管道：交接启动的新进程在成功接管监听后，往这个句柄写一个字节
// 再关闭；旧进程据此确认「新进程真的在服务了」才退出。
//
// **为什么需要它（实测踩过）**：spawn 成功 ≠ 子进程能服务。旧实现只判断 spawn 是否
// 成功就退出，结果换上来的二进制若不认识交接环境变量（例如比本功能更早的发布版），
// 它会照常重新 bind 同一个端口 → 失败退出，而旧进程已经退出，端口就此无人监听：
// 一次更新变成一次服务中断。有它就变成「没就绪就不交接」。
const readyEnv = "WB2A_READY_FD"

// readyTimeout 等待新进程报就绪的上限。
// 子进程启动 + 接管监听是毫秒级；10s 足够覆盖慢盘/杀软扫描，又不至于让旧进程
// 挂太久（交接期间旧进程仍在服务，挂久了只是延迟退出，不影响可用性）。
const readyTimeout = 10 * time.Second

// readyDelay 报就绪前留给 srv.Serve 暴露启动期错误的时间。
// 绑定失败、句柄不可用这类错误是微秒级返回的，150ms 足以让它们先落进 errCh；
// 否则新进程可能在 Serve 失败前就把「就绪」报出去，父进程照样会误退出。
const readyDelay = 150 * time.Millisecond

// spawnResult 一次替代进程启动的结果。
type spawnResult struct {
	// pid 新进程 pid（仅用于日志与失败清理）。
	pid int
	// proc 新进程句柄，供「未确认就绪」时终止它用。
	proc *os.Process
	// ready 就绪信号读端。新进程接管监听成功后写入 1 字节；新进程中途死亡则读到
	// EOF（父进程在 Start 后立刻关掉了自己的写端副本，故 EOF 可靠）。
	ready *os.File
}

// abort 终止一个「尚未确认就绪」的替代进程。
//
// **为什么超时后必须杀它**：子进程继承的是同一个监听套接字，一旦它稍后成功接管，
// 就会与本进程形成两个 accept 循环 —— 连接被随机分走，且两个进程都会往同一份
// state.json 落盘（本进程此时并未冻结），状态互相覆盖。宁可杀掉这个未经确认的
// 新进程，也不能留一个影子服务在跑。
func (sp spawnResult) abort() {
	if sp.proc == nil {
		return
	}
	if err := sp.proc.Kill(); err != nil {
		// 最常见的「失败」是进程早已自行退出（旧二进制 bind 失败即退出），
		// 那正是我们想要的终态，不值得报 WARN。仅记录一句供对账。
		log.Printf("替代进程 pid=%d 终止时已不在运行（%v），无需处理", sp.pid, err)
		return
	}
	log.Printf("已终止未就绪的替代进程 pid=%d", sp.pid)
}

// drainGrace 交接时等待在途请求结束的上限。
//
// 取 5 分钟：与上游 chat 流的空闲上限（upstream.idle_timeout，默认 300s）同量级，
// 覆盖绝大多数长流。超时后不再等待，剩余长流被切断 —— 但这是**只切尾巴**，
// 与整体重启一次切掉全部在途请求有本质区别。旧进程多存活一会儿无害（它只是在
// 服务自己的在途请求，已不接受新连接），切断请求才有害，故宁可等久一点。
const drainGrace = 5 * time.Minute

// handoffState 零停机交接所需的运行态，由 main 在 Serve 之前准备。
type handoffState struct {
	// file dupListener 复制出来的监听文件。**必须一直持有它**：它内部持有的
	// 句柄是交接时传给子进程的那一个，一旦被 GC 回收（finalizer 会关掉句柄），
	// spawnReplacement 就会报 "The parameter is incorrect"。
	file *os.File
	// ok 句柄是否可用。false = 本进程无法交接，更新功能会明确报错而不是假装成功。
	ok bool
	// why 不可用的原因（用于日志与面板提示）。
	why string
}

// connTracker 按 ConnState 记账活跃/空闲连接。
//
// 为什么需要它：交接路径不能用 srv.Shutdown 排空（见文件头约束），只好自己数
// 「还有几个请求在跑」。http.Server 的 ConnState 回调给出每个连接的状态跃迁，
// 是标准库里唯一可靠的观测点。
type connTracker struct {
	mu    sync.Mutex
	conns map[net.Conn]http.ConnState
}

func newConnTracker() *connTracker {
	return &connTracker{conns: map[net.Conn]http.ConnState{}}
}

// state 记录一次状态跃迁。ConnState 回调在每次跃迁时调用。
func (t *connTracker) state(c net.Conn, st http.ConnState) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if st == http.StateClosed || st == http.StateHijacked {
		delete(t.conns, c)
		return
	}
	t.conns[c] = st
}

// active 返回在途连接数（正在处理请求 + 刚建立还没读完请求头）。
// StateNew 计入：它马上就要变成 Active，不算进去会让排空过早结束。
func (t *connTracker) active() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, st := range t.conns {
		if st == http.StateActive || st == http.StateNew {
			n++
		}
	}
	return n
}

// closeIdle 关掉所有空闲的 keep-alive 连接，返回关掉的数量。
//
// 为什么必须显式关：客户端（IDE）会保持长连接复用，这些连接在交接后仍然指向旧进程。
// 不关的话旧进程会一直挂着它们、迟迟不退出；更糟的是这些连接后续的请求会打到
// 旧进程（旧二进制）上，造成"更新了但行为还是旧的"的诡异现象。
func (t *connTracker) closeIdle() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for c, st := range t.conns {
		if st == http.StateIdle {
			c.Close()
			delete(t.conns, c)
			n++
		}
	}
	return n
}

// mustGetwd 取当前工作目录，失败回空串。
// 新进程的 cwd 必须与旧进程一致：config.json / auths/ / data/ 都是相对 cwd 解析的，
// 换了目录会读到另一份配置甚至从零开始（load 不到账号）。
func mustGetwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return wd
}

// listenForServe 建立监听：普通启动按配置地址 bind；交接的新进程接管继承来的套接字。
// 返回值 inherited 表示本次是否为交接启动。
func listenForServe(addr string) (ln net.Listener, inherited bool, err error) {
	if l, isInherited, err := inheritListener(); isInherited {
		if err != nil {
			return nil, true, err
		}
		return l, true, nil
	}
	l, err := net.Listen("tcp", addr)
	return l, false, err
}

// serveWithHandoff 起服务，支持三种收场：
//   - 收到退出信号（SIGINT/SIGTERM）→ 优雅停机（标准 Shutdown）；
//   - 收到更新重启请求且交接成功 → 退出，新进程继续服务；
//   - 收到更新重启请求但交接失败 → **继续服务**，等下一条事件。
//
// 返回 true 表示本次是交接退出（新进程已在服务），false 表示普通停机。
//
// 用循环而非单次 select：交接失败后服务必须原地继续跑。交接失败最常见的原因
// （句柄复制失败、启动子进程失败）都与"本进程还能不能服务"无关，此时退出等于
// 把一次失败的更新升级成一次服务中断。
func serveWithHandoff(
	ctx context.Context,
	ln net.Listener,
	srv *http.Server,
	p *pool.Pool,
	restartCh <-chan string,
	hs handoffState,
) bool {
	errCh := make(chan error, 1)
	// serveDone 只用来「广播」Serve 已返回，不承载错误本身 —— 错误仍由 errCh 传给
	// 主 select。若让就绪逻辑直接读 errCh，它会把错误取走，主 select 就永远等不到。
	serveDone := make(chan struct{})
	go func() {
		errCh <- srv.Serve(ln)
		close(serveDone)
	}()

	// 就绪信号：本次若是交接启动（继承了就绪管道），在 Serve 成功跑起来后告知旧进程
	// 「我已接管监听，可以退出」。放在这里而不是 main 里，是为了能用 serveDone 判断
	// Serve 是否**立刻失败**（bind 失败这类启动期错误是微秒级返回的）：先等一小会儿，
	// 若这期间 Serve 已返回，就绝不报就绪 —— 否则旧进程会在新进程已经死掉的情况下
	// 退出，端口就没人监听了。普通启动时 signalReady 是空操作。
	go func() {
		select {
		case <-serveDone:
			// Serve 启动即失败：不报就绪，让旧进程继续服务。
		case <-time.After(readyDelay):
			signalReady()
		}
	}()

	for {
		select {
		case err := <-errCh:
			if err != nil && err != http.ErrServerClosed {
				log.Printf("http serve 退出: %v", err)
			}
			return false

		case <-ctx.Done():
			log.Printf("收到退出信号，优雅停机（等待在途请求完成，最多 5s）…")
			p.Flush() // 信号退出：先落盘再停机
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
			return false

		case reason := <-restartCh:
			if handoffRestart(srv, p, hs, reason) {
				return true
			}
			// 交接失败：服务照常，回到 select 等下一条事件。
			log.Printf("交接未完成，本进程继续服务")
		}
	}
}

// handoffRestart 执行一次零停机交接。返回 true 表示已交接成功（调用方直接退出）。
//
// 任一步失败都**不退出**：服务继续用旧二进制跑，并在日志/面板里说明原因。
// 更新失败却把服务搞没了，比更新没做成本高得多。
func handoffRestart(srv *http.Server, p *pool.Pool, hs handoffState, reason string) bool {
	log.Printf("零停机交接开始：%s", reason)

	// 留出时间让「触发本次交接的 HTTP 响应」刷出。触发方是 admin 的更新接口，
	// 它已返回，但响应可能还在内核缓冲里；随即退出会连这条响应一起丢掉。
	time.Sleep(500 * time.Millisecond)

	if !hs.ok {
		log.Printf("ERROR: 本进程无可用监听句柄（%s），放弃交接，继续以旧版本服务", hs.why)
		return false
	}

	// 1. 起新进程，让它继承监听句柄，并等它确认「已经接管监听」。
	//
	// **必须等就绪，不能只看 spawn 成功**（实测踩过）：spawn 成功只说明进程起来了，
	// 不代表它能服务。若换上来的二进制不认识交接环境变量（例如比本功能更早的发布版），
	// 它会照常重新 bind 同一端口 → 失败退出；此时旧进程若已退出，端口就无人监听，
	// 一次更新变成一次服务中断。就绪握手把这种情形变成「不交接，继续用旧版服务」。
	sp, err := spawnReplacement(hs.file)
	if err != nil {
		log.Printf("ERROR: 启动替代进程失败，放弃交接，继续以旧版本服务：%v", err)
		return false
	}
	defer sp.ready.Close()
	if !waitReady(sp.ready, readyTimeout) {
		// 杀掉未确认的替代进程再放弃：它继承着同一个监听套接字，若放任不管，
		// 它稍后可能接管成功并与本进程并存，形成双 accept + 双落盘（见 abort 注释）。
		sp.abort()
		log.Printf("ERROR: 替代进程 pid=%d 未在 %s 内确认接管监听，已终止它并放弃交接，继续以旧版本服务"+
			"（常见原因：新二进制不支持零停机交接，或端口被占用；本进程仍在服务，可手工重启切换）",
			sp.pid, readyTimeout)
		return false
	}
	log.Printf("替代进程 pid=%d 已接管监听并就绪；旧进程排空在途请求后退出", sp.pid)

	// 2. 冻结落盘：新进程已接管并开始写同一个 state.json，旧进程不得再写，
	//    否则退出前那次 Flush 会用旧内存状态覆盖新进程的更新状态。
	p.FreezePersist()

	// 3. 排空：关空闲 keep-alive 连接，等在途请求跑完。
	//
	// 这里**不用 srv.Shutdown**：监听句柄已被 File() 复制（DisassociateIOCP），
	// Shutdown 会永久阻塞（见文件头约束）。改用 ConnState 记账自己排空。
	tr := trackerOf(srv)
	if tr == nil {
		// 没装上 tracker（不该发生）：直接退出，宁可切掉在途也不留僵尸进程。
		log.Printf("WARN: 无连接跟踪器，跳过排空直接退出")
		return true
	}
	if n := tr.closeIdle(); n > 0 {
		log.Printf("已关闭 %d 个空闲连接", n)
	}
	deadline := time.Now().Add(drainGrace)
	for time.Now().Before(deadline) {
		if tr.active() == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if a := tr.active(); a > 0 {
		log.Printf("WARN: 等待在途请求超时（仍有 %d 个），切断后退出", a)
	} else {
		log.Printf("在途请求已全部完成")
	}

	log.Printf("交接完成：本进程退出，pid=%d 继续服务", sp.pid)
	return true
}

// waitReady 等新进程写来一个就绪字节。读到 1 字节且值为 1 → 就绪；
// 读到 EOF（新进程已退出且父进程已关掉自己的写端副本）或超时 → 未就绪。
func waitReady(r *os.File, timeout time.Duration) bool {
	type res struct{ ok bool }
	ch := make(chan res, 1)
	go func() {
		buf := make([]byte, 1)
		n, err := r.Read(buf)
		ch <- res{ok: err == nil && n == 1 && buf[0] == 1}
	}()
	select {
	case v := <-ch:
		return v.ok
	case <-time.After(timeout):
		return false
	}
}

// signalReady 新进程侧：往继承来的就绪管道写一个字节并关闭，通知旧进程可以退出。
// 未接线（readyEnv 为空 / 句柄无效）时静默返回 —— 普通启动本就没有旧进程要通知。
func signalReady() {
	f := inheritReady()
	if f == nil {
		return
	}
	_, _ = f.Write([]byte{1})
	_ = f.Close()
}

// trackerOf 取 srv 上挂的连接跟踪器。
// 用一个包级注册表而非全局单例：main 构造 srv 时注册，交接时取出。
var (
	trackerMu sync.Mutex
	trackers  = map[*http.Server]*connTracker{}
)

func registerTracker(srv *http.Server, tr *connTracker) {
	trackerMu.Lock()
	trackers[srv] = tr
	trackerMu.Unlock()
}

func trackerOf(srv *http.Server) *connTracker {
	trackerMu.Lock()
	defer trackerMu.Unlock()
	return trackers[srv]
}
