import React from 'react'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { ToastProvider } from '../hooks/useToast'
import MCPServersPage from './MCPServersPage'

// ---------------------------------------------------------------------------
// Types used in mocks (mirror the production types)
// ---------------------------------------------------------------------------

interface MockMCPServerResponse {
  id: string
  name: string
  alias: string
  url: string
  auth_type: string
  auth_header?: string
  oauth_token_url?: string
  oauth_client_id?: string
  oauth_scopes?: string
  source: string
  scope: string
  org_id?: string
  team_id?: string
  is_active: boolean
  code_mode_enabled: boolean
  protocol_version: string
  created_at: string
  updated_at: string
}

// ---------------------------------------------------------------------------
// Test fixtures
// ---------------------------------------------------------------------------

function makeServer(overrides: Partial<MockMCPServerResponse> = {}): MockMCPServerResponse {
  return {
    id: 'server-1',
    name: 'GitHub MCP',
    alias: 'github-mcp',
    url: 'https://mcp.example.com/sse',
    auth_type: 'none',
    source: 'api',
    scope: 'org',
    org_id: 'org-1',
    is_active: true,
    code_mode_enabled: false,
    protocol_version: 'auto',
    created_at: '2024-01-01T00:00:00Z',
    updated_at: '2024-01-01T00:00:00Z',
    ...overrides,
  }
}

const MOCK_ME = {
  id: 'user-1',
  email: 'admin@example.com',
  display_name: 'Org Admin',
  role: 'org_admin',
  org_id: 'org-1',
  is_system_admin: false,
}

const MOCK_TEAMS = { data: [], has_more: false }

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

function renderMCPServersPage() {
  const { Wrapper } = makeWrapper()
  return render(<MCPServersPage />, { wrapper: Wrapper })
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

    // Default fallthrough for unmatched requests
    return {
      ok: true,
      status: 200,
      json: () => Promise.resolve({}),
    }
  }))
}

function defaultEntries(servers: MockMCPServerResponse[] = [makeServer()]): FetchMockEntry[] {
  return [
    {
      matcher: (u) => u.includes('/api/v1/me'),
      method: 'GET',
      response: MOCK_ME,
    },
    {
      matcher: (u) => u.includes('/api/v1/orgs/org-1/mcp-servers'),
      method: 'GET',
      response: servers,
    },
    {
      matcher: (u) => u.includes('/api/v1/orgs/org-1/teams'),
      method: 'GET',
      response: MOCK_TEAMS,
    },
    {
      matcher: (u) => u.includes('/api/v1/mcp-servers/health'),
      method: 'GET',
      response: [],
    },
  ]
}

// ---------------------------------------------------------------------------
// Dialog helpers
// ---------------------------------------------------------------------------

/** Returns the dialog element rendered in the portal. */
function getDialog(titleText: string | RegExp) {
  const heading = screen.getByRole('heading', { name: titleText })
  const dialog = heading.closest('[role="dialog"]')
  if (!dialog) throw new Error(`Could not find dialog with title matching ${String(titleText)}`)
  return dialog as HTMLElement
}

/** Clicks the submit button inside the currently-open dialog. */
async function submitDialog(dialog: HTMLElement, buttonName: string | RegExp) {
  await userEvent.click(within(dialog).getByRole('button', { name: buttonName }))
}

/** Opens the "Add Server" dialog via the page header button. */
async function openCreateDialog() {
  const addButton = await screen.findByRole('button', { name: /add server/i })
  await userEvent.click(addButton)
  await waitFor(() => expect(screen.getByRole('heading', { name: /add mcp server/i })).toBeInTheDocument())
  return getDialog(/add mcp server/i)
}

/** Opens the edit dialog for a server identified by its table row name. */
async function openEditDialogForServer(serverName: string) {
  const nameEl = await screen.findByText(serverName)
  const row = nameEl.closest('tr')
  if (!row) throw new Error(`Could not find table row for server "${serverName}"`)
  const editBtn = within(row).getByTitle('Edit server')
  await userEvent.click(editBtn)
  await waitFor(() => expect(screen.getByRole('heading', { name: /edit mcp server/i })).toBeInTheDocument())
  return getDialog(/edit mcp server/i)
}

