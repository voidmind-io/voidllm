import React from 'react'
import { cn, formatDate } from '../../lib/utils'

export interface TimeAgoProps extends React.HTMLAttributes<HTMLTimeElement> {
  date: string
  fallback?: string
}

/** Compute a human-readable relative time string from an ISO 8601 timestamp.
 *
 * Returns an empty string when the input is invalid (NaN). The caller is
 * responsible for rendering the fallback in that case.
 */
// eslint-disable-next-line react-refresh/only-export-components
export function relativeTime(iso: string): string {
  const ms = Date.parse(iso)
  if (Number.isNaN(ms)) return ''

  const diffSec = Math.floor((Date.now() - ms) / 1000)

  // Future timestamps — show "in Xh" style (for expiry dates)
  if (diffSec < 0) {
    const absSec = Math.abs(diffSec)
    if (absSec < 60) return 'in <1m'
    const absMin = Math.floor(absSec / 60)
    if (absMin < 60) return `in ${absMin}m`
    const absHour = Math.floor(absMin / 60)
    if (absHour < 24) return `in ${absHour}h`
    const absDay = Math.floor(absHour / 24)
    if (absDay < 7) return `in ${absDay}d`
    if (absDay < 30) return `in ${Math.floor(absDay / 7)}w`
    return formatDate(iso)
  }

  if (diffSec < 60) return 'just now'

  const diffMin = Math.floor(diffSec / 60)
  if (diffMin < 60) return `${diffMin}m ago`

  const diffHour = Math.floor(diffMin / 60)
  if (diffHour < 24) return `${diffHour}h ago`

  const diffDay = Math.floor(diffHour / 24)
  if (diffDay < 7) return `${diffDay}d ago`

  const diffWeek = Math.floor(diffDay / 7)
  if (diffDay < 30) return `${diffWeek}w ago`

  return formatDate(iso)
}

/** Renders a relative timestamp using the semantic <time> element.
 *
 * Shows a fallback string when the date prop is empty or unparseable.
 * Provides a full date tooltip on hover via the title attribute.
 */
export function TimeAgo({ date, fallback = 'never', className, ...rest }: TimeAgoProps) {
  if (!date) {
    return (
      <time className={cn('text-sm text-text-tertiary', className)} {...rest}>
        {fallback}
      </time>
    )
  }

  const text = relativeTime(date)
  if (!text) {
    return (
      <time className={cn('text-sm text-text-tertiary', className)} {...rest}>
        {fallback}
      </time>
    )
  }

  return (
    <time
      dateTime={date}
      title={new Date(date).toLocaleString()}
      className={cn('text-sm text-text-tertiary', className)}
      {...rest}
    >
      {text}
    </time>
  )
}
