// 网关 JSON 响应的类型定义。
// 字段与 internal/pool/pool.go 的 Status 结构、internal/server/handler.go 的
// healthz/status/models 响应一一对应；改后端时需同步这里。

export type Region = 'cn' | 'global'

/** 单个账号的观测状态（脱敏）。对应 pool.Status。 */
export interface AccountStatus {
  uid: string
  region?: Region
  nickname?: string
  credits: number
  cooling: boolean
  /** "hard_credit"（余额耗尽，等签到）| "soft_rate"（429，短冷却） */
  cool_kind?: string
  /** 冷却剩余秒数，仅在 cooling 为 true 时存在 */
  cool_remaining_sec?: number
  until?: string
  reason?: string
  /** 连续软冷却次数，指数退避的指数 */
  soft_streak?: number
  disabled: boolean
  success_count?: number
  err_total?: number
  last_success?: string
  last_err?: string
  in_flight: number
  breaker_fails: number
  breaker_until?: string
}

/** 一次思考空转命中的观测记录（后端 loopguard.go 的 LoopHit） */
export interface ThinkingLoopHit {
  at: string
  uid: string
  model: string
  /** 命中时已吐出的思考字符数（切断点） */
  think_chars: number
  /** 唯一块占比；停滞判据命中时它可能仍然很高 */
  ratio: number
  stale_chunks: number
  /** 命中请求的上下文大小估算，与模型 context_length 对照判断占满程度 */
  req_est_tokens: number
  /** 第几次重试时命中（1 = 首次尝试就空转） */
  retry: number
}

/** 对应 GET /status 的响应 */
export interface PoolStatus {
  accounts: AccountStatus[]
  total: number
  healthy: number
  cooling: number
  disabled: number
  in_flight_full: number
  sticky_sessions: number
  redis_mode: string
  /** 累计空转命中次数（进程级，重启清零） */
  thinking_loop_total: number
  /** 近期命中记录（最多 20 条，按时间正序） */
  thinking_loops: ThinkingLoopHit[]
}

/** 对应 GET /healthz（无鉴权） */
export interface Health {
  healthy: number
  total: number
  service: string
}

/** 对应 GET /v1/models 里 data 数组的元素 */
export interface ModelInfo {
  id: string
  object: string
  created: number
  owned_by: string
  context_length: number
  max_output_tokens?: number
}

export interface ModelList {
  object: string
  data: ModelInfo[]
}

/** 三种客户端协议。value 即请求路径。 */
export const PROTOCOLS = [
  { id: 'openai', label: 'OpenAI Chat', path: '/v1/chat/completions' },
  { id: 'anthropic', label: 'Anthropic Messages', path: '/v1/messages' },
  { id: 'responses', label: 'OpenAI Responses', path: '/v1/responses' },
] as const

export type ProtocolId = (typeof PROTOCOLS)[number]['id']

/** 一条流式增量。三种协议归一到同一形状。 */
export interface Delta {
  /** 正文增量 */
  text?: string
  /** 思维链增量（上游 deepseek/glm 会先吐这个） */
  thinking?: string
  /** 终止原因，出现即表示本轮结束 */
  finish?: string
  /** 上游报告的 completion token 数 */
  tokens?: number
}

// ── 账号管理（/__admin/*，由 vite.admin.ts 提供，不经网关）────────────

/** 磁盘上一个 auth 文件的元信息（不含完整凭证）。 */
export interface AuthFileView {
  file: string
  uid: string
  nickname: string
  enterpriseId: string
  domain: string
  region: Region
  expiresAt: number
  expired: boolean
  tokenHint: string
}

export interface AccountsResponse {
  authDir: string
  accounts: AuthFileView[]
}

export interface LoginStart {
  region: Region
  authUrl: string
  /** 本次登录会话标识；轮询时必须原样带回，用于隔离并发登录。 */
  sessionId: string
}

export type LoginPoll =
  | {
      status: 'ok'
      file: string
      account: { uid: string; nickname: string; region: string }
      loaded: number
    }
  | { status: 'pending'; message: string }
  | { status: 'busy'; message: string }

// ── API 密钥（/__admin/apikeys/*，读写 config.json）──────────────────

/** 一条 API 密钥配置。region 为空表示不限区域。 */
export interface APIKeyEntry {
  key: string
  region: string
  name: string
}

export interface APIKeysResponse {
  configPath: string
  /** 顶层 api_key（旧版单一密钥，不限区域） */
  legacyKey: string
  keys: APIKeyEntry[]
}

/** 网关当前已加载的账号 uid 集合（由 Node 侧用全部密钥取并集得到）。 */
export interface LoadedAccounts {
  /** 是否成功连上网关；false 表示网关没起来或所有密钥都失效 */
  reachable: boolean
  uuids: string[]
  /** 账号池总数（来自 /healthz，不受区域过滤） */
  total: number
  keyCount: number
}

