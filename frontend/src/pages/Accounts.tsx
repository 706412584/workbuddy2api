import { useCallback, useEffect, useRef, useState } from 'react'
import type { AuthFileView } from '../api/types'
import {
  ApiError,
  deleteAuth,
  fmtTime,
  getLoadedAccounts,
  importAuth,
  listAuthFiles,
  pollLogin,
  reloadPool,
  startLogin,
} from '../api/client'
interface Props {
  onChanged: () => void
}

type Mode = 'oauth' | 'import'

export function Accounts({ onChanged }: Props) {
  const [files, setFiles] = useState<AuthFileView[]>([])
  const [authDir, setAuthDir] = useState('')
  /** 网关已加载的 uid 集合；null = 还没问出来（此时不判断是否加载） */
  const [loadedUids, setLoadedUids] = useState<Set<string> | null>(null)
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)

  const [mode, setMode] = useState<Mode>('oauth')
  const [region, setRegion] = useState<'cn' | 'global'>('cn')

  // 登录流程状态机
  const [authUrl, setAuthUrl] = useState('')
  const [phase, setPhase] = useState<'idle' | 'waiting' | 'ok'>('idle')
  const [loginMsg, setLoginMsg] = useState('')
  const [rescanning, setRescanning] = useState(false)

  const [importText, setImportText] = useState('')
  const pollTimer = useRef(0)
  /** 轮询代数：递增即作废此前所有在途响应，避免迟到响应覆盖新状态。 */
  const pollGen = useRef(0)
  /** 本次登录会话标识，由 startLogin 返回；轮询时带回以隔离并发登录。 */
  const sessionIdRef = useRef('')

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

  const doImport = async () => {
    setErr('')
    try {
      const r = await importAuth(importText)
      setImportText('')
      setLoginMsg(`已写入 ${r.file}，已生效`)
      await load()
      onChanged()
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
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

      <div className="grid-2">
        <div className="panel">
          <header>
            <h2>添加账号</h2>
          </header>
          <div className="body">
            <div className="field">
              <div className="tabs">
                <button aria-selected={mode === 'oauth'} onClick={() => setMode('oauth')}>
                  设备码登录
                </button>
                <button aria-selected={mode === 'import'} onClick={() => setMode('import')}>
                  粘贴凭证
                </button>
              </div>
            </div>

            {mode === 'oauth' ? (
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
            ) : (
              <>
                <div className="field">
                  <label>凭证 JSON（网关读取的嵌套形）</label>
                  <textarea
                    rows={12}
                    value={importText}
                    onChange={(e) => setImportText(e.target.value)}
                    placeholder={`{\n "account": {"uid":"...","enterpriseId":"...","nickname":"..."},\n "auth": {"accessToken":"...","refreshToken":"...","expiresAt":0,"domain":"copilot.tencent.com"}\n}`}
                    style={{ width: '100%', fontSize: 11.5 }}
                  />
                  <div className="dim" style={{ fontSize: 11.5, marginTop: 6 }}>
                    也接受扁平形（accessToken/uid 直接在顶层）。文件名按 uid 生成，
                    同 uid 会覆盖。
                  </div>
                </div>
                <button className="btn primary" onClick={doImport} disabled={!importText.trim()}>
                  写入账号文件
                </button>
              </>
            )}

            <div className="dim" style={{ fontSize: 11.5, marginTop: 14 }}>
              登录复用仓库根的 <span className="mono">login.exe</span>（设备码流程），
              该文件不存在时会尝试 <span className="mono">go build</span> 一次。
              此处不会代替网关做签到，签到由网关的定时任务负责。
            </div>
          </div>
        </div>

        <div className="panel">
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

          {files.length === 0 ? (
            <div className="empty">
              还没有账号文件。用左侧「设备码登录」添加第一个账号。
            </div>
          ) : (
            <div className="scroll-x">
              <table>
                <thead>
                  <tr>
                    <th>账号</th>
                    <th>区域</th>
                    <th>已加载</th>
                    <th>凭证有效期</th>
                    <th>文件</th>
                    <th />
                  </tr>
                </thead>
                <tbody>
                  {files.map((f) => {
                    const isLoaded = loadedUids?.has(f.uid) ?? null
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
                          ) : isLoaded ? (
                            <span className="badge ok">已加载</span>
                          ) : (
                            <span className="badge warn" title="磁盘上有这个文件，但不在网关的账号池里">
                              未加载
                            </span>
                          )}
                        </td>
                        <td className="nowrap" style={{ fontSize: 11.5 }}>
                          {f.expired ? (
                            <span className="badge err">已过期</span>
                          ) : (
                            <span className="mono dim">{fmtTime(new Date(f.expiresAt * 1000).toISOString())}</span>
                          )}
                        </td>
                        <td className="mono dim" style={{ fontSize: 11 }}>
                          {f.file}
                        </td>
                        <td>
                          <button className="btn danger" onClick={() => void doDelete(f)}>
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
          {authDir && (
            <div className="dim mono" style={{ fontSize: 11, padding: '10px 14px' }}>
              {authDir}
            </div>
          )}
        </div>
      </div>
    </>
  )
}
