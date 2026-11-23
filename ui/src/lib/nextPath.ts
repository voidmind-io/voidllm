/** sessionStorage key holding the post-login destination across the SSO redirect. */
export const NEXT_PATH_KEY = 'voidllm_next'

const MAX_NEXT_LENGTH = 2048

/**
 * Returns raw only when it is a same-origin relative path that is safe to
 * navigate to after login; otherwise returns '/'.
 */
export function safeNextPath(raw: string | null | undefined): string {
  if (typeof raw !== 'string' || raw.length === 0 || raw.length > MAX_NEXT_LENGTH) return '/'
  if (raw[0] !== '/' || raw[1] === '/' || raw[1] === '\\') return '/'
  if (raw.includes('\\')) return '/'
  // eslint-disable-next-line no-control-regex
  if (/[\u0000-\u001f\u007f]/.test(raw)) return '/'
  let url: URL
  try {
    url = new URL(raw, window.location.origin)
  } catch {
    return '/'
  }
  if (url.origin !== window.location.origin) return '/'
  const lowerPath = url.pathname.toLowerCase()
  if (lowerPath.startsWith('/login') || lowerPath.startsWith('/auth/')) return '/'
  return raw
}

/** Builds the login URL carrying the current location as the return target. */
export function loginUrlWithNext(): string {
  const { pathname, search, hash } = window.location
  if (pathname.startsWith('/login')) return '/login'
  return `/login?next=${encodeURIComponent(pathname + search + hash)}`
}

/** Stored next paths older than this are ignored. */
export const NEXT_PATH_TTL_MS = 10 * 60 * 1000

/** Persists the validated post-login destination with a timestamp. */
export function storeNextPath(path: string): void {
  sessionStorage.setItem(NEXT_PATH_KEY, JSON.stringify({ path: safeNextPath(path), ts: Date.now() }))
}

/** Removes any stored post-login destination. */
export function clearNextPath(): void {
  sessionStorage.removeItem(NEXT_PATH_KEY)
}

/** Reads and removes the stored destination; returns '/' when missing, malformed, legacy or expired. */
export function consumeNextPath(): string {
  const raw = sessionStorage.getItem(NEXT_PATH_KEY)
  clearNextPath()
  if (raw === null) return '/'
  let parsed: unknown
  try {
    parsed = JSON.parse(raw)
  } catch {
    return '/'
  }
  if (typeof parsed !== 'object' || parsed === null) return '/'
  const { path, ts } = parsed as { path?: unknown; ts?: unknown }
  if (typeof path !== 'string' || typeof ts !== 'number') return '/'
  if (Date.now() - ts > NEXT_PATH_TTL_MS) return '/'
  return safeNextPath(path)
}
