import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type {
  AccountTestResult,
  Health,
  PoolStatus,
  RegionModels,
  ScheduleStatus,
  ThinkingLoopHit,
} from '../api/types'
import {
  breakerActive,
  fmtDuration,
  fmtTime,
  getRegionModels,
  getSchedule,
  resetAccounts,
  runScheduleTask,
  testAccount,
} from '../api/client'

/** 手选测试模型的持久化键（与 App.tsx 的密钥键同一命名空间）。 */
const MODEL_STORAGE = 'wb2api.admin.testModel'

interface Props {
  status: PoolStatus | null
  health: Health | null
  now: number
  /** 重置改动账号状态，完成后需要让外层刷新一次（可用数、冷却倒计时都变了） */
  onChanged: () => void
  /** 当前密钥在 config.json 里绑的区域；空串 = 不限区域。网关按它过滤 /status。 */
  keyRegion?: string
  /** 网关账号池总数（来自无鉴权的 /healthz），不受密钥区域过滤 */
  poolTotal?: number
}

/** 账号综合健康度：决定表格里那一枚状态徽标。 */
function accountState(a: PoolStatus['accounts'][number], now: number) {
  if (a.disabled) return { label: '已禁用', cls: 'err' } as const
  if (breakerActive(a, now)) return { label: '熔断中', cls: 'err' } as const
  if (a.cooling) {
    const kind = a.cool_kind === 'hard_credit' ? '余额冷却' : '限流冷却'
    return { label: kind, cls: 'warn' } as const
  }
  return { label: '正常', cls: 'ok' } as const
}

type SortKey = 'status' | 'credits' | 'activity'

/** 排序用的严重度：越大越糟。与 accountState 的分支一一对应。 */
function severity(a: PoolStatus['accounts'][number], now: number): number {
  if (a.disabled) return 3
  if (breakerActive(a, now)) return 2
  if (a.cooling) return 1
  return 0
}

/** 最近一次活动（成功或失败）的时间戳；两者都没有返回 0，排在最后。 */
function lastActive(a: PoolStatus['accounts'][number]): number {
  const ms = (iso?: string) =>
    !iso || iso.startsWith('0001-01-01') ? 0 : (new Date(iso).getTime() || 0)
  return Math.max(ms(a.last_success), ms(a.last_err))
}

/** 一行测试结果的可复制摘要。 */
function testSummary(r: AccountTestResult, nick: string): string {
  return [
    `${nick || '（无昵称）'} (${r.uid})${r.region ? ` [${r.region}]` : ''}`,
    `${r.ok ? '通过' : '失败'} · HTTP ${r.httpStatus || '—'}${r.code ? ` code=${r.code}` : ''} · ${r.latencyMs}ms`,
    `模型 ${r.model}`,
    r.message,
  ].join('\n')
}

function regionBadge(region?: string) {
  if (region === 'global') return <span className="badge accent">global</span>
  if (region === 'cn') return <span className="badge">cn</span>
  return <span className="badge">—</span>
}

/** 相对时间：「还有 5h 37m」「还有 12m」。已经到点就返回「即将」。 */
function fmtLeft(ms: number): string {
  if (ms <= 0) return '即将'
  const s = Math.floor(ms / 1000)
  const h = Math.floor(s / 3600)
  const m = Math.floor((s % 3600) / 60)
  if (h) return `${h}h ${m}m`
  if (m) return `${m}m`
  return `${s}s`
}

/** 本地 HH:MM，用于展示下一次整点。 */
function fmtClock(iso: string): string {
  const d = new Date(iso)
  return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
}

/**
 * 思考空转卡：近期被自动切断的空转及其上下文大小。
 *
 * 为什么把「上下文大小」放在显眼位置：空转实测多发生在长上下文（请求体 920KB /
 * 约 23 万 token 时触发过一次）。列出每次的估算值，一眼就能看出是否集中在长上下文 ——
 * 若确实如此，正解是别让上下文涨到那个区间，而不是反复重试。
 *
 * 「阻断」列区分是哪条判据抓的：停滞（ratio 可能仍高）还是占比。
 * 两条都已命中时标停滞 —— 它先于占比触发，更能说明是「原地打转」而非「文本重复」。
 */
