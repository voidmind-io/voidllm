import React, { act } from 'react'
import { fireEvent, render, renderHook, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ToastProvider, useToast } from './useToast'

// ---------------------------------------------------------------------------
// Shared wrapper
// ---------------------------------------------------------------------------

function wrapper({ children }: { children: React.ReactNode }) {
  return <ToastProvider>{children}</ToastProvider>
}

// ---------------------------------------------------------------------------
// Helper component — triggers a toast on button click
// ---------------------------------------------------------------------------

interface TestComponentProps {
  variant: 'success' | 'error' | 'info'
  message: string
  duration?: number
}

function TestComponent({ variant, message, duration }: TestComponentProps) {
  const { toast } = useToast()
  return (
    <button onClick={() => toast({ message, variant, duration })}>Show</button>
  )
}

// ---------------------------------------------------------------------------
// useToast hook — basic behaviour
// ---------------------------------------------------------------------------

describe('useToast hook', () => {
  it('toast() shows a toast with the given message', async () => {
    render(
      <ToastProvider>
        <TestComponent variant="info" message="Hello toast" />
      </ToastProvider>,
    )

    await userEvent.click(screen.getByRole('button', { name: 'Show' }))

    expect(screen.getByText('Hello toast')).toBeInTheDocument()
  })

  it('dismiss() removes a toast by id', async () => {
    const { result } = renderHook(() => useToast(), { wrapper })

    act(() => {
      result.current.toast({ message: 'Dismissible', variant: 'info' })
    })

    expect(screen.getByText('Dismissible')).toBeInTheDocument()

    const statusEl = screen.getByRole('status')
    const dismissBtn = within(statusEl).getByRole('button', {
      name: 'Dismiss notification',
    })
    await userEvent.click(dismissBtn)

    expect(screen.queryByText('Dismissible')).not.toBeInTheDocument()
  })

  it('multiple toasts stack — all three are visible simultaneously', async () => {
    const { result } = renderHook(() => useToast(), { wrapper })

    act(() => {
      result.current.toast({ message: 'First', variant: 'info' })
      result.current.toast({ message: 'Second', variant: 'success' })
      result.current.toast({ message: 'Third', variant: 'error' })
    })

    expect(screen.getByText('First')).toBeInTheDocument()
    expect(screen.getByText('Second')).toBeInTheDocument()
    expect(screen.getByText('Third')).toBeInTheDocument()
  })

  it('useToast outside a ToastProvider throws an error', () => {
    // Suppress the React error boundary console noise for this test
    const spy = vi.spyOn(console, 'error').mockImplementation(() => undefined)

    expect(() => renderHook(() => useToast())).toThrow(
      'useToast must be used within a ToastProvider',
    )

    spy.mockRestore()
  })
})

// ---------------------------------------------------------------------------
// Auto-dismiss behaviour
// ---------------------------------------------------------------------------

