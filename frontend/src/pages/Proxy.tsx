import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { AuthFileView, ProxySlot, SidecarStatus } from '../api/types'
import {
  ApiError,
  assignSidecar,
  getProxySlots,
  getSidecar,
  listAuthFiles,
  saveProxySlots,
  syncSidecar,
  testProxy,
} from '../api/client'

interface Props {
  onChanged: () => void
}

const URL_PLACEHOLDER = 'http://user:pass@127.0.0.1:7890 或 socks5://127.0.0.1:1080'

/** 探测结果（每个槽位一条） */
type TestResult = { ok: boolean; text: string }

/** 未绑定账号用的哨兵值（与「直连」选项的 value="" 区分开，便于批量操作表达） */
const DIRECT = ''

/** 未保存草稿的 sessionStorage 键（切 tab 不丢改动） */
const DRAFT_KEY = 'wb2api.proxy.draft'

/**
 * 槽位 id 输入框：本地编辑态 + 失焦提交。
 *
 * 用受控组件而非 defaultValue：id 是表格 key，改名会重建行；
 * 若改名失败（重名/空值），输入框必须回到旧值而不是停在用户输入上
 * ——否则界面显示的 id 与真实 id 不一致，后续操作全错。
 */
function SlotIdInput({
  id,
  onCommit,
  title,
}: {
  id: string
  onCommit: (v: string) => void
  title?: string
}) {
  const [draft, setDraft] = useState(id)
  useEffect(() => setDraft(id), [id]) // 外部 id 变了（改名成功/回滚）就同步
  return (
    <input
      className="mono"
      value={draft}
      onChange={(e) => setDraft(e.target.value)}
      onBlur={() => {
        if (draft.trim() === id) {
          setDraft(id) // 无变化或只有空白差异：归位
          return
        }
        onCommit(draft)
      }}
      onKeyDown={(e) => {
        if (e.key === 'Enter') (e.target as HTMLInputElement).blur()
        if (e.key === 'Escape') setDraft(id)
      }}
      style={{ width: 108, fontSize: 11.5 }}
      title={title}
    />
  )
}

