// Reads the repository the UI sits in: profiles, aoc.yaml, results, and the
// aoc argv for a request. Nothing here starts a process except docker ps.
import { promises as fs } from 'node:fs'
import path from 'node:path'
import { execFile } from 'node:child_process'
import { promisify } from 'node:util'
import YAML from 'yaml'
import type { Config, DockerContainer, ProfileSummary, ResultSummary, RunRequest } from '../shared/types.ts'

const execFileP = promisify(execFile)

export const ROOT = path.resolve(process.env.AOC_ROOT ?? path.join(import.meta.dirname, '..', '..'))
export const AOC = path.join(ROOT, 'bin', 'aoc')
export const STATE = path.join(ROOT, 'ui', '.state')

async function exists(p: string) {
  return fs.access(p).then(() => true, () => false)
}

async function readJSON<T>(p: string): Promise<T | undefined> {
  try {
    return JSON.parse(await fs.readFile(p, 'utf8')) as T
  } catch {
    return undefined
  }
}

export async function listProfiles(): Promise<ProfileSummary[]> {
  const dir = path.join(ROOT, 'profiles')
  const names = (await fs.readdir(dir)).filter((f) => f.endsWith('.yaml')).sort()
  const out: ProfileSummary[] = []
  for (const f of names) {
    try {
      const doc = YAML.parse(await fs.readFile(path.join(dir, f), 'utf8')) ?? {}
      out.push({
        name: doc.name ?? f.replace(/\.yaml$/, ''),
        path: `profiles/${f}`,
        description: doc.description ?? '',
        duration: String(doc.duration ?? '3m'),
        warmup: String(doc.warmup ?? '20s'),
        drain: String(doc.drain ?? '45s'),
        generators: doc.generators ?? {},
        faults: doc.faults ?? [],
        agentEnv: doc.agent?.env ?? {},
      })
    } catch {
      // an invalid profile is the CLI's problem to report; skip it here
    }
  }
  return out
}

export async function readConfig(): Promise<Config> {
  const doc = YAML.parse(await fs.readFile(path.join(ROOT, 'aoc.yaml'), 'utf8').catch(() => '')) ?? {}
  let hasApiKey = !!process.env.DD_API_KEY
  if (!hasApiKey) {
    const env = await fs.readFile(path.join(ROOT, '.env'), 'utf8').catch(() => '')
    hasApiKey = /^\s*(export\s+)?DD_API_KEY\s*=\s*\S/m.test(env)
  }
  return {
    aImage: doc.a?.image ?? 'datadog/agent:7',
    profile: doc.profile ?? 'profiles/baseline.yaml',
    duration: String(doc.duration ?? '10m'),
    runs: Number(doc.runs ?? 2),
    thresholds: doc.thresholds ?? {},
    hasApiKey,
    aocBinary: await exists(AOC),
    dockerOk: await dockerOk(),
  }
}

async function dockerOk() {
  try {
    await execFileP('docker', ['info', '--format', '{{.ServerVersion}}'], { timeout: 5000 })
    return true
  } catch {
    return false
  }
}

const kv = (flag: string, vals: string[]) => vals.filter((v) => v.trim()).flatMap((v) => [flag, v.trim()])

// One request, one argv. Mirrors the flags in internal/cli/run.go and ab.go.
export function buildArgv(r: RunRequest, plan = false): string[] {
  const a: string[] = [r.kind]
  if (r.kind === 'ab') {
    if (r.bSrc) a.push('--b-src', r.bSrc)
    else if (r.bImage) a.push('--b', r.bImage)
    if (r.aSrc) a.push('--a-src', r.aSrc)
    else if (r.aImage) a.push('--a', r.aImage)
    if (r.workload) a.push('--workload', r.workload)
    if (r.focus) a.push('--focus', r.focus)
    if (r.runs && r.runs > 0) a.push('--runs', String(r.runs))
    a.push(...kv('--code', r.code), ...kv('--watch', r.watch), ...kv('--b-env', r.bEnv))
    if (r.sequential) a.push('--sequential')
    if (r.threshold && r.threshold > 0) a.push('--threshold', String(r.threshold))
  } else {
    a.push('--profile', r.workload || 'baseline')
    if (r.agentImage) a.push('--agent-image', r.agentImage)
    if (r.keep) a.push('--keep')
  }
  a.push(...kv('--set', r.set), ...kv('--fault', r.fault), ...kv('--env', r.env))
  if (r.duration) a.push('--duration', r.duration)
  if (r.name) a.push('--name', r.name)
  if (plan) a.push('--plan')
  return a
}

