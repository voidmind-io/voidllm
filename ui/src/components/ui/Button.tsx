import React from 'react'
import { cn } from '../../lib/utils'

export interface ButtonProps
  extends Omit<React.ButtonHTMLAttributes<HTMLButtonElement>, 'children'> {
  children: React.ReactNode
  variant?: 'primary' | 'secondary' | 'destructive' | 'ghost'
  size?: 'sm' | 'md' | 'lg'
  loading?: boolean
  fullWidth?: boolean
  icon?: React.ReactNode
}

function LoadingSpinner() {
  return (
    <span
      role="status"
      aria-label="Loading"
      className="inline-block h-4 w-4 animate-spin rounded-full border-2 border-current border-t-transparent"
    />
  )
}

const variantClasses: Record<NonNullable<ButtonProps['variant']>, string> = {
  primary: 'bg-accent text-white',
  secondary: 'bg-transparent border border-border text-text-secondary',
  destructive: 'bg-error text-white',
  ghost: 'bg-transparent text-text-secondary',
}

const variantHoverClasses: Record<NonNullable<ButtonProps['variant']>, string> = {
  primary: 'hover:brightness-110 active:brightness-95 hover:shadow-[0_0_20px_var(--accent-glow)]',
  secondary: 'hover:bg-bg-tertiary active:brightness-95',
  destructive: 'hover:brightness-110 active:brightness-95',
  ghost: 'hover:text-text-primary hover:opacity-80 active:brightness-95',
}

const sizeClasses: Record<NonNullable<ButtonProps['size']>, string> = {
  sm: 'px-3 py-1.5 text-sm gap-1.5',
  md: 'px-4 py-2 text-sm gap-2',
  lg: 'px-6 py-3 text-base gap-2',
}

export const Button = React.forwardRef<HTMLButtonElement, ButtonProps>(
  function Button(
    {
      children,
      variant = 'primary',
      size = 'md',
      loading = false,
      disabled = false,
      fullWidth = false,
      icon,
      className,
      type = 'button',
      ...rest
    },
    ref,
  ) {
    const isDisabled = disabled || loading

    return (
      <button
        ref={ref}
        type={type}
        disabled={isDisabled}
        aria-busy={loading || undefined}
        className={cn(
          'inline-flex items-center justify-center font-medium rounded-md transition-all duration-150 cursor-pointer',
          'focus:outline-none focus:ring-2 focus:ring-accent focus:ring-offset-2 focus:ring-offset-bg-primary',
          variantClasses[variant],
          !isDisabled && variantHoverClasses[variant],
          sizeClasses[size],
          isDisabled && 'opacity-50 cursor-not-allowed',
          fullWidth && 'w-full',
          className,
        )}
        {...rest}
      >
        {loading ? <LoadingSpinner /> : icon != null ? <span className="shrink-0">{icon}</span> : null}
        {children}
      </button>
    )
  },
)
