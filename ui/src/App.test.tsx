import { act, render, screen } from '@testing-library/react'
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom'
import { StrictMode } from 'react'
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

  it('preserves the hash in next', () => {
    localStorage.setItem(LOCAL_STORAGE_KEY, 'vl_sk_x')
    localStorage.setItem(SESSION_EXPIRY_KEY, '2020-01-01T00:00:00Z')
    renderGuard('/keys?a=1#sec')
    expect(screen.getByText('login page ?next=%2Fkeys%3Fa%3D1%23sec')).toBeInTheDocument()
  })
})

describe('RequireAuth expiry warning', () => {
  const WARNING = /Your session ends in \d+ minutes?\./

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

  it('shows the actual remaining minutes', () => {
    localStorage.setItem(LOCAL_STORAGE_KEY, 'vl_sk_x')
    localStorage.setItem(SESSION_EXPIRY_KEY, new Date(Date.now() + 2 * 60_000 - 5_000).toISOString())
    renderGuard()
    expect(screen.getByText(/Your session ends in 2 minutes\./)).toBeInTheDocument()
  })

  it('uses the singular for the last minute', () => {
    localStorage.setItem(LOCAL_STORAGE_KEY, 'vl_sk_x')
    localStorage.setItem(SESSION_EXPIRY_KEY, new Date(Date.now() + 30_000).toISOString())
    renderGuard()
    expect(screen.getByText(/Your session ends in 1 minute\./)).toBeInTheDocument()
  })

  it('dismisses the warning when the session expires', () => {
    vi.useFakeTimers()
    localStorage.setItem(LOCAL_STORAGE_KEY, 'vl_sk_x')
    localStorage.setItem(SESSION_EXPIRY_KEY, new Date(Date.now() + 2 * 60_000).toISOString())
    renderGuard()
    expect(screen.getByText(WARNING)).toBeInTheDocument()
    act(() => {
      vi.advanceTimersByTime(3 * 60_000)
    })
    expect(screen.getByText(/login page/)).toBeInTheDocument()
    expect(screen.queryByText(WARNING)).not.toBeInTheDocument()
  })

  it('shows the warning once under StrictMode', () => {
    localStorage.setItem(LOCAL_STORAGE_KEY, 'vl_sk_x')
    localStorage.setItem(SESSION_EXPIRY_KEY, new Date(Date.now() + 4 * 60_000 + 1_234).toISOString())
    render(
      <StrictMode>
        <ToastProvider>
          <MemoryRouter>
            <Routes>
              <Route element={<RequireAuth />}>
                <Route index element={<div>home</div>} />
              </Route>
            </Routes>
          </MemoryRouter>
        </ToastProvider>
      </StrictMode>,
    )
    expect(screen.getAllByText(WARNING)).toHaveLength(1)
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

  it('replaces a previous warning instead of stacking', () => {
    localStorage.setItem(LOCAL_STORAGE_KEY, 'vl_sk_x')
    localStorage.setItem(SESSION_EXPIRY_KEY, new Date(Date.now() + 2 * 60_000 + 7_000).toISOString())
    renderGuard()
    expect(screen.getAllByText(WARNING)).toHaveLength(1)
    localStorage.setItem(SESSION_EXPIRY_KEY, new Date(Date.now() + 4 * 60_000 + 7_000).toISOString())
    act(() => {
      window.dispatchEvent(new StorageEvent('storage', { key: SESSION_EXPIRY_KEY }))
    })
    expect(screen.getAllByText(WARNING)).toHaveLength(1)
    expect(screen.getByText(/Your session ends in 5 minutes\./)).toBeInTheDocument()
  })

  it('dismisses the warning when another tab extends the session', () => {
    localStorage.setItem(LOCAL_STORAGE_KEY, 'vl_sk_x')
    localStorage.setItem(SESSION_EXPIRY_KEY, new Date(Date.now() + 2 * 60_000 + 7_000).toISOString())
    renderGuard()
    expect(screen.getByText(WARNING)).toBeInTheDocument()
    localStorage.setItem(SESSION_EXPIRY_KEY, new Date(Date.now() + 60 * 60_000).toISOString())
    act(() => {
      window.dispatchEvent(new StorageEvent('storage', { key: SESSION_EXPIRY_KEY }))
    })
    expect(screen.queryByText(WARNING)).not.toBeInTheDocument()
  })

  it('dismisses the warning and redirects when another tab signs out', () => {
    localStorage.setItem(LOCAL_STORAGE_KEY, 'vl_sk_x')
    localStorage.setItem(SESSION_EXPIRY_KEY, new Date(Date.now() + 2 * 60_000 + 9_000).toISOString())
    renderGuard()
    expect(screen.getByText(WARNING)).toBeInTheDocument()
    localStorage.clear()
    act(() => {
      window.dispatchEvent(new StorageEvent('storage', { key: LOCAL_STORAGE_KEY }))
    })
    expect(screen.queryByText(WARNING)).not.toBeInTheDocument()
    expect(screen.getByText(/login page/)).toBeInTheDocument()
  })

  it('removes the storage listener on unmount', () => {
    const remove = vi.spyOn(window, 'removeEventListener')
    localStorage.setItem(LOCAL_STORAGE_KEY, 'vl_sk_x')
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
    unmount()
    expect(remove).toHaveBeenCalledWith('storage', expect.any(Function))
    remove.mockRestore()
  })
})
