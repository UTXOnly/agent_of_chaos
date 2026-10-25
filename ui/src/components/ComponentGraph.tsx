import { useEffect, useMemo, useRef, useState } from 'react'
import type { ComponentStat, IntakeSample, TelemetrySeries } from '@shared/types'
import { cn, fmtBytes, fmtShort } from '@/lib/utils'

// The logs agent's pipeline as the agent itself reports it: one node per
// component that exposes logs_component_utilization (processor, strategy,
// sender per pipeline; worker and destination shared), plus the tailers and
// the intake. Every number on the picture is a metric; no node or edge is
// drawn from a guess. Hover for the live numbers, click for history and the
// raw series.
//
// The ratio is the agent's N=15 EWMA of busy time over wall time. A
// component stops its busy clock while it waits to hand off downstream
// (comp/logs-library/processor/processor.go), so 0% with a full buffer
// means blocked, not idle; the picture says so.

export interface NodeDef {
  id: string
  kind: 'tailer' | 'component' | 'intake' | 'sink'
  label: string
  sub: string
  col: number
  row: number // -1: centered on the column
  comp?: ComponentStat
  tone?: 'bad'
  state?: 'blocked' | 'saturated' | 'busy'
  detail: string // third line
}
interface EdgeDef {
  from: string
  to: string
  rate: number
  unit: 'logs' | 'bytes' | 'payloads' | 'requests'
  tone?: 'bad'
  label?: string
  loop?: boolean
  quiet?: boolean // label only on hover
}

const COLS = ['tailer', 'processor', 'strategy', 'sender', 'worker', 'destination', 'intake']
const colOf = (name: string) => (name.startsWith('destination') ? 5 : Math.max(0, COLS.indexOf(name)))
const isPipeline = (c: ComponentStat) => /^\d+$/.test(c.instance)

export const EXPLAIN: Record<string, { short: string; what: string; watch: string; metrics: string }> = {
  tailer: {
    short: 'reads files, splits lines, decodes',
    what: 'Tailers follow each file the generators write (and each container the docker launcher sees), split lines, run auto multi-line aggregation and decode into messages. Written faster than decoded means the tailer is behind, by choice (back-pressure) or by cost.',
    watch: 'logs__bytes_missed is bytes a file lost to rotation before the tailer read them: the only place in the agent where records disappear without being sent.',
    metrics: 'logs__decoded, logs__bytes_missed, logs__rotations_nix, logs__auto_multi_line_aggregator_flush',
  },
  processor: {
    short: 'rules, tags, encoding',
    what: 'One per pipeline. Applies processing rules (exclude, mask, multi-line), adds tags and encodes each message to its wire form. It stops its busy clock before writing to the strategy, so waiting on a full output channel shows as idle.',
    watch: 'High utilization means encoding and rule matching are the cost. Near 0% with items at the channel size while the strategy is saturated means blocked downstream.',
    metrics: 'logs_component_utilization{name="processor"}, logs__processed',
  },
  strategy: {
    short: 'batches and compresses',
    what: 'One per pipeline. The batch strategy groups messages into a payload until max count, max bytes or the timer, then compresses it (zstd or gzip) and hands it to the sender.',
    watch: 'Items are messages in the open batch. Busy time includes waiting to hand a payload to the sender, so a blocked sender shows up here as saturation.',
    metrics: 'logs_component_utilization{name="strategy"}, logs__batch_payload_flushed',
  },
  sender: {
    short: 'hands payloads to workers',
    what: 'Takes finished payloads from the strategies and hands them to a worker. logs_sender__send_wait is time spent waiting for a free worker.',
    watch: 'Send wait growing means every worker is busy or blocked on the destination.',
    metrics: 'logs_component_utilization{name="sender"}, logs_sender__send_wait',
  },
  worker: {
    short: 'concurrent HTTP sender',
    what: 'A concurrent HTTP sender (q0s0: queue 0, sender 0). Workers take payloads and drive the destinations.',
    watch: 'Busy workers with a slow destination is the normal shape of a throttled intake.',
    metrics: 'logs_component_utilization{name="worker"}',
  },
  destination: {
    short: 'HTTP endpoint, retries with backoff',
    what: 'The HTTP destination for the logs endpoint. Sends a payload, and on a 429, 5xx or network error keeps it and retries with exponential backoff; the reliable destination drops nothing.',
    watch: 'Items and bytes here are payloads in flight or waiting to retry: the memory the agent holds under an intake fault.',
    metrics: 'logs_component_utilization{name="destination_reliable_0"}, logs__retry_count, logs__network_errors, logs_client_http_destination__send',
  },
  intake: {
    short: 'the fake intake',
    what: 'Counts every record, applies the fault (latency, errors, drops, outage, slow read) and answers.',
    watch: 'Missing is generated minus unique received; it falls during the drain as the agent catches up, and what is left is lost.',
    metrics: 'aoc intake ledger, logs__destination_http_resp',
  },
  missed: {
    short: 'lost: rotated away unread',
    what: 'Bytes the tailer could not read before the file rotated away (logs__bytes_missed).',
    watch: 'This is lost data. It appears when the tailer is throttled by back-pressure or cannot keep up, and files rotate faster than it reads.',
    metrics: 'logs__bytes_missed, logs__rotation_size_mismatch',
  },
}
export const explainKey = (n: NodeDef) => (n.comp ? n.comp.name.replace(/_reliable_\d+$/, '') : n.id)