function ThinkingLoopsCard({ hits }: { hits: ThinkingLoopHit[] }) {
  return (
    <div className="panel">
      <header>
        <h2>思考空转</h2>
        <span className="sub">最近 {hits.length} 次（重启清零）</span>
        <div className="spacer" />
        <span className="dim" style={{ fontSize: 11.5 }}>
          命中即切断上游流、追加提示后换号重试
        </span>
      </header>
      <div className="body">
        <table>
          <thead>
            <tr>
              <th>时间</th>
              <th>账号</th>
              <th>模型</th>
              <th title="命中时已吐出的思考字符数，即切断点">切断于</th>
              <th title="原始请求体的 token 估算；与该模型 context_length 对照可判断占满程度">
                上下文
              </th>
              <th title="停滞 = 连续无新内容；占比 = 唯一块占比过低">判据</th>
              <th title="第几次重试时命中；1 表示首次尝试就空转">第几次</th>
            </tr>
          </thead>
          <tbody>
            {/* 后端按时间正序返回，倒序展示让最近的在最上面 */}
            {[...hits].reverse().map((h, i) => (
              <tr key={`${h.at}-${i}`}>
                <td className="mono">{fmtTime(h.at)}</td>
                <td className="mono">{h.uid.slice(0, 8)}</td>
                <td className="mono">{h.model}</td>
                <td className="mono">{fmtTokens(h.think_chars)} 字符</td>
                <td className="mono">{fmtTokens(h.req_est_tokens)} tokens</td>
                <td>{h.stale_chunks > 0 ? '停滞' : '占比'}</td>
                <td className="mono">{h.retry}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}

/** 大数字紧凑显示：232535 → 「23.3 万」，便于一眼比较量级。 */
function fmtTokens(n: number): string {
  if (n >= 10000) return `${(n / 10000).toFixed(1)} 万`
  return String(n)
}

/** 定时任务排程卡：下次运行时间 + 立即执行。 */
function ScheduleCard({
  sched,
  running,
  onRun,
}: {
  sched: ScheduleStatus
  /** 正在触发请求的任务 key（POST 尚未返回）；执行中由 sched.tasks[].manual 反映 */
  running: string
  onRun: (key: ScheduleStatus['tasks'][number]['key']) => void
}) {
  const anyConfigOnly = sched.tasks.some((t) => t.source === 'config')
  // 倒计时以响应里的服务端时刻为基准（渲染期不读时钟）；卡片 60s 一刷，分钟级够用。
  const base = new Date(sched.now).getTime()

  return (
    <div className="panel">
      <header>
        <h2>定时任务</h2>
        <span className="sub">下次运行</span>
        <div className="spacer" />
        <span className="dim" style={{ fontSize: 11.5 }}>
          时点为推算值，执行记录来自网关
        </span>
      </header>
      <div className="body">
        <table>
          <thead>
            <tr>
              <th>任务</th>
              <th>时点</th>
              <th>下次</th>
              <th>本进程上次</th>
              <th>手动执行</th>
              <th>说明</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {sched.tasks.map((t) => {
              // 两处「忙」：POST 在途（running）与后台真的在跑（manual.running）。
              // 前者是毫秒级，后者可达两分钟，都要禁掉按钮防重复触发。
              const busy = running === t.key || !!t.manual?.running
              return (
                <tr key={t.key}>
                  <td style={{ color: 'var(--text-strong)' }}>
                    {t.label}
                    {!t.enabled && <span className="badge err" style={{ marginLeft: 6 }}>已禁用</span>}
                  </td>
                  <td className="mono dim">
                    {t.hours.length
                      ? t.hours.map((h) => `${String(h).padStart(2, '0')}:00`).join(' ')
                      : '—'}
                  </td>
                  <td className="mono">
                    {t.nextRun ? (
                      <>
                        <span style={{ color: 'var(--text-strong)' }}>{fmtClock(t.nextRun)}</span>
                        <span className="dim"> · 还有 {fmtLeft(new Date(t.nextRun).getTime() - base)}</span>
                      </>
                    ) : (
                      <span className="dim">—</span>
                    )}
                  </td>
                  <td className="mono dim" style={{ fontSize: 11.5 }}>
                    {t.lastRun
                      ? new Date(t.lastRun).toLocaleString('zh-CN', { hour12: false })
                      : t.quiet
                        ? '无记录'
                        : '未跑过'}
                  </td>
                  <td style={{ fontSize: 11.5, maxWidth: 260 }}>
                    {t.manual ? (
                      <>
                        <span className="mono dim">
                          {new Date(t.manual.started).toLocaleString('zh-CN', { hour12: false })}
                        </span>
                        <div style={{ color: t.manual.running ? 'var(--warn)' : undefined }}>
                          {t.manual.running ? '执行中…' : t.manual.summary}
                        </div>
                      </>
                    ) : (
                      <span className="dim">未手动执行过</span>
                    )}
                  </td>
                  <td className="dim" style={{ fontSize: 11.5 }}>
                    {t.note}
                    {t.source === 'config' && (
                      <div style={{ color: 'var(--warn)' }}>
                        时点取自 config.json，未与运行中的进程核对
                      </div>
                    )}
                  </td>
                  <td>
                    <button
                      className="btn"
                      disabled={busy}
                      title="立即执行一次；跑完的结果显示在左侧「手动执行」列"
                      onClick={() => onRun(t.key)}
                    >
                      {busy ? '执行中' : '执行'}
                    </button>
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>

        <div className="dim" style={{ fontSize: 11, marginTop: 10, lineHeight: 1.7 }}>
          时点表取自网关启动时打的排程日志，只统计本次进程启动之后的执行。
          调度器只在出错或跳过时打日志，成功路径静默 —— 标「无记录」不代表没跑过，
          而是这一趟没留下日志。时点与「本进程上次」由面板按日志推算；
          「手动执行」是网关记录的真实运行态（重启即忘）。执行期间本卡每 3s 刷新一次。
          {anyConfigOnly && ' 标黄的任务时点取自 config.json，未与运行中的进程核对。'}
          {sched.logTruncated && ' 日志只读了末尾一块，更早的启动行可能没读到。'}
        </div>
      </div>
    </div>
  )
}

export function Overview({ status, health, now, onChanged, keyRegion, poolTotal }: Props) {
  const [regionFilter, setRegionFilter] = useState<'all' | 'cn' | 'global'>('all')
  const [onlyProblem, setOnlyProblem] = useState(false)
  const [sortKey, setSortKey] = useState<SortKey>('status')
  const [sortDesc, setSortDesc] = useState(true)

  /** 测试结果按 uid 存；「测试中」与「重置中」也是按 uid 的瞬时态 */
  const [tests, setTests] = useState<Record<string, AccountTestResult>>({})
  const [testing, setTesting] = useState<Set<string>>(new Set())
  const [resetting, setResetting] = useState<Set<string>>(new Set())
  const [batching, setBatching] = useState(false)
  const [copied, setCopied] = useState('')
  const [err, setErr] = useState('')
  const [note, setNote] = useState('')

  /** 可测模型表按区域取，随账号区域自动切换 */
  const [models, setModels] = useState<RegionModels | null>(null)
  /**
   * 空串 = 用该账号区域的推荐模型（优先 auto）。
   * 持久化：手选的模型刷新页面后要留住，否则每次进面板都得重选一遍。
   */
  const [model, setModel] = useState(() => localStorage.getItem(MODEL_STORAGE) ?? '')

  useEffect(() => {
    if (model) localStorage.setItem(MODEL_STORAGE, model)
    else localStorage.removeItem(MODEL_STORAGE)
  }, [model])

  useEffect(() => {
    let cancelled = false
    getRegionModels()
      .then((m) => {
        if (!cancelled) setModels(m)
      })
      .catch(() => {
        // 取不到就让下拉框留空，测试仍可用默认模型跑
      })
    return () => {
      cancelled = true
    }
  }, [])

  /**
   * 定时任务排程。空闲时 60s 一次（时点只按整点变化，跟 5s 的账号轮询不同频）；
   * 有任务在跑时 3s 一次 —— 否则一趟两分钟的执行，面板要等一分钟才更新状态。
   */
  const [sched, setSched] = useState<ScheduleStatus | null>(null)
  /** 正在触发 POST 的任务 key（毫秒级，仅用于立刻禁用按钮） */
  const [triggering, setTriggering] = useState('')
  const anyRunning = !!sched?.tasks.some((t) => t.manual?.running)
  useEffect(() => {
    let cancelled = false
    const load = () =>
      getSchedule()
        .then((s) => {
          if (!cancelled) setSched(s)
        })
        .catch(() => {
          // 拿不到就不显示这张卡，总览页其余部分不受影响
        })
    void load()
    const t = setInterval(load, anyRunning ? 3_000 : 60_000)
    return () => {
      cancelled = true
      clearInterval(t)
    }
  }, [anyRunning])

  /**
   * 立即执行一次任务。POST 只表示「已开始」—— 真正的执行在网关后台跑，
   * 结果要靠轮询 sched 的 manual 字段拿到（所以这里成功后就地刷一次排程）。
   */
  const runTask = useCallback(async (key: ScheduleStatus['tasks'][number]['key']) => {
    setTriggering(key)
    setErr('')
    try {
      await runScheduleTask(key)
      setSched(await getSchedule())
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setTriggering('')
    }
  }, [])

  /** 某账号区域的可选模型；区域未知时并入两区，宁可多给也不要漏。 */
  const modelsFor = useCallback(
    (region?: string) => {
      if (!models) return []
      if (region === 'cn') return models.cn
      if (region === 'global') return models.global
      return [...new Set([...models.cn, ...models.global])]
    },
    [models],
  )

  /**
   * 该账号测试时实际会用的模型。
   * 默认选 auto：它由上游自己挑一个可用模型，最能反映「账号能不能用」；
   * 而具体模型（如 deepseek-v4.1-flash）常有独立的 6004 限流，会给出假阴性。
   *
   * 手选模型若不在该账号区域（如筛 global 时选了 cn 专有模型），会退回 auto ——
   * 否则上游会回 11102「模型不存在」，把区域错配误报成账号故障。
   */
  const effectiveModel = useCallback(
    (region?: string) => {
      const list = modelsFor(region)
      if (model && list.includes(model)) return model
      return list.includes('auto') ? 'auto' : (list[0] ?? '')
    },
    [model, modelsFor],
  )

  // 下拉框跟随区域筛选：筛到 cn 就只列 cn 的模型
  const selectableModels = useMemo(
    () => (regionFilter === 'all' ? modelsFor(undefined) : modelsFor(regionFilter)),
    [regionFilter, modelsFor],
  )

  // 切区会让已选模型从列表里消失（如 cn 专有模型遇上 global 筛选）。
  // 此时 value 指向一个不存在的 option，浏览器自己回落到第一项，
  // 界面显示「自动」而 state 还留着旧模型 —— 切回「全部」它又会冒出来。
  // 在切区这一刻就清掉，让显示与 state 一致。
  const pickRegion = (r: 'all' | 'cn' | 'global') => {
    setRegionFilter(r)
    const list = r === 'all' ? modelsFor(undefined) : modelsFor(r)
    if (model && !list.includes(model)) setModel('')
  }

  // 5s 轮询会让整个组件重渲染，但测试结果必须留住 —— 它存在 state 里，不随 status 变化丢失。
  // 组件卸载后不再写 state，避免 React 警告。
  const alive = useRef(true)
  useEffect(() => {
    alive.current = true
    return () => {
      alive.current = false
    }
  }, [])

  const mark = (set: Set<string>, uid: string, on: boolean) => {
    const next = new Set(set)
    if (on) next.add(uid)
    else next.delete(uid)
    return next
  }

  const doTest = useCallback(
    async (uid: string, region?: string) => {
      setErr('')
      setNote('')
      setTesting((s) => mark(s, uid, true))
      try {
        const r = await testAccount(uid, effectiveModel(region))
        if (!alive.current) return
        setTests((t) => ({ ...t, [uid]: r }))
      } catch (e) {
        if (!alive.current) return
        setErr(`测试失败：${e instanceof Error ? e.message : String(e)}`)
      } finally {
        if (alive.current) setTesting((s) => mark(s, uid, false))
      }
    },
    [effectiveModel],
  )

  /**
   * 把一份文本放进剪贴板，并把按钮文字短暂切成「已复制」。
   * navigator.clipboard 在非安全上下文（http 的非 localhost）下不存在，故留降级。
   */
  const copy = async (key: string, text: string) => {
    try {
      if (navigator.clipboard) await navigator.clipboard.writeText(text)
      else throw new Error('no clipboard')
      setCopied(key)
      setTimeout(() => setCopied((c) => (c === key ? '' : c)), 1500)
    } catch {
      setErr('复制失败 —— 浏览器不允许写入剪贴板，请手动选中文本复制。')
    }
  }

  /**
   * 串行测一批账号。
   * 并发测会同时占用多个上游配额，测出来的限流是彼此挤出来的，反而不准；
   * 串行还能让每行结果按顺序落位。
   */
  const doTestAll = useCallback(
    async (list: PoolStatus['accounts']) => {
      if (list.length === 0) return
      if (
        list.length > 1 &&
        !confirm(
          `将依次测试 ${list.length} 个账号（串行，耗时随账号数累加）。\n\n` +
            `· 每个账号真实打一次上游，会消耗额度\n` +
            `· 结果只存在本页，刷新即丢`,
        )
      ) {
        return
      }
      setErr('')
      setNote('')
      setBatching(true)
      for (const a of list) {
        await doTest(a.uid, a.region)
      }
      if (alive.current) setBatching(false)
    },
    [doTest],
  )

  const doReset = useCallback(
    async (uids: string[], includeDisabled = false) => {
      if (uids.length === 0) return
      const label = uids.length === 1 ? '该账号' : `这 ${uids.length} 个账号`
      if (
        !confirm(
          `确认重置${label}的状态？\n\n` +
            `· 会清除冷却、连续限流退避与熔断状态\n` +
            (includeDisabled ? `· 会同时解除「已禁用」标记（凭证若已失效，很快会再次被禁用）\n` : '') +
            `· 立即生效，不影响进行中的请求\n` +
            `· 不校验账号是否真的可用 —— 上游若仍限流，很快会再次进入冷却`,
        )
      ) {
        return
      }
      setErr('')
      setNote('')
      setResetting((s) => {
        const next = new Set(s)
        for (const u of uids) next.add(u)
        return next
      })
      try {
        const r = await resetAccounts(uids, includeDisabled)
        if (!alive.current) return
        setNote(`已重置 ${r.outcomes.length} 个账号的状态。`)
        setTests({}) // 状态已变，旧的测试结论不再对应当前池子
        onChanged()
      } catch (e) {
        if (!alive.current) return
        setErr(`重置失败：${e instanceof Error ? e.message : String(e)}`)
        onChanged() // 状态可能已被部分改动，刷新一次让界面回到真实状态
      } finally {
        if (alive.current) {
          setResetting((s) => {
            const next = new Set(s)
            for (const u of uids) next.delete(u)
            return next
          })
        }
      }
    },
    [onChanged],
  )

  if (!status) {
    return <div className="empty">尚无数据 —— 请先在右上角填入 API 密钥。</div>
  }

  const accounts = status.accounts
    .filter((a) => {
      if (regionFilter !== 'all' && a.region !== regionFilter) return false
      if (onlyProblem && !(a.disabled || a.cooling || breakerActive(a, now))) return false
      return true
    })
    .sort((x, y) => {
      const cmp =
        sortKey === 'status'
          ? severity(x, now) - severity(y, now)
          : sortKey === 'credits'
            ? x.credits - y.credits
            : lastActive(x) - lastActive(y)
      return sortDesc ? -cmp : cmp
    })

  const regions = new Set(status.accounts.map((a) => a.region ?? 'cn'))

  // 批量重置只针对「异常」账号：正常账号没有可重置的东西，全量重置毫无收益。
  const problemUids = status.accounts
    .filter((a) => a.disabled || a.cooling || breakerActive(a, now))
    .map((a) => a.uid)

  return (
    <>
      {health && health.healthy === 0 && (
        <div className="note err">
          <span>●</span>
          <div>
            <b>账号池无可用账号</b> —— 所有请求都会返回 503。检查下方账号的冷却/禁用状态，
            或补充新账号。
          </div>
        </div>
      )}

      {err && (
        <div className="note err">
          <span>●</span>
          <div style={{ wordBreak: 'break-word' }}>{err}</div>
        </div>
      )}

      {note && (
        <div className="note info">
          <span>●</span>
          <div>{note}</div>
        </div>
      )}

      {/* 网关按密钥绑定的区域过滤 /status（handler.go 的 keyRegionPred，有测试守着）。
          没有这句提示时，账号池少几个账号看起来就像账号丢了。 */}
      {keyRegion && (
        <div className="note info">
          <span>●</span>
          <div>
            当前密钥绑定区域 <b>{keyRegion}</b>，网关据此过滤了账号 —— 下方的计数与表格
            只含该区域的账号
            {poolTotal ? `，账号池实际共 ${poolTotal} 个` : ''}。
            改用不限区域的密钥即可看到全部。
          </div>
        </div>
      )}

      <div className="stats">
        <div className="stat">
          <div className="k">账号总数</div>
          <div className="v">{status.total}</div>
        </div>
        <div className="stat">
          <div className="k">健康</div>
          <div className={'v ' + (status.healthy ? 'ok' : 'err')}>{status.healthy}</div>
        </div>
        <div className="stat">
          <div className="k">冷却中</div>
          <div className={'v ' + (status.cooling ? 'warn' : '')}>{status.cooling}</div>
        </div>
        <div className="stat">
          <div className="k">已禁用</div>
          <div className={'v ' + (status.disabled ? 'err' : '')}>{status.disabled}</div>
        </div>
        <div className="stat">
          <div className="k">在途占满</div>
          <div className="v">{status.in_flight_full}</div>
        </div>
        <div className="stat">
          <div className="k">粘性会话</div>
          <div className="v">{status.sticky_sessions}</div>
        </div>
        <div className="stat">
          <div className="k">Redis</div>
          <div className="v" style={{ fontSize: 15, paddingTop: 5 }}>
            {status.redis_mode}
          </div>
        </div>
        <div className="stat">
          <div className="k">区域数</div>
          <div className="v">{regions.size}</div>
        </div>
        <div className="stat">
          <div className="k">思考空转</div>
          <div className="v" title="已自动切断并重试的空转次数；重启后清零">
            {status.thinking_loop_total}
          </div>
        </div>
      </div>

      {status.thinking_loops.length > 0 && <ThinkingLoopsCard hits={status.thinking_loops} />}

      {sched && <ScheduleCard sched={sched} running={triggering} onRun={runTask} />}

      <div className="panel">
        <header>
          <h2>账号池</h2>
          <span className="sub">
            {accounts.length} / {status.accounts.length}
          </span>
          <div className="spacer" />
          <div className="tabs">
            {(['all', 'cn', 'global'] as const).map((r) => (
              <button
                key={r}
                aria-selected={regionFilter === r}
                onClick={() => pickRegion(r)}
              >
                {r === 'all' ? '全部' : r}
              </button>
            ))}
          </div>
          <label
            style={{ margin: 0, display: 'flex', alignItems: 'center', gap: 6, cursor: 'pointer' }}
          >
            <input
              type="checkbox"
              checked={onlyProblem}
              onChange={(e) => setOnlyProblem(e.target.checked)}
            />
            只看异常
          </label>
        </header>

        {/* 控件一行放不下，按日志/统计页的既有做法另起一条工具条，避免把 header 挤爆 */}
        <div className="toolbar">
          <label style={{ margin: 0, display: 'flex', alignItems: 'center', gap: 6 }}>
            <span className="dim" style={{ fontSize: 12 }}>
              测试模型
            </span>
            <select
              value={model}
              onChange={(e) => setModel(e.target.value)}
              disabled={!models?.reachable}
              title="选项跟随上方的区域筛选；auto 由上游挑一个可用模型，最能判断账号本身是否可用"
            >
              <option value="">
                {models?.reachable ? '自动（优先 auto）' : '取不到模型表'}
              </option>
              {/* 跟随区域筛选：筛到 cn 就只列 cn 的模型，避免选出该区不存在的模型 */}
              {selectableModels.map((m) => (
                <option key={m} value={m}>
                  {m}
                </option>
              ))}
            </select>
          </label>

          <label style={{ margin: 0, display: 'flex', alignItems: 'center', gap: 6 }}>
            <span className="dim" style={{ fontSize: 12 }}>
              排序
            </span>
            <select value={sortKey} onChange={(e) => setSortKey(e.target.value as SortKey)}>
              <option value="status">按状态</option>
              <option value="credits">按额度</option>
              <option value="activity">按最近活动</option>
            </select>
            <button
              className="btn"
              onClick={() => setSortDesc((v) => !v)}
              title={
                sortKey === 'credits'
                  ? sortDesc
                    ? '当前：额度高的在前'
                    : '当前：额度低的在前'
                  : sortDesc
                    ? '当前：问题大的在前'
                    : '当前：正常的在前'
              }
            >
              {sortDesc ? '降序' : '升序'}
            </button>
          </label>

          <div className="spacer" />

          <button
            className="btn"
            onClick={() => void doTestAll(accounts)}
            disabled={batching || testing.size > 0 || accounts.length === 0}
            title="按当前筛选与排序，依次测一遍（串行，会消耗额度）"
          >
            {batching ? <span className="spin" /> : null}
            测试当前列表（{accounts.length}）
          </button>
          {Object.keys(tests).length > 0 && (
            <button
              className="btn"
              onClick={() =>
                void copy(
                  'all',
                  Object.keys(tests)
                    .map((uid) => {
                      const a = status.accounts.find((x) => x.uid === uid)
                      return testSummary(tests[uid], a?.nickname ?? '')
                    })
                    .join('\n\n'),
                )
              }
              title="把当前所有测试结果按纯文本复制走"
            >
              {copied === 'all' ? '已复制' : `复制结果（${Object.keys(tests).length}）`}
            </button>
          )}
          {problemUids.length > 0 && (
            <button
              className="btn"
              onClick={() => void doReset(problemUids, true)}
              disabled={resetting.size > 0}
              title="清除这些账号的冷却、限流退避与禁用标记（立即生效）"
            >
              {resetting.size > 0 ? <span className="spin" /> : null}
              重置异常账号（{problemUids.length}）
            </button>
          )}
        </div>

        {accounts.length === 0 ? (
          <div className="empty">没有匹配的账号。</div>
        ) : (
          <div className="scroll-x">
            <table>
              <thead>
                <tr>
                  <th>账号</th>
                  <th>区域</th>
                  <th>状态</th>
                  <th>额度</th>
                  <th>在途</th>
                  <th>成功/失败</th>
                  <th>最近活动</th>
                  <th>原因</th>
                  <th>操作</th>
                </tr>
              </thead>
              <tbody>
                {accounts.map((a) => {
                  const st = accountState(a, now)
                  // 熔断剩余时间由前端算：网关只给绝对时间，且这个值每分钟才变。
                  const breakerLeft = Math.max(
                    0,
                    Math.round((new Date(a.breaker_until ?? 0).getTime() - now) / 1000),
                  )
                  return (
                    <tr key={a.uid}>
                      <td>
                        <div style={{ color: 'var(--text-strong)' }}>{a.nickname || '（无昵称）'}</div>
                        <div className="mono dim" style={{ fontSize: 11 }}>
                          {a.uid}
                        </div>
                      </td>
                      <td>{regionBadge(a.region)}</td>
                      <td>
                        <span className={'badge ' + st.cls}>{st.label}</span>
                        {a.cooling && a.cool_remaining_sec ? (
                          <div className="dim mono" style={{ fontSize: 11, marginTop: 3 }}>
                            剩 {fmtDuration(Math.max(0, a.cool_remaining_sec))}
                          </div>
                        ) : null}
                        {breakerActive(a, now) && (
                          <div className="dim mono" style={{ fontSize: 11, marginTop: 3 }}>
                            熔断剩 {fmtDuration(breakerLeft)}
                            {a.breaker_fails ? ` (连失 ${a.breaker_fails})` : ''}
                          </div>
                        )}
                      </td>
                      <td className="mono">{a.credits}</td>
                      <td className="mono">{a.in_flight}</td>
                      <td className="mono nowrap">
                        <span style={{ color: 'var(--ok)' }}>{a.success_count ?? 0}</span>
                        {' / '}
                        <span style={{ color: a.err_total ? 'var(--err)' : 'var(--text-dim)' }}>
                          {a.err_total ?? 0}
                        </span>
                      </td>
                      <td className="nowrap" style={{ fontSize: 11.5 }}>
                        <div className="mono dim">成功 {fmtTime(a.last_success)}</div>
                        <div className="mono dim">失败 {fmtTime(a.last_err)}</div>
                      </td>
                      <td className="dim" style={{ maxWidth: 240, fontSize: 12 }}>
                        {a.reason || '—'}
                        {a.soft_streak ? (
                          <div style={{ fontSize: 11 }}>连续退避 ×{a.soft_streak}</div>
                        ) : null}
                      </td>
                      <td>
                        <div className="row" style={{ flexWrap: 'nowrap' }}>
                          <button
                            className="btn"
                            onClick={() => void doTest(a.uid, a.region)}
                            disabled={testing.has(a.uid)}
                            title={`直连上游测一次，绕开网关自己挑号的逻辑（模型：${effectiveModel(a.region) || '默认'}）`}
                          >
                            {testing.has(a.uid) ? <span className="spin" /> : null}
                            测试
                          </button>
                          <button
                            className="btn"
                            onClick={() => void doReset([a.uid], a.disabled)}
                            disabled={resetting.has(a.uid)}
                            title="清除该账号的冷却与限流退避（立即生效）"
                          >
                            {resetting.has(a.uid) ? <span className="spin" /> : null}
                            重置
                          </button>
                        </div>
                        {tests[a.uid] && (
                          <div
                            className="mono"
                            style={{
                              fontSize: 11,
                              marginTop: 4,
                              maxWidth: 260,
                              whiteSpace: 'normal',
                              color: tests[a.uid].ok ? 'var(--ok)' : 'var(--err)',
                            }}
                          >
                            {tests[a.uid].ok
                              ? `通 ${tests[a.uid].latencyMs}ms`
                              : `失败 ${tests[a.uid].httpStatus || '—'}${tests[a.uid].code ? ' code=' + tests[a.uid].code : ''}`}
                            <div className="dim" style={{ color: 'inherit', opacity: 0.85 }}>
                              {tests[a.uid].message}
                            </div>
                            {/* 标出测的是哪个模型：6004 是模型级限流，换模型可能就是通的 */}
                            <div
                              className="dim"
                              style={{
                                color: 'inherit',
                                opacity: 0.7,
                                display: 'flex',
                                alignItems: 'center',
                                gap: 6,
                              }}
                            >
                              @{tests[a.uid].model}
                              <button
                                className="btn"
                                style={{ padding: '0 6px', fontSize: 10.5, lineHeight: 1.6 }}
                                onClick={() =>
                                  void copy(a.uid, testSummary(tests[a.uid], a.nickname ?? ''))
                                }
                                title="复制这一行的完整结论"
                              >
                                {copied === a.uid ? '已复制' : '复制'}
                              </button>
                            </div>
                          </div>
                        )}
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </>
  )
}
