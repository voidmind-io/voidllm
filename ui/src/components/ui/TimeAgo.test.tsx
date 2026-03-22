import { render, screen } from '@testing-library/react'
import { describe, it, expect, beforeAll, afterAll } from 'vitest'
import { TimeAgo, relativeTime } from './TimeAgo'

// Pin "now" to a fixed instant so all relative-time calculations are deterministic.
const NOW = new Date('2026-03-18T12:00:00Z')

beforeAll(() => {
  vi.useFakeTimers({ shouldAdvanceTime: false })
  vi.setSystemTime(NOW)
})

afterAll(() => {
  vi.useRealTimers()
})

// Helper: subtract seconds from the fixed now and return an ISO string.
function secsAgo(seconds: number): string {
  return new Date(NOW.getTime() - seconds * 1000).toISOString()
}

// ---------------------------------------------------------------------------
// relativeTime — pure function unit tests
// ---------------------------------------------------------------------------

describe('relativeTime', () => {
  it('returns "just now" for 30 seconds ago', () => {
    expect(relativeTime(secsAgo(30))).toBe('just now')
  })

  it('returns "5m ago" for 5 minutes ago', () => {
    expect(relativeTime(secsAgo(5 * 60))).toBe('5m ago')
  })

  it('returns "3h ago" for 3 hours ago', () => {
    expect(relativeTime(secsAgo(3 * 60 * 60))).toBe('3h ago')
  })

  it('returns "2d ago" for 2 days ago', () => {
    expect(relativeTime(secsAgo(2 * 24 * 60 * 60))).toBe('2d ago')
  })

  it('returns "1w ago" for 10 days ago', () => {
    expect(relativeTime(secsAgo(10 * 24 * 60 * 60))).toBe('1w ago')
  })

  it('returns formatted absolute date for 60 days ago', () => {
    const iso = secsAgo(60 * 24 * 60 * 60)
    // formatDate delegates to new Date(iso).toLocaleString() — match exactly.
    expect(relativeTime(iso)).toBe(new Date(iso).toLocaleString())
  })

  it('returns empty string for an invalid date string', () => {
    expect(relativeTime('not-a-date')).toBe('')
  })

  // Future timestamps (for expiry dates)
  it('returns "in 5m" for 5 minutes in the future', () => {
    expect(relativeTime(secsAgo(-5 * 60))).toBe('in 5m')
  })

  it('returns "in 3h" for 3 hours in the future', () => {
    expect(relativeTime(secsAgo(-3 * 60 * 60))).toBe('in 3h')
  })

  it('returns "in 2d" for 2 days in the future', () => {
    expect(relativeTime(secsAgo(-2 * 24 * 60 * 60))).toBe('in 2d')
  })

  it('returns "in <1m" for a few seconds in the future', () => {
    expect(relativeTime(secsAgo(-10))).toBe('in <1m')
  })
})

// ---------------------------------------------------------------------------
// TimeAgo component
// ---------------------------------------------------------------------------

describe('TimeAgo', () => {
  describe('Rendering', () => {
    it('renders relative time text for a valid date', () => {
      render(<TimeAgo date={secsAgo(5 * 60)} />)
      expect(screen.getByText('5m ago')).toBeInTheDocument()
    })

    it('renders fallback "never" when date is an empty string', () => {
      render(<TimeAgo date="" />)
      expect(screen.getByText('never')).toBeInTheDocument()
    })

    it('renders custom fallback when date is empty', () => {
      render(<TimeAgo date="" fallback="unknown" />)
      expect(screen.getByText('unknown')).toBeInTheDocument()
    })

    it('renders custom fallback when date is invalid', () => {
      render(<TimeAgo date="bad-date" fallback="n/a" />)
      expect(screen.getByText('n/a')).toBeInTheDocument()
    })
  })

  describe('time element attributes', () => {
    it('has dateTime attribute set to the ISO string', () => {
      const iso = secsAgo(5 * 60)
      render(<TimeAgo date={iso} data-testid="ta" />)
      expect(screen.getByTestId('ta')).toHaveAttribute('dateTime', iso)
    })

    it('has title attribute with locale string for hover tooltip', () => {
      const iso = secsAgo(5 * 60)
      render(<TimeAgo date={iso} data-testid="ta" />)
      expect(screen.getByTestId('ta')).toHaveAttribute(
        'title',
        new Date(iso).toLocaleString(),
      )
    })

    it('does not have dateTime attribute when date is empty', () => {
      render(<TimeAgo date="" data-testid="ta" />)
      expect(screen.getByTestId('ta')).not.toHaveAttribute('dateTime')
    })

    it('does not have dateTime attribute when date is invalid', () => {
      render(<TimeAgo date="not-a-date" data-testid="ta" />)
      expect(screen.getByTestId('ta')).not.toHaveAttribute('dateTime')
    })
  })

  describe('Native attributes', () => {
    it('passes data-testid', () => {
      render(<TimeAgo date={secsAgo(60)} data-testid="timeago-el" />)
      expect(screen.getByTestId('timeago-el')).toBeInTheDocument()
    })

    it('merges custom className with defaults', () => {
      render(<TimeAgo date={secsAgo(60)} className="my-custom" data-testid="ta" />)
      expect(screen.getByTestId('ta').className).toContain('my-custom')
    })

    it('default classes include text-text-tertiary', () => {
      render(<TimeAgo date={secsAgo(60)} data-testid="ta" />)
      expect(screen.getByTestId('ta').className).toContain('text-text-tertiary')
    })
  })
})
