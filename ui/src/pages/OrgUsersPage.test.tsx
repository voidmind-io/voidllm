import React from 'react'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { ToastProvider } from '../hooks/useToast'
import OrgUsersPage from './OrgUsersPage'

// ---------------------------------------------------------------------------
// Types used in mocks (mirror the production types)
// ---------------------------------------------------------------------------

interface MockMembership {
  id: string
  org_id: string
  user_id: string
  role: string
  daily_token_limit: number
  monthly_token_limit: number
  requests_per_minute: number
  requests_per_day: number
  created_at: string
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

function makeMembership(overrides: Partial<MockMembership> = {}): MockMembership {
  return {
    id: 'mem-1',
    org_id: 'org-1',
    user_id: 'user-1',
    role: 'member',
    daily_token_limit: 0,
    monthly_token_limit: 0,
    requests_per_minute: 0,
    requests_per_day: 0,
    created_at: '2024-01-01T00:00:00Z',
    ...overrides,
  }
}

function makeUser(overrides: Partial<MockUser> = {}): MockUser {
  return {
    id: 'user-1',
    email: 'alice@example.com',
    display_name: 'Alice',
    auth_provider: 'local',
    is_system_admin: false,
    created_at: '2024-01-01T00:00:00Z',
    updated_at: '2024-01-01T00:00:00Z',
    ...overrides,
  }
}

const USER_ALICE = makeUser({ id: 'user-1', email: 'alice@example.com', display_name: 'Alice' })
const USER_BOB = makeUser({ id: 'user-2', email: 'bob@example.com', display_name: 'Bob' })

const MEMBER_UNLIMITED = makeMembership({
  id: 'mem-1',
  user_id: 'user-1',
  role: 'member',
  daily_token_limit: 0,
  monthly_token_limit: 0,
  requests_per_minute: 0,
  requests_per_day: 0,
})

const MEMBER_WITH_LIMITS = makeMembership({
  id: 'mem-2',
  user_id: 'user-2',
  role: 'member',
  daily_token_limit: 500,
  monthly_token_limit: 0,
  requests_per_minute: 5,
  requests_per_day: 0,
})

const MOCK_ME_ADMIN = {
  id: 'user-admin',
  email: 'admin@example.com',
  display_name: 'Admin',
  role: 'org_admin',
  org_id: 'org-1',
  is_system_admin: false,
}

const MOCK_ME_MEMBER = {
  id: 'user-1',
  email: 'alice@example.com',
  display_name: 'Alice',
  role: 'member',
  org_id: 'org-1',
  is_system_admin: false,
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
        <ToastProvider>{children}</ToastProvider>
      </QueryClientProvider>
    )
  }
  return { queryClient, Wrapper }
}

function renderOrgUsersPage() {
  const { queryClient, Wrapper } = makeWrapper()
  const utils = render(<OrgUsersPage />, { wrapper: Wrapper })
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
        json: () => Promise.resolve(entry.response),
      }
    }

    return {
      ok: true,
      status: 200,
      json: () => Promise.resolve({}),
    }
  }))
}

function defaultEntries(
  me: unknown,
  members: MockMembership[],
  users: MockUser[] = [USER_ALICE, USER_BOB],
): FetchMockEntry[] {
  return [
    {
      matcher: (u) => u.includes('/api/v1/me'),
      method: 'GET',
      response: me,
    },
    {
      matcher: (u) => u.includes('/api/v1/orgs/org-1/members'),
      method: 'GET',
      response: { data: members, has_more: false },
    },
    ...users.map((user) => ({
      matcher: (u: string) => u.includes(`/api/v1/users/${user.id}`),
      method: 'GET',
      response: user,
    })),
  ]
}

// ---------------------------------------------------------------------------
// Row / dialog helpers
// ---------------------------------------------------------------------------

