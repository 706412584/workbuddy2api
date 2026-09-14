import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { LogTail } from '../api/types'
import { ApiError, getLogs } from '../api/client'

/** 结构化 chat 行：| #12 | 15:02:12 | model | stream | 200 | uid=… | TTFB=… | */
const CHAT_ROW_RE = /^\|\s*#\d+\s*\|.*\|\s*(\d{3})\s*\|/

/**
 * 判断一行是否「有问题」。
 * 两类来源：结构化 chat 行的非 200 状态码，以及散落在正文里的上游报错
 * （如 `upstream 429 soft_rate`、`travel … (http 500)`）。
 * 刻意保守：宁可漏报也不要把 `tok=499` 这种数字误判成 5xx。
 */
function isProblem(line: string): boolean {
  const m = CHAT_ROW_RE.exec(line)
  if (m) return m[1] !== '200'
  if (/(?:\bhttp\b|\bcode\b|\bupstream\b|\bstatus\b)[^\d]{0,4}[45]\d\d\b/i.test(line)) return true
  return /panic|fatal|\berror\b|错误|失败|异常|拒绝|已放弃/i.test(line)
}

const LINE_CHOICES = [200, 500, 1000, 2000] as const

export function Logs() {
  const [data, setData] = useState<LogTail | null>(null)
  const [lines, setLines] = useState<number>(500)
  const [q, setQ] = useState('')
  const [onlyProblem, setOnlyProblem] = useState(false)
  const [auto, setAuto] = useState(true)
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  const [note, setNote] = useState('')
  /** 是否自动滚到底部（看实时日志时用） */
  const [follow, setFollow] = useState(true)

  const preRef = useRef<HTMLPreElement>(null)
  const alive = useRef(true)
  useEffect(() => {
    alive.current = true
    return () => {
      alive.current = false
    }
  }, [])

  // showBusy 只在用户主动点「刷新」时为真：5s 轮询跟着点亮忙碌态的话，按钮文字在
  // 「刷新/刷新中」间来回切，宽度一变就推动整条工具条 —— 看起来就是页面在闪。
  const load = useCallback(async (showBusy = false) => {
    if (showBusy) setBusy(true)
    try {
      const r = await getLogs(lines)
      if (!alive.current) return
      // 内容没变就不 setState：自动刷新下避免每 5s 无谓重渲染上千行
      setData((prev) =>
        prev && prev.lines.join('\n') === r.lines.join('\n') ? { ...prev, ...r } : r,
      )
      setErr('')
    } catch (e) {
      if (!alive.current) return
      setErr(
        e instanceof ApiError
          ? `日志接口不可用：[HTTP ${e.status}] ${e.message}`
          : `日志接口不可用：${String(e)}。该功能由 vite dev/preview server 提供。`,
      )
    } finally {
      if (showBusy && alive.current) setBusy(false)
    }
  }, [lines])

  useEffect(() => {
    void load()
  }, [load])

  useEffect(() => {
    if (!auto) return
    const t = setInterval(() => void load(false), 5000)
    return () => clearInterval(t)
  }, [auto, load])

  const rows = useMemo(() => {
    const all = data?.lines ?? []
    const needle = q.trim().toLowerCase()
    return all.filter((l) => {
      if (onlyProblem && !isProblem(l)) return false
      if (needle && !l.toLowerCase().includes(needle)) return false
      return true
    })
  }, [data, q, onlyProblem])

  // 跟随底部：只在开启且没有筛选时做，否则会把用户正在看的内容顶走
  useEffect(() => {
    if (!follow || q.trim() || onlyProblem) return
    const el = preRef.current
    if (el) el.scrollTop = el.scrollHeight
  }, [rows, follow, q, onlyProblem])

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(rows.join('\n'))
      setNote(`已复制 ${rows.length} 行到剪贴板`)
    } catch (e) {
      setNote(`复制失败：${String(e)}。可手动全选下方文本。`)
    }
  }

  const problemCount = useMemo(() => (data?.lines ?? []).filter(isProblem).length, [data])

  return (
    <>
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

      <div className="panel">
        <header>
          <h2>网关日志</h2>
          <span className="sub">
            {rows.length} / {data?.lines.length ?? 0} 行
            {problemCount > 0 && (
              <>
                {' · '}
                <span style={{ color: 'var(--warn)' }}>{problemCount} 行异常</span>
              </>
            )}
          </span>
          <div className="spacer" />
          <span className="dim mono" style={{ fontSize: 11 }}>
            {data?.mtime ? new Date(data.mtime).toLocaleTimeString('zh-CN', { hour12: false }) : '—'}
          </span>
        </header>

        <div className="toolbar">
          <select
            value={lines}
            onChange={(e) => setLines(Number(e.target.value))}
            title="只取文件末尾这么多行（服务端按字节截尾，不会整读大文件）"
          >
            {LINE_CHOICES.map((n) => (
              <option key={n} value={n}>
                末尾 {n} 行
              </option>
            ))}
          </select>
          <input
            placeholder="过滤文本…"
            value={q}
            onChange={(e) => setQ(e.target.value)}
            style={{ width: 200 }}
          />
          <label title="只看非 200 的请求行，以及含上游报错的行">
            <input
              type="checkbox"
              checked={onlyProblem}
              onChange={(e) => setOnlyProblem(e.target.checked)}
            />
            只看异常
          </label>
          <label>
            <input type="checkbox" checked={follow} onChange={(e) => setFollow(e.target.checked)} />
            跟随底部
          </label>
          <label>
            <input type="checkbox" checked={auto} onChange={(e) => setAuto(e.target.checked)} />
            自动 5s
          </label>
          <div className="spacer" />
          <button className="btn" onClick={() => void load(true)} disabled={busy}>
            {busy ? <span className="spin" /> : null}
            刷新
          </button>
          <button className="btn" onClick={() => void copy()} disabled={rows.length === 0}>
            复制
          </button>
        </div>

        <div className="body">
          <div className="dim mono" style={{ fontSize: 11, marginBottom: 8 }}>
            {data?.path ?? '—'}
            {data?.size ? ` · ${(data.size / 1024).toFixed(1)} KB` : ''}
            {data?.truncated ? ' · 已截断（文件更早的部分未显示）' : ''}
          </div>

          {rows.length === 0 ? (
            <div className="empty">
              {data?.lines.length ? '没有匹配的日志行。' : '日志为空 —— 网关尚未产生输出。'}
            </div>
          ) : (
            <pre className="logview" ref={preRef}>
              {rows.map((l, i) => (
                <div key={i} className={isProblem(l) ? 'logline problem' : 'logline'}>
                  {l || '\u00a0'}
                </div>
              ))}
            </pre>
          )}
        </div>
      </div>
    </>
  )
}
