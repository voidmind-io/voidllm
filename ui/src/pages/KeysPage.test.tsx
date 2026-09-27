import React from 'react'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { ToastProvider } from '../hooks/useToast'
import KeysPage from './KeysPage'

// ---------------------------------------------------------------------------
// Types used in mocks (mirror the production types)
// ---------------------------------------------------------------------------

interface MockAPIKey {
  id: string
  key?: string
  key_hint: string
  key_type: string
  name: string
  org_id: string
  team_id: string | null
  user_id: string | null
  service_account_id: string | null
  daily_token_limit: number
  monthly_token_limit: number
  requests_per_minute: number
  requests_per_day: number
  expires_at: string | null
  last_used_at: string | null
  created_by: string
  created_at: string
  updated_at: string
}

// ---------------------------------------------------------------------------
// Test fixtures
// ---------------------------------------------------------------------------

function makeKey(overrides: Partial<MockAPIKey> = {}): MockAPIKey {
  return {
    id: 'key-1',
    key_hint: 'sk-...abcd',
    key_type: 'user_key',
    name: 'Prod Key',
    org_id: 'org-1',
    team_id: null,
    user_id: 'user-1',
    service_account_id: null,
    daily_token_limit: 1000,
    monthly_token_limit: 0,
    requests_per_minute: 60,
    requests_per_day: 0,
    expires_at: null,
    last_used_at: null,
    created_by: 'user-1',
    created_at: '2024-01-01T00:00:00Z',
    updated_at: '2024-01-01T00:00:00Z',
    ...overrides,
  }
}

const KEY_WITH_LIMITS = makeKey({ id: 'key-1', name: 'Prod Key' })

