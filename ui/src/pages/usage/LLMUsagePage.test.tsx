import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { describe, it, expect, vi, beforeEach } from 'vitest'
import LLMUsagePage from './LLMUsagePage'
import { exportData } from '../../lib/export'

vi.mock('../../lib/export', () => ({ exportData: vi.fn() }))

function row(overrides: Record<string, unknown>) {
  return {
    group_key: 'gpt-4o',
    total_requests: 10,
    prompt_tokens: 1000,
    completion_tokens: 200,
    total_tokens: 1200,
    cost_estimate: 0.5,
    avg_duration_ms: 100,
    cached_read_tokens: 250,
    cache_write_tokens: 40,
    ...overrides,
  }
}

function stubFetch(data: unknown[]) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === 'string' ? input : input.toString()
      const body = url.includes('/api/v1/me')
        ? { id: 'u1', email: 'a@b.c', display_name: 'A', role: 'org_admin', org_id: 'org-1', is_system_admin: false }
        : { org_id: 'org-1', from: '', to: '', group_by: 'model', data }
      return { ok: true, status: 200, json: () => Promise.resolve(body) }
    }),
  )
}

function renderPage() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <LLMUsagePage />
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('LLMUsagePage cache columns', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
    vi.mocked(exportData).mockClear()
  })

  it('renders cache column headers', async () => {
    stubFetch([row({})])
    renderPage()
    expect(await screen.findByText('Cached Read Tokens')).toBeInTheDocument()
    expect(screen.getByText('Cache Write Tokens')).toBeInTheDocument()
  })

  it('shows the cache hit rate', async () => {
    stubFetch([row({})])
    renderPage()
    const card = await screen.findByRole('group', { name: 'Cache Hit Rate' })
    await waitFor(() => expect(card).toHaveTextContent('25.0%'))
  })

  it('shows a hyphen when there are no prompt tokens', async () => {
    stubFetch([row({ prompt_tokens: 0, total_tokens: 200, cached_read_tokens: 0, cache_write_tokens: 0 })])
    renderPage()
    const card = await screen.findByRole('group', { name: 'Cache Hit Rate' })
    await screen.findAllByText('gpt-4o')
    expect(card).toHaveTextContent('-')
    expect(card).not.toHaveTextContent('%')
  })

  it('includes cache fields in export headers', async () => {
    stubFetch([row({})])
    renderPage()
    await screen.findAllByText('gpt-4o')
    const csv = screen.getByRole('button', { name: /csv/i })
    await waitFor(() => expect(csv).toBeEnabled())
    await userEvent.click(csv)
    const headers = vi.mocked(exportData).mock.calls[0][1]
    const keys = headers.map((h) => h.key)
    expect(keys).toContain('cached_read_tokens')
    expect(keys).toContain('cache_write_tokens')
  })
})
