import { useEffect, useState } from 'react'
import Markdown from 'react-markdown'
import { ExternalLink, RefreshCw } from 'lucide-react'
import type { ResultSummary } from '@shared/types'
import { api } from '@/lib/api'
import { ago, cn, fmtInt, fmtShort } from '@/lib/utils'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'

// What is on disk under results/. The brief is the story; Datadog has the
// charts. This view is a doorway to both, not a replacement for either.
export function Results({ refreshKey }: { refreshKey: number }) {
  const [rows, setRows] = useState<ResultSummary[]>([])
  const [loading, setLoading] = useState(false)
  const [open, setOpen] = useState<ResultSummary | null>(null)
  const load = () => {
    setLoading(true)
    api
      .results()
      .then(setRows)
      .finally(() => setLoading(false))
  }
  useEffect(load, [refreshKey])

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <p className="text-xs text-muted">
          {rows.length} result director{rows.length === 1 ? 'y' : 'ies'} in <code className="font-mono">results/</code>. Datadog is the record; this is the working copy.
        </p>
        <Button size="sm" variant="ghost" onClick={load} disabled={loading}>
          <RefreshCw className={cn('size-3', loading && 'animate-spin')} /> refresh
        </Button>
      </div>
      <div className="grid gap-2">
        {rows.map((r) => (
          <button
            key={r.name}
            onClick={() => setOpen(r)}
            className="text-left rounded-lg bg-surface hairline px-4 py-3 hover:ring-1 hover:ring-border-strong transition-shadow cursor-pointer grid grid-cols-[1fr_auto] gap-3 items-start"
          >
            <div className="min-w-0">
              <div className="flex items-center gap-2 flex-wrap">
                <span className="font-mono text-sm font-medium">{r.name}</span>
                <Badge variant={r.kind === 'ab' ? 'accent' : 'default'}>{r.kind === 'ab' ? 'A/B' : 'run'}</Badge>
                <Verdict r={r} />
                <span className="text-[11px] text-faint font-mono">{ago(r.mtime)}</span>
              </div>
              {r.focus && <p className="text-xs text-muted mt-1 truncate">{r.focus}</p>}
              {r.gateLine && <p className="text-[11px] text-muted/80 mt-1 truncate font-mono">{r.gateLine}</p>}
              {r.delivery && (
                <p className="text-[11px] text-muted mt-1 font-mono">
                  generated {fmtShort(r.delivery.generated)} · unique {fmtShort(r.delivery.unique)} · missing{' '}
                  <span className={r.delivery.missing ? 'text-bad' : ''}>{fmtInt(r.delivery.missing)}</span> · duplicates {fmtInt(r.delivery.duplicates)} · {fmtShort(r.recvLogsPerSec)}/s
                </p>
              )}
            </div>
            {r.signals && r.signals.length > 0 && (
              <div className="hidden md:grid grid-cols-[auto_auto] gap-x-3 gap-y-0.5 font-mono text-[11px] text-right">
                {dedupe(r.signals).map((s) => (
                  <SignalCell key={s.name + s.metric} s={s} />
                ))}
              </div>
            )}
          </button>
        ))}
        {rows.length === 0 && !loading && <p className="text-sm text-faint py-8 text-center">No results yet.</p>}
      </div>
      <ResultDialog r={open} onClose={() => setOpen(null)} />
    </div>
  )
}

function dedupe(signals: NonNullable<ResultSummary['signals']>) {
  const seen = new Set<string>()
  return signals.filter((s) => (seen.has(s.name) ? false : (seen.add(s.name), true)))
}

function SignalCell({ s }: { s: NonNullable<ResultSummary['signals']>[number] }) {
  return (
    <>
      <span className="text-muted">{s.name}</span>
      <span className={cn(s.regressed ? 'text-bad' : 'text-foreground')}>{s.delta}</span>
    </>
  )
}

function Verdict({ r }: { r: ResultSummary }) {
  if (r.gatePass === false) return <Badge variant="bad">gate failed</Badge>
  if (r.verdict) return <Badge variant={r.verdict === 'pass' ? 'good' : r.verdict === 'fail' ? 'bad' : 'warn'}>{r.verdict}</Badge>
  if (r.delivery) return <Badge variant={r.delivery.missing || r.delivery.duplicates ? 'bad' : 'good'}>{r.delivery.missing || r.delivery.duplicates ? 'lost or duplicated' : 'delivered'}</Badge>
  return null
}

function ResultDialog({ r, onClose }: { r: ResultSummary | null; onClose: () => void }) {
  const [file, setFile] = useState('findings.md')
  const [text, setText] = useState('')
  useEffect(() => {
    if (!r) return
    const f = r.hasFindings ? 'findings.md' : 'report.md'
    setFile(f)
  }, [r])
  useEffect(() => {
    if (!r) return
    setText('')
    api.resultFile(r.name, file).then(setText)
  }, [r, file])
  return (
    <Dialog open={!!r} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="w-[min(96vw,64rem)]">
        <DialogHeader>
          <div className="flex items-center gap-3 pr-8">
            <DialogTitle className="font-mono">{r?.name}</DialogTitle>
            {r && <Verdict r={r} />}
            {r?.notebook && (
              <a className="ml-auto inline-flex items-center gap-1 text-xs text-accent hover:underline" href={r.notebook} target="_blank" rel="noreferrer">
                notebook <ExternalLink className="size-3" />
              </a>
            )}
          </div>
          <DialogDescription>{r?.focus ?? 'results/' + r?.name}</DialogDescription>
          <Tabs value={file} onValueChange={setFile} className="mt-2">
            <TabsList>
              {r?.hasFindings && <TabsTrigger value="findings.md">findings</TabsTrigger>}
              {r?.kind === 'ab' && <TabsTrigger value="compare.md">compare</TabsTrigger>}
              {r?.hasReport && <TabsTrigger value="report.md">report</TabsTrigger>}
              <TabsTrigger value="conclusion.md">conclusion</TabsTrigger>
            </TabsList>
          </Tabs>
        </DialogHeader>
        <div className="overflow-auto scrollbar-thin px-6 pb-6 prose-md">
          {text ? <Markdown>{text}</Markdown> : <p className="text-xs text-faint py-6">{file} is not there for this result.</p>}
        </div>
      </DialogContent>
    </Dialog>
  )
}
