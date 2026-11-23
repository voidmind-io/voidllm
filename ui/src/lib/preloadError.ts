const PRELOAD_RELOAD_KEY = 'voidllm_preload_reload'
const GUARD_RESET_DELAY_MS = 5_000

/** Reloads once when a lazy chunk fails to load (stale deploy); guarded against loops. */
export function handlePreloadError(event: Event): void {
  if (sessionStorage.getItem(PRELOAD_RELOAD_KEY) === '1') return
  sessionStorage.setItem(PRELOAD_RELOAD_KEY, '1')
  event.preventDefault()
  window.location.reload()
}

/** Clears the reload guard so a later deploy in the same tab can auto-reload again. */
export function clearPreloadReloadGuard(): void {
  sessionStorage.removeItem(PRELOAD_RELOAD_KEY)
}

/** Clears the reload guard once the app has been running for a short while. */
export function scheduleReloadGuardReset(): void {
  window.setTimeout(clearPreloadReloadGuard, GUARD_RESET_DELAY_MS)
}
