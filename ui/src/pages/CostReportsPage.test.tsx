import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { describe, it, expect, vi, beforeEach } from 'vitest'
import CostReportsPage from './CostReportsPage'
import { exportData } from '../lib/export'

vi.mock('../lib/export', () => ({ exportData: vi.fn() }))

function row(key: string, overrides: Record<string, unknown>) {
  return {
    group_key: key,
    total_requests: 10,
    prompt_tokens: 1000,
    completion_tokens: 200,
    total_tokens: 1200,
    cost_estimate: 1,
    avg_duration_ms: 100,
    cached_read_tokens: 250,
    cache_write_tokens: 0,
    ...overrides,
  }
}

function stubFetch(modelRows: unknown[]) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === 'string' ? input : input.toString()
      let body: unknown
      if (url.includes('/api/v1/me')) {
        body = { id: 'u1', email: 'a@b.c', display_name: 'A', role: 'org_admin', org_id: 'org-1', is_system_admin: false }
      } else if (url.includes('/api/v1/license')) {
        body = { edition: 'enterprise', valid: true, features: ['cost_reports'], max_orgs: -1, max_teams: -1 }
      } else {
        const groupBy = url.includes('group_by=day') ? 'day' : 'model'
        body = { org_id: 'org-1', from: '', to: '', group_by: groupBy, data: groupBy === 'model' ? modelRows : [] }
      }
      return { ok: true, status: 200, json: () => Promise.resolve(body) }
    }),
  )
}

function renderPage() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <CostReportsPage />
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

async function findRow(name: string): Promise<HTMLElement> {
  const els = await screen.findAllByText(name)
  const tr = els.map((e) => e.closest('tr')).find((t) => t !== null)
  if (!tr) throw new Error('row not found: ' + name)
  return tr
}

describe('CostReportsPage cache savings', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
    vi.mocked(exportData).mockClear()
  })

  it('sums non-null savings in the stat card', async () => {
    stubFetch([
      row('gpt-4o', { cache_savings: 0.5 }),
      row('claude', { cache_savings: 0.25 }),
      row('local', {}),
    ])
    renderPage()
    const card = await screen.findByRole('group', { name: 'Est. Cache Savings' })
    await waitFor(() => expect(card).toHaveTextContent('$0.75'))
  })

  it('shows a hyphen when no model has savings', async () => {
    stubFetch([row('local', {})])
    renderPage()
    const card = await screen.findByRole('group', { name: 'Est. Cache Savings' })
    await screen.findAllByText('local')
    expect(card).toHaveTextContent('-')
    expect(card).not.toHaveTextContent('$')
  })

  it('renders per-model savings, hit rate and hyphen for unknown savings', async () => {
    stubFetch([row('gpt-4o', { cache_savings: 0.5 }), row('local', { cost_estimate: 0.1 })])
    renderPage()
    const gpt = await findRow('gpt-4o')
    const local = await findRow('local')
    expect(within(gpt).getByText('$0.50')).toBeInTheDocument()
    expect(within(gpt).getByText('25.0%')).toBeInTheDocument()
    expect(within(local).getAllByText('-').length).toBeGreaterThan(0)
  })

  it('includes cache fields in the export headers', async () => {
    stubFetch([row('gpt-4o', { cache_savings: 0.5 })])
    renderPage()
    await screen.findAllByText('gpt-4o')
    const csv = screen.getByRole('button', { name: /csv/i })
    await waitFor(() => expect(csv).toBeEnabled())
    await userEvent.click(csv)
    const keys = vi.mocked(exportData).mock.calls[0][1].map((h) => h.key)
    expect(keys).toEqual(expect.arrayContaining(['cache_savings', 'cached_read_tokens', 'cache_write_tokens']))
  })
})
