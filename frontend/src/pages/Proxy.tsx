import { useCallback, useEffect, useState } from 'react'
import type { AuthFileView, ProxySlot, ProxyStatus } from '../api/types'
import { ApiError, getProxySlots, listAuthFiles, saveProxySlots, testProxy } from '../api/client'

interface Props {
  onChanged: () => void
}

/** 新建槽位的默认 URL 模板（提示格式，不是真实代理）。 */
const URL_PLACEHOLDER = 'http://user:pass@127.0.0.1:7890 或 socks5://127.0.0.1:1080'

export function Proxy({ onChanged }: Props) {
  const [status, setStatus] = useState<ProxyStatus | null>(null)
  /** 账号列表，用于「绑定」下拉框 */
  const [files, setFiles] = useState<AuthFileView[]>([])
  /** 编辑中的槽位（本地草稿，点保存才落盘） */
  const [slots, setSlots] = useState<ProxySlot[]>([])
  /** 编辑中的绑定 uid → slotId */
  const [binds, setBinds] = useState<Record<string, string>>({})
  const [err, setErr] = useState('')
  const [msg, setMsg] = useState('')
  const [busy, setBusy] = useState(false)
  /** 各槽位的探测结果（key = slotId） */
  const [tests, setTests] = useState<Record<string, { ok: boolean; text: string }>>({})
  const [testing, setTesting] = useState('')

  const load = useCallback(async () => {
    setBusy(true)
    try {
      const s = await getProxySlots()
      setStatus(s)
      setSlots(s.slots ?? [])
      setErr('')
    } catch (e) {
      setErr(e instanceof ApiError ? `代理接口不可用：[HTTP ${e.status}] ${e.message}` : String(e))
    } finally {
      setBusy(false)
    }
    // 账号列表（绑定下拉框用）。失败不阻塞代理管理本身。
    try {
      const res = await listAuthFiles()
      setFiles(res.accounts)
    } catch {
      // 忽略：没账号列表时仍可管理槽位
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  /** 绑定表以服务端返回的完整 binds 为基准（含失效绑定，见 orphanBinds）。 */
  useEffect(() => {
    if (!status) return
    setBinds((prev) => {
      // 服务端数据为准，但保留用户本次会话内尚未保存的编辑（prev 里更晚的改动）
      const merged: Record<string, string> = { ...(status.binds ?? {}) }
      for (const [uid, sid] of Object.entries(prev)) {
        if (sid) merged[uid] = sid
      }
      return merged
    })
  }, [status])

  const addSlot = () => {
    const used = new Set(slots.map((s) => s.id))
    let n = slots.length + 1
    while (used.has(`slot-${n}`)) n++
    setSlots([...slots, { id: `slot-${n}`, name: `代理 ${n}`, url: '', enabled: true }])
  }

  const updSlot = (id: string, patch: Partial<ProxySlot>) =>
    setSlots(slots.map((s) => (s.id === id ? { ...s, ...patch } : s)))

  const delSlot = (id: string) => {
    const used = Object.values(binds).filter((v) => v === id).length
    if (used > 0 && !confirm(`槽位 ${id} 被 ${used} 个账号绑定。删除后这些账号会回落直连（绑定保留为「已失效」）。确认删除？`)) {
      return
    }
    setSlots(slots.filter((s) => s.id !== id))
  }

  const doTest = async (s: ProxySlot) => {
    if (!s.url.trim()) {
      setTests({ ...tests, [s.id]: { ok: false, text: '请先填代理地址' } })
      return
    }
    setTesting(s.id)
    try {
      const r = await testProxy(s.url)
      const text = r.ok
        ? `✓ 可用（${r.dialMs ?? 0}ms 连接 + ${r.elapsedMs ?? 0}ms 请求，HTTP ${r.status}）`
        : `✗ ${r.stage === 'dial' ? '连不上代理' : '转发失败'}：${r.error ?? '未知错误'}`
      setTests({ ...tests, [s.id]: { ok: r.ok, text } })
    } catch (e) {
      setTests({ ...tests, [s.id]: { ok: false, text: String(e) } })
    } finally {
      setTesting('')
    }
  }

  const doSave = async () => {
    setErr('')
    setMsg('')
    // 前端先做一遍基础校验，省一次往返（服务端仍会再验）
    for (const s of slots) {
      if (!s.id.trim()) return setErr('有槽位缺少 id')
      if (!s.url.trim()) return setErr(`槽位 ${s.id} 缺少代理地址`)
    }
    try {
      await saveProxySlots(slots, binds)
      setMsg('已保存并热重载（不重启进程）')
      await load()
      onChanged()
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    }
  }

  return (
    <>
      <div className="note info">
        <span>●</span>
        <div>
          代理槽位集中管理，账号通过槽位 id 绑定（多个账号可共用同一槽位）。
          绑定后该账号的<b>全部出站流量</b>（chat / 模型列表 / token 刷新 / 签到 / 余额 / 旅行 / 上报）
          都走该代理；未绑定的账号直连。保存后<b>立即生效</b>，不重启进程。
        </div>
      </div>

      {err && (
        <div className="note err">
          <span>●</span>
          <div style={{ wordBreak: 'break-word' }}>{err}</div>
        </div>
      )}
      {msg && (
        <div className="note info">
          <span>●</span>
          <div>{msg}</div>
        </div>
      )}

      <div className="panel">
        <header>
          <h2>代理槽位</h2>
          <span className="sub">{slots.length} 个</span>
          <div className="spacer" />
          <button className="btn" onClick={addSlot}>
            新建槽位
          </button>
          <button className="btn" onClick={() => void load()} disabled={busy}>
            重新读取
          </button>
          <button className="btn primary" onClick={() => void doSave()}>
            保存
          </button>
        </header>

        {slots.length === 0 ? (
          <div className="empty">
            还没有代理槽位。点「新建槽位」添加 —— 未配置时所有账号都直连（与改造前一致）。
          </div>
        ) : (
          <div className="scroll-x">
            <table>
              <thead>
                <tr>
                  <th>槽位 ID</th>
                  <th>名称</th>
                  <th style={{ minWidth: 300 }}>代理地址</th>
                  <th>启用</th>
                  <th>在用</th>
                  <th>探测</th>
                  <th></th>
                </tr>
              </thead>
              <tbody>
                {slots.map((s) => {
                  const t = tests[s.id]
                  return (
                    <tr key={s.id}>
                      <td className="mono">{s.id}</td>
                      <td>
                        <input
                          value={s.name}
                          onChange={(e) => updSlot(s.id, { name: e.target.value })}
                          style={{ width: 110 }}
                        />
                      </td>
                      <td>
                        <input
                          value={s.url}
                          placeholder={URL_PLACEHOLDER}
                          onChange={(e) => updSlot(s.id, { url: e.target.value })}
                          style={{ width: '100%', fontSize: 11.5 }}
                        />
                        {t && (
                          <div
                            className="dim"
                            style={{
                              fontSize: 11,
                              marginTop: 3,
                              color: t.ok ? 'var(--ok, #2e7d32)' : 'var(--danger, #c62828)',
                              wordBreak: 'break-word',
                            }}
                          >
                            {t.text}
                          </div>
                        )}
                      </td>
                      <td>
                        <input
                          type="checkbox"
                          checked={s.enabled}
                          onChange={(e) => updSlot(s.id, { enabled: e.target.checked })}
                          title="关闭后绑定它的账号回落直连"
                        />
                      </td>
                      <td className="dim">{s.usage ?? 0}</td>
                      <td>
                        <button
                          className="btn"
                          onClick={() => void doTest(s)}
                          disabled={testing === s.id}
                          style={{ fontSize: 11.5 }}
                        >
                          {testing === s.id ? '测试中' : '测试'}
                        </button>
                      </td>
                      <td>
                        <button className="btn" onClick={() => delSlot(s.id)} style={{ fontSize: 11.5 }}>
                          删除
                        </button>
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}
      </div>

      <div className="panel">
        <header>
          <h2>账号绑定</h2>
          <span className="sub">
            {Object.values(binds).filter(Boolean).length} / {files.length} 已绑定
          </span>
        </header>
        {files.length === 0 ? (
          <div className="empty">还没有账号。</div>
        ) : (
          <div className="scroll-x">
            <table>
              <thead>
                <tr>
                  <th>账号</th>
                  <th>区域</th>
                  <th>代理</th>
                </tr>
              </thead>
              <tbody>
                {files.map((f) => {
                  const bound = binds[f.uid] ?? ''
                  const orphan = bound && !slots.some((s) => s.id === bound)
                  return (
                    <tr key={f.uid}>
                      <td>
                        <div style={{ color: 'var(--text-strong)' }}>{f.nickname || '—'}</div>
                        <div className="mono dim" style={{ fontSize: 11 }}>
                          {f.uid.slice(0, 8)}
                        </div>
                      </td>
                      <td>
                        <span className="badge">{f.region}</span>
                      </td>
                      <td>
                        <select
                          value={bound}
                          onChange={(e) => {
                            const v = e.target.value
                            setBinds((prev) => {
                              const next = { ...prev }
                              if (v === '') delete next[f.uid]
                              else next[f.uid] = v
                              return next
                            })
                          }}
                          style={{ minWidth: 180 }}
                        >
                          <option value="">直连（不经过代理）</option>
                          {slots.map((s) => (
                            <option key={s.id} value={s.id}>
                              {s.name}（{s.id}）{s.enabled ? '' : ' · 已停用'}
                            </option>
                          ))}
                          {orphan && <option value={bound}>⚠️ 已失效：{bound}</option>}
                        </select>
                        {orphan && (
                          <div className="dim" style={{ fontSize: 11, color: 'var(--warn, #e65100)', marginTop: 3 }}>
                            绑定的槽位已不存在，当前按直连处理 —— 请重新选择或删除该绑定。
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

      <div className="dim" style={{ fontSize: 11.5, lineHeight: 1.7 }}>
        支持 <span className="mono">http</span> / <span className="mono">https</span> /
        <span className="mono">socks5</span> / <span className="mono">socks5h</span> 代理，
        带认证的写法如 <span className="mono">http://user:pass@host:port</span>。
        「测试」只做连通性探测（连代理 + 经代理访问一个 204 端点），<b>不打上游、不消耗额度</b>。
        配置保存在 <span className="mono">config.json</span> 的
        <span className="mono">proxy_slots</span> 与 <span className="mono">account_proxies</span>。
      </div>
    </>
  )
}
