// Polls the fake intake for each side while a run is active: side a on 8282,
// side b on 8283 (internal/cli/ab.go's base ports). Three reads per tick:
// /harness/status (delivery, generators, faults), /harness/timeseries (the
// last per-second point: latency, errors, bytes) and /harness/telemetry (the
// agent's own counters, digested into stage rates and utilization).
import { EventEmitter } from 'node:events'
import type { ComponentStat, IntakeSample, RunSamples, TelemetrySeries } from '../shared/types.ts'

const PORTS: Record<'a' | 'b', number> = { a: 8282, b: 8283 }
const KEEP = 2400 // samples per side, 1/s → 40 minutes, longer than any single run

interface Status {
  recent_logs_per_sec: number
  missing: number
  unique: number
  generated: number
  inflight: number
  faults_active: boolean
  faults?: { note?: string; latency_ms?: number; error_rate?: number; error_status?: number; drop_rate?: number; outage?: boolean; read_bps?: number }
  window_seconds: number
  generators?: { name: string; phase: string; record_rate: number; target_rate: number; final: boolean }[]
  observer?: { proc_cpu_percent?: number; proc_rss?: number; cpu_percent?: number; mem_anon_bytes?: number; telemetry_ok?: boolean }
}
interface Point {
  t: number
  requests: number
  logs: number
  raw_bytes: number
  wire_bytes: number
  err_4xx: number
  err_5xx: number
  dropped: number
  gen_records: number
  e2e_p50: number
  e2e_p99: number
  e2e_max: number
  sender_p50: number
  sender_p99: number
}
interface Series {
  name: string
  labels: string
  type: string
  value: number
  first: number
  rate: number
}

export class IntakePoller extends EventEmitter {
  samples: Record<'a' | 'b', IntakeSample[]> = { a: [], b: [] }
  private timer?: NodeJS.Timeout
  private running = false
  private base: Record<'a' | 'b', { decoded: number; sent: number } | undefined> = { a: undefined, b: undefined }
  telemetry: Partial<Record<'a' | 'b', TelemetrySeries[]>> = {}

  setActive(active: boolean) {
    if (active && !this.running) {
      this.running = true
      this.samples = { a: [], b: [] }
      this.base = { a: undefined, b: undefined }
      this.telemetry = {}
      this.tick()
    } else if (!active && this.running) {
      this.running = false
      if (this.timer) clearTimeout(this.timer)
    }
  }

  recent(): IntakeSample[] {
    return [...this.samples.a.slice(-600), ...this.samples.b.slice(-600)]
  }

  snapshot(): RunSamples {
    return { samples: { a: [...this.samples.a], b: [...this.samples.b] }, telemetry: { ...this.telemetry } }
  }

  private async tick() {
    if (!this.running) return
    await Promise.all((['a', 'b'] as const).map((side) => this.poll(side)))
    if (this.running) this.timer = setTimeout(() => this.tick(), 1000)
  }

  private async get<T>(side: 'a' | 'b', path: string): Promise<T | undefined> {
    try {
      const res = await fetch(`http://localhost:${PORTS[side]}${path}`, { signal: AbortSignal.timeout(900) })
      if (!res.ok) return undefined
      return (await res.json()) as T
    } catch {
      return undefined
    }
  }

