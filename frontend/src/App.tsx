import { useCallback, useEffect, useMemo, useState } from 'react'
import type { APIKeyEntry, Health, ModelList, PoolStatus } from './api/types'
import {
  ApiError,
  getAdminToken,
  getAPIKeys,
  getHealth,
  getModels,
  getStatus,
  setAdminToken,
} from './api/client'
import { Overview } from './pages/Overview'
import { Models } from './pages/Models'
import { Playground } from './pages/Playground'
import { Accounts } from './pages/Accounts'
import { APIKeys } from './pages/APIKeys'
import { Logs } from './pages/Logs'
import { Stats } from './pages/Stats'

type Tab = 'overview' | 'stats' | 'accounts' | 'apikeys' | 'models' | 'playground' | 'logs'

const KEY_STORAGE = 'wb2api.admin.key'
const POLL_MS = 5000

const TABS: { id: Tab; label: string }[] = [
  { id: 'overview', label: '总览' },
  { id: 'stats', label: '统计' },
  { id: 'accounts', label: '账号' },
  { id: 'apikeys', label: '密钥' },
  { id: 'models', label: '模型' },
  { id: 'playground', label: '调试台' },
  { id: 'logs', label: '日志' },
]

/** 下拉里的一个选项：config.json 里的一把密钥。value 就是密钥本身。 */
const MANUAL = '__manual__'

/**
 * 顶栏的密钥选择器。
 *
 * 下拉列出 config.json 里已登记的密钥（含顶层 api_key），选中即切换 —— 手敲 64 位
 * 十六进制串太容易出错，而且敲错只能等 401 才知道。
 * 但密钥未必都在 config.json 里（临时密钥、刚改过配置没重启），所以保留手输：
 * 选中「手动输入…」就退回原来的文本框。当前值不在表里时自动进手输态。
 */
function KeyPicker({
  value,
  keys,
  legacyKey,
  onChange,
}: {
  value: string
  keys: APIKeyEntry[]
  legacyKey: string
  onChange: (v: string) => void
}) {
  const [manual, setManual] = useState(false)
  const [show, setShow] = useState(false)

  /** 表里所有可选密钥，按「名称 / 区域」生成标签。 */
  const options = useMemo(() => {
    const out: { key: string; label: string }[] = []
    if (legacyKey.trim()) out.push({ key: legacyKey.trim(), label: '顶层 api_key · 不限区域' })
    for (const [i, k] of keys.entries()) {
      if (!k.key.trim()) continue
      const name = k.name.trim() || `密钥 ${i + 1}`
      out.push({ key: k.key.trim(), label: `${name} · ${k.region || '不限区域'}` })
    }
    return out
  }, [keys, legacyKey])

  // 当前值不在表里（手输的、或配置改过还没重启）→ 直接进手输态，别让下拉显示空
  const inTable = options.some((o) => o.key === value)
  const isManual = manual || (value !== '' && !inTable)

  if (isManual || options.length === 0) {
    return (
      <>
        <input
          type={show ? 'text' : 'password'}
          placeholder={options.length === 0 ? '粘贴 API 密钥…' : '手动输入密钥…'}
          value={value}
          onChange={(e) => onChange(e.target.value)}
          style={{ width: 320 }}
          spellCheck={false}
        />
        <button className="btn" onClick={() => setShow((v) => !v)}>
          {show ? '隐藏' : '显示'}
        </button>
        {options.length > 0 && (
          <button
            className="btn"
            onClick={() => {
              setManual(false)
              setShow(false)
              onChange(options[0].key)
            }}
            title="回到下拉选择"
          >
            选择已有
          </button>
        )}
      </>
    )
  }

  return (
    <>
      <select
        value={value}
        onChange={(e) => {
          if (e.target.value === MANUAL) {
            setManual(true)
            onChange('')
          } else {
            onChange(e.target.value)
          }
        }}
        style={{ width: 320 }}
        title={value ? `当前密钥：${value}` : '未选择密钥'}
      >
        <option value="">未选择密钥（网关不校验鉴权）</option>
        {options.map((o) => (
          <option key={o.key} value={o.key}>
            {o.label}
          </option>
        ))}
        <option value={MANUAL}>手动输入…</option>
      </select>
      <button className="btn" onClick={() => setManual(true)} title="改用手动输入">
        手动
      </button>
    </>
  )
}

/**
 * 管理员口令输入。仅局域网访问 /__admin/* 时需要；本机访问服务端不校验。
 *
 * 为什么独立于 API 密钥：/__admin/* 能增删账号、改写密钥表，给它一把独立凭证。
 * 平时不占地方 —— 只有填过值、或最近一次管理请求被 401 拒绝时才展开。
 */
