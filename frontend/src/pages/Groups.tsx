import { useCallback, useEffect, useMemo, useState } from 'react'
import type { AuthFileView } from '../api/types'
import {
  ApiError,
  createGroup,
  deleteGroup,
  getAPIKeys,
  getGroups,
  getLoadedAccounts,
  listAuthFiles,
  renameGroup,
  setAccountGroup,
} from '../api/client'

/**
 * Groups 分组管理页。
 *
 * 设计取舍：
 *   - 分组的主体是 uid→组名 的映射，但**允许空组**：后端另存一份
 *     account_group_names（见 config.go），面板因此可以「先建组、再往里放号」。
 *     空组只存在于这份名单里，一旦有了成员就自然由映射本身代表。
 *   - 删除分组 = 解除全部账号归属（**不删账号**），并解绑引用它的密钥。
 *     这两件事必须一起做，否则密钥会指向一个不存在的组、变成「组内无账号」而全失败。
 *   - 账号选择用多选 + 批量设置，而不是逐行编辑：分组操作天然是批量的
 *     （「把这 10 个封禁号移进受限组」）。
 */
export function Groups({ onChanged }: { onChanged: () => void }) {
  const [groups, setGroups] = useState<Record<string, string>>({})
  const [names, setNames] = useState<string[]>([])
  const [available, setAvailable] = useState(true)
  const [files, setFiles] = useState<AuthFileView[]>([])
  const [loaded, setLoaded] = useState<Set<string> | null>(null)
  const [err, setErr] = useState('')
  const [msg, setMsg] = useState('')
  const [busy, setBusy] = useState(false)
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [target, setTarget] = useState('')
  const [renaming, setRenaming] = useState<string | null>(null)
  const [renameDraft, setRenameDraft] = useState('')
  const [filter, setFilter] = useState('')
  /** 新建分组的输入草稿 */
  const [newName, setNewName] = useState('')
  /** 显式创建、当前没有账号的组名（后端 account_group_names） */
  const [emptyNames, setEmptyNames] = useState<string[]>([])
  /** 分组名 → 绑定了它的密钥名（展示用）。取不到密钥表时为空。 */
  const [keyNames, setKeyNames] = useState<Record<string, string[]>>({})

  const load = useCallback(async () => {
    setBusy(true)
    try {
      const g = await getGroups()
      setGroups(g.groups)
      setNames(g.names)
      setEmptyNames(g.emptyNames ?? [])
      setAvailable(g.available)
      const res = await listAuthFiles()
      setFiles(res.accounts)
      try {
        const l = await getLoadedAccounts()
        setLoaded(l.reachable ? new Set(l.uuids) : null)
      } catch {
        setLoaded(null)
      }
      // 哪些密钥在用这个组：从密钥表算，比让 APIKeys 页往模块变量里写更可靠
      // （跨页写入依赖用户先访问密钥页，且页面重挂载就丢）。
      try {
        const k = await getAPIKeys()
        const m: Record<string, string[]> = {}
        k.keys.forEach((e, i) => {
          const g = (e.group ?? '').trim()
          if (!g) return
          ;(m[g] ??= []).push(e.name.trim() || `密钥 ${i + 1}`)
        })
        setKeyNames(m)
      } catch {
        setKeyNames({})
      }
      setErr('')
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : String(e))
    }
    setBusy(false)
  }, [])
  useEffect(() => {
    void load()
  }, [load])

  const run = useCallback(
    async (fn: () => Promise<string>) => {
      setBusy(true)
      setErr('')
      setMsg('')
      try {
        setMsg(await fn())
        await load()
        onChanged()
      } catch (e) {
        setErr(e instanceof ApiError ? e.message : String(e))
      }
      setBusy(false)
    },
    [load, onChanged],
  )

  // 每个分组的账号数
  const counts = useMemo(() => {
    const m: Record<string, number> = {}
    for (const g of Object.values(groups)) m[g] = (m[g] ?? 0) + 1
    return m
  }, [groups])

  const ungrouped = useMemo(() => files.filter((f) => !groups[f.uid]).length, [files, groups])

  // 账号列表：按分组筛选
  const visible = useMemo(() => {
    if (filter === '') return files
    if (filter === '__none__') return files.filter((f) => !groups[f.uid])
    return files.filter((f) => groups[f.uid] === filter)
  }, [files, groups, filter])

  const toggle = (uid: string) =>
    setSelected((s) => {
      const n = new Set(s)
      if (n.has(uid)) n.delete(uid)
      else n.add(uid)
      return n
    })

  const doAssign = () =>
    run(async () => {
      const uids = [...selected]
      if (uids.length === 0) return '没有选中账号'
      const r = await setAccountGroup(uids, target.trim())
      setSelected(new Set())
      return `已把 ${r.updated} 个账号设为「${target.trim() || '未分组'}」`
    })

  const doCreate = () =>
    run(async () => {
      const name = newName.trim()
      if (!name) return '请填写分组名'
      const r = await createGroup(name)
      setNewName('')
      return r.created ? `已新建分组「${name}」` : `分组「${name}」已存在`
    })

  const doRename = (from: string) =>
    run(async () => {
      const to = renameDraft.trim()
      if (!to || to === from) {
        setRenaming(null)
        return '未改动'
      }
      const r = await renameGroup(from, to)
      setRenaming(null)
      return `已重命名「${from}」→「${to}」：${r.accounts} 个账号，${r.keys} 个密钥`
    })

  const doDelete = (name: string) => {
    const n = counts[name] ?? 0
    if (
      !confirm(
        `删除分组「${name}」？\n\n` +
          `· ${n} 个账号将被移出该分组（账号本身不删除，变回未分组）\n` +
          `· 绑定了该分组的密钥会被解绑（变回未分组语义）\n\n` +
          `此操作不可撤销。`,
      )
    )
      return
    void run(async () => {
      const r = await deleteGroup(name)
      if (filter === name) setFilter('')
      return `已删除分组「${name}」：解除 ${r.accounts} 个账号，解绑 ${r.keys} 个密钥`
    })
  }

  if (!available) {
    return (
      <div className="panel">
        <header>
          <h2>账号分组</h2>
        </header>
        <div className="empty">
          分组功能未接线（网关版本过旧或未启用）。分组用于把账号按用途隔离，让不同密钥
          只能使用各自的账号集合。
        </div>
      </div>
    )
  }

  return (
    <>
      <div className="note info">
        <span>●</span>
        <div>
          分组把账号按用途隔离：<b>绑定了分组的密钥只能使用该组账号</b>；
          <b>未绑定分组的密钥只能使用未分组账号</b>。两者互不通用、不会互相回退 ——
          这正是「把被封禁的号放进受限组、普通密钥不再选中它们」的实现方式。
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

      {/* ── 分组列表 ─────────────────────────────────────────────── */}
      <div className="panel">
        <header>
          <h2>分组</h2>
          <span className="sub">
            {names.length} 个 · 未分组 {ungrouped} 个账号
          </span>
          <div className="spacer" />
          <button className="btn" onClick={() => void load()} disabled={busy}>
            刷新
          </button>
        </header>

        <div className="toolbar">
          <span className="dim" style={{ fontSize: 12 }}>
            新建分组
          </span>
          <input
            value={newName}
            onChange={(e) => setNewName(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter') void doCreate()
            }}
            placeholder="分组名（如 受限 / 高优）"
            style={{ width: 200 }}
          />
          <button className="btn primary" onClick={() => void doCreate()} disabled={busy || !newName.trim()}>
            新建
          </button>
          <span className="dim" style={{ fontSize: 11.5 }}>
            空组会保留在列表里，直到你给它分配账号或删除它。
          </span>
        </div>

        {names.length === 0 ? (
          <div className="empty">
            还没有分组。用上方「新建分组」建一个，再在下方账号列表里选中账号「设为该分组」。
          </div>
        ) : (
          <div className="scroll-x">
            <table>
              <thead>
                <tr>
                  <th style={{ minWidth: 160 }}>分组名</th>
                  <th>账号数</th>
                  <th>使用该组的密钥</th>
                  <th></th>
                </tr>
              </thead>
              <tbody>
                {names.map((n) => (
                  <tr key={n}>
                    <td>
                      {renaming === n ? (
                        <input
                          autoFocus
                          value={renameDraft}
                          onChange={(e) => setRenameDraft(e.target.value)}
                          onKeyDown={(e) => {
                            if (e.key === 'Enter') void doRename(n)
                            if (e.key === 'Escape') setRenaming(null)
                          }}
                          style={{ width: 150 }}
                        />
                      ) : (
                        <span className="mono">{n}</span>
                      )}
                    </td>
                    <td>
                      <span
                        className="usage-link"
                        onClick={() => setFilter(n)}
                        title="点击筛选下方账号列表"
                      >
                        {counts[n] ?? 0}
                      </span>
                      {emptyNames.includes(n) && (counts[n] ?? 0) === 0 && (
                        <span className="badge warn" style={{ marginLeft: 6 }} title="这个组还没有账号，绑定它的密钥会全部失败">
                          空组
                        </span>
                      )}
                    </td>
                    <td className="dim" style={{ fontSize: 11.5 }}>
                      {keyNames[n]?.length ? keyNames[n].join('、') : '—'}
                    </td>
                    <td className="nowrap">
                      {renaming === n ? (
                        <>
                          <button className="btn primary" onClick={() => void doRename(n)} disabled={busy}>
                            保存
                          </button>
                          <button className="btn" onClick={() => setRenaming(null)}>
                            取消
                          </button>
                        </>
                      ) : (
                        <>
                          <button
                            className="btn"
                            onClick={() => {
                              setRenaming(n)
                              setRenameDraft(n)
                            }}
                            disabled={busy}
                          >
                            重命名
                          </button>
                          <button className="btn" onClick={() => void doDelete(n)} disabled={busy}>
                            删除
                          </button>
                          <button className="btn" onClick={() => setFilter(n)} disabled={busy}>
                            查看账号
                          </button>
                        </>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      {/* ── 账号归属 ─────────────────────────────────────────────── */}
      <div className="panel">
        <header>
          <h2>账号归属</h2>
          <span className="sub">
            {visible.length} 个
            {selected.size > 0 ? ` · 已选 ${selected.size}` : ''}
          </span>
          <div className="spacer" />
          <select value={filter} onChange={(e) => setFilter(e.target.value)}>
            <option value="">全部分组</option>
            <option value="__none__">未分组</option>
            {names.map((n) => (
              <option key={n} value={n}>
                {n}
              </option>
            ))}
          </select>
        </header>

        <div className="toolbar">
          <span className="dim" style={{ fontSize: 12 }}>
            把选中账号设为
          </span>
          <input
            value={target}
            onChange={(e) => setTarget(e.target.value)}
            placeholder="分组名（留空 = 移出分组）"
            style={{ width: 200 }}
            list="group-names"
          />
          <datalist id="group-names">
            {names.map((n) => (
              <option key={n} value={n} />
            ))}
          </datalist>
          <button className="btn primary" onClick={() => void doAssign()} disabled={busy || selected.size === 0}>
            设为该分组（{selected.size}）
          </button>
          <button className="btn" onClick={() => setSelected(new Set())} disabled={selected.size === 0}>
            清空选择
          </button>
          <div className="spacer" />
          <button
            className="btn"
            onClick={() => setSelected(new Set(visible.map((f) => f.uid)))}
            disabled={visible.length === 0}
          >
            全选当前列表
          </button>
        </div>

        {visible.length === 0 ? (
          <div className="empty">没有符合条件的账号。</div>
        ) : (
          <div className="scroll-x">
            <table>
              <thead>
                <tr>
                  <th style={{ width: 34 }}>
                    <input
                      type="checkbox"
                      checked={visible.length > 0 && visible.every((f) => selected.has(f.uid))}
                      onChange={(e) =>
                        setSelected(
                          e.target.checked ? new Set(visible.map((f) => f.uid)) : new Set(),
                        )
                      }
                    />
                  </th>
                  <th>账号</th>
                  <th>区域</th>
                  <th>分组</th>
                  <th>状态</th>
                  <th title="最近几次调用（成功/失败原因）">最近调用</th>
                </tr>
              </thead>
              <tbody>
                {visible.map((f) => {
                  const isLoaded = typeof f.inPool === 'boolean' ? f.inPool : loaded?.has(f.uid) ?? null
                  return (
                    <tr key={f.file} className={selected.has(f.uid) ? 'slot-off' : ''}>
                      <td>
                        <input
                          type="checkbox"
                          checked={selected.has(f.uid)}
                          onChange={() => toggle(f.uid)}
                        />
                      </td>
                      <td>
                        <div style={{ color: 'var(--text-strong)' }}>{f.nickname || '（无昵称）'}</div>
                        <div className="mono dim" style={{ fontSize: 11 }}>
                          {f.uid}
                        </div>
                      </td>
                      <td>
                        <span className={'badge' + (f.region === 'global' ? ' accent' : '')}>
                          {f.region}
                        </span>
                      </td>
                      <td>
                        {groups[f.uid] ? (
                          <span className="badge accent">{groups[f.uid]}</span>
                        ) : (
                          <span className="dim">未分组</span>
                        )}
                      </td>
                      <td>
                        {isLoaded === null ? (
                          <span className="badge">未知</span>
                        ) : !isLoaded ? (
                          <span className="badge warn">未加载</span>
                        ) : f.disabled ? (
                          <span className="badge err" title={f.disabledReason}>
                            已禁用
                          </span>
                        ) : (
                          <span className="badge ok">已加载</span>
                        )}
                      </td>
                      <td>
                        <CallHistory calls={f.calls} />
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

function CallHistory({ calls }: { calls?: { t: string; ok: boolean; reason?: string }[] }) {
  if (!calls || calls.length === 0) return <span className="dim">无记录</span>
  const okN = calls.filter((c) => c.ok).length
  const failN = calls.length - okN
  return (
    <div style={{ lineHeight: 1.5 }}>
      <div className="dim">
        最近 {calls.length}：<span style={{ color: 'var(--ok)' }}>{okN}✓</span>
        {failN > 0 && (
          <>
            {' '}
            <span style={{ color: 'var(--err)' }}>{failN}✗</span>
          </>
        )}
      </div>
      {calls.slice(0, 2).map((c, i) => (
        <div
          key={i}
          className="mono dim"
          style={{ fontSize: 10.5, whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis', maxWidth: 180 }}
          title={`${c.t}${c.reason ? ' · ' + c.reason : ''}`}
        >
          {c.ok ? '✓' : '✗'} {c.t.slice(11, 19)} {c.reason ? c.reason.slice(0, 16) : ''}
        </div>
      ))}
    </div>
  )
}