function build(s: IntakeSample | undefined) {
  const nodes: NodeDef[] = []
  const edges: EdgeDef[] = []
  if (!s) return { nodes, edges, rows: 1 }
  const comps = s.components
  const pipelines = [...new Set(comps.filter(isPipeline).map((c) => c.instance))].sort((a, b) => Number(a) - Number(b))
  const rows = Math.max(1, pipelines.length)
  const n = pipelines.length || 1
  const written = s.genRecordsPerSec || s.generators.reduce((a, g) => a + g.recordRate, 0)
  const ratioOf = (name: string, instance?: string) => Math.max(0, ...comps.filter((c) => c.name === name && (instance === undefined || c.instance === instance)).map((c) => c.ratio))
  const destRatio = Math.max(0, ...comps.filter((x) => x.name.startsWith('destination')).map((x) => x.ratio))
  const downstream = (c: ComponentStat): number => {
    if (c.name === 'processor') return ratioOf('strategy', c.instance)
    if (c.name === 'strategy') return Math.max(ratioOf('sender', c.instance), ratioOf('sender'), ratioOf('worker'))
    if (c.name === 'sender') return Math.max(ratioOf('worker'), destRatio)
    if (c.name === 'worker') return destRatio
    return 0
  }

  nodes.push({ id: 'tailer', kind: 'tailer', label: 'tailers · decoder', sub: `${fmtShort(written)}/s written`, detail: `${fmtShort(s.stages.decoded)}/s decoded`, col: 0, row: -1 })
  for (const c of comps) {
    const ratio = c.ratio
    const state: NodeDef['state'] | undefined = ratio >= 0.85 ? 'saturated' : ratio < 0.1 && c.items >= 50 && downstream(c) >= 0.6 ? 'blocked' : ratio >= 0.6 ? 'busy' : undefined
    nodes.push({
      id: `${c.name}/${c.instance}`,
      kind: 'component',
      label: c.name.replace('destination_reliable_0', 'destination'),
      sub: isPipeline(c) ? `p${c.instance}` : c.instance,
      detail: `${fmtShort(c.items)} items · ${fmtBytes(c.bytes).replace(' ', '')}`,
      col: colOf(c.name),
      row: isPipeline(c) ? pipelines.indexOf(c.instance) : -1,
      comp: c,
      state,
    })
  }
  nodes.push({
    id: 'intake',
    kind: 'intake',
    label: 'intake',
    sub: `${fmtShort(s.recentLogsPerSec)}/s in${s.faultsActive ? ' · fault on' : ''}`,
    detail: s.missing > 0 ? `missing ${fmtShort(s.missing)}${s.missingPerSec > 0 ? ` +${fmtShort(s.missingPerSec)}/s` : ''}` : 'nothing missing',
    col: 6,
    row: -1,
    tone: s.faultsActive ? 'bad' : undefined,
  })
  if (s.tailer.bytesMissedTotal > 0) nodes.push({ id: 'missed', kind: 'sink', label: 'missed on rotation', sub: `${fmtBytes(s.tailer.bytesMissedTotal)} total`, detail: `${fmtBytes(s.tailer.bytesMissedPerSec)}/s now`, col: 0, row: rows, tone: 'bad' })

  const by = (name: string) => nodes.filter((x) => x.comp?.name === name)
  const procs = by('processor')
  const strats = by('strategy')
  const senders = by('sender')
  const workers = by('worker')
  const dests = nodes.filter((x) => x.comp?.name.startsWith('destination'))

  if (s.tailer.bytesMissedPerSec > 0) edges.push({ from: 'tailer', to: 'missed', rate: s.tailer.bytesMissedPerSec, unit: 'bytes', tone: 'bad', label: 'lost' })
  procs.forEach((p, i) => edges.push({ from: 'tailer', to: p.id, rate: s.stages.decoded / n, unit: 'logs', quiet: i > 0, label: i === 0 ? 'decoded ÷ pipelines' : undefined }))
  if (!procs.length) edges.push({ from: 'tailer', to: dests[0]?.id ?? 'intake', rate: s.stages.sent || s.recentLogsPerSec, unit: 'logs' })
  for (const st of strats) {
    const p = procs.find((x) => x.comp!.instance === st.comp!.instance)
    if (p) edges.push({ from: p.id, to: st.id, rate: s.stages.processed / n, unit: 'logs', quiet: true })
    const next = senders.find((x) => x.comp!.instance === st.comp!.instance) ?? senders[0] ?? workers[0] ?? dests[0]
    if (next) edges.push({ from: st.id, to: next.id, rate: (s.flushes.maxCount + s.flushes.timer) / n, unit: 'payloads', quiet: true })
  }
  for (const se of senders) for (const w of workers.length ? workers : dests) edges.push({ from: se.id, to: w.id, rate: (s.flushes.maxCount + s.flushes.timer) / senders.length, unit: 'payloads', quiet: true })
  for (const w of workers) for (const d of dests) edges.push({ from: w.id, to: d.id, rate: s.sender.sendOkPerSec + s.sender.sendRetryablePerSec, unit: 'requests', quiet: true })
  const last = dests[0] ?? workers[0] ?? senders[0]
  if (last) {
    edges.push({ from: last.id, to: 'intake', rate: s.stages.sent || s.recentLogsPerSec, unit: 'logs', label: 'sent', quiet: true })
    const retry = Math.max(s.sender.retryPerSec, s.sender.sendRetryablePerSec)
    if (retry > 0.01) edges.push({ from: 'intake', to: last.id, rate: retry, unit: 'requests', tone: 'bad', label: 'retry', loop: true })
  }
  return { nodes, edges, rows }
}

