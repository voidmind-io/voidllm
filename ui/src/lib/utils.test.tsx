import { describe, it, expect } from 'vitest'
import { parseLimitInput } from './utils'

describe('parseLimitInput', () => {
  it('treats an empty string as unlimited (0), no error', () => {
    expect(parseLimitInput('')).toEqual({ value: 0 })
  })

  it('accepts "0" as a valid explicit value', () => {
    expect(parseLimitInput('0')).toEqual({ value: 0 })
  })

  it('accepts a positive whole number', () => {
    expect(parseLimitInput('42')).toEqual({ value: 42 })
  })

  it('rejects a negative number', () => {
    const result = parseLimitInput('-1')
    expect(result.value).toBe(0)
    expect(result.error).toBe('Enter a whole number of 0 or greater')
  })

  it('rejects a decimal number', () => {
    const result = parseLimitInput('1.5')
    expect(result.value).toBe(0)
    expect(result.error).toBe('Enter a whole number of 0 or greater')
  })

  it('rejects exponential notation', () => {
    const result = parseLimitInput('1e5')
    expect(result.value).toBe(0)
    expect(result.error).toBe('Enter a whole number of 0 or greater')
  })

  it('rejects non-numeric text', () => {
    const result = parseLimitInput('abc')
    expect(result.value).toBe(0)
    expect(result.error).toBe('Enter a whole number of 0 or greater')
  })

  it('rejects a value above Number.MAX_SAFE_INTEGER with a distinct error', () => {
    const result = parseLimitInput('9007199254740993')
    expect(result.value).toBe(0)
    expect(result.error).toBe('Number is too large')
  })

  it('accepts Number.MAX_SAFE_INTEGER itself', () => {
    const result = parseLimitInput(String(Number.MAX_SAFE_INTEGER))
    expect(result.value).toBe(Number.MAX_SAFE_INTEGER)
    expect(result.error).toBeUndefined()
  })

  it('trims surrounding whitespace before validating', () => {
    expect(parseLimitInput('  7  ')).toEqual({ value: 7 })
  })
})
