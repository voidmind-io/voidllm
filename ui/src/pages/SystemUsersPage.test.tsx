import React from 'react'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { ToastProvider } from '../hooks/useToast'
import SystemUsersPage from './SystemUsersPage'

// ---------------------------------------------------------------------------
// Types used in mocks (mirror the production types)
// ---------------------------------------------------------------------------

interface MockOrg {
  id: string
  name: string
  slug: string
  timezone: string | null
  daily_token_limit: number
  monthly_token_limit: number
  requests_per_minute: number
  requests_per_day: number
  member_count: number
  team_count: number
  created_at: string
  updated_at: string
}

interface MockUser {
  id: string
  email: string
  display_name: string
  auth_provider: string
  is_system_admin: boolean
  created_at: string
  updated_at: string
}

// ---------------------------------------------------------------------------
// Test fixtures
// ---------------------------------------------------------------------------

function makeOrg(overrides: Partial<MockOrg> = {}): MockOrg {
  return {
    id: 'org-1',
    name: 'Org Alpha',
    slug: 'org-alpha',
    timezone: null,
    daily_token_limit: 0,
    monthly_token_limit: 0,
    requests_per_minute: 0,
    requests_per_day: 0,
    member_count: 1,
    team_count: 0,
    created_at: '2024-01-01T00:00:00Z',
    updated_at: '2024-01-01T00:00:00Z',
    ...overrides,
  }
}

const ORG_ALPHA = makeOrg({ id: 'org-1', name: 'Org Alpha', slug: 'org-alpha' })
const ORG_BETA = makeOrg({ id: 'org-2', name: 'Org Beta', slug: 'org-beta' })

const MOCK_ME = {
  id: 'user-admin',
  email: 'admin@example.com',
  display_name: 'Admin',
  role: 'system_admin',
  is_system_admin: true,
}

// ---------------------------------------------------------------------------
// Render helpers
// ---------------------------------------------------------------------------

function makeWrapper() {
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  })
  function Wrapper({ children }: { children: React.ReactNode }) {
    return (
      <QueryClientProvider client={queryClient}>
        <ToastProvider>
          <MemoryRouter>{children}</MemoryRouter>
        </ToastProvider>
      </QueryClientProvider>
    )
  }
  return { queryClient, Wrapper }
}

function renderSystemUsersPage() {
  const { queryClient, Wrapper } = makeWrapper()
  const utils = render(<SystemUsersPage />, { wrapper: Wrapper })
  return { queryClient, ...utils }
}

// ---------------------------------------------------------------------------
// Fetch mock helpers
// ---------------------------------------------------------------------------

type FetchMockEntry = {
  matcher: (url: string) => boolean
  response: unknown
  method?: string
}

function setupFetchMock(entries: FetchMockEntry[], capturedBodies?: Map<string, string>) {
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : input.toString()
    const method = (init?.method ?? 'GET').toUpperCase()

    const entry = entries.find(
      (e) => e.matcher(url) && (!e.method || e.method.toUpperCase() === method),
    )

    if (entry) {
      if (capturedBodies && init?.body) {
        capturedBodies.set(`${method}:${url}`, init.body as string)
      }
      return {
        ok: true,
        status: 200,
        // Cloning via JSON round-trip guarantees a fresh reference on every
        // call, the same way a real HTTP response would - this matters for
        // regression-testing effects/derived-state that key off array
        // identity rather than content.
        json: () => Promise.resolve(JSON.parse(JSON.stringify(entry.response))),
      }
    }

    return {
      ok: true,
      status: 200,
      json: () => Promise.resolve({}),
    }
  }))
}

function defaultEntries(orgs: MockOrg[], users: MockUser[] = []): FetchMockEntry[] {
  return [
    {
      matcher: (u) => u.includes('/api/v1/me') && !u.includes('/api/v1/models'),
      method: 'GET',
      response: MOCK_ME,
    },
    {
      matcher: (u) => u.includes('/api/v1/orgs'),
      method: 'GET',
      response: { data: orgs, has_more: false },
    },
    {
      matcher: (u) => u.includes('/api/v1/users'),
      method: 'GET',
      response: { data: users, has_more: false },
    },
  ]
}

