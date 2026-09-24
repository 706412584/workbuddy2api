import { useEffect, useMemo, useState } from 'react'
import type { ModelInfo, ModelList, RegionModels } from '../api/types'
import { getRegionModels } from '../api/client'

interface Props {
  models: ModelList | null
  /** 当前密钥是否绑定了区域——决定列表是「全池并集」还是「单区域」 */
  keyRegion?: string
}

function fmtTokens(n: number): string {
  if (!n) return '—'
  if (n >= 1000) return `${Math.round(n / 1000)}K`
  return String(n)
}

/** 一枚区域徽标；不可用就用普通（灰）徽标，不用红色 —— 这不是错误。 */
function RegionBadge({ region, on }: { region: string; on: boolean }) {
  return <span className={'badge' + (on ? ' accent' : '')}>{region}</span>
}

/**
 * 倍率徽标。三态必须区分（上游教训：缺失≠免费）：
 *   - 未下发（undefined）→ 「—」灰字，不写 x0.00
 *   - x0.00 → 「免费」绿字（真免费，如 global deepseek-v4.1-flash）
 *   - 其他 → 原文，越贵颜色越暖，便于一眼分辨
 */
function CreditsBadge({ credits }: { credits?: string }) {
  if (credits === undefined) {
    return (
      <span className="dim" style={{ fontSize: 11 }}>
        —
      </span>
    )
  }
  const n = Number(credits.replace(/^x/i, ''))
  const free = n === 0
  const color = free
    ? 'var(--ok, #2e7d32)'
    : n >= 3
      ? 'var(--danger, #c62828)'
      : n >= 1
        ? 'var(--warn, #e65100)'
        : 'var(--text-strong)'
  return (
    <span className="mono" style={{ color, fontWeight: free ? 600 : 400 }}>
      {free ? '免费' : credits}
    </span>
  )
}

/** 能力旗标：只显示有值的，紧凑排布。 */
function Caps({ m }: { m: ModelInfo }) {
  const caps: string[] = []
  if (m.supports_images) caps.push('图')
  if (m.supports_reasoning) caps.push('思')
  if (m.supports_tool_call) caps.push('具')
  if (caps.length === 0) return <span className="dim">—</span>
  return (
    <span className="dim" style={{ fontSize: 11, letterSpacing: 1 }} title="图=多模态 思=推理 具=工具调用">
      {caps.join(' ')}
    </span>
  )
}

