import { describe, it, expect, vi, beforeEach } from 'vitest'
import { logoutRequest } from './auth'

beforeEach(() => {
  localStorage.clear()
  localStorage.setItem('voidllm_session', 'vl_sk_x')
  vi.unstubAllGlobals()
})

function expectLogoutCall(fetchMock: ReturnType<typeof vi.fn>) {
  expect(fetchMock).toHaveBeenCalledTimes(1)
  const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit]
  expect(url).toBe('/api/v1/auth/logout')
  expect(init.method).toBe('POST')
  expect((init.headers as Record<string, string>).Authorization).toBe('Bearer vl_sk_x')
  expect(init.signal).toBeDefined()
}

describe('logoutRequest', () => {
  it('POSTs to /auth/logout with the session token', async () => {
    const fetchMock = vi.fn(async () => new Response(null, { status: 204 }))
    vi.stubGlobal('fetch', fetchMock)
    await logoutRequest()
    expectLogoutCall(fetchMock)
  })

  it('swallows network failures after calling fetch', async () => {
    const fetchMock = vi.fn(async () => {
      throw new TypeError('network')
    })
    vi.stubGlobal('fetch', fetchMock)
    await expect(logoutRequest()).resolves.toBeUndefined()
    expectLogoutCall(fetchMock)
  })

  it('swallows server errors and does not trigger the 401 redirect', async () => {
    const fetchMock = vi.fn(async () => new Response('{}', { status: 401 }))
    vi.stubGlobal('fetch', fetchMock)
    await expect(logoutRequest()).resolves.toBeUndefined()
    expectLogoutCall(fetchMock)
    expect(localStorage.getItem('voidllm_session')).toBe('vl_sk_x')
  })
})
