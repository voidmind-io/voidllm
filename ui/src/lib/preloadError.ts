const PRELOAD_RELOAD_KEY = 'voidllm_preload_reload'

/** Reloads once when a lazy chunk fails to load (stale deploy); guarded against loops. */
export function handlePreloadError(): void {
  if (sessionStorage.getItem(PRELOAD_RELOAD_KEY) === '1') return
  sessionStorage.setItem(PRELOAD_RELOAD_KEY, '1')
  window.location.reload()
}
