import { describe, it, expect, beforeEach } from 'vitest'
import { LOCAL_STORAGE_KEY, SESSION_EXPIRY_KEY } from './constants'
import { clearSession, isSessionExpired, msUntilSessionExpiry, storeSession } from './session'

beforeEach(() => localStorage.clear())

describe('session storage helpers', () => {
  it('stores token and expiry, and clears both', () => {
    storeSession('vl_sk_x', '2030-01-01T00:00:00Z')
    expect(localStorage.getItem(LOCAL_STORAGE_KEY)).toBe('vl_sk_x')
    expect(localStorage.getItem(SESSION_EXPIRY_KEY)).toBe('2030-01-01T00:00:00Z')
    clearSession()
    expect(localStorage.getItem(LOCAL_STORAGE_KEY)).toBeNull()
    expect(localStorage.getItem(SESSION_EXPIRY_KEY)).toBeNull()
  })

  it('removes a stale expiry when storing a token without one', () => {
    localStorage.setItem(SESSION_EXPIRY_KEY, '2020-01-01T00:00:00Z')
    storeSession('vl_sk_x')
    expect(localStorage.getItem(SESSION_EXPIRY_KEY)).toBeNull()
  })

  it('detects expiry', () => {
    expect(isSessionExpired()).toBe(false)
    storeSession('t', '2020-01-01T00:00:00Z')
    expect(isSessionExpired()).toBe(true)
    expect(msUntilSessionExpiry()).toBe(0)
    storeSession('t', new Date(Date.now() + 60_000).toISOString())
    expect(isSessionExpired()).toBe(false)
  })
})