export function Models({ models, keyRegion }: Props) {
  const [q, setQ] = useState('')
  const [regionModels, setRegionModels] = useState<RegionModels | null>(null)
  /** 按倍率升序排（免费在前）；默认关，避免打乱用户熟悉的字母序 */
  const [sortByCredits, setSortByCredits] = useState(false)

  /** 每个模型在哪些区域可用。取自 /__admin/models（用区域绑定密钥分别问网关）。 */
  useEffect(() => {
    let cancelled = false
    getRegionModels()
      .then((m) => {
        if (!cancelled) setRegionModels(m)
      })
      .catch(() => {
        // 取不到就不显示区域列，其余照旧
      })
    return () => {
      cancelled = true
    }
  }, [])

  /**
   * 每个 id 在两区各自的条目。同名模型两区倍率可能不同
   * （实测 deepseek-v4.1-flash：CN x0.11 / global x0.00），
   * 故按区域分别索引，表格里并排展示而非合并。
   */
  const byRegion = useMemo(() => {
    const cn = new Map<string, ModelInfo>()
    const global = new Map<string, ModelInfo>()
    for (const m of regionModels?.details?.cn ?? []) cn.set(m.id, m)
    for (const m of regionModels?.details?.global ?? []) global.set(m.id, m)
    return { cn, global }
  }, [regionModels])

  const rows = useMemo(() => {
    const data = models?.data ?? []
    const needle = q.trim().toLowerCase()
    const filtered = needle ? data.filter((m) => m.id.toLowerCase().includes(needle)) : data
    if (!sortByCredits) return filtered
    // 倍率升序：免费(0) 在前，「未下发」视为未知排最后（不假设它免费）
    const rank = (m: ModelInfo): number => {
      const det = byRegion.cn.get(m.id) ?? byRegion.global.get(m.id)
      const c = m.credits ?? det?.credits
      if (c === undefined) return Number.POSITIVE_INFINITY
      const n = Number(c.replace(/^x/i, ''))
      return Number.isNaN(n) ? Number.POSITIVE_INFINITY : n
    }
    return [...filtered].sort((a, b) => rank(a) - rank(b) || a.id.localeCompare(b.id))
  }, [models, q, sortByCredits, byRegion])

  if (!models) {
    return <div className="empty">尚无数据 —— 请先在右上角填入 API 密钥。</div>
  }

  return (
    <>
      <div className="note info">
        <span>●</span>
        <div>
          {keyRegion ? (
            <>
              当前密钥绑定区域 <b>{keyRegion}</b>，故只列出该区域可服务的模型。
              列表里出现的模型一定能用；请求不在列表里的模型会得到 404。
            </>
          ) : (
            <>
              当前密钥<b>未绑定区域</b>，列出的是账号池中各区域模型的并集。
              网关会按模型把请求路由到支持它的区域；若某区域没有账号，该区域的模型不会出现。
            </>
          )}
        </div>
      </div>

      <div className="panel">
        <header>
          <h2>模型</h2>
          <span className="sub">{rows.length} / {models.data.length}</span>
          <div className="spacer" />
          <label className="dim" style={{ fontSize: 12, display: 'flex', alignItems: 'center', gap: 5 }}>
            <input
              type="checkbox"
              checked={sortByCredits}
              onChange={(e) => setSortByCredits(e.target.checked)}
            />
            按倍率排序
          </label>
          <input
            placeholder="筛选模型 id…"
            value={q}
            onChange={(e) => setQ(e.target.value)}
            style={{ width: 200 }}
          />
        </header>

        {rows.length === 0 ? (
          <div className="empty">没有匹配的模型。</div>
        ) : (
          <div className="scroll-x">
            <table>
              <thead>
                <tr>
                  <th>模型 ID</th>
                  <th>区域</th>
                  <th title="该模型在两区各自的积分倍率；免费档已高亮">倍率</th>
                  <th>上下文</th>
                  <th>最大输出</th>
                  <th title="图=多模态 思=推理 具=工具调用">能力</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((m) => {
                  const cn = regionModels?.cn.includes(m.id) ?? false
                  const global = regionModels?.global.includes(m.id) ?? false
                  const cnDet = byRegion.cn.get(m.id)
                  const globalDet = byRegion.global.get(m.id)
                  const hasDetails = regionModels?.details !== undefined
                  // 单区模型直接显示该区倍率；两区都有则并排显示（可能不同）
                  const bothRegions = cn && global && hasDetails
                  return (
                    <tr key={m.id}>
                      <td className="mono" style={{ color: 'var(--text-strong)' }}>
                        {m.id}
                        {m.name && m.name !== m.id && (
                          <span className="dim" style={{ fontSize: 11, marginLeft: 6 }}>
                            {m.name}
                          </span>
                        )}
                      </td>
                      <td>
                        {regionModels ? (
                          <div className="row" style={{ flexWrap: 'nowrap', gap: 6 }}>
                            <RegionBadge region="cn" on={cn} />
                            <RegionBadge region="global" on={global} />
                            {!cn && !global && (
                              <span className="dim" style={{ fontSize: 11 }}>
                                区域未知
                              </span>
                            )}
                          </div>
                        ) : (
                          <span className="dim">—</span>
                        )}
                      </td>
                      <td>
                        {!hasDetails ? (
                          <CreditsBadge credits={m.credits} />
                        ) : bothRegions ? (
                          <div className="row" style={{ flexWrap: 'nowrap', gap: 8 }}>
                            <span style={{ fontSize: 11 }} className="dim">
                              cn
                            </span>
                            <CreditsBadge credits={cnDet?.credits} />
                            <span style={{ fontSize: 11 }} className="dim">
                              global
                            </span>
                            <CreditsBadge credits={globalDet?.credits} />
                          </div>
                        ) : (
                          <CreditsBadge credits={(cnDet ?? globalDet)?.credits} />
                        )}
                      </td>
                      <td className="mono">{fmtTokens(m.context_length)}</td>
                      <td className="mono">{fmtTokens(m.max_output_tokens ?? 0)}</td>
                      <td>
                        <Caps m={cnDet ?? globalDet ?? m} />
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}
      </div>

      {regionModels && (
        <div className="dim" style={{ fontSize: 11.5, lineHeight: 1.7 }}>
          区域列取自各区域绑定密钥分别问到的模型表（<span className="mono">/v1/models</span>
          会随密钥绑定的区域收窄），亮色表示该区域可用。
          {regionModels.details
            ? ' 两区都有的模型并排显示各自倍率 —— 同名模型两区倍率可能不同（如 deepseek-v4.1-flash 在 cn 与 global 就不一样）。'
            : ''}
          <b>「—」表示上游未下发倍率，不等于免费</b>（图像/视频类模型常见）。
          {regionModels.degraded &&
            ' 缺少某区域的绑定密钥，该区域一列取自「不限区域」密钥的并集，可能把另一区的模型也算进来 —— 仅供参考。'}
        </div>
      )}
    </>
  )
}
