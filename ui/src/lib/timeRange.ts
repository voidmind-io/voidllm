/**
 * Range descriptors for usage queries. Hooks store the descriptor in their
 * query key and resolve it to concrete from/to timestamps inside the queryFn,
 * so every refetch uses a window that ends "now".
 *
 * The "-cal" variants use calendar arithmetic (setHours/setDate) instead of
 * fixed millisecond offsets, matching the Dashboard behaviour across DST.
 */
export type UsageRange = '24h' | '7d' | '30d' | '90d' | '24h-cal' | '7d-cal' | '30d-cal'

const RANGE_HOURS: Record<'24h' | '7d' | '30d' | '90d', number> = {
  '24h': 24,
  '7d': 168,
  '30d': 720,
  '90d': 2160,
}

/** Resolves a range descriptor to an ISO from/to window ending at the current time. */
export function resolveRange(range: UsageRange): { from: string; to: string } {
  switch (range) {
    case '24h-cal': {
      const to = new Date()
      const from = new Date()
      from.setHours(from.getHours() - 24)
      return { from: from.toISOString(), to: to.toISOString() }
    }
    case '7d-cal': {
      const to = new Date()
      const from = new Date()
      from.setDate(from.getDate() - 7)
      return { from: from.toISOString(), to: to.toISOString() }
    }
    case '30d-cal': {
      const to = new Date()
      const from = new Date()
      from.setDate(from.getDate() - 30)
      return { from: from.toISOString(), to: to.toISOString() }
    }
    default: {
      const now = new Date()
      const from = new Date(now.getTime() - RANGE_HOURS[range] * 3_600_000)
      return { from: from.toISOString(), to: now.toISOString() }
    }
  }
}
