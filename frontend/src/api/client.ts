// 网关 HTTP 客户端。
//
// 所有请求走 /api 前缀，由 vite 的 dev proxy 转发到 :7863（网关不发 CORS 头，
// 浏览器直连必被拦）。因此这里只使用相对路径，不出现网关地址。
import type {
  AccountStatus,
  AccountsResponse,
  APIKeyEntry,
  APIKeysResponse,
  Delta,
  Health,
  LoadedAccounts,
  LoginPoll,
  LoginStart,
  ModelList,
  PoolStatus,
  ProtocolId,
  AccountTestResult,
  RegionModels,
  LogTail,
  RequestStats,
  ScheduleStatus,
  ScheduleTask,
} from './types'
import { PROTOCOLS } from './types'

/** 携带 HTTP 状态与网关错误码的错误类型，供 UI 区分「密钥错」与「上游错」。 */
export class ApiError extends Error {
  status: number
  code?: string

  constructor(status: number, message: string, code?: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
  }
}

// ── 管理员口令（/__admin/* 的鉴权）──────────────────────────────────
//
// 与 apiKey 分开：apiKey 用于 /v1/* 与 /status，而 /__admin/* 能增删账号、改写密钥表，
// 给它一把独立口令。本机访问时服务端不校验（能碰本机的人本就能直接读配置文件），
// 所以只在局域网访问时才需要填。
//
// 存在模块级变量而非 React state：adminJSON 是普通函数、不在组件树里，
// 拿不到 context。App.tsx 负责在口令变化时同步过来。
const ADMIN_TOKEN_STORAGE = 'wb2api.admin.token'
let adminToken = localStorage.getItem(ADMIN_TOKEN_STORAGE) ?? ''

/** setAdminToken 更新口令并持久化；传空串表示清除。 */
export function setAdminToken(v: string) {
  adminToken = v
  if (v) localStorage.setItem(ADMIN_TOKEN_STORAGE, v)
  else localStorage.removeItem(ADMIN_TOKEN_STORAGE)
}

/** getAdminToken 读当前口令（供 UI 回显）。 */
export function getAdminToken() {
  return adminToken
}

/** 从错误响应体里提取可读信息。三种协议的错误体形状不同，逐一说实话。 */
async function readError(res: Response): Promise<ApiError> {
  const text = await res.text().catch(() => '')
  let message = text || res.statusText
  let code: string | undefined
  try {
    const body = JSON.parse(text)
    // OpenAI 系： {"error":{"message","type","code"}}
    // Anthropic： {"type":"error","error":{"type","message"}}
    const err = body?.error
    if (err && typeof err === 'object') {
      message = err.message ?? message
      code = typeof err.code === 'string' ? err.code : err.type
    } else if (typeof body?.message === 'string') {
      message = body.message
    }
  } catch {
    // 非 JSON（如代理层返回的纯文本）——保留原文
  }
  return new ApiError(res.status, message, code)
}

async function getJSON<T>(path: string, key?: string): Promise<T> {
  const res = await fetch(`/api${path}`, {
    headers: key ? { Authorization: `Bearer ${key}` } : undefined,
  })
  if (!res.ok) throw await readError(res)
  return (await res.json()) as T
}

/** GET /healthz —— 探活，恒无鉴权。 */
export const getHealth = () => getJSON<Health>('/healthz')

/** GET /status —— 账号池观测。返回内容随密钥绑定的区域收窄。 */
export const getStatus = (key: string) => getJSON<PoolStatus>('/status', key)

/** GET /v1/models —— 该密钥可见的模型（按 id 去重）。 */
export const getModels = (key: string) => getJSON<ModelList>('/v1/models', key)

/** 一条 SSE 帧。 */
export interface SSEFrame {
  event: string
  data: string
}

/**
 * 把字节流切成 SSE 帧。
 * 逐行解析（而非按 \n\n 切块）：跨 chunk 断开的 CRLF 也不会切错。
 */