// The inverse of buildArgv, for a command line an agent or a shell composed:
// aoc's argv (run or ab first) back into the request the UI renders. Flags
// the form does not know (--config, --only, --compare-only, --pull, …) are
// run as given; the request only has to say what the Live view shows.
export function parseArgv(argv: string[]): RunRequest | string {
  const [kind, ...rest] = argv
  if (kind !== 'run' && kind !== 'ab') return 'argv must start with run or ab'
  const r: RunRequest = { kind, name: '', workload: '', set: [], fault: [], env: [], bEnv: [], code: [], watch: [] }
  const lists: Record<string, string[]> = { '--set': r.set, '--fault': r.fault, '--env': r.env, '--b-env': r.bEnv, '--code': r.code, '--watch': r.watch }
  const switches = new Set(['--sequential', '--keep', '--plan', '--compare-only', '--no-build', '--notebook', '--event'])
  for (let i = 0; i < rest.length; i++) {
    let flag = rest[i]
    let val: string | undefined
    if (!flag.startsWith('--')) return `unexpected argument ${flag}`
    const eq = flag.indexOf('=')
    if (eq > 0) {
      val = flag.slice(eq + 1)
      flag = flag.slice(0, eq)
    }
    if (switches.has(flag)) {
      if (flag === '--sequential') r.sequential = true
      if (flag === '--keep') r.keep = true
      continue
    }
    if (val === undefined) val = rest[++i]
    if (val === undefined) return `${flag} needs a value`
    if (flag in lists) {
      lists[flag].push(val)
      continue
    }
    switch (flag) {
      case '--name': r.name = val; break
      case '--workload':
      case '--profile': r.workload = val; break
      case '--focus': r.focus = val; break
      case '--duration': r.duration = val; break
      case '--runs': r.runs = Number(val); break
      case '--threshold': r.threshold = Number(val); break
      case '--b': r.bImage = val; break
      case '--b-src': r.bSrc = val; break
      case '--a': r.aImage = val; break
      case '--a-src': r.aSrc = val; break
      case '--agent-image': r.agentImage = val; break
      case '--config': r.config = val; break
      default: // plumbing: aoc gets it, the form does not show it
    }
  }
  return r
}

const profileName = (w: string) => path.basename(w).replace(/\.yaml$/, '')

// Fill the name and workload aoc would default to, where this side can know
// them: results/<name> is where the brief lands, so the record needs it.
export async function completeRequest(r: RunRequest): Promise<RunRequest | string> {
  const cfg = await readConfig()
  if (!r.workload) r.workload = r.kind === 'run' ? 'baseline' : profileName(cfg.profile)
  if (!r.name && r.config) {
    const doc = YAML.parse(await fs.readFile(path.resolve(ROOT, r.config), 'utf8').catch(() => '')) ?? {}
    if (doc.name) r.name = String(doc.name)
  }
  if (!r.name) {
    if (r.kind === 'run') r.name = profileName(r.workload)
    else if (r.bImage && !r.bSrc) r.name = `${r.bImage.split(':').pop()}-${profileName(r.workload)}`
    else return 'add --name: with --b-src the default name depends on the checkout, and results/<name> is where the brief lands'
  }
  if (!/^[\w.-]{1,64}$/.test(r.name)) return 'name: letters, digits, . _ - only'
  return r
}

