import { render, screen } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { ToastProvider } from '../hooks/useToast'
import SettingsPage from './SettingsPage'

const ORG = {
  id: 'org-1',
  name: 'Acme',
  slug: 'acme',
  timezone: null,
  daily_token_limit: 0,
  monthly_token_limit: 0,
  requests_per_minute: 0,
  requests_per_day: 0,
  created_at: '',
  updated_at: '',
}

function stub(role: string, isSystemAdmin = false) {
  const urls: string[] = []
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
    const url = typeof input === 'string' ? input : input.toString()
    urls.push(url)
    const body = url.endsWith('/api/v1/me')
      ? { id: 'u1', email: 'a@b.c', display_name: 'A', role, org_id: 'org-1', is_system_admin: isSystemAdmin }
      : ORG
    return { ok: true, status: 200, statusText: 'ok', json: () => Promise.resolve(body) }
  }))
  return urls
}

function renderPage() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={queryClient}>
      <ToastProvider>
        <MemoryRouter>
          <SettingsPage />
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  )
}

describe('SettingsPage org request gating', () => {
  beforeEach(() => {
    vi.unstubAllGlobals()
    localStorage.clear()
  })

  it('does not request the org for members and shows the admin-only notice', async () => {
    const urls = stub('member')
    renderPage()
    expect(
      await screen.findByText('Only organization admins can view organization settings.'),
    ).toBeInTheDocument()
    expect(urls.some((u) => u.endsWith('/orgs/org-1'))).toBe(false)
  })

  it('requests the org for admins', async () => {
    const urls = stub('org_admin')
    renderPage()
    expect(await screen.findByDisplayValue('Acme')).toBeInTheDocument()
    expect(urls.some((u) => u.endsWith('/orgs/org-1'))).toBe(true)
  })
})
