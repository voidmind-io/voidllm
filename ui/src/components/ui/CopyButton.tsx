import React, { useState, useEffect } from 'react'
import { cn } from '../../lib/utils'

export interface CopyButtonProps extends React.ButtonHTMLAttributes<HTMLButtonElement> {
  text: string
  label?: string
  copiedLabel?: string
  timeout?: number
}

const baseClasses =
  'inline-flex items-center gap-1.5 rounded-md border border-border bg-transparent px-2 py-1 text-xs text-text-secondary cursor-pointer transition-colors duration-200 focus:outline-none focus:ring-2 focus:ring-accent focus:ring-offset-2 focus:ring-offset-bg-primary'

const copiedClasses = 'text-success border-success/30'

export function CopyButton({
  text,
  label = 'Copy',
  copiedLabel = 'Copied!',
  timeout = 2000,
  className,
  onClick,
  disabled,
  ...rest
}: CopyButtonProps) {
  const [copied, setCopied] = useState(false)

  useEffect(() => {
    if (!copied) return
    const timer = setTimeout(() => setCopied(false), timeout)
    return () => clearTimeout(timer)
  }, [copied, timeout])

  function handleClick(e: React.MouseEvent<HTMLButtonElement>) {
    if (disabled) return
    navigator.clipboard.writeText(text).then(() => setCopied(true)).catch(() => undefined)
    onClick?.(e)
  }

  return (
    <button
      type="button"
      disabled={disabled}
      className={cn(
        baseClasses,
        !disabled && 'hover:bg-bg-tertiary hover:text-text-primary',
        disabled && 'opacity-50 cursor-not-allowed',
        copied && copiedClasses,
        className,
      )}
      onClick={handleClick}
      {...rest}
    >
      {copied ? (
        <svg
          width="16"
          height="16"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          strokeWidth="1.5"
          strokeLinecap="round"
          strokeLinejoin="round"
          aria-hidden="true"
        >
          <polyline points="20 6 9 17 4 12" />
        </svg>
      ) : (
        <svg
          width="16"
          height="16"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          strokeWidth="1.5"
          strokeLinecap="round"
          strokeLinejoin="round"
          aria-hidden="true"
        >
          <rect x="9" y="9" width="13" height="13" rx="2" ry="2" />
          <path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1" />
        </svg>
      )}
      <span aria-live="polite">{copied ? copiedLabel : label}</span>
    </button>
  )
}
