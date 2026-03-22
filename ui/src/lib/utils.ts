import { twMerge } from 'tailwind-merge'

/** Merge class names using tailwind-merge to resolve conflicting Tailwind classes. */
export function cn(...classes: (string | false | null | undefined)[]): string {
  return twMerge(classes.filter(Boolean).join(' '))
}

/** Format a number with locale-aware separators. */
export function formatNumber(n: number): string {
  return new Intl.NumberFormat().format(n)
}

/** Format a token count with locale-aware separators. */
export function formatTokens(n: number): string {
  return new Intl.NumberFormat().format(n)
}

/** Format a number as USD currency. */
export function formatCost(n: number): string {
  // Show more decimals for small amounts (LLM costs are often fractions of a cent)
  const decimals = Math.abs(n) < 0.01 ? 6 : Math.abs(n) < 1 ? 4 : 2
  return new Intl.NumberFormat('en-US', {
    style: 'currency',
    currency: 'USD',
    minimumFractionDigits: 2,
    maximumFractionDigits: decimals,
  }).format(n)
}

/** Format an ISO UTC timestamp in the user's local timezone. */
export function formatDate(iso: string): string {
  return new Date(iso).toLocaleString()
}