export function shellLine(argv: string[]): string {
  const q = (s: string) => (/^[\w./:=,@+-]+$/.test(s) ? s : `'${s.replace(/'/g, `'\\''`)}'`)
  return ['./bin/aoc', ...argv.map(q)].join(' ')
}

export async function plan(r: RunRequest): Promise<{ ok: boolean; output: string; argv: string[] }> {
  const argv = buildArgv(r, true)
  try {
    const { stdout, stderr } = await execFileP(AOC, argv, { cwd: ROOT, timeout: 30_000, maxBuffer: 4 << 20 })
    return { ok: true, output: stderr + stdout, argv }
  } catch (e) {
    const err = e as { stdout?: string; stderr?: string; message: string }
    return { ok: false, output: (err.stderr ?? '') + (err.stdout ?? '') || err.message, argv }
  }
}

interface Findings {
  verdict?: string[] | string
  gate?: { pass: boolean; line: string }
  signals?: { name: string; metric: string; a: string; b: string; delta: string; kind: string }[]
  focus?: string | null
}
interface AbJSON {
  experiment: string
  focus?: string
  notebook?: string
  started?: string
  finished?: string
  sides?: { side: string; name: string; image: string; runs?: { lost: number; duplicates: number }[] }[]
}
interface Report {
  delivery?: { generated_records: number; unique: number; missing: number; duplicates: number; ratio: number }
  throughput?: { recv_logs_per_sec: number }
}

// pass | regression | fail from the gate and the signal kinds; the first
// verdict line in findings.json is a sentence, not a label.
function shortVerdict(f: Findings): string {
  if (f.gate && !f.gate.pass) return 'fail'
  const regressed = f.signals?.filter((s) => s.kind === 'regression').map((s) => s.name) ?? []
  if (regressed.length) return 'regression: ' + [...new Set(regressed)].join(', ')
  const first = Array.isArray(f.verdict) ? f.verdict[0] : f.verdict
  if (first && /^(pass|fail|inconclusive)\b/i.test(first)) return first.split(/[\s:]/)[0].toLowerCase()
  return 'pass'
}

export async function listResults(limit = 60): Promise<ResultSummary[]> {
  const dir = path.join(ROOT, 'results')
  const entries = await fs.readdir(dir, { withFileTypes: true }).catch(() => [])
  const dirs = entries.filter((e) => e.isDirectory())
  const out: ResultSummary[] = []
  for (const d of dirs) {
    const p = path.join(dir, d.name)
    const st = await fs.stat(p)
    const [f, ab, rep] = await Promise.all([
      readJSON<Findings>(path.join(p, 'findings.json')),
      readJSON<AbJSON>(path.join(p, 'ab.json')),
      readJSON<Report>(path.join(p, 'report.json')),
    ])
    if (!f && !ab && !rep) continue
    const kind = ab || f ? 'ab' : 'run'
    const notebookURL = ab?.notebook ?? (await fs.readFile(path.join(p, 'notebook.url'), 'utf8').catch(() => '')).trim()
    out.push({
      name: d.name,
      kind,
      mtime: st.mtime.toISOString(),
      focus: ab?.focus ?? f?.focus ?? undefined,
      verdict: f ? shortVerdict(f) : undefined,
      gatePass: f?.gate?.pass,
      gateLine: f?.gate?.line,
      signals: f?.signals?.map((s) => ({ ...s, regressed: s.kind === 'regression' })),
      notebook: notebookURL || undefined,
      sides: ab?.sides?.map((s) => ({
        side: s.side,
        name: s.name,
        image: s.image,
        lost: s.runs?.reduce((n, r) => n + r.lost, 0) ?? 0,
        duplicates: s.runs?.reduce((n, r) => n + r.duplicates, 0) ?? 0,
      })),
      delivery: rep?.delivery
        ? {
            generated: rep.delivery.generated_records,
            unique: rep.delivery.unique,
            missing: rep.delivery.missing,
            duplicates: rep.delivery.duplicates,
            ratio: rep.delivery.ratio,
          }
        : undefined,
      recvLogsPerSec: rep?.throughput?.recv_logs_per_sec,
      hasFindings: await exists(path.join(p, 'findings.md')),
      hasReport: await exists(path.join(p, 'report.md')),
    })
  }
  out.sort((x, y) => y.mtime.localeCompare(x.mtime))
  return out.slice(0, limit)
}

export async function readResultFile(name: string, file: 'findings.md' | 'compare.md' | 'report.md' | 'conclusion.md') {
  if (!/^[\w.-]+$/.test(name)) throw new Error('bad result name')
  return fs.readFile(path.join(ROOT, 'results', name, file), 'utf8')
}

// The aoc stacks on this host: compose projects aoc, aoc-a, aoc-b.
export async function aocContainers(): Promise<DockerContainer[]> {
  try {
    const { stdout } = await execFileP(
      'docker',
      ['ps', '-a', '--format', '{{.Names}}\t{{.Label "com.docker.compose.project"}}\t{{.Status}}\t{{.Image}}'],
      { timeout: 5000 },
    )
    return stdout
      .split('\n')
      .filter(Boolean)
      .map((l) => {
        const [name, project, status, image] = l.split('\t')
        return { name, project, status, image }
      })
      .filter((c) => /^aoc(-[ab])?$/.test(c.project))
  } catch {
    return []
  }
}

// Tear down every aoc compose project: what `aoc` itself does at the end of a
// run, for the case where a run was killed before it could.
export async function cleanupStacks(): Promise<string[]> {
  const notes: string[] = []
  for (const project of ['aoc', 'aoc-a', 'aoc-b']) {
    try {
      await execFileP('docker', ['compose', '-p', project, 'down', '-v', '--remove-orphans'], { cwd: ROOT, timeout: 120_000 })
      notes.push(`${project}: down`)
    } catch (e) {
      notes.push(`${project}: ${(e as Error).message.split('\n')[0]}`)
    }
  }
  return notes
}
