import type { IntakeSample } from '@shared/types'
import { cn, fmtBytes, fmtShort } from '@/lib/utils'

// A 270° arc gauge. Value text wears ink, the arc wears the side's color and
// turns warn/bad past the thresholds, which are stated with each gauge.
export function Gauge({
  label,
  value,
  max,
  text,
  warn,
  bad,
  side,
  hint,
}: {
  label: string
  value: number | undefined
  max: number
  text: string
  warn?: number
  bad?: number
  side: 'a' | 'b'
  hint?: string
}) {
  const r = 26
  const c = 2 * Math.PI * r
  const span = 0.75
  const frac = value === undefined ? 0 : Math.max(0, Math.min(1, value / max))
  const tone = value === undefined ? 'var(--faint)' : bad !== undefined && value >= bad ? 'var(--bad)' : warn !== undefined && value >= warn ? 'var(--warn)' : side === 'a' ? 'var(--series-a)' : 'var(--series-b)'
  return (
    <div className="flex flex-col items-center gap-1 min-w-[88px]" title={hint}>
      <svg width="72" height="62" viewBox="0 0 72 62">
        <g transform="translate(36 36) rotate(135)">
          <circle r={r} fill="none" stroke="var(--border-strong)" strokeWidth={5} strokeDasharray={`${c * span} ${c}`} strokeLinecap="round" />
          <circle r={r} fill="none" stroke={tone} strokeWidth={5} strokeDasharray={`${c * span * frac} ${c}`} strokeLinecap="round" style={{ transition: 'stroke-dasharray 600ms ease, stroke 300ms' }} />
        </g>
        <text x="36" y="40" textAnchor="middle" fontSize="12" fontWeight={600} fill="var(--foreground)" fontFamily="var(--font-mono)">
          {text}
        </text>
      </svg>
      <div className="eyebrow !text-[9.5px] text-center leading-3">{label}</div>
    </div>
  )
}

export function GaugeRow({ s, side }: { s: IntakeSample | undefined; side: 'a' | 'b' }) {
  const busiest = s?.telemetryOk ? Object.entries(s.utilization).sort((x, y) => y[1] - x[1])[0] : undefined
  const ratio = s && s.wireBytesPerSec > 0 ? s.rawBytesPerSec / s.wireBytesPerSec : undefined
  const rejected = s ? s.err4xx + s.err5xx + s.dropped : 0
  const reqs = s ? s.requestsPerSec + rejected : 0
  const errPct = reqs > 0 ? (rejected / reqs) * 100 : 0
  return (
    <div className={cn('grid grid-cols-4 md:grid-cols-8 gap-y-3 gap-x-1 rounded-md p-3 bg-background/40 hairline')}>
      <Gauge side={side} label={busiest ? `busiest: ${busiest[0]}` : 'busiest component'} value={busiest?.[1]} max={1} text={busiest ? `${Math.round(busiest[1] * 100)}%` : '–'} warn={0.6} bad={0.85} hint="logs_component_utilization ratio; the pipeline is as busy as its busiest component" />
      <Gauge side={side} label="core agent cpu" value={s?.agentCpuPct} max={100} text={s?.agentCpuPct != null ? `${s.agentCpuPct.toFixed(0)}%` : '–'} warn={60} bad={90} hint="of one core" />
      <Gauge side={side} label="core agent rss" value={s?.agentRssBytes} max={1 << 30} text={s?.agentRssBytes ? fmtBytes(s.agentRssBytes).replace(' MiB', 'M').replace(' GiB', 'G') : '–'} warn={600 << 20} bad={900 << 20} hint="gauge spans 1 GiB" />
      <Gauge side={side} label="e2e p99" value={s?.e2eP99} max={5} text={s && s.e2eP99 > 0 ? `${s.e2eP99 < 10 ? s.e2eP99.toFixed(1) : s.e2eP99.toFixed(0)}s` : '–'} warn={2} bad={5} hint="written → received, 99th percentile this second; gauge spans 5 s" />
      <Gauge side={side} label="backlog" value={s ? Math.log10(s.backlog + 1) : undefined} max={6} text={s ? fmtShort(s.backlog) : '–'} warn={Math.log10(5000)} bad={Math.log10(50000)} hint="decoded but not yet sent since the window opened; log scale to 1M" />
      <Gauge side={side} label="rejected" value={errPct} max={100} text={s ? `${errPct.toFixed(0)}%` : '–'} warn={1} bad={25} hint="requests the intake refused or dropped this second" />
      <Gauge side={side} label="goroutines" value={s?.goroutines} max={3000} text={s?.goroutines ? fmtShort(s.goroutines) : '–'} warn={1500} bad={2500} hint="go_goroutines; gauge spans 3000" />
      <Gauge side={side} label="compression" value={ratio} max={30} text={ratio ? `${ratio.toFixed(0)}×` : '–'} hint="raw bytes ÷ wire bytes this second" />
    </div>
  )
}
