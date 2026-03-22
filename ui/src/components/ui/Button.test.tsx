import { createRef } from 'react'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, it, expect, vi } from 'vitest'
import { Button } from './Button'

describe('Button', () => {
  describe('Rendering', () => {
    it('renders with label text', () => {
      render(<Button>Click me</Button>)
      expect(screen.getByRole('button', { name: 'Click me' })).toBeInTheDocument()
    })

    it('type defaults to "button"', () => {
      render(<Button>Submit</Button>)
      expect(screen.getByRole('button')).toHaveAttribute('type', 'button')
    })

    it('type="submit" is applied when passed', () => {
      render(<Button type="submit">Submit</Button>)
      expect(screen.getByRole('button')).toHaveAttribute('type', 'submit')
    })
  })

  describe('Variants', () => {
    it('default variant is primary — has accent bg class', () => {
      render(<Button>Primary</Button>)
      expect(screen.getByRole('button').className).toContain('bg-accent')
    })

    it('secondary variant has border class', () => {
      render(<Button variant="secondary">Secondary</Button>)
      expect(screen.getByRole('button').className).toContain('border')
    })

    it('destructive variant has error bg class', () => {
      render(<Button variant="destructive">Delete</Button>)
      expect(screen.getByRole('button').className).toContain('bg-error')
    })

    it('ghost variant has no background and no border', () => {
      render(<Button variant="ghost">Ghost</Button>)
      const cls = screen.getByRole('button').className
      expect(cls).toContain('bg-transparent')
      expect(cls).not.toContain('border')
    })

    it('ghost and secondary have distinct hover classes', () => {
      const { rerender } = render(<Button variant="ghost">Ghost</Button>)
      const ghostCls = screen.getByRole('button').className

      rerender(<Button variant="secondary">Secondary</Button>)
      const secondaryCls = screen.getByRole('button').className

      expect(ghostCls).not.toBe(secondaryCls)
    })
  })

  describe('Sizes', () => {
    it('sm size applies smaller padding class', () => {
      render(<Button size="sm">Small</Button>)
      expect(screen.getByRole('button').className).toContain('px-3')
    })

    it('md is the default size', () => {
      render(<Button>Medium</Button>)
      expect(screen.getByRole('button').className).toContain('px-4')
    })

    it('lg applies larger padding class', () => {
      render(<Button size="lg">Large</Button>)
      expect(screen.getByRole('button').className).toContain('px-6')
    })
  })

  describe('Interactions', () => {
    it('onClick fires when clicked', async () => {
      const onClick = vi.fn()
      render(<Button onClick={onClick}>Click</Button>)
      await userEvent.click(screen.getByRole('button'))
      expect(onClick).toHaveBeenCalledOnce()
    })

    it('onClick does NOT fire when disabled', async () => {
      const onClick = vi.fn()
      render(<Button disabled onClick={onClick}>Click</Button>)
      await userEvent.click(screen.getByRole('button'))
      expect(onClick).not.toHaveBeenCalled()
    })

    it('onClick does NOT fire when loading', async () => {
      const onClick = vi.fn()
      render(<Button loading onClick={onClick}>Click</Button>)
      await userEvent.click(screen.getByRole('button'))
      expect(onClick).not.toHaveBeenCalled()
    })
  })

  describe('Loading state', () => {
    it('shows spinner when loading=true', () => {
      render(<Button loading>Save</Button>)
      expect(screen.getByRole('status', { name: 'Loading' })).toBeInTheDocument()
    })

    it('button has disabled attribute when loading', () => {
      render(<Button loading>Save</Button>)
      expect(screen.getByRole('button')).toBeDisabled()
    })

    it('children are still visible when loading', () => {
      render(<Button loading>Save</Button>)
      expect(screen.getByRole('button', { name: /save/i })).toBeInTheDocument()
    })

    it('icon is hidden when loading', () => {
      const icon = <span data-testid="btn-icon">X</span>
      render(<Button loading icon={icon}>Action</Button>)
      expect(screen.queryByTestId('btn-icon')).not.toBeInTheDocument()
    })

    it('aria-busy is true when loading', () => {
      render(<Button loading>Save</Button>)
      expect(screen.getByRole('button')).toHaveAttribute('aria-busy', 'true')
    })

    it('aria-busy is absent when not loading', () => {
      render(<Button>Save</Button>)
      expect(screen.getByRole('button')).not.toHaveAttribute('aria-busy')
    })
  })

  describe('Disabled state', () => {
    it('button has disabled attribute', () => {
      render(<Button disabled>Action</Button>)
      expect(screen.getByRole('button')).toBeDisabled()
    })

    it('has opacity class when disabled', () => {
      render(<Button disabled>Action</Button>)
      expect(screen.getByRole('button').className).toContain('opacity-50')
    })
  })

  describe('fullWidth', () => {
    it('w-full class is present when fullWidth=true', () => {
      render(<Button fullWidth>Action</Button>)
      expect(screen.getByRole('button').className).toContain('w-full')
    })

    it('w-full class is not present when fullWidth is omitted', () => {
      render(<Button>Action</Button>)
      expect(screen.getByRole('button').className).not.toContain('w-full')
    })
  })

  describe('Icon', () => {
    it('icon renders when provided', () => {
      const icon = <span data-testid="btn-icon">icon</span>
      render(<Button icon={icon}>Action</Button>)
      expect(screen.getByTestId('btn-icon')).toBeInTheDocument()
    })

    it('icon is wrapped in a shrink-0 span', () => {
      const icon = <span data-testid="btn-icon">icon</span>
      render(<Button icon={icon}>Action</Button>)
      const wrapper = screen.getByTestId('btn-icon').parentElement
      expect(wrapper?.className).toContain('shrink-0')
    })

    it('icon is NOT rendered when loading', () => {
      const icon = <span data-testid="btn-icon">icon</span>
      render(<Button loading icon={icon}>Action</Button>)
      expect(screen.queryByTestId('btn-icon')).not.toBeInTheDocument()
    })

    it('renders no icon wrapper when icon is omitted', () => {
      render(<Button>Action</Button>)
      const btn = screen.getByRole('button')
      expect(btn.querySelector('.shrink-0')).not.toBeInTheDocument()
    })
  })

  describe('className', () => {
    it('additional className is merged onto the button', () => {
      render(<Button className="custom-class">Action</Button>)
      expect(screen.getByRole('button').className).toContain('custom-class')
    })
  })

  describe('Ref forwarding', () => {
    it('forwards ref to the underlying button element', () => {
      const ref = createRef<HTMLButtonElement>()
      render(<Button ref={ref}>Ref Test</Button>)
      expect(ref.current).not.toBeNull()
      expect(ref.current?.tagName).toBe('BUTTON')
    })
  })

  describe('Native attribute passthrough', () => {
    it('passes data attributes through to the button', () => {
      render(<Button data-testid="native-btn">Action</Button>)
      expect(screen.getByTestId('native-btn')).toBeInTheDocument()
    })

    it('passes aria-label through to the button', () => {
      render(<Button aria-label="custom label">Action</Button>)
      expect(screen.getByRole('button', { name: 'custom label' })).toBeInTheDocument()
    })
  })
})
