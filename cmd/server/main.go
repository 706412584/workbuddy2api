// main.go workbuddy2api 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"workbuddy2api/internal/admin"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/redisstore"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/web"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

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
		ModelsByRegion: func() (cn, global []string) {
			return h.ModelsForRegion(auth.RegionCN), h.ModelsForRegion(auth.RegionGlobal)
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
			Enabled:           true,
			MinThinkChars:     cfg.Server.LoopGuardMinThinkChars,
			MaxDistinctRatio:  cfg.Server.LoopGuardMaxDistinctRatio,
			MinElapsedSeconds: cfg.Server.LoopGuardMinElapsedSeconds,
			MaxRetries:        cfg.Server.LoopGuardMaxRetries,
			MaxStaleChunks:    cfg.Server.LoopGuardMaxStaleChunks,
		},
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		p.Flush() // 信号触发：先落盘再做优雅停机
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("workbuddy2api listening on %s", cfg.Listen)
	log.Printf("api keys: %s", describeAPIKeys(cfg))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
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

// serverAPIKeys 把配置里的密钥列表转成 server 包的规范类型。
// 区域字符串已由 validateAPIKeys 归一化，此处只做类型转换。
func serverAPIKeys(in []APIKeySpec) []server.APIKeySpec {
	if len(in) == 0 {
		return nil
	}
	out := make([]server.APIKeySpec, 0, len(in))
	for _, k := range in {
		out = append(out, server.APIKeySpec{Key: k.Key, Region: auth.Region(k.Region), Name: k.Name})
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
