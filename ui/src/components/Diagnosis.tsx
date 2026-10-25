import { AlertTriangle, CheckCircle2, Info, OctagonAlert } from 'lucide-react'
import type { IntakeSample } from '@shared/types'
import { cn, fmtBytes, fmtShort } from '@/lib/utils'

type Sev = 'bad' | 'warn' | 'info' | 'ok'
export interface Line {
  id: string
  sev: Sev
  text: string
}

// Why the agent is under stress, in sentences, from the last sample and the
// recent ones. Rules, not a model: each line names the number it came from.
export function diagnose(s: IntakeSample | undefined, recent: IntakeSample[]): Line[] {
  if (!s) return [{ id: 'none', sev: 'info', text: 'No intake yet. The stack is coming up.' }]
  const out: Line[] = []
  const rejected = s.err4xx + s.err5xx + s.dropped
  const reqs = s.requestsPerSec + rejected
  const errPct = reqs > 0 ? (rejected / reqs) * 100 : 0
  const busiest = s.telemetryOk ? Object.entries(s.utilization).sort((x, y) => y[1] - x[1])[0] : undefined
  const last30 = recent.slice(-30)
  const avg = (f: (x: IntakeSample) => number) => (last30.length ? last30.reduce((n, x) => n + f(x), 0) / last30.length : 0)
  const genAvg = avg((x) => x.genRecordsPerSec)
  const recvAvg = avg((x) => x.recentLogsPerSec)
  const decodedAvg = avg((x) => x.stages.decoded)

  if (s.faultsActive) out.push({ id: 'fault', sev: 'bad', text: `Intake fault active: ${s.faultNote ?? 'see the profile'}. What follows is the agent coping with it.` })
  if (errPct >= 1) {
    const codes = Object.entries(s.httpResp)
      .filter(([c, v]) => v > 0.01 && !/^2/.test(c))
      .map(([c]) => c)
    out.push({ id: 'rejected',
      sev: errPct > 25 ? 'bad' : 'warn',
      text: `${errPct.toFixed(0)}% of requests rejected${codes.length ? ` (the agent sees ${codes.join(', ')})` : ''}; the sender retries with backoff, so records queue up behind it.`,
    })
  }
  if (s.tailer.bytesMissedPerSec > 0) out.push({ id: 'tailer-loss', sev: 'bad', text: `Losing data at the tailer: ${fmtShort(s.tailer.bytesMissedPerSec)} B/s missed on rotation (${fmtShort(s.tailer.bytesMissedTotal)} B so far). Files rotate away before the tailer reads them; these records never reach the agent, which is where the intake's missing count comes from.` })
  if (s.sender.retryPerSec > 0.05) out.push({ id: 'retrying', sev: 'warn', text: `The destination is retrying ${s.sender.retryPerSec.toFixed(1)} payloads/s with backoff (${s.sender.networkErrorsPerSec.toFixed(1)} network errors/s); ${fmtBytes(s.components.filter((c) => c.name.startsWith('destination')).reduce((n, c) => n + c.bytes, 0))} of payloads are held in the destination meanwhile. Nothing is dropped there; the cost is memory and delay.` })
  if (s.backlog > 5000) out.push({ id: 'backlog', sev: s.backlog > 50000 ? 'bad' : 'warn', text: `${fmtShort(s.backlog)} records decoded but not yet sent: that is memory in the agent's channels and sender queue, and latency for every record behind them.` })
  if (busiest && busiest[1] > 0.6) out.push({ id: 'bottleneck', sev: busiest[1] > 0.85 ? 'bad' : 'warn', text: `${busiest[0]} is ${Math.round(busiest[1] * 100)}% busy: the pipeline's bottleneck right now; throughput can only grow by making this stage cheaper.` })
  if (s.telemetryOk && genAvg > 300 && decodedAvg < genAvg * 0.85) {
    const backpressured = errPct >= 1 || s.backlog > 5000 || (busiest && /sender|destination|worker|strategy/.test(busiest[0]) && busiest[1] > 0.6)
    out.push({
      id: 'tailer-behind',
      sev: 'warn',
      text: backpressured
        ? `Back-pressure: the tailer decodes only ${fmtShort(decodedAvg)}/s against ${fmtShort(genAvg)}/s written (30 s average) because the sender is blocked downstream, not because tailing is slow. Files keep rotating meanwhile; records are lost if a file is deleted before the tailer reaches it.`
        : `The tailer decodes ${fmtShort(decodedAvg)}/s against ${fmtShort(genAvg)}/s written (30 s average): it is falling behind the files; expect rotation losses if it cannot catch up before files are deleted.`,
    })
  }
  if (s.e2eP99 > 2) out.push({ id: 'latency', sev: s.e2eP99 > 5 ? 'bad' : 'warn', text: `End-to-end p99 is ${s.e2eP99.toFixed(1)} s${s.faultsActive ? ', mostly the intake fault' : ''}; p50 ${s.e2eP50.toFixed(2)} s, max ${s.e2eMax.toFixed(1)} s.` })
  if (s.agentCpuPct != null && s.agentCpuPct > 80) out.push({ id: 'cpu', sev: s.agentCpuPct > 95 ? 'bad' : 'warn', text: `The core agent is at ${s.agentCpuPct.toFixed(0)}% of a core.` })
  if (s.telemetryOk && s.stages.sent > 50 && s.flushes.maxCount > s.flushes.timer * 2) out.push({ id: 'flush-full', sev: 'info', text: `Payloads flush full (${s.flushes.maxCount.toFixed(1)}/s by size vs ${s.flushes.timer.toFixed(1)}/s by timer): the batcher is saturated, which is efficient on the wire but adds queueing.` })
  if (s.telemetryOk && s.stages.sent > 50 && s.flushes.timer > s.flushes.maxCount * 2) out.push({ id: 'flush-timer', sev: 'info', text: `Payloads flush on the timer (${s.flushes.timer.toFixed(1)}/s): the sender has headroom; batches are smaller than they could be.` })
  if (s.rotationsPerSec > 0.5) out.push({ id: 'rotations', sev: 'info', text: `${s.rotationsPerSec.toFixed(1)} file rotations/s: the tailer is reopening files constantly.` })
  if (s.missing > 0 && !s.faultsActive && s.inflight === 0 && recvAvg < genAvg * 0.5) out.push({ id: 'missing-no-fault', sev: 'bad', text: `${fmtShort(s.missing)} records missing with no fault active and nothing in flight.` })
  if (s.goroutines && s.goroutines > 1500) out.push({ id: 'goroutines', sev: 'warn', text: `${fmtShort(s.goroutines)} goroutines in the core agent.` })
  if (out.length === 0) {
    out.push({
      id: 'ok',
      sev: 'ok',
      text: s.recentLogsPerSec > 0 ? `No pressure: ${fmtShort(s.recentLogsPerSec)}/s received against ${fmtShort(genAvg)}/s written, e2e p99 ${s.e2eP99.toFixed(2)} s, ${busiest ? `${busiest[0]} ${Math.round(busiest[1] * 100)}% busy` : 'no component telemetry yet'}.` : 'Quiet: nothing arriving yet.',
    })
  }
  return out
}

