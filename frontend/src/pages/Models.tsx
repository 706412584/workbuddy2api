import { useEffect, useMemo, useState } from 'react'
import type { ModelList, RegionModels } from '../api/types'
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

export function Models({ models, keyRegion }: Props) {
  const [q, setQ] = useState('')
  const [regionModels, setRegionModels] = useState<RegionModels | null>(null)

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

  const rows = useMemo(() => {
    const data = models?.data ?? []
    const needle = q.trim().toLowerCase()
    return needle ? data.filter((m) => m.id.toLowerCase().includes(needle)) : data
  }, [models, q])

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
          <input
            placeholder="筛选模型 id…"
            value={q}
            onChange={(e) => setQ(e.target.value)}
            style={{ width: 220 }}
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
                  <th>上下文</th>
                  <th>最大输出</th>
                  <th>归属</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((m) => {
                  const cn = regionModels?.cn.includes(m.id) ?? false
                  const global = regionModels?.global.includes(m.id) ?? false
                  return (
                    <tr key={m.id}>
                      <td className="mono" style={{ color: 'var(--text-strong)' }}>
                        {m.id}
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
                      <td className="mono">{fmtTokens(m.context_length)}</td>
                      <td className="mono">{fmtTokens(m.max_output_tokens ?? 0)}</td>
                      <td className="dim">{m.owned_by}</td>
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
          {regionModels.degraded &&
            ' 缺少某区域的绑定密钥，该区域一列取自「不限区域」密钥的并集，可能把另一区的模型也算进来 —— 仅供参考。'}
        </div>
      )}
    </>
  )
}
