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
    vi.stubGlobal('location', { href: '/dash' })
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
    expect(window.location.href).toBe('/login')
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
})