async function* sseFrames(body: ReadableStream<Uint8Array>): AsyncGenerator<SSEFrame> {
  const reader = body.getReader()
  const dec = new TextDecoder()
  let buf = ''
  let event = ''
  let data: string[] = []
  try {
    for (;;) {
      const { done, value } = await reader.read()
      if (done) break
      buf += dec.decode(value, { stream: true })
      let nl: number
      while ((nl = buf.indexOf('\n')) >= 0) {
        let line = buf.slice(0, nl)
        buf = buf.slice(nl + 1)
        if (line.endsWith('\r')) line = line.slice(0, -1)
        if (line === '') {
          // 空行 = 帧结束
          if (data.length) yield { event, data: data.join('\n') }
          event = ''
          data = []
          continue
        }
        if (line.startsWith(':')) continue // 心跳/注释
        const colon = line.indexOf(':')
        const field = colon < 0 ? line : line.slice(0, colon)
        let val = colon < 0 ? '' : line.slice(colon + 1)
        if (val.startsWith(' ')) val = val.slice(1) // SSE 规定只吃掉一个前导空格
        if (field === 'event') event = val
        else if (field === 'data') data.push(val)
      }
    }
  } finally {
    reader.releaseLock()
  }
}

/**
 * 把三种协议的一帧归一成 Delta。
 * 认不出的帧返回 null（如 response.created、message_start、role 帧），不视为错误。
 * 上游明确报错时抛异常。
 */
export function extractDelta(protocol: ProtocolId, frame: SSEFrame): Delta | null {
  if (frame.data === '[DONE]') return null // OpenAI 流的结束哨兵

  let p: any
  try {
    p = JSON.parse(frame.data)
  } catch {
    return null // 非 JSON 帧直接忽略，避免一个坏帧打断整轮
  }

  // 统一先查错误：三种协议都可能在中途吐错误帧
  if (p?.error && typeof p.error === 'object') {
    throw new ApiError(200, p.error.message ?? '上游返回错误事件', p.error.code ?? p.error.type)
  }
  if (p?.type === 'error') {
    throw new ApiError(200, p.error?.message ?? '上游返回 error 事件', p.error?.type)
  }
  if (p?.type === 'response.failed') {
    throw new ApiError(200, p.response?.error?.message ?? 'response.failed', 'upstream_failed')
  }

  if (protocol === 'openai') {
    const choice = p?.choices?.[0]
    if (!choice) return null
    const out: Delta = {}
    const d = choice.delta
    if (typeof d?.content === 'string' && d.content) out.text = d.content
    // 上游把思维链放在 reasoning_content（非标准字段，实测存在）
    if (typeof d?.reasoning_content === 'string' && d.reasoning_content) {
      out.thinking = d.reasoning_content
    }
    if (choice.finish_reason) out.finish = choice.finish_reason
    if (p.usage?.completion_tokens) out.tokens = p.usage.completion_tokens
    return out.text || out.thinking || out.finish || out.tokens ? out : null
  }

  if (protocol === 'anthropic') {
    switch (p?.type) {
      case 'content_block_delta': {
        const d = p.delta
        if (d?.type === 'text_delta' && d.text) return { text: d.text }
        if (d?.type === 'thinking_delta' && d.thinking) return { thinking: d.thinking }
        return null
      }
      case 'message_delta': {
        const out: Delta = {}
        if (p.delta?.stop_reason) out.finish = p.delta.stop_reason
        if (p.usage?.output_tokens) out.tokens = p.usage.output_tokens
        return out.finish || out.tokens ? out : null
      }
      case 'message_stop':
        return { finish: 'stop' }
      default:
        return null
    }
  }

  // responses
  switch (p?.type) {
    case 'response.output_text.delta':
      return p.delta ? { text: p.delta } : null
    case 'response.reasoning_summary_text.delta':
    case 'response.reasoning_text.delta':
      return p.delta ? { thinking: p.delta } : null
    case 'response.completed': {
      const u = p.response?.usage
      return u?.output_tokens ? { tokens: u.output_tokens } : null
    }
    default:
      return null
  }
}

