import type { IntakeSample } from '@shared/types'

// Helpers for reading a sample history at a point in time, steadily.

// The last sample at or before t.
export function at(samples: IntakeSample[], t: number): IntakeSample | undefined {
  let lo = 0
  let hi = samples.length - 1
  let best = -1
  while (lo <= hi) {
    const mid = (lo + hi) >> 1
    if (samples[mid].t <= t) {
      best = mid
      lo = mid + 1
    } else hi = mid - 1
  }
  return best >= 0 ? samples[best] : undefined
}

// Everything at or before t.
export function upTo(samples: IntakeSample[], t: number): IntakeSample[] {
  let n = 0
  while (n < samples.length && samples[n].t <= t) n++
  return samples.slice(0, n)
}

// The sample at t with its per-second numbers replaced by their mean over
// the previous `windowSec` seconds, so a panel can be read without the
// one-second jitter. Counts that only grow (missing, generated, unique,
// bytes missed total) and the component states keep the instant value.
export function smooth(samples: IntakeSample[], t: number, windowSec = 5): IntakeSample | undefined {
  const cur = at(samples, t)
  if (!cur) return undefined
  const win = samples.filter((s) => s.t <= t && s.t > t - windowSec * 1000)
  if (win.length < 2) return cur
  const mean = (f: (s: IntakeSample) => number) => win.reduce((n, s) => n + f(s), 0) / win.length
  return {
    ...cur,
    recentLogsPerSec: mean((s) => s.recentLogsPerSec),
    inflight: mean((s) => s.inflight),
    genRecordsPerSec: mean((s) => s.genRecordsPerSec),
    requestsPerSec: mean((s) => s.requestsPerSec),
    err4xx: mean((s) => s.err4xx),
    err5xx: mean((s) => s.err5xx),
    dropped: mean((s) => s.dropped),
    wireBytesPerSec: mean((s) => s.wireBytesPerSec),
    rawBytesPerSec: mean((s) => s.rawBytesPerSec),
    e2eP50: mean((s) => s.e2eP50),
    e2eP99: mean((s) => s.e2eP99),
    e2eMax: Math.max(...win.map((s) => s.e2eMax)),
    senderP50: mean((s) => s.senderP50),
    senderP99: mean((s) => s.senderP99),
    agentCpuPct: cur.agentCpuPct == null ? undefined : mean((s) => s.agentCpuPct ?? 0),
    agentRssBytes: cur.agentRssBytes == null ? undefined : mean((s) => s.agentRssBytes ?? 0),
    stages: { decoded: mean((s) => s.stages.decoded), processed: mean((s) => s.stages.processed), sent: mean((s) => s.stages.sent) },
    missingPerSec: mean((s) => s.missingPerSec),
    backlog: mean((s) => s.backlog),
    rotationsPerSec: mean((s) => s.rotationsPerSec),
    encodedBytesPerSec: mean((s) => s.encodedBytesPerSec),
    logBytesPerSec: mean((s) => s.logBytesPerSec),
    tailer: { ...cur.tailer, bytesMissedPerSec: mean((s) => s.tailer.bytesMissedPerSec), multilineFlushPerSec: mean((s) => s.tailer.multilineFlushPerSec), singleLineFlushPerSec: mean((s) => s.tailer.singleLineFlushPerSec), rotationSizeMismatchPerSec: mean((s) => s.tailer.rotationSizeMismatchPerSec) },
    sender: {
      retryPerSec: mean((s) => s.sender.retryPerSec),
      networkErrorsPerSec: mean((s) => s.sender.networkErrorsPerSec),
      sendWaitMsPerSec: mean((s) => s.sender.sendWaitMsPerSec),
      inUseMsPerSec: mean((s) => s.sender.inUseMsPerSec),
      idleMsPerSec: mean((s) => s.sender.idleMsPerSec),
      sendOkPerSec: mean((s) => s.sender.sendOkPerSec),
      sendRetryablePerSec: mean((s) => s.sender.sendRetryablePerSec),
    },
  }
}

// A stress score per sample, to find the moment worth looking at first:
// the busiest component, rejected requests, loss at the tailer and e2e
// latency, each normalised, summed.
export function stress(s: IntakeSample): number {
  const busiest = Math.max(0, ...Object.values(s.utilization))
  const rejected = s.requestsPerSec + s.err4xx + s.err5xx + s.dropped > 0 ? (s.err4xx + s.err5xx + s.dropped) / (s.requestsPerSec + s.err4xx + s.err5xx + s.dropped) : 0
  const loss = Math.min(1, s.tailer.bytesMissedPerSec / 1e6)
  const lat = Math.min(1, s.e2eP99 / 10)
  const backlog = Math.min(1, Math.log10(s.backlog + 1) / 6)
  return busiest + rejected + loss * 1.5 + lat + backlog
}

export function peakTime(samples: IntakeSample[]): number | undefined {
  let best: IntakeSample | undefined
  let bestScore = -1
  for (const s of samples) {
    const sc = stress(s)
    if (sc > bestScore) {
      bestScore = sc
      best = s
    }
  }
  return best?.t
}
