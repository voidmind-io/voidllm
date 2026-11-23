import { render, screen } from '@testing-library/react'
import { MemoryRouter, Routes, Route, Link, useLocation } from 'react-router-dom'
import { describe, it, expect, vi, afterEach } from 'vitest'
import userEvent from '@testing-library/user-event'
import { ErrorBoundary } from './ErrorBoundary'
import { handlePreloadError, clearPreloadReloadGuard } from '../../lib/preloadError'

function Boom(): never {
  throw new Error('boom')
}

function Harness() {
  const loc = useLocation()
  return (
    <>
      <Link to="/ok">go ok</Link>
      <ErrorBoundary key={loc.pathname}>
        <Routes>
          <Route path="/bad" element={<Boom />} />
          <Route path="/ok" element={<div>fine page</div>} />
        </Routes>
      </ErrorBoundary>
    </>
  )
}

afterEach(() => {
  vi.restoreAllMocks()
  sessionStorage.clear()
})

describe('ErrorBoundary', () => {
  it('shows the fallback for a throwing child and resets on navigation', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => undefined)
    render(
      <MemoryRouter initialEntries={['/bad']}>
        <Harness />
      </MemoryRouter>,
    )
    expect(screen.getByText('Something went wrong')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Reload page' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Go to dashboard' })).toHaveAttribute('href', '/')
    await userEvent.click(screen.getByRole('link', { name: 'go ok' }))
    expect(screen.getByText('fine page')).toBeInTheDocument()
  })
})

describe('ErrorBoundary dashboard link', () => {
  it('leaves the error state when already on /', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => undefined)
    let shouldThrow = true
    function Flaky() {
      if (shouldThrow) throw new Error('boom')
      return <div>dashboard ok</div>
    }
    render(
      <MemoryRouter initialEntries={['/']}>
        <ErrorBoundary>
          <Flaky />
        </ErrorBoundary>
      </MemoryRouter>,
    )
    expect(screen.getByText('Something went wrong')).toBeInTheDocument()
    shouldThrow = false
    await userEvent.click(screen.getByRole('link', { name: 'Go to dashboard' }))
    expect(screen.getByText('dashboard ok')).toBeInTheDocument()
  })
})

describe('handlePreloadError', () => {
  it('reloads only once per session and prevents default when reloading', () => {
    const reload = vi.fn()
    vi.stubGlobal('location', { reload })
    const first = new Event('vite:preloadError', { cancelable: true })
    const second = new Event('vite:preloadError', { cancelable: true })
    handlePreloadError(first)
    handlePreloadError(second)
    expect(reload).toHaveBeenCalledTimes(1)
    expect(first.defaultPrevented).toBe(true)
    expect(second.defaultPrevented).toBe(false)
    vi.unstubAllGlobals()
  })

  it('can reload again after the guard is cleared', () => {
    const reload = vi.fn()
    vi.stubGlobal('location', { reload })
    handlePreloadError(new Event('vite:preloadError', { cancelable: true }))
    clearPreloadReloadGuard()
    handlePreloadError(new Event('vite:preloadError', { cancelable: true }))
    expect(reload).toHaveBeenCalledTimes(2)
    vi.unstubAllGlobals()
  })
})
