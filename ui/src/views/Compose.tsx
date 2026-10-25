import { useEffect, useMemo, useState } from 'react'
import { ArrowRight, FlaskConical, GitCompareArrows, Play, Terminal } from 'lucide-react'
import type { Config, ProfileSummary, RunRequest } from '@shared/types'
import { api } from '@/lib/api'
import { cn } from '@/lib/utils'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input, Textarea } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Switch } from '@/components/ui/switch'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { ListEditor } from '@/components/ListEditor'

const FAULT_PRESETS = [
  { label: 'slow intake 300ms', value: 'latency_ms=300,jitter_ms=100' },
  { label: 'drop 5%', value: 'drop_rate=0.05' },
  { label: '429 storm', value: 'error_rate=0.5,error_status=429' },
  { label: '503 50%', value: 'error_rate=0.5,error_status=503' },
  { label: 'outage', value: 'outage=true' },
  { label: 'slow uplink', value: 'read_bps=500000' },
  { label: 'flapping', value: 'drop_rate=0.3,flap_on_ms=5000,flap_off_ms=20000' },
  { label: 'TCP resets', value: 'tcp_reset_rate=0.05' },
  { label: 'TCP refused', value: 'tcp_refuse=true' },
]
const SET_PRESETS = [
  { label: 'rate 20k', value: 'rate=20000' },
  { label: 'flat out', value: 'rate=0' },
  { label: 'json streams 16', value: 'json.streams=16' },
  { label: 'spikes', value: 'mode=spike,spike-jitter=15s' },
  { label: '250 KiB traces', value: 'multiline-frames=3000' },
  { label: '30 replicas', value: 'app.replicas=30' },
]
const ENV_PRESETS = [
  { label: 'profiling 60s', value: 'DD_INTERNAL_PROFILING_PERIOD=60s' },
  { label: 'auto multiline', value: 'DD_LOGS_CONFIG_AUTO_MULTI_LINE_DETECTION=true' },
  { label: 'debug logs', value: 'DD_LOG_LEVEL=debug' },
]

type Draft = Omit<RunRequest, 'kind'> & { kind: 'run' | 'ab'; bMode: 'src' | 'image'; aMode: 'config' | 'src' | 'image'; preset: 'quick' | 'decide' | 'delta' | 'custom' }

function slug(s: string) {
  return s
    .toLowerCase()
    .replace(/[^a-z0-9._-]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 48)
}

const initial = (): Draft => ({
  kind: 'ab',
  name: '',
  workload: 'baseline',
  focus: '',
  duration: '2m',
  runs: 1,
  set: [],
  fault: [],
  env: [],
  bEnv: [],
  code: [],
  watch: [],
  bImage: '',
  bSrc: '.',
  aImage: '',
  aSrc: 'main',
  agentImage: '',
  sequential: false,
  keep: false,
  threshold: undefined,
  bMode: 'src',
  aMode: 'src',
  preset: 'quick',
})

