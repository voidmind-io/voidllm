import React from 'react'
import { cn } from '../../lib/utils'

export interface KeyHintProps extends React.HTMLAttributes<HTMLSpanElement> {
  /** The pre-formatted key hint from the backend (e.g. "vl_uk_...2ad6"). */
  hint: string
}

export function KeyHint({ hint, className, ...rest }: KeyHintProps) {
  // hint is already formatted by the backend as "prefix...suffix"
  const ellipsisIdx = hint.indexOf('...')
  const prefix = ellipsisIdx >= 0 ? hint.slice(0, ellipsisIdx + 3) : ''
  const suffix = ellipsisIdx >= 0 ? hint.slice(ellipsisIdx + 3) : hint

  return (
    <span className={cn('font-mono text-xs text-text-secondary', className)} {...rest}>
      <span className="text-text-tertiary">{prefix}</span>{suffix}
    </span>
  )
}
