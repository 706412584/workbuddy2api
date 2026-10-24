// main.go workbuddy2api 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"workbuddy2api/internal/admin"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/groupreg"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/proxyreg"
	"workbuddy2api/internal/redisstore"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/updater"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/version"
	"workbuddy2api/internal/web"
)

// proxyRegistry 出站代理槽位与账号绑定的运行期注册表。
// 包级变量是因为 admin 面板要在运行期读写它（热重载），而 admin 的 Config 是值传递的
// 函数注入缝，不便携带状态；与本仓库既有做法（degradeGate 等进程级状态）一致。
var proxyRegistry *proxyreg.Registry

// sidecar mihomo sidecar 编排器（订阅 → 出口端口 → 槽位）。未启用时为 nil。
var sidecar *Sidecar

// absFromExe 把相对路径按**可执行文件所在目录**解析为绝对路径。
//
// 为什么不用相对 CWD：Windows 上双击 exe / 计划任务启动时 CWD 可能是
// C:\Windows\System32，相对路径会落到意想不到的地方（sidecar 配置、mihomo.exe
// 的查找都受影响）。注入到 Sidecar 便于测试替换。
func absFromExe(p string) string {
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		return p
	}
	exe, err := os.Executable()
	if err != nil {
		return p
	}
	return filepath.Join(filepath.Dir(exe), p)
}

