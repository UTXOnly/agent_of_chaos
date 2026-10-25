import * as React from 'react'
import { cn } from '@/lib/utils'

function Card({ className, ...props }: React.ComponentProps<'div'>) {
  return <div className={cn('rounded-lg bg-surface hairline text-foreground', className)} {...props} />
}
function CardHeader({ className, ...props }: React.ComponentProps<'div'>) {
  return <div className={cn('flex flex-col gap-1 px-5 pt-4 pb-2', className)} {...props} />
}
function CardTitle({ className, ...props }: React.ComponentProps<'div'>) {
  return <div className={cn('text-sm font-semibold leading-none tracking-tight', className)} {...props} />
}
function CardDescription({ className, ...props }: React.ComponentProps<'div'>) {
  return <div className={cn('text-xs text-muted leading-5', className)} {...props} />
}
function CardContent({ className, ...props }: React.ComponentProps<'div'>) {
  return <div className={cn('px-5 pb-5', className)} {...props} />
}

export { Card, CardHeader, CardTitle, CardDescription, CardContent }