/** Fills the required Name/Alias/URL fields in the create dialog. */
async function fillRequiredCreateFields(dialog: HTMLElement) {
  await userEvent.type(within(dialog).getByRole('textbox', { name: /^name$/i }), 'My MCP Server')
  await userEvent.type(within(dialog).getByRole('textbox', { name: /alias/i }), 'my-mcp-server')
  await userEvent.type(within(dialog).getByRole('textbox', { name: /^url$/i }), 'https://mcp.example.com/sse')
}

/** Finds the Protocol Version combobox inside a given dialog. */
function getProtocolVersionSelect(dialog: HTMLElement): HTMLElement {
  return within(dialog).getByRole('combobox', { name: /protocol version/i })
}

// ---------------------------------------------------------------------------
// Tests: CreateMCPServerDialog — Protocol Version field
// ---------------------------------------------------------------------------

describe('CreateMCPServerDialog — Protocol Version field', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  it('renders the field with "Auto-detect" preselected', async () => {
    setupFetchMock(defaultEntries())
    renderMCPServersPage()

    const dialog = await openCreateDialog()
    const select = getProtocolVersionSelect(dialog)
    expect(select).toHaveTextContent(/auto-detect/i)
  })

  it('offers all five protocol version options', async () => {
    setupFetchMock(defaultEntries())
    renderMCPServersPage()

    const dialog = await openCreateDialog()
    const select = getProtocolVersionSelect(dialog)
    await userEvent.click(select)

    expect(screen.getByRole('option', { name: /auto-detect \(recommended\)/i })).toBeInTheDocument()
    expect(screen.getByRole('option', { name: '2026-07-28' })).toBeInTheDocument()
    expect(screen.getByRole('option', { name: '2025-11-25' })).toBeInTheDocument()
    expect(screen.getByRole('option', { name: '2025-06-18' })).toBeInTheDocument()
    expect(screen.getByRole('option', { name: '2025-03-26' })).toBeInTheDocument()
  })

  it('sends protocol_version "auto" when left untouched', async () => {
    const capturedBodies = new Map<string, string>()
    setupFetchMock(
      [
        {
          matcher: (u) => u.includes('/api/v1/orgs/org-1/mcp-servers'),
          method: 'POST',
          response: makeServer({ id: 'new-server' }),
        },
        ...defaultEntries(),
      ],
      capturedBodies,
    )
    renderMCPServersPage()

    const dialog = await openCreateDialog()
    await fillRequiredCreateFields(dialog)
    await submitDialog(dialog, /add server/i)

    await waitFor(() => expect(capturedBodies.has('POST:/api/v1/orgs/org-1/mcp-servers')).toBe(true))
    const body = JSON.parse(capturedBodies.get('POST:/api/v1/orgs/org-1/mcp-servers')!)
    expect(body.protocol_version).toBe('auto')
  })

  it('sends the selected protocol_version revision', async () => {
    const capturedBodies = new Map<string, string>()
    setupFetchMock(
      [
        {
          matcher: (u) => u.includes('/api/v1/orgs/org-1/mcp-servers'),
          method: 'POST',
          response: makeServer({ id: 'new-server' }),
        },
        ...defaultEntries(),
      ],
      capturedBodies,
    )
    renderMCPServersPage()

    const dialog = await openCreateDialog()
    await fillRequiredCreateFields(dialog)

    const select = getProtocolVersionSelect(dialog)
    await userEvent.click(select)
    await userEvent.click(screen.getByRole('option', { name: '2025-06-18' }))

    await submitDialog(dialog, /add server/i)

    await waitFor(() => expect(capturedBodies.has('POST:/api/v1/orgs/org-1/mcp-servers')).toBe(true))
    const body = JSON.parse(capturedBodies.get('POST:/api/v1/orgs/org-1/mcp-servers')!)
    expect(body.protocol_version).toBe('2025-06-18')
  })

  it('shows helper text explaining the field is an override for auto-detection', async () => {
    setupFetchMock(defaultEntries())
    renderMCPServersPage()

    await openCreateDialog()

    expect(
      screen.getByText(/auto-detects which mcp revision this server speaks/i),
    ).toBeInTheDocument()
  })
})

