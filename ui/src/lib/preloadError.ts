const PRELOAD_RELOAD_KEY = 'voidllm_preload_reload'
const RELOAD_COOLDOWN_MS = 60_000

/** Reloads when a lazy chunk fails to load (stale deploy), at most once per minute. */
export function handlePreloadError(event: Event): void {
  const last = Number(sessionStorage.getItem(PRELOAD_RELOAD_KEY))
  const now = Date.now()
  if (Number.isFinite(last) && last > 0 && now - last <= RELOAD_COOLDOWN_MS) return
  sessionStorage.setItem(PRELOAD_RELOAD_KEY, String(now))
  event.preventDefault()
  window.location.reload()
}
