import { renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { describe, it, expect, vi, afterEach } from 'vitest'
import { useUsage, useMyUsage } from './useUsage'

function wrapper() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const Wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  )
  return { client, Wrapper }
}

function stubFetch() {
  const fn = vi.fn(async () => ({
    ok: true,
    status: 200,
    json: () => Promise.resolve({ org_id: 'o', from: '', to: '', group_by: 'day', data: [] }),
  }))
  vi.stubGlobal('fetch', fn)
  return fn
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('usage hooks', () => {
  it('keys on the range descriptor and resolves the window at fetch time', async () => {
    const fetchFn = stubFetch()
    const { client, Wrapper } = wrapper()
    const { result } = renderHook(() => useUsage('org-1', '7d', 'day'), { wrapper: Wrapper })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(client.getQueryCache().find({ queryKey: ['usage', 'org-1', '7d', 'day'] })).toBeDefined()

    await client.invalidateQueries({ queryKey: ['usage'] })
    const urls = (fetchFn.mock.calls as unknown as [string][]).map((c) => c[0])
    expect(urls).toHaveLength(2)
    const toOf = (u: string) => new URL(u, 'http://x').searchParams.get('to')
    expect((toOf(urls[1]) ?? '') >= (toOf(urls[0]) ?? '')).toBe(true)
  })

  it('does not fetch when disabled', async () => {
    const fetchFn = stubFetch()
    const { Wrapper } = wrapper()
    renderHook(() => useUsage('org-1', '7d', 'day', { enabled: false }), { wrapper: Wrapper })
    await new Promise((r) => setTimeout(r, 20))
    expect(fetchFn).not.toHaveBeenCalled()
  })

  it('useMyUsage calls /usage/me', async () => {
    const fetchFn = stubFetch()
    const { Wrapper } = wrapper()
    const { result } = renderHook(() => useMyUsage('24h', 'model'), { wrapper: Wrapper })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    const url = (fetchFn.mock.calls as unknown as [string][])[0][0]
    expect(url).toContain('/api/v1/usage/me?')
    expect(url).toContain('group_by=model')
  })
})
