import { clsx, type ClassValue } from 'clsx'
import { twMerge } from 'tailwind-merge'

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}

export function fmtInt(n: number | undefined | null): string {
  if (n == null || Number.isNaN(n)) return '–'
  return Math.round(n).toLocaleString('en-US')
}

// 12345 → "12.3k", 1234567 → "1.23M"
export function fmtShort(n: number | undefined | null): string {
  if (n == null || Number.isNaN(n)) return '–'
  const abs = Math.abs(n)
  if (abs >= 1e9) return (n / 1e9).toFixed(2) + 'G'
  if (abs >= 1e6) return (n / 1e6).toFixed(2) + 'M'
  if (abs >= 1e4) return (n / 1e3).toFixed(1) + 'k'
  if (abs >= 1e3) return (n / 1e3).toFixed(2) + 'k'
  return Math.round(n).toString()
}

export function fmtBytes(n: number | undefined | null): string {
  if (n == null || Number.isNaN(n)) return '–'
  const u = ['B', 'KiB', 'MiB', 'GiB']
  let i = 0
  let v = n
  while (v >= 1024 && i < u.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${u[i]}`
}

export function fmtDur(sec: number | undefined | null): string {
  if (sec == null || Number.isNaN(sec)) return '–'
  const s = Math.max(0, Math.round(sec))
  const h = Math.floor(s / 3600)
  const m = Math.floor((s % 3600) / 60)
  const r = s % 60
  if (h) return `${h}h${String(m).padStart(2, '0')}m`
  if (m) return `${m}m${String(r).padStart(2, '0')}s`
  return `${r}s`
}

export function ago(iso: string | undefined): string {
  if (!iso) return ''
  const d = (Date.now() - new Date(iso).getTime()) / 1000
  if (d < 60) return 'just now'
  if (d < 3600) return `${Math.floor(d / 60)} min ago`
  if (d < 86400) return `${Math.floor(d / 3600)} h ago`
  return `${Math.floor(d / 86400)} d ago`
}
