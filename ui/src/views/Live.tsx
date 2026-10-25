import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { AlertTriangle, Ban, CheckCircle2, Clock, FileText, Octagon, Square, XCircle } from 'lucide-react'
import type { IntakeSample, RunRecord, RunSamples, TelemetrySeries } from '@shared/types'
import { api, isActive, type Live as LiveState } from '@/lib/api'
import { at, peakTime, smooth, upTo } from '@/lib/steady'
import { cn, fmtBytes, fmtDur, fmtInt, fmtShort, ago } from '@/lib/utils'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import { ComponentGraph } from '@/components/ComponentGraph'
import { GaugeRow } from '@/components/Gauges'
import { Diagnosis } from '@/components/Diagnosis'
import { PhaseTrack } from '@/components/PhaseTrack'
import { RateChart } from '@/components/RateChart'
import { Scrubber } from '@/components/Scrubber'
import { ChevronDown, ChevronUp } from 'lucide-react'

const EMPTY: RunSamples = { samples: { a: [], b: [] }, telemetry: {} }

export function Live({ live, selected, onSelect }: { live: LiveState; selected?: string; onSelect: (id: string) => void }) {
  const active = live.runs.find(isActive)
  const run = live.runs.find((r) => r.id === selected) ?? active ?? [...live.runs].reverse()[0]
  const queued = live.runs.filter((r) => r.phase === 'queued')
  const history = [...live.runs].filter((r) => !isActive(r) && r.phase !== 'queued').reverse()
  const isLiveRun = !!run && isActive(run) && run === active

  // a finished run's samples come from the server's store, once
  const [stored, setStored] = useState<{ id: string; data: RunSamples | null }>()
  useEffect(() => {
    if (!run || isLiveRun) return
    if (stored?.id === run.id) return
    let gone = false
    api
      .samples(run.id)
      .then((d) => !gone && setStored({ id: run.id, data: d }))
      .catch(() => !gone && setStored({ id: run.id, data: null }))
    return () => {
      gone = true
    }
  }, [run?.id, isLiveRun, run?.phase])
  const data: RunSamples = isLiveRun ? { samples: live.samples, telemetry: live.telemetry } : stored && run && stored.id === run.id && stored.data ? stored.data : EMPTY
  const all = useMemo(() => [...data.samples.a, ...data.samples.b].sort((x, y) => x.t - y.t), [data])
  const hasData = all.length > 0
  const t0 = all[0]?.t ?? 0
  const t1 = all.at(-1)?.t ?? 0
  const peak = useMemo(() => peakTime(all), [all])

  // the time cursor: follows the newest sample while live, else sits where
  // the user put it (starting at the most stressed second)
  const [follow, setFollow] = useState(true)
  const [cursor, setCursor] = useState<number>()
  const [playing, setPlaying] = useState(false)
  const lastRun = useRef<string>(undefined)
  useEffect(() => {
    if (!run || lastRun.current === run.id) return
    lastRun.current = run.id
    setFollow(isLiveRun)
    setCursor(undefined)
    setPlaying(false)
  }, [run?.id, isLiveRun])
  useEffect(() => {
    if (!isLiveRun && follow) setFollow(false)
  }, [isLiveRun, follow])
  useEffect(() => {
    if (!follow && cursor === undefined && hasData) setCursor(peak ?? t1)
  }, [follow, cursor, hasData, peak, t1])
  const onCursor = useCallback((t: number) => {
    setFollow(false)
    setCursor(t)
  }, [])
  // live values step every 2 s instead of every sample
  const tView = follow ? Math.floor(t1 / 2000) * 2000 : Math.min(t1, Math.max(t0, cursor ?? t1))
  const review = !follow

  const faultSpans = useMemo(() => {
    const spans: [number, number][] = []
    let open: number | null = null
    for (const s of all) {
      if (s.faultsActive && open === null) open = s.t
      if (!s.faultsActive && open !== null) {
        spans.push([open, s.t])
        open = null
      }
    }
    if (open !== null) spans.push([open, t1])
    return spans
  }, [all, t1])
  const windowStart = useMemo(() => all.find((s) => s.windowSeconds > 0)?.t, [all])

  if (!run) {
    return (
      <div className="rounded-lg hairline p-12 text-center space-y-2">
        <div className="eyebrow">nothing running</div>
        <p className="text-sm text-muted">Compose a run, or start one from a shell: the UI watches the intake either way once a run is launched here.</p>
      </div>
    )
  }

  const isAb = run.request.kind === 'ab'
  const win = Math.max(120, Math.min(900, (run.windowSec ?? 180) + 120))
  const range: [number, number] | undefined = review || !isLiveRun ? [t0, t1] : undefined
  const histA = upTo(data.samples.a, tView)
  const histB = upTo(data.samples.b, tView)
  const sA = smooth(data.samples.a, tView)
  const sB = smooth(data.samples.b, tView)

  return (
    <div className="space-y-5">
      <RunHeader run={run} />
      <Card>
        <CardContent className="pt-5 space-y-4">
          <PhaseTrack run={run} />
          {hasData && (
            <Scrubber t0={t0} t1={t1} value={tView} live={follow} playing={playing} canFollow={isLiveRun} peak={peak} faultSpans={faultSpans} windowStart={windowStart} onChange={onCursor} onFollow={() => setFollow(true)} onPlay={setPlaying} />
          )}
          {!hasData && !isLiveRun && <p className="text-xs text-faint">No samples were kept for this run{stored?.id === run.id && stored.data === null ? ' (it ran before samples were stored, or never reached the intake)' : ''}.</p>}
        </CardContent>
      </Card>

      {hasData && (
        <Card className="overflow-hidden relative">
          <div className="flex items-center justify-between px-5 pt-3 pb-1">
            <div className="text-sm font-semibold">
              Inside the logs agent
              {review && <span className="ml-2 font-mono text-[11px] text-accent">at {new Date(tView).toLocaleTimeString()}</span>}
            </div>
            <div className="eyebrow">every node and edge is a metric the agent exposes · fill = logs_component_utilization · hover, click a node</div>
          </div>
          <div className="px-3 pb-3 space-y-2">
            <GraphLane side="a" label={isAb ? 'a · control' : 'agent'} s={at(data.samples.a, tView)} history={histA} telemetry={data.telemetry.a} />
            {isAb && <GraphLane side="b" label="b · under test" s={at(data.samples.b, tView)} history={histB} telemetry={data.telemetry.b} />}
          </div>
        </Card>
      )}

      <div className="grid grid-cols-1 xl:grid-cols-[1fr_320px] gap-5">
        <div className="space-y-4 min-w-0">
          {hasData && (
            <>
              <div className={cn('grid gap-4', isAb ? 'md:grid-cols-2' : 'md:grid-cols-1')}>
                <SideColumn side="a" label={isAb ? 'a · control' : 'agent'} s={sA} history={histA} t={tView} review={review} />
                {isAb && <SideColumn side="b" label="b · under test" s={sB} history={histB} t={tView} review={review} />}
              </div>

              <Card>
                <CardContent className="pt-4 pb-3 space-y-4">
                  <div className="grid md:grid-cols-2 gap-x-6 gap-y-4">
                    {CHARTS.map((c) => (
                      <RateChart key={c.label} a={data.samples.a} b={isAb ? data.samples.b : []} pick={c.pick} pick2={c.pick2} label={c.label} label2={c.label2} fmt={c.fmt} bars={c.bars} windowSec={win} height={c.tall ? 160 : 120} range={range} cursor={review ? tView : undefined} onPick={onCursor} />
                    ))}
                  </div>
                  <div className="flex items-center gap-4 text-[11px] font-mono text-muted">
                    <span className="flex items-center gap-1.5">
                      <i className="inline-block w-3 h-0.5 bg-a" /> a {isAb ? 'control' : ''}
                    </span>
                    {isAb && (
                      <span className="flex items-center gap-1.5">
                        <i className="inline-block w-3 h-0.5 bg-b" /> b under test
                      </span>
                    )}
                    <span className="flex items-center gap-1.5">
                      <i className="inline-block w-3 h-2 bg-bad/20" /> faults active
                    </span>
                    {review && (
                      <span className="flex items-center gap-1.5">
                        <i className="inline-block w-px h-3 bg-accent" /> cursor
                      </span>
                    )}
                    <span className="text-faint">hover for values · click a chart to move the cursor</span>
                  </div>
                </CardContent>
              </Card>
            </>
          )}

          <LogPane run={run} lines={live.logs[run.id] ?? run.tail} />
        </div>

        <div className="space-y-4">
          {queued.length > 0 && (
            <Card>
              <CardHeader>
                <CardTitle className="flex items-center gap-2">
                  <Clock className="size-3.5 text-muted" /> Queue
                </CardTitle>
              </CardHeader>
              <CardContent className="space-y-2">
                {queued.map((q) => (
                  <div key={q.id} className="flex items-center justify-between gap-2 text-xs font-mono">
                    <button className="truncate hover:text-accent cursor-pointer text-left" onClick={() => onSelect(q.id)}>
                      {q.request.name}
                    </button>
                    <Button size="sm" variant="ghost" onClick={() => api.stop(q.id)}>
                      <Ban className="size-3" /> drop
                    </Button>
                  </div>
                ))}
              </CardContent>
            </Card>
          )}
          <Card>
            <CardHeader>
              <div className="flex items-center justify-between">
                <CardTitle>Recent</CardTitle>
                {history.length > 0 && (
                  <button className="text-[11px] text-faint hover:text-muted font-mono cursor-pointer" onClick={() => api.clearRuns()}>
                    clear
                  </button>
                )}
              </div>
            </CardHeader>
            <CardContent className="space-y-1">
              {history.length === 0 && <p className="text-xs text-faint">No finished runs in this server's history.</p>}
              {history.slice(0, 20).map((h) => (
                <button
                  key={h.id}
                  onClick={() => onSelect(h.id)}
                  className={cn('w-full text-left rounded-md px-2.5 py-2 hover:bg-surface-2 cursor-pointer transition-colors', h.id === run.id && 'bg-surface-2 ring-1 ring-border-strong')}
                >
                  <div className="flex items-center gap-2">
                    <PhaseIcon run={h} />
                    <span className="font-mono text-xs truncate flex-1">{h.request.name}</span>
                    <span className="text-[11px] text-faint">{ago(h.endedAt)}</span>
                  </div>
                  <div className="text-[11px] text-muted font-mono mt-0.5 truncate pl-5">
                    {h.request.kind} · {h.request.workload} {h.request.fault.length ? '· fault' : ''}
                  </div>
                </button>
              ))}
            </CardContent>
          </Card>
        </div>
      </div>
    </div>
  )
}

const CHARTS: { label: string; label2?: string; pick: (s: IntakeSample) => number; pick2?: (s: IntakeSample) => number; fmt?: (v: number) => string; bars?: boolean; tall?: boolean }[] = [
  { label: 'received logs / s', label2: 'written by the generators', pick: (s) => s.recentLogsPerSec, pick2: (s) => s.genRecordsPerSec, tall: true },
  { label: 'end-to-end latency p99 (s)', label2: 'p50', pick: (s) => s.e2eP99, pick2: (s) => s.e2eP50, fmt: (v) => `${v.toFixed(v < 1 ? 2 : 1)}s`, tall: true },
  { label: 'requests rejected or dropped / s', pick: (s) => s.err4xx + s.err5xx + s.dropped, bars: true },
  { label: 'backlog: decoded, not yet sent', pick: (s) => s.backlog },
  { label: 'core agent CPU (% of a core)', pick: (s) => s.agentCpuPct ?? 0, fmt: (v) => `${v.toFixed(0)}%` },
  { label: 'core agent RSS', label2: 'Go heap in use', pick: (s) => s.agentRssBytes ?? 0, pick2: (s) => s.heapInUse ?? 0, fmt: (v) => `${(v / 1048576).toFixed(0)}M` },
  { label: 'bytes on the wire / s', label2: 'raw (before compression)', pick: (s) => s.wireBytesPerSec, pick2: (s) => s.rawBytesPerSec, fmt: (v) => `${(v / 1024).toFixed(0)}K` },
  { label: 'busiest component utilization', pick: (s) => Math.max(0, ...Object.values(s.utilization)), fmt: (v) => `${(v * 100).toFixed(0)}%` },
  { label: 'bytes missed on rotation / s (lost at the tailer)', pick: (s) => s.tailer.bytesMissedPerSec, fmt: (v) => `${(v / 1024).toFixed(0)}K` },
  { label: 'sender retries / s', label2: 'network errors', pick: (s) => s.sender.retryPerSec, pick2: (s) => s.sender.networkErrorsPerSec, fmt: (v) => v.toFixed(1) },
  { label: 'intake missing, growth / s', pick: (s) => s.missingPerSec },
  { label: 'bytes held in the destination (retrying)', label2: 'in pipelines', pick: (s) => s.components.filter((c) => c.name.startsWith('destination')).reduce((n, c) => n + c.bytes, 0), pick2: (s) => s.components.filter((c) => c.name === 'strategy' || c.name === 'processor' || c.name === 'sender').reduce((n, c) => n + c.bytes, 0), fmt: (v) => `${(v / 1048576).toFixed(1)}M` },
]

function RunHeader({ run }: { run: RunRecord }) {
  const [confirm, setConfirm] = useState(false)
  const active = isActive(run)
  useEffect(() => {
    if (!confirm) return
    const t = setTimeout(() => setConfirm(false), 4000)
    return () => clearTimeout(t)
  }, [confirm])
  return (
    <div className="flex flex-wrap items-start justify-between gap-3">
      <div className="min-w-0">
        <div className="flex items-center gap-2 flex-wrap">
          {active && <span className="live-dot inline-block size-2 rounded-full bg-accent" />}
          <h2 className="font-mono text-lg font-semibold truncate">{run.request.name}</h2>
          <Badge variant={run.request.kind === 'ab' ? 'accent' : 'default'}>{run.request.kind === 'ab' ? 'A/B' : 'run'}</Badge>
          <PhaseBadge run={run} />
        </div>
        <p className="text-sm text-muted mt-1">{run.request.focus || `${run.request.workload}${run.request.fault.length ? ' · ' + run.request.fault.join(' ') : ''}`}</p>
        <p className="font-mono text-[11px] text-faint mt-1 truncate max-w-[70ch]">{run.command}</p>
      </div>
      <div className="flex items-center gap-2">
        {run.phase === 'finished' && (
          <Button variant="outline" asChild>
            <a href="#results">
              <FileText className="size-3.5" /> {run.resultsDir}
            </a>
          </Button>
        )}
        {active && !run.stopRequested && (
          <Button variant={confirm ? 'destructive' : 'outline'} onClick={() => (confirm ? api.stop(run.id) : setConfirm(true))}>
            <Square className="size-3.5" /> {confirm ? 'Confirm stop' : 'Stop'}
          </Button>
        )}
        {active && run.stopRequested && (
          <Tooltip>
            <TooltipTrigger asChild>
              <Button variant="destructive" onClick={() => api.stop(run.id, true)}>
                <Octagon className="size-3.5" /> Kill now
              </Button>
            </TooltipTrigger>
            <TooltipContent>aoc was interrupted and is taking the stack down. Killing skips that; use the cleanup button afterwards.</TooltipContent>
          </Tooltip>
        )}
      </div>
    </div>
  )
}

function PhaseBadge({ run }: { run: RunRecord }) {
  const v = run.phase === 'finished' ? (run.exitCode === 3 ? 'bad' : 'good') : run.phase === 'failed' ? 'bad' : run.phase === 'interrupted' ? 'warn' : run.phase === 'queued' ? 'muted' : 'accent'
  const text = run.phase === 'finished' ? (run.exitCode === 3 ? 'finished · delivery gate failed' : 'finished') : run.stopRequested && isActive(run) ? 'stopping' : run.phase
  return <Badge variant={v}>{text}</Badge>
}

function PhaseIcon({ run }: { run: RunRecord }) {
  if (run.phase === 'finished' && run.exitCode === 0) return <CheckCircle2 className="size-3.5 text-good shrink-0" />
  if (run.phase === 'finished') return <AlertTriangle className="size-3.5 text-bad shrink-0" />
  if (run.phase === 'failed') return <XCircle className="size-3.5 text-bad shrink-0" />
  return <Ban className="size-3.5 text-warn shrink-0" />
}

function GraphLane({ side, label, s, history, telemetry }: { side: 'a' | 'b'; label: string; s: IntakeSample | undefined; history: IntakeSample[]; telemetry: TelemetrySeries[] | undefined }) {
  const [selected, setSelected] = useState<string | null>(null)
  return (
    <div className={cn('rounded-md hairline p-2', s?.faultsActive && 'fault-glow')}>
      <div className="flex items-center gap-2 px-2 pt-1">
        <span className={cn('font-mono text-xs font-semibold', side === 'a' ? 'text-a' : 'text-b')}>{label}</span>
        {s?.faultsActive && (
          <Badge variant="bad">
            <AlertTriangle className="size-3" /> {s.faultNote ?? 'fault'}
          </Badge>
        )}
        {s && s.tailer.bytesMissedPerSec > 0 && <Badge variant="bad">losing {fmtBytes(s.tailer.bytesMissedPerSec)}/s at the tailer</Badge>}
        {s && s.missing > 0 && <Badge variant={s.missingPerSec > 0 ? 'bad' : 'warn'}>missing {fmtShort(s.missing)}</Badge>}
      </div>
      <ComponentGraph s={s} side={side} history={history} telemetry={telemetry} selected={selected} onSelect={setSelected} />
    </div>
  )
}

function SideColumn({ side, label, s, history, t, review }: { side: 'a' | 'b'; label: string; s: IntakeSample | undefined; history: IntakeSample[]; t: number; review: boolean }) {
  const stale = !s || (!review && Date.now() - s.t > 5000)
  const delivered = s && s.generated > 0 ? (s.unique / s.generated) * 100 : undefined
  return (
    <Card className={cn('relative overflow-hidden', s?.faultsActive && 'fault-glow')}>
      <div className={cn('absolute left-0 top-0 bottom-0 w-1', side === 'a' ? 'bg-a' : 'bg-b')} />
      <CardHeader className="pb-2">
        <div className="flex items-center justify-between">
          <CardTitle className={cn('font-mono', side === 'a' ? 'text-a' : 'text-b')}>{label}</CardTitle>
          <div className="flex items-center gap-1.5">
            {s?.faultsActive && (
              <Badge variant="bad">
                <AlertTriangle className="size-3" /> {s.faultNote ?? 'fault'}
              </Badge>
            )}
            {s && !s.telemetryOk && <Badge variant="muted">no agent telemetry</Badge>}
            {stale && <Badge variant="muted">{s ? 'stale' : 'no intake yet'}</Badge>}
          </div>
        </div>
      </CardHeader>
      <CardContent className="space-y-4">
        <GaugeRow s={s} side={side} />
        <div>
          <div className="eyebrow mb-1.5">why <span className="normal-case tracking-normal text-faint">· values are 5 s means; lines hold for 15 s</span></div>
          <Diagnosis history={history} t={t} side={side} />
        </div>
        <div className="grid grid-cols-3 md:grid-cols-6 gap-x-4 gap-y-3">
          <Stat label="received /s" value={fmtShort(s?.recentLogsPerSec)} big />
          <Stat label="missing" value={fmtInt(s?.missing)} tone={s && s.missing > 0 ? 'bad' : undefined} big />
          <Stat label="delivered" value={delivered == null ? '–' : `${delivered.toFixed(delivered > 99 ? 2 : 1)}%`} big />
          <Stat label="generated" value={fmtShort(s?.generated)} />
          <Stat label="unique" value={fmtShort(s?.unique)} />
          <Stat label="inflight" value={fmtInt(s?.inflight)} />
          <Stat label="requests /s" value={s ? s.requestsPerSec.toFixed(1) : '–'} />
          <Stat label="wire kB/s" value={s ? (s.wireBytesPerSec / 1024).toFixed(0) : '–'} />
          <Stat label="e2e p50" value={s && s.e2eP50 > 0 ? `${s.e2eP50.toFixed(2)}s` : '–'} />
          <Stat label="sender p99" value={s && s.senderP99 > 0 ? `${s.senderP99.toFixed(2)}s` : '–'} />
          <Stat label="heap" value={fmtBytes(s?.heapInUse)} />
          <Stat label="window" value={s ? fmtDur(s.windowSeconds) : '–'} />
        </div>
        {s && s.generators.length > 0 && (
          <div>
            <div className="eyebrow mb-1.5">generators</div>
            <div className="flex flex-wrap gap-1.5">
              {s.generators.map((g) => (
                <Tooltip key={g.name}>
                  <TooltipTrigger asChild>
                    <span
                      className={cn(
                        'inline-flex items-center gap-1.5 rounded-sm px-1.5 py-0.5 font-mono text-[11px] hairline',
                        g.final ? 'text-faint' : g.phase === 'running' || g.recordRate > 0 ? 'text-foreground' : 'text-muted',
                      )}
                    >
                      <i className={cn('inline-block size-1.5 rounded-full', g.final ? 'bg-faint' : g.recordRate > 0 ? 'bg-good' : 'bg-warn')} />
                      {g.name.replace(/^gen-/, '')}
                      <span className="text-muted">{fmtShort(g.recordRate)}/s</span>
                    </span>
                  </TooltipTrigger>
                  <TooltipContent>
                    {g.name}: {g.phase}
                    {g.targetRate ? `, target ${fmtShort(g.targetRate)}/s` : ''}
                    {g.final ? ', final report in' : ''}
                  </TooltipContent>
                </Tooltip>
              ))}
            </div>
          </div>
        )}
      </CardContent>
    </Card>
  )
}

function Stat({ label, value, big, tone }: { label: string; value: string; big?: boolean; tone?: 'bad' }) {
  return (
    <div>
      <div className="eyebrow !text-[10px]">{label}</div>
      <div className={cn('num leading-none mt-1', big ? 'text-2xl font-medium' : 'text-sm', tone === 'bad' && 'text-bad')}>{value}</div>
    </div>
  )
}

function LogPane({ run, lines }: { run: RunRecord; lines: string[] }) {
  const ref = useRef<HTMLDivElement>(null)
  const [follow, setFollow] = useState(true)
  const [open, setOpen] = useState(!isActive(run))
  const shown = useMemo(() => lines.slice(-400), [lines])
  useEffect(() => {
    if (follow && ref.current) ref.current.scrollTop = ref.current.scrollHeight
  }, [shown, follow])
  return (
    <Card className="relative">
      <CardHeader className="pb-2">
        <div className="flex items-center justify-between">
          <button className="flex items-center gap-1.5 font-mono text-sm font-semibold cursor-pointer" onClick={() => setOpen((o) => !o)}>
            {open ? <ChevronUp className="size-3.5 text-muted" /> : <ChevronDown className="size-3.5 text-muted" />} log
            {!open && <span className="ml-2 font-normal text-[11px] text-muted truncate max-w-[70ch]">{shown.at(-1)}</span>}
          </button>
          <div className="flex items-center gap-3 text-[11px] font-mono text-muted">
            <span className="truncate max-w-[40ch]">{run.logPath}</span>
            <label className="flex items-center gap-1 cursor-pointer">
              <input type="checkbox" checked={follow} onChange={(e) => setFollow(e.target.checked)} className="accent-[var(--accent)]" /> follow
            </label>
            <a className="hover:text-foreground" href={`/api/runs/${run.id}/log`} target="_blank" rel="noreferrer">
              full
            </a>
          </div>
        </div>
      </CardHeader>
      {open && (
      <CardContent>
        <div
          ref={ref}
          onScroll={(e) => {
            const el = e.currentTarget
            setFollow(el.scrollHeight - el.scrollTop - el.clientHeight < 24)
          }}
          className="relative h-72 overflow-auto scrollbar-thin rounded-md bg-background/70 hairline p-3 font-mono text-[11.5px] leading-[1.45]"
        >
          {shown.map((l, i) => (
            <div key={i} className={cn('whitespace-pre-wrap break-all', classify(l))}>
              {l}
            </div>
          ))}
          {shown.length === 0 && <div className="text-faint">waiting for output</div>}
        </div>
      </CardContent>
      )}
    </Card>
  )
}

function classify(l: string) {
  if (/error|failed|not reachable|lost\s+[1-9]|duplicates\s+[1-9]|[1-9][\d,]* (lost|duplicate)/i.test(l)) return 'text-bad'
  if (/warning/i.test(l)) return 'text-warn'
  if (/measured window open|drained|verdict|results in|findings/i.test(l)) return 'text-accent'
  if (/^\s*\$ /.test(l)) return 'text-muted'
  if (/^\s*\d\d:\d\d:\d\d/.test(l)) return 'text-foreground/90'
  return 'text-muted'
}