/** 按协议构造请求体。字段差异是真实的：Anthropic 要 max_tokens，Responses 用 instructions。 */
function buildBody(
  protocol: ProtocolId,
  opts: { model: string; system: string; prompt: string; maxTokens: number; stream: boolean },
): Record<string, unknown> {
  const { model, system, prompt, maxTokens, stream } = opts
  switch (protocol) {
    case 'openai':
      return {
        model,
        stream,
        messages: [
          ...(system ? [{ role: 'system', content: system }] : []),
          { role: 'user', content: prompt },
        ],
      }
    case 'anthropic':
      return {
        model,
        stream,
        max_tokens: maxTokens,
        ...(system ? { system } : {}),
        messages: [{ role: 'user', content: prompt }],
      }
    case 'responses':
      return {
        model,
        stream,
        ...(system ? { instructions: system } : {}),
        input: prompt,
      }
  }
}

export interface StreamOpts {
  protocol: ProtocolId
  key: string
  model: string
  system: string
  prompt: string
  maxTokens: number
  signal: AbortSignal
  onDelta: (d: Delta) => void
  /** 原始帧回调，供调试面板展示协议转换细节 */
  onRaw?: (f: SSEFrame) => void
}

/** 发送一次流式对话。返回响应耗时（毫秒）。 */
export async function streamChat(o: StreamOpts): Promise<number> {
  const path = PROTOCOLS.find((p) => p.id === o.protocol)!.path
  const started = performance.now()
  const res = await fetch(`/api${path}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${o.key}` },
    body: JSON.stringify(
      buildBody(o.protocol, {
        model: o.model,
        system: o.system,
        prompt: o.prompt,
        maxTokens: o.maxTokens,
        stream: true,
      }),
    ),
    signal: o.signal,
  })

  if (!res.ok) throw await readError(res)
  if (!res.body) throw new ApiError(res.status, '响应没有 body，无法读取流')

  for await (const frame of sseFrames(res.body)) {
    o.onRaw?.(frame)
    const d = extractDelta(o.protocol, frame)
    if (d) o.onDelta(d)
  }
  return performance.now() - started
}

/** 非流式：直接把原始 JSON 交给调用方展示，便于核对协议字段。 */
export async function chatOnce(o: Omit<StreamOpts, 'signal' | 'onDelta' | 'onRaw'>) {
  const path = PROTOCOLS.find((p) => p.id === o.protocol)!.path
  const res = await fetch(`/api${path}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${o.key}` },
    body: JSON.stringify(
      buildBody(o.protocol, {
        model: o.model,
        system: o.system,
        prompt: o.prompt,
        maxTokens: o.maxTokens,
        stream: false,
      }),
    ),
  })
  if (!res.ok) throw await readError(res)
  return (await res.json()) as Record<string, unknown>
}

// ── 账号管理（/__admin/*，由 vite.admin.ts 提供，不经网关）────────────

async function adminJSON<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    ...init,
    headers: {
      'Content-Type': 'application/json',
      // 口令始终带上：本机访问时服务端忽略它，局域网访问时必须有。
      // 无脑带比"先探测是否本机"简单，且不会在切换访问方式时出现状态不一致。
      ...(adminToken ? { 'X-Admin-Token': adminToken } : {}),
      ...(init?.headers ?? {}),
    },
  })
  const data = await res.json().catch(() => ({}))
  if (!res.ok) {
    throw new ApiError(res.status, (data as { error?: string }).error ?? res.statusText)
  }
  return data as T
}

export const listAuthFiles = () => adminJSON<AccountsResponse>('/__admin/accounts')

/** 网关已加载的账号 uid 集合，与面板用哪把密钥无关。 */
export const getLoadedAccounts = () => adminJSON<LoadedAccounts>('/__admin/loaded')

export const startLogin = (region: 'cn' | 'global') =>
  adminJSON<LoginStart>('/__admin/login/start', {
    method: 'POST',
    body: JSON.stringify({ region }),
  })

/** 轮询登录结果。必须带 startLogin 返回的 sessionId，否则服务端回 400。 */
export const pollLogin = (sessionId: string) =>
  adminJSON<LoginPoll>('/__admin/login/poll', {
    method: 'POST',
    body: JSON.stringify({ sessionId }),
  })

export const importAuth = (json: string) =>
  adminJSON<{ file: string }>('/__admin/accounts/import', {
    method: 'POST',
    body: JSON.stringify({ json }),
  })

export const deleteAuth = (file: string) =>
  adminJSON<{ ok: boolean }>('/__admin/accounts/delete', {
    method: 'POST',
    body: JSON.stringify({ file }),
  })