async function findRowByDisplayName(name: string): Promise<HTMLElement> {
  const cell = await screen.findByText(name)
  const row = cell.closest('tr')
  if (!row) throw new Error(`Could not find table row for user "${name}"`)
  return row as HTMLElement
}

function getDialog(titleText: string | RegExp): HTMLElement {
  const heading = screen.getByRole('heading', { name: titleText })
  const dialog = heading.closest('[role="dialog"]')
  if (!dialog) throw new Error(`Could not find dialog with title matching ${String(titleText)}`)
  return dialog as HTMLElement
}

async function openEditLimitsDialog(displayName: string): Promise<HTMLElement> {
  const row = await findRowByDisplayName(displayName)
  await userEvent.click(within(row).getByTitle('Edit limits'))
  await waitFor(() => expect(screen.getByRole('heading', { name: /edit user limits/i })).toBeInTheDocument())
  return getDialog(/edit user limits/i)
}

// ---------------------------------------------------------------------------
// Tests: Limits column
// ---------------------------------------------------------------------------

describe('OrgUsersPage — Limits column', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  it('shows "Unlimited" when a member has no limits set', async () => {
    setupFetchMock(defaultEntries(MOCK_ME_ADMIN, [MEMBER_UNLIMITED]))
    renderOrgUsersPage()

    await screen.findByText('Alice')
    const row = await findRowByDisplayName('Alice')
    expect(within(row).getByText('Unlimited')).toBeInTheDocument()
  })

  it('shows a summary of the configured limits when limits are set', async () => {
    setupFetchMock(defaultEntries(MOCK_ME_ADMIN, [MEMBER_UNLIMITED, MEMBER_WITH_LIMITS]))
    renderOrgUsersPage()

    const row = await findRowByDisplayName('Bob')
    // requests_per_minute=5, daily_token_limit=500 -> "5 rpm · 500 tok/day"
    expect(within(row).getByText('5 rpm · 500 tok/day')).toBeInTheDocument()
  })
})

// ---------------------------------------------------------------------------
// Tests: Edit limits visibility
// ---------------------------------------------------------------------------

describe('OrgUsersPage — Edit limits action visibility', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  it('shows the "Edit limits" action for org admins', async () => {
    setupFetchMock(defaultEntries(MOCK_ME_ADMIN, [MEMBER_WITH_LIMITS]))
    renderOrgUsersPage()

    const row = await findRowByDisplayName('Bob')
    expect(within(row).getByTitle('Edit limits')).toBeInTheDocument()
  })

  it('does not render the members table (or the edit limits action) for non-admins', async () => {
    setupFetchMock(defaultEntries(MOCK_ME_MEMBER, [MEMBER_WITH_LIMITS]))
    renderOrgUsersPage()

    await waitFor(() => {
      expect(
        screen.getByText(/only organization admins can manage users/i),
      ).toBeInTheDocument()
    })
    expect(screen.queryByTitle('Edit limits')).not.toBeInTheDocument()
    expect(screen.queryByText('Bob')).not.toBeInTheDocument()
  })
})

// ---------------------------------------------------------------------------
// Tests: EditLimitsDialog behavior
// ---------------------------------------------------------------------------

