import { Component } from 'react'
import type { ErrorInfo, ReactNode } from 'react'
import { Link } from 'react-router-dom'
import { Button } from '../ui/Button'

interface ErrorBoundaryProps {
  children: ReactNode
}

interface ErrorBoundaryState {
  hasError: boolean
}

/** Catches render errors in the page tree and shows a recoverable fallback. */
export class ErrorBoundary extends Component<ErrorBoundaryProps, ErrorBoundaryState> {
  state: ErrorBoundaryState = { hasError: false }

  static getDerivedStateFromError(): ErrorBoundaryState {
    return { hasError: true }
  }

  componentDidCatch(error: Error, info: ErrorInfo): void {
    console.error('Unhandled render error', error, info.componentStack)
  }

  render() {
    if (!this.state.hasError) return this.props.children
    return (
      <div
        role="alert"
        className="rounded-lg border border-border bg-bg-secondary p-12 text-center"
      >
        <h2 className="text-lg font-semibold text-text-primary">Something went wrong</h2>
        <p className="mt-2 text-sm text-text-tertiary">
          This page hit an unexpected error. Reloading usually fixes it.
        </p>
        <div className="mt-6 flex items-center justify-center gap-3">
          <Button type="button" onClick={() => window.location.reload()}>
            Reload page
          </Button>
          <Link
            to="/"
            className="text-sm text-accent hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent rounded-sm"
          >
            Go to dashboard
          </Link>
        </div>
      </div>
    )
  }
}
