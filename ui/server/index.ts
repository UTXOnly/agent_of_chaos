// aoc-ui server: a thin HTTP layer over ./bin/aoc. Configure a run, start
// it, watch it, stop it; everything else is the CLI's job.
import { serve } from '@hono/node-server'
import { serveStatic } from '@hono/node-server/serve-static'
import { Hono } from 'hono'
import { streamSSE } from 'hono/streaming'
import { promises as fs } from 'node:fs'
import path from 'node:path'
import type { RunRequest, RunSamples, ServerEvent } from '../shared/types.ts'
import { IntakePoller } from './intake.ts'
import { ROOT, STATE, aocContainers, cleanupStacks, completeRequest, listProfiles, listResults, parseArgv, plan, readConfig, readResultFile } from './repo.ts'
import { Runner } from './runner.ts'

const PORT = Number(process.env.AOC_UI_PORT ?? 4100)
const DIST = path.join(import.meta.dirname, '..', 'dist')

const runner = new Runner()
const intake = new IntakePoller()
await runner.load()

type Client = (e: ServerEvent) => void
const clients = new Set<Client>()
const broadcast = (e: ServerEvent) => {
  for (const c of clients) c(e)
}

// Samples of finished runs, kept on disk so a run can be reviewed after it
// ends: ui/.state/samples/<id>.json. The active run's live samples stay in
// the poller until the next run starts.
const SAMPLES_DIR = path.join(STATE, 'samples')
await fs.mkdir(SAMPLES_DIR, { recursive: true })
const samplesPath = (id: string) => path.join(SAMPLES_DIR, `${id.replace(/[^\w.-]/g, '_')}.json`)
async function saveSamples(id: string) {
  const snap = intake.snapshot()
  if (snap.samples.a.length + snap.samples.b.length === 0) return
  await fs.writeFile(samplesPath(id), JSON.stringify(snap)).catch(() => {})
}
async function loadSamples(id: string): Promise<RunSamples | undefined> {
  const live = runner.runs.find((r) => r.id === id)
  if (live && !['finished', 'failed', 'interrupted'].includes(live.phase) && runner.busy) return intake.snapshot()
  try {
    return JSON.parse(await fs.readFile(samplesPath(id), 'utf8')) as RunSamples
  } catch {
    return undefined
  }
}

runner.on('event', (e: ServerEvent) => {
  broadcast(e)
  if (e.type === 'run') {
    if (['finished', 'failed', 'interrupted'].includes(e.run.phase) && e.run.startedAt) void saveSamples(e.run.id)
    intake.setActive(runner.busy)
  }
})
intake.on('sample', (sample) => broadcast({ type: 'intake', sample }))
intake.on('telemetry', (side, series) => broadcast({ type: 'telemetry', side, series }))

// docker ps every 5 s, only while someone is watching
setInterval(async () => {
  if (clients.size === 0) return
  broadcast({ type: 'docker', containers: await aocContainers() })
}, 5000)

const app = new Hono()

app.get('/api/health', (c) => {
  const cur = runner.current
  return c.json({
    ok: true,
    pid: process.pid,
    root: ROOT,
    busy: runner.busy,
    uptime: process.uptime(),
    active: cur ? { id: cur.id, name: cur.request.name, kind: cur.request.kind, phase: cur.phase, phaseSince: cur.phaseSince, resultsDir: cur.resultsDir } : null,
  })
})
app.get('/api/config', async (c) => c.json(await readConfig()))
app.get('/api/profiles', async (c) => c.json(await listProfiles()))
app.get('/api/results', async (c) => c.json(await listResults()))
app.get('/api/results/:name/:file', async (c) => {
  const file = c.req.param('file') as 'findings.md' | 'compare.md' | 'report.md' | 'conclusion.md'
  if (!['findings.md', 'compare.md', 'report.md', 'conclusion.md'].includes(file)) return c.text('not found', 404)
  try {
    return c.text(await readResultFile(c.req.param('name'), file))
  } catch {
    return c.text('not found', 404)
  }
})
app.get('/api/docker', async (c) => c.json(await aocContainers()))
app.post('/api/docker/cleanup', async (c) => {
  if (runner.busy) return c.json({ error: 'a run is active; stop it first' }, 409)
  return c.json({ notes: await cleanupStacks() })
})

function validate(body: unknown): RunRequest | string {
  const r = body as Partial<RunRequest>
  if (r.kind !== 'run' && r.kind !== 'ab') return 'kind must be run or ab'
  if (!r.name || !/^[\w.-]{1,64}$/.test(r.name)) return 'name: letters, digits, . _ - only'
  if (r.kind === 'ab' && !r.bImage && !r.bSrc) return 'ab needs an image under test (--b) or a checkout ref (--b-src)'
  const list = (v: unknown) => (Array.isArray(v) ? v.map(String) : [])
  return {
    kind: r.kind,
    name: r.name,
    workload: String(r.workload ?? ''),
    focus: r.focus ? String(r.focus) : undefined,
    duration: r.duration ? String(r.duration) : undefined,
    runs: r.runs ? Number(r.runs) : undefined,
    set: list(r.set),
    fault: list(r.fault),
    env: list(r.env),
    bEnv: list(r.bEnv),
    code: list(r.code),
    watch: list(r.watch),
    bImage: r.bImage || undefined,
    bSrc: r.bSrc || undefined,
    aImage: r.aImage || undefined,
    aSrc: r.aSrc || undefined,
    agentImage: r.agentImage || undefined,
    sequential: !!r.sequential,
    keep: !!r.keep,
    threshold: r.threshold ? Number(r.threshold) : undefined,
  }
}

