import { useEffect, useState } from 'react'
import { Activity, Boxes, Power, SlidersHorizontal, Trash2 } from 'lucide-react'
import { api, isActive, useLive } from '@/lib/api'
import { cn } from '@/lib/utils'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import { Compose } from '@/views/Compose'
import { Live } from '@/views/Live'
import { Results } from '@/views/Results'

type View = 'compose' | 'live' | 'results'

export default function App() {
  const live = useLive()
  const [view, setView] = useState<View>(() => (location.hash.replace('#', '') as View) || 'compose')
  const [selected, setSelected] = useState<string>()
  const [resultsKey, setResultsKey] = useState(0)
  const [cleanupNote, setCleanupNote] = useState<string>()
  const active = live.runs.find(isActive)
  const queued = live.runs.filter((r) => r.phase === 'queued').length

  useEffect(() => {
    if (location.hash !== '#' + view) location.hash = view
  }, [view])
  useEffect(() => {
    const onHash = () => {
      const v = location.hash.replace('#', '') as View
      if (v === 'compose' || v === 'live' || v === 'results') setView(v)
    }
    addEventListener('hashchange', onHash)
    return () => removeEventListener('hashchange', onHash)
  }, [])
  // when a run ends, the results list is stale
  useEffect(() => {
    if (!active) setResultsKey((k) => k + 1)
  }, [active?.id])

  const go = (v: View) => setView(v)

  return (
    <div className="min-h-dvh">
      <header className="sticky top-0 z-40 backdrop-blur bg-background/80 border-b border-border">
        <div className="mx-auto max-w-[1400px] px-5 h-14 flex items-center gap-6">
          <div className="flex items-center gap-2.5">
            <Mark />
            <div className="leading-none">
              <div className="font-semibold tracking-tight">agent of chaos</div>
              <div className="eyebrow !text-[9px] mt-1">logs pipeline test bench</div>
            </div>
          </div>
          <nav className="flex items-center gap-1 ml-4">
            <NavButton on={view === 'compose'} onClick={() => go('compose')} icon={<SlidersHorizontal />}>
              Compose
            </NavButton>
            <NavButton on={view === 'live'} onClick={() => go('live')} icon={<Activity />}>
              Live
              {active && <span className="live-dot ml-1 inline-block size-1.5 rounded-full bg-accent" />}
              {queued > 0 && <span className="ml-1 text-[10px] text-muted">+{queued}</span>}
            </NavButton>
            <NavButton on={view === 'results'} onClick={() => go('results')} icon={<Boxes />}>
              Results
            </NavButton>
          </nav>
          <div className="ml-auto flex items-center gap-2">
            {active && (
              <button onClick={() => go('live')} className="hidden md:flex items-center gap-2 font-mono text-xs text-muted hover:text-foreground cursor-pointer">
                <span className="text-accent">{active.phase}</span>
                <span className="truncate max-w-[24ch]">{active.request.name}</span>
              </button>
            )}
            <Stacks containers={live.containers} busy={!!active} onCleanup={async () => setCleanupNote((await api.cleanup()).notes.join(' · '))} />
            <Tooltip>
              <TooltipTrigger asChild>
                <span className={cn('inline-block size-2 rounded-full', live.connected ? 'bg-good' : 'bg-bad')} />
              </TooltipTrigger>
              <TooltipContent>{live.connected ? 'connected to the aoc-ui server' : 'reconnecting…'}</TooltipContent>
            </Tooltip>
            <Tooltip>
              <TooltipTrigger asChild>
                <Button
                  size="icon"
                  variant="ghost"
                  onClick={() => {
                    if (confirm(active ? 'Stop the server? The active run is interrupted first and aoc takes its stack down.' : 'Stop the aoc-ui server?')) void api.shutdown()
                  }}
                >
                  <Power className="size-4 text-muted" />
                </Button>
              </TooltipTrigger>
              <TooltipContent>Stop the server (same as ui/bin/aoc-ui stop)</TooltipContent>
            </Tooltip>
          </div>
        </div>
      </header>

      {cleanupNote && (
        <div className="mx-auto max-w-[1400px] px-5 pt-3">
          <div className="rounded-md bg-surface-2 hairline text-xs font-mono px-3 py-2 flex items-center justify-between">
            <span>{cleanupNote}</span>
            <button className="text-faint hover:text-foreground cursor-pointer" onClick={() => setCleanupNote(undefined)}>
              dismiss
            </button>
          </div>
        </div>
      )}

      <main className="mx-auto max-w-[1400px] px-5 py-6">
        {view === 'compose' && (
          <Compose
            busy={!!active}
            onLaunched={() => {
              setSelected(undefined)
              go('live')
            }}
          />
        )}
        {view === 'live' && <Live live={live} selected={selected} onSelect={setSelected} />}
        {view === 'results' && <Results refreshKey={resultsKey} />}
      </main>
    </div>
  )
}

function NavButton({ on, onClick, icon, children }: { on: boolean; onClick: () => void; icon: React.ReactNode; children: React.ReactNode }) {
  return (
    <button
      onClick={onClick}
      className={cn(
        'inline-flex items-center gap-1.5 h-8 px-3 rounded-md text-sm transition-colors cursor-pointer [&_svg]:size-3.5',
        on ? 'bg-surface-2 text-foreground hairline' : 'text-muted hover:text-foreground',
      )}
    >
      {icon}
      {children}
    </button>
  )
}

function Stacks({ containers, busy, onCleanup }: { containers: { name: string; project: string; status: string }[]; busy: boolean; onCleanup: () => void }) {
  if (containers.length === 0) return null
  const projects = [...new Set(containers.map((c) => c.project))]
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span className="inline-flex items-center gap-1">
          <Badge variant={busy ? 'accent' : 'warn'}>
            <Boxes className="size-3" /> {containers.length} container{containers.length > 1 ? 's' : ''}
          </Badge>
          {!busy && (
            <Button size="sm" variant="ghost" onClick={onCleanup} title="docker compose down for every aoc stack">
              <Trash2 className="size-3" /> clean up
            </Button>
          )}
        </span>
      </TooltipTrigger>
      <TooltipContent className="font-mono text-[11px]">
        {projects.map((p) => (
          <div key={p}>
            <div className="text-muted">{p}</div>
            {containers
              .filter((c) => c.project === p)
              .map((c) => (
                <div key={c.name}>
                  {c.name} <span className="text-faint">{c.status}</span>
                </div>
              ))}
          </div>
        ))}
        {!busy && <div className="mt-1 text-warn">left over from an earlier run</div>}
      </TooltipContent>
    </Tooltip>
  )
}

function Mark() {
  return (
    <svg width="26" height="26" viewBox="0 0 26 26" aria-hidden>
      <rect x="1" y="1" width="24" height="24" rx="5" fill="none" stroke="var(--accent)" strokeWidth="1.5" />
      <path d="M5 17 L10 8 L13 14 L16 10 L21 17" fill="none" stroke="var(--accent)" strokeWidth="2" strokeLinejoin="round" strokeLinecap="round" />
    </svg>
  )
}
