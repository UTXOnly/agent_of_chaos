import { Pause, Play, Radio, Zap } from 'lucide-react'
import { useEffect, useRef } from 'react'
import { Button } from '@/components/ui/button'
import { cn, fmtDur } from '@/lib/utils'

// A time cursor over a run's samples. Following "live" pins it to the newest
// sample; dragging detaches it. Play replays from the cursor at 1 s per
// second.
export function Scrubber({
  t0,
  t1,
  value,
  live,
  playing,
  canFollow,
  peak,
  faultSpans,
  windowStart,
  onChange,
  onFollow,
  onPlay,
}: {
  t0: number
  t1: number
  value: number
  live: boolean
  playing: boolean
  canFollow: boolean
  peak?: number
  faultSpans: [number, number][]
  windowStart?: number
  onChange: (t: number) => void
  onFollow: () => void
  onPlay: (p: boolean) => void
}) {
  const span = Math.max(1, t1 - t0)
  const pct = (t: number) => `${Math.max(0, Math.min(100, ((t - t0) / span) * 100))}%`
  const timer = useRef<number>(undefined)
  useEffect(() => {
    if (!playing) return
    timer.current = window.setInterval(() => {
      const next = value + 1000
      if (next >= t1) {
        onPlay(false)
        onChange(t1)
      } else onChange(next)
    }, 1000)
    return () => clearInterval(timer.current)
  }, [playing, value, t1, onChange, onPlay])
  const rel = windowStart ? value - windowStart : value - t0
  return (
    <div className="flex items-center gap-3">
      <Button size="sm" variant={live ? 'default' : 'outline'} onClick={onFollow} disabled={!canFollow} title="follow the newest sample">
        <Radio className="size-3" /> live
      </Button>
      <Button size="sm" variant="outline" onClick={() => onPlay(!playing)} disabled={live} title="replay from the cursor">
        {playing ? <Pause className="size-3" /> : <Play className="size-3" />}
      </Button>
      {peak !== undefined && (
        <Button size="sm" variant="outline" onClick={() => onChange(peak)} title="jump to the most stressed second of the run">
          <Zap className="size-3" /> peak stress
        </Button>
      )}
      <div className="relative flex-1 h-8">
        <div className="absolute inset-x-0 top-1/2 -translate-y-1/2 h-1.5 rounded-full bg-border-strong overflow-hidden">
          {faultSpans.map(([a, b], i) => (
            <div key={i} className="absolute top-0 bottom-0 bg-bad/40" style={{ left: pct(a), width: `calc(${pct(b)} - ${pct(a)})` }} />
          ))}
          {windowStart && <div className="absolute top-0 bottom-0 w-px bg-foreground/60" style={{ left: pct(windowStart) }} title="window open" />}
          {peak !== undefined && <div className="absolute top-0 bottom-0 w-0.5 bg-warn" style={{ left: pct(peak) }} />}
        </div>
        <input
          type="range"
          min={t0}
          max={t1}
          step={1000}
          value={value}
          onChange={(e) => onChange(Number(e.target.value))}
          className="absolute inset-0 w-full appearance-none bg-transparent cursor-pointer [&::-webkit-slider-thumb]:appearance-none [&::-webkit-slider-thumb]:size-3.5 [&::-webkit-slider-thumb]:rounded-full [&::-webkit-slider-thumb]:bg-accent [&::-webkit-slider-thumb]:shadow [&::-moz-range-thumb]:size-3.5 [&::-moz-range-thumb]:rounded-full [&::-moz-range-thumb]:bg-accent [&::-moz-range-thumb]:border-0"
          aria-label="time cursor"
        />
      </div>
      <div className={cn('num text-xs w-40 text-right', live && 'text-accent')}>
        {new Date(value).toLocaleTimeString()} <span className="text-muted">{windowStart ? (rel >= 0 ? `t+${fmtDur(rel / 1000)}` : `−${fmtDur(-rel / 1000)}`) : fmtDur(rel / 1000)}</span>
      </div>
    </div>
  )
}