const fmtRate = (e: EdgeDef) => (e.unit === 'bytes' ? `${fmtBytes(e.rate)}/s` : e.unit === 'logs' ? `${fmtShort(e.rate)}/s` : `${e.rate.toFixed(1)} ${e.unit === 'payloads' ? 'payloads' : 'req'}/s`)

// mono text is about 0.62 em per character; keep labels inside the box
const fit = (text: string, px: number, size = 10) => {
  const max = Math.floor(px / (size * 0.62))
  return text.length <= max ? text : text.slice(0, Math.max(1, max - 1)) + '…'
}

interface Tip {
  x: number
  y: number
  node: NodeDef
}

export function ComponentGraph({
  s,
  side,
  history,
  telemetry,
  selected,
  onSelect,
}: {
  s: IntakeSample | undefined
  side: 'a' | 'b'
  history: IntakeSample[]
  telemetry: TelemetrySeries[] | undefined
  selected: string | null
  onSelect: (id: string | null) => void
}) {
  const ref = useRef<HTMLDivElement>(null)
  const [W, setW] = useState(1200)
  useEffect(() => {
    const el = ref.current
    if (!el) return
    const ro = new ResizeObserver(() => setW(Math.max(960, el.clientWidth)))
    ro.observe(el)
    setW(Math.max(960, el.clientWidth))
    return () => ro.disconnect()
  }, [])
  const [hover, setHover] = useState<string | null>(null)
  const [tip, setTip] = useState<Tip | null>(null)
  const { nodes, edges, rows } = useMemo(() => build(s), [s])
  const NODE_W = Math.max(150, Math.min(200, (W - 48) / 7 - 26))
  const NODE_H = 64
  const ROW_H = 88
  const sinkRows = nodes.some((n) => n.kind === 'sink') ? 1 : 0
  const H = Math.max(2, rows + sinkRows) * ROW_H + 24
  const colX = (c: number) => 24 + c * ((W - 48 - NODE_W) / 6)
  const rowY = (r: number) => (r < 0 ? 12 + ((rows - 1) * ROW_H) / 2 : 12 + r * ROW_H)
  const pos = (n: NodeDef) => ({ x: colX(n.col), y: rowY(n.row) })
  const byId = new Map(nodes.map((n) => [n.id, n]))
  const maxRate = Math.max(1, ...edges.filter((e) => e.unit === 'logs').map((e) => e.rate))
  const sideColor = side === 'a' ? 'var(--series-a)' : 'var(--series-b)'
  const toneOf = (ratio: number, state?: NodeDef['state']) => (state === 'blocked' ? 'var(--warn)' : ratio > 0.85 ? 'var(--bad)' : ratio > 0.6 ? 'var(--warn)' : sideColor)
  const place = (ev: React.MouseEvent, n: NodeDef) => {
    const r = ref.current?.getBoundingClientRect()
    if (r) setTip({ x: ev.clientX - r.left + 14, y: ev.clientY - r.top + 14, node: n })
  }

  return (
    <div ref={ref} className="relative w-full overflow-x-auto scrollbar-thin" onMouseLeave={() => setTip(null)}>
      <svg viewBox={`0 0 ${W} ${H}`} width={W} height={H} className="block">
        <defs>
          <marker id={`arrow-${side}`} viewBox="0 0 10 10" refX="9" refY="5" markerWidth="8" markerHeight="8" markerUnits="userSpaceOnUse" orient="auto-start-reverse">
            <path d="M0,0 L10,5 L0,10 z" fill="var(--muted)" />
          </marker>
          <marker id={`arrow-bad-${side}`} viewBox="0 0 10 10" refX="9" refY="5" markerWidth="8" markerHeight="8" markerUnits="userSpaceOnUse" orient="auto-start-reverse">
            <path d="M0,0 L10,5 L0,10 z" fill="var(--bad)" />
          </marker>
        </defs>
        {edges.map((e, i) => {
          const a = byId.get(e.from)
          const b = byId.get(e.to)
          if (!a || !b) return null
          const pa = pos(a)
          const pb = pos(b)
          const x1 = pa.x + NODE_W
          const y1 = pa.y + NODE_H / 2
          const x2 = pb.x
          const y2 = pb.y + NODE_H / 2
          const vertical = a.col === b.col
          let d: string
          if (e.loop) {
            d = `M${pb.x + NODE_W},${pb.y + 10} C${(pb.x + NODE_W + pa.x) / 2},${pb.y - 40} ${(pb.x + NODE_W + pa.x) / 2},${pa.y - 40} ${pa.x + 8},${pa.y + 2}`
          } else if (vertical) {
            d = `M${pa.x + NODE_W / 2},${pa.y + NODE_H} L${pb.x + NODE_W / 2},${pb.y}`
          } else {
            const mx = (x1 + x2) / 2
            d = `M${x1},${y1} C${mx},${y1} ${mx},${y2} ${x2},${y2}`
          }
          const strength = e.unit === 'logs' ? Math.sqrt(e.rate / maxRate) : e.unit === 'bytes' ? Math.min(1, Math.sqrt(e.rate / 2e6)) : Math.min(1, e.rate / 20)
          const width = e.rate > 0 ? 1 + 4.5 * strength : 1
          const dur = e.rate > 0 ? Math.max(0.25, 2.2 - 1.9 * strength) : 0
          const color = e.tone === 'bad' ? 'var(--bad)' : sideColor
          const touched = hover === e.from || hover === e.to || selected === e.from || selected === e.to
          const dim = selected && selected !== e.from && selected !== e.to
          const showLabel = !e.quiet || touched
          const fan = e.label?.startsWith('decoded')
          const lx = e.loop ? (pb.x + NODE_W + pa.x) / 2 : vertical ? pa.x + NODE_W / 2 + 8 : fan ? pa.x : e.label === 'sent' ? x1 + 4 : (x1 + x2) / 2
          const ly = e.loop ? Math.min(pa.y, pb.y) - 34 : vertical ? (pa.y + NODE_H + pb.y) / 2 : fan ? pa.y + NODE_H + 14 : e.label === 'sent' ? y1 - 8 : (y1 + y2) / 2 - 8
          return (
            <g key={i} opacity={dim ? 0.2 : 1}>
              <path d={d} fill="none" stroke={color} strokeOpacity={0.16} strokeWidth={width + 3} />
              <path d={d} fill="none" stroke={color} strokeWidth={width} strokeLinecap="round" strokeDasharray={e.rate > 0 ? '6 9' : '2 5'} markerEnd={`url(#arrow${e.tone === 'bad' ? '-bad' : ''}-${side})`} style={dur ? { animation: `flow ${dur}s linear infinite` } : undefined} />
              {showLabel && (
                <text x={lx} y={ly} textAnchor={vertical || fan || e.label === 'sent' ? 'start' : 'middle'} fontSize={10} fontFamily="var(--font-mono)" fill={e.tone === 'bad' ? 'var(--bad)' : 'var(--muted)'} style={{ paintOrder: 'stroke', stroke: 'var(--surface)', strokeWidth: 3 }}>
                  {[e.label, fmtRate(e)].filter(Boolean).join(' ')}
                </text>
              )}
            </g>
          )
        })}
        {nodes.map((n) => {
          const p = pos(n)
          const c = n.comp
          const ratio = c?.ratio ?? 0
          const tone = toneOf(ratio, n.state)
          const isSel = selected === n.id
          const isHover = hover === n.id
          const dim = selected && !isSel && !edges.some((e) => (e.from === selected && e.to === n.id) || (e.to === selected && e.from === n.id))
          const clip = `clip-${side}-${n.id.replace(/[^a-z0-9]/gi, '_')}`
          const inner = NODE_W - 20
          return (
            <g
              key={n.id}
              transform={`translate(${p.x} ${p.y})`}
              className="cursor-pointer"
              opacity={dim ? 0.3 : 1}
              onClick={() => onSelect(isSel ? null : n.id)}
              onMouseEnter={(ev) => {
                setHover(n.id)
                place(ev, n)
              }}
              onMouseMove={(ev) => place(ev, n)}
              onMouseLeave={() => {
                setHover(null)
                setTip(null)
              }}
            >
              <clipPath id={clip}>
                <rect width={NODE_W} height={NODE_H} rx={7} />
              </clipPath>
              <rect width={NODE_W} height={NODE_H} rx={7} fill={n.tone === 'bad' ? 'rgba(229,72,77,0.10)' : 'var(--surface-2)'} stroke={isSel ? 'var(--accent)' : n.tone === 'bad' ? 'var(--bad)' : n.state === 'blocked' ? 'var(--warn)' : isHover ? 'var(--foreground)' : 'var(--border-strong)'} strokeWidth={isSel ? 2 : 1} strokeOpacity={isHover && !isSel ? 0.5 : 1} />
              {c && <rect clipPath={`url(#${clip})`} x={0} y={NODE_H - Math.max(2, Math.min(1, ratio) * NODE_H)} width={NODE_W} height={Math.max(2, Math.min(1, ratio) * NODE_H)} fill={tone} opacity={0.26} style={{ transition: 'y 600ms ease, height 600ms ease' }} />}
              {n.state === 'blocked' && <rect clipPath={`url(#${clip})`} x={0} y={0} width={NODE_W} height={NODE_H} fill="var(--warn)" opacity={0.08} />}
              <g clipPath={`url(#${clip})`}>
                <text x={10} y={18} fontSize={11.5} fontWeight={600} fontFamily="var(--font-mono)" fill="var(--foreground)">
                  {fit(n.label, inner - (c ? 40 : 0), 11.5)}
                </text>
                {c && (
                  <text x={NODE_W - 10} y={18} textAnchor="end" fontSize={11.5} fontWeight={600} fontFamily="var(--font-mono)" fill={n.state === 'blocked' ? 'var(--warn)' : ratio > 0.85 ? 'var(--bad)' : ratio > 0.6 ? 'var(--warn)' : 'var(--foreground)'}>
                    {Math.round(ratio * 100)}%
                  </text>
                )}
                <text x={10} y={34} fontSize={10} fontFamily="var(--font-mono)" fill={n.tone === 'bad' ? 'var(--bad)' : n.state === 'blocked' ? 'var(--warn)' : 'var(--muted)'}>
                  {fit(n.state === 'blocked' ? `${n.sub} · blocked` : n.state === 'saturated' ? `${n.sub} · saturated` : n.state === 'busy' ? `${n.sub} · busy` : n.sub, inner)}
                </text>
                <text x={10} y={50} fontSize={10} fontFamily="var(--font-mono)" fill={n.kind === 'sink' || (n.kind === 'intake' && s && s.missing > 0) ? 'var(--bad)' : 'var(--foreground)'} fillOpacity={0.85}>
                  {fit(n.detail, inner)}
                </text>
              </g>
            </g>
          )
        })}
        {!s && (
          <text x={W / 2} y={H / 2} textAnchor="middle" fontSize={12} fill="var(--faint)" fontFamily="var(--font-mono)">
            waiting for the intake
          </text>
        )}
        {s && !s.telemetryOk && (
          <text x={W / 2} y={H - 6} textAnchor="middle" fontSize={11} fill="var(--warn)" fontFamily="var(--font-mono)">
            the agent's telemetry is not reachable yet: components appear once it is scraped
          </text>
        )}
      </svg>
      {tip && <NodeTip tip={tip} s={s} containerWidth={ref.current?.clientWidth ?? W} />}
      {selected && <Inspector id={selected} node={byId.get(selected)} history={history} telemetry={telemetry} side={side} onClose={() => onSelect(null)} />}
    </div>
  )
}

