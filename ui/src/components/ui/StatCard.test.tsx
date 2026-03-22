import { render, screen } from '@testing-library/react'
import { describe, it, expect } from 'vitest'
import { StatCard } from './StatCard'

describe('StatCard', () => {
  describe('Rendering', () => {
    it('renders label and value', () => {
      render(<StatCard label="Total Requests" value="12,345" />)
      expect(screen.getByText('Total Requests')).toBeInTheDocument()
      expect(screen.getByText('12,345')).toBeInTheDocument()
    })

    it('renders string value', () => {
      render(<StatCard label="Status" value="Healthy" />)
      expect(screen.getByText('Healthy')).toBeInTheDocument()
    })

    it('renders zero as value without collapsing', () => {
      render(<StatCard label="Errors" value={0} />)
      expect(screen.getByText('0')).toBeInTheDocument()
    })

    it('renders number value', () => {
      render(<StatCard label="Users" value={42} />)
      expect(screen.getByText('42')).toBeInTheDocument()
    })
  })

  describe('Icon', () => {
    it('renders icon when provided', () => {
      const icon = <span data-testid="stat-icon">★</span>
      render(<StatCard label="Tokens" value="1,000" icon={icon} />)
      expect(screen.getByTestId('stat-icon')).toBeInTheDocument()
    })

    it('does not render icon container when not provided', () => {
      render(<StatCard label="Tokens" value="1,000" />)
      // The icon wrapper is a <span> with class shrink-0; it must not appear
      const card = screen.getByText('Tokens').closest('div')
      expect(card?.querySelector('.shrink-0')).not.toBeInTheDocument()
    })
  })

  describe('Trend', () => {
    it('positive trend shows ▲ and success color class', () => {
      render(<StatCard label="Revenue" value="$500" trend={{ value: 12 }} />)
      const trendEl = screen.getByText(/▲/)
      expect(trendEl).toBeInTheDocument()
      expect(trendEl.className).toContain('text-success')
    })

    it('negative trend shows ▼ with absolute value and error color class', () => {
      render(<StatCard label="Revenue" value="$500" trend={{ value: -5 }} />)
      const trendEl = screen.getByText('▼ 5')
      expect(trendEl).toBeInTheDocument()
      expect(trendEl.className).toContain('text-error')
    })

    it('zero trend shows — and tertiary color class', () => {
      render(<StatCard label="Revenue" value="$500" trend={{ value: 0 }} />)
      const trendEl = screen.getByText(/—/)
      expect(trendEl).toBeInTheDocument()
      expect(trendEl.className).toContain('text-text-tertiary')
    })

    it('renders trend label when provided', () => {
      render(<StatCard label="Revenue" value="$500" trend={{ value: 8, label: 'vs last month' }} />)
      expect(screen.getByText('▲ vs last month')).toBeInTheDocument()
    })

    it('shows numeric value when no label provided', () => {
      render(<StatCard label="Revenue" value="$500" trend={{ value: 8 }} />)
      expect(screen.getByText('▲ 8')).toBeInTheDocument()
    })
  })

  describe('Native attributes', () => {
    it('passes data-testid', () => {
      render(<StatCard label="Requests" value="99" data-testid="stat-card" />)
      expect(screen.getByTestId('stat-card')).toBeInTheDocument()
    })

    it('passes aria-label', () => {
      render(<StatCard label="Requests" value="99" aria-label="requests stat" />)
      expect(screen.getByLabelText('requests stat')).toBeInTheDocument()
    })

    it('passes title', () => {
      render(<StatCard label="Requests" value="99" title="stat tooltip" />)
      const card = screen.getByTitle('stat tooltip')
      expect(card).toBeInTheDocument()
    })
  })

  describe('className', () => {
    it('merges custom className with defaults', () => {
      render(<StatCard label="Requests" value="99" className="custom-class" data-testid="sc-merge" />)
      expect(screen.getByTestId('sc-merge').className).toContain('custom-class')
    })

    it('default classes include bg-bg-secondary and border', () => {
      render(<StatCard label="Requests" value="99" data-testid="sc-defaults" />)
      const cls = screen.getByTestId('sc-defaults').className
      expect(cls).toContain('bg-bg-secondary')
      expect(cls).toContain('border')
    })
  })
})
