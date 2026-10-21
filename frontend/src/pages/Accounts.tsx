import { useCallback, useEffect, useRef, useState } from 'react'
import type { AuthFileView, CallRecord } from '../api/types'
import {
  ApiError,
  deleteAuth,
  exportAccounts,
  fmtTime,
  getLoadedAccounts,
  getGroups,
  saveGroups,
  importAuth,
  importAuthBatch,
  listAuthFiles,
  pollLogin,
  reloadPool,
  startLogin,
  toggleAccount,
} from '../api/client'
interface Props {
  onChanged: () => void
}

export function Accounts({ onChanged }: Props) {
  const [files, setFiles] = useState<AuthFileView[]>([])
  const [authDir, setAuthDir] = useState('')
  /** 网关已加载的 uid 集合；null = 还没问出来（此时不判断是否加载） */
  const [loadedUids, setLoadedUids] = useState<Set<string> | null>(null)
  const [err, setErr] = useState('')
  // 账号分组：草稿表（uid → 分组名）+ 是否有未保存改动。
  // 后端把分组表存在 config.json 的 account_groups，本页只做编辑与保存。
  const [groups, setGroups] = useState<Record<string, string>>({})
  const [groupsAvailable, setGroupsAvailable] = useState(true)
  const [groupsDirty, setGroupsDirty] = useState(false)
  const setGroupDraft = useCallback((uid: string, g: string) => {
    setGroups((m) => {
      const n = { ...m }
      const t = g.trim()
      if (t) n[uid] = t
      else delete n[uid]
      return n
    })
    setGroupsDirty(true)
  }, [])
  const doSaveGroups = useCallback(async () => {
    try {
      await saveGroups(groups)
      setGroupsDirty(false)
      setErr('')
      setRowMsg('分组已保存')
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : String(e))
    }
  }, [groups])
  const [busy, setBusy] = useState(false)

  const [region, setRegion] = useState<'cn' | 'global'>('cn')

  // 登录流程状态机
  const [authUrl, setAuthUrl] = useState('')
  const [phase, setPhase] = useState<'idle' | 'waiting' | 'ok'>('idle')
  const [loginMsg, setLoginMsg] = useState('')
  const [rescanning, setRescanning] = useState(false)

  const [importText, setImportText] = useState('')
  /** 导出：是否含 token 明文（默认否，安全底线） */
  const [exportWithTokens, setExportWithTokens] = useState(false)
  /** 导出：限定单个文件（空 = 全部） */
  const [exportFile, setExportFile] = useState('')
  const pollTimer = useRef(0)
  /** 轮询代数：递增即作废此前所有在途响应，避免迟到响应覆盖新状态。 */
  const pollGen = useRef(0)
  /** 本次登录会话标识，由 startLogin 返回；轮询时带回以隔离并发登录。 */
  const sessionIdRef = useRef('')
  /** 导入区的隐藏文件输入；点按钮或拖放都走它，导入后清空 value 以便重复选同一文件。 */
  const fileInputRef = useRef<HTMLInputElement>(null)
  /** 拖放悬停高亮（仅视觉反馈）。 */
  const [dragOver, setDragOver] = useState(false)

  const load = useCallback(async () => {
    setBusy(true)
    try {
      const res = await listAuthFiles()
      setFiles(res.accounts)
      setAuthDir(res.authDir)
      setErr('')
    } catch (e) {
      setErr(
        e instanceof ApiError
          ? `账号接口不可用：[HTTP ${e.status}] ${e.message}`
          : `账号接口不可用：${String(e)}。该功能由 vite dev/preview server 提供，直接用网关端口打开页面时不存在。`,
      )
    }
    // 分组表：与账号列表同批拉，失败就置为不可用（输入框禁用，不谎称未分组）。
    try {
      const g = await getGroups()
      setGroups(g.groups)
      setGroupsAvailable(g.available)
      setGroupsDirty(false)
    } catch {
      setGroupsAvailable(false)
    }
    // 单独取「网关已加载」，失败就把判断置为未知 —— 绝不因为查不到就宣称账号未加载。
    try {
      const l = await getLoadedAccounts()
      setLoadedUids(l.reachable ? new Set(l.uuids) : null)
    } catch {
      setLoadedUids(null)
    }
    setBusy(false)
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  // 组件卸载时停掉轮询，避免登录结束后定时器还在跑
  useEffect(() => () => clearInterval(pollTimer.current), [])

  const beginLogin = async () => {
    setErr('')
    setAuthUrl('')
    setLoginMsg('')
    try {
      const res = await startLogin(region)
      sessionIdRef.current = res.sessionId
      setAuthUrl(res.authUrl)
      setPhase('waiting')
      setLoginMsg('已生成授权链接。请在浏览器打开并完成登录，本页会自动检测。')
      startPolling()
    } catch (e) {
      setPhase('idle')
      setErr(e instanceof Error ? e.message : String(e))
    }
  }

  const startPolling = () => {
    clearInterval(pollTimer.current)
    // 轮询是 3s 一次的定时器，上一次请求可能还没回来就发了下一发。
    // 用代数计数作废旧响应：登录成功后到达的迟到响应不得覆盖成功提示。
    const gen = ++pollGen.current
    pollTimer.current = window.setInterval(async () => {
      try {
        const r = await pollLogin(sessionIdRef.current)
        if (gen !== pollGen.current) return // 本轮已被更新的一轮取代
        if (r.status === 'ok') {
          pollGen.current++ // 作废所有在途响应
          clearInterval(pollTimer.current)
          sessionIdRef.current = ''
          setPhase('ok')
          setLoginMsg(`已添加 ${r.account.nickname || r.account.uid}（${r.account.region}），已生效`)
          setAuthUrl('')
          await load()
          onChanged()
        } else if (r.status === 'pending') {
          setLoginMsg(`等待浏览器完成登录…（${r.message}）`)
        }
        // busy：上一次查询还没回来，静默跳过这轮（服务端回 200 + status=busy）
      } catch (e) {
        if (gen !== pollGen.current) return
        clearInterval(pollTimer.current)
        setPhase('idle')
        setErr(e instanceof Error ? e.message : String(e))
      }
    }, 3000)
  }

  const cancelLogin = () => {
    pollGen.current++
    clearInterval(pollTimer.current)
    sessionIdRef.current = ''
    setPhase('idle')
    setAuthUrl('')
    setLoginMsg('已取消本地轮询（上游 state 仍然有效，可稍后重新走一次）')
  }

  /**
   * 导入凭证。payload 省略时用粘贴框内容（兼容原调用点）。
   * 支持单条（嵌套/扁平形）与导出批量格式（顶层 accounts 数组）自动判别。
   */
  const doImport = async (payload?: string) => {
    const text = (payload ?? importText).trim()
    if (!text) return
    setErr('')
    setLoginMsg('')
    try {
      // 先探测是否为导出批量格式（顶层含 accounts 数组）——是则走批量接口，
      // 逐条导入且部分失败不中断；否则按单条（原有行为）。
      let isBatch = false
      try {
        const probe = JSON.parse(text)
        isBatch = Array.isArray(probe?.accounts) && probe.accounts.length > 0
      } catch {
        // 解析失败交给后端报明确错误（这里不抢先判）
      }
      if (isBatch) {
        const r = await importAuthBatch(text)
        if (payload === undefined) setImportText('')
        const okN = r.imported.length
        const failN = r.failed.length
        setLoginMsg(
          `批量导入：成功 ${okN} 个${
            failN
              ? `，失败 ${failN} 个（${r.failed
                  .slice(0, 3)
                  .map((f) => `#${f.index}: ${f.error}`)
                  .join('；')}${failN > 3 ? ' …' : ''}）`
              : ''
          }，池中现有 ${r.loaded} 个`,
        )
        if (failN > 0) setErr(`有 ${failN} 条未导入，详见上方说明`)
      } else {
        const r = await importAuth(text)
        if (payload === undefined) setImportText('')
        setLoginMsg(`已写入 ${r.file}，已生效`)
      }
      await load()
      onChanged()
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    }
  }

  /**
   * 从文件导入（拖放或选择）。支持多选：逐个文件调用 doImport，
   * 与批量格式一样「坏文件不中断其余」。
   */
  const importFromFiles = async (fileList: FileList | null) => {
    if (!fileList || fileList.length === 0) return
    const names: string[] = []
    for (const f of Array.from(fileList)) {
      try {
        const text = await f.text()
        await doImport(text)
        names.push(f.name)
      } catch (e) {
        setErr(`读取 ${f.name} 失败：${e instanceof Error ? e.message : String(e)}`)
      }
    }
    if (names.length > 0) setLoginMsg(`已从文件导入 ${names.length} 个：${names.join('、')}`)
    if (fileInputRef.current) fileInputRef.current.value = '' // 允许重复选同一文件
  }

  /**
   * 导出凭证。默认**不含 token**（可安全分享）；勾选后才带 token 明文，
   * 此时二次确认 —— 文件拿到即可完全接管账号。
   */
  const doExport = async () => {
    if (exportWithTokens) {
      const ok = confirm(
        '⚠️ 将导出 accessToken / refreshToken **明文**。\n\n' +
          '拿到该文件即可完全接管这些账号（global 账号凭证有效期可达 365 天）。\n' +
          '请仅在可信环境保存、传输，用完及时删除。\n\n确认导出？',
      )
      if (!ok) return
    }
    setErr('')
    setLoginMsg('')
    try {
      const r = await exportAccounts(exportWithTokens, exportFile || undefined)
      const blob = JSON.stringify(r, null, 2)
      const name = `wb2api-accounts-${r.exportedAt.replace(/[:.]/g, '-')}${
        exportWithTokens ? '' : '-no-token'
      }.json`
      const url = URL.createObjectURL(new Blob([blob], { type: 'application/json' }))
      const a = document.createElement('a')
      a.href = url
      a.download = name
      a.click()
      URL.revokeObjectURL(url)
      setLoginMsg(
        `已导出 ${r.count} 个账号${exportWithTokens ? '（含 token 明文）' : '（不含 token）'} → ${name}`,
      )
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    }
  }

  /** 正在切换启用/禁用的 uid（毫秒级，仅用于立刻禁用按钮） */
  const [toggling, setToggling] = useState<Set<string>>(new Set())
  /**
   * 表格内动作（启用/禁用）的结果提示。
   * 与 loginMsg 分开：loginMsg 渲染在「添加账号」面板里，而这里是表格上的操作，
   * 提示必须紧挨着被操作的行 —— 否则用户在表格上点了按钮，反馈出现在上方另一个面板里。
   */
  const [rowMsg, setRowMsg] = useState('')

  /**
   * 人工启用/禁用。只改池中调度状态，不碰凭证文件 —— 误禁用可一键恢复，
   * 误删除要重新登录，所以这是两个分开的动作。
   */
  const doToggle = async (f: AuthFileView) => {
    const next = !f.disabled
    if (next) {
      const ok = confirm(
        `确认禁用账号 ${f.nickname || f.uid.slice(0, 8)}？\n\n` +
          '该账号会立即退出轮换，不再被任何请求选中（凭证文件保留）。\n' +
          '签到与 token 保活不会解除禁用；只有在此页点「启用」，\n' +
          '或对它执行「测试连接 / 重置（勾选解除禁用）」才会恢复。',
      )
      if (!ok) return
    }
    setErr('')
    setRowMsg('')
    setToggling((s) => new Set(s).add(f.uid))
    try {
      const r = await toggleAccount(f.uid, next)
      setRowMsg(`账号 ${f.nickname || f.uid.slice(0, 8)} 已${r.action}`)
      await load()
      onChanged()
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setToggling((s) => {
        const n = new Set(s)
        n.delete(f.uid)
        return n
      })
    }
  }

  const doDelete = async (f: AuthFileView) => {
    if (!confirm(`确认删除账号文件 ${f.file}？\n\n该操作只删除本地凭证文件，网关会立即把它从池中移除。`)) {
      return
    }
    setErr('')
    try {
      await deleteAuth(f.file)
      await load()
      onChanged()
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    }
  }

  /** 重扫 auths 目录。手动改善了目录（从面板之外）时用它让池子对齐。 */
  const doRescan = async () => {
    setErr('')
    setLoginMsg('')
    setRescanning(true)
    try {
      const r = await reloadPool()
      setLoginMsg(`已重新扫描 auths 目录，池中 ${r.loaded} 个账号。`)
      await load()
      onChanged()
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setRescanning(false)
    }
  }

  // 待加载 = 磁盘上有文件、但网关的账号池里没有。loadedUids 为 null（查不到）时不做判断。
  const pendingUids =
    loadedUids === null ? [] : files.filter((f) => f.uid && !loadedUids.has(f.uid))

  return (
    <>
      {pendingUids.length > 0 && (
        <div className="note err">
          <span>●</span>
          <div style={{ flex: 1 }}>
            <b>{pendingUids.length} 个账号文件未被当前网关加载：</b>
            <span className="mono"> {pendingUids.map((f) => f.nickname || f.uid).join('、')}</span>
            <div className="dim" style={{ fontSize: 11.5, marginTop: 3 }}>
              从本页增删的账号会自动重扫目录、立即生效；只有直接在{' '}
              <span className="mono">auths/</span> 里改动（绕开了面板）才需要点右侧按钮。
            </div>
          </div>
          {groupsDirty && (
            <>
              <span className="badge warn" title="有未保存的分组改动">
                分组未保存
              </span>
              <button className="btn primary" onClick={() => void doSaveGroups()}>
                保存分组
              </button>
            </>
          )}
          <button className="btn primary" onClick={() => void doRescan()} disabled={rescanning}>
            {rescanning ? <span className="spin" /> : null}
            {rescanning ? ' 扫描中' : '重新扫描 auths'}
          </button>
        </div>
      )}

      {err && (
        <div className="note err">
          <span>●</span>
          <div style={{ wordBreak: 'break-word' }}>{err}</div>
        </div>
      )}

      <div className="grid-2 forms">
        <div className="panel">
          <header>
            <h2>添加账号</h2>
          </header>
          <div className="body">
            {/* 凭证导入已统一到右侧「导入 / 导出」面板（原「粘贴凭证」标签在此，
                与导出分处两地导致用户找不到导入入口）。此处只保留设备码登录。 */}
            {(
              <>
                <div className="field">
                  <label>账号区域</label>
                  <div className="tabs">
                    <button
                      aria-selected={region === 'cn'}
                      onClick={() => setRegion('cn')}
                      disabled={phase === 'waiting'}
                    >
                      国内版 cn
                    </button>
                    <button
                      aria-selected={region === 'global'}
                      onClick={() => setRegion('global')}
                      disabled={phase === 'waiting'}
                    >
                      国外版 global
                    </button>
                  </div>
                  <div className="dim" style={{ fontSize: 11.5, marginTop: 6 }}>
                    区域决定上游域名（{region === 'cn' ? 'copilot.tencent.com' : 'www.workbuddy.ai'}），
                    最终以凭证里的 domain 为准。
                  </div>
                </div>

                <div className="row">
                  <button
                    className="btn primary"
                    onClick={beginLogin}
                    disabled={phase === 'waiting'}
                  >
                    {phase === 'waiting' ? <span className="spin" /> : null}
                    {phase === 'waiting' ? ' 等待登录' : '生成授权链接'}
                  </button>
                  {phase === 'waiting' && (
                    <button className="btn danger" onClick={cancelLogin}>
                      取消
                    </button>
                  )}
                </div>

                {authUrl && (
                  <div className="field" style={{ marginTop: 12 }}>
                    <label>在浏览器打开此链接完成登录</label>
                    <div className="row">
                      <a
                        href={authUrl}
                        target="_blank"
                        rel="noreferrer"
                        className="mono"
                        style={{ fontSize: 12, color: 'var(--accent)', wordBreak: 'break-all' }}
                      >
                        {authUrl}
                      </a>
                    </div>
                    <button
                      className="btn"
                      style={{ marginTop: 8 }}
                      onClick={() => void navigator.clipboard.writeText(authUrl)}
                    >
                      复制链接
                    </button>
                  </div>
                )}

                {loginMsg && (
                  <div className="dim" style={{ fontSize: 12, marginTop: 10 }}>
                    {loginMsg}
                  </div>
                )}
              </>
            )}

            <div className="dim" style={{ fontSize: 11.5, marginTop: 14 }}>
              登录复用仓库根的 <span className="mono">login.exe</span>（设备码流程），
              该文件不存在时会尝试 <span className="mono">go build</span> 一次。
              此处不会代替网关做签到，签到由网关的定时任务负责。
              已有凭证要导入请用右侧「<span className="mono">导入 / 导出</span>」。
            </div>
          </div>
        </div>

        <div className="panel">
          <header>
            <h2>导入 / 导出</h2>
          </header>
          <div className="body">
            {/* 导入区：与导出并列，解决「有导出却找不到导入」的入口不对称。
                支持拖放与文件选择（一次多个），也保留粘贴框。 */}
            <div className="field">
              <label>导入账号</label>
              <div
                onDragOver={(e) => {
                  e.preventDefault()
                  setDragOver(true)
                }}
                onDragLeave={() => setDragOver(false)}
                onDrop={(e) => {
                  e.preventDefault()
                  setDragOver(false)
                  void importFromFiles(e.dataTransfer.files)
                }}
                onClick={() => fileInputRef.current?.click()}
                style={{
                  border: `1px dashed ${dragOver ? 'var(--accent)' : 'var(--border)'}`,
                  borderRadius: 6,
                  padding: '14px 12px',
                  textAlign: 'center',
                  cursor: 'pointer',
                  fontSize: 12,
                  color: 'var(--text-dim)',
                  background: dragOver ? 'var(--bg-hover, rgba(0,0,0,.03))' : undefined,
                }}
                title="支持一次拖入多个 JSON 文件；也可点击选择"
              >
                拖入 JSON 文件到此处，或<span style={{ color: 'var(--accent)' }}>点击选择</span>
                <div style={{ fontSize: 11, marginTop: 4 }}>
                  支持导出文件（含 accounts 数组）与单个 auths/*.json，可多选
                </div>
              </div>
              <input
                ref={fileInputRef}
                type="file"
                accept=".json,application/json"
                multiple
                style={{ display: 'none' }}
                onChange={(e) => void importFromFiles(e.target.files)}
              />
              <textarea
                rows={4}
                value={importText}
                onChange={(e) => setImportText(e.target.value)}
                placeholder="…或直接粘贴凭证 JSON（嵌套形 / 扁平形 / 导出的批量格式均可）"
                style={{ width: '100%', fontSize: 11.5, marginTop: 8 }}
              />
              <button
                className="btn"
                onClick={() => void doImport()}
                disabled={!importText.trim()}
                style={{ marginTop: 8 }}
              >
                导入粘贴内容
              </button>
            </div>

            <hr style={{ border: 0, borderTop: '1px solid var(--border)', margin: '16px 0' }} />

            <div className="field">
              <label>范围</label>
              <select
                value={exportFile}
                onChange={(e) => setExportFile(e.target.value)}
                style={{ width: '100%' }}
              >
                <option value="">全部账号（{files.length} 个）</option>
                {files.map((f) => (
                  <option key={f.file} value={f.file}>
                    {f.nickname || f.uid.slice(0, 8)} · {f.region} · {f.file}
                  </option>
                ))}
              </select>
            </div>

            <div className="field">
              <label
                style={{ display: 'flex', alignItems: 'center', gap: 6, cursor: 'pointer' }}
                title="勾选后导出文件含 accessToken/refreshToken 明文，可直接用于迁移"
              >
                <input
                  type="checkbox"
                  checked={exportWithTokens}
                  onChange={(e) => setExportWithTokens(e.target.checked)}
                />
                包含 token（可直接迁移到另一台机器）
              </label>
              <div
                className="dim"
                style={{ fontSize: 11.5, marginTop: 4, color: exportWithTokens ? 'var(--warn, #e65100)' : undefined }}
              >
                {exportWithTokens
                  ? '⚠️ 导出文件含 token 明文，拿到即可完全接管账号 —— 只在可信环境保存。'
                  : '默认不含 token，可安全分享/归档；但导入后需重新登录才能使用。'}
              </div>
            </div>

            <button className="btn primary" onClick={() => void doExport()}>
              导出 JSON
            </button>

            <div className="dim" style={{ fontSize: 11.5, marginTop: 14 }}>
              导出格式与 <span className="mono">auths/*.json</span> 同构，
              可直接被上面的「粘贴凭证」导回（也支持一次导入多个）。
            </div>
          </div>
        </div>
      </div>

      <div className="panel" style={{ marginTop: 14 }}>
        <header>
          <h2>凭证文件</h2>
          <span className="sub">
            {files.length} 个
          </span>
          <div className="spacer" />
          <button className="btn" onClick={() => void load()} disabled={busy}>
            {busy ? <span className="spin" /> : null}
            {busy ? ' 读取中' : '重新读取'}
          </button>
        </header>

        {/* 启用/禁用的结果紧贴在表格上方 —— 它是表格内的动作，反馈不该跑到上面的面板里 */}
        {rowMsg && (
          <div className="dirty-bar" style={{ color: 'var(--ok)', background: 'rgba(63, 185, 80, 0.08)', borderBottomColor: 'rgba(63, 185, 80, 0.3)' }}>
            <span>{rowMsg}</span>
            <div className="spacer" />
            <button className="btn" onClick={() => setRowMsg('')}>
              知道了
            </button>
          </div>
        )}

        {files.length === 0 ? (
          <div className="empty">
            还没有账号文件。用上方「设备码登录」添加第一个账号。
          </div>
        ) : (
          <div className="scroll-x">
            <table>
              <thead>
                <tr>
                  <th>账号</th>
                  <th>区域</th>
                  <th>状态</th>
                  <th title="账号分组。API key 可绑定到分组：未分组 key 只用未分组账号，分组 key 只用该组账号（互不通用、不回退）">
                    分组
                  </th>
                  <th title="最近几次调用（成功/失败原因）。用于回答「这个号最近发生了什么」——注意「凭证有效期」上方的状态不受此影响">
                    最近调用
                  </th>
                  <th>凭证有效期</th>
                  <th>文件</th>
                  <th>操作</th>
                </tr>
              </thead>
              <tbody>
                {files.map((f) => {
                  const isToggling = toggling.has(f.uid)
                  // 加载态以 inPool 为准：它与后端 404 判定同源，不会出现
                  // 「状态列说已加载、禁用按钮却灰着」这种自相矛盾的行。
                  // loadedUids 只在老后端不回 inPool 时兜底；都没有才显示「未知」。
                  const isLoaded =
                    typeof f.inPool === 'boolean' ? f.inPool : (loadedUids?.has(f.uid) ?? null)
                  return (
                    <tr key={f.file}>
                      <td>
                        <div style={{ color: 'var(--text-strong)' }}>
                          {f.nickname || '（无昵称）'}
                        </div>
                        <div className="mono dim" style={{ fontSize: 11 }}>
                          {f.uid || '(缺 uid)'} · {f.tokenHint}
                        </div>
                      </td>
                      <td>
                        <span className={'badge' + (f.region === 'global' ? ' accent' : '')}>
                          {f.region}
                        </span>
                      </td>
                      <td>
                        {isLoaded === null ? (
                          <span className="badge" title="查不到网关账号池，无法判断">
                            未知
                          </span>
                        ) : !isLoaded ? (
                          <span className="badge warn" title="磁盘上有这个文件，但不在网关的账号池里">
                            未加载
                          </span>
                        ) : f.disabled ? (
                          <span
                            className="badge err"
                            title={f.disabledReason || '已禁用：不参与轮换'}
                          >
                            已禁用
                          </span>
                        ) : (
                          <span className="badge ok">已加载</span>
                        )}
                      </td>
                      <td style={{ fontSize: 11.5 }}>
                        <input
                          value={groups[f.uid] ?? ''}
                          placeholder="未分组"
                          title="改分组后按「保存分组」生效；空 = 移出分组"
                          onChange={(e) => setGroupDraft(f.uid, e.target.value)}
                          style={{ width: 86 }}
                          disabled={!groupsAvailable}
                        />
                      </td>
                      <td style={{ fontSize: 11 }}>
                        <CallHistory calls={f.calls} lastSuccess={f.lastSuccess} />
                      </td>
                      <td className="nowrap" style={{ fontSize: 11.5 }}>
                        {f.expired ? (
                          <span className="badge err">已过期</span>
                        ) : (
                          <span className="mono dim">
                            {fmtTime(new Date(f.expiresAt * 1000).toISOString())}
                          </span>
                        )}
                      </td>
                      {/* 文件名不折行：窄屏下它会把每行撑成好几行，反而不如让表格横向滚动
                          （与 .grid-2.eq 的既有约定一致：铺得下就铺满，铺不下才滚）。 */}
                      <td className="mono dim nowrap" style={{ fontSize: 11 }}>
                        {f.file}
                      </td>
                      <td>
                        <div className="row" style={{ gap: 6 }}>
                          {/* 只有已在池中的账号才能切换调度状态：文件在盘上但没加载时，
                              disabled 没有承载对象（后端会回 404）。 */}
                          <button
                            className="btn"
                            onClick={() => void doToggle(f)}
                            disabled={isToggling || !f.inPool}
                            title={
                              f.inPool
                                ? f.disabled
                                  ? '恢复参与轮换（保留凭证文件）'
                                  : '退出轮换但保留凭证文件'
                                : '账号未加载到网关池，无法切换'
                            }
                          >
                            {isToggling ? <span className="spin" /> : null}
                            {f.disabled ? '启用' : '禁用'}
                          </button>
                          <button className="btn danger" onClick={() => void doDelete(f)}>
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
        {authDir && (
          <div className="dim mono" style={{ fontSize: 11, padding: '10px 14px' }}>
            {authDir}
          </div>
        )}
      </div>
    </>
  )
}

/**
 * CallHistory 一个账号最近几次调用的紧凑展示。
 *
 * 为什么需要（2026-10-21）：排查「被封号是否因为闲置」时发现，**成功没有可靠日志**
 * （请求日志行不带日期且批量刷盘），而 `lastSuccess` 这个字段名有欺骗性 ——
 * 它记的是「末次成功」，号一直失败时不更新，看起来像"很久没被用"。
 * 本组件把「末次成功」与「最近若干次调用」并列，一眼能区分
 * 「没被使用」与「被使用但一直失败」。
 */
function CallHistory({ calls, lastSuccess }: { calls?: CallRecord[]; lastSuccess?: string }) {
  if (!calls || calls.length === 0) {
    return <span className="dim">无记录</span>
  }
  const okN = calls.filter((c) => c.ok).length
  const failN = calls.length - okN
  return (
    <div style={{ lineHeight: 1.5 }}>
      <div className="dim" title={lastSuccess ? `末次成功：${lastSuccess}` : '还没有成功过'}>
        最近 {calls.length}：<span style={{ color: 'var(--ok)' }}>{okN}✓</span>
        {failN > 0 && (
          <>
            {' '}
            <span style={{ color: 'var(--err)' }}>{failN}✗</span>
          </>
        )}
      </div>
      {calls.slice(0, 3).map((c, i) => (
        <div
          key={i}
          className="mono dim"
          style={{ fontSize: 10.5, whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis', maxWidth: 190 }}
          title={`${c.t}${c.model ? ' · ' + c.model : ''}${c.ms ? ' · ' + c.ms + 'ms' : ''}${c.reason ? ' · ' + c.reason : ''}`}
        >
          {c.ok ? '✓' : '✗'} {fmtTime(c.t)} {c.reason ? c.reason.slice(0, 18) : ''}
        </div>
      ))}
    </div>
  )
}
