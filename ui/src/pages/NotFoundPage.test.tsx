import { render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { describe, it, expect } from 'vitest'
import NotFoundPage from './NotFoundPage'

describe('NotFoundPage', () => {
  it('shows the requested path and a link to the dashboard', () => {
    render(
      <MemoryRouter initialEntries={['/nope/here']}>
        <NotFoundPage />
      </MemoryRouter>,
    )
    expect(screen.getByRole('heading', { name: 'Page not found' })).toBeInTheDocument()
    expect(screen.getByText('/nope/here')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Go to dashboard' })).toHaveAttribute('href', '/')
  })
})