export function Compose({ onLaunched, busy }: { onLaunched: () => void; busy: boolean }) {
  const [cfg, setCfg] = useState<Config>()
  const [profiles, setProfiles] = useState<ProfileSummary[]>([])
  const [d, setD] = useState<Draft>(initial)
  const [plan, setPlan] = useState<{ ok: boolean; output: string } | null>(null)
  const [planning, setPlanning] = useState(false)
  const [err, setErr] = useState<string>()
  const [launching, setLaunching] = useState(false)
  const up = (patch: Partial<Draft>) => setD((x) => ({ ...x, ...patch, preset: 'duration' in patch || 'runs' in patch ? 'custom' : x.preset }))

  useEffect(() => {
    api.config().then(setCfg).catch(() => {})
    api.profiles().then(setProfiles).catch(() => {})
  }, [])

  const profile = profiles.find((p) => p.name === d.workload)
  const autoName = useMemo(() => {
    const base = d.kind === 'ab' ? (d.bMode === 'src' ? slug(d.bSrc === '.' ? 'local' : d.bSrc ?? '') : slug((d.bImage ?? '').split(':').pop() ?? '')) : 'run'
    return [base || d.kind, d.workload, d.fault.length ? 'fault' : '', new Date().toISOString().slice(5, 10).replace('-', '')].filter(Boolean).join('-')
  }, [d.kind, d.bMode, d.bSrc, d.bImage, d.workload, d.fault.length])

  const req = (): RunRequest => ({
    kind: d.kind,
    name: d.name || autoName,
    workload: d.workload,
    focus: d.focus || undefined,
    duration: d.duration || undefined,
    runs: d.kind === 'ab' ? d.runs : undefined,
    set: d.set,
    fault: d.fault,
    env: d.env,
    bEnv: d.kind === 'ab' ? d.bEnv : [],
    code: d.kind === 'ab' ? d.code : [],
    watch: d.kind === 'ab' ? d.watch : [],
    bImage: d.kind === 'ab' && d.bMode === 'image' ? d.bImage : undefined,
    bSrc: d.kind === 'ab' && d.bMode === 'src' ? d.bSrc : undefined,
    aImage: d.kind === 'ab' && d.aMode === 'image' ? d.aImage : undefined,
    aSrc: d.kind === 'ab' && d.aMode === 'src' ? d.aSrc : undefined,
    agentImage: d.kind === 'run' ? d.agentImage || undefined : undefined,
    sequential: d.kind === 'ab' ? d.sequential : undefined,
    keep: d.kind === 'run' ? d.keep : undefined,
    threshold: d.threshold,
  })

  const command = useMemo(() => {
    const r = req()
    const q = (s: string) => (/^[\w./:=,@+-]+$/.test(s) ? s : `'${s}'`)
    const a: string[] = ['./bin/aoc', r.kind]
    if (r.kind === 'ab') {
      if (r.bSrc) a.push('--b-src', q(r.bSrc))
      if (r.bImage) a.push('--b', q(r.bImage))
      if (r.aSrc) a.push('--a-src', q(r.aSrc))
      if (r.aImage) a.push('--a', q(r.aImage))
      a.push('--workload', r.workload)
      if (r.focus) a.push('--focus', q(r.focus))
      if (r.runs) a.push('--runs', String(r.runs))
      for (const c of r.code) a.push('--code', q(c))
      for (const w of r.watch) a.push('--watch', q(w))
      for (const e of r.bEnv) a.push('--b-env', q(e))
      if (r.sequential) a.push('--sequential')
      if (r.threshold) a.push('--threshold', String(r.threshold))
    } else {
      a.push('--profile', r.workload)
      if (r.agentImage) a.push('--agent-image', q(r.agentImage))
      if (r.keep) a.push('--keep')
    }
    for (const s of r.set) a.push('--set', q(s))
    for (const f of r.fault) a.push('--fault', q(f))
    for (const e of r.env) a.push('--env', q(e))
    if (r.duration) a.push('--duration', r.duration)
    a.push('--name', r.name)
    return a.join(' ')
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [d, autoName])

  const applyPreset = (p: Draft['preset']) => {
    if (p === 'quick') up({ duration: '2m', runs: 1 })
    else if (p === 'decide') up({ duration: cfg?.duration ?? '10m', runs: cfg?.runs ?? 2 })
    else if (p === 'delta') up({ duration: cfg?.duration ?? '10m', runs: 3 })
    setD((x) => ({ ...x, preset: p }))
  }

  const doPlan = async () => {
    setPlanning(true)
    setErr(undefined)
    try {
      const r = await api.plan(req())
      setPlan(r)
    } catch (e) {
      setErr((e as Error).message)
    } finally {
      setPlanning(false)
    }
  }
  const doLaunch = async () => {
    setLaunching(true)
    setErr(undefined)
    try {
      await api.launch(req())
      onLaunched()
    } catch (e) {
      setErr((e as Error).message)
    } finally {
      setLaunching(false)
    }
  }

  const ready = d.kind === 'run' || (d.bMode === 'src' ? !!d.bSrc : !!d.bImage)

  return (
    <div className="grid grid-cols-1 xl:grid-cols-[1fr_380px] gap-5">
      <div className="space-y-5">
        {/* kind */}
        <div className="grid grid-cols-2 gap-3">
          {(
            [
              { k: 'ab', icon: GitCompareArrows, title: 'A/B test', text: 'Two builds, same workload, a verdict on delivery, throughput, saturation, CPU and memory.' },
              { k: 'run', icon: FlaskConical, title: 'Experiment', text: 'One agent in a situation: a workload, a fault, a knob. What it lost, duplicated or slowed down.' },
            ] as const
          ).map(({ k, icon: Icon, title, text }) => (
            <button
              key={k}
              type="button"
              onClick={() => setD((x) => ({ ...x, kind: k }))}
              className={cn(
                'text-left rounded-lg p-4 hairline transition-colors cursor-pointer',
                d.kind === k ? 'bg-surface ring-1 ring-accent/70' : 'bg-transparent hover:bg-surface/60',
              )}
            >
              <div className="flex items-center gap-2 mb-1.5">
                <Icon className={cn('size-4', d.kind === k ? 'text-accent' : 'text-muted')} />
                <span className="font-semibold text-sm">{title}</span>
              </div>
              <p className="text-xs text-muted leading-5">{text}</p>
            </button>
          ))}
        </div>

        {/* builds */}
        {d.kind === 'ab' ? (
          <Card>
            <CardHeader>
              <CardTitle>Sides</CardTitle>
              <CardDescription>Only the thing you changed should differ between a and b.</CardDescription>
            </CardHeader>
            <CardContent className="grid md:grid-cols-2 gap-5">
              <div className="space-y-2 rounded-md p-3 bg-b/[0.04] hairline">
                <div className="flex items-center justify-between">
                  <Label className="!text-b">b · under test</Label>
                  <Seg value={d.bMode} onChange={(v) => up({ bMode: v as Draft['bMode'] })} options={[{ v: 'src', l: 'checkout' }, { v: 'image', l: 'image' }]} />
                </div>
                {d.bMode === 'src' ? (
                  <>
                    <Input value={d.bSrc} onChange={(e) => up({ bSrc: e.target.value })} placeholder=". or a git ref" />
                    <p className="text-[11px] text-muted leading-4">
                      <code className="text-foreground">.</code> builds the agent checkout as it is, uncommitted changes included. About a minute after the first build.
                    </p>
                  </>
                ) : (
                  <Input value={d.bImage} onChange={(e) => up({ bImage: e.target.value })} placeholder="datadog/agent-dev:my-branch-py3" />
                )}
                <ListEditor label="b-env" hint="only b gets these" values={d.bEnv} onChange={(v) => up({ bEnv: v })} placeholder="DD_LOGS_CONFIG_MY_FLAG=true" tone="b" />
              </div>
              <div className="space-y-2 rounded-md p-3 bg-a/[0.04] hairline">
                <div className="flex items-center justify-between">
                  <Label className="!text-a">a · control</Label>
                  <Seg
                    value={d.aMode}
                    onChange={(v) => up({ aMode: v as Draft['aMode'] })}
                    options={[{ v: 'src', l: 'checkout' }, { v: 'image', l: 'image' }, { v: 'config', l: 'release' }]}
                  />
                </div>
                {d.aMode === 'src' && <Input value={d.aSrc} onChange={(e) => up({ aSrc: e.target.value })} placeholder="main" />}
                {d.aMode === 'image' && <Input value={d.aImage} onChange={(e) => up({ aImage: e.target.value })} placeholder="datadog/agent:7" />}
                {d.aMode === 'config' && (
                  <div className="h-9 flex items-center font-mono text-sm text-muted">
                    {cfg?.aImage ?? 'datadog/agent:7'} <span className="ml-2 text-[11px] text-faint">from aoc.yaml</span>
                  </div>
                )}
                <p className="text-[11px] text-muted leading-4">
                  {d.aMode === 'src' ? 'Built the same way as b, so only the branch differs.' : d.aMode === 'image' ? 'A pulled image.' : 'The latest 7.x release, re-pulled before every run.'}
                </p>
              </div>
            </CardContent>
          </Card>
        ) : (
          <Card>
            <CardHeader>
              <CardTitle>Agent</CardTitle>
              <CardDescription>Leave empty for the workload's image, else the compose default.</CardDescription>
            </CardHeader>
            <CardContent>
              <Input value={d.agentImage} onChange={(e) => up({ agentImage: e.target.value })} placeholder="datadog/agent:7 or aoc/agent-dev:my-branch" />
            </CardContent>
          </Card>
        )}

        {/* workload */}
        <Card>
          <CardHeader>
            <div className="flex items-baseline justify-between">
              <CardTitle>Workload</CardTitle>
              <span className="text-[11px] font-mono text-faint">{profiles.length} profiles</span>
            </div>
            <CardDescription>Pick the closest shape, then change any setting without touching a file.</CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            <div className="grid grid-cols-2 md:grid-cols-3 lg:grid-cols-4 gap-2">
              {profiles.map((p) => (
                <button
                  key={p.name}
                  type="button"
                  onClick={() => up({ workload: p.name })}
                  className={cn(
                    'text-left rounded-md px-3 py-2 hairline transition-colors cursor-pointer min-h-[60px]',
                    d.workload === p.name ? 'bg-accent/10 ring-1 ring-accent/70' : 'hover:bg-surface-2',
                  )}
                >
                  <div className="font-mono text-xs font-medium truncate">{p.name}</div>
                  <div className="text-[11px] text-muted mt-0.5 font-mono">
                    {p.duration} · {Object.keys(p.generators).length} gen{p.faults.length ? ` · ${p.faults.length} fault step${p.faults.length > 1 ? 's' : ''}` : ''}
                  </div>
                </button>
              ))}
            </div>
            {profile && <p className="text-xs text-muted leading-5 border-l-2 border-accent/50 pl-3">{profile.description}</p>}
            <div className="grid md:grid-cols-2 gap-5">
              <ListEditor label="set" hint="generator settings" values={d.set} onChange={(v) => up({ set: v })} placeholder="rate=20000 or json.streams=16" presets={SET_PRESETS} />
              <ListEditor label="fault" hint="the intake, whole window" values={d.fault} onChange={(v) => up({ fault: v })} placeholder="latency_ms=300" presets={FAULT_PRESETS} tone="bad" />
            </div>
          </CardContent>
        </Card>

        {/* window & agent env */}
        <Card>
          <CardHeader>
            <CardTitle>Window</CardTitle>
            <CardDescription>A quick look is 2 minutes and one round. A decision is the config's window and rounds. A delta on its threshold is three rounds.</CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            <div className="flex flex-wrap items-end gap-4">
              <Seg
                value={d.preset}
                onChange={(v) => applyPreset(v as Draft['preset'])}
                options={[
                  { v: 'quick', l: 'quick look · 2m ×1' },
                  { v: 'decide', l: `decision · ${cfg?.duration ?? '10m'} ×${cfg?.runs ?? 2}` },
                  { v: 'delta', l: `delta · ${cfg?.duration ?? '10m'} ×3` },
                  { v: 'custom', l: 'custom' },
                ]}
              />
              <div className="space-y-1.5 w-28">
                <Label>duration</Label>
                <Input value={d.duration} onChange={(e) => up({ duration: e.target.value })} placeholder={profile?.duration} />
              </div>
              {d.kind === 'ab' && (
                <div className="space-y-1.5 w-24">
                  <Label>rounds</Label>
                  <Input type="number" min={1} max={9} value={d.runs ?? 1} onChange={(e) => up({ runs: Number(e.target.value) })} />
                </div>
              )}
              {d.kind === 'ab' && (
                <div className="space-y-1.5 w-28">
                  <Label>threshold %</Label>
                  <Input type="number" min={0} step={1} value={d.threshold ?? ''} onChange={(e) => up({ threshold: e.target.value ? Number(e.target.value) : undefined })} placeholder="per signal" />
                </div>
              )}
              {d.kind === 'ab' ? (
                <label className="flex items-center gap-2 h-9 text-xs text-muted">
                  <Switch checked={d.sequential} onCheckedChange={(v) => up({ sequential: v })} /> sequential sides
                </label>
              ) : (
                <label className="flex items-center gap-2 h-9 text-xs text-muted">
                  <Switch checked={d.keep} onCheckedChange={(v) => up({ keep: v })} /> keep the stack up afterwards
                </label>
              )}
            </div>
            <div className="grid md:grid-cols-2 gap-5">
              <ListEditor label="env" hint={d.kind === 'ab' ? 'both sides' : 'the agent'} values={d.env} onChange={(v) => up({ env: v })} placeholder="DD_LOG_LEVEL=debug" presets={ENV_PRESETS} />
              {d.kind === 'ab' && (
                <div className="grid grid-cols-2 gap-3">
                  <ListEditor label="code" hint="packages under test" values={d.code} onChange={(v) => up({ code: v })} placeholder="pkg/logs/sender" />
                  <ListEditor label="watch" hint="promote a metric" values={d.watch} onChange={(v) => up({ watch: v })} placeholder="tag bytes per log" />
                </div>
              )}
            </div>
          </CardContent>
        </Card>
      </div>

      {/* right rail: the question, the command, launch */}
      <div className="space-y-4 xl:sticky xl:top-20 self-start">
        <Card>
          <CardHeader>
            <CardTitle>The question</CardTitle>
            <CardDescription>The focus opens the brief and names the notebook. One sentence.</CardDescription>
          </CardHeader>
          <CardContent className="space-y-3">
            <Textarea rows={3} value={d.focus} onChange={(e) => up({ focus: e.target.value })} placeholder="Does the new batching path hold throughput under a slow intake?" />
            <div className="space-y-1.5">
              <Label>name</Label>
              <Input value={d.name} onChange={(e) => up({ name: slug(e.target.value) })} placeholder={autoName} />
              <p className="text-[11px] text-faint font-mono">results/{d.name || autoName}/</p>
            </div>
          </CardContent>
        </Card>

        <Card className="relative overflow-hidden">
          <CardHeader>
            <div className="flex items-center gap-2">
              <Terminal className="size-3.5 text-muted" />
              <CardTitle>Command</CardTitle>
            </div>
            <CardDescription>What the server will run from the repo root. Paste it into a shell to run it by hand.</CardDescription>
          </CardHeader>
          <CardContent>
            <pre className="font-mono text-[12px] leading-5 whitespace-pre-wrap break-all text-foreground/90 bg-background/60 rounded-md p-3 hairline select-all">{command}</pre>
          </CardContent>
        </Card>

        {cfg && (
          <div className="flex flex-wrap gap-1.5">
            <Badge variant={cfg.dockerOk ? 'good' : 'bad'}>docker {cfg.dockerOk ? 'ok' : 'down'}</Badge>
            <Badge variant={cfg.aocBinary ? 'good' : 'bad'}>bin/aoc {cfg.aocBinary ? 'built' : 'missing'}</Badge>
            <Badge variant={cfg.hasApiKey ? 'good' : 'warn'}>{cfg.hasApiKey ? 'DD_API_KEY set' : 'no DD_API_KEY: local only'}</Badge>
          </div>
        )}

        {err && <div className="rounded-md bg-bad/10 text-bad text-xs px-3 py-2 hairline font-mono">{err}</div>}

        <div className="flex gap-2">
          <Button variant="secondary" className="flex-1" onClick={doPlan} disabled={planning || !ready}>
            {planning ? 'planning…' : 'Plan'}
          </Button>
          <Button className="flex-[2]" size="lg" onClick={doLaunch} disabled={launching || !ready}>
            <Play className="size-4" />
            {busy ? 'Queue' : 'Launch'}
            <ArrowRight className="size-4" />
          </Button>
        </div>
        {busy && <p className="text-[11px] text-muted text-center">A run is active. This one starts when it ends; the host carries one at a time.</p>}
      </div>

      <Dialog open={!!plan} onOpenChange={(o) => !o && setPlan(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Plan</DialogTitle>
            <DialogDescription>The resolved workload, the wall time and any warning, without starting anything.</DialogDescription>
          </DialogHeader>
          <pre className={cn('font-mono text-[12px] leading-5 whitespace-pre-wrap p-5 overflow-auto scrollbar-thin', !plan?.ok && 'text-bad')}>{plan?.output}</pre>
          <div className="px-5 pb-4 flex justify-end gap-2 border-t border-border pt-3">
            <Button variant="ghost" onClick={() => setPlan(null)}>
              Back
            </Button>
            <Button
              onClick={() => {
                setPlan(null)
                void doLaunch()
              }}
              disabled={!plan?.ok}
            >
              <Play className="size-4" /> Launch this
            </Button>
          </div>
        </DialogContent>
      </Dialog>
    </div>
  )
}

function Seg({ value, onChange, options }: { value: string; onChange: (v: string) => void; options: { v: string; l: string }[] }) {
  return (
    <div className="inline-flex items-center gap-0.5 rounded-md bg-surface-2 p-0.5 hairline h-9">
      {options.map((o) => (
        <button
          key={o.v}
          type="button"
          onClick={() => onChange(o.v)}
          className={cn(
            'h-full rounded-sm px-2.5 text-[11px] font-mono transition-colors cursor-pointer',
            value === o.v ? 'bg-background text-foreground shadow-sm' : 'text-muted hover:text-foreground',
          )}
        >
          {o.l}
        </button>
      ))}
    </div>
  )
}