function NodeTip({ tip, s, containerWidth }: { tip: Tip; s: IntakeSample | undefined; containerWidth: number }) {
  const n = tip.node
  const ex = EXPLAIN[explainKey(n)]
  const c = n.comp
  const left = Math.max(0, Math.min(tip.x, containerWidth - 340))
  const rows: [string, string][] = []
  if (c) rows.push(['busy (EWMA ~15 s)', `${(c.ratio * 100).toFixed(1)}%`], ['items held', fmtShort(c.items)], ['bytes held', fmtBytes(c.bytes)])
  if (n.id === 'tailer' && s) rows.push(['decoded', `${fmtShort(s.stages.decoded)}/s`], ['rotations', `${s.rotationsPerSec.toFixed(1)}/s`], ['multi-line flushes', `${s.tailer.multilineFlushPerSec.toFixed(0)}/s`], ['bytes missed', `${fmtBytes(s.tailer.bytesMissedPerSec)}/s`])
  if (n.id === 'intake' && s) rows.push(['received', `${fmtShort(s.recentLogsPerSec)}/s`], ['missing', fmtShort(s.missing)], ['rejected this second', String(s.err4xx + s.err5xx)], ['e2e p99', s.e2eP99 ? `${s.e2eP99.toFixed(2)} s` : '–'])
  if (n.id === 'missed' && s) rows.push(['missed total', fmtBytes(s.tailer.bytesMissedTotal)], ['missed now', `${fmtBytes(s.tailer.bytesMissedPerSec)}/s`])
  if (c?.name.startsWith('destination') && s) rows.push(['retries', `${s.sender.retryPerSec.toFixed(1)}/s`], ['network errors', `${s.sender.networkErrorsPerSec.toFixed(1)}/s`])
  if (c?.name === 'sender' && s) rows.push(['send wait', `${(s.sender.sendWaitMsPerSec / 1000).toFixed(2)} s/s`])
  return (
    <div className="absolute z-20 w-[320px] rounded-md bg-surface-2 hairline shadow-2xl p-3 pointer-events-none" style={{ left, top: tip.y }}>
      <div className="flex items-baseline gap-2">
        <span className="font-mono text-xs font-semibold">{n.label}</span>
        <span className="font-mono text-[10px] text-muted">{n.sub}</span>
        {n.state && <span className={cn('ml-auto font-mono text-[10px] uppercase', n.state === 'saturated' ? 'text-bad' : 'text-warn')}>{n.state}</span>}
      </div>
      {ex && <p className="text-[11px] text-muted mt-1 leading-4">{ex.short}</p>}
      {rows.length > 0 && (
        <table className="w-full mt-2 text-[11px] font-mono">
          <tbody>
            {rows.map(([k, v]) => (
              <tr key={k}>
                <td className="text-muted py-0.5">{k}</td>
                <td className="text-right num">{v}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {n.state === 'blocked' && <p className="text-[11px] text-warn mt-2 leading-4">Busy time stops while this component waits to hand off, so 0% with a full buffer means it is waiting on the next stage, not idle.</p>}
      <p className="text-[10px] text-faint mt-2">click for history and raw series</p>
    </div>
  )
}

function seriesFor(id: string, node: NodeDef | undefined, telemetry: TelemetrySeries[] | undefined): TelemetrySeries[] {
  if (!telemetry) return []
  if (node?.comp) {
    const { name, instance } = node.comp
    const own = telemetry.filter((m) => m.name.startsWith('logs_component_utilization') && m.labels.includes(`name="${name}"`) && m.labels.includes(`instance="${instance}"`))
    const extra =
      name === 'processor'
        ? telemetry.filter((m) => m.name === 'logs__processed')
        : name === 'strategy'
          ? telemetry.filter((m) => m.name === 'logs__batch_payload_flushed')
          : name === 'sender'
            ? telemetry.filter((m) => m.name === 'logs_sender__send_wait')
            : name.startsWith('destination')
              ? telemetry.filter((m) => /^logs__(retry_count|network_errors|sent)$|^logs_client_http_destination__(send|in_use_ms|idle_ms)$/.test(m.name))
              : []
    return [...own, ...extra]
  }
  const pick = (re: RegExp) => telemetry.filter((m) => re.test(m.name))
  switch (id) {
    case 'tailer':
      return pick(/^logs__(decoded|bytes_missed|rotations|rotation_size|auto_multi_line|log_line_sizes_(count|sum)|truncated|open_files)/)
    case 'missed':
      return pick(/^logs__(bytes_missed|rotations|rotation_size)/)
    case 'intake':
      return pick(/^logs__(destination_http_resp|retry_count|network_errors|sent|http_connectivity)/)
  }
  return []
}

function Inspector({ id, node, history, telemetry, side, onClose }: { id: string; node?: NodeDef; history: IntakeSample[]; telemetry?: TelemetrySeries[]; side: 'a' | 'b'; onClose: () => void }) {
  const ex = node ? EXPLAIN[explainKey(node)] : undefined
  const series = seriesFor(id, node, telemetry)
  const comp = node?.comp
  const hist = comp ? history.map((h) => h.components.find((c) => c.name === comp.name && c.instance === comp.instance)) : []
  const spark = (vals: number[], label: string, fmt: (v: number) => string) => {
    const v = vals.slice(-300)
    const max = Math.max(1e-9, ...v)
    const w = 240
    const h = 36
    const d = v.map((x, i) => `${i ? 'L' : 'M'}${((i / Math.max(1, v.length - 1)) * w).toFixed(1)},${(h - (x / max) * h).toFixed(1)}`).join(' ')
    return (
      <div key={label} className="flex items-center gap-3">
        <div className="w-20 eyebrow !text-[9.5px]">{label}</div>
        <svg width={w} height={h} className="shrink-0">
          <path d={d} fill="none" stroke={side === 'a' ? 'var(--series-a)' : 'var(--series-b)'} strokeWidth={1.5} />
        </svg>
        <div className="num text-xs w-20 text-right">{fmt(v.at(-1) ?? 0)}</div>
        <div className="num text-[10px] text-muted">max {fmt(max)}</div>
      </div>
    )
  }
  return (
    <div className="mt-3 rounded-md bg-background/60 hairline p-4 grid lg:grid-cols-[1fr_440px] gap-4">
      <div className="space-y-2 min-w-0">
        <div className="flex items-center gap-2">
          <span className="font-mono text-sm font-semibold">{node?.label ?? id}</span>
          {node?.sub && <span className="font-mono text-[11px] text-muted">{node.sub}</span>}
          {node?.state && <span className={cn('font-mono text-[10px] uppercase', node.state === 'saturated' ? 'text-bad' : 'text-warn')}>{node.state}</span>}
          <button className="ml-auto text-[11px] text-faint hover:text-foreground cursor-pointer" onClick={onClose}>
            close
          </button>
        </div>
        {ex && (
          <>
            <p className="text-xs leading-5 text-foreground/90">{ex.what}</p>
            <p className="text-xs leading-5 text-muted">
              <span className="text-foreground/80">What to watch: </span>
              {ex.watch}
            </p>
            <p className="text-[11px] font-mono text-faint">{ex.metrics}</p>
          </>
        )}
        {comp && (
          <div className="space-y-1 pt-1">
            {spark(hist.map((c) => c?.ratio ?? 0), 'busy', (v) => `${(v * 100).toFixed(0)}%`)}
            {spark(hist.map((c) => c?.items ?? 0), 'items', fmtShort)}
            {spark(hist.map((c) => c?.bytes ?? 0), 'bytes', fmtBytes)}
          </div>
        )}
        {(id === 'tailer' || id === 'missed') && history.length > 0 && (
          <div className="space-y-1 pt-1">
            {spark(history.map((h) => h.stages.decoded), 'decoded /s', fmtShort)}
            {spark(history.map((h) => h.genRecordsPerSec), 'written /s', fmtShort)}
            {spark(history.map((h) => h.tailer.bytesMissedPerSec), 'missed B/s', fmtBytes)}
          </div>
        )}
        {id === 'intake' && history.length > 0 && (
          <div className="space-y-1 pt-1">
            {spark(history.map((h) => h.recentLogsPerSec), 'received /s', fmtShort)}
            {spark(history.map((h) => h.missing), 'missing', fmtShort)}
            {spark(history.map((h) => h.e2eP99), 'e2e p99 s', (v) => v.toFixed(1))}
          </div>
        )}
      </div>
      <div className="min-w-0">
        <div className="eyebrow mb-1">raw series, last scrape</div>
        {series.length === 0 ? (
          <p className="text-[11px] text-faint">No series are attributed to this node.</p>
        ) : (
          <div className="max-h-64 overflow-auto scrollbar-thin rounded-sm hairline">
            <table className="w-full text-[10.5px] font-mono">
              <tbody>
                {series.map((m, i) => (
                  <tr key={i} className="border-b border-border last:border-0">
                    <td className="px-2 py-1 text-muted truncate max-w-[280px]" title={`${m.name}${m.labels}`}>
                      {m.name.replace(/^logs_component_utilization__/, '… ').replace(/^logs__/, '')}
                      <span className="text-faint"> {m.labels.replace(/url="[^"]*"/, '').replace(/[{}]/g, '').replace(/,$/, '')}</span>
                    </td>
                    <td className="px-2 py-1 text-right num">{fmtShort(m.value)}</td>
                    <td className="px-2 py-1 text-right num text-muted w-16">{m.type === 'counter' ? `${fmtShort(m.rate)}/s` : ''}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </div>
  )
}
