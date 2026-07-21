import { render, screen } from '@testing-library/react'
import { MemoryRouter, Routes, Route } from 'react-router-dom'
import { describe, it, expect, vi, beforeEach } from 'vitest'
import AcceptInvitePage from './AcceptInvitePage'

// ---------------------------------------------------------------------------
// Render helpers
// ---------------------------------------------------------------------------

function renderAcceptInvitePage(initialPath: string) {
  return render(
    <MemoryRouter initialEntries={[initialPath]}>
      <Routes>
        <Route path="/invite/:token" element={<AcceptInvitePage />} />
        <Route path="/invite" element={<AcceptInvitePage />} />
        <Route path="/login" element={<div>Login Page</div>} />
      </Routes>
    </MemoryRouter>,
  )
}

function mockFetchOnce(response: { ok: boolean; status: number; body?: unknown }) {
  return vi.fn(async () => ({
    ok: response.ok,
    status: response.status,
    json: () => Promise.resolve(response.body ?? {}),
  }))
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

describe('AcceptInvitePage', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  it('renders the invalid state and makes no network request when there is no token in the route', () => {
    const fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)

    renderAcceptInvitePage('/invite')

    expect(
      screen.getByText(/this invite link is invalid or has already been used/i),
    ).toBeInTheDocument()
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('renders the form once the peek request resolves for a valid token', async () => {
    vi.stubGlobal(
      'fetch',
      mockFetchOnce({
        ok: true,
        status: 200,
        body: {
          email: 'jane@example.com',
          org_name: 'Acme Corp',
          role: 'member',
          expires_at: '2026-08-01T00:00:00Z',
        },
      }),
    )

    renderAcceptInvitePage('/invite/good-token')

    expect(await screen.findByText('Acme Corp')).toBeInTheDocument()
    expect(screen.getByLabelText(/display name/i)).toBeInTheDocument()
    expect(screen.getByText('jane@example.com')).toBeInTheDocument()
  })

  it('renders the expired state for a 410 response', async () => {
    vi.stubGlobal('fetch', mockFetchOnce({ ok: false, status: 410 }))

    renderAcceptInvitePage('/invite/expired-token')

    expect(await screen.findByText(/this invite link has expired/i)).toBeInTheDocument()
  })

  it('renders the invalid state for a non-410 error response', async () => {
    vi.stubGlobal('fetch', mockFetchOnce({ ok: false, status: 404 }))

    renderAcceptInvitePage('/invite/bad-token')

    expect(
      await screen.findByText(/this invite link is invalid or has already been used/i),
    ).toBeInTheDocument()
  })
})
