import { renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { describe, it, expect, vi } from 'vitest'
import { useAuditLog } from './useAuditLog'

function setup(cursor: string) {
  vi.stubGlobal('fetch', vi.fn(async () => ({
    ok: true,
    status: 200,
    statusText: 'ok',
    json: () => Promise.resolve({ data: [], has_more: false }),
  })))
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  )
  const { result } = renderHook(
    () =>
      useAuditLog({ orgId: 'org-1', resourceType: '', action: '', range: '24h', limit: 50, cursor }),
    { wrapper },
  )
  return { queryClient, result }
}

describe('useAuditLog refetch', () => {
  it('polls page 1 every 60s', async () => {
    const { queryClient, result } = setup('')
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    const query = queryClient.getQueryCache().getAll()[0]
    expect(query.observers[0].options.refetchInterval).toBe(60_000)
  })

  it('does not poll later pages', async () => {
    const { queryClient, result } = setup('abc')
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    const query = queryClient.getQueryCache().getAll()[0]
    expect(query.observers[0].options.refetchInterval).toBe(false)
  })
})
