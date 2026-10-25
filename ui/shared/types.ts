// Types shared by the server and the browser. The server is the only writer
// of RunRecord; the browser only renders what arrives over /api/events.

export type RunKind = 'run' | 'ab'

// What the form collects. Every field maps to one aoc flag; the server turns
// it into argv and never interprets the values itself.
export interface RunRequest {
  kind: RunKind
  name: string
  workload: string // profile name or path
  focus?: string
  duration?: string // "2m", empty = profile/config default
  runs?: number // ab only: rounds per side
  set: string[] // K=V
  fault: string[] // K=V
  env: string[] // K=V both sides
  bEnv: string[] // K=V side b only (ab)
  code: string[] // ab: packages under test
  watch: string[] // ab: metrics promoted to the headline
  // the image under test: one of these
  bImage?: string
  bSrc?: string
  // the control: one of these, or empty for the config's a.image
  aImage?: string
  aSrc?: string
  agentImage?: string // run only
  sequential?: boolean
  keep?: boolean
  threshold?: number
  config?: string // a saved results/<name>/aoc.yaml (--config), command-line launches only
}

export type Phase =
  | 'queued'
  | 'starting'
  | 'building'
  | 'up'
  | 'warmup'
  | 'window'
  | 'draining'
  | 'collecting'
  | 'finished'
  | 'failed'
  | 'interrupted'

export interface RunRecord {
  id: string
  request: RunRequest
  argv: string[]
  command: string // the one line to paste into a shell
  resultsDir: string // results/<name>
  phase: Phase
  phaseSince: string // ISO
  startedAt?: string
  endedAt?: string
  exitCode?: number | null
  pid?: number
  wallEstimateSec?: number // from the plan line
  windowSec?: number
  // the last log lines; the full log is on disk
  tail: string[]
  logPath: string
  round?: number
  rounds?: number
  stopRequested?: boolean
}

// One sample of the intake's /harness/status for one side.
export interface IntakeSample {
  side: 'a' | 'b'
  t: number // epoch ms
  recentLogsPerSec: number
  missing: number
  unique: number
  generated: number
  inflight: number
  faultsActive: boolean
  faultNote?: string
  windowSeconds: number
  generators: { name: string; phase: string; recordRate: number; targetRate: number; final: boolean }[]
  agentCpuPct?: number
  agentRssBytes?: number
  // the intake's per-second point for this second (internal/intake/stats.go Point)
  genRecordsPerSec: number
  requestsPerSec: number
  err4xx: number // responses the intake answered with a 4xx this second (faults)
  err5xx: number
  dropped: number // connections the intake dropped this second
  wireBytesPerSec: number
  rawBytesPerSec: number
  e2eP50: number // seconds
  e2eP99: number
  e2eMax: number
  senderP50: number
  senderP99: number
  // the agent's own telemetry, digested (internal/intake/observe.go series)
  telemetryOk: boolean
  stages: { decoded: number; processed: number; sent: number } // logs per second
  utilization: Record<string, number> // component → busiest instance's ratio 0..1
  httpResp: Record<string, number> // status code → responses per second, aoc intake only
  flushes: { maxCount: number; timer: number } // payload flushes per second by reason
  rotationsPerSec: number
  goroutines?: number
  heapInUse?: number
  gcPerSec?: number
  encodedBytesPerSec: number
  logBytesPerSec: number // bytes_sent (before compression)
  backlog: number // decoded − sent since the window opened, records the agent still holds
  missingPerSec: number // change of the intake's missing count since the previous sample
  // logs_component_utilization__{ratio,items,bytes} per (name, instance)
  components: ComponentStat[]
  tailer: {
    bytesMissedPerSec: number // logs__bytes_missed: rotated away before the tailer read them
    bytesMissedTotal: number
    rotationSizeMismatchPerSec: number
    multilineFlushPerSec: number
    singleLineFlushPerSec: number
  }
  sender: {
    retryPerSec: number // logs__retry_count
    networkErrorsPerSec: number // logs__network_errors
    sendWaitMsPerSec: number // logs_sender__send_wait: ms spent waiting for a worker, per second
    inUseMsPerSec: number // logs_client_http_destination__in_use_ms (logs destination)
    idleMsPerSec: number
    sendOkPerSec: number // logs_client_http_destination__send error=none, the aoc intake
    sendRetryablePerSec: number // error=retryable
  }
}

export interface ComponentStat {
  name: string // processor | strategy | sender | worker | destination_reliable_0 | …
  instance: string // pipeline index, or q0s0 for the shared sender side
  ratio: number // 0..1 busy
  items: number // buffered items
  bytes: number // buffered bytes
}

// What the UI keeps of a run once it is over: every sample per side and the
// last telemetry scrape, so the live panels can be scrubbed afterwards.
export interface RunSamples {
  samples: { a: IntakeSample[]; b: IntakeSample[] }
  telemetry: Partial<Record<'a' | 'b', TelemetrySeries[]>>
}

// One of the agent's telemetry series as the intake's observer scraped it.
export interface TelemetrySeries {
  name: string
  labels: string
  type: string
  value: number
  rate: number
}

export interface ProfileSummary {
  name: string
  path: string
  description: string
  duration: string
  warmup: string
  drain: string
  generators: Record<string, Record<string, unknown>>
  faults: unknown[]
  agentEnv: Record<string, string>
}

export interface Config {
  aImage: string
  profile: string
  duration: string
  runs: number
  thresholds: Record<string, number>
  hasApiKey: boolean
  aocBinary: boolean
  dockerOk: boolean
}

export interface ResultSummary {
  name: string
  kind: RunKind
  mtime: string
  focus?: string
  verdict?: string
  gatePass?: boolean
  gateLine?: string
  signals?: { name: string; metric: string; a: string; b: string; delta: string; regressed: boolean }[]
  notebook?: string
  sides?: { side: string; name: string; image: string; lost: number; duplicates: number }[]
  delivery?: { generated: number; unique: number; missing: number; duplicates: number; ratio: number }
  recvLogsPerSec?: number
  hasFindings: boolean
  hasReport: boolean
}

export interface DockerContainer {
  name: string
  project: string
  status: string
  image: string
}

export type ServerEvent =
  | { type: 'hello'; runs: RunRecord[]; samples: IntakeSample[]; telemetry: Partial<Record<'a' | 'b', TelemetrySeries[]>> }
  | { type: 'run'; run: RunRecord }
  | { type: 'log'; id: string; line: string }
  | { type: 'intake'; sample: IntakeSample }
  | { type: 'docker'; containers: DockerContainer[] }
  | { type: 'telemetry'; side: 'a' | 'b'; series: TelemetrySeries[] }
