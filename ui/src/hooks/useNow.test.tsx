import { act, render, screen } from '@testing-library/react'
import { describe, it, expect, vi, afterEach } from 'vitest'
import { useNow } from './useNow'

function Probe({ id }: { id: string }) {
  const now = useNow()
  return <span data-testid={id}>{now}</span>
}

afterEach(() => {
  vi.useRealTimers()
})

describe('useNow', () => {
  it('ticks every minute while subscribed and stops when unmounted', () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2030-01-01T00:00:00Z'))
    const a = render(<Probe id="a" />)
    const b = render(<Probe id="b" />)
    expect(vi.getTimerCount()).toBe(1)
    const first = screen.getByTestId('a').textContent
    act(() => {
      vi.advanceTimersByTime(60_000)
    })
    expect(screen.getByTestId('a').textContent).not.toBe(first)
    expect(screen.getByTestId('a').textContent).toBe(screen.getByTestId('b').textContent)
    a.unmount()
    expect(vi.getTimerCount()).toBe(1)
    b.unmount()
    expect(vi.getTimerCount()).toBe(0)
  })
})
