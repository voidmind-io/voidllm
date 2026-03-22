import { render, screen } from '@testing-library/react'
import { describe, it, expect } from 'vitest'
import { KeyHint } from './KeyHint'

describe('KeyHint', () => {
  describe('Rendering', () => {
    it('renders the full hint text', () => {
      render(<KeyHint hint="vl_uk_...2ad6" />)
      expect(screen.getByText('2ad6')).toBeInTheDocument()
    })

    it('renders prefix in tertiary color', () => {
      render(<KeyHint hint="vl_uk_...2ad6" />)
      const prefix = screen.getByText('vl_uk_...')
      expect(prefix.className).toContain('text-text-tertiary')
    })

    it('splits on ellipsis correctly for user key', () => {
      render(<KeyHint hint="vl_uk_...e8b1" />)
      expect(screen.getByText('vl_uk_...')).toBeInTheDocument()
      expect(screen.getByText('e8b1')).toBeInTheDocument()
    })

    it('splits on ellipsis correctly for team key', () => {
      render(<KeyHint hint="vl_tk_...a2c3" />)
      expect(screen.getByText('vl_tk_...')).toBeInTheDocument()
      expect(screen.getByText('a2c3')).toBeInTheDocument()
    })

    it('splits on ellipsis correctly for sa key', () => {
      render(<KeyHint hint="vl_sa_...f4d5" />)
      expect(screen.getByText('vl_sa_...')).toBeInTheDocument()
      expect(screen.getByText('f4d5')).toBeInTheDocument()
    })

    it('splits on ellipsis correctly for session key', () => {
      render(<KeyHint hint="vl_sk_...0684" />)
      expect(screen.getByText('vl_sk_...')).toBeInTheDocument()
      expect(screen.getByText('0684')).toBeInTheDocument()
    })

    it('renders hint without ellipsis as plain text', () => {
      render(<KeyHint hint="somekey" />)
      expect(screen.getByText('somekey')).toBeInTheDocument()
    })
  })

  describe('Styling', () => {
    it('outer span has font-mono class', () => {
      render(<KeyHint hint="vl_uk_...2ad6" data-testid="kh" />)
      expect(screen.getByTestId('kh').className).toContain('font-mono')
    })

    it('outer span has text-xs class', () => {
      render(<KeyHint hint="vl_uk_...2ad6" data-testid="kh" />)
      expect(screen.getByTestId('kh').className).toContain('text-xs')
    })
  })

  describe('Native attributes', () => {
    it('passes data-testid', () => {
      render(<KeyHint hint="vl_uk_...2ad6" data-testid="my-hint" />)
      expect(screen.getByTestId('my-hint')).toBeInTheDocument()
    })

    it('merges custom className', () => {
      render(<KeyHint hint="vl_uk_...2ad6" className="extra" data-testid="kh" />)
      expect(screen.getByTestId('kh').className).toContain('extra')
      expect(screen.getByTestId('kh').className).toContain('font-mono')
    })
  })
})
