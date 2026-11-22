import { act, render, screen } from '@testing-library/react'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { describe, it, expect, vi, afterEach } from 'vitest'
import { RequireAuth } from './App'
import { LOCAL_STORAGE_KEY, SESSION_EXPIRY_KEY } from './lib/constants'

vi.mock('./components/layout/Shell', () => ({
  Shell: () => <div>shell</div>,
}))

function renderGuard() {
  render(
    <MemoryRouter initialEntries={['/']}>
      <Routes>
        <Route path="/login" element={<div>login page</div>} />
        <Route element={<RequireAuth />}>
          <Route index element={<div>home</div>} />
        </Route>
      </Routes>
    </MemoryRouter>,
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
    renderGuard()
    expect(screen.getByText('login page')).toBeInTheDocument()
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
    renderGuard()
    expect(screen.getByText('shell')).toBeInTheDocument()
    act(() => {
      vi.advanceTimersByTime(6_000)
    })
    expect(screen.getByText('login page')).toBeInTheDocument()
    expect(localStorage.getItem(LOCAL_STORAGE_KEY)).toBeNull()
  })
})
