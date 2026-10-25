import { useEffect, useMemo, useRef, useState } from 'react'
import type { IntakeSample } from '@shared/types'
import { fmtShort } from '@/lib/utils'

export type Pick = (s: IntakeSample) => number

// One axis, series a and b (solid) with an optional second measure per side
// (dashed), a crosshair tooltip, fault spans shaded. Width follows the
// container so text stays its natural size.
export function RateChart({
  a,
  b,
  pick,
  pick2,
  label,
  label2,
  fmt = fmtShort,
  windowSec = 240,
  height = 160,
  bars = false,
  range,
  cursor,
  onPick,
}: {
  a: IntakeSample[]
  b: IntakeSample[]
  pick: Pick
  pick2?: Pick
  label: string
  label2?: string
  fmt?: (v: number) => string
  windowSec?: number
  height?: number
  bars?: boolean
  range?: [number, number] // fixed time axis (review mode) instead of the trailing window
  cursor?: number // a vertical marker at this time
  onPick?: (t: number) => void // click: move the cursor here
}) {
  const ref = useRef<HTMLDivElement>(null)
  const [hover, setHover] = useState<number | null>(null)
  const [W, setW] = useState(800)
  useEffect(() => {
    const el = ref.current
    if (!el) return
    const ro = new ResizeObserver(() => setW(Math.max(240, el.clientWidth)))
    ro.observe(el)
    setW(Math.max(240, el.clientWidth))
    return () => ro.disconnect()
  }, [])
  const H = height
  const PAD = { l: 48, r: 12, t: 16, b: 18 }

  const m = useMemo(() => {
    const now = range ? range[1] : Math.max(a.at(-1)?.t ?? 0, b.at(-1)?.t ?? 0, Date.now() - 1000)
    const t0 = range ? range[0] : now - windowSec * 1000
    const sa = a.filter((s) => s.t >= t0)
    const sb = b.filter((s) => s.t >= t0)
    const all = [...sa, ...sb]
    const max = Math.max(1e-9, ...all.map(pick), ...(pick2 ? all.map(pick2) : [0])) * 1.12
    const x = (t: number) => PAD.l + ((t - t0) / (now - t0)) * (W - PAD.l - PAD.r)
    const y = (v: number) => PAD.t + (1 - v / max) * (H - PAD.t - PAD.b)
    const path = (s: IntakeSample[], p: Pick) => s.map((q, i) => `${i ? 'L' : 'M'}${x(q.t).toFixed(1)},${y(p(q)).toFixed(1)}`).join(' ')
    const faults: [number, number][] = []
    let open: number | null = null
    for (const s of all.sort((p, q) => p.t - q.t)) {
      if (s.faultsActive && open === null) open = s.t
      if (!s.faultsActive && open !== null) {
        faults.push([open, s.t])
        open = null
      }
    }
    if (open !== null) faults.push([open, now])
    return { now, t0, max, sa, sb, x, y, path, faults }
  }, [a, b, windowSec, pick, pick2, H, W, range])

  const { now, t0, max, sa, sb, x, y, path, faults } = m
  const onMove = (e: React.MouseEvent) => {
    const r = ref.current?.getBoundingClientRect()
    if (!r) return
    const px = e.clientX - r.left
    setHover(t0 + ((px - PAD.l) / (W - PAD.l - PAD.r)) * (now - t0))
  }
  const nearest = (pts: IntakeSample[], t: number) => {
    let best: IntakeSample | undefined
    for (const p of pts) if (!best || Math.abs(p.t - t) < Math.abs(best.t - t)) best = p
    return best
  }
  const ha = hover != null ? nearest(sa, hover) : undefined
  const hb = hover != null ? nearest(sb, hover) : undefined
  const ticks = [0, 0.25, 0.5, 0.75].map((f) => f * max)
  const empty = sa.length === 0 && sb.length === 0
  const barW = Math.max(1, ((W - PAD.l - PAD.r) / windowSec) * 0.8)

  return (
    <div ref={ref} className={onPick ? "relative w-full select-none cursor-crosshair" : "relative w-full select-none"} onMouseMove={onMove} onMouseLeave={() => setHover(null)} onClick={() => hover != null && onPick?.(Math.round(hover / 1000) * 1000)}>
      <svg viewBox={`0 0 ${W} ${H}`} width={W} height={H} className="block max-w-full">
        {faults.map(([s, e], i) => (
          <rect key={i} x={x(s)} y={PAD.t} width={Math.max(1, x(e) - x(s))} height={H - PAD.t - PAD.b} fill="var(--bad)" opacity={0.1} />
        ))}
        {ticks.map((v) => (
          <g key={v}>
            <line x1={PAD.l} x2={W - PAD.r} y1={y(v)} y2={y(v)} stroke="var(--border)" strokeWidth={1} />
            <text x={PAD.l - 6} y={y(v) + 3} textAnchor="end" fontSize={10} fill="var(--muted)" fontFamily="var(--font-mono)">
              {fmt(v)}
            </text>
          </g>
        ))}
        {[0, 0.5, 1].map((f) => (
          <text key={f} x={x(t0 + f * (now - t0))} y={H - 5} textAnchor={f === 0 ? 'start' : f === 1 ? 'end' : 'middle'} fontSize={10} fill="var(--muted)" fontFamily="var(--font-mono)">
            {range ? new Date(t0 + f * (now - t0)).toLocaleTimeString() : f === 1 ? 'now' : `-${Math.round(((1 - f) * windowSec) / 60)}m`}
          </text>
        ))}
        {cursor !== undefined && cursor >= t0 && cursor <= now && <line x1={x(cursor)} x2={x(cursor)} y1={PAD.t} y2={H - PAD.b} stroke="var(--accent)" strokeWidth={1.25} />}
        {bars ? (
          <>
            {sa.map((s) => (pick(s) > 0 ? <rect key={s.t} x={x(s.t) - barW / 2} y={y(pick(s))} width={barW} height={y(0) - y(pick(s))} fill="var(--series-a)" opacity={0.85} /> : null))}
            {sb.map((s) => (pick(s) > 0 ? <rect key={s.t} x={x(s.t)} y={y(pick(s))} width={barW} height={y(0) - y(pick(s))} fill="var(--series-b)" opacity={0.85} /> : null))}
          </>
        ) : (
          <>
            {pick2 && <path d={path(sa, pick2)} fill="none" stroke="var(--series-a)" strokeWidth={1.25} strokeDasharray="3 3" opacity={0.8} />}
            {pick2 && <path d={path(sb, pick2)} fill="none" stroke="var(--series-b)" strokeWidth={1.25} strokeDasharray="3 3" opacity={0.8} />}
            <path d={path(sa, pick)} fill="none" stroke="var(--series-a)" strokeWidth={2} strokeLinejoin="round" strokeLinecap="round" />
            <path d={path(sb, pick)} fill="none" stroke="var(--series-b)" strokeWidth={2} strokeLinejoin="round" strokeLinecap="round" />
            {sa.at(-1) && <circle cx={x(sa.at(-1)!.t)} cy={y(pick(sa.at(-1)!))} r={3.5} fill="var(--series-a)" stroke="var(--surface)" strokeWidth={2} />}
            {sb.at(-1) && <circle cx={x(sb.at(-1)!.t)} cy={y(pick(sb.at(-1)!))} r={3.5} fill="var(--series-b)" stroke="var(--surface)" strokeWidth={2} />}
          </>
        )}
        {hover != null && !empty && <line x1={x(hover)} x2={x(hover)} y1={PAD.t} y2={H - PAD.b} stroke="var(--border-strong)" strokeDasharray="3 3" />}
        {empty && (
          <text x={W / 2} y={H / 2} textAnchor="middle" fontSize={12} fill="var(--faint)" fontFamily="var(--font-mono)">
            waiting for the intake
          </text>
        )}
      </svg>
      <div className="absolute left-12 top-0 eyebrow">
        {label}
        {pick2 && label2 && <span className="text-faint normal-case tracking-normal"> · dashed {label2}</span>}
      </div>
      {hover != null && (ha || hb) && (
        <div className="absolute top-2 right-3 rounded-md bg-surface-2 hairline px-2.5 py-1.5 text-xs font-mono shadow-xl pointer-events-none">
          <div className="text-muted">{new Date((ha ?? hb)!.t).toLocaleTimeString()}</div>
          {ha && (
            <div className="flex items-center gap-2">
              <span className="inline-block size-2 rounded-full" style={{ background: 'var(--series-a)' }} />a <span className="ml-auto">{fmt(pick(ha))}</span>
              {pick2 && <span className="text-muted">/ {fmt(pick2(ha))}</span>}
            </div>
          )}
          {hb && (
            <div className="flex items-center gap-2">
              <span className="inline-block size-2 rounded-full" style={{ background: 'var(--series-b)' }} />b <span className="ml-auto">{fmt(pick(hb))}</span>
              {pick2 && <span className="text-muted">/ {fmt(pick2(hb))}</span>}
            </div>
          )}
        </div>
      )}
    </div>
  )
}
