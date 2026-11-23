import { useSyncExternalStore } from 'react'

const TICK_MS = 60_000

let now = Date.now()
let timer: ReturnType<typeof setInterval> | null = null
const listeners = new Set<() => void>()

function subscribe(listener: () => void): () => void {
  listeners.add(listener)
  if (timer === null) {
    now = Date.now()
    timer = setInterval(() => {
      now = Date.now()
      listeners.forEach((l) => l())
    }, TICK_MS)
  }
  return () => {
    listeners.delete(listener)
    if (listeners.size === 0 && timer !== null) {
      clearInterval(timer)
      timer = null
    }
  }
}

function getSnapshot(): number {
  return now
}

/** Returns the current time in epoch ms, refreshed every minute via one shared ticker. */
export function useNow(): number {
  return useSyncExternalStore(subscribe, getSnapshot)
}
