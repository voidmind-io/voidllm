import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { handlePreloadError } from './preloadError'

describe('handlePreloadError', () => {
  const reload = vi.fn()

  beforeEach(() => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-01-01T00:00:00Z'))
    sessionStorage.clear()
    reload.mockClear()
    vi.stubGlobal('location', { ...window.location, reload })
  })

  afterEach(() => {
    vi.useRealTimers()
    vi.unstubAllGlobals()
  })

  it('reloads and prevents default on the first error', () => {
    const event = new Event('vite:preloadError', { cancelable: true })
    handlePreloadError(event)
    expect(reload).toHaveBeenCalledTimes(1)
    expect(event.defaultPrevented).toBe(true)
  })

  it('does not reload again within 60 seconds', () => {
    handlePreloadError(new Event('vite:preloadError', { cancelable: true }))
    vi.advanceTimersByTime(59_000)
    handlePreloadError(new Event('vite:preloadError', { cancelable: true }))
    expect(reload).toHaveBeenCalledTimes(1)
  })

  it('reloads again after 60 seconds', () => {
    handlePreloadError(new Event('vite:preloadError', { cancelable: true }))
    vi.advanceTimersByTime(61_000)
    handlePreloadError(new Event('vite:preloadError', { cancelable: true }))
    expect(reload).toHaveBeenCalledTimes(2)
  })
})
