import React from 'react'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { ToastProvider } from '../hooks/useToast'
import SettingsPage from './SettingsPage'
import OrgDetailSettingsTab from './OrgDetailSettingsTab'
import TeamSettingsTab from './TeamSettingsTab'

const LIMITS = {
  daily_token_limit: 100,
  monthly_token_limit: 200,
  requests_per_minute: 10,
  requests_per_day: 50,
}

const ORG = {
  id: 'org-1',
  name: 'Acme',
  slug: 'acme',
  timezone: null,
  ...LIMITS,
  created_at: '2024-01-01T00:00:00Z',
  updated_at: '2024-01-01T00:00:00Z',
}

const TEAM = {
  id: 'team-1',
  org_id: 'org-1',
  name: 'Backend',
  slug: 'backend',
  member_count: 1,
  key_count: 0,
  ...LIMITS,
  created_at: '2024-01-01T00:00:00Z',
  updated_at: '2024-01-01T00:00:00Z',
}

const ME = {
  id: 'u1',
  email: 'a@example.com',
  display_name: 'A',
  role: 'org_admin',
  org_id: 'org-1',
  is_system_admin: false,
}

let patchBodies: Record<string, unknown>[] = []
let serverLimits: typeof LIMITS = { ...LIMITS }

function json(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  })
}

beforeEach(() => {
  patchBodies = []
  serverLimits = { ...LIMITS }
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init?: RequestInit) => {
      if (init?.method === 'PATCH') {
        const body = JSON.parse(String(init.body)) as Record<string, unknown>
        patchBodies.push(body)
        return json(url.includes('/teams/') ? { ...TEAM, ...serverLimits, ...body } : { ...ORG, ...serverLimits, ...body })
      }
      if (url.includes('/teams/')) return json({ ...TEAM, ...serverLimits })
      if (url.endsWith('/orgs/org-1')) return json({ ...ORG, ...serverLimits })
      return json(ME)
    }),
  )
})

function renderAt(ui: React.ReactElement, path: string, routePath: string) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  const utils = render(
    <QueryClientProvider client={queryClient}>
      <ToastProvider>
        <MemoryRouter initialEntries={[path]}>
          <Routes>
            <Route path={routePath} element={ui} />
          </Routes>
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  )
  return { queryClient, ...utils }
}

const FORMS: { name: string; queryKey: unknown[]; entity: object; render: () => { queryClient: QueryClient } }[] = [
  { name: 'SettingsPage', queryKey: ['org', 'org-1'], entity: ORG, render: () => renderAt(<SettingsPage />, '/org/settings', '/org/settings') },
  {
    name: 'OrgDetailSettingsTab',
    queryKey: ['org', 'org-1'],
    entity: ORG,
    render: () => renderAt(<OrgDetailSettingsTab />, '/orgs/org-1/settings', '/orgs/:orgId/settings'),
  },
  {
    name: 'TeamSettingsTab',
    queryKey: ['teams', 'org-1', 'detail', 'team-1'],
    entity: TEAM,
    render: () => renderAt(<TeamSettingsTab />, '/teams/team-1/settings', '/teams/:teamId/settings'),
  },
]

describe.each(FORMS)('$name limit fields', ({ render: mount, queryKey, entity }) => {
  async function setup() {
    const { queryClient } = mount()
    const input = (await screen.findByLabelText('Daily Token Limit')) as HTMLInputElement
    const save = screen.getByRole('button', { name: /save changes/i })
    return { input, save, queryClient, user: userEvent.setup() }
  }

  it.each(['-5', 'abc', '1e6', '9007199254740992'])(
    'rejects %s with an inline error and disabled Save',
    async (bad) => {
      const { input, save, user } = await setup()
      await user.clear(input)
      await user.type(input, bad)
      expect(input).toHaveAttribute('aria-invalid', 'true')
      expect(input.closest('div')?.querySelector('p[id$="-error"]')).toBeInTheDocument()
      expect(save).toBeDisabled()
      expect(patchBodies).toHaveLength(0)
    },
  )

  it('treats an empty field as unlimited (0) and allows saving', async () => {
    const { input, save, user } = await setup()
    await user.clear(input)
    expect(input).not.toHaveAttribute('aria-invalid')
    expect(save).toBeEnabled()
    await user.click(save)
    await waitFor(() => expect(patchBodies).toHaveLength(1))
    expect(patchBodies[0]).toEqual({ daily_token_limit: 0 })
  })

  it('saves a valid number and sends only the changed field', async () => {
    const { input, save, user } = await setup()
    await user.clear(input)
    await user.type(input, '12345')
    expect(save).toBeEnabled()
    await user.click(save)
    await waitFor(() => expect(patchBodies).toHaveLength(1))
    expect(patchBodies[0]).toEqual({ daily_token_limit: 12345 })
  })

  it('typing 0 into a field that is already 0 is not dirty and sends nothing', async () => {
    serverLimits = { ...LIMITS, daily_token_limit: 0 }
    const { input, save, user } = await setup()
    await user.clear(input)
    await user.type(input, '0')
    expect(input).not.toHaveAttribute('aria-invalid')
    expect(save).toBeDisabled()
    expect(patchBodies).toHaveLength(0)
  })

  it('does not overwrite a refetched limit when only the name changes', async () => {
    serverLimits = { ...LIMITS, daily_token_limit: 0 }
    const { queryClient, user } = await setup()
    const input = screen.getByLabelText('Daily Token Limit')
    expect(input).toHaveValue('0')
    queryClient.setQueryData(queryKey, { ...entity, ...LIMITS, daily_token_limit: 5000 })
    const name = screen.getByLabelText('Name')
    await user.clear(name)
    await user.type(name, 'Renamed')
    await user.click(screen.getByRole('button', { name: /save changes/i }))
    await waitFor(() => expect(patchBodies).toHaveLength(1))
    expect(patchBodies[0]).toEqual({ name: 'Renamed' })
  })
})