// The lines that held at any point in the last `holdSec` seconds up to t,
// each with its latest wording, in the order they first appeared, so the
// list does not reshuffle or flicker as single seconds come and go.
export function steadyDiagnose(history: IntakeSample[], t: number, holdSec = 15): Line[] {
  const win = history.filter((s) => s.t <= t && s.t > t - holdSec * 1000)
  if (win.length === 0) return diagnose(undefined, [])
  const seen = new Map<string, { line: Line; first: number; hits: number }>()
  for (const s of win) {
    const upto = history.filter((h) => h.t <= s.t)
    for (const l of diagnose(s, upto)) {
      const e = seen.get(l.id)
      if (e) {
        e.line = l
        e.hits++
      } else seen.set(l.id, { line: l, first: s.t, hits: 1 })
    }
  }
  const lines = [...seen.values()]
  // "ok" only when nothing else held
  const real = lines.filter((e) => e.line.id !== 'ok' && e.line.id !== 'none')
  const chosen = real.length ? real : lines
  const rank: Record<Sev, number> = { bad: 0, warn: 1, info: 2, ok: 3 }
  return chosen.sort((x, y) => rank[x.line.sev] - rank[y.line.sev] || x.first - y.first).map((e) => e.line)
}

const ICON = { bad: OctagonAlert, warn: AlertTriangle, info: Info, ok: CheckCircle2 }
const TONE = { bad: 'text-bad', warn: 'text-warn', info: 'text-muted', ok: 'text-good' }

export function Diagnosis({ history, t, side }: { history: IntakeSample[]; t: number; side: 'a' | 'b' }) {
  const lines = steadyDiagnose(history, t)
  return (
    <ul className="space-y-1.5">
      {lines.map((l, i) => {
        const Icon = ICON[l.sev]
        return (
          <li key={l.id + i} className="flex items-start gap-2 text-xs leading-5">
            <Icon className={cn('size-3.5 mt-1 shrink-0', TONE[l.sev])} />
            <span className={cn(l.sev === 'info' ? 'text-muted' : 'text-foreground/90')}>{l.text}</span>
            <span className={cn('sr-only')}>{side}</span>
          </li>
        )
      })}
    </ul>
  )
}
