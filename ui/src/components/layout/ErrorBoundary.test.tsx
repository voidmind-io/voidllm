import { render, screen } from '@testing-library/react'
import { MemoryRouter, Routes, Route, Link, useLocation } from 'react-router-dom'
import { describe, it, expect, vi, afterEach } from 'vitest'
import userEvent from '@testing-library/user-event'
import { ErrorBoundary } from './ErrorBoundary'
import { handlePreloadError } from '../../lib/preloadError'

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

describe('handlePreloadError', () => {
  it('reloads only once per session', () => {
    const reload = vi.fn()
    vi.stubGlobal('location', { reload })
    handlePreloadError()
    handlePreloadError()
    expect(reload).toHaveBeenCalledTimes(1)
    vi.unstubAllGlobals()
  })
})
