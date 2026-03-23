import { render, screen, waitFor, act } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { fireEvent } from '@testing-library/react'
import { describe, it, expect, vi, beforeAll, afterAll, beforeEach, afterEach } from 'vitest'
import { CopyButton } from './CopyButton'

const writeText = vi.fn().mockResolvedValue(undefined)
const originalClipboard = navigator.clipboard

beforeAll(() => {
  Object.assign(navigator, { clipboard: { writeText } })
})

afterAll(() => {
  Object.assign(navigator, { clipboard: originalClipboard })
})

describe('CopyButton', () => {
  beforeEach(() => {
    writeText.mockClear()
    writeText.mockResolvedValue(undefined)
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  describe('Rendering', () => {
    it('renders with default "Copy" label', () => {
      render(<CopyButton text="hello" />)
      expect(screen.getByRole('button', { name: /copy/i })).toBeInTheDocument()
      expect(screen.getByText('Copy')).toBeInTheDocument()
    })

    it('renders with a custom label', () => {
      render(<CopyButton text="hello" label="Copy token" />)
      expect(screen.getByText('Copy token')).toBeInTheDocument()
    })

    it('shows the copy icon by default (no check icon)', () => {
      render(<CopyButton text="hello" />)
      const button = screen.getByRole('button')
      // Copy icon contains a rect element; check icon contains a polyline
      expect(button.querySelector('rect')).toBeInTheDocument()
      expect(button.querySelector('polyline')).not.toBeInTheDocument()
    })

    it('has type="button"', () => {
      render(<CopyButton text="hello" />)
      expect(screen.getByRole('button')).toHaveAttribute('type', 'button')
    })
  })

  describe('Copy behavior', () => {
    it('calls navigator.clipboard.writeText with the text prop on click', async () => {
      render(<CopyButton text="my-secret-token" />)
      await userEvent.click(screen.getByRole('button'))
      expect(writeText).toHaveBeenCalledOnce()
      expect(writeText).toHaveBeenCalledWith('my-secret-token')
    })

    it('shows "Copied!" label after click', async () => {
      render(<CopyButton text="hello" />)
      await userEvent.click(screen.getByRole('button'))
      await waitFor(() => expect(screen.getByText('Copied!')).toBeInTheDocument())
    })

    it('shows check icon after click (not copy icon)', async () => {
      render(<CopyButton text="hello" />)
      await userEvent.click(screen.getByRole('button'))
      const button = screen.getByRole('button')
      await waitFor(() => {
        expect(button.querySelector('polyline')).toBeInTheDocument()
        expect(button.querySelector('rect')).not.toBeInTheDocument()
      })
    })

    it('reverts to original label after the timeout elapses', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true })
      render(<CopyButton text="hello" timeout={2000} />)

      // fireEvent avoids userEvent's internal setTimeout which deadlocks with fake timers
      await act(async () => {
        fireEvent.click(screen.getByRole('button'))
        // flush the clipboard promise
        await Promise.resolve()
      })

      expect(screen.getByText('Copied!')).toBeInTheDocument()

      act(() => {
        vi.advanceTimersByTime(2000)
      })

      expect(screen.getByText('Copy')).toBeInTheDocument()
    })

    it('does not revert before the timeout elapses', async () => {
      vi.useFakeTimers()
      render(<CopyButton text="hello" timeout={2000} />)

      // fireEvent (not userEvent) because userEvent uses real timers internally
      // Flush the clipboard promise so setCopied(true) fires
      await act(async () => {
        fireEvent.click(screen.getByRole('button'))
        await Promise.resolve()
        await Promise.resolve()
      })

      expect(screen.getByText('Copied!')).toBeInTheDocument()

      // Advance to just before timeout — should still show "Copied!"
      act(() => { vi.advanceTimersByTime(1999) })
      expect(screen.getByText('Copied!')).toBeInTheDocument()

      // Advance past timeout — should revert to "Copy"
      act(() => { vi.advanceTimersByTime(1) })
      expect(screen.queryByText('Copied!')).not.toBeInTheDocument()
      expect(screen.getByText('Copy')).toBeInTheDocument()

      vi.useRealTimers()
    })

    it('shows a custom copiedLabel after click', async () => {
      render(<CopyButton text="hello" copiedLabel="Done!" />)
      await userEvent.click(screen.getByRole('button'))
      await waitFor(() => expect(screen.getByText('Done!')).toBeInTheDocument())
    })

    it('reverts to original label after a custom timeout elapses', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true })
      render(<CopyButton text="hello" label="Copy key" timeout={500} />)

      await act(async () => {
        fireEvent.click(screen.getByRole('button'))
        await Promise.resolve()
      })

      expect(screen.getByText('Copied!')).toBeInTheDocument()

      act(() => {
        vi.advanceTimersByTime(500)
      })

      expect(screen.getByText('Copy key')).toBeInTheDocument()
    })
  })

  describe('Props', () => {
    it('forwards onClick handler alongside the copy action', async () => {
      const onClick = vi.fn()
      render(<CopyButton text="hello" onClick={onClick} />)
      await userEvent.click(screen.getByRole('button'))
      expect(onClick).toHaveBeenCalledOnce()
      expect(writeText).toHaveBeenCalledOnce()
    })
  })

  describe('Native attributes', () => {
    it('passes data-testid to the button element', () => {
      render(<CopyButton text="hello" data-testid="copy-btn" />)
      expect(screen.getByTestId('copy-btn')).toBeInTheDocument()
    })

    it('passes aria-label to the button element', () => {
      render(<CopyButton text="hello" aria-label="copy api key" />)
      expect(screen.getByRole('button', { name: 'copy api key' })).toBeInTheDocument()
    })
  })

  describe('className', () => {
    it('merges custom className with defaults', () => {
      render(<CopyButton text="hello" className="my-custom-class" data-testid="copy-btn" />)
      const btn = screen.getByTestId('copy-btn')
      expect(btn.className).toContain('my-custom-class')
      expect(btn.className).toContain('inline-flex')
      expect(btn.className).toContain('items-center')
      expect(btn.className).toContain('rounded-md')
    })
  })
})