/** 按区域划分的可测模型表（取自网关 /v1/models）。 */
export interface RegionModels {
  cn: string[]
  global: string[]
  /** 是否至少有一把密钥问到了网关 */
  reachable: boolean
  /** 缺少某区域的绑定密钥，该区域列表是不限区域密钥的并集（可能含另一区模型） */
  degraded: boolean
}

/** 网关日志尾部（GET /__admin/logs）。 */
export interface LogTail {
  /** 日志文件绝对路径 */
  path: string
  /** 最近 N 行，按时间正序 */
  lines: string[]
  /** 是否只读了文件末尾一块（更早的行没取） */
  truncated: boolean
  /** 文件总字节数 */
  size: number
  /** 文件最后修改时间（ISO）；文件不存在时为 null */
  mtime: string | null
}

/** 按模型/账号聚合的一组指标（GET /__admin/stats）。 */
export interface StatGroup {
  /** 模型名（注意：日志里被截断到 11 字符）或 uid 前 8 位 */
  key: string
  requests: number
  ok: number
  errors: number
  successRate: number
  /** 累计输出 token（上游未给 usage 的行不计入） */
  tokens: number
  /** 首字节耗时均值（毫秒）；无非流式样本时为 null */
  avgTtfbMs: number | null
  avgTokps: number | null
}

/** 请求统计（GET /__admin/stats，由日志解析得出）。 */
export interface RequestStats {
  /** null 表示不限时间窗口 */
  windowHours: number | null
  /** 无法按窗口裁剪等情况的说明，正常时为空串 */
  windowNote: string
  /** 是否只读了日志末尾一块 */
  truncated: boolean
  logSize: number
  logMtime: string | null
  totalRequests: number
  ok: number
  errors: number
  successRate: number
  ttfb: {
    samples: number
    avg: number | null
    p50: number | null
    p95: number | null
    max: number | null
  }
  tokps: { samples: number; avg: number | null; p50: number | null; p95: number | null }
  tokensTotal: number
  statusCounts: { status: number; count: number }[]
  byModel: StatGroup[]
  byAccount: StatGroup[]
  /** 窗口内首/末行的行内时间戳（HH:MM:SS，可能带日期前缀） */
  firstTs: string | null
  lastTs: string | null
}

/** 一类定时任务的下次运行时间（GET /__admin/schedule，由面板推算）。 */
export interface ScheduleTask {
  key: 'checkin' | 'travel' | 'activity' | 'keepalive'
  label: string
  note: string
  enabled: boolean
  /** 当天要跑的整点，升序 */
  hours: number[]
  /**
   * 时点表的来源：
   * - `log`：取自运行中进程启动时打的开关行，与进程实际排程一致
   * - `config`：日志里读不到（进程跑了很久），只能从 config.json 推 —— 若配置改了没重启，这就是错的
   */
  source: 'log' | 'config'
  /**
   * 最后一次**有日志记录**的执行（ISO）；从未有记录为 null。
   * 注意：调度器只在出错/跳过时打日志，成功路径静默 —— quiet 为 true 时
   * 这个值可能远早于真实的上次运行。
   */
  lastRun: string | null
  /** 成功时不打日志，lastRun 因此偏旧 */
  quiet: boolean
  /** 下一次运行时间（ISO）；任务禁用或时点表缺失为 null */
  nextRun: string | null
  /**
   * 「立即执行」的运行态；从未手动跑过为 null。
   * 与 lastRun 是两个来源：lastRun 从日志推算（含定时那趟），manual 只记手动触发。
   */
  manual: ManualRun | null
}

/** 一次手动执行的记录（进程内，重启即忘）。 */
export interface ManualRun {
  running: boolean
  /** 本轮开始时刻（ISO） */
  started: string
  /** 本轮结束时刻（ISO）；未跑完为零值时间 */
  finished: string
  /** 结果摘要；执行中为空串。失败也写在这里（如「未执行：与定时那趟撞车」） */
  summary: string
}

/** 调度状态（GET /__admin/schedule）。 */
export interface ScheduleStatus {
  now: string
  /** 是否定位到运行中进程的启动行 */
  startupFound: boolean
  /** 日志是否只读了末尾一块（更早的启动行可能没读到） */
  logTruncated: boolean
  tasks: ScheduleTask[]
}

/** 单账号连通性测试结果（直连上游，不经网关）。 */
export interface AccountTestResult {
  ok: boolean
  uid: string
  nickname: string
  region: Region
  model: string
  httpStatus: number
  /** 上游业务错误码；0 表示没有 */
  code: number
  /** 已翻译成中文的诊断，成功时为「链路正常」 */
  message: string
  latencyMs: number
  /** 成功时收到的首个 SSE 事件，证明链路真的通了 */
  firstEvent?: string
}
