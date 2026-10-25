// One aoc process at a time. The runner spawns ./bin/aoc, keeps the log on
// disk, reads the phase off the log lines aoc prints, and stops a run with
// SIGINT so aoc can take its compose stack down itself.
import { spawn, type ChildProcess } from 'node:child_process'
import { promises as fs, createWriteStream, type WriteStream } from 'node:fs'
import path from 'node:path'
import { EventEmitter } from 'node:events'
import type { Phase, RunRecord, RunRequest, ServerEvent } from '../shared/types.ts'
import { AOC, ROOT, STATE, buildArgv, shellLine } from './repo.ts'

const TAIL = 200
const RUNS_FILE = path.join(STATE, 'runs.json')
const LOG_DIR = path.join(STATE, 'logs')

export class Runner extends EventEmitter {
  runs: RunRecord[] = []
  private child?: ChildProcess
  private active?: RunRecord
  private stream?: WriteStream
  private forceTimer?: NodeJS.Timeout

  async load() {
    await fs.mkdir(LOG_DIR, { recursive: true })
    try {
      const saved = JSON.parse(await fs.readFile(RUNS_FILE, 'utf8')) as RunRecord[]
      // anything still "running" when the server last exited is gone
      for (const r of saved) {
        if (!['finished', 'failed', 'interrupted'].includes(r.phase)) {
          r.phase = 'interrupted'
          r.endedAt = r.endedAt ?? new Date().toISOString()
        }
      }
      this.runs = saved.slice(-100)
    } catch {
      this.runs = []
    }
  }

  private emitEvent(e: ServerEvent) {
    this.emit('event', e)
  }

  private async save() {
    await fs.mkdir(STATE, { recursive: true })
    await fs.writeFile(RUNS_FILE, JSON.stringify(this.runs, null, 1))
  }

  get busy() {
    return !!this.active
  }

  enqueue(req: RunRequest): RunRecord {
    const argv = buildArgv(req)
    const id = `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 6)}`
    const rec: RunRecord = {
      id,
      request: req,
      argv,
      command: shellLine(argv),
      resultsDir: `results/${req.name}`,
      phase: 'queued',
      phaseSince: new Date().toISOString(),
      tail: [],
      logPath: path.relative(ROOT, path.join(LOG_DIR, `${id}.log`)),
    }
    this.runs.push(rec)
    this.emitEvent({ type: 'run', run: rec })
    void this.save()
    this.pump()
    return rec
  }

  private pump() {
    if (this.active) return
    const next = this.runs.find((r) => r.phase === 'queued')
    if (next) this.start(next)
  }

  private setPhase(rec: RunRecord, phase: Phase) {
    if (rec.phase === phase) return
    rec.phase = phase
    rec.phaseSince = new Date().toISOString()
    this.emitEvent({ type: 'run', run: rec })
  }

  private start(rec: RunRecord) {
    this.active = rec
    rec.startedAt = new Date().toISOString()
    this.setPhase(rec, 'starting')
    const logPath = path.join(ROOT, rec.logPath)
    this.stream = createWriteStream(logPath, { flags: 'a' })
    this.stream.write(`$ ${rec.command}\n`)
    const child = spawn(AOC, rec.argv, {
      cwd: ROOT,
      env: { ...process.env, TERM: 'dumb', NO_COLOR: '1' },
      stdio: ['ignore', 'pipe', 'pipe'],
    })
    this.child = child
    rec.pid = child.pid
    let buf = ''
    const onData = (chunk: Buffer) => {
      buf += chunk.toString()
      let i: number
      while ((i = buf.indexOf('\n')) >= 0) {
        const line = buf.slice(0, i).replace(/\r$/, '')
        buf = buf.slice(i + 1)
        this.line(rec, line)
      }
    }
    child.stdout!.on('data', onData)
    child.stderr!.on('data', onData)
    child.on('error', (err) => {
      this.line(rec, `aoc-ui: could not start ${AOC}: ${err.message}`)
    })
    child.on('close', (code, signal) => {
      if (buf) this.line(rec, buf)
      rec.exitCode = code
      rec.endedAt = new Date().toISOString()
      if (this.forceTimer) clearTimeout(this.forceTimer)
      const phase: Phase = rec.stopRequested || code === 130 || signal ? 'interrupted' : code === 0 || code === 3 ? 'finished' : 'failed'
      this.setPhase(rec, phase)
      this.line(rec, `aoc-ui: exit ${code ?? signal} (${phase})`)
      this.stream?.end()
      this.stream = undefined
      this.child = undefined
      this.active = undefined
      void this.save()
      this.pump()
    })
    void this.save()
  }

