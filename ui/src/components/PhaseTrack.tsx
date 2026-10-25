import type { Phase, RunRecord } from '@shared/types'
import { cn, fmtDur } from '@/lib/utils'
import { useEffect, useState } from 'react'

const STEPS: { key: Phase; label: string }[] = [
  { key: 'building', label: 'build' },
  { key: 'up', label: 'stack up' },
  { key: 'warmup', label: 'warm-up' },
  { key: 'window', label: 'window' },
  { key: 'draining', label: 'drain' },
  { key: 'collecting', label: 'collect' },
]
const ORDER: Phase[] = ['queued', 'starting', 'building', 'up', 'warmup', 'window', 'draining', 'collecting', 'finished']

export function useNow(ms = 1000) {
  const [now, setNow] = useState(Date.now())
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), ms)
    return () => clearInterval(t)
  }, [ms])
  return now
}

export function PhaseTrack({ run }: { run: RunRecord }) {
  const now = useNow()
  const idx = ORDER.indexOf(run.phase)
  const done = run.phase === 'finished' || run.phase === 'failed' || run.phase === 'interrupted'
  const elapsed = run.startedAt ? ((run.endedAt ? new Date(run.endedAt).getTime() : now) - new Date(run.startedAt).getTime()) / 1000 : 0
  const est = run.wallEstimateSec
  const pct = est ? Math.min(100, (elapsed / est) * 100) : undefined
  const inPhase = (new Date(run.phaseSince).getTime() < now ? now - new Date(run.phaseSince).getTime() : 0) / 1000

  return (
    <div className="space-y-3">
      <div className="grid grid-cols-6 gap-1">
        {STEPS.map((s) => {
          const si = ORDER.indexOf(s.key)
          const state = done && run.phase !== 'finished' ? (si < idx ? 'done' : 'off') : si < idx || run.phase === 'finished' ? 'done' : si === idx ? 'now' : 'off'
          return (
            <div key={s.key} className="space-y-1.5">
              <div
                className={cn(
                  'h-1.5 rounded-full transition-colors',
                  state === 'done' && 'bg-foreground/70',
                  state === 'now' && 'bg-accent',
                  state === 'off' && 'bg-border-strong',
                )}
              />
              <div className={cn('eyebrow !text-[10px] truncate', state === 'now' && '!text-accent', state === 'off' && '!text-faint')}>
                {s.label}
                {state === 'now' && !done && <span className="ml-1 normal-case tracking-normal">{fmtDur(inPhase)}</span>}
              </div>
            </div>
          )
        })}
      </div>
      <div className="flex items-baseline justify-between font-mono text-xs">
        <span className="text-muted">
          elapsed <span className="text-foreground">{fmtDur(elapsed)}</span>
          {est ? (
            <>
              {' '}
              of about <span className="text-foreground">{fmtDur(est)}</span>
            </>
          ) : null}
        </span>
        {run.rounds && run.rounds > 1 && (
          <span className="text-muted">
            round <span className="text-foreground">{run.round ?? 1}</span>/{run.rounds}
          </span>
        )}
      </div>
      {pct !== undefined && (
        <div className="h-px w-full bg-border-strong relative overflow-visible">
          <div className="absolute left-0 top-0 h-px bg-accent transition-[width] duration-1000" style={{ width: `${pct}%` }} />
          <div className="absolute top-1/2 -translate-y-1/2 size-2 rounded-full bg-accent transition-[left] duration-1000" style={{ left: `calc(${pct}% - 4px)` }} />
        </div>
      )}
    </div>
  )
}
