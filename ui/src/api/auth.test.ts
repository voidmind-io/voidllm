import { describe, it, expect, vi, beforeEach } from 'vitest'
import { logoutRequest } from './auth'

beforeEach(() => {
  localStorage.clear()
  vi.unstubAllGlobals()
})

describe('logoutRequest', () => {
  it('POSTs to /auth/logout with the session token', async () => {
    localStorage.setItem('voidllm_session', 'vl_sk_x')
    const fetchMock = vi.fn(async () => new Response(null, { status: 204 }))
    vi.stubGlobal('fetch', fetchMock)
    await logoutRequest()
    const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit]
    expect(url).toBe('/api/v1/auth/logout')
    expect(init.method).toBe('POST')
    expect((init.headers as Record<string, string>).Authorization).toBe('Bearer vl_sk_x')
  })

  it('swallows network failures', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => { throw new TypeError('network') }))
    await expect(logoutRequest()).resolves.toBeUndefined()
  })

  it('swallows server errors', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response('{}', { status: 500 })))
    await expect(logoutRequest()).resolves.toBeUndefined()
  })
})
