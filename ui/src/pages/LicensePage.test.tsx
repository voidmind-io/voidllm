import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { ToastProvider } from '../hooks/useToast'
import LicensePage from './LicensePage'

const ME_ADMIN = {
  id: 'u1',
  email: 'admin@example.com',
  display_name: 'Admin',
  role: 'system_admin',
  org_id: 'org-1',
  is_system_admin: true,
}

const COMMUNITY = {
  edition: 'community',
  valid: true,
  features: [],
  max_orgs: 1,
  max_teams: 3,
  fallback_max_depth: 0,
}

interface FetchCall {
  url: string
  method: string
  body?: string
}

function setupFetch(
  license: unknown,
  trialResponse: { status: number; body: unknown } = { status: 202, body: { status: 'check_inbox' } },
) {
  const calls: FetchCall[] = []
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : input.toString()
    const method = (init?.method ?? 'GET').toUpperCase()
    calls.push({ url, method, body: init?.body as string | undefined })
    let status = 200
    let body: unknown = {}
    if (url.endsWith('/api/v1/me')) body = ME_ADMIN
    else if (url.endsWith('/api/v1/license/trial')) {
      status = trialResponse.status
      body = trialResponse.body
    } else if (url.endsWith('/api/v1/license')) body = license
    return { ok: status < 400, status, statusText: 'err', json: () => Promise.resolve(body) }
  }))
  return calls
}

function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  return render(
    <QueryClientProvider client={queryClient}>
      <ToastProvider>
        <MemoryRouter>
          <LicensePage />
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  )
}

async function openForm() {
  const user = userEvent.setup()
  await user.click(await screen.findByRole('button', { name: 'Start free trial' }))
  return user
}

const boxA = () => screen.getByRole('checkbox', { name: /Send me a 30-day Enterprise trial key/ })
const boxB = () => screen.getByRole('checkbox', { name: /VoidLLM may contact me by email/ })
const submitBtn = () => screen.getByRole('button', { name: 'Request trial key' })

describe('LicensePage trial request', () => {
  beforeEach(() => {
    vi.unstubAllGlobals()
    localStorage.clear()
  })

  it('shows the trial card for community', async () => {
    setupFetch(COMMUNITY)
    renderPage()
    expect(await screen.findByText('Try Enterprise free for 30 days')).toBeInTheDocument()
  })

  it('checkboxes are unchecked by default and submit is disabled', async () => {
    setupFetch(COMMUNITY)
    renderPage()
    await openForm()
    expect(boxA()).not.toBeChecked()
    expect(boxB()).not.toBeChecked()
    expect(submitBtn()).toBeDisabled()
  })

  it('keeps submit disabled without consent A even with a valid email', async () => {
    setupFetch(COMMUNITY)
    renderPage()
    const user = await openForm()
    await user.type(screen.getByLabelText('Work email'), 'dev@company.com')
    expect(submitBtn()).toBeDisabled()
    await user.click(boxB())
    expect(submitBtn()).toBeDisabled()
  })

  it('keeps submit disabled for an invalid email', async () => {
    setupFetch(COMMUNITY)
    renderPage()
    const user = await openForm()
    await user.type(screen.getByLabelText('Work email'), 'not-an-email')
    await user.click(boxA())
    expect(submitBtn()).toBeDisabled()
  })

  it('enables submit with valid email and consent A only (B optional)', async () => {
    setupFetch(COMMUNITY)
    renderPage()
    const user = await openForm()
    await user.type(screen.getByLabelText('Work email'), 'dev@company.com')
    await user.click(boxA())
    expect(submitBtn()).toBeEnabled()
  })

  it('sends the exact body and shows the inbox state', async () => {
    const calls = setupFetch(COMMUNITY)
    renderPage()
    const user = await openForm()
    await user.type(screen.getByLabelText('Work email'), 'dev@company.com')
    await user.type(screen.getByLabelText('Company (optional)'), 'Acme')
    await user.click(boxA())
    await user.click(boxB())
    await user.click(submitBtn())

    await waitFor(() => {
      expect(screen.getByText(/Check your inbox and confirm the email/)).toBeInTheDocument()
    })
    const call = calls.find((c) => c.url.endsWith('/license/trial'))
    expect(call?.method).toBe('POST')
    expect(JSON.parse(call?.body ?? '{}')).toEqual({
      email: 'dev@company.com',
      company: 'Acme',
      consent_trial: true,
      consent_marketing: true,
    })
  })

  it('omits company and sends consent_marketing false when left default', async () => {
    const calls = setupFetch(COMMUNITY)
    renderPage()
    const user = await openForm()
    await user.type(screen.getByLabelText('Work email'), 'dev@company.com')
    await user.click(boxA())
    await user.click(submitBtn())
    await waitFor(() => expect(calls.some((c) => c.url.endsWith('/license/trial'))).toBe(true))
    const call = calls.find((c) => c.url.endsWith('/license/trial'))
    expect(JSON.parse(call?.body ?? '{}')).toEqual({
      email: 'dev@company.com',
      consent_trial: true,
      consent_marketing: false,
    })
  })

  it('shows the server message on 429', async () => {
    setupFetch(COMMUNITY, { status: 429, body: { error: { message: 'Too many requests' } } })
    renderPage()
    const user = await openForm()
    await user.type(screen.getByLabelText('Work email'), 'dev@company.com')
    await user.click(boxA())
    await user.click(submitBtn())
    expect(await screen.findAllByText('Too many requests')).not.toHaveLength(0)
    expect(screen.queryByText(/Check your inbox/)).not.toBeInTheDocument()
  })

  it('shows the server message on 502', async () => {
    const message = 'Could not reach the license server. You can request a trial at https://voidllm.ai/trial'
    setupFetch(COMMUNITY, { status: 502, body: { error: { code: 'trial_unavailable', message } } })
    renderPage()
    const user = await openForm()
    await user.type(screen.getByLabelText('Work email'), 'dev@company.com')
    await user.click(boxA())
    await user.click(submitBtn())
    expect(await screen.findAllByText(message)).not.toHaveLength(0)
  })

  it('links to the terms and privacy notice', async () => {
    setupFetch(COMMUNITY)
    renderPage()
    await openForm()
    expect(screen.getByRole('link', { name: 'trial terms' })).toHaveAttribute('href', 'https://voidllm.ai/trial#terms')
    expect(screen.getByRole('link', { name: 'privacy notice' })).toHaveAttribute('href', 'https://voidllm.ai/privacy')
  })
})

