import { describe, it, expect, vi, afterEach } from 'vitest'
import apiClient, { ApiError } from './client'
import { queryClient } from '../lib/queryClient'

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('apiClient errors', () => {
  it('throws ApiError carrying status and message', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => ({
        ok: false,
        status: 403,
        statusText: 'Forbidden',
        json: () => Promise.resolve({ error: { message: 'nope' } }),
      })),
    )
    const err = await apiClient('/x').catch((e: unknown) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect((err as ApiError).status).toBe(403)
    expect((err as ApiError).message).toBe('nope')
  })
})

describe('apiClient 401', () => {
  it('throws a non-retryable ApiError(401) and clears the session', async () => {
    vi.stubGlobal('location', { href: '/dash', pathname: '/dash', search: '?a=1' })
    localStorage.setItem('voidllm_session', 'vl_sk_x')
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => ({
        ok: false,
        status: 401,
        statusText: 'Unauthorized',
        json: () => Promise.resolve({}),
      })),
    )
    const err = await apiClient('/x').catch((e: unknown) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect((err as ApiError).status).toBe(401)
    expect((err as ApiError).message).toBe('Session expired')
    expect(window.location.href).toBe('/login?next=%2Fdash%3Fa%3D1')
    const retry = queryClient.getDefaultOptions().queries?.retry as (
      count: number,
      error: unknown,
    ) => boolean
    expect(retry(0, err)).toBe(false)
  })
})

describe('queryClient retry policy', () => {
  const retry = queryClient.getDefaultOptions().queries?.retry as (
    count: number,
    error: unknown,
  ) => boolean

  it('does not retry 4xx', () => {
    expect(retry(0, new ApiError(403, 'x'))).toBe(false)
    expect(retry(0, new ApiError(404, 'x'))).toBe(false)
  })

  it('retries once on 5xx and network errors', () => {
    expect(retry(0, new ApiError(500, 'x'))).toBe(true)
    expect(retry(0, new TypeError('Failed to fetch'))).toBe(true)
    expect(retry(1, new ApiError(500, 'x'))).toBe(false)
  })

  it('retries once on timeouts (status 0)', () => {
    expect(retry(0, new ApiError(0, 'Request timed out'))).toBe(true)
    expect(retry(1, new ApiError(0, 'Request timed out'))).toBe(false)
  })
})

describe('apiClient timeout and cancellation', () => {
  function hangingFetch() {
    return vi.fn(
      (_url: string, init?: RequestInit) =>
        new Promise((_resolve, reject) => {
          init?.signal?.addEventListener('abort', () => reject(init.signal?.reason))
        }),
    )
  }

  it('throws ApiError(0) when the timeout elapses', async () => {
    vi.stubGlobal('fetch', hangingFetch())
    const err = await apiClient('/x', { timeoutMs: 10 }).catch((e: unknown) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect((err as ApiError).status).toBe(0)
    expect((err as ApiError).message).toBe('Request timed out')
  })

  it('propagates the caller abort instead of a timeout error', async () => {
    vi.stubGlobal('fetch', hangingFetch())
    const controller = new AbortController()
    const promise = apiClient('/x', { signal: controller.signal }).catch((e: unknown) => e)
    controller.abort()
    const err = await promise
    expect(err).not.toBeInstanceOf(ApiError)
    expect((err as DOMException).name).toBe('AbortError')
  })

  it('passes a signal to fetch and none when timeoutMs is null and no caller signal', async () => {
    const fetchMock = vi.fn(async () => ({ ok: true, status: 200, json: () => Promise.resolve({}) }))
    vi.stubGlobal('fetch', fetchMock)
    await apiClient('/x')
    expect((fetchMock.mock.calls[0] as unknown[])[1]).toMatchObject({ signal: expect.any(AbortSignal) })
    await apiClient('/x', { timeoutMs: null })
    expect(((fetchMock.mock.calls[1] as unknown[])[1] as RequestInit).signal).toBeUndefined()
  })
})
