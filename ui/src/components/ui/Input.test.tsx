import { createRef } from 'react'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, it, expect, vi } from 'vitest'
import { Input } from './Input'

describe('Input', () => {
  describe('Rendering', () => {
    it('renders an input element', () => {
      render(<Input />)
      expect(screen.getByRole('textbox')).toBeInTheDocument()
    })

    it('label renders when provided and links to input via htmlFor', () => {
      render(<Input label="Email address" />)
      const label = screen.getByText('Email address')
      expect(label).toBeInTheDocument()
      const input = screen.getByLabelText('Email address')
      expect(input).toBeInTheDocument()
    })

    it('label not rendered when omitted', () => {
      render(<Input />)
      expect(screen.queryByRole('generic', { name: /label/i })).not.toBeInTheDocument()
      // No label element at all in the DOM
      const { container } = render(<Input />)
      expect(container.querySelector('label')).toBeNull()
    })

    it('custom id overrides auto-generated id', () => {
      render(<Input id="my-input" label="Name" />)
      const input = screen.getByRole('textbox')
      expect(input).toHaveAttribute('id', 'my-input')
      const label = screen.getByText('Name')
      expect(label).toHaveAttribute('for', 'my-input')
    })

    it('placeholder passes through', () => {
      render(<Input placeholder="Enter value" />)
      expect(screen.getByPlaceholderText('Enter value')).toBeInTheDocument()
    })
  })

  describe('Types (native passthrough)', () => {
    it('type="email" applied', () => {
      const { container } = render(<Input type="email" />)
      expect(container.querySelector('input')).toHaveAttribute('type', 'email')
    })

    it('type="password" applied', () => {
      const { container } = render(<Input type="password" />)
      expect(container.querySelector('input')).toHaveAttribute('type', 'password')
    })

    it('type="number" applied', () => {
      render(<Input type="number" />)
      expect(screen.getByRole('spinbutton')).toHaveAttribute('type', 'number')
    })
  })

  describe('Error state', () => {
    it('error message renders with role="alert"', () => {
      render(<Input error="This field is required" />)
      expect(screen.getByRole('alert')).toBeInTheDocument()
    })

    it('input has aria-invalid="true" when error is set', () => {
      render(<Input error="Bad value" />)
      expect(screen.getByRole('textbox')).toHaveAttribute('aria-invalid', 'true')
    })

    it('error message text matches error prop', () => {
      render(<Input error="Must be a valid email" />)
      expect(screen.getByRole('alert')).toHaveTextContent('Must be a valid email')
    })

    it('input has border-error class when error is set', () => {
      render(<Input error="Oops" />)
      expect(screen.getByRole('textbox').className).toContain('border-error')
    })

    it('aria-invalid is absent when no error', () => {
      render(<Input />)
      expect(screen.getByRole('textbox')).not.toHaveAttribute('aria-invalid')
    })
  })

  describe('Description', () => {
    it('description renders when provided', () => {
      render(<Input description="We will never share your email." />)
      expect(screen.getByText('We will never share your email.')).toBeInTheDocument()
    })

    it('description is hidden when error is present', () => {
      render(<Input description="Helpful hint" error="Something went wrong" />)
      expect(screen.queryByText('Helpful hint')).not.toBeInTheDocument()
    })

    it('description has correct id for aria-describedby', () => {
      render(<Input id="test-input" description="Some help text" />)
      const desc = screen.getByText('Some help text')
      expect(desc).toHaveAttribute('id', 'test-input-desc')
    })
  })

  describe('Disabled state', () => {
    it('input has disabled attribute', () => {
      render(<Input disabled />)
      expect(screen.getByRole('textbox')).toBeDisabled()
    })

    it('has opacity class when disabled', () => {
      render(<Input disabled />)
      expect(screen.getByRole('textbox').className).toContain('opacity-50')
    })
  })

  describe('Interactions', () => {
    it('onChange fires when typing', async () => {
      const onChange = vi.fn()
      render(<Input onChange={onChange} />)
      await userEvent.type(screen.getByRole('textbox'), 'hello')
      expect(onChange).toHaveBeenCalled()
    })
  })

  describe('Accessibility', () => {
    it('label associated via htmlFor — clicking label focuses input', async () => {
      render(<Input label="Username" />)
      await userEvent.click(screen.getByText('Username'))
      expect(screen.getByRole('textbox')).toHaveFocus()
    })

    it('aria-describedby links to error element when error is present', () => {
      render(<Input id="acc-input" error="Invalid" />)
      const input = screen.getByRole('textbox')
      const errorId = 'acc-input-error'
      expect(input).toHaveAttribute('aria-describedby', errorId)
      expect(document.getElementById(errorId)).toBeInTheDocument()
    })

    it('aria-describedby links to description element when no error', () => {
      render(<Input id="acc-input" description="Some hint" />)
      const input = screen.getByRole('textbox')
      const descId = 'acc-input-desc'
      expect(input).toHaveAttribute('aria-describedby', descId)
      expect(document.getElementById(descId)).toBeInTheDocument()
    })

    it('aria-describedby is absent when neither error nor description', () => {
      render(<Input />)
      expect(screen.getByRole('textbox')).not.toHaveAttribute('aria-describedby')
    })

    it('ref forwarding works', () => {
      const ref = createRef<HTMLInputElement>()
      render(<Input ref={ref} />)
      expect(ref.current).not.toBeNull()
      expect(ref.current?.tagName).toBe('INPUT')
    })

    it('passes data-testid through to input', () => {
      render(<Input data-testid="my-input" />)
      expect(screen.getByTestId('my-input')).toBeInTheDocument()
    })

    it('passes aria-label through to input', () => {
      render(<Input aria-label="Search" />)
      expect(screen.getByRole('textbox', { name: 'Search' })).toBeInTheDocument()
    })
  })

  describe('fullWidth', () => {
    it('w-full class on wrapper when fullWidth=true (default)', () => {
      const { container } = render(<Input />)
      expect(container.firstElementChild?.className).toContain('w-full')
    })

    it('w-full class on wrapper when fullWidth=true explicitly', () => {
      const { container } = render(<Input fullWidth={true} />)
      expect(container.firstElementChild?.className).toContain('w-full')
    })

    it('w-full class absent on wrapper when fullWidth=false', () => {
      const { container } = render(<Input fullWidth={false} />)
      expect(container.firstElementChild?.className).not.toContain('w-full')
    })
  })

  describe('className', () => {
    it('additional className is merged onto the input element', () => {
      render(<Input className="custom-input-class" />)
      expect(screen.getByRole('textbox').className).toContain('custom-input-class')
    })
  })
})