// ---------------------------------------------------------------------------
// Tests: EditMCPServerDialog — Protocol Version field
// ---------------------------------------------------------------------------

describe('EditMCPServerDialog — Protocol Version field', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  it("initializes with the server's saved protocol_version", async () => {
    const server = makeServer({ id: 'server-1', name: 'GitHub MCP', protocol_version: '2025-11-25' })
    setupFetchMock(defaultEntries([server]))
    renderMCPServersPage()

    const dialog = await openEditDialogForServer('GitHub MCP')
    const select = getProtocolVersionSelect(dialog)
    expect(select).toHaveTextContent('2025-11-25')
  })

  it('falls back to "Auto-detect" when the server has no protocol_version set', async () => {
    const server = makeServer({ id: 'server-1', name: 'GitHub MCP', protocol_version: '' })
    setupFetchMock(defaultEntries([server]))
    renderMCPServersPage()

    const dialog = await openEditDialogForServer('GitHub MCP')
    const select = getProtocolVersionSelect(dialog)
    expect(select).toHaveTextContent(/auto-detect/i)
  })

  it('does NOT include protocol_version in the PATCH payload when left unchanged', async () => {
    const capturedBodies = new Map<string, string>()
    const server = makeServer({ id: 'server-1', name: 'GitHub MCP', protocol_version: '2025-06-18' })
    setupFetchMock(
      [
        ...defaultEntries([server]),
        {
          matcher: (u) => u.includes('/api/v1/mcp-servers/server-1'),
          method: 'PATCH',
          response: { ...server, name: 'GitHub MCP Renamed' },
        },
      ],
      capturedBodies,
    )
    renderMCPServersPage()

    const dialog = await openEditDialogForServer('GitHub MCP')

    // Change an unrelated field only — leave Protocol Version untouched
    const nameInput = within(dialog).getByRole('textbox', { name: /^name$/i })
    await userEvent.clear(nameInput)
    await userEvent.type(nameInput, 'GitHub MCP Renamed')

    await submitDialog(dialog, /save changes/i)

    await waitFor(() => expect(capturedBodies.has('PATCH:/api/v1/mcp-servers/server-1')).toBe(true))
    const body = JSON.parse(capturedBodies.get('PATCH:/api/v1/mcp-servers/server-1')!)
    expect(body).not.toHaveProperty('protocol_version')
    expect(body.name).toBe('GitHub MCP Renamed')
  })

  it('includes protocol_version in the PATCH payload when changed', async () => {
    const capturedBodies = new Map<string, string>()
    const server = makeServer({ id: 'server-1', name: 'GitHub MCP', protocol_version: 'auto' })
    setupFetchMock(
      [
        ...defaultEntries([server]),
        {
          matcher: (u) => u.includes('/api/v1/mcp-servers/server-1'),
          method: 'PATCH',
          response: { ...server, protocol_version: '2026-07-28' },
        },
      ],
      capturedBodies,
    )
    renderMCPServersPage()

    const dialog = await openEditDialogForServer('GitHub MCP')
    const select = getProtocolVersionSelect(dialog)
    await userEvent.click(select)
    await userEvent.click(screen.getByRole('option', { name: '2026-07-28' }))

    await submitDialog(dialog, /save changes/i)

    await waitFor(() => expect(capturedBodies.has('PATCH:/api/v1/mcp-servers/server-1')).toBe(true))
    const body = JSON.parse(capturedBodies.get('PATCH:/api/v1/mcp-servers/server-1')!)
    expect(body.protocol_version).toBe('2026-07-28')
    // No other field was touched
    expect(Object.keys(body)).toEqual(['protocol_version'])
  })

  it('shows helper text explaining the field is an override for auto-detection', async () => {
    const server = makeServer({ id: 'server-1', name: 'GitHub MCP' })
    setupFetchMock(defaultEntries([server]))
    renderMCPServersPage()

    await openEditDialogForServer('GitHub MCP')

    expect(
      screen.getByText(/auto-detects which mcp revision this server speaks/i),
    ).toBeInTheDocument()
  })
})
