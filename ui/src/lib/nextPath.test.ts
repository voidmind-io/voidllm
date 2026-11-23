import { describe, it, expect } from 'vitest'
import { NEXT_PATH_KEY, NEXT_PATH_TTL_MS, consumeNextPath, safeNextPath, storeNextPath } from './nextPath'

describe('safeNextPath', () => {
  it.each(['/keys', '/usage/llm?range=7d', '/teams/abc#x'])('allows %s', (p) => {
    expect(safeNextPath(p)).toBe(p)
  })

  it.each([
    '//evil.com',
    '/\\evil.com',
    '/a\\b',
    'https://evil.com',
    'javascript:alert(1)',
    '/login',
    '/login?next=/keys',
    '/auth/callback',
    '/LOGIN',
    '/Login?next=/keys',
    '/Auth/callback',
    '/AUTH/x',
    '/ke\nys',
    '/ke\u0000ys',
    '',
    'keys',
    null,
    undefined,
    '/' + 'a'.repeat(2048),
  ])('rejects %j', (p) => {
    expect(safeNextPath(p)).toBe('/')
  })
})

describe('consumeNextPath', () => {
  it('returns a fresh stored path and removes it', () => {
    storeNextPath('/keys')
    expect(consumeNextPath()).toBe('/keys')
    expect(sessionStorage.getItem(NEXT_PATH_KEY)).toBeNull()
  })

  it('ignores an entry older than 10 minutes', () => {
    sessionStorage.setItem(NEXT_PATH_KEY, JSON.stringify({ path: '/keys', ts: Date.now() - NEXT_PATH_TTL_MS - 1 }))
    expect(consumeNextPath()).toBe('/')
    expect(sessionStorage.getItem(NEXT_PATH_KEY)).toBeNull()
  })

  it('ignores legacy and malformed values', () => {
    sessionStorage.setItem(NEXT_PATH_KEY, '/keys')
    expect(consumeNextPath()).toBe('/')
    sessionStorage.setItem(NEXT_PATH_KEY, JSON.stringify({ path: 5, ts: 'x' }))
    expect(consumeNextPath()).toBe('/')
  })

  it('re-validates the stored path', () => {
    sessionStorage.setItem(NEXT_PATH_KEY, JSON.stringify({ path: '//evil.com', ts: Date.now() }))
    expect(consumeNextPath()).toBe('/')
  })
})
