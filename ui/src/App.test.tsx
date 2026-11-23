import { act, render, screen } from '@testing-library/react'
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom'
import { describe, it, expect, vi, afterEach } from 'vitest'
import { RequireAuth } from './App'
import { LOCAL_STORAGE_KEY, SESSION_EXPIRY_KEY } from './lib/constants'
import { queryClient } from './lib/queryClient'
import { ToastProvider } from './hooks/useToast'

vi.mock('./components/layout/Shell', () => ({
  Shell: () => <div>shell</div>,
}))

function LoginProbe() {
  const loc = useLocation()
  return <div>login page {loc.search}</div>
}

function renderGuard(initial = '/') {
  render(
    <ToastProvider>
      <MemoryRouter initialEntries={[initial]}>
        <Routes>
          <Route path="/login" element={<LoginProbe />} />
          <Route element={<RequireAuth />}>
            <Route index element={<div>home</div>} />
            <Route path="keys" element={<div>keys</div>} />
          </Route>
        </Routes>
      </MemoryRouter>
    </ToastProvider>,
  )
}

afterEach(() => {
  vi.useRealTimers()
  localStorage.clear()
})

describe('RequireAuth session expiry', () => {
  it('logs out on load when expires_at has passed', () => {
    localStorage.setItem(LOCAL_STORAGE_KEY, 'vl_sk_x')
    localStorage.setItem(SESSION_EXPIRY_KEY, '2020-01-01T00:00:00Z')
    queryClient.setQueryData(['me'], { id: 'prev-user' })
    renderGuard()
    expect(screen.getByText(/login page/)).toBeInTheDocument()
    expect(queryClient.getQueryData(['me'])).toBeUndefined()
    expect(localStorage.getItem(LOCAL_STORAGE_KEY)).toBeNull()
    expect(localStorage.getItem(SESSION_EXPIRY_KEY)).toBeNull()
  })

  it('keeps a valid session', () => {
    localStorage.setItem(LOCAL_STORAGE_KEY, 'vl_sk_x')
    localStorage.setItem(SESSION_EXPIRY_KEY, '2099-01-01T00:00:00Z')
    renderGuard()
    expect(screen.getByText('shell')).toBeInTheDocument()
  })

  it('logs out via timer when the session expires while open', () => {
    vi.useFakeTimers()
    localStorage.setItem(LOCAL_STORAGE_KEY, 'vl_sk_x')
    localStorage.setItem(SESSION_EXPIRY_KEY, new Date(Date.now() + 5_000).toISOString())
    queryClient.setQueryData(['me'], { id: 'prev-user' })
    renderGuard()
    expect(screen.getByText('shell')).toBeInTheDocument()
    act(() => {
      vi.advanceTimersByTime(6_000)
    })
    expect(screen.getByText(/login page/)).toBeInTheDocument()
    expect(localStorage.getItem(LOCAL_STORAGE_KEY)).toBeNull()
    expect(queryClient.getQueryData(['me'])).toBeUndefined()
  })

  it('passes the current path as next when redirecting to login', () => {
    localStorage.setItem(LOCAL_STORAGE_KEY, 'vl_sk_x')
    localStorage.setItem(SESSION_EXPIRY_KEY, '2020-01-01T00:00:00Z')
    renderGuard('/keys')
    expect(screen.getByText('login page ?next=%2Fkeys')).toBeInTheDocument()
  })
})

describe('RequireAuth expiry warning', () => {
  const WARNING = /Your session ends in 5 minutes/

  it('warns 5 minutes before expiry and cleans up on unmount', () => {
    vi.useFakeTimers()
    localStorage.setItem(LOCAL_STORAGE_KEY, 'vl_sk_x')
    localStorage.setItem(SESSION_EXPIRY_KEY, new Date(Date.now() + 10 * 60_000).toISOString())
    renderGuard()
    expect(screen.queryByText(WARNING)).not.toBeInTheDocument()
    act(() => {
      vi.advanceTimersByTime(5 * 60_000 + 1_000)
    })
    expect(screen.getByText(WARNING)).toBeInTheDocument()
  })

  it('warns immediately when less than 5 minutes remain', () => {
    localStorage.setItem(LOCAL_STORAGE_KEY, 'vl_sk_x')
    localStorage.setItem(SESSION_EXPIRY_KEY, new Date(Date.now() + 2 * 60_000).toISOString())
    renderGuard()
    expect(screen.getByText(WARNING)).toBeInTheDocument()
  })

  it('does not warn without a known expiry', () => {
    localStorage.setItem(LOCAL_STORAGE_KEY, 'vl_sk_x')
    renderGuard()
    expect(screen.queryByText(WARNING)).not.toBeInTheDocument()
  })

  it('clears the pending warning timer on unmount', () => {
    vi.useFakeTimers()
    localStorage.setItem(LOCAL_STORAGE_KEY, 'vl_sk_x')
    localStorage.setItem(SESSION_EXPIRY_KEY, new Date(Date.now() + 10 * 60_000).toISOString())
    const baseline = vi.getTimerCount()
    const { unmount } = render(
      <ToastProvider>
        <MemoryRouter>
          <Routes>
            <Route element={<RequireAuth />}>
              <Route index element={<div>home</div>} />
            </Route>
          </Routes>
        </MemoryRouter>
      </ToastProvider>,
    )
    expect(vi.getTimerCount()).toBeGreaterThan(baseline)
    unmount()
    expect(vi.getTimerCount()).toBe(baseline)
  })
})
