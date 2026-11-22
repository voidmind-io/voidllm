import apiClient from './client'

const LOGOUT_TIMEOUT_MS = 3000

/**
 * Best-effort server-side logout. Revokes the session on the server; any
 * failure (network, timeout, 401) is ignored so local logout always proceeds.
 */
export async function logoutRequest(): Promise<void> {
  try {
    await apiClient<void>('/auth/logout', {
      method: 'POST',
      signal: AbortSignal.timeout(LOGOUT_TIMEOUT_MS),
    })
  } catch {
    // Ignored on purpose: the client clears its session regardless.
  }
}