  private async poll(side: 'a' | 'b') {
    const since = Math.floor(Date.now() / 1000) - 3
    const [s, pts, series] = await Promise.all([
      this.get<Status>(side, '/harness/status'),
      this.get<Point[]>(side, `/harness/timeseries?since=${since}`),
      this.get<Series[]>(side, '/harness/telemetry'),
    ])
    if (!s) return // the stack is not up yet, or this side is not part of the run
    // the newest complete second; the current one is still filling
    const p = pts && pts.length > 1 ? pts[pts.length - 2] : pts?.[0]
    const f = s.faults ?? {}
    const faultNote =
      f.note ||
      [
        f.outage && 'outage',
        f.latency_ms && `latency ${f.latency_ms}ms`,
        f.error_rate && `${f.error_status ?? 'errors'} on ${Math.round(f.error_rate * 100)}%`,
        f.drop_rate && `drop ${Math.round(f.drop_rate * 100)}%`,
        f.read_bps && `read ${Math.round(f.read_bps / 1000)} kB/s`,
      ]
        .filter(Boolean)
        .join(', ')
    const t = digest(series ?? [])
    const prev = this.samples[side].at(-1)
    const now = Date.now()
    const base = this.base[side] ?? (t.decodedTotal > 0 ? (this.base[side] = { decoded: t.decodedTotal, sent: t.sentTotal }) : undefined)
    const sample: IntakeSample = {
      side,
      t: now,
      recentLogsPerSec: s.recent_logs_per_sec ?? 0,
      missing: s.missing ?? 0,
      unique: s.unique ?? 0,
      generated: s.generated ?? 0,
      inflight: s.inflight ?? 0,
      faultsActive: !!s.faults_active,
      faultNote: faultNote || undefined,
      windowSeconds: s.window_seconds ?? 0,
      generators: (s.generators ?? []).map((g) => ({
        name: g.name,
        phase: g.phase,
        recordRate: g.record_rate,
        targetRate: g.target_rate,
        final: g.final,
      })),
      agentCpuPct: s.observer?.proc_cpu_percent || s.observer?.cpu_percent,
      agentRssBytes: s.observer?.proc_rss || s.observer?.mem_anon_bytes,
      genRecordsPerSec: p?.gen_records ?? 0,
      requestsPerSec: p?.requests ?? 0,
      err4xx: p?.err_4xx ?? 0,
      err5xx: p?.err_5xx ?? 0,
      dropped: p?.dropped ?? 0,
      wireBytesPerSec: p?.wire_bytes ?? 0,
      rawBytesPerSec: p?.raw_bytes ?? 0,
      e2eP50: p?.e2e_p50 ?? 0,
      e2eP99: p?.e2e_p99 ?? 0,
      e2eMax: p?.e2e_max ?? 0,
      senderP50: p?.sender_p50 ?? 0,
      senderP99: p?.sender_p99 ?? 0,
      telemetryOk: !!s.observer?.telemetry_ok && (series?.length ?? 0) > 0,
      stages: t.stages,
      utilization: t.utilization,
      httpResp: t.httpResp,
      flushes: t.flushes,
      rotationsPerSec: t.rotations,
      goroutines: t.goroutines,
      heapInUse: t.heapInUse,
      gcPerSec: t.gcPerSec,
      encodedBytesPerSec: t.encodedBytes,
      logBytesPerSec: t.logBytes,
      backlog: base ? Math.max(0, t.decodedTotal - base.decoded - (t.sentTotal - base.sent)) : 0,
      missingPerSec: prev && now > prev.t ? Math.max(0, ((s.missing ?? 0) - prev.missing) / ((now - prev.t) / 1000)) : 0,
      components: t.components,
      tailer: t.tailer,
      sender: t.sender,
    }
    const arr = this.samples[side]
    arr.push(sample)
    if (arr.length > KEEP) arr.splice(0, arr.length - KEEP)
    this.emit('sample', sample)
    if (series) {
      const kept = series.filter((m) => /^(logs|go_goroutines|go_memstats_heap_inuse_bytes|process_)/.test(m.name) && !/datadoghq\.com/.test(m.labels)).map(({ name, labels, type, value, rate }) => ({ name, labels, type, value, rate }))
      this.telemetry[side] = kept
      this.emit('telemetry', side, kept)
    }
  }
}

function label(labels: string, key: string): string | undefined {
  const m = new RegExp(`${key}="([^"]*)"`).exec(labels)
  return m?.[1]
}

