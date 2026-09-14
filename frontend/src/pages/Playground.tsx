import { useEffect, useRef, useState } from 'react'
import type { ModelList, ProtocolId } from '../api/types'
import { PROTOCOLS } from '../api/types'
import { ApiError, chatOnce, streamChat } from '../api/client'

interface Props {
  apiKey: string
  models: ModelList | null
}

type Phase = 'idle' | 'running' | 'done' | 'stopped' | 'error'

/** 原始帧最多留这么多，避免上万 token 的长回复把内存和渲染拖垮。 */
const RAW_FRAME_CAP = 300

/** 历史里最多留这么多趟，防止长回复把内存撑爆。 */
const HISTORY_CAP = 20

const DEFAULT_SYSTEM = 'You are a helpful assistant.'

/** 一趟已结束的请求。新一轮会清空响应区，所以结束时把结果整份存下来。 */
interface Run {
  id: number
  /** 本地时刻 HH:MM:SS */
  at: string
  protocol: ProtocolId
  model: string
  system: string
  prompt: string
  stream: boolean
  phase: 'done' | 'stopped' | 'error'
  text: string
  thinking: string
  meta: string
  error: string
  rawJSON: string
  raw: string
}

export function Playground({ apiKey, models }: Props) {
  const [protocol, setProtocol] = useState<ProtocolId>('openai')
  const [model, setModel] = useState('deepseek-v4.1-flash')
  const [system, setSystem] = useState(DEFAULT_SYSTEM)
  const [prompt, setPrompt] = useState('用一句话介绍你自己。')
  const [maxTokens, setMaxTokens] = useState(1024)
  const [stream, setStream] = useState(true)

  const [phase, setPhase] = useState<Phase>('idle')
  const [text, setText] = useState('')
  const [thinking, setThinking] = useState('')
  const [raw, setRaw] = useState('')
  const [meta, setMeta] = useState('')
  const [error, setError] = useState('')
  const [rawJSON, setRawJSON] = useState('')

  /** 已结束的请求，新的在前。下一轮 reset 只清实时区，不动它。 */
  const [runs, setRuns] = useState<Run[]>([])
  /** 正在看哪一趟历史；null = 看实时内容 */
  const [sel, setSel] = useState<number | null>(null)
  const seq = useRef(0)

  // 增量先写进 ref、按动画帧刷进 state：否则每个 token 一次 setState，
  // 上万 token 的长回复会把主线程压住。
  const buf = useRef({ text: '', thinking: '', raw: [] as string[], dropped: 0 })
  const raf = useRef(0)
  const abort = useRef<AbortController | null>(null)

  /** 原始帧文本（含被丢弃帧的提示），实时渲染与历史快照共用同一份拼法。 */
  const rawText = () =>
    buf.current.raw.join('') + (buf.current.dropped ? `\n… 另有 ${buf.current.dropped} 帧未显示\n` : '')

  const flush = () => {
    if (raf.current) return
    raf.current = requestAnimationFrame(() => {
      raf.current = 0
      setText(buf.current.text)
      setThinking(buf.current.thinking)
      setRaw(rawText())
    })
  }

  useEffect(() => {
    return () => {
      if (raf.current) cancelAnimationFrame(raf.current)
      abort.current?.abort()
    }
  }, [])

  const reset = () => {
    buf.current = { text: '', thinking: '', raw: [], dropped: 0 }
    setText('')
    setThinking('')
    setRaw('')
    setRawJSON('')
    setMeta('')
    setError('')
  }

  /**
   * 一趟结束就整份存进历史。实时区稍后会被下一轮清掉，历史是唯一的留底。
   * 参数由调用方传入而非在这里读 state：请求发出后用户还能改输入框，
   * 结束时读 state 会把这趟的 prompt 记成改过之后的那个。
   */
  const archive = (
    req: Pick<Run, 'protocol' | 'model' | 'system' | 'prompt' | 'stream'>,
    r: Omit<Run, 'id' | 'at' | 'protocol' | 'model' | 'system' | 'prompt' | 'stream'>,
  ) => {
    const id = ++seq.current
    const at = new Date().toLocaleTimeString('zh-CN', { hour12: false })
    setRuns((list) => [{ id, at, ...req, ...r }, ...list].slice(0, HISTORY_CAP))
  }

  const send = async () => {
    if (!apiKey) {
      setError('请先在右上角填入 API 密钥。')
      setPhase('error')
      return
    }
    if (!model.trim() || !prompt.trim()) {
      setError('模型与输入内容都不能为空。')
      setPhase('error')
      return
    }

    reset()
    setSel(null) // 新一轮开始，实时区重新成为焦点
    setPhase('running')
    const ctl = new AbortController()
    abort.current = ctl

    // 这一趟的参数在这里定死，后面存档用它，不再读 state。
    const req = {
      protocol,
      model: model.trim(),
      system: system.trim(),
      prompt,
      stream,
    }
    const common = { ...req, key: apiKey, maxTokens }

    // meta 用局部变量拼、结束时一次性写入：既给实时区，也给历史快照 ——
    // 靠函数式 setState 累加的话，结束时读不到最终值。
    let tok = 0
    let finish = ''

    try {
      if (!stream) {
        const res = await chatOnce(common)
        const json = JSON.stringify(res, null, 2)
        const msg = extractText(protocol, res)
        setRawJSON(json)
        setText(msg)
        setMeta('非流式')
        setPhase('done')
        archive(req, {
          phase: 'done',
          text: msg,
          thinking: '',
          meta: '非流式',
          error: '',
          rawJSON: json,
          raw: '',
        })
        return
      }

      const ms = await streamChat({
        ...common,
        signal: ctl.signal,
        onDelta: (d) => {
          if (d.text) buf.current.text += d.text
          if (d.thinking) buf.current.thinking += d.thinking
          if (d.tokens) tok = d.tokens
          if (d.finish) finish = d.finish
          flush()
        },
        onRaw: (f) => {
          if (buf.current.raw.length < RAW_FRAME_CAP) {
            buf.current.raw.push(`event: ${f.event || '(无)'}\ndata: ${f.data}\n\n`)
          } else {
            buf.current.dropped++
          }
        },
      })
      flush()
      const finalMeta = [
        `${(ms / 1000).toFixed(2)}s`,
        tok ? `tok=${tok}` : '',
        finish ? `finish=${finish}` : '',
      ]
        .filter(Boolean)
        .join(' ')
      setMeta(finalMeta)
      setPhase('done')
      archive(req, {
        phase: 'done',
        text: buf.current.text,
        thinking: buf.current.thinking,
        meta: finalMeta,
        error: '',
        rawJSON: '',
        raw: rawText(),
      })
    } catch (e) {
      if (ctl.signal.aborted) {
        flush()
        const m = ['已手动停止', tok ? `tok=${tok}` : '', finish ? `finish=${finish}` : '']
          .filter(Boolean)
          .join(' ')
        setMeta(m)
        setPhase('stopped')
        archive(req, {
          phase: 'stopped',
          text: buf.current.text,
          thinking: buf.current.thinking,
          meta: m,
          error: '',
          rawJSON: '',
          raw: rawText(),
        })
        return
      }
      const msg =
        e instanceof ApiError
          ? `[HTTP ${e.status}${e.code ? ' ' + e.code : ''}] ${e.message}`
          : String(e)
      flush()
      setError(msg)
      setPhase('error')
      archive(req, {
        phase: 'error',
        text: buf.current.text,
        thinking: buf.current.thinking,
        meta: '',
        error: msg,
        rawJSON: '',
        raw: rawText(),
      })
    } finally {
      abort.current = null
    }
  }

  const stop = () => abort.current?.abort()

  const running = phase === 'running'
  const modelIds = (models?.data ?? []).map((m) => m.id)
  const path = PROTOCOLS.find((p) => p.id === protocol)!.path

  const shown = sel === null ? null : (runs.find((r) => r.id === sel) ?? null)
  const view = shown ?? {
    phase,
    text,
    thinking,
    meta,
    error,
    rawJSON,
    raw,
    at: '',
    protocol,
    model,
    system,
    prompt,
    stream,
  }

  /** 把历史那趟的参数搬回左侧表单，方便改一改重跑。 */
  const load = (r: Run) => {
    setProtocol(r.protocol)
    setModel(r.model)
    setSystem(r.system)
    setPrompt(r.prompt)
    setStream(r.stream)
  }

  return (
    <div className="grid-2">
      <div className="panel">
        <header>
          <h2>请求</h2>
        </header>
        <div className="body">
          <div className="field">
            <label>协议</label>
            <div className="tabs">
              {PROTOCOLS.map((p) => (
                <button
                  key={p.id}
                  aria-selected={protocol === p.id}
                  onClick={() => setProtocol(p.id)}
                  disabled={running}
                >
                  {p.label}
                </button>
              ))}
            </div>
            <div className="dim mono" style={{ fontSize: 11, marginTop: 6 }}>
              POST {path}
            </div>
          </div>

          <div className="field">
            <label>模型（网关会映射成上游模型名）</label>
            <input
              list="model-ids"
              value={model}
              onChange={(e) => setModel(e.target.value)}
              style={{ width: '100%' }}
            />
            <datalist id="model-ids">
              {modelIds.map((id) => (
                <option key={id} value={id} />
              ))}
            </datalist>
          </div>

          <div className="field">
            <label>
              {protocol === 'responses' ? 'instructions（系统提示）' : 'system（系统提示）'}
            </label>
            <textarea
              rows={3}
              value={system}
              onChange={(e) => setSystem(e.target.value)}
              style={{ width: '100%' }}
            />
          </div>

          <div className="field">
            <label>输入</label>
            <textarea
              rows={6}
              value={prompt}
              onChange={(e) => setPrompt(e.target.value)}
              style={{ width: '100%' }}
            />
          </div>

          <div className="field">
            <label>max_tokens（仅 Anthropic 协议需要）</label>
            <input
              type="number"
              min={1}
              max={128000}
              value={maxTokens}
              onChange={(e) => setMaxTokens(Number(e.target.value) || 1)}
              style={{ width: 120 }}
            />
          </div>

          <div className="row">
            <button className="btn primary" onClick={send} disabled={running}>
              {running ? <span className="spin" /> : null}
              {running ? ' 生成中' : '发送'}
            </button>
            <button className="btn danger" onClick={stop} disabled={!running}>
              停止
            </button>
            <label style={{ margin: 0, display: 'flex', gap: 6, alignItems: 'center', cursor: 'pointer' }}>
              <input
                type="checkbox"
                checked={stream}
                onChange={(e) => setStream(e.target.checked)}
                disabled={running}
              />
              流式
            </label>
          </div>
        </div>
      </div>

      <div className="panel">
        <header>
          <h2>响应</h2>
          <div className="spacer" />
          <span className="meta">
            {shown ? (
              <>
                <span className="badge accent">历史 #{shown.id}</span>
                <span>{shown.at}</span>
                <span>{PROTOCOLS.find((p) => p.id === shown.protocol)!.label}</span>
                <span className="mono">{shown.model}</span>
              </>
            ) : (
              <>
                {running && <span className="badge ok">流式接收中</span>}
                {phase === 'done' && <span className="badge ok">完成</span>}
                {phase === 'stopped' && <span className="badge warn">已停止</span>}
                {phase === 'error' && <span className="badge err">失败</span>}
              </>
            )}
            {view.meta && <span>{view.meta}</span>}
            {shown && (
              <button className="btn" onClick={() => setSel(null)} title="回到最新一轮的实时结果">
                返回实时
              </button>
            )}
          </span>
        </header>
        <div className="body">
          {view.error && (
            <div className="note err" style={{ marginBottom: 12 }}>
              <span>●</span>
              <div style={{ wordBreak: 'break-word' }}>{view.error}</div>
            </div>
          )}

          <div className="output">
            {view.thinking && <div className="thinking">{view.thinking}</div>}
            {view.text}
            {running && !shown && <span className="caret" />}
            {!view.text && !view.thinking && !running && !view.error && (
              <span className="dim">响应会显示在这里。</span>
            )}
          </div>

          {view.rawJSON && (
            <details className="raw" open>
              <summary>原始 JSON 响应</summary>
              <pre>{view.rawJSON}</pre>
            </details>
          )}

          {view.raw && (
            <details className="raw">
              <summary>原始协议事件（用于核对协议转换）</summary>
              <pre>{view.raw}</pre>
            </details>
          )}
        </div>
      </div>

      {runs.length > 0 && (
        <div className="panel" style={{ gridColumn: '1 / -1' }}>
          <header>
            <h2>历史</h2>
            <span className="sub">
              {runs.length} 趟 · 留在内存里，刷新页面或切走标签页即清空
            </span>
            <div className="spacer" />
            <button className="btn" onClick={() => setRuns([])}>
              清空
            </button>
          </header>
          <div className="body">
            <table>
              <thead>
                <tr>
                  <th>时间</th>
                  <th>协议</th>
                  <th>模型</th>
                  <th>结果</th>
                  <th>输入</th>
                  <th>正文</th>
                  <th>操作</th>
                </tr>
              </thead>
              <tbody>
                {runs.map((r) => (
                  <tr key={r.id} style={r.id === sel ? { background: 'var(--bg-raised)' } : undefined}>
                    <td className="mono dim nowrap">{r.at}</td>
                    <td className="dim nowrap">
                      {PROTOCOLS.find((p) => p.id === r.protocol)!.label}
                      {!r.stream && <span className="badge" style={{ marginLeft: 6 }}>非流式</span>}
                    </td>
                    <td className="mono nowrap">{r.model}</td>
                    <td className="nowrap">
                      {r.phase === 'done' && <span className="badge ok">完成</span>}
                      {r.phase === 'stopped' && <span className="badge warn">已停止</span>}
                      {r.phase === 'error' && <span className="badge err">失败</span>}
                      {r.meta && <span className="dim mono" style={{ fontSize: 11, marginLeft: 6 }}>{r.meta}</span>}
                    </td>
                    <td className="dim" style={{ maxWidth: 240 }}>
                      <div style={{ whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>
                        {r.prompt}
                      </div>
                    </td>
                    <td className="dim" style={{ maxWidth: 260 }}>
                      <div style={{ whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>
                        {r.error || r.text || r.thinking || '—'}
                      </div>
                    </td>
                    <td>
                      <div className="row" style={{ flexWrap: 'nowrap' }}>
                        <button className="btn" onClick={() => setSel(r.id)} disabled={r.id === sel}>
                          查看
                        </button>
                        <button className="btn" onClick={() => load(r)} title="把这趟的参数填回左侧表单">
                          重填
                        </button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}
    </div>
  )
}

/** 从非流式响应里按协议取出正文，避免只显示一坨 JSON。 */
function extractText(protocol: ProtocolId, res: Record<string, any>): string {
  if (protocol === 'openai') return res?.choices?.[0]?.message?.content ?? ''
  if (protocol === 'anthropic') {
    const blocks = res?.content
    if (!Array.isArray(blocks)) return ''
    return blocks
      .filter((b: any) => b?.type === 'text')
      .map((b: any) => b.text)
      .join('')
  }
  const out = res?.output
  if (!Array.isArray(out)) return ''
  return out
    .flatMap((item: any) => (Array.isArray(item?.content) ? item.content : []))
    .filter((c: any) => typeof c?.text === 'string')
    .map((c: any) => c.text)
    .join('')
}