describe('auto-dismiss', () => {
  beforeEach(() => {
    vi.useFakeTimers()
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('success toast auto-dismisses after 3000 ms', () => {
    render(
      <ToastProvider>
        <TestComponent variant="success" message="Saved!" />
      </ToastProvider>,
    )

    fireEvent.click(screen.getByRole('button', { name: 'Show' }))
    expect(screen.getByText('Saved!')).toBeInTheDocument()

    act(() => {
      vi.advanceTimersByTime(3000)
    })

    expect(screen.queryByText('Saved!')).not.toBeInTheDocument()
  })

  it('error toast does NOT auto-dismiss after 10000 ms', () => {
    render(
      <ToastProvider>
        <TestComponent variant="error" message="Something went wrong" />
      </ToastProvider>,
    )

    fireEvent.click(screen.getByRole('button', { name: 'Show' }))
    expect(screen.getByText('Something went wrong')).toBeInTheDocument()

    act(() => {
      vi.advanceTimersByTime(10000)
    })

    expect(screen.getByText('Something went wrong')).toBeInTheDocument()
  })

  it('info toast auto-dismisses after 3000 ms', () => {
    render(
      <ToastProvider>
        <TestComponent variant="info" message="FYI!" />
      </ToastProvider>,
    )

    fireEvent.click(screen.getByRole('button', { name: 'Show' }))
    expect(screen.getByText('FYI!')).toBeInTheDocument()

    act(() => {
      vi.advanceTimersByTime(3000)
    })

    expect(screen.queryByText('FYI!')).not.toBeInTheDocument()
  })

  it('custom duration is respected', () => {
    render(
      <ToastProvider>
        <TestComponent variant="success" message="Custom" duration={5000} />
      </ToastProvider>,
    )

    fireEvent.click(screen.getByRole('button', { name: 'Show' }))
    expect(screen.getByText('Custom')).toBeInTheDocument()

    // Should still be visible just before the custom duration
    act(() => {
      vi.advanceTimersByTime(4999)
    })
    expect(screen.getByText('Custom')).toBeInTheDocument()

    // Should be gone at or after the custom duration
    act(() => {
      vi.advanceTimersByTime(1)
    })
    expect(screen.queryByText('Custom')).not.toBeInTheDocument()
  })

  it('clears auto-dismiss timer when unmounted before expiry', () => {
    vi.useFakeTimers()
    const { unmount } = render(
      <ToastProvider>
        <TestComponent variant="success" message="Unmount me" />
      </ToastProvider>,
    )
    fireEvent.click(screen.getByRole('button', { name: 'Show' }))
    expect(screen.getByText('Unmount me')).toBeInTheDocument()
    unmount()
    // Advancing timers after unmount should not throw or warn
    act(() => { vi.advanceTimersByTime(5000) })
    vi.useRealTimers()
  })
})

// ---------------------------------------------------------------------------
// ToastItem rendering
// ---------------------------------------------------------------------------

describe('ToastItem rendering', () => {
  it('renders the message text', async () => {
    render(
      <ToastProvider>
        <TestComponent variant="info" message="Render me" />
      </ToastProvider>,
    )

    await userEvent.click(screen.getByRole('button', { name: 'Show' }))

    expect(screen.getByText('Render me')).toBeInTheDocument()
  })

  it('success variant applies success styling classes', async () => {
    render(
      <ToastProvider>
        <TestComponent variant="success" message="Well done" />
      </ToastProvider>,
    )

    await userEvent.click(screen.getByRole('button', { name: 'Show' }))

    const statusEl = screen.getByRole('status')
    expect(statusEl.className).toContain('bg-success/10')
    expect(statusEl.className).toContain('border-success/30')
  })

  it('error variant applies error styling classes', async () => {
    render(
      <ToastProvider>
        <TestComponent variant="error" message="Oh no" />
      </ToastProvider>,
    )

    await userEvent.click(screen.getByRole('button', { name: 'Show' }))

    const alertEl = screen.getByRole('alert')
    expect(alertEl.className).toContain('bg-error/10')
    expect(alertEl.className).toContain('border-error/30')
  })

  it('info variant applies info styling classes', async () => {
    render(
      <ToastProvider>
        <TestComponent variant="info" message="Just so you know" />
      </ToastProvider>,
    )

    await userEvent.click(screen.getByRole('button', { name: 'Show' }))

    const statusEl = screen.getByRole('status')
    expect(statusEl.className).toContain('bg-accent/10')
    expect(statusEl.className).toContain('border-accent/30')
  })

  it('close button has aria-label="Dismiss notification"', async () => {
    render(
      <ToastProvider>
        <TestComponent variant="info" message="Closable" />
      </ToastProvider>,
    )

    await userEvent.click(screen.getByRole('button', { name: 'Show' }))

    expect(
      screen.getByRole('button', { name: 'Dismiss notification' }),
    ).toBeInTheDocument()
  })

  it('clicking the close button removes the toast', async () => {
    render(
      <ToastProvider>
        <TestComponent variant="info" message="Click to close" />
      </ToastProvider>,
    )

    await userEvent.click(screen.getByRole('button', { name: 'Show' }))
    expect(screen.getByText('Click to close')).toBeInTheDocument()

    await userEvent.click(
      screen.getByRole('button', { name: 'Dismiss notification' }),
    )

    expect(screen.queryByText('Click to close')).not.toBeInTheDocument()
  })

  it('toast element has role="status" for accessibility', async () => {
    render(
      <ToastProvider>
        <TestComponent variant="success" message="Accessible" />
      </ToastProvider>,
    )

    await userEvent.click(screen.getByRole('button', { name: 'Show' }))

    expect(screen.getByRole('status')).toBeInTheDocument()
  })
})

// ---------------------------------------------------------------------------
// Portal — toasts render into document.body
// ---------------------------------------------------------------------------

describe('Portal', () => {
  it('toast content is rendered into document.body, not the test container', async () => {
    const { container } = render(
      <ToastProvider>
        <TestComponent variant="info" message="Portal test" />
      </ToastProvider>,
    )

    await userEvent.click(screen.getByRole('button', { name: 'Show' }))

    // The message should not be a descendant of the component's own container
    expect(container.querySelector('[role="status"]')).toBeNull()

    // But it should exist somewhere in the document (portalled into body)
    expect(document.body.querySelector('[role="status"]')).not.toBeNull()
    expect(screen.getByText('Portal test')).toBeInTheDocument()
  })
})