const MOCK_ME_ORG_ADMIN = {
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

function renderKeysPage() {
  const { Wrapper } = makeWrapper()
  return render(<KeysPage />, { wrapper: Wrapper })
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

function defaultEntries(me: unknown, keys: MockAPIKey[] = [KEY_WITH_LIMITS]): FetchMockEntry[] {
  return [
    {
      matcher: (u) => u.includes('/api/v1/me') && !u.includes('available-models'),
      method: 'GET',
      response: me,
    },
    {
      matcher: (u) => u.includes('/api/v1/me/available-models'),
      method: 'GET',
      response: { models: [] },
    },
    {
      matcher: (u) => u.includes('/api/v1/orgs/org-1/keys'),
      method: 'GET',
      response: { data: keys, has_more: false },
    },
    {
      matcher: (u) => u.includes('/api/v1/orgs/org-1/teams'),
      method: 'GET',
      response: { data: [], has_more: false },
    },
    {
      matcher: (u) => u.includes('/api/v1/orgs/org-1/service-accounts'),
      method: 'GET',
      response: { data: [], has_more: false },
    },
  ]
}

// ---------------------------------------------------------------------------
// Dialog helpers
// ---------------------------------------------------------------------------

function getDialog(titleText: string | RegExp): HTMLElement {
  const heading = screen.getByRole('heading', { name: titleText })
  const dialog = heading.closest('[role="dialog"]')
  if (!dialog) throw new Error(`Could not find dialog with title matching ${String(titleText)}`)
  return dialog as HTMLElement
}

async function openCreateDialog(): Promise<HTMLElement> {
  await userEvent.click(screen.getByRole('button', { name: 'Create Key' }))
  await waitFor(() => expect(screen.getByRole('heading', { name: /create api key/i })).toBeInTheDocument())
  return getDialog(/create api key/i)
}

async function openEditDialogForKey(keyName: string): Promise<HTMLElement> {
  const nameCell = await screen.findByText(keyName)
  const row = nameCell.closest('tr')
  if (!row) throw new Error(`Could not find table row for key "${keyName}"`)
  await userEvent.click(within(row as HTMLElement).getByTitle('Edit key'))
  await waitFor(() => expect(screen.getByRole('heading', { name: /edit api key/i })).toBeInTheDocument())
  return getDialog(/edit api key/i)
}

/** Reads a ReadOnlyLimit's displayed value by its label text. */
function getReadOnlyValue(dialog: HTMLElement, label: string): string | null | undefined {
  const labelEl = within(dialog).getByText(label)
  return labelEl.nextElementSibling?.textContent
}

// ---------------------------------------------------------------------------
// Tests: CreateKeyDialog — limits section visibility
// ---------------------------------------------------------------------------

describe('KeysPage — CreateKeyDialog limits section visibility', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  it('shows the "Rate & Token Limits" section for org admins', async () => {
    setupFetchMock(defaultEntries(MOCK_ME_ORG_ADMIN))
    renderKeysPage()

    const dialog = await openCreateDialog()
    expect(within(dialog).getByText(/rate & token limits/i)).toBeInTheDocument()
  })

  it('hides the "Rate & Token Limits" section for members', async () => {
    setupFetchMock(defaultEntries(MOCK_ME_MEMBER))
    renderKeysPage()

    const dialog = await openCreateDialog()
    expect(within(dialog).queryByText(/rate & token limits/i)).not.toBeInTheDocument()
  })
})

// ---------------------------------------------------------------------------
// Tests: CreateKeyDialog — payload
// ---------------------------------------------------------------------------

describe('KeysPage — CreateKeyDialog payload', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  it('member payload contains no limit keys', async () => {
    const capturedBodies = new Map<string, string>()
    setupFetchMock(
      [
        {
          matcher: (u) => u.includes('/api/v1/orgs/org-1/keys') && !u.includes('/model-access'),
          method: 'POST',
          response: makeKey({ id: 'new-key', name: 'My New Key', key: 'vl_uk_secret' }),
        },
        ...defaultEntries(MOCK_ME_MEMBER),
      ],
      capturedBodies,
    )
    renderKeysPage()

    const dialog = await openCreateDialog()
    await userEvent.type(within(dialog).getByLabelText(/^name$/i), 'My New Key')
    await userEvent.click(within(dialog).getByRole('button', { name: /create key/i }))

    await waitFor(() => expect(capturedBodies.has('POST:/api/v1/orgs/org-1/keys')).toBe(true))
    const body = JSON.parse(capturedBodies.get('POST:/api/v1/orgs/org-1/keys')!)
    expect(body).not.toHaveProperty('daily_token_limit')
    expect(body).not.toHaveProperty('monthly_token_limit')
    expect(body).not.toHaveProperty('requests_per_minute')
    expect(body).not.toHaveProperty('requests_per_day')
  })

  it('org admin payload contains only the limit field that was touched', async () => {
    const capturedBodies = new Map<string, string>()
    setupFetchMock(
      [
        {
          matcher: (u) => u.includes('/api/v1/orgs/org-1/keys') && !u.includes('/model-access'),
          method: 'POST',
          response: makeKey({ id: 'new-key-2', name: 'Admin Key', key: 'vl_uk_secret2' }),
        },
        ...defaultEntries(MOCK_ME_ORG_ADMIN),
      ],
      capturedBodies,
    )
    renderKeysPage()

    const dialog = await openCreateDialog()
    await userEvent.type(within(dialog).getByLabelText(/^name$/i), 'Admin Key')

    // Expand the collapsible limits section and set only the daily token limit.
    await userEvent.click(within(dialog).getByText(/rate & token limits/i))
    await userEvent.type(within(dialog).getByLabelText(/daily token limit/i), '2000')

    await userEvent.click(within(dialog).getByRole('button', { name: /create key/i }))

    await waitFor(() => expect(capturedBodies.has('POST:/api/v1/orgs/org-1/keys')).toBe(true))
    const body = JSON.parse(capturedBodies.get('POST:/api/v1/orgs/org-1/keys')!)
    expect(body.daily_token_limit).toBe(2000)
    expect(body).not.toHaveProperty('monthly_token_limit')
    expect(body).not.toHaveProperty('requests_per_minute')
    expect(body).not.toHaveProperty('requests_per_day')
  })

  it('shows an inline error and disables Create when a negative limit is entered, and sends no POST', async () => {
    const capturedBodies = new Map<string, string>()
    setupFetchMock(
      [
        {
          matcher: (u) => u.includes('/api/v1/orgs/org-1/keys') && !u.includes('/model-access'),
          method: 'POST',
          response: makeKey({ id: 'new-key-3', name: 'Invalid Limit Key' }),
        },
        ...defaultEntries(MOCK_ME_ORG_ADMIN),
      ],
      capturedBodies,
    )
    renderKeysPage()

    const dialog = await openCreateDialog()
    await userEvent.type(within(dialog).getByLabelText(/^name$/i), 'Invalid Limit Key')

    await userEvent.click(within(dialog).getByText(/rate & token limits/i))
    await userEvent.type(within(dialog).getByLabelText(/daily token limit/i), '-1')

    expect(await within(dialog).findByRole('alert')).toHaveTextContent(/whole number of 0 or greater/i)
    expect(within(dialog).getByRole('button', { name: /create key/i })).toBeDisabled()

    await userEvent.click(within(dialog).getByRole('button', { name: /create key/i }))
    expect(capturedBodies.has('POST:/api/v1/orgs/org-1/keys')).toBe(false)
  })
})

// ---------------------------------------------------------------------------
// Tests: EditKeyDialog — member (read-only limits)
// ---------------------------------------------------------------------------

