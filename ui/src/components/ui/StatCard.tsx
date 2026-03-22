import React from 'react'
import { cn } from '../../lib/utils'

export interface StatCardProps extends React.HTMLAttributes<HTMLDivElement> {
  label: string
  value: string | number
  icon?: React.ReactNode
  trend?: { value: number; label?: string }
}

function trendPrefix(value: number): string {
  if (value > 0) return '▲'
  if (value < 0) return '▼'
  return '—'
}

function trendColorClass(value: number): string {
  if (value > 0) return 'text-success'
  if (value < 0) return 'text-error'
  return 'text-text-tertiary'
}

export function StatCard({ label, value, icon, trend, className, ...rest }: StatCardProps) {
  return (
    <div
      role="group"
      aria-label={label}
      className={cn('rounded-lg border border-border bg-bg-secondary p-5', className)}
      {...rest}
    >
      {icon != null ? (
        <span className="shrink-0 text-text-tertiary mb-2 block" aria-hidden="true">{icon}</span>
      ) : null}
      <div className="text-2xl font-semibold text-text-primary">{value}</div>
      <div className="text-sm text-text-tertiary mt-1">{label}</div>
      {trend != null ? (
        <div className={cn('text-sm mt-2', trendColorClass(trend.value))}>
          {trendPrefix(trend.value)}{trend.label != null ? ` ${trend.label}` : ` ${Math.abs(trend.value)}`}
        </div>
      ) : null}
    </div>
  )
}
