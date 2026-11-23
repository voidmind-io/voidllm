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
  if (url.pathname.startsWith('/login') || url.pathname.startsWith('/auth/')) return '/'
  return raw
}

/** Builds the login URL carrying the current location as the return target. */
export function loginUrlWithNext(): string {
  const { pathname, search } = window.location
  if (pathname.startsWith('/login')) return '/login'
  return `/login?next=${encodeURIComponent(pathname + search)}`
}
