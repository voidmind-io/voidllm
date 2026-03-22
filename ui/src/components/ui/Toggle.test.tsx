import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, it, expect, vi } from 'vitest'
import { Toggle } from './Toggle'

describe('Toggle', () => {
  describe('State', () => {
    it('renders unchecked with role switch and aria-checked false', () => {
      render(<Toggle checked={false} onChange={vi.fn()} />)
      expect(screen.getByRole('switch')).toHaveAttribute('aria-checked', 'false')
    })

    it('renders checked with aria-checked true', () => {
      render(<Toggle checked={true} onChange={vi.fn()} />)
      expect(screen.getByRole('switch')).toHaveAttribute('aria-checked', 'true')
    })

    it('track has bg-accent class when checked', () => {
      render(<Toggle checked={true} onChange={vi.fn()} />)
      expect(screen.getByRole('switch').className).toContain('bg-accent')
    })

    it('track has bg-bg-tertiary class when unchecked', () => {
      render(<Toggle checked={false} onChange={vi.fn()} />)
      expect(screen.getByRole('switch').className).toContain('bg-bg-tertiary')
    })
  })

  describe('Interaction', () => {
    it('calls onChange with true when clicked while unchecked', async () => {
      const onChange = vi.fn()
      render(<Toggle checked={false} onChange={onChange} />)
      await userEvent.click(screen.getByRole('switch'))
      expect(onChange).toHaveBeenCalledOnce()
      expect(onChange).toHaveBeenCalledWith(true)
    })

    it('calls onChange with false when clicked while checked', async () => {
      const onChange = vi.fn()
      render(<Toggle checked={true} onChange={onChange} />)
      await userEvent.click(screen.getByRole('switch'))
      expect(onChange).toHaveBeenCalledOnce()
      expect(onChange).toHaveBeenCalledWith(false)
    })

    it('does not call onChange when disabled', async () => {
      const onChange = vi.fn()
      render(<Toggle checked={false} onChange={onChange} disabled />)
      await userEvent.click(screen.getByRole('switch'))
      expect(onChange).not.toHaveBeenCalled()
    })
  })

  describe('Label', () => {
    it('renders label text when provided', () => {
      render(<Toggle checked={false} onChange={vi.fn()} label="Enable feature" />)
      expect(screen.getByText('Enable feature')).toBeInTheDocument()
    })

    it('clicking label text triggers onChange', async () => {
      const onChange = vi.fn()
      render(<Toggle checked={false} onChange={onChange} label="Enable feature" />)
      await userEvent.click(screen.getByText('Enable feature'))
      expect(onChange).toHaveBeenCalledOnce()
      expect(onChange).toHaveBeenCalledWith(true)
    })

    it('does not render a label element when label prop is not provided', () => {
      render(<Toggle checked={false} onChange={vi.fn()} data-testid="toggle-no-label" />)
      const switchEl = screen.getByRole('switch')
      expect(switchEl.closest('label')).toBeNull()
    })
  })

  describe('Sizes', () => {
    it('default size md applies w-9 and h-5 track classes', () => {
      render(<Toggle checked={false} onChange={vi.fn()} />)
      const cls = screen.getByRole('switch').className
      expect(cls).toContain('w-9')
      expect(cls).toContain('h-5')
    })

    it('size sm applies w-7 and h-4 track classes', () => {
      render(<Toggle checked={false} onChange={vi.fn()} size="sm" />)
      const cls = screen.getByRole('switch').className
      expect(cls).toContain('w-7')
      expect(cls).toContain('h-4')
    })
  })

  describe('Native attributes', () => {
    it('passes data-testid to the button', () => {
      render(<Toggle checked={false} onChange={vi.fn()} data-testid="my-toggle" />)
      expect(screen.getByTestId('my-toggle')).toBeInTheDocument()
    })

    it('has type="button"', () => {
      render(<Toggle checked={false} onChange={vi.fn()} />)
      expect(screen.getByRole('switch')).toHaveAttribute('type', 'button')
    })

    it('merges custom className onto the track button', () => {
      render(<Toggle checked={false} onChange={vi.fn()} className="custom-class" />)
      expect(screen.getByRole('switch').className).toContain('custom-class')
    })
  })
})
