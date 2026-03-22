import React from 'react'
import { cn } from '../../lib/utils'

export interface ToggleProps extends Omit<React.ButtonHTMLAttributes<HTMLButtonElement>, 'onChange'> {
  checked: boolean
  onChange: (checked: boolean) => void
  label?: string
  size?: 'sm' | 'md'
}

export function Toggle({ checked, onChange, label, size = 'md', disabled, className, ...rest }: ToggleProps) {
  const trackClasses = cn(
    'rounded-full p-0.5 transition-colors duration-200',
    'focus:outline-none focus:ring-2 focus:ring-accent focus:ring-offset-2 focus:ring-offset-bg-primary',
    size === 'md' ? 'w-9 h-5' : 'w-7 h-4',
    checked ? 'bg-accent' : 'bg-bg-tertiary',
    disabled ? 'opacity-50 cursor-not-allowed' : 'cursor-pointer',
    className,
  )

  const knobClasses = cn(
    'block rounded-full bg-white transition-transform duration-200',
    size === 'md' ? 'h-4 w-4' : 'h-3 w-3',
    checked
      ? size === 'md'
        ? 'translate-x-4'
        : 'translate-x-3'
      : 'translate-x-0',
  )

  const button = (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      disabled={disabled}
      onClick={() => onChange(!checked)}
      className={trackClasses}
      {...rest}
    >
      <span className={knobClasses} />
    </button>
  )

  if (label != null) {
    return (
      <div className={cn('inline-flex items-center gap-2', disabled ? 'cursor-not-allowed' : 'cursor-pointer')}>
        {button}
        <span
          className="text-sm text-text-secondary select-none"
          onClick={() => { if (!disabled) onChange(!checked) }}
        >
          {label}
        </span>
      </div>
    )
  }

  return button
}
