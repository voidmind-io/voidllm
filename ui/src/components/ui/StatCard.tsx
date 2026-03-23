import React from 'react'
import { cn } from '../../lib/utils'

// Map of named colors to Tailwind bg/text utility pairs
const iconColorMap: Record<string, { bg: string; text: string }> = {
  purple: { bg: 'bg-accent/10', text: 'text-accent' },
  green:  { bg: 'bg-success/10', text: 'text-success' },
  pink:   { bg: 'bg-pink-500/10', text: 'text-pink-400' },
  blue:   { bg: 'bg-blue-500/10', text: 'text-blue-400' },
  yellow: { bg: 'bg-warning/10', text: 'text-warning' },
  red:    { bg: 'bg-error/10', text: 'text-error' },
}

export interface StatCardProps extends React.HTMLAttributes<HTMLDivElement> {
  label: string
  value: string | number
  icon?: React.ReactNode
  trend?: { value: number; label?: string }
  /** Optional color name for the icon bubble. E.g. "purple", "green", "pink" */
  iconColor?: string
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

export function StatCard({ label, value, icon, trend, iconColor, className, ...rest }: StatCardProps) {
  const colorClasses = iconColor != null ? (iconColorMap[iconColor] ?? null) : null

  return (
    <div
      role="group"
      aria-label={label}
      className={cn(
        'relative overflow-hidden rounded-xl border border-border bg-bg-secondary p-5',
        className,
      )}
      {...rest}
    >
      {/* Subtle gradient overlay at top of card */}
      <div
        className="pointer-events-none absolute inset-x-0 top-0 h-24"
        style={{
          background:
            'linear-gradient(180deg, rgba(139,92,246,0.04) 0%, transparent 100%)',
        }}
        aria-hidden="true"
      />

      <div className="relative">
        {icon != null ? (
          <span
            className={cn(
              'shrink-0 mb-3 block w-fit',
              colorClasses != null
                ? cn('p-2 rounded-lg', colorClasses.bg, colorClasses.text)
                : 'text-text-tertiary',
            )}
            aria-hidden="true"
          >
            {icon}
          </span>
        ) : null}

        <div className="text-2xl font-semibold text-text-primary">{value}</div>
        <div className="text-sm text-text-tertiary mt-1">{label}</div>

        {trend != null ? (
          <div className={cn('text-sm mt-2', trendColorClass(trend.value))}>
            {trendPrefix(trend.value)}{trend.label != null ? ` ${trend.label}` : ` ${Math.abs(trend.value)}`}
          </div>
        ) : null}
      </div>
    </div>
  )
}