// slotsToRegistry 把 config 的槽位定义转成注册表类型。
// 两种类型分开是刻意的：config 侧带 json tag 供配置文件读写，注册表侧是纯运行期模型，
// 各自演进互不影响（避免改注册表内部结构时牵动配置文件格式）。
func slotsToRegistry(in []ProxySlot) []proxyreg.Slot {
	out := make([]proxyreg.Slot, 0, len(in))
	for _, s := range in {
		out = append(out, proxyreg.Slot{ID: s.ID, Name: s.Name, URL: s.URL, Enabled: s.Enabled})
	}
	return out
}

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	// -version：不进主流程，直接报版本退出。发布包/容器里想确认"跑的是哪版"时，
	// 比翻面板或看日志都快，也让「检查更新」的版本比较有个可直接核对的来源。
	if *showVersion {
		fmt.Println(version.String())
		return
	}

	cfg, err := Load(*cfgPath)
	if err != nil {
		// 配置文件不存在时给一次机会用纯默认 + env
		if os.IsNotExist(err) {
			log.Printf("config %s not found, using defaults+env", *cfgPath)
			cfg, err = Load("")
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)

	p := pool.New(cfg.StateFile)
	defer p.Flush() // 进程退出前强制落盘（后台 flush 每 5s 一次，退出时补一次）
	p.SetStore(store)
	p.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用，否则本地优先
	p.SyncToDir(auths)      // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）

	// 熔断器 + 在途上限 + 三因子加权调优（从 config 注入，非正值回退默认）。
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetSoftRateMax(cfg.SoftRateMaxDur) // 软冷却指数退避封顶（soft_rate_max，默认 2h）
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)

	// 会话粘性路由（可配关闭）。
	var sessRouter *session.Router
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	if cfg.SessionSticky.Enabled {
		sessRouter = session.New(session.Config{
			TTL:        cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store:      store,
			Available:  p.AvailableUIDs,
			// 按模型的可用性口径：绑定号在当前模型被 6004 限额时重分配，
			// 而不是被钉在这个号上反复失败。
			AvailableForModel: p.AvailableUIDsForModel,
		})
		sessRouter.LoadFromStore() // 启动时从 Redis 恢复粘性（读操作仅此处）
		sessRouter.StartGC()
		defer sessRouter.StopGC()
	}
	sessCount := func() int {
		if sessRouter != nil {
			return sessRouter.Count()
		}
		return 0
	}

	up := upstream.New()
	// 出站代理：把 config 的槽位/绑定装进运行期注册表，并接线到 upstream 的解析缝。
	// 解析函数每次出站都调用 → 面板改绑定后无需重启（配合 ResetProxyTransportCache）。
	proxyRegistry = proxyreg.New(slotsToRegistry(cfg.ProxySlots), cfg.AccountProxies)
	up.SetProxyResolver(func(a *auth.Auth) string {
		if a == nil {
			return ""
		}
		return proxyRegistry.Resolve(a.UID)
	})
	// 账号调用历史落盘（只读诊断）：不配就不落盘（仅内存）。
	if cfg.CallLogDir != "" {
		keep := cfg.CallLogKeepDays
		if keep == 0 {
			keep = 7
		}
		p.EnableCallLog(absFromExe(cfg.CallLogDir), keep)
		defer p.CloseCallLog()
		log.Printf("[calllog] 账号调用历史落盘已启用：dir=%s keepDays=%d", cfg.CallLogDir, keep)
	}

	// 账号分组注册表（uid → 分组）。空配置 = 全部未分组 → 不过滤（行为不变）。
	// 空组名一并注入：面板可以「先建组、再放号」。
	groupsReg := groupreg.New(cfg.AccountGroups)
	groupsReg.SetExtra(cfg.AccountGroupNames)
	server.SetGroups(groupsReg)

	// mihomo sidecar（订阅 → N 个本地出口端口 → 槽位）：启用时起子进程并自动同步。
	// 未启用（enabled=false 或未填订阅）时完全不启动，行为零变化。
	//
	// **启动时只同步、不落盘**：槽位写入 config.json 由 admin 端点负责（那里有
	// persistConfigEdits 的备份+一致性断言机制）。这里落盘过一版，结果启动路径
	// 把账号绑定写进去了、槽位却没写 —— 绑定指向不存在的槽位。故启动路径只保证
	// 「mihomo 起着、槽位在内存里」，持久化交给面板首次点「同步」。
	sidecar = NewSidecar(cfg.ProxySidecar, cfg.StateFile, absFromExe)
	if sidecar.Enabled() {
		if _, err := sidecar.Sync(); err != nil {
			// 首次同步失败不致命：面板可重试，也可走「粘贴订阅」路径绕过。
			log.Printf("[sidecar] 首次同步失败（可在面板重试）：%v", err)
		} else {
			log.Printf("[sidecar] 已就绪（槽位尚未落盘；到面板点一次「同步订阅」即写入 config）")
		}
		sidecar.StartAutoSync()
		defer sidecar.Stop()
	}
	// 短 RPC 总时长上限（refresh/checkin/balance/FetchModels），语义不变。
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// 聊天 SSE 首字节前（响应头）上限：cfg 已 normalize（缺省回落 timeout_seconds）。
	up.HeaderTimeout = time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second
	if tr, ok := up.ChatHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = up.HeaderTimeout
	}
	// 聊天 SSE 流中空闲上限（S3 空闲监控读取）。
	up.IdleTimeout = time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second
	up.SanitizeFingerprints = cfg.Features.SanitizeBlacklistFingerprints
	// 出站 UA（A 段）：非空才做显式覆盖，空 = 默认 WorkBuddy 三段式
	// `WorkBuddy/<client_version> WorkBuddy/<client_version> CLI/<cli_version>`。
	up.UserAgent = cfg.Upstream.UserAgent
	// 版本段（upstream.client_version / cli_version）：空 = 各走内置默认。
	up.ClientVersion = cfg.Upstream.ClientVersion
	up.CliVersion = cfg.Upstream.CliVersion
	// 设备风控头（X-Device-Token）全局兜底 + 文件读取路径；空 = 不注入。
	up.DeviceToken = cfg.Upstream.DeviceToken
	up.DeviceTokenFile = cfg.Upstream.DeviceTokenFile
	// 用量归属头（X-Product/X-IDE-*）+ 客户端 IP 透传开关（见 ChatHeaders / handler）。
	up.ClientName = cfg.Upstream.ClientName
	up.PassthroughIP = cfg.Upstream.PassthroughIP

	sch := scheduler.New(scheduler.Config{
		Pool:                p,
		Upstream:            up,
		CheckinHours:        cfg.Schedule.CheckinHours,
		TravelHours:         cfg.Schedule.TravelHours,
		ActivityHours:       cfg.Schedule.ActivityHours,
		KeepaliveHours:      cfg.Schedule.KeepaliveHours,
		ActivityReportCount: cfg.Schedule.ActivityReportCount,
		CheckinDisabled:     !cfg.Schedule.CheckinEnabled,
		TravelDisabled:      !cfg.Schedule.TravelEnabled,
		ActivityDisabled:    !cfg.Schedule.ActivityEnabled,
		KeepaliveDisabled:   !cfg.Schedule.KeepaliveEnabled,
	})
	switch {
	case !cfg.Schedule.CheckinEnabled:
		log.Printf("签到已禁用（schedule.checkin_enabled=false）")
	default:
		log.Printf("签到已启用：%v 点（签到 + 余额查询解冻）", cfg.Schedule.CheckinHours)
	}
	switch {
	case !cfg.Schedule.TravelEnabled:
		log.Printf("猫猫旅行已禁用（schedule.travel_enabled=false）")
	default:
		log.Printf("猫猫旅行已启用：%v 点（独立排程：领养 / 派出 / 领奖）", cfg.Schedule.TravelHours)
	}
	switch {
	case !cfg.Schedule.ActivityEnabled:
		log.Printf("活跃上报已禁用（schedule.activity_enabled=false）")
	default:
		log.Printf("活跃上报已启用：%v 点（每号 %d 条，点亮连登 + 补满领猫对话门槛）", cfg.Schedule.ActivityHours, cfg.Schedule.ActivityReportCount)
	}
	if !cfg.Schedule.KeepaliveEnabled {
		log.Printf("token 保活已禁用（schedule.keepalive_enabled=false）")
	} else {
		log.Printf("token 保活已启用：%v 点", cfg.Schedule.KeepaliveHours)
	}

	// 管理面板：账号/密钥/日志/统计都在网关进程内处理，不再需要一个另跑的 dev server。
	// 路径一律转绝对：面板要把它们显示给用户，相对路径在界面上没有意义。
	repoRoot, err := os.Getwd()
	if err != nil {
		log.Fatalf("resolve working directory: %v", err)
	}
	abs := func(p string) string {
		a, err := filepath.Abs(p)
		if err != nil {
			return p
		}
		return a
	}

	// restartCh 更新重启请求通道：admin 的更新接口发信号，serveWithHandoff 收到后
	// 做零停机交接（见 handoff.go）。容量 1 即可 —— 交接期间不会有第二次请求。
	restartCh := make(chan string, 1)
	// shutdownCh 远程优雅停机请求通道：admin 的 /system/shutdown 发信号，
	// serveWithHandoff 收到后走与 SIGINT/SIGTERM 相同的收场（落盘 + Shutdown）。
	// 让 stop 脚本能优雅停机，不必 taskkill /F 硬切。容量 1 同理。
	shutdownCh := make(chan string, 1)

	// h 先声明后赋值：admin 的新增回调要引用它，而它又需要 admin 才能构造。
	var h *server.Handler
	adm := admin.New(admin.Config{
		Pool:       p,
		Upstream:   up,
		AuthDir:    abs(cfg.AuthDir),
		ConfigPath: abs(*cfgPath),
		LogPath:    abs(filepath.Join("data", "gateway.log")),
		LoginBin:   abs(loginBin()),
		RepoRoot:   repoRoot,
		// 空 = 管理面板仅本机可访问（默认）；非空则允许局域网访问并校验该口令。
		Token: cfg.Admin.Token,
		// 账号热重载：与启动时同一套动作，改完即时生效（旧版靠重启进程）
		ReloadAccounts: func() (int, error) {
			auths, err := auth.LoadDir(cfg.AuthDir)
			if err != nil {
				return 0, err
			}
			p.SyncToDir(auths)
			p.Flush()
			return len(auths), nil
		},
		// 密钥热重载：换掉 handler 里的密钥表（旧版靠重启进程）
		ReloadKeys: func(legacy string, keys []admin.KeyEntry) {
			in := make([]APIKeySpec, 0, len(keys))
			for _, k := range keys {
				in = append(in, APIKeySpec{Key: k.Key, Region: k.Region, Name: k.Name})
			}
			h.SetKeys(legacy, serverAPIKeys(in))
		},
		Registry:             proxyRegistry,
		ResetProxyTransports: upstream.ResetProxyTransportCache,
		// 出口健康查询（槽位列表显示「该出口是否可用 / 失败次数 / 冷却剩余」）。
		ProxyHealth: func(rawURL string) (admin.ProxyHealthInfo, bool) {
			hp, ok := upstream.ProxyHealthByURL(rawURL)
			return admin.ProxyHealthInfo{
				Unhealthy: hp.Unhealthy, Fails: hp.Fails,
				LastErr: hp.LastErr, CooldownMs: hp.CooldownMs,
				ProbedAt: hp.ProbedAt,
			}, ok
		},
		// 面板「测试」的结论写回同一本健康账，让槽位健康列反映手动探测结果
		// （否则从没被用过的坏槽位会一直显示「正常」）。
		ProxyNoteResult: func(rawURL string, ok bool, errMsg string) {
			upstream.NoteProxyResult(rawURL, ok, errMsg)
		},
		// mihomo sidecar 端点（状态/同步/分配）；未启用时为 nil → 端点返回未接线。
		Sidecar: sidecarController(sidecar, p),
		// 账号分组（读/保存/批量设置）。nil → 端点返回未接线。
		Groups: groupsAdapter{},
		ModelsByRegion: func() (cn, global []string) {
			return h.ModelsForRegion(auth.RegionCN), h.ModelsForRegion(auth.RegionGlobal)
		},
		ModelDetailsByRegion: func() (cn, global []map[string]any) {
			return h.ModelDetailsForRegion(auth.RegionCN), h.ModelDetailsForRegion(auth.RegionGlobal)
		},
		// 立即执行一次定时任务（面板「执行」按钮）。阻塞到跑完，由 admin 放在
		// 后台 goroutine 里调用并把「执行中／结果」透给面板。
		//
		// 任务被禁用（schedule.*_enabled=false）时这里照样执行：按钮是一次性显式
		// 动作，与「要不要按排程自动跑」是两回事。
		RunTask: func(key string) string {
			switch key {
			case "checkin":
				return sch.RunCheckinNow()
			case "travel":
				return sch.RunTravelNow()
			case "activity":
				return sch.RunActivityNow()
			case "keepalive":
				return sch.RunKeepaliveNow()
			}
			return "未知任务：" + key
		},
		// 更新：检查本仓库 GitHub Releases（见 internal/updater），安装后经 restartCh
		// 触发零停机交接。Updater 为 nil（未接线）时相关接口返回明确错误。
		//
		// 代理显式注入（config 的 update.proxy），不靠环境变量：启动脚本会清空
		// HTTP(S)_PROXY 以避免残留代理影响 login，而 GitHub 直连不通的机器上
		// 自更新会卡在下载超时（实测 2026-10-15，见 internal/updater/proxy.go）。
		Updater: newUpdater(cfg),
		// RequestRestart 非阻塞地投递一次交接请求。交接是异步的：接口先返回，
		// 让用户看到「已更新，正在交接」，再真正重启。
		RequestRestart: func(reason string) {
			select {
			case restartCh <- reason:
			default:
				// 已有一个待处理请求：不排队（重复交接没有意义），如实记录。
				log.Printf("更新重启请求已在处理中，忽略重复请求：%s", reason)
			}
		},
		// RequestShutdown 非阻塞地投递一次优雅停机请求（stop 脚本用）。
		RequestShutdown: func(reason string) {
			select {
			case shutdownCh <- reason:
			default:
				log.Printf("停机请求已在处理中，忽略重复请求：%s", reason)
			}
		},
	})

	h = server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     up,
		APIKey:       cfg.APIKey,
		APIKeys:      serverAPIKeys(cfg.APIKeys),
		Protocol:     serverProtocol(cfg.Protocol),
		Session:      sessRouter,
		StickyCount:  sessCount,
		RedisMode:    redisMode,
		SoftCooldown: cfg.SoftRateDur,
		PromptMode:   cfg.Prompt.Mode,
		PromptText:   cfg.PromptText,
		MaxBodyBytes: int64(cfg.Server.MaxBodyMB) << 20, // MB → 字节
		Web:          web.Handler(),
		Admin:        adm,
		// 思考死循环处置（默认开启）：空转请求会被切断、追加提示后换号重试，
		// 2 次仍不行则回明确错误。主判据是「连续 2000 块（48KB）没有新内容」，
		// 配置项为 0 时 toLoopGuard 回落到内置默认，所以 Enabled 恒定 true。
		LoopGuard: server.LoopGuardConfig{
			Enabled:             true,
			MinThinkChars:       cfg.Server.LoopGuardMinThinkChars,
			MaxDistinctRatio:    cfg.Server.LoopGuardMaxDistinctRatio,
			MinElapsedSeconds:   cfg.Server.LoopGuardMinElapsedSeconds,
			MaxRetries:          cfg.Server.LoopGuardMaxRetries,
			MaxStaleChunks:      cfg.Server.LoopGuardMaxStaleChunks,
			MaxThinkOnlySeconds: cfg.Server.LoopGuardMaxThinkOnlySeconds,
		},
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)

	// 监听：普通启动 bind 配置地址；交接启动则接管旧进程继承来的套接字
	// （端口全程无"无监听者"空窗，见 handoff.go）。
	ln, inherited, err := listenForServe(cfg.Listen)
	if err != nil {
		log.Fatalf("listen %s: %v", cfg.Listen, err)
	}

	// 句柄复制必须在 Accept 之前完成（Windows 限制，见 handoff_windows.go）。
	// 因此顺序是：先 dup，再起 Serve。dup 失败不致命 —— 服务照常跑，只是本次
	// 进程无法做零停机交接，更新接口会明确报错而不是假装成功。
	//
	// 交接启动的新进程同样是"监听持有者"，也要能再交接给下一次更新，故两条路径一致。
	hs := handoffState{ok: false, why: "未初始化"}
	if f, err := dupListener(ln); err != nil {
		hs.why = err.Error()
		log.Printf("WARN: 无法复制监听句柄，本进程不支持零停机更新（更新后需手工重启）：%v", err)
	} else {
		hs = handoffState{file: f, ok: true}
	}

	// 连接跟踪器：交接排空靠它数在途请求（不能用 Shutdown，见 handoff.go 约束）。
	tracker := newConnTracker()
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		ConnState:         tracker.state,
	}
	registerTracker(srv, tracker)

	log.Printf("workbuddy2api %s listening on %s", version.String(), cfg.Listen)
	if inherited {
		log.Printf("本次为交接启动：已接管旧进程的监听套接字")
	}
	log.Printf("api keys: %s", describeAPIKeys(cfg))

	handedOff := serveWithHandoff(ctx, ln, srv, p, restartCh, shutdownCh, hs)
	if handedOff {
		log.Printf("bye（已交接给新进程）")
		return
	}
	log.Printf("bye")
}

