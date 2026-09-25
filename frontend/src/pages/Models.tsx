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

/**
 * 跨区别名提示：说明「同名模型在另一区用的是另一个 id」。
 *
 * 存在的意义是消除面板上的重复行歧义：Hy4 在 cn 叫 `hy4-preview`（x0.29）、
 * global 叫 `hy4-preview-f`（x0.00），两行展示名都是 "Hy4 preview"，
 * 不标注就会被当成两个不同的模型。
 *
 * 措辞刻意写成「同名模型在 X 为 Y」而不是「X 侧为 Y」：区域徽标是按**本行 id**
 * 判定的，本行 id 不在对区名单里时徽标是灰的。写成「X 侧为 Y」会与灰徽标打架。
 */
function AliasNote({ alias }: { alias: { region: string; id: string; credits?: string } }) {
  return (
    <span className="dim" style={{ fontSize: 11 }}>
      同名模型在 {alias.region} 为 <span className="mono">{alias.id}</span>
      {alias.credits !== undefined && (
        <>
          （
          <span style={{ color: alias.credits === 'x0.00' ? 'var(--ok, #2e7d32)' : 'inherit' }}>
            {alias.credits === 'x0.00' ? '免费' : alias.credits}
          </span>
          ）
        </>
      )}
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

  /**
   * 跨区别名：同一展示名在两区用了**不同的 id**。
   *
   * 实测 Hy4 —— cn 是 `hy4-preview`（x0.29），global 是 `hy4-preview-f`（x0.00）。
   * 面板的倍率列按 id 关联两区，故这类模型会渲染成两行、倍率看似互不相干，
   * 而两行的展示名又完全相同，用户看到的就是「两个一模一样的 Hy4 preview」。
   *
   * 只在**两区倍率不同**时标注：倍率相同则解释价值为零（如 kimi-k3-1 / kimi-k3
   * 都是 x1.62），标注只会制造噪音。同理，两区都未下发倍率的（auto / default-model）
   * 自然被排除——undefined === undefined。
   *
   * 同名 id 在一区内不唯一时放弃配对（歧义不猜，宁可少标）。实测 global 的
   * `deepseek-v4.1-flash` 与 `deepseek-v4.1-flash-sg` 共用展示名
   * "Deepseek-V4.1-Flash"，正属此列。
   */
  const crossRegionAlias = useMemo(() => {
    const out = new Map<string, { region: string; id: string; credits?: string }>()
    const details = regionModels?.details
    if (!details) return out
    const cnList = details.cn ?? []
    const globalList = details.global ?? []
    const globalIDs = new Set(globalList.map((m) => m.id))
    const groupByName = (list: ModelInfo[]) => {
      const m = new Map<string, ModelInfo[]>()
      for (const x of list) {
        if (!x.name) continue
        const arr = m.get(x.name)
        if (arr) arr.push(x)
        else m.set(x.name, [x])
      }
      return m
    }
    const cnByName = groupByName(cnList)
    const globalByName = groupByName(globalList)
    for (const c of cnList) {
      if (!c.name || globalIDs.has(c.id)) continue // 两区同 id：无需别名
      const cnGroup = cnByName.get(c.name) ?? []
      const globalGroup = globalByName.get(c.name) ?? []
      if (cnGroup.length !== 1 || globalGroup.length !== 1) continue // 无对应或歧义
      const g = globalGroup[0]
      if (c.credits === g.credits) continue // 倍率相同：无解释价值
      out.set(c.id, { region: 'global', id: g.id, credits: g.credits })
      out.set(g.id, { region: 'cn', id: c.id, credits: c.credits })
    }
    return out
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
                  // 该 id 在另一区的对应 id（仅当跨区 id 不同且倍率有差异时存在）
                  const alias = crossRegionAlias.get(m.id)
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
                          // 有跨区别名时改用纵向堆叠：倍率一行、提示一行。
                          // 横排会被窄列挤成从句中折断，可读性差。
                          <div
                            className="row"
                            style={{ flexDirection: alias ? 'column' : 'row', flexWrap: 'nowrap', gap: alias ? 2 : 8, alignItems: 'flex-start' }}
                          >
                            <CreditsBadge credits={(cnDet ?? globalDet)?.credits} />
                            {alias && <AliasNote alias={alias} />}
                          </div>
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
          {crossRegionAlias.size > 0 &&
            ' 少数模型两区用了不同的 id（如 Hy4：cn 是 hy4-preview、global 是 hy4-preview-f），会各自成行，行内已注明同名模型在对区的 id 与倍率；注意区域徽标只按本行 id 判定，故这类行会有一枚徽标是灰的。'}
          <b>「—」表示上游未下发倍率，不等于免费</b>（图像/视频类模型常见）。
          {regionModels.degraded &&
            ' 缺少某区域的绑定密钥，该区域一列取自「不限区域」密钥的并集，可能把另一区的模型也算进来 —— 仅供参考。'}
        </div>
      )}
    </>
  )
}