  private line(rec: RunRecord, line: string) {
    this.stream?.write(line + '\n')
    rec.tail.push(line)
    if (rec.tail.length > TAIL) rec.tail.splice(0, rec.tail.length - TAIL)
    this.emitEvent({ type: 'log', id: rec.id, line })
    this.classify(rec, line)
  }

  // The phase comes from the lines internal/cli/run.go and ab.go print.
  private classify(rec: RunRecord, line: string) {
    const m = (re: RegExp) => re.exec(line)
    let r: RegExpExecArray | null
    if ((r = m(/wall time: about (.+)$/))) rec.wallEstimateSec = parseDuration(r[1])
    if ((r = m(/window=(\S+)/))) rec.windowSec = parseDuration(r[1])
    if ((r = m(/rounds=(\d+)/))) rec.rounds = Number(r[1])
    if ((r = m(/round (\d+)(?: of (\d+))?/i))) {
      rec.round = Number(r[1])
      if (r[2]) rec.rounds = Number(r[2])
    }
    if (/\$ docker compose .*build|aoc build|agent-dev-env|building .*image|Building/i.test(line) && ['starting'].includes(rec.phase)) {
      this.setPhase(rec, 'building')
    } else if (/\$ docker compose .*up |waiting for the agent to report healthy|intake is up at/.test(line)) {
      this.setPhase(rec, 'up')
    } else if (/warm-up /.test(line)) {
      this.setPhase(rec, 'warmup')
    } else if (/measured window open/.test(line)) {
      this.setPhase(rec, 'window')
    } else if (/window closed|draining for/.test(line)) {
      this.setPhase(rec, 'draining')
    } else if (/^\s*\d\d:\d\d:\d\d\s+(\[[^\]]+\]\s+)?(drained|drain ended|profiles:|agent status)/.test(line)) {
      this.setPhase(rec, 'collecting')
    } else if (/pausing .* before the next round/.test(line)) {
      this.setPhase(rec, 'up')
    }
    if (rec.phase !== 'starting') return
    if (/^\s*\d\d:\d\d:\d\d\s+/.test(line) && !/build/i.test(line)) this.setPhase(rec, 'up')
  }

  // SIGINT first: aoc cancels its context and runs compose down. After 90 s
  // without an exit, SIGTERM; after another 30 s, SIGKILL. The caller can
  // escalate sooner with force.
  stop(id: string, force = false): boolean {
    const rec = this.runs.find((r) => r.id === id)
    if (!rec) return false
    if (rec.phase === 'queued') {
      rec.stopRequested = true
      rec.endedAt = new Date().toISOString()
      this.setPhase(rec, 'interrupted')
      void this.save()
      return true
    }
    if (rec !== this.active || !this.child) return false
    rec.stopRequested = true
    this.emitEvent({ type: 'run', run: rec })
    if (force) {
      this.line(rec, 'aoc-ui: SIGKILL (the compose stack may need cleanup)')
      this.child.kill('SIGKILL')
      return true
    }
    this.line(rec, 'aoc-ui: SIGINT, waiting for aoc to take the stack down')
    this.child.kill('SIGINT')
    const child = this.child
    this.forceTimer = setTimeout(() => {
      if (this.child !== child) return
      this.line(rec, 'aoc-ui: still running after 90 s, SIGTERM')
      child.kill('SIGTERM')
      this.forceTimer = setTimeout(() => {
        if (this.child === child) child.kill('SIGKILL')
      }, 30_000)
    }, 90_000)
    return true
  }

  // Server shutdown: interrupt the active run and wait for its exit.
  async shutdown(timeoutMs = 120_000) {
    const active = this.active
    if (!active) return
    this.stop(active.id)
    await new Promise<void>((resolve) => {
      const t = setTimeout(resolve, timeoutMs)
      const check = () => {
        if (!this.active) {
          clearTimeout(t)
          resolve()
        } else setTimeout(check, 250)
      }
      check()
    })
  }

  clearHistory() {
    this.runs = this.runs.filter((r) => r === this.active || r.phase === 'queued')
    void this.save()
  }
}

// "2m30s", "90s", "1h2m" → seconds
export function parseDuration(s: string): number | undefined {
  const re = /(\d+(?:\.\d+)?)\s*(h|m|s|ms)/g
  let total = 0
  let any = false
  let r: RegExpExecArray | null
  while ((r = re.exec(s))) {
    any = true
    const n = Number(r[1])
    total += r[2] === 'h' ? n * 3600 : r[2] === 'm' ? n * 60 : r[2] === 's' ? n : n / 1000
  }
  if (!any && /^\d+$/.test(s.trim())) return Number(s)
  return any ? total : undefined
}
