import * as React from 'react'
import { cva, type VariantProps } from 'class-variance-authority'
import { cn } from '@/lib/utils'

const badgeVariants = cva(
  'inline-flex items-center gap-1 rounded-sm px-1.5 py-0.5 font-mono text-[11px] font-medium uppercase tracking-wide leading-4 whitespace-nowrap',
  {
    variants: {
      variant: {
        default: 'bg-surface-2 text-foreground hairline',
        muted: 'bg-transparent text-muted hairline',
        accent: 'bg-accent/15 text-accent',
        good: 'bg-good/15 text-good',
        warn: 'bg-warn/15 text-warn',
        bad: 'bg-bad/15 text-bad',
        a: 'bg-a/15 text-a',
        b: 'bg-b/15 text-b',
      },
    },
    defaultVariants: { variant: 'default' },
  },
)

function Badge({ className, variant, ...props }: React.ComponentProps<'span'> & VariantProps<typeof badgeVariants>) {
  return <span className={cn(badgeVariants({ variant }), className)} {...props} />
}

export { Badge, badgeVariants }
