import { describe, it, expect } from 'vitest'
import { cacheHitRate, formatHitRate, sumSavings } from './cache'

describe('cacheHitRate', () => {
  it('returns null when there are no prompt tokens', () => {
    expect(cacheHitRate(10, 0)).toBeNull()
    expect(cacheHitRate(0, -5)).toBeNull()
  })
  it('computes the ratio', () => {
    expect(cacheHitRate(250, 1000)).toBe(0.25)
    expect(cacheHitRate(0, 1000)).toBe(0)
  })
  it('clamps to 1', () => {
    expect(cacheHitRate(2000, 1000)).toBe(1)
  })
})

describe('formatHitRate', () => {
  it('formats null as a hyphen', () => {
    expect(formatHitRate(null)).toBe('-')
  })
  it('formats with one decimal', () => {
    expect(formatHitRate(0.25)).toBe('25.0%')
    expect(formatHitRate(1)).toBe('100.0%')
    expect(formatHitRate(0)).toBe('0.0%')
  })
})

describe('sumSavings', () => {
  it('returns null when no row has savings', () => {
    expect(sumSavings([])).toBeNull()
    expect(sumSavings([{}, { cache_savings: null }])).toBeNull()
  })
  it('sums non-null values including negatives and zero', () => {
    expect(sumSavings([{ cache_savings: 1.5 }, { cache_savings: null }, { cache_savings: -0.5 }])).toBe(1)
    expect(sumSavings([{ cache_savings: 0 }])).toBe(0)
  })
})
