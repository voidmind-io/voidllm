import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Routes, Route } from 'react-router-dom'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { describe, it, expect, vi, afterEach } from 'vitest'
import LoginPage from './LoginPage'
import CallbackPage from './CallbackPage'

function mockFetch() {
  return vi.fn(async (url: string) => {
    if (url.endsWith('/auth/providers')) {
      return { ok: true, status: 200, json: () => Promise.resolve({ local: true, oidc: true }) }
    }
    return {
      ok: true,
      status: 200,
      json: () =>
        Promise.resolve({ token: 'vl_sk_t', expires_at: '2099-01-01T00:00:00Z', user: { id: 'u' } }),
    }
  })
}

function renderLogin(path: string) {
  return render(
    <QueryClientProvider client={new QueryClient()}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/login" element={<LoginPage />} />
          <Route path="/keys" element={<div>keys page</div>} />
          <Route path="/" element={<div>home page</div>} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

async function signIn() {
  await userEvent.type(screen.getByLabelText('Email'), 'a@b.co')
  await userEvent.type(screen.getByLabelText('Password'), 'secret')
  await userEvent.click(screen.getByRole('button', { name: 'Sign in' }))
}

afterEach(() => {
  vi.unstubAllGlobals()
  localStorage.clear()
  sessionStorage.clear()
})

describe('LoginPage return path', () => {
  it('navigates to a safe next after login', async () => {
    vi.stubGlobal('fetch', mockFetch())
    renderLogin('/login?next=%2Fkeys')
    await signIn()
    expect(await screen.findByText('keys page')).toBeInTheDocument()
  })

  it('ignores an unsafe next', async () => {
    vi.stubGlobal('fetch', mockFetch())
    renderLogin('/login?next=%2F%2Fevil.com')
    await signIn()
    expect(await screen.findByText('home page')).toBeInTheDocument()
  })

  it('stores the validated next before starting SSO', async () => {
    vi.stubGlobal('fetch', mockFetch())
    renderLogin('/login?next=%2Fkeys')
    const button = await screen.findByRole('button', { name: 'Sign in with SSO' })
    button.closest('a')?.addEventListener('click', (e) => e.preventDefault())
    await userEvent.click(button)
    await waitFor(() => expect(sessionStorage.getItem('voidllm_next')).toBe('/keys'))
  })

  it('keeps an earlier stored next when SSO is clicked without a next param', async () => {
    vi.stubGlobal('fetch', mockFetch())
    sessionStorage.setItem('voidllm_next', '/keys')
    renderLogin('/login?error=sso_error')
    const button = await screen.findByRole('button', { name: 'Sign in with SSO' })
    button.closest('a')?.addEventListener('click', (e) => e.preventDefault())
    await userEvent.click(button)
    expect(sessionStorage.getItem('voidllm_next')).toBe('/keys')
  })

  it('shows messages for new SSO error codes', () => {
    vi.stubGlobal('fetch', mockFetch())
    renderLogin('/login?error=invalid_state')
    expect(screen.getByText(/SSO sign-in expired/)).toBeInTheDocument()
  })
})

describe('CallbackPage', () => {
  it('goes to the stored next and clears it', () => {
    const loc = { href: '', origin: 'http://localhost' }
    vi.stubGlobal('location', loc)
    document.cookie = 'voidllm_oidc_token=vl_sk_t'
    sessionStorage.setItem('voidllm_next', '/usage/llm?range=7d')
    render(<CallbackPage />)
    expect(loc.href).toBe('/usage/llm?range=7d')
    expect(sessionStorage.getItem('voidllm_next')).toBeNull()
  })

  it('re-validates the stored next', () => {
    const loc = { href: '', origin: 'http://localhost' }
    vi.stubGlobal('location', loc)
    document.cookie = 'voidllm_oidc_token=vl_sk_t'
    sessionStorage.setItem('voidllm_next', '//evil.com')
    render(<CallbackPage />)
    expect(loc.href).toBe('/')
  })
})
