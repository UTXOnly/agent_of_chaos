import { useEffect, useSyncExternalStore } from 'react'
import type {
  Config,
  DockerContainer,
  IntakeSample,
  ProfileSummary,
  ResultSummary,
  RunRecord,
  RunRequest,
  RunSamples,
  ServerEvent,
  TelemetrySeries,
} from '@shared/types'

async function j<T>(url: string, init?: RequestInit): Promise<T> {
  const res = await fetch(url, { headers: { 'content-type': 'application/json' }, ...init })
  const body = await res.json().catch(() => ({}))
  if (!res.ok) throw new Error((body as { error?: string }).error ?? `${res.status} ${res.statusText}`)
  return body as T
}

export const api = {
  config: () => j<Config>('/api/config'),
  profiles: () => j<ProfileSummary[]>('/api/profiles'),
  results: () => j<ResultSummary[]>('/api/results'),
  resultFile: (name: string, file: string) => fetch(`/api/results/${name}/${file}`).then((r) => (r.ok ? r.text() : '')),
  plan: (r: RunRequest) => j<{ ok: boolean; output: string; argv: string[] }>('/api/plan', { method: 'POST', body: JSON.stringify(r) }),
  launch: (r: RunRequest) => j<RunRecord>('/api/runs', { method: 'POST', body: JSON.stringify(r) }),
  stop: (id: string, force = false) => j<{ ok: boolean }>(`/api/runs/${id}/stop${force ? '?force=1' : ''}`, { method: 'POST' }),
  log: (id: string) => fetch(`/api/runs/${id}/log`).then((r) => r.text()),
  samples: (id: string) => j<RunSamples>(`/api/runs/${id}/samples`),
  clearRuns: () => j<{ ok: boolean }>('/api/runs', { method: 'DELETE' }),
  cleanup: () => j<{ notes: string[] }>('/api/docker/cleanup', { method: 'POST' }),
  shutdown: () => j<{ ok: boolean }>('/api/shutdown', { method: 'POST' }),
}

// ── live store: one EventSource, one snapshot ───────────────────────────────

export interface Live {
  connected: boolean
  runs: RunRecord[]
  logs: Record<string, string[]>
  samples: { a: IntakeSample[]; b: IntakeSample[] }
  containers: DockerContainer[]
  telemetry: Partial<Record<'a' | 'b', TelemetrySeries[]>>
}

const MAX_SAMPLES = 900
const MAX_LOG = 400

let state: Live = { connected: false, runs: [], logs: {}, samples: { a: [], b: [] }, containers: [], telemetry: {} }
const listeners = new Set<() => void>()
let es: EventSource | undefined

function set(patch: Partial<Live>) {
  state = { ...state, ...patch }
  for (const l of listeners) l()
}

function apply(e: ServerEvent) {
  switch (e.type) {
    case 'hello': {
      const a = e.samples.filter((s) => s.side === 'a')
      const b = e.samples.filter((s) => s.side === 'b')
      const logs: Record<string, string[]> = {}
      for (const r of e.runs) logs[r.id] = r.tail
      set({ runs: e.runs, samples: { a, b }, logs, connected: true, telemetry: e.telemetry ?? {} })
      break
    }
    case 'run': {
      const runs = state.runs.some((r) => r.id === e.run.id)
        ? state.runs.map((r) => (r.id === e.run.id ? { ...e.run, tail: r.tail } : r))
        : [...state.runs, e.run]
      // a fresh run: drop the previous run's samples
      const fresh = e.run.phase === 'starting' && !state.runs.some((r) => r.id === e.run.id && r.phase !== 'queued')
      set({ runs, samples: fresh ? { a: [], b: [] } : state.samples })
      break
    }
    case 'log': {
      const cur = state.logs[e.id] ?? []
      const next = cur.length >= MAX_LOG ? [...cur.slice(cur.length - MAX_LOG + 1), e.line] : [...cur, e.line]
      set({ logs: { ...state.logs, [e.id]: next } })
      break
    }
    case 'intake': {
      const side = e.sample.side
      const arr = state.samples[side]
      const next = arr.length >= MAX_SAMPLES ? [...arr.slice(1), e.sample] : [...arr, e.sample]
      set({ samples: { ...state.samples, [side]: next } })
      break
    }
    case 'docker':
      set({ containers: e.containers })
      break
    case 'telemetry':
      set({ telemetry: { ...state.telemetry, [e.side]: e.series } })
      break
  }
}

function connect() {
  if (es) return
  es = new EventSource('/api/events')
  es.onmessage = (m) => {
    try {
      apply(JSON.parse(m.data) as ServerEvent)
    } catch {
      // a ping or a malformed line
    }
  }
  es.onerror = () => {
    set({ connected: false })
    es?.close()
    es = undefined
    setTimeout(connect, 1500)
  }
}

export function useLive(): Live {
  useEffect(() => {
    connect()
  }, [])
  return useSyncExternalStore(
    (l) => {
      listeners.add(l)
      return () => listeners.delete(l)
    },
    () => state,
  )
}

export const ACTIVE_PHASES = new Set(['starting', 'building', 'up', 'warmup', 'window', 'draining', 'collecting'])
export const isActive = (r: RunRecord) => ACTIVE_PHASES.has(r.phase)