// ---------------------------------------------------------------------------
// Dialog helpers
// ---------------------------------------------------------------------------

function getDialog(): HTMLElement {
  return screen.getByRole('dialog')
}

async function openCreateDialog() {
  await userEvent.click(screen.getByRole('button', { name: 'Create User' }))
}

function getOrgSelect(): HTMLElement {
  return screen.getByRole('combobox', { name: /organization/i })
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

describe('SystemUsersPage — CreateUserDialog default organization', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  it('selects the first organization when the org list loads and none has been chosen', async () => {
    setupFetchMock(defaultEntries([ORG_ALPHA, ORG_BETA]))
    renderSystemUsersPage()

    await openCreateDialog()

    await waitFor(() => {
      expect(getOrgSelect()).toHaveTextContent('Org Alpha')
    })
  })

  it('submits the first organization id when the user creates a user without touching the selector', async () => {
    const capturedBodies = new Map<string, string>()
    setupFetchMock(
      [
        {
          matcher: (u) => u.includes('/api/v1/users') && !u.includes('/api/v1/users/'),
          method: 'POST',
          response: {
            id: 'new-user',
            email: 'newuser@example.com',
            display_name: 'New User',
            auth_provider: 'local',
            is_system_admin: false,
            created_at: '2024-01-01T00:00:00Z',
            updated_at: '2024-01-01T00:00:00Z',
          },
        },
        ...defaultEntries([ORG_ALPHA, ORG_BETA]),
      ],
      capturedBodies,
    )
    renderSystemUsersPage()

    await openCreateDialog()

    await waitFor(() => expect(getOrgSelect()).toHaveTextContent('Org Alpha'))

    const dialog = getDialog()
    await userEvent.type(within(dialog).getByLabelText(/^email$/i), 'newuser@example.com')
    await userEvent.type(within(dialog).getByLabelText(/display name/i), 'New User')
    await userEvent.type(within(dialog).getByLabelText(/^password$/i), 'password123')

    await userEvent.click(within(dialog).getByRole('button', { name: 'Create User' }))

    await waitFor(() => expect(capturedBodies.has('POST:/api/v1/users')).toBe(true))
    const body = JSON.parse(capturedBodies.get('POST:/api/v1/users') ?? '{}')
    expect(body.org_id).toBe(ORG_ALPHA.id)
  })

  it('keeps an explicitly chosen organization selected after the org list is refetched', async () => {
    setupFetchMock(defaultEntries([ORG_ALPHA, ORG_BETA]))
    const { queryClient } = renderSystemUsersPage()

    await openCreateDialog()

    await waitFor(() => expect(getOrgSelect()).toHaveTextContent('Org Alpha'))

    await userEvent.click(getOrgSelect())
    await userEvent.click(screen.getByRole('option', { name: 'Org Beta' }))
    expect(getOrgSelect()).toHaveTextContent('Org Beta')

    // Force the org list query to refetch. The mock returns a brand-new
    // array/object reference (but identical content) on every call, which is
    // exactly the situation that used to trip up an effect keyed on the
    // `orgs` reference and stomp the user's explicit choice.
    await queryClient.refetchQueries({ queryKey: ['orgs'] })

    await waitFor(() => expect(getOrgSelect()).toHaveTextContent('Org Beta'))
  })

  it('resets the selection to the first organization when the dialog is closed and reopened', async () => {
    setupFetchMock(defaultEntries([ORG_ALPHA, ORG_BETA]))
    renderSystemUsersPage()

    await openCreateDialog()

    await waitFor(() => expect(getOrgSelect()).toHaveTextContent('Org Alpha'))

    await userEvent.click(getOrgSelect())
    await userEvent.click(screen.getByRole('option', { name: 'Org Beta' }))
    expect(getOrgSelect()).toHaveTextContent('Org Beta')

    await userEvent.click(within(getDialog()).getByRole('button', { name: /cancel/i }))

    await openCreateDialog()

    await waitFor(() => expect(getOrgSelect()).toHaveTextContent('Org Alpha'))
  })
})
