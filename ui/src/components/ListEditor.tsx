import { X } from 'lucide-react'
import { useState } from 'react'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { cn } from '@/lib/utils'

// A list of K=V (or bare) entries as chips, with presets. Enter or comma adds.
export function ListEditor({
  label,
  hint,
  values,
  onChange,
  placeholder,
  presets,
  tone = 'default',
}: {
  label: string
  hint?: string
  values: string[]
  onChange: (v: string[]) => void
  placeholder?: string
  presets?: { label: string; value: string }[]
  tone?: 'default' | 'b' | 'bad'
}) {
  const [draft, setDraft] = useState('')
  const add = (raw: string) => {
    const items = raw
      .split(/\n/)
      .map((s) => s.trim())
      .filter(Boolean)
    if (items.length) onChange([...values, ...items.filter((i) => !values.includes(i))])
    setDraft('')
  }
  return (
    <div className="space-y-1.5">
      <div className="flex items-baseline justify-between">
        <Label>{label}</Label>
        {hint && <span className="text-[11px] text-faint font-mono">{hint}</span>}
      </div>
      <div className="flex flex-wrap gap-1.5 min-h-6">
        {values.map((v) => (
          <span
            key={v}
            className={cn(
              'inline-flex items-center gap-1 rounded-sm pl-2 pr-1 py-0.5 font-mono text-xs hairline',
              tone === 'b' && 'bg-b/10 text-b',
              tone === 'bad' && 'bg-bad/10 text-bad',
              tone === 'default' && 'bg-surface-2',
            )}
          >
            {v}
            <button type="button" className="rounded-sm hover:bg-white/10 p-0.5 cursor-pointer" onClick={() => onChange(values.filter((x) => x !== v))} aria-label={`remove ${v}`}>
              <X className="size-3" />
            </button>
          </span>
        ))}
        {values.length === 0 && <span className="text-xs text-faint self-center">none</span>}
      </div>
      <Input
        value={draft}
        placeholder={placeholder}
        onChange={(e) => setDraft(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter') {
            e.preventDefault()
            add(draft)
          }
          if (e.key === 'Backspace' && !draft && values.length) onChange(values.slice(0, -1))
        }}
        onBlur={() => draft.trim() && add(draft)}
        className="h-8 text-xs"
      />
      {presets && (
        <div className="flex flex-wrap gap-1">
          {presets.map((p) => (
            <button
              key={p.value}
              type="button"
              onClick={() => add(p.value)}
              className="rounded-sm px-1.5 py-0.5 text-[11px] font-mono text-muted hairline hover:text-foreground hover:bg-surface-2 cursor-pointer"
              title={p.value}
            >
              {p.label}
            </button>
          ))}
        </div>
      )}
    </div>
  )
}
