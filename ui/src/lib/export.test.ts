import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { exportData } from './export'

let captured = ''

beforeEach(() => {
  captured = ''
  vi.stubGlobal(
    'Blob',
    class {
      constructor(parts: string[]) {
        captured = parts.join('')
      }
    },
  )
  URL.createObjectURL = vi.fn(() => 'blob:x')
  URL.revokeObjectURL = vi.fn()
  vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
})

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

function csvLines(rows: Record<string, unknown>[]): string[] {
  exportData(rows, [{ key: 'v', label: 'V' }], 'x', 'csv')
  return captured.split('\n')
}

describe('exportData CSV sanitization', () => {
  it('does not prefix negative numbers', () => {
    expect(csvLines([{ v: -0.12 }])).toEqual(['V', '-0.12'])
  })
  it('prefixes strings starting with formula characters', () => {
    expect(csvLines([{ v: '-1' }, { v: '=SUM(A1)' }, { v: '+1' }, { v: '@x' }])).toEqual([
      'V',
      "'-1",
      "'=SUM(A1)",
      "'+1",
      "'@x",
    ])
  })
  it('leaves normal strings and numbers alone', () => {
    expect(csvLines([{ v: 'gpt-4o' }, { v: 5 }])).toEqual(['V', 'gpt-4o', '5'])
  })
})
