import { LOCAL_STORAGE_KEY, SESSION_EXPIRY_KEY } from './constants'

/** Largest delay setTimeout accepts (about 24.8 days). */
const MAX_TIMEOUT_MS = 2_147_483_647

/** Stores the session token and its expiry (when known). */
export function storeSession(token: string, expiresAt?: string): void {
  localStorage.setItem(LOCAL_STORAGE_KEY, token)
  if (expiresAt) {
    localStorage.setItem(SESSION_EXPIRY_KEY, expiresAt)
  } else {
    localStorage.removeItem(SESSION_EXPIRY_KEY)
  }
}

/** Removes the session token and its expiry from storage. */
export function clearSession(): void {
  localStorage.removeItem(LOCAL_STORAGE_KEY)
  localStorage.removeItem(SESSION_EXPIRY_KEY)
}

/** Returns the stored session expiry in epoch ms, or null if unknown or unparsable. */
export function getSessionExpiryMs(): number | null {
  const raw = localStorage.getItem(SESSION_EXPIRY_KEY)
  if (!raw) return null
  const ms = Date.parse(raw)
  return Number.isNaN(ms) ? null : ms
}

/** True when a stored expiry exists and has passed. */
export function isSessionExpired(now: number = Date.now()): boolean {
  const ms = getSessionExpiryMs()
  return ms !== null && ms <= now
}

/** Milliseconds until expiry clamped to setTimeout limits, or null if no expiry is known. */
export function msUntilSessionExpiry(now: number = Date.now()): number | null {
  const ms = getSessionExpiryMs()
  if (ms === null) return null
  return Math.min(Math.max(ms - now, 0), MAX_TIMEOUT_MS)
}