// loginBin 设备码登录工具的文件名。
// 管理面板的「添加账号」调它，按 login.sh / login.exe 的命名约定拼。
func loginBin() string {
	if runtime.GOOS == "windows" {
		return "login.exe"
	}
	return "login"
}

// newUpdater 构造自更新客户端并注入代理配置。
//
// 代理走显式配置（config 的 update.proxy / update.no_proxy）而非环境变量：
// 启动脚本会清空 HTTP(S)_PROXY 以避免残留代理让 login 失败，靠 env 就等于在
// GitHub 直连不通的机器上自更新必然失败（见 internal/updater/proxy.go）。
// 未配置时不打印任何代理日志——默认直连是绝大多数部署形态。
func newUpdater(cfg *Config) *updater.Client {
	c := updater.New()
	c.Proxy = cfg.Update.Proxy
	c.NoProxy = cfg.Update.NoProxy
	if c.Proxy != "" {
		log.Printf("[updater] 出站代理：%s（no_proxy=%q）", sanitizeProxyForLog(c.Proxy), c.NoProxy)
	}
	return c
}

// sanitizeProxyForLog 抹掉代理 URL 里的凭据再打日志（形如 http://user:pass@h:p）。
func sanitizeProxyForLog(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "<非法 URL>"
	}
	if u.User != nil {
		u.User = nil
	}
	return u.String()
}

