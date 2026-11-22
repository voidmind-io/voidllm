import { render, screen, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { describe, it, expect, vi, afterEach } from 'vitest'
import AuditLogPage from './AuditLogPage'

function mockFetch(features: string[]) {
  const fn = vi.fn(async (url: string) => {
    let body: unknown = { data: [], has_more: false }
    if (url.endsWith('/me')) body = { org_id: 'org-1' }
    else if (url.endsWith('/license')) body = { edition: 'x', valid: true, features }
    return { ok: true, status: 200, statusText: 'ok', json: () => Promise.resolve(body) }
  })
  vi.stubGlobal('fetch', fn)
  return fn
}

function renderPage() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <AuditLogPage />
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

const auditCalls = (fn: ReturnType<typeof mockFetch>) =>
  fn.mock.calls.filter((c) => String(c[0]).includes('/audit-logs'))

describe('AuditLogPage license gating', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('makes no audit request without the audit_logs feature', async () => {
    const fn = mockFetch([])
    renderPage()
    await screen.findByText(/requires an Enterprise license/i)
    expect(auditCalls(fn)).toHaveLength(0)
  })

  it('requests audit logs with the audit_logs feature', async () => {
    const fn = mockFetch(['audit_logs'])
    renderPage()
    await waitFor(() => expect(auditCalls(fn).length).toBeGreaterThan(0))
  })
})