app.post('/api/plan', async (c) => {
  const v = validate(await c.req.json())
  if (typeof v === 'string') return c.json({ error: v }, 400)
  return c.json(await plan(v))
})

app.get('/api/runs', (c) => c.json(runner.runs))
// Two bodies: the form's RunRequest, or {argv: ["ab", "--b-src", ".", …]}
// for a command line composed by an agent or a shell (ui/bin/aoc-ui run).
app.post('/api/runs', async (c) => {
  const body = (await c.req.json()) as { argv?: unknown }
  let argv: string[] | undefined
  let v: RunRequest | string
  if (Array.isArray(body.argv)) {
    argv = body.argv.map(String)
    v = parseArgv(argv)
    if (typeof v !== 'string') v = await completeRequest(v)
  } else {
    v = validate(body)
  }
  if (typeof v === 'string') return c.json({ error: v }, 400)
  const cfg = await readConfig()
  if (!cfg.aocBinary) return c.json({ error: 'bin/aoc is missing: run `make build` in the repo root' }, 412)
  return c.json(runner.enqueue(v, argv), 201)
})
app.get('/api/runs/:id', (c) => {
  const rec = runner.runs.find((r) => r.id === c.req.param('id'))
  return rec ? c.json(rec) : c.json({ error: 'not found' }, 404)
})
// Long poll: returns the record once it leaves ?phase=, ends, or after
// ?timeout= seconds (default 60, max 120). `done` says whether it is over.
app.get('/api/runs/:id/wait', async (c) => {
  const timeout = Math.min(120, Math.max(1, Number(c.req.query('timeout') ?? 60))) * 1000
  const rec = await runner.waitFor(c.req.param('id'), c.req.query('phase') || undefined, timeout)
  return rec ? c.json({ ...rec, done: Runner.done(rec) }) : c.json({ error: 'not found' }, 404)
})
app.post('/api/runs/:id/stop', (c) => {
  const force = c.req.query('force') === '1'
  return runner.stop(c.req.param('id'), force) ? c.json({ ok: true }) : c.json({ error: 'not running' }, 404)
})
app.get('/api/runs/:id/log', async (c) => {
  const rec = runner.runs.find((r) => r.id === c.req.param('id'))
  if (!rec) return c.text('not found', 404)
  return c.text(await fs.readFile(path.join(ROOT, rec.logPath), 'utf8').catch(() => rec.tail.join('\n')))
})
app.get('/api/runs/:id/samples', async (c) => {
  const snap = await loadSamples(c.req.param('id'))
  return snap ? c.json(snap) : c.json({ error: 'no samples for this run' }, 404)
})
app.delete('/api/runs', (c) => {
  runner.clearHistory()
  return c.json({ ok: true })
})

app.get('/api/events', (c) =>
  streamSSE(c, async (stream) => {
    const send = (e: ServerEvent) => {
      void stream.writeSSE({ data: JSON.stringify(e) })
    }
    clients.add(send)
    send({ type: 'hello', runs: runner.runs, samples: intake.recent(), telemetry: intake.telemetry })
    send({ type: 'docker', containers: await aocContainers() })
    const ping = setInterval(() => void stream.writeSSE({ event: 'ping', data: '' }), 15000)
    await new Promise<void>((resolve) => {
      stream.onAbort(() => resolve())
    })
    clearInterval(ping)
    clients.delete(send)
  }),
)

// For agents and scripts: stop the server over HTTP. The active run is
// interrupted first so aoc takes its stack down.
app.post('/api/shutdown', (c) => {
  setTimeout(() => void shutdown('http'), 50)
  return c.json({ ok: true, busy: runner.busy })
})

app.use('/*', serveStatic({ root: path.relative(process.cwd(), DIST) || '.' }))
app.get('*', async (c) => {
  try {
    return c.html(await fs.readFile(path.join(DIST, 'index.html'), 'utf8'))
  } catch {
    return c.text('aoc-ui: no front end build in ui/dist. Run `pnpm build` in ui/, or `pnpm dev` for the Vite dev server.', 503)
  }
})

let stopping = false
async function shutdown(reason: string) {
  if (stopping) return
  stopping = true
  console.log(`aoc-ui: shutting down (${reason})${runner.busy ? ', interrupting the active run' : ''}`)
  await runner.shutdown()
  await fs.rm(path.join(STATE, 'aoc-ui.pid'), { force: true })
  process.exit(0)
}
process.on('SIGINT', () => void shutdown('SIGINT'))
process.on('SIGTERM', () => void shutdown('SIGTERM'))

await fs.mkdir(STATE, { recursive: true })
await fs.writeFile(path.join(STATE, 'aoc-ui.pid'), String(process.pid))
serve({ fetch: app.fetch, port: PORT, hostname: '127.0.0.1' }, (info) => {
  console.log(`aoc-ui: http://localhost:${info.port}  (repo ${ROOT}, pid ${process.pid})`)
})