export function Proxy({ onChanged }: Props) {
  const [files, setFiles] = useState<AuthFileView[]>([])
  const [slots, setSlots] = useState<ProxySlot[]>([])
  const [binds, setBinds] = useState<Record<string, string>>({})
  /** 服务端快照，用于「未保存改动」判定与「放弃改动」回滚 */
  const [saved, setSaved] = useState<{ slots: ProxySlot[]; binds: Record<string, string> }>({
    slots: [],
    binds: {},
  })
  const [err, setErr] = useState('')
  const [msg, setMsg] = useState('')
  const [busy, setBusy] = useState(false)
  const [tests, setTests] = useState<Record<string, TestResult>>({})
  const [testingAll, setTestingAll] = useState(false)

  // 账号列表的筛选与多选
  const [q, setQ] = useState('')
  const [regionFilter, setRegionFilter] = useState<'' | 'cn' | 'global'>('')
  const [boundFilter, setBoundFilter] = useState<'' | 'bound' | 'unbound'>('')
  const [selected, setSelected] = useState<Set<string>>(new Set())
  /** 批量绑定的目标槽位（'' = 解绑回直连） */
  const [bulkTarget, setBulkTarget] = useState<string>('')

  /**
   * 拉取服务端配置。
   *
   * keepEdits=true 时**只更新基准（saved），不覆盖用户正在编辑的 slots/binds**
   * ——用于「带草稿恢复后的首次 load」：服务端数据要作为「放弃改动」的回归点，
   * 但不能把刚恢复的草稿冲掉。
   */
  const load = useCallback(async (keepEdits = false) => {
    setBusy(true)
    try {
      const s = await getProxySlots()
      if (!keepEdits) {
        setSlots(s.slots ?? [])
        setBinds(s.binds ?? {})
      }
      setSaved({ slots: s.slots ?? [], binds: s.binds ?? {} })
      setErr('')
    } catch (e) {
      setErr(e instanceof ApiError ? `代理接口不可用：[HTTP ${e.status}] ${e.message}` : String(e))
    } finally {
      setBusy(false)
    }
    try {
      const res = await listAuthFiles()
      setFiles(res.accounts)
    } catch {
      // 忽略：没账号列表时仍可管理槽位
    }
  }, [])

  /** 草稿是否已尝试恢复过（避免写入 effect 在 load 返回前误判 dirty=false 而删草稿） */
  const restoredRef = useRef(false)

  // 挂载时先恢复草稿，**再** load()。顺序很关键：
  // 写入 effect 在挂载时会跑一次，此时 slots/binds/saved 都还是空值 → dirty=false
  // → 会把草稿删掉。所以必须先把草稿灌进状态，让首轮写入 effect 看到 dirty=true。
  useEffect(() => {
    let hadDraft = false
    try {
      const raw = sessionStorage.getItem(DRAFT_KEY)
      if (raw) {
        const d = JSON.parse(raw) as { slots: ProxySlot[]; binds: Record<string, string> }
        if (Array.isArray(d.slots)) {
          setSlots(d.slots)
          setBinds(d.binds ?? {})
          setMsg('已恢复上次未保存的改动（点「保存」生效，或「放弃改动」丢弃）')
          hadDraft = true
        }
      }
    } catch {
      sessionStorage.removeItem(DRAFT_KEY)
    }
    restoredRef.current = true
    // 有草稿时 keepEdits=true：只刷新服务端基准（供「放弃改动」回滚），
    // 不覆盖刚恢复的编辑；无草稿时正常加载。
    void load(hadDraft)
  }, [load])

  /** 有未保存改动？（切页/刷新会丢，故显式提示） */
  const dirty = useMemo(
    () => JSON.stringify({ slots, binds }) !== JSON.stringify(saved),
    [slots, binds, saved],
  )

  // 未保存的草稿存 sessionStorage：SPA 内切 tab 会卸载本组件，
  // 而 beforeunload 只在关页/刷新时触发，管不到切 tab —— 没有这层的话
  // 用户改了 22 个绑定、切去别页看一眼再回来，改动就静默没了。
  // 用 sessionStorage（非 localStorage）：关掉标签页即清，不留脏草稿。
  useEffect(() => {
    if (!restoredRef.current) return // 恢复完成前不写也不删（见上面的顺序说明）
    if (!dirty) {
      sessionStorage.removeItem(DRAFT_KEY)
      return
    }
    try {
      sessionStorage.setItem(DRAFT_KEY, JSON.stringify({ slots, binds }))
    } catch {
      // 存储满/隐私模式：降级为「切页会丢」，不阻断操作
    }
  }, [dirty, slots, binds])

  // 关页/刷新时提醒（sessionStorage 能救切 tab，但救不了关页）
  useEffect(() => {
    if (!dirty) return
    const h = (e: BeforeUnloadEvent) => {
      e.preventDefault()
      e.returnValue = ''
    }
    window.addEventListener('beforeunload', h)
    return () => window.removeEventListener('beforeunload', h)
  }, [dirty])

  // ── 槽位操作 ────────────────────────────────────────────────────────
  const addSlot = () => {
    const used = new Set(slots.map((s) => s.id))
    let n = slots.length + 1
    while (used.has(`slot-${n}`)) n++
    setSlots([...slots, { id: `slot-${n}`, name: `代理 ${n}`, url: '', enabled: true }])
  }

  const dupSlot = (s: ProxySlot) => {
    const used = new Set(slots.map((x) => x.id))
    let n = slots.length + 1
    while (used.has(`slot-${n}`)) n++
    setSlots([...slots, { ...s, id: `slot-${n}`, name: `${s.name} 副本` }])
  }

  const updSlot = (id: string, patch: Partial<ProxySlot>) =>
    setSlots(slots.map((s) => (s.id === id ? { ...s, ...patch } : s)))

  /** 改槽位 id：同步迁移所有绑定（否则绑定会瞬间失效） */
  const reIdSlot = (oldId: string, newId: string) => {
    const nid = newId.trim()
    if (!nid || nid === oldId) return
    if (slots.some((s) => s.id === nid)) {
      setErr(`槽位 id ${nid} 已被占用`)
      return
    }
    setErr('')
    setSlots(slots.map((s) => (s.id === oldId ? { ...s, id: nid } : s)))
    setBinds((prev) => {
      const next: Record<string, string> = {}
      for (const [uid, sid] of Object.entries(prev)) next[uid] = sid === oldId ? nid : sid
      return next
    })
  }

  const delSlot = (id: string) => {
    const used = Object.values(binds).filter((v) => v === id).length
    if (
      used > 0 &&
      !confirm(`槽位 ${id} 被 ${used} 个账号绑定。删除后这些账号会回落直连（绑定保留为「已失效」）。确认删除？`)
    ) {
      return
    }
    setSlots(slots.filter((s) => s.id !== id))
  }

  // ── 绑定操作 ────────────────────────────────────────────────────────
  const bindOne = (uid: string, sid: string) => {
    setBinds((prev) => {
      const next = { ...prev }
      if (sid === DIRECT) delete next[uid]
      else next[uid] = sid
      return next
    })
  }

  /** 批量绑定选中的账号到某槽位（'' = 解绑回直连）。 */
  const bindSelected = (sid: string) => {
    if (selected.size === 0) return
    setBinds((prev) => {
      const next = { ...prev }
      for (const uid of selected) {
        if (sid === DIRECT) delete next[uid]
        else next[uid] = sid
      }
      return next
    })
    const label = sid === DIRECT ? '直连' : sid
    setMsg(`已把 ${selected.size} 个账号改为「${label}」（记得点保存）`)
    setSelected(new Set())
  }

  // ── 探测 ────────────────────────────────────────────────────────────
  const runTest = async (s: ProxySlot): Promise<TestResult> => {
    if (!s.url.trim()) return { ok: false, text: '请先填代理地址' }
    try {
      const r = await testProxy(s.url)
      if (r.ok) {
        return {
          ok: true,
          text: `✓ 可用（连接 ${r.dialMs ?? 0}ms + 请求 ${r.elapsedMs ?? 0}ms，HTTP ${r.status}）`,
        }
      }
      // 区分三档，避免把「出口故障」错读成「目标站问题」：
      //   dial    → 连不到代理端口（代理没起来 / 端口写错）
      //   request 且带 status → 出口能连但返回 4xx/5xx（节点失效，mihomo 本地回 502/503）
      //   request 无 status   → 传输层失败（超时、TLS、被重置）
      const label =
        r.stage === 'dial'
          ? '连不上代理主机'
          : r.status
            ? `出口不可用（HTTP ${r.status}）`
            : '代理可达但转发失败'
      return { ok: false, text: `✗ ${label}：${r.error ?? '未知错误'}` }
    } catch (e) {
      return { ok: false, text: String(e) }
    }
  }

  const doTest = async (s: ProxySlot) => {
    setTests((t) => ({ ...t, [s.id]: { ok: false, text: '测试中…' } }))
    const r = await runTest(s)
    setTests((t) => ({ ...t, [s.id]: r }))
  }

  /** 一键测全部槽位（串行，避免同时打一堆连接）。 */
  const testAll = async () => {
    setTestingAll(true)
    for (const s of slots) {
      if (!s.enabled) continue
      setTests((t) => ({ ...t, [s.id]: { ok: false, text: '测试中…' } }))
      const r = await runTest(s)
      setTests((t) => ({ ...t, [s.id]: r }))
    }
    setTestingAll(false)
  }

  // ── 保存 ────────────────────────────────────────────────────────────
  const doSave = async () => {
    setErr('')
    setMsg('')
    for (const s of slots) {
      if (!s.id.trim()) return setErr('有槽位缺少 id')
      if (!s.url.trim()) return setErr(`槽位 ${s.id} 缺少代理地址`)
    }
    try {
      await saveProxySlots(slots, binds)
      setSaved({ slots, binds })
      sessionStorage.removeItem(DRAFT_KEY)
      setSelected(new Set()) // 绑定已落地，清掉多选避免误操作
      setMsg('已保存并热重载（不重启进程）')
      await load()
      onChanged()
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    }
  }

  const discard = () => {
    if (!confirm('放弃未保存的改动？')) return
    setSlots(saved.slots)
    setBinds(saved.binds)
    sessionStorage.removeItem(DRAFT_KEY)
    setMsg('已放弃改动')
    setErr('')
  }

  // ── 派生数据 ────────────────────────────────────────────────────────
  const usageBySlot = useMemo(() => {
    const m: Record<string, string[]> = {}
    for (const [uid, sid] of Object.entries(binds)) {
      if (!sid) continue
      ;(m[sid] ??= []).push(uid)
    }
    return m
  }, [binds])

  const nicknameOf = useCallback(
    (uid: string) => files.find((f) => f.uid === uid)?.nickname || uid.slice(0, 8),
    [files],
  )

  const visibleFiles = useMemo(() => {
    const needle = q.trim().toLowerCase()
    return files.filter((f) => {
      if (regionFilter && f.region !== regionFilter) return false
      const bound = (binds[f.uid] ?? '') !== ''
      if (boundFilter === 'bound' && !bound) return false
      if (boundFilter === 'unbound' && bound) return false
      if (needle) {
        const hay = `${f.nickname} ${f.uid}`.toLowerCase()
        if (!hay.includes(needle)) return false
      }
      return true
    })
  }, [files, q, regionFilter, boundFilter, binds])

  const allVisibleSelected = visibleFiles.length > 0 && visibleFiles.every((f) => selected.has(f.uid))
  const toggleAllVisible = () => {
    setSelected((prev) => {
      const next = new Set(prev)
      if (allVisibleSelected) for (const f of visibleFiles) next.delete(f.uid)
      else for (const f of visibleFiles) next.add(f.uid)
      return next
    })
  }

  const boundCount = Object.values(binds).filter(Boolean).length

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

      {/* ── 订阅 sidecar（mihomo）────────────────────────────────────── */}
      <SidecarPanel onDone={() => void load()} />

      {/* ── 槽位 ───────────────────────────────────────────────────── */}
      <div className="panel">
        <header>
          <h2>代理槽位</h2>
          <span className="sub">{slots.length} 个</span>
          <div className="spacer" />
          <button className="btn" onClick={addSlot}>
            新建槽位
          </button>
          <button className="btn" onClick={() => void testAll()} disabled={testingAll || slots.length === 0}>
            {testingAll ? '测试中…' : '测试全部'}
          </button>
          <button className="btn" onClick={() => void load()} disabled={busy}>
            重新读取
          </button>
        </header>

        {dirty && (
          <div className="dirty-bar">
            <span>●</span>
            <span>有未保存的改动 —— 离开此页会丢失。</span>
            <div className="spacer" />
            <button className="btn" onClick={discard}>
              放弃改动
            </button>
            <button className="btn primary" onClick={() => void doSave()}>
              保存
            </button>
          </div>
        )}

        {slots.length === 0 ? (
          <div className="empty">
            还没有代理槽位。点「新建槽位」添加 —— 未配置时所有账号都直连（与改造前一致）。
          </div>
        ) : (
          <div className="scroll-x">
            <table>
              <thead>
                <tr>
                  <th style={{ minWidth: 120 }}>槽位 ID</th>
                  <th style={{ minWidth: 110 }}>名称</th>
                  <th style={{ minWidth: 320 }}>代理地址</th>
                  <th>启用</th>
                  <th title="出口健康：连续失败达阈值后主动探测确认；不健康时选号会排除绑定本槽位的账号（轮换改用别的出口）">
                    出口
                  </th>
                  <th title="点击查看使用该槽位的账号">在用</th>
                  <th>探测</th>
                  <th></th>
                </tr>
              </thead>
              <tbody>
                {slots.map((s) => {
                  const t = tests[s.id]
                  const users = usageBySlot[s.id] ?? []
                  return (
                    <tr key={s.id} className={s.enabled ? '' : 'slot-off'}>
                      <td>
                        <SlotIdInput
                          id={s.id}
                          onCommit={(v) => reIdSlot(s.id, v)}
                          title="改 id 会同步迁移已绑定该槽位的账号"
                        />
                      </td>
                      <td>
                        <input
                          value={s.name}
                          onChange={(e) => updSlot(s.id, { name: e.target.value })}
                          style={{ width: 100 }}
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
                            style={{
                              fontSize: 11,
                              marginTop: 3,
                              color: t.ok ? 'var(--ok)' : 'var(--err)',
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
                          title="关闭后绑定它的账号回落直连（槽位保留，便于临时切换）"
                        />
                      </td>
                      <td>
                        {s.healthy === false ? (
                          <span
                            className="badge err"
                            title={
                              `出口不可用：连续失败 ${s.fail_count ?? 0} 次` +
                              (s.last_error ? `\n最近错误：${s.last_error}` : '') +
                              (s.cooldown_ms ? `\n${Math.ceil(s.cooldown_ms / 1000)}s 后自动重探` : '')
                            }
                          >
                            {s.cooldown_ms ? `${Math.ceil(s.cooldown_ms / 1000)}s` : '异常'}
                          </span>
                        ) : (
                          <span className="badge ok" title="出口正常（或尚未出站）">
                            正常
                          </span>
                        )}
                      </td>
                      <td>
                        <span
                          className="usage-link"
                          onClick={() =>
                            users.length &&
                            alert(
                              `${users.length} 个账号使用「${s.name}」：\n\n` +
                                users.map((u) => '· ' + nicknameOf(u)).join('\n'),
                            )
                          }
                          title={users.length ? '点击查看账号列表' : '暂无账号使用'}
                        >
                          {users.length}
                        </span>
                      </td>
                      <td>
                        <button className="btn" onClick={() => void doTest(s)} style={{ fontSize: 11.5 }}>
                          测试
                        </button>
                      </td>
                      <td>
                        <div className="row" style={{ flexWrap: 'nowrap', gap: 4 }}>
                          <button
                            className="btn"
                            onClick={() => dupSlot(s)}
                            style={{ fontSize: 11.5 }}
                            title="复制一个相同的槽位"
                          >
                            复制
                          </button>
                          <button className="btn" onClick={() => delSlot(s.id)} style={{ fontSize: 11.5 }}>
                            删除
                          </button>
                        </div>
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}
      </div>

      {/* ── 账号绑定 ───────────────────────────────────────────────── */}
      <div className="panel">
        <header>
          <h2>账号绑定</h2>
          <span className="sub">
            {boundCount} / {files.length} 已绑定
          </span>
          <div className="spacer" />
          <button className="btn primary" onClick={() => void doSave()} disabled={!dirty}>
            保存
          </button>
        </header>

        <div className="bulk-bar">
          <button
            className="btn"
            onClick={toggleAllVisible}
            disabled={visibleFiles.length === 0}
            title={visibleFiles.length === 0 ? '当前筛选下没有账号可选' : `选中当前可见的 ${visibleFiles.length} 个`}
          >
            {allVisibleSelected ? '取消全选' : `全选当前（${visibleFiles.length}）`}
          </button>
          <span className="count">已选 {selected.size} 个</span>
          <div className="spacer" />
          <label className="dim" style={{ fontSize: 12, display: 'flex', alignItems: 'center', gap: 6 }}>
            批量设为
            <select
              value={bulkTarget}
              onChange={(e) => setBulkTarget(e.target.value)}
              style={{ minWidth: 150 }}
            >
              <option value={DIRECT}>直连（不经过代理）</option>
              {slots.map((s) => (
                <option key={s.id} value={s.id}>
                  {s.name}（{s.id}）{s.enabled ? '' : ' · 已停用'}
                </option>
              ))}
            </select>
            <button className="btn" disabled={selected.size === 0} onClick={() => bindSelected(bulkTarget)}>
              应用到已选
            </button>
          </label>
          {selected.size > 0 && (
            <button className="btn" onClick={() => setSelected(new Set())}>
              清空选择
            </button>
          )}
        </div>

        <div className="toolbar">
          <input
            placeholder="搜索昵称或 uid…"
            value={q}
            onChange={(e) => setQ(e.target.value)}
            style={{ width: 200 }}
          />
          <label>
            区域
            <select value={regionFilter} onChange={(e) => setRegionFilter(e.target.value as '' | 'cn' | 'global')}>
              <option value="">全部</option>
              <option value="cn">cn</option>
              <option value="global">global</option>
            </select>
          </label>
          <label>
            绑定状态
            <select value={boundFilter} onChange={(e) => setBoundFilter(e.target.value as '' | 'bound' | 'unbound')}>
              <option value="">全部</option>
              <option value="bound">已绑定</option>
              <option value="unbound">未绑定</option>
            </select>
          </label>
          <div className="spacer" />
          <span className="dim" style={{ fontSize: 12 }}>
            显示 {visibleFiles.length} / {files.length}
          </span>
        </div>

        {files.length === 0 ? (
          <div className="empty">还没有账号。</div>
        ) : visibleFiles.length === 0 ? (
          <div className="empty">没有匹配的账号。</div>
        ) : (
          <div className="scroll-x">
            <table>
              <thead>
                <tr>
                  <th style={{ width: 36 }}>
                    <input type="checkbox" checked={allVisibleSelected} onChange={toggleAllVisible} />
                  </th>
                  <th>账号</th>
                  <th>区域</th>
                  <th style={{ minWidth: 220 }}>代理</th>
                </tr>
              </thead>
              <tbody>
                {visibleFiles.map((f) => {
                  const bound = binds[f.uid] ?? ''
                  const orphan = bound !== '' && !slots.some((s) => s.id === bound)
                  return (
                    <tr key={f.uid} className={bound ? 'bound' : ''}>
                      <td>
                        <input
                          type="checkbox"
                          checked={selected.has(f.uid)}
                          onChange={(e) => {
                            setSelected((prev) => {
                              const next = new Set(prev)
                              if (e.target.checked) next.add(f.uid)
                              else next.delete(f.uid)
                              return next
                            })
                          }}
                        />
                      </td>
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
                          onChange={(e) => bindOne(f.uid, e.target.value)}
                          style={{ minWidth: 180 }}
                        >
                          <option value={DIRECT}>直连（不经过代理）</option>
                          {slots.map((s) => (
                            <option key={s.id} value={s.id}>
                              {s.name}（{s.id}）{s.enabled ? '' : ' · 已停用'}
                            </option>
                          ))}
                          {orphan && <option value={bound}>⚠️ 已失效：{bound}</option>}
                        </select>
                        {orphan && (
                          <div style={{ fontSize: 11, color: 'var(--warn)', marginTop: 3 }}>
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

/**
 * SidecarPanel 订阅 → 出口端口 → 槽位 的一键面板。
 *
 * 设计取舍：
 *   - 订阅 URL 存在 config.json（proxy_sidecar.subscription_url），不在前端保存；
 *     面板只负责「触发同步」「粘贴订阅」「分配账号」三件事。
 *   - 「同步订阅」与「自动分配」是**两步**而非一步：同步只写槽位，分配只改绑定。
 *     分开的理由是绑定是**粘性**的（已绑有效槽位的账号不会被改），分步才能让用户
 *     先看清展开了哪些出口、再决定要不要摊账号。
 *   - 未启用时（后端返回 enabled=false）整块显示为提示，不渲染按钮 ——
 *     避免用户在没配订阅的部署上点了按钮却只拿到报错。
 */
function SidecarPanel({ onDone }: { onDone: () => void }) {
  const [st, setSt] = useState<SidecarStatus | null>(null)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const [msg, setMsg] = useState('')
  const [pasted, setPasted] = useState('')
  const [showPaste, setShowPaste] = useState(false)

  const load = useCallback(async () => {
    try {
      setSt(await getSidecar())
      setErr('')
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : String(e))
    }
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
        onDone()
      } catch (e) {
        setErr(e instanceof ApiError ? e.message : String(e))
      } finally {
        setBusy(false)
      }
    },
    [load, onDone],
  )

  const doSync = () =>
    run(async () => {
      const r = await syncSidecar(pasted.trim() || undefined)
      setPasted('')
      setShowPaste(false)
      const warn = r.warnings?.length ? '；' + r.warnings.join('；') : ''
      return `已展开 ${r.slots.length} 个出口（订阅共 ${r.node_count} 个节点）${warn}`
    })

  const doAssign = () =>
    run(async () => {
      const r = await assignSidecar()
      return r.message ?? `已为 ${r.assigned} 个账号分配出口（已绑定的保持不变）`
    })

  if (!st) {
    return (
      <div className="panel">
        <header>
          <h2>订阅出口（mihomo）</h2>
        </header>
        <div className="empty">{err || '读取中…'}</div>
      </div>
    )
  }

  if (!st.enabled) {
    return (
      <div className="panel">
        <header>
          <h2>订阅出口（mihomo）</h2>
          <span className="sub">未启用</span>
        </header>
        <div className="empty">
          未启用订阅导出。在 <span className="mono">config.json</span> 里配置
          <span className="mono"> proxy_sidecar</span>：
          <span className="mono">{'{ "enabled": true, "subscription_url": "…" }'}</span>
          ，并把 <span className="mono">mihomo.exe</span> 放到网关同目录。
          <div className="dim" style={{ marginTop: 6 }}>
            启用后可把一份 Clash/分享链接订阅展开成 N 个本地出口端口，让不同账号走不同出口 IP。
          </div>
        </div>
      </div>
    )
  }

  const engineOK = !!st.engines?.some((e) => e.running)
  return (
    <div className="panel">
      <header>
        <h2>订阅出口（mihomo）</h2>
        <span className="sub">
          {st.node_count} 个节点 → {st.slot_count} 个出口
        </span>
        <div className="spacer" />
        <button className="btn" onClick={() => setShowPaste((v) => !v)} disabled={busy}>
          {showPaste ? '取消粘贴' : '粘贴订阅'}
        </button>
        <button className="btn primary" onClick={() => void doSync()} disabled={busy}>
          {busy ? '处理中…' : '同步订阅'}
        </button>
        <button className="btn" onClick={() => void doAssign()} disabled={busy || st.slot_count === 0}>
          自动分配账号
        </button>
        <button className="btn" onClick={() => void load()} disabled={busy}>
          刷新
        </button>
      </header>

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
      {st.last_error && (
        <div className="note err">
          <span>●</span>
          <div style={{ wordBreak: 'break-word' }}>上次同步失败：{st.last_error}</div>
        </div>
      )}

      {showPaste && (
        <div style={{ padding: '10px 14px' }}>
          <textarea
            className="mono"
            value={pasted}
            onChange={(e) => setPasted(e.target.value)}
            placeholder="在此粘贴订阅内容（Clash YAML / base64 / 分享链接列表）。留空则按 config 里的订阅地址拉取。"
            style={{ width: '100%', minHeight: 110, fontSize: 11.5 }}
          />
          <div className="dim" style={{ fontSize: 11.5, marginTop: 4 }}>
            订阅站无法直连（需代理）时用这条路径：在能访问的机器上打开订阅链接、全选复制后粘贴到这里。
            粘贴后点「同步订阅」。
          </div>
        </div>
      )}

      <div className="dim" style={{ padding: '4px 14px 12px', fontSize: 11.5, lineHeight: 1.9 }}>
        <div>
          mihomo 核心：{st.binary_ok ? <span className="badge ok">就绪</span> : <span className="badge err">未找到</span>}
          {st.binary_path && <span className="mono" style={{ marginLeft: 8 }}>{st.binary_path}</span>}
        </div>
        <div>
          出口进程：{engineOK ? <span className="badge ok">运行中</span> : <span className="badge warn">未运行</span>}
          <span style={{ marginLeft: 8 }}>
            端口 {st.base_port}–{st.base_port + Math.max(0, st.slot_count - 1)}（仅监听 127.0.0.1）
          </span>
        </div>
        {st.last_sync && <div>上次同步：{new Date(st.last_sync).toLocaleString()}</div>}
      </div>
    </div>
  )
}
