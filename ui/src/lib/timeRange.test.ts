import { describe, it, expect, vi, afterEach } from 'vitest'
import { resolveRange } from './timeRange'

afterEach(() => {
  vi.useRealTimers()
})

describe('resolveRange', () => {
  it('resolves fixed ranges relative to the current time', () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-03-10T12:00:00Z'))
    const r = resolveRange('7d')
    expect(r.to).toBe('2026-03-10T12:00:00.000Z')
    expect(r.from).toBe('2026-03-03T12:00:00.000Z')
  })

  it('moves the window forward as time passes', () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-03-10T12:00:00Z'))
    const first = resolveRange('24h')
    vi.setSystemTime(new Date('2026-03-10T12:05:00Z'))
    const second = resolveRange('24h')
    expect(second.to).not.toBe(first.to)
    expect(new Date(second.to).getTime() - new Date(first.to).getTime()).toBe(300_000)
  })

  it('supports calendar variants', () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-03-10T12:00:00Z'))
    const r = resolveRange('24h-cal')
    expect(new Date(r.to).getTime() - new Date(r.from).getTime()).toBeGreaterThan(0)
    expect(r.to).toBe('2026-03-10T12:00:00.000Z')
  })
})