// The agent's series → what the pipeline picture needs.
function digest(series: Series[]) {
  const stages = { decoded: 0, processed: 0, sent: 0 }
  const utilization: Record<string, number> = {}
  const httpResp: Record<string, number> = {}
  const flushes = { maxCount: 0, timer: 0 }
  let rotations = 0
  let goroutines: number | undefined
  let heapInUse: number | undefined
  let gcPerSec: number | undefined
  let encodedBytes = 0
  let logBytes = 0
  let decodedTotal = 0
  let sentTotal = 0
  const comp = new Map<string, ComponentStat>()
  const compOf = (labels: string) => {
    const name = label(labels, 'name') ?? 'component'
    const instance = label(labels, 'instance') ?? ''
    const k = `${name}/${instance}`
    let c = comp.get(k)
    if (!c) comp.set(k, (c = { name, instance, ratio: 0, items: 0, bytes: 0 }))
    return c
  }
  const tailer = { bytesMissedPerSec: 0, bytesMissedTotal: 0, rotationSizeMismatchPerSec: 0, multilineFlushPerSec: 0, singleLineFlushPerSec: 0 }
  const sender = { retryPerSec: 0, networkErrorsPerSec: 0, sendWaitMsPerSec: 0, inUseMsPerSec: 0, idleMsPerSec: 0, sendOkPerSec: 0, sendRetryablePerSec: 0 }
  for (const m of series) {
    switch (m.name) {
      case 'logs__decoded':
        stages.decoded += m.rate
        decodedTotal += m.value
        break
      case 'logs__processed':
        stages.processed += m.rate
        break
      case 'logs__sent':
        stages.sent += m.rate
        sentTotal += m.value
        break
      case 'logs_component_utilization__ratio': {
        const name = label(m.labels, 'name') ?? 'component'
        utilization[name] = Math.max(utilization[name] ?? 0, m.value)
        compOf(m.labels).ratio = m.value
        break
      }
      case 'logs_component_utilization__items':
        compOf(m.labels).items = m.value
        break
      case 'logs_component_utilization__bytes':
        compOf(m.labels).bytes = m.value
        break
      case 'logs__bytes_missed':
        tailer.bytesMissedPerSec += m.rate
        tailer.bytesMissedTotal += m.value
        break
      case 'logs__rotation_size_mismatch':
        tailer.rotationSizeMismatchPerSec += m.rate
        break
      case 'logs__auto_multi_line_aggregator_flush':
        if (label(m.labels, 'line_type') === 'multi_line') tailer.multilineFlushPerSec += m.rate
        else tailer.singleLineFlushPerSec += m.rate
        break
      case 'logs__retry_count':
        sender.retryPerSec += m.rate
        break
      case 'logs__network_errors':
        sender.networkErrorsPerSec += m.rate
        break
      case 'logs_sender__send_wait':
        sender.sendWaitMsPerSec += m.rate
        break
      case 'logs_client_http_destination__in_use_ms':
        if (/^logs_/.test(label(m.labels, 'sender') ?? '')) sender.inUseMsPerSec += m.rate
        break
      case 'logs_client_http_destination__idle_ms':
        if (/^logs_/.test(label(m.labels, 'sender') ?? '')) sender.idleMsPerSec += m.rate
        break
      case 'logs_client_http_destination__send': {
        const host = label(m.labels, 'endpoint_host') ?? ''
        if (!/localhost|intake|127\.0\.0\.1/.test(host)) break
        if (label(m.labels, 'error') === 'none') sender.sendOkPerSec += m.rate
        else sender.sendRetryablePerSec += m.rate
        break
      }
      case 'logs__destination_http_resp': {
        const url = label(m.labels, 'url') ?? ''
        if (!/localhost|intake:|aoc-intake|8282|8283/.test(url)) break
        const code = label(m.labels, 'status_code') ?? '?'
        httpResp[code] = (httpResp[code] ?? 0) + m.rate
        break
      }
      case 'logs__batch_payload_flushed': {
        const reason = label(m.labels, 'flush_reason')
        if (reason === 'max_count' || reason === 'max_bytes') flushes.maxCount += m.rate
        else flushes.timer += m.rate
        break
      }
      case 'logs__rotations_nix':
        rotations += m.rate
        break
      case 'go_goroutines':
        goroutines = m.value
        break
      case 'go_memstats_heap_inuse_bytes':
        heapInUse = m.value
        break
      case 'go_gc_duration_seconds_count':
        gcPerSec = m.rate
        break
      case 'logs__encoded_bytes_sent':
        if (label(m.labels, 'source') === 'logs') encodedBytes += m.rate
        break
      case 'logs__bytes_sent':
        if (label(m.labels, 'source') === 'logs') logBytes += m.rate
        break
    }
  }
  const components = [...comp.values()].sort((x, y) => x.name.localeCompare(y.name) || x.instance.localeCompare(y.instance, undefined, { numeric: true }))
  return { stages, utilization, httpResp, flushes, rotations, goroutines, heapInUse, gcPerSec, encodedBytes, logBytes, decodedTotal, sentTotal, components, tailer, sender }
}
