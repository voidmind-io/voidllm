/** Fraction of prompt tokens served from cache (0..1), or null when there are no prompt tokens. */
export function cacheHitRate(read: number, prompt: number): number | null {
  if (prompt <= 0) return null
  return Math.min(Math.max(read, 0) / prompt, 1)
}

/** Format a hit rate as "xx.x%" or "-" when unknown. */
export function formatHitRate(r: number | null): string {
  if (r === null) return '-'
  return `${(r * 100).toFixed(1)}%`
}

/** Sum the non-null cache savings of the rows, or null when no row carries a value. */
export function sumSavings(rows: { cache_savings?: number | null }[]): number | null {
  let total = 0
  let found = false
  for (const r of rows) {
    if (r.cache_savings !== null && r.cache_savings !== undefined) {
      total += r.cache_savings
      found = true
    }
  }
  return found ? total : null
}
