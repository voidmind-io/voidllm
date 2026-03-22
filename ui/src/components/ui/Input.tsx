import React from 'react'
import { cn } from '../../lib/utils'

export interface InputProps
  extends Omit<React.InputHTMLAttributes<HTMLInputElement>, 'size'> {
  label?: string
  error?: string
  description?: string
  fullWidth?: boolean
}

export const Input = React.forwardRef<HTMLInputElement, InputProps>(
  function Input(
    {
      label,
      error,
      description,
      fullWidth = true,
      id: idProp,
      className,
      disabled,
      ...rest
    },
    ref,
  ) {
    const generatedId = React.useId()
    const id = idProp ?? generatedId
    const descId = `${id}-desc`
    const errorId = `${id}-error`

    const ariaDescribedBy = error ? errorId : description ? descId : undefined

    return (
      <div className={cn(fullWidth && 'w-full')}>
        {label != null && (
          <label
            htmlFor={id}
            className="block text-sm font-medium text-text-secondary mb-1.5"
          >
            {label}
          </label>
        )}
        <input
          ref={ref}
          id={id}
          disabled={disabled}
          aria-invalid={error ? true : undefined}
          aria-describedby={ariaDescribedBy}
          className={cn(
            'block w-full rounded-md bg-bg-secondary border border-border px-3 py-2 text-sm text-text-primary placeholder:text-text-tertiary',
            'transition-colors duration-150',
            'focus:outline-none focus:border-accent focus:ring-2 focus:ring-accent/40',
            error && 'border-error focus:border-error focus:ring-error/40',
            disabled && 'opacity-50 cursor-not-allowed bg-bg-tertiary',
            className,
          )}
          {...rest}
        />
        {description != null && error == null && (
          <p id={descId} className="mt-1.5 text-xs text-text-tertiary">
            {description}
          </p>
        )}
        {error != null && (
          <p id={errorId} role="alert" className="mt-1.5 text-xs text-error">
            {error}
          </p>
        )}
      </div>
    )
  },
)
