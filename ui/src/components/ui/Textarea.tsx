import React from 'react'
import { cn } from '../../lib/utils'

export interface TextareaProps
  extends React.TextareaHTMLAttributes<HTMLTextAreaElement> {
  label?: string
  error?: string
  description?: string
  /** Extra classes applied to the outer wrapper div (e.g. "flex-1" for stretching layouts). */
  wrapperClassName?: string
}

export const Textarea = React.forwardRef<HTMLTextAreaElement, TextareaProps>(
  function Textarea(
    {
      label,
      error,
      description,
      wrapperClassName,
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
      <div className={cn('flex flex-col', wrapperClassName)}>
        {label != null && (
          <label
            htmlFor={id}
            className="block text-sm font-medium text-text-secondary mb-1.5"
          >
            {label}
          </label>
        )}
        <textarea
          ref={ref}
          id={id}
          disabled={disabled}
          aria-invalid={error ? true : undefined}
          aria-describedby={ariaDescribedBy}
          className={cn(
            'block w-full rounded-md border border-border bg-bg-secondary px-3 py-2 text-sm text-text-primary resize-none placeholder:text-text-tertiary',
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

Textarea.displayName = 'Textarea'
