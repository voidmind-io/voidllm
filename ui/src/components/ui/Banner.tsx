import { cn } from '../../lib/utils'

export interface BannerProps {
  variant: 'info' | 'warning' | 'error' | 'success'
  title: string
  description?: string
  onDismiss?: (e: React.MouseEvent) => void
  className?: string
}

const variantClasses: Record<BannerProps['variant'], string> = {
  info: 'bg-info/10 border-info/20 text-info',
  warning: 'bg-warning/10 border-warning/20 text-warning',
  error: 'bg-error/10 border-error/20 text-error',
  success: 'bg-success/10 border-success/20 text-success',
}

const dotClasses: Record<BannerProps['variant'], string> = {
  info: 'bg-info ring-info/40',
  warning: 'bg-warning ring-warning/40',
  error: 'bg-error ring-error/40',
  success: 'bg-success ring-success/40',
}

const descriptionClasses: Record<BannerProps['variant'], string> = {
  info: 'text-info/80',
  warning: 'text-warning/80',
  error: 'text-error/80',
  success: 'text-success/80',
}

export function Banner({ variant, title, description, onDismiss, className }: BannerProps) {
  return (
    <div
      role={variant === 'error' ? 'alert' : 'status'}
      className={cn(
        'rounded-lg border px-4 py-3 flex items-start gap-3',
        variantClasses[variant],
        className,
      )}
    >
      <span
        className={cn(
          'mt-1 shrink-0 w-2 h-2 rounded-full ring-2 ring-offset-2 ring-offset-transparent',
          dotClasses[variant],
        )}
        aria-hidden="true"
      />
      <div className="flex-1 min-w-0">
        <p className="text-sm font-medium">{title}</p>
        {description !== undefined && (
          <p className={cn('mt-0.5 text-sm', descriptionClasses[variant])}>{description}</p>
        )}
      </div>
      {onDismiss !== undefined && (
        <button
          type="button"
          onClick={(e) => onDismiss?.(e)}
          aria-label="Dismiss"
          className={cn(
            'shrink-0 -mt-0.5 -mr-1 p-1 rounded transition-colors',
            'hover:opacity-70 focus:outline-none focus:ring-2 focus:ring-current focus:ring-offset-1 focus:ring-offset-transparent',
          )}
        >
          <svg
            aria-hidden="true"
            className="h-3.5 w-3.5"
            fill="none"
            viewBox="0 0 24 24"
            stroke="currentColor"
            strokeWidth={2.5}
          >
            <path strokeLinecap="round" strokeLinejoin="round" d="M6 18L18 6M6 6l12 12" />
          </svg>
        </button>
      )}
    </div>
  )
}
