import React, { createContext, useCallback, useContext, useEffect, useRef, useState } from 'react'
import ReactDOM from 'react-dom'
import { cn } from '../lib/utils'

export interface ToastMessage {
  id: string
  message: string
  variant: 'success' | 'error' | 'info'
  duration: number
}

export interface ToastContextValue {
  toast: (opts: Omit<ToastMessage, 'id' | 'duration'> & { duration?: number }) => string
  update: (id: string, opts: Omit<ToastMessage, 'id' | 'duration'> & { duration?: number }) => void
  dismiss: (id: string) => void
}

const ToastContext = createContext<ToastContextValue | null>(null)

// ---------------------------------------------------------------------------
// Icons
// ---------------------------------------------------------------------------

function SuccessIcon() {
  return (
    <svg
      className="h-5 w-5 shrink-0 text-success"
      fill="none"
      viewBox="0 0 24 24"
      stroke="currentColor"
      strokeWidth={2}
      aria-hidden="true"
    >
      <path strokeLinecap="round" strokeLinejoin="round" d="M5 13l4 4L19 7" />
    </svg>
  )
}

function ErrorIcon() {
  return (
    <svg
      className="h-5 w-5 shrink-0 text-error"
      fill="none"
      viewBox="0 0 24 24"
      stroke="currentColor"
      strokeWidth={2}
      aria-hidden="true"
    >
      <circle cx="12" cy="12" r="10" />
      <path strokeLinecap="round" strokeLinejoin="round" d="M15 9l-6 6M9 9l6 6" />
    </svg>
  )
}

function InfoIcon() {
  return (
    <svg
      className="h-5 w-5 shrink-0 text-accent"
      fill="none"
      viewBox="0 0 24 24"
      stroke="currentColor"
      strokeWidth={2}
      aria-hidden="true"
    >
      <circle cx="12" cy="12" r="10" />
      <path strokeLinecap="round" strokeLinejoin="round" d="M12 16v-4M12 8h.01" />
    </svg>
  )
}

function CloseIcon() {
  return (
    <svg
      className="h-4 w-4"
      fill="none"
      viewBox="0 0 24 24"
      stroke="currentColor"
      strokeWidth={2}
      aria-hidden="true"
    >
      <path strokeLinecap="round" strokeLinejoin="round" d="M6 18L18 6M6 6l12 12" />
    </svg>
  )
}

const variantIcon: Record<ToastMessage['variant'], () => React.ReactNode> = {
  success: () => <SuccessIcon />,
  error: () => <ErrorIcon />,
  info: () => <InfoIcon />,
}

const variantClasses: Record<ToastMessage['variant'], string> = {
  success: 'bg-success/10 border-success/30',
  error: 'bg-error/10 border-error/30',
  info: 'bg-accent/10 border-accent/30',
}

// ---------------------------------------------------------------------------
// ToastItem
// ---------------------------------------------------------------------------

interface ToastItemProps {
  toast: ToastMessage
  onDismiss: (id: string) => void
}

function ToastItem({ toast, onDismiss }: ToastItemProps) {
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(() => {
    if (toast.duration === 0) return
    timerRef.current = setTimeout(() => {
      onDismiss(toast.id)
    }, toast.duration)
    return () => {
      if (timerRef.current !== null) {
        clearTimeout(timerRef.current)
      }
    }
  }, [toast.id, toast.duration, onDismiss])

  return (
    <div
      role={toast.variant === 'error' ? 'alert' : 'status'}
      aria-live={toast.variant === 'error' ? 'assertive' : 'polite'}
      className={cn(
        'flex items-center gap-3 rounded-lg px-4 py-3 shadow-lg border text-text-primary',
        'min-w-[280px] max-w-[420px]',
        variantClasses[toast.variant],
      )}
    >
      {variantIcon[toast.variant]()}
      <span className="flex-1 text-sm">{toast.message}</span>
      <button
        onClick={() => onDismiss(toast.id)}
        className="text-text-tertiary hover:text-text-primary transition-colors shrink-0"
        aria-label="Dismiss notification"
      >
        <CloseIcon />
      </button>
    </div>
  )
}

// ---------------------------------------------------------------------------
// ToastContainer
// ---------------------------------------------------------------------------

interface ToastContainerProps {
  toasts: ToastMessage[]
  onDismiss: (id: string) => void
}

function ToastContainer({ toasts, onDismiss }: ToastContainerProps) {
  if (toasts.length === 0) return null

  return ReactDOM.createPortal(
    <div
      className="fixed bottom-4 right-4 z-50 flex flex-col gap-2"
      aria-label="Notifications"
    >
      {toasts.map((t) => (
        <ToastItem key={t.id} toast={t} onDismiss={onDismiss} />
      ))}
    </div>,
    document.body,
  )
}

// ---------------------------------------------------------------------------
// ToastProvider
// ---------------------------------------------------------------------------

const DEFAULT_DURATION: Record<ToastMessage['variant'], number> = {
  success: 3000,
  info: 3000,
  error: 0,
}

export function ToastProvider({ children }: { children: React.ReactNode }) {
  const [toasts, setToasts] = useState<ToastMessage[]>([])

  const dismiss = useCallback((id: string) => {
    setToasts((prev) => prev.filter((t) => t.id !== id))
  }, [])

  const toast = useCallback(
    (opts: Omit<ToastMessage, 'id' | 'duration'> & { duration?: number }): string => {
      const id = crypto.randomUUID()
      const duration = opts.duration ?? DEFAULT_DURATION[opts.variant]
      setToasts((prev) => {
        const next = [...prev, { ...opts, id, duration }]
        return next.length > 5 ? next.slice(next.length - 5) : next
      })
      return id
    },
    [],
  )

  const update = useCallback(
    (id: string, opts: Omit<ToastMessage, 'id' | 'duration'> & { duration?: number }) => {
      const duration = opts.duration ?? DEFAULT_DURATION[opts.variant]
      setToasts((prev) => prev.map((t) => (t.id === id ? { ...opts, id, duration } : t)))
    },
    [],
  )

  return (
    <ToastContext.Provider value={{ toast, update, dismiss }}>
      {children}
      <ToastContainer toasts={toasts} onDismiss={dismiss} />
    </ToastContext.Provider>
  )
}

// ---------------------------------------------------------------------------
// useToast
// ---------------------------------------------------------------------------

/** Access the toast notification system. Must be used inside a ToastProvider. */
// eslint-disable-next-line react-refresh/only-export-components
export function useToast(): ToastContextValue {
  const ctx = useContext(ToastContext)
  if (ctx === null) {
    throw new Error('useToast must be used within a ToastProvider')
  }
  return ctx
}