// serverAPIKeys 把配置里的密钥列表转成 server 包的规范类型。
// 区域字符串已由 validateAPIKeys 归一化，此处只做类型转换。
func serverAPIKeys(in []APIKeySpec) []server.APIKeySpec {
	if len(in) == 0 {
		return nil
	}
	out := make([]server.APIKeySpec, 0, len(in))
	for _, k := range in {
		out = append(out, server.APIKeySpec{Key: k.Key, Region: auth.Region(k.Region), Name: k.Name, Group: k.Group})
	}
	return out
}

// serverProtocol 把配置里的协议适配段转成 server 包的规范类型。
func serverProtocol(in ProtocolConfig) server.ProtocolConfig {
	return server.ProtocolConfig{
		DefaultModel: in.DefaultModel,
		ModelMapping: in.ModelMapping,
	}
}

// describeAPIKeys 汇总密钥配置供启动日志排查（只报数量与区域，绝不打印密钥本身）。
func describeAPIKeys(cfg *Config) string {
	n := len(cfg.APIKeys)
	if cfg.APIKey != "" {
		n++
	}
	if n == 0 {
		return "未配置（不鉴权）"
	}
	desc := make([]string, 0, n)
	if cfg.APIKey != "" {
		desc = append(desc, "legacy=不限区域")
	}
	for _, k := range cfg.APIKeys {
		region := k.Region
		if region == "" {
			region = "不限区域"
		}
		name := k.Name
		if name == "" {
			name = "-"
		}
		desc = append(desc, name+"="+region)
	}
	return fmt.Sprintf("%d 个 [%s]", n, strings.Join(desc, ", "))
}