describe('LicensePage banners and pricing', () => {
  beforeEach(() => {
    vi.unstubAllGlobals()
    localStorage.clear()
  })

  it('shows the trial banner with days left and a pricing link', async () => {
    setupFetch({
      ...COMMUNITY,
      edition: 'enterprise',
      trial: true,
      days_left: 12,
      expires_at: '2030-01-01T00:00:00Z',
    })
    renderPage()
    expect(await screen.findByText('Enterprise trial: 12 days left')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'View pricing' })).toHaveAttribute('href', 'https://voidllm.ai/pricing')
    expect(screen.queryByText('Try Enterprise free for 30 days')).not.toBeInTheDocument()
  })

  it('shows the expired banner with restart hint', async () => {
    setupFetch({ ...COMMUNITY, trial: true, expired: true, restart_required: true })
    renderPage()
    expect(await screen.findByText('Your trial has ended. Enterprise features are off.')).toBeInTheDocument()
    expect(screen.getByText('Restart VoidLLM to finish switching off SSO, audit logs and tracing.')).toBeInTheDocument()
  })

  it('omits the restart hint when no restart is required', async () => {
    setupFetch({ ...COMMUNITY, trial: true, expired: true, restart_required: false })
    renderPage()
    await screen.findByText('Your trial has ended. Enterprise features are off.')
    expect(screen.queryByText(/Restart VoidLLM to finish/)).not.toBeInTheDocument()
  })

  it('shows the new prices and links', async () => {
    setupFetch(COMMUNITY)
    renderPage()
    expect(await screen.findByText('99 EUR/month')).toBeInTheDocument()
    expect(screen.getByText('399 EUR/month')).toBeInTheDocument()
    expect(screen.getByText('from about 1,000 EUR/month, billed annually')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Contact us' })).toHaveAttribute('href', 'mailto:hello@voidmind.io')
    expect(screen.getByRole('link', { name: 'Upgrade to Pro' })).toHaveAttribute('href', 'https://voidllm.ai/pricing')
    expect(screen.getByRole('link', { name: 'Upgrade to Enterprise' })).toHaveAttribute('href', 'https://voidllm.ai/pricing')
    expect(screen.queryByText(/\$299|\$799/)).not.toBeInTheDocument()
  })
})