describe('KeysPage — EditKeyDialog for members', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  it('shows read-only limit values and a note that only org admins can change them', async () => {
    setupFetchMock(defaultEntries(MOCK_ME_MEMBER, [KEY_WITH_LIMITS]))
    renderKeysPage()

    const dialog = await openEditDialogForKey('Prod Key')

    expect(getReadOnlyValue(dialog, 'Daily Token Limit')).toBe('1,000')
    expect(getReadOnlyValue(dialog, 'Monthly Token Limit')).toBe('Unlimited')
    expect(getReadOnlyValue(dialog, 'Requests per Minute')).toBe('60')
    expect(getReadOnlyValue(dialog, 'Requests per Day')).toBe('Unlimited')

    expect(
      within(dialog).getByText(/only organization admins can change key limits/i),
    ).toBeInTheDocument()

    // No editable number inputs for limits should be present.
    expect(within(dialog).queryAllByRole('spinbutton')).toHaveLength(0)
  })

  it('rename payload contains no limit keys', async () => {
    const capturedBodies = new Map<string, string>()
    setupFetchMock(
      [
        {
          matcher: (u) => u.includes('/api/v1/orgs/org-1/keys/key-1'),
          method: 'PATCH',
          response: { ...KEY_WITH_LIMITS, name: 'Renamed Key' },
        },
        ...defaultEntries(MOCK_ME_MEMBER, [KEY_WITH_LIMITS]),
      ],
      capturedBodies,
    )
    renderKeysPage()

    const dialog = await openEditDialogForKey('Prod Key')
    const nameInput = within(dialog).getByLabelText(/^name$/i)
    await userEvent.clear(nameInput)
    await userEvent.type(nameInput, 'Renamed Key')

    await userEvent.click(within(dialog).getByRole('button', { name: /save changes/i }))

    await waitFor(() =>
      expect(capturedBodies.has('PATCH:/api/v1/orgs/org-1/keys/key-1')).toBe(true),
    )
    const body = JSON.parse(capturedBodies.get('PATCH:/api/v1/orgs/org-1/keys/key-1')!)
    expect(body).toEqual({ name: 'Renamed Key' })
  })
})

// ---------------------------------------------------------------------------
// Tests: EditKeyDialog — org admin (editable limits)
// ---------------------------------------------------------------------------

describe('KeysPage — EditKeyDialog for org admins', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  it('sends only the changed limit field on submit', async () => {
    const capturedBodies = new Map<string, string>()
    setupFetchMock(
      [
        {
          matcher: (u) => u.includes('/api/v1/orgs/org-1/keys/key-1'),
          method: 'PATCH',
          response: { ...KEY_WITH_LIMITS, requests_per_minute: 120 },
        },
        ...defaultEntries(MOCK_ME_ORG_ADMIN, [KEY_WITH_LIMITS]),
      ],
      capturedBodies,
    )
    renderKeysPage()

    const dialog = await openEditDialogForKey('Prod Key')

    const rpmInput = within(dialog).getByLabelText(/requests per minute/i)
    await userEvent.clear(rpmInput)
    await userEvent.type(rpmInput, '120')

    await userEvent.click(within(dialog).getByRole('button', { name: /save changes/i }))

    await waitFor(() =>
      expect(capturedBodies.has('PATCH:/api/v1/orgs/org-1/keys/key-1')).toBe(true),
    )
    const body = JSON.parse(capturedBodies.get('PATCH:/api/v1/orgs/org-1/keys/key-1')!)
    expect(body).toEqual({ requests_per_minute: 120 })
  })

  it('shows an inline error and disables Save when a negative limit is entered, and sends no PATCH', async () => {
    const capturedBodies = new Map<string, string>()
    setupFetchMock(
      [
        {
          matcher: (u) => u.includes('/api/v1/orgs/org-1/keys/key-1'),
          method: 'PATCH',
          response: KEY_WITH_LIMITS,
        },
        ...defaultEntries(MOCK_ME_ORG_ADMIN, [KEY_WITH_LIMITS]),
      ],
      capturedBodies,
    )
    renderKeysPage()

    const dialog = await openEditDialogForKey('Prod Key')

    const rpmInput = within(dialog).getByLabelText(/requests per minute/i)
    await userEvent.clear(rpmInput)
    await userEvent.type(rpmInput, '-1')

    expect(await within(dialog).findByRole('alert')).toHaveTextContent(/whole number of 0 or greater/i)
    expect(within(dialog).getByRole('button', { name: /save changes/i })).toBeDisabled()

    await userEvent.click(within(dialog).getByRole('button', { name: /save changes/i }))
    expect(capturedBodies.has('PATCH:/api/v1/orgs/org-1/keys/key-1')).toBe(false)
  })
})
