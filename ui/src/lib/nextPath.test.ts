import { describe, it, expect } from 'vitest'
import { safeNextPath } from './nextPath'

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
