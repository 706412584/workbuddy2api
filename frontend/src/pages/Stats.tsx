import { useCallback, useEffect, useRef, useState } from 'react'
import type { RequestStats, StatGroup } from '../api/types'
import { ApiError, getStats } from '../api/client'

/** 时间窗口选项；null = 不限（用日志里读到的全部行）。 */
const WINDOWS: { label: string; hours: number | null }[] = [
  { label: '近 1 小时', hours: 1 },
  { label: '近 3 小时', hours: 3 },
  { label: '近 24 小时', hours: 24 },
  { label: '全部', hours: null },
]

const REFRESH_MS = 10_000

function fmtMs(v: number | null): string {
  if (v === null) return '—'
  return v >= 1000 ? `${(v / 1000).toFixed(2)}s` : `${v}ms`
}

function fmtNum(v: number | null, digits = 1): string {
  return v === null ? '—' : v.toFixed(digits)
}

function fmtPct(v: number): string {
  return `${(v * 100).toFixed(2)}%`
}

function fmtTokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(2)}M`
  if (n >= 1000) return `${(n / 1000).toFixed(1)}K`
  return String(n)
}

/** 成功率着色：低于 95% 警告，低于 80% 报错。 */
function rateClass(rate: number): string {
  if (rate < 0.8) return 'err'
  if (rate < 0.95) return 'warn'
  return 'ok'
}

export function Stats() {
  const [data, setData] = useState<RequestStats | null>(null)
  const [hours, setHours] = useState<number | null>(null)
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  const [auto, setAuto] = useState(true)

  const alive = useRef(true)
  useEffect(() => {
    alive.current = true
    return () => {
      alive.current = false
    }
  }, [])

  // showBusy 只在用户主动点「刷新」时为真：自动轮询跟着点亮忙碌态的话，按钮文字在
  // 「刷新/刷新中」间来回切，宽度一变就推动整条工具条 —— 看起来就是页面在闪。
  const load = useCallback(async (showBusy = false) => {
    if (showBusy) setBusy(true)
    try {
      const r = await getStats(hours)
      if (!alive.current) return
      setData(r)
      setErr('')
    } catch (e) {
      if (!alive.current) return
      setErr(
        e instanceof ApiError
          ? `统计接口不可用：[HTTP ${e.status}] ${e.message}`
          : `统计接口不可用：${String(e)}。该功能由 vite dev/preview server 提供。`,
      )
    } finally {
      if (showBusy && alive.current) setBusy(false)
    }
  }, [hours])

  useEffect(() => {
    void load()
  }, [load])

  useEffect(() => {
    if (!auto) return
    const t = setInterval(() => void load(false), REFRESH_MS)
    return () => clearInterval(t)
  }, [auto, load])

  if (!data) {
    return (
      <>
        {err && (
          <div className="note err">
            <span>●</span>
            <div style={{ wordBreak: 'break-word' }}>{err}</div>
          </div>
        )}
        {!err && <div className="empty">正在读取日志…</div>}
      </>
    )
  }

  const empty = data.totalRequests === 0

  return (
    <>
      {err && (
        <div className="note err">
          <span>●</span>
          <div style={{ wordBreak: 'break-word' }}>{err}</div>
        </div>
      )}

      {data.windowNote && (
        <div className="note info">
          <span>●</span>
          <div>{data.windowNote}</div>
        </div>
      )}

      {data.truncated && (
        <div className="note info">
          <span>●</span>
          <div>
            日志文件较大，只读取了末尾一块（约 4 MB）。更早的请求未计入，选「全部」也不包含它们。
          </div>
        </div>
      )}

      <div className="panel">
        <header>
          <h2>请求统计</h2>
          <span className="sub">
            {empty ? '窗口内没有请求' : `${data.totalRequests} 次请求`}
          </span>
          <div className="spacer" />
          <span className="dim mono" style={{ fontSize: 11 }}>
            {data.firstTs && data.lastTs
              ? `${data.firstTs.replace('T', ' ')} → ${data.lastTs.replace('T', ' ')}`
              : '—'}
          </span>
        </header>

        <div className="toolbar">
          <div className="tabs">
            {WINDOWS.map((w) => (
              <button
                key={w.label}
                aria-selected={hours === w.hours}
                onClick={() => setHours(w.hours)}
              >
                {w.label}
              </button>
            ))}
          </div>
          <div className="spacer" />
          <label>
            <input type="checkbox" checked={auto} onChange={(e) => setAuto(e.target.checked)} />
            自动 10s
          </label>
          <button className="btn" onClick={() => void load(true)} disabled={busy}>
            {busy ? <span className="spin" /> : null}
            刷新
          </button>
        </div>

        <div className="body">
          <div className="stats">
            <div className="stat">
              <div className="k">请求数</div>
              <div className="v">{data.totalRequests}</div>
            </div>
            <div className="stat">
              <div className="k">成功率</div>
              <div className={'v ' + (empty ? '' : rateClass(data.successRate))}>
                {empty ? '—' : fmtPct(data.successRate)}
              </div>
            </div>
            <div className="stat">
              <div className="k">失败</div>
              <div className={'v ' + (data.errors ? 'err' : '')}>{data.errors}</div>
            </div>
            <div className="stat">
              <div className="k">TTFB 中位</div>
              <div className="v" style={{ fontSize: 19 }}>
                {fmtMs(data.ttfb.p50)}
              </div>
            </div>
            <div className="stat">
              <div className="k">TTFB p95</div>
              <div className="v" style={{ fontSize: 19 }}>
                {fmtMs(data.ttfb.p95)}
              </div>
            </div>
            <div className="stat">
              <div className="k">输出 token</div>
              <div className="v" style={{ fontSize: 19 }}>
                {fmtTokens(data.tokensTotal)}
              </div>
            </div>
            <div className="stat">
              <div className="k">tok/s 均值</div>
              <div className="v" style={{ fontSize: 19 }}>
                {fmtNum(data.tokps.avg)}
              </div>
            </div>
          </div>

          <div className="dim" style={{ fontSize: 11.5, marginTop: 10, lineHeight: 1.7 }}>
            TTFB 样本 {data.ttfb.samples}（仅流式有值，非流式恒为 —），最大 {fmtMs(data.ttfb.max)}；
            tok/s 样本 {data.tokps.samples}，p50 {fmtNum(data.tokps.p50)} / p95 {fmtNum(data.tokps.p95)}。
            上游未返回 usage 的请求不计入 token 与 tok/s。
          </div>

          {data.statusCounts.length > 0 && (
            <div className="row" style={{ marginTop: 12, gap: 8 }}>
              {data.statusCounts.map((s) => (
                <span
                  key={s.status}
                  className={'badge ' + (s.status === 200 ? 'ok' : 'err')}
                  title="HTTP 状态码分布"
                >
                  {s.status} × {s.count}
                </span>
              ))}
            </div>
          )}
        </div>
      </div>

      {!empty && (
        <div className="grid-2 eq">
          <GroupTable
            title="按模型"
            groups={data.byModel}
            keyHeader="模型"
            note="模型名取自网关日志，已被截断到 11 字符（deepseek-v4 实为 deepseek-v4.1-flash）"
          />
          <GroupTable
            title="按账号"
            groups={data.byAccount}
            keyHeader="账号"
            note="只显示 uid 前 8 位（日志里就是这么记的），可与「总览」页的账号前缀对照"
          />
        </div>
      )}
    </>
  )
}

function GroupTable({
  title,
  groups,
  keyHeader,
  note,
}: {
  title: string
  groups: StatGroup[]
  keyHeader: string
  note: string
}) {
  return (
    <div className="panel">
      <header>
        <h2>{title}</h2>
        <span className="sub">{groups.length}</span>
      </header>
      <div className="body">
        <div className="dim" style={{ fontSize: 11, marginBottom: 8, whiteSpace: 'nowrap' }}>
          {note}
        </div>
        {groups.length === 0 ? (
          <div className="empty">无数据。</div>
        ) : (
          <div className="scroll-x">
            <table>
              <thead>
                <tr>
                  <th>{keyHeader}</th>
                  <th>请求</th>
                  <th>成功率</th>
                  <th>TTFB 均值</th>
                  <th>tok/s</th>
                  <th>token</th>
                </tr>
              </thead>
              <tbody>
                {groups.map((g) => (
                  <tr key={g.key}>
                    <td className="mono" style={{ color: 'var(--text-strong)' }}>
                      {g.key}
                    </td>
                    <td className="mono">{g.requests}</td>
                    <td className="mono">
                      <span className={rateClass(g.successRate)} style={{ color: `var(--${rateClass(g.successRate)})` }}>
                        {fmtPct(g.successRate)}
                      </span>
                      {g.errors > 0 && <span className="dim"> ({g.errors} 失败)</span>}
                    </td>
                    <td className="mono">{fmtMs(g.avgTtfbMs)}</td>
                    <td className="mono">{fmtNum(g.avgTokps)}</td>
                    <td className="mono">{fmtTokens(g.tokens)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </div>
  )
}
