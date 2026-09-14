import { useCallback, useEffect, useState } from 'react'
import type { APIKeyEntry } from '../api/types'
import { ApiError, generateKey, getAPIKeys, saveAPIKeys } from '../api/client'

interface Props {
  /** 顶栏当前在用的密钥，用于提示「你正在删除自己正在用的密钥」 */
  activeKey: string
  onChanged: () => void
}

/** 密钥在界面上以掩码展示；这是本机管理面板，用户需要明文才能配到客户端里。 */
function maskKey(k: string): string {
  if (k.length <= 12) return k
  return `${k.slice(0, 6)}…${k.slice(-4)}`
}

export function APIKeys({ activeKey, onChanged }: Props) {
  const [legacyKey, setLegacyKey] = useState('')
  const [keys, setKeys] = useState<APIKeyEntry[]>([])
  const [configPath, setConfigPath] = useState('')
  const [revealed, setRevealed] = useState<Set<number>>(new Set())
  const [dirty, setDirty] = useState(false)

  const [err, setErr] = useState('')
  const [note, setNote] = useState('')
  const [saving, setSaving] = useState(false)

  const load = useCallback(async () => {
    try {
      const r = await getAPIKeys()
      setLegacyKey(r.legacyKey)
      setKeys(r.keys)
      setConfigPath(r.configPath)
      setDirty(false)
      setErr('')
    } catch (e) {
      setErr(
        e instanceof ApiError
          ? `密钥接口不可用：[HTTP ${e.status}] ${e.message}`
          : `密钥接口不可用：${String(e)}。该功能由 vite dev/preview server 提供。`,
      )
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  const mutate = (next: APIKeyEntry[]) => {
    setKeys(next)
    setDirty(true)
    setNote('')
    // 行号会因增删错位，直接清空展开状态，避免明文停留在错误的行上
    setRevealed(new Set())
  }

  const addKey = () => {
    mutate([...keys, { key: generateKey(), region: '', name: '' }])
  }

  const patch = (i: number, p: Partial<APIKeyEntry>) => {
    mutate(keys.map((k, idx) => (idx === i ? { ...k, ...p } : k)))
  }

  const remove = (i: number) => {
    const k = keys[i]
    // 删掉正在用的那把密钥，保存后本页自己的请求就会 401 —— 值得单独提醒。
    if (k.key && k.key === activeKey) {
      if (!confirm('这把密钥就是本页顶栏当前在用的密钥。删除并保存后，本页将无法再访问网关。\n\n仍要删除吗？')) {
        return
      }
    }
    mutate(keys.filter((_, idx) => idx !== i))
  }

  const toggleReveal = (i: number) => {
    setRevealed((prev) => {
      const next = new Set(prev)
      if (next.has(i)) next.delete(i)
      else next.add(i)
      return next
    })
  }

  const total = keys.length + (legacyKey.trim() ? 1 : 0)

  const save = async (allowUnauthenticated = false) => {
    setErr('')
    setNote('')
    setSaving(true)
    try {
      const r = await saveAPIKeys(legacyKey, keys, allowUnauthenticated)
      setDirty(false)
      setNote(`已保存 ${r.keyCount} 把密钥，已立即生效。原文件备份在 ${r.backup}。`)
      onChanged()
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) {
        // 清空密钥 = 关闭鉴权，必须二次确认
        if (confirm(`${e.message}`)) {
          setSaving(false)
          await save(true)
          return
        }
        setNote('已取消保存。')
      } else {
        setErr(e instanceof Error ? e.message : String(e))
      }
    } finally {
      setSaving(false)
    }
  }

  return (
    <>
      {total === 0 && (
        <div className="note err">
          <span>●</span>
          <div>
            <b>当前没有任何密钥，网关不校验鉴权 —— 任何能访问 7863 的人都可以调用。</b>
            如需恢复鉴权，请在下方添加至少一把密钥。
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

      <div className="panel">
        <header>
          <h2>API 密钥</h2>
          <span className="sub">{total} 把</span>
          <div className="spacer" />
          <button className="btn" onClick={addKey}>
            新增密钥
          </button>
          <button className="btn primary" onClick={() => void save()} disabled={!dirty || saving}>
            {saving ? <span className="spin" /> : null}
            {saving ? ' 保存中' : '保存'}
          </button>
        </header>

        <div className="body" style={{ paddingTop: 0 }}>
          <div className="dim" style={{ fontSize: 11.5, marginBottom: 12 }}>
            密钥绑定区域后，该密钥的请求只走对应区域的账号，其 <span className="mono">/v1/models</span> 与{' '}
            <span className="mono">/status</span> 也按该区域过滤；选「不限」则两区通吃。
            改动写入 <span className="mono">config.json</span>，保存后立即生效。
          </div>

          <div className="field">
            <label>顶层 api_key（旧版字段，不限区域）</label>
            <div className="row">
              <input
                value={legacyKey}
                onChange={(e) => {
                  setLegacyKey(e.target.value)
                  setDirty(true)
                  setNote('')
                }}
                placeholder="留空表示不使用该字段"
                style={{ flex: 1, minWidth: 260, fontFamily: 'var(--mono)', fontSize: 12 }}
                spellCheck={false}
              />
              {legacyKey ? (
                <button className="btn" onClick={() => void navigator.clipboard.writeText(legacyKey)}>
                  复制
                </button>
              ) : null}
            </div>
            <div className="dim" style={{ fontSize: 11, marginTop: 4 }}>
              与下方列表合并生效；同一密钥不能同时出现在两处。
            </div>
          </div>
        </div>

        <div className="scroll-x">
          <table>
            <thead>
              <tr>
                <th style={{ width: 150 }}>名称</th>
                <th style={{ width: 110 }}>区域</th>
                <th>密钥</th>
                <th style={{ width: 210 }} />
              </tr>
            </thead>
            <tbody>
              {keys.map((k, i) => {
                const show = revealed.has(i)
                const isActive = k.key !== '' && k.key === activeKey
                return (
                  <tr key={i}>
                    <td>
                      <input
                        value={k.name}
                        onChange={(e) => patch(i, { name: e.target.value })}
                        placeholder={`密钥 ${i + 1}`}
                        style={{ width: '100%' }}
                      />
                    </td>
                    <td>
                      <select
                        value={k.region}
                        onChange={(e) => patch(i, { region: e.target.value })}
                        style={{ width: '100%' }}
                      >
                        <option value="">不限</option>
                        <option value="cn">cn</option>
                        <option value="global">global</option>
                      </select>
                    </td>
                    <td>
                      <input
                        value={show ? k.key : maskKey(k.key)}
                        onChange={(e) => patch(i, { key: e.target.value })}
                        style={{
                          width: '100%',
                          fontFamily: 'var(--mono)',
                          fontSize: 12,
                        }}
                        spellCheck={false}
                      />
                      {isActive && (
                        <div style={{ marginTop: 4 }}>
                          <span className="badge ok">本页正在使用</span>
                        </div>
                      )}
                    </td>
                    <td className="nowrap">
                      <button className="btn" onClick={() => toggleReveal(i)}>
                        {show ? '隐藏' : '显示'}
                      </button>{' '}
                      <button
                        className="btn"
                        onClick={() => void navigator.clipboard.writeText(k.key)}
                        disabled={!k.key}
                      >
                        复制
                      </button>{' '}
                      <button className="btn" onClick={() => patch(i, { key: generateKey() })}>
                        重新生成
                      </button>{' '}
                      <button className="btn danger" onClick={() => remove(i)}>
                        删除
                      </button>
                    </td>
                  </tr>
                )
              })}
              {keys.length === 0 && (
                <tr>
                  <td colSpan={4} className="empty">
                    还没有密钥。点「新增密钥」会自动生成一把 48 位随机密钥。
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>

        {configPath && (
          <div className="dim mono" style={{ fontSize: 11, padding: '10px 14px' }}>
            {configPath}
          </div>
        )}
      </div>
    </>
  )
}