function AdminTokenPicker({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  const [open, setOpen] = useState(value !== '')
  const [show, setShow] = useState(false)

  if (!open) {
    return (
      <button
        className="btn"
        title="局域网访问管理接口需要口令（config.json 的 admin.token）；本机访问不需要"
        onClick={() => setOpen(true)}
      >
        管理口令
      </button>
    )
  }
  return (
    <>
      <input
        type={show ? 'text' : 'password'}
        placeholder="管理口令（本机可留空）"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        style={{ width: 200 }}
        spellCheck={false}
        title="对应 config.json 的 admin.token。本机访问时留空即可。"
      />
      <button className="btn" onClick={() => setShow((v) => !v)}>
        {show ? '隐藏' : '显示'}
      </button>
    </>
  )
}

export default function App() {
  const [tab, setTab] = useState<Tab>('overview')
  const [apiKey, setApiKey] = useState(() => localStorage.getItem(KEY_STORAGE) ?? '')
  const [adminTok, setAdminTok] = useState(() => getAdminToken())

  const [status, setStatus] = useState<PoolStatus | null>(null)
  const [health, setHealth] = useState<Health | null>(null)
  const [models, setModels] = useState<ModelList | null>(null)
  const [err, setErr] = useState('')
  const [updatedAt, setUpdatedAt] = useState<Date | null>(null)
  const [autoRefresh, setAutoRefresh] = useState(true)
  const [busy, setBusy] = useState(false)
  const [now, setNow] = useState(() => Date.now())

  useEffect(() => {
    if (apiKey) localStorage.setItem(KEY_STORAGE, apiKey)
    else localStorage.removeItem(KEY_STORAGE)
  }, [apiKey])

  // 口令存进 client.ts 的模块级变量（adminJSON 读它），并持久化。
  useEffect(() => {
    setAdminToken(adminTok)
  }, [adminTok])

  // 密钥绑定的区域只有 config.json 知道（网关不回传），所以读一次面板自己的
  // /__admin/apikeys。只在挂载时读一次：apiKey 每敲一个字符就变，跟着它请求太浪费；
  // 一张表里存着全部密钥，读一次就够查任何一把。
  const [keyTable, setKeyTable] = useState<APIKeyEntry[]>([])
  const [legacyKey, setLegacyKey] = useState('')
  // 管理接口鉴权失败的提示。与 err 分开：err 每次 refresh 都会被清掉，
  // 而这条提示要一直留到用户填对口径为止。
  const [adminHint, setAdminHint] = useState('')
  useEffect(() => {
    getAPIKeys()
      .then((r) => {
        setKeyTable(r.keys)
        setLegacyKey(r.legacyKey)
        setAdminHint('')
      })
      .catch((e) => {
        // 取不到就一律当不限区域：只是提示语措辞不同，不影响模型列表。
        // 但 401/403 要明说 —— 局域网访问时这是「还没填管理口令」的唯一信号，
        // 静默吞掉会让账号页、日志页一起空白而看不出原因。
        if (e instanceof ApiError && (e.status === 401 || e.status === 403)) {
          setAdminHint(
            e.status === 401
              ? '管理口令无效（401）。请在右上角「管理口令」填入 config.json 的 admin.token。'
              : '管理接口仅允许本机访问（403）。若要从局域网使用，请在 config.json 配置 admin.token 后重启网关。',
          )
        }
      })
    // adminTok 变化时重试：填完口令应当立即生效，不必手动刷新页面。
  }, [adminTok])

  // healthz 不需要鉴权，所以即使没填密钥也照探——这样能区分「网关没起来」和「密钥不对」。
  //
  // showBusy 只在用户主动点「刷新」时为真。5s 轮询是背景行为，跟着点亮忙碌态的话，
  // 按钮文字在「刷新/刷新中」之间来回切，宽度一变就把左边的密钥输入框整体推走几十像素
  // —— 看起来就是整页在闪。轮询因此走静默路径，界面不动。
  const refresh = useCallback(async (showBusy = false) => {
    if (showBusy) setBusy(true)
    try {
      const h = await getHealth()
      setHealth(h)
      if (!apiKey) {
        setStatus(null)
        setModels(null)
        setErr('')
        setUpdatedAt(new Date())
        return
      }
      const [s, m] = await Promise.all([getStatus(apiKey), getModels(apiKey)])
      setStatus(s)
      setModels(m)
      setErr('')
      setUpdatedAt(new Date())
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) {
        setErr('密钥被拒绝（401）。请确认填的是 config.json 里 api_key 或 api_keys 中的值。')
      } else if (e instanceof ApiError) {
        setErr(`[HTTP ${e.status}${e.code ? ' ' + e.code : ''}] ${e.message}`)
      } else {
        setErr(
          `请求网关失败：${String(e)}。若网关未启动，或 vite 代理目标不是它，都会走到这里。`,
        )
      }
    } finally {
      if (showBusy) setBusy(false)
    }
  }, [apiKey])

  useEffect(() => {
    void refresh()
  }, [refresh])

  useEffect(() => {
    if (!autoRefresh) return
    const t = setInterval(() => void refresh(false), POLL_MS)
    return () => clearInterval(t)
  }, [autoRefresh, refresh])

  // 冷却/熔断是倒计时，需要本地秒级走字；没有这类账号时不必每秒重渲染整张表。
  const hasCountdown = !!status?.accounts.some(
    (a) => a.cooling || (a.breaker_until && !a.breaker_until.startsWith('0001-01-01')),
  )

  useEffect(() => {
    if (!hasCountdown) return
    const t = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(t)
  }, [hasCountdown])

  const gatewayUp = health !== null
  const healthy = health?.healthy ?? 0
  /** 当前密钥在 config.json 里绑的区域；查不到（旧版顶层 api_key / 刚敲一半）即不限区域 */
  const keyRegion = keyTable.find((k) => k.key === apiKey)?.region ?? ''

  return (
    <div className="app">
      <aside className="sidebar">
        <div className="brand">
          <h1>workbuddy2api</h1>
          <p>管理面板</p>
        </div>

        <nav className="nav">
          {TABS.map((t) => (
            <button key={t.id} aria-current={tab === t.id} onClick={() => setTab(t.id)}>
              <span
                className="dot"
                style={{
                  background:
                    t.id === 'overview'
                      ? gatewayUp
                        ? healthy
                          ? 'var(--ok)'
                          : 'var(--err)'
                        : 'var(--text-dim)'
                      : 'var(--border-strong)',
                }}
              />
              {t.label}
            </button>
          ))}
        </nav>

        <div style={{ marginTop: 'auto', fontSize: 11.5, lineHeight: 1.7 }} className="dim">
          <div>
            网关 <span className="mono">127.0.0.1:7863</span>
          </div>
          <div>
            本页 <span className="mono">{location.host}</span>
          </div>
        </div>
      </aside>

      <div className="main">
        <header className="topbar">
          <span className={'badge ' + (gatewayUp ? (healthy ? 'ok' : 'err') : 'err')}>
            {gatewayUp ? (healthy ? `在线 · ${healthy}/${health!.total} 可用` : '在线 · 无可用账号') : '网关不可达'}
          </span>

          <div className="spacer" />

          <KeyPicker
            value={apiKey}
            keys={keyTable}
            legacyKey={legacyKey}
            onChange={setApiKey}
          />
          <AdminTokenPicker value={adminTok} onChange={setAdminTok} />
          <button className="btn" onClick={() => void refresh(true)} disabled={busy}>
            {busy ? <span className="spin" /> : null}
            {busy ? ' 刷新中' : '刷新'}
          </button>
          <label style={{ margin: 0, display: 'flex', gap: 6, alignItems: 'center', cursor: 'pointer' }}>
            <input
              type="checkbox"
              checked={autoRefresh}
              onChange={(e) => setAutoRefresh(e.target.checked)}
            />
            自动 5s
          </label>
        </header>

        <main className="content">
          {err && (
            <div className="note err">
              <span>●</span>
              <div style={{ wordBreak: 'break-word' }}>{err}</div>
            </div>
          )}

          {adminHint && (
            <div className="note err">
              <span>●</span>
              <div style={{ wordBreak: 'break-word' }}>{adminHint}</div>
            </div>
          )}

          {tab === 'overview' && (
            <Overview
              status={status}
              health={health}
              now={now}
              onChanged={refresh}
              keyRegion={keyRegion}
              poolTotal={health?.total}
            />
          )}
          {tab === 'accounts' && <Accounts onChanged={refresh} />}
          {tab === 'apikeys' && <APIKeys activeKey={apiKey} onChanged={refresh} />}
          {tab === 'models' && <Models models={models} keyRegion={keyRegion} />}
          {tab === 'playground' && <Playground apiKey={apiKey} models={models} />}
          {tab === 'logs' && <Logs />}
          {tab === 'stats' && <Stats />}

          {tab === 'overview' && (
            <div className="dim" style={{ fontSize: 12, textAlign: 'right' }}>
              {updatedAt ? `更新于 ${updatedAt.toLocaleTimeString('zh-CN', { hour12: false })}` : ''}
            </div>
          )}
        </main>
      </div>
    </div>
  )
}
