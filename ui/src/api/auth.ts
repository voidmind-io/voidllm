import { LOCAL_STORAGE_KEY } from '../lib/constants'

const LOGOUT_TIMEOUT_MS = 3000

/**
 * Best-effort server-side logout. Uses fetch directly (not apiClient) so the
 * client's 401 handler does not fire mid-logout. All failures are swallowed so
 * local logout always proceeds.
 */
export async function logoutRequest(): Promise<void> {
  try {
    await fetch('/api/v1/auth/logout', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Authorization: `Bearer ${localStorage.getItem(LOCAL_STORAGE_KEY) ?? ''}`,
      },
      signal: AbortSignal.timeout(LOGOUT_TIMEOUT_MS),
    })
  } catch {
    // Ignored on purpose: the client clears its session regardless.
  }
}
