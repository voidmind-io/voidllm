import React from 'react'
import { cn } from '../../lib/utils'

export interface BadgeProps extends React.HTMLAttributes<HTMLSpanElement> {
  children: React.ReactNode
  variant?: 'default' | 'success' | 'warning' | 'error' | 'info' | 'muted'
  icon?: React.ReactNode
}

const baseClasses = 'inline-flex items-center gap-1 rounded-md px-2 py-0.5 text-xs font-medium'

const variantClasses: Record<NonNullable<BadgeProps['variant']>, string> = {
  default: 'bg-accent/15 text-accent',
  success: 'bg-success/15 text-success',
  warning: 'bg-warning/15 text-warning',
  error: 'bg-error/15 text-error',
  info: 'bg-info/15 text-info',
  muted: 'bg-bg-tertiary text-text-tertiary font-mono',
}

export function Badge({ children, variant = 'default', icon, className, ...rest }: BadgeProps) {
  return (
    <span className={cn(baseClasses, variantClasses[variant], className)} {...rest}>
      {icon != null ? <span className="shrink-0">{icon}</span> : null}
      {children}
    </span>
  )
}
