import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { Sidebar } from './Sidebar'
import { LOCAL_STORAGE_KEY, SESSION_EXPIRY_KEY } from '../../lib/constants'

const originalLocation = window.location

function mockFetch(logout: () => Promise<Response>) {
  const fn = vi.fn(async (url: string) => {
    if (url.endsWith('/auth/logout')) return logout()
    if (url.endsWith('/license')) {
      return new Response(JSON.stringify({ plan: 'free', features: [] }), { status: 200 })
    }
    return new Response(
      JSON.stringify({ id: 'u1', email: 'a@example.com', display_name: 'A', role: 'member', is_system_admin: false }),
      { status: 200 },
    )
  })
  vi.stubGlobal('fetch', fn)
  return fn
}

function renderSidebar() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <Sidebar />
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

beforeEach(() => {
  localStorage.setItem(LOCAL_STORAGE_KEY, 'vl_sk_x')
  localStorage.setItem(SESSION_EXPIRY_KEY, '2099-01-01T00:00:00Z')
  Object.defineProperty(window, 'location', {
    configurable: true,
    value: { href: '/' },
  })
})

afterEach(() => {
  Object.defineProperty(window, 'location', { configurable: true, value: originalLocation })
  vi.unstubAllGlobals()
  localStorage.clear()
})

describe('Sidebar logout', () => {
  it('calls the logout endpoint, clears storage, and goes to /login', async () => {
    const fetchMock = mockFetch(async () => new Response(null, { status: 204 }))
    renderSidebar()
    await userEvent.click(screen.getByRole('button', { name: 'Logout' }))
    await waitFor(() => expect(window.location.href).toBe('/login'))
    const urls = fetchMock.mock.calls.map((c) => c[0])
    expect(urls).toContain('/api/v1/auth/logout')
    expect(localStorage.getItem(LOCAL_STORAGE_KEY)).toBeNull()
    expect(localStorage.getItem(SESSION_EXPIRY_KEY)).toBeNull()
  })

  it('still clears storage and navigates when the call fails', async () => {
    mockFetch(() => Promise.reject(new TypeError('network down')))
    renderSidebar()
    await userEvent.click(screen.getByRole('button', { name: 'Logout' }))
    await waitFor(() => expect(window.location.href).toBe('/login'))
    expect(localStorage.getItem(LOCAL_STORAGE_KEY)).toBeNull()
    expect(localStorage.getItem(SESSION_EXPIRY_KEY)).toBeNull()
  })
})