/** 按区域列出可测模型，供测试连接的下拉框用。 */
export const getRegionModels = () => adminJSON<RegionModels>('/__admin/models')

/** 网关日志尾部。lines 由服务端夹到 1..5000。 */
export const getLogs = (lines: number) => adminJSON<LogTail>(`/__admin/logs?lines=${lines}`)

/** 请求统计。hours 传 null 表示不限窗口。 */
export const getStats = (hours: number | null) =>
  adminJSON<RequestStats>(`/__admin/stats?hours=${hours ?? 'all'}`)

/** 调度状态（时点与上次运行为推算值；manual 是真实的手动执行记录）。 */
export const getSchedule = () => adminJSON<ScheduleStatus>('/__admin/schedule')

/**
 * 立即执行一次定时任务。
 *
 * 服务端**不等执行完**就返回（一趟全量要几十秒到两分钟），运行态由
 * getSchedule 的 manual 字段透出 —— 触发后应转去轮询它，而不是等这个请求。
 */
export const runScheduleTask = (task: ScheduleTask['key']) =>
  adminJSON<{ ok: true; task: string; label: string; msg: string }>('/__admin/schedule/run', {
    method: 'POST',
    body: JSON.stringify({ task }),
  })

/**
 * 测单个账号的连通性。
 * 绕开网关直连上游 —— 网关的 chat 端点自己挑号，没法指定账号。
 */
export const testAccount = (uid: string, model?: string) =>
  adminJSON<AccountTestResult>('/__admin/accounts/test', {
    method: 'POST',
    body: JSON.stringify({ uid, model }),
  })

/**
 * 重置账号状态（冷却 / 退避 / 熔断 / 可选禁用）。
 * 面板与网关同进程，改内存即时生效，不再有停机。
 */
export const resetAccounts = (uids: string[], includeDisabled = false) =>
  adminJSON<{
    ok: boolean
    outcomes: { uid: string; nickname: string; before: string; after: string }[]
  }>('/__admin/pool/reset', {
    method: 'POST',
    body: JSON.stringify({ uids, includeDisabled }),
  })

// ── API 密钥（读写 config.json）─────────────────────────────────────

export const getAPIKeys = () => adminJSON<APIKeysResponse>('/__admin/apikeys')

export const saveAPIKeys = (
  legacyKey: string,
  keys: APIKeyEntry[],
  allowUnauthenticated = false,
) =>
  adminJSON<{ ok: boolean; backup: string; keyCount: number }>('/__admin/apikeys', {
    method: 'POST',
    body: JSON.stringify({ legacyKey, keys, allowUnauthenticated }),
  })

/** 生成 48 位十六进制密钥，与仓库现有密钥同格式。 */
export function generateKey(): string {
  const b = new Uint8Array(24)
  crypto.getRandomValues(b)
  return Array.from(b)
    .map((x) => x.toString(16).padStart(2, '0'))
    .join('')
}

/** 让网关重扫 auths 目录（池子与磁盘对齐），返回池中账号数。 */
export const reloadPool = () =>
  adminJSON<{ ok: boolean; loaded: number }>('/__admin/pool/reload', { method: 'POST' })

// ── 展示辅助 ──────────────────────────────────────────────────────

/** Go 的 time.Time 零值会被序列化成 0001-01-01，需要识别成「从未」。 */
export function fmtTime(iso?: string): string {
  if (!iso || iso.startsWith('0001-01-01')) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '—'
  return d.toLocaleString('zh-CN', { hour12: false })
}

/** 秒 → 「2h 3m」「19m 21s」这类紧凑形式。 */
export function fmtDuration(sec?: number): string {
  if (!sec || sec <= 0) return '—'
  const h = Math.floor(sec / 3600)
  const m = Math.floor((sec % 3600) / 60)
  const s = Math.floor(sec % 60)
  if (h) return `${h}h ${m}m`
  if (m) return `${m}m ${s}s`
  return `${s}s`
}

/** 账号是否已熔断（熔断时间还没到）。 */
export function breakerActive(a: AccountStatus, now = Date.now()): boolean {
  if (!a.breaker_until || a.breaker_until.startsWith('0001-01-01')) return false
  return new Date(a.breaker_until).getTime() > now
}