describe('OrgUsersPage — EditLimitsDialog', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  it('disables the Save button when the form has not been changed', async () => {
    setupFetchMock(defaultEntries(MOCK_ME_ADMIN, [MEMBER_WITH_LIMITS]))
    renderOrgUsersPage()

    const dialog = await openEditLimitsDialog('Bob')
    expect(within(dialog).getByRole('button', { name: /save changes/i })).toBeDisabled()
  })

  it('enables the Save button once a field is changed', async () => {
    setupFetchMock(defaultEntries(MOCK_ME_ADMIN, [MEMBER_WITH_LIMITS]))
    renderOrgUsersPage()

    const dialog = await openEditLimitsDialog('Bob')
    const monthlyInput = within(dialog).getByLabelText(/monthly token limit/i)
    await userEvent.type(monthlyInput, '2000')

    expect(within(dialog).getByRole('button', { name: /save changes/i })).not.toBeDisabled()
  })

  it('sends only the changed fields on submit', async () => {
    const capturedBodies = new Map<string, string>()
    setupFetchMock(
      [
        {
          matcher: (u) => u.includes('/api/v1/orgs/org-1/members/mem-2'),
          method: 'PATCH',
          response: { ...MEMBER_WITH_LIMITS, monthly_token_limit: 2000 },
        },
        ...defaultEntries(MOCK_ME_ADMIN, [MEMBER_WITH_LIMITS]),
      ],
      capturedBodies,
    )
    renderOrgUsersPage()

    const dialog = await openEditLimitsDialog('Bob')
    // Only touch the monthly token limit field - daily/rpm/rpd stay as-is.
    const monthlyInput = within(dialog).getByLabelText(/monthly token limit/i)
    await userEvent.type(monthlyInput, '2000')

    await userEvent.click(within(dialog).getByRole('button', { name: /save changes/i }))

    await waitFor(() =>
      expect(capturedBodies.has('PATCH:/api/v1/orgs/org-1/members/mem-2')).toBe(true),
    )
    const body = JSON.parse(capturedBodies.get('PATCH:/api/v1/orgs/org-1/members/mem-2')!)
    expect(body).toEqual({ monthly_token_limit: 2000 })
  })

  it('sends 0 when a previously-set field is cleared to empty', async () => {
    const capturedBodies = new Map<string, string>()
    setupFetchMock(
      [
        {
          matcher: (u) => u.includes('/api/v1/orgs/org-1/members/mem-2'),
          method: 'PATCH',
          response: { ...MEMBER_WITH_LIMITS, daily_token_limit: 0 },
        },
        ...defaultEntries(MOCK_ME_ADMIN, [MEMBER_WITH_LIMITS]),
      ],
      capturedBodies,
    )
    renderOrgUsersPage()

    const dialog = await openEditLimitsDialog('Bob')
    const dailyInput = within(dialog).getByLabelText(/daily token limit/i)
    // MEMBER_WITH_LIMITS.daily_token_limit is 500 - clear it entirely.
    await userEvent.clear(dailyInput)

    await userEvent.click(within(dialog).getByRole('button', { name: /save changes/i }))

    await waitFor(() =>
      expect(capturedBodies.has('PATCH:/api/v1/orgs/org-1/members/mem-2')).toBe(true),
    )
    const body = JSON.parse(capturedBodies.get('PATCH:/api/v1/orgs/org-1/members/mem-2')!)
    expect(body).toEqual({ daily_token_limit: 0 })
  })

  it('shows an inline error and disables Save when a negative limit is entered, and sends no PATCH', async () => {
    const capturedBodies = new Map<string, string>()
    setupFetchMock(
      [
        {
          matcher: (u) => u.includes('/api/v1/orgs/org-1/members/mem-2'),
          method: 'PATCH',
          response: MEMBER_WITH_LIMITS,
        },
        ...defaultEntries(MOCK_ME_ADMIN, [MEMBER_WITH_LIMITS]),
      ],
      capturedBodies,
    )
    renderOrgUsersPage()

    const dialog = await openEditLimitsDialog('Bob')
    const rpmInput = within(dialog).getByLabelText(/requests per minute/i)
    await userEvent.clear(rpmInput)
    await userEvent.type(rpmInput, '-1')

    expect(await within(dialog).findByRole('alert')).toHaveTextContent(/whole number of 0 or greater/i)
    expect(within(dialog).getByRole('button', { name: /save changes/i })).toBeDisabled()

    // Clicking a disabled button is a no-op, but assert no PATCH was ever sent
    // regardless of how disablement is implemented.
    await userEvent.click(within(dialog).getByRole('button', { name: /save changes/i }))
    expect(capturedBodies.has('PATCH:/api/v1/orgs/org-1/members/mem-2')).toBe(false)
  })
})
