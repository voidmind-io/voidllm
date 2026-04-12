import React from 'react'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { ToastProvider } from '../hooks/useToast'
import ModelsPage from './ModelsPage'

// ---------------------------------------------------------------------------
// Types used in mocks (mirror the production types)
// ---------------------------------------------------------------------------

interface MockModelResponse {
  id: string
  name: string
  type: string
  provider: string
  base_url: string
  max_context_tokens: number
  input_price_per_1m: number
  output_price_per_1m: number
  is_active: boolean
  source: string
  aliases: string[]
  created_at: string
  updated_at: string
  timeout?: string
  fallback_model_name?: string
  deployments?: unknown[]
}

// ---------------------------------------------------------------------------
// Test fixtures
// ---------------------------------------------------------------------------

function makeChatModel(overrides: Partial<MockModelResponse> = {}): MockModelResponse {
  return {
    id: 'model-1',
    name: 'gpt-4o',
    type: 'chat',
    provider: 'openai',
    base_url: 'https://api.openai.com/v1',
    max_context_tokens: 0,
    input_price_per_1m: 0,
    output_price_per_1m: 0,
    is_active: true,
    source: 'api',
    aliases: [],
    created_at: '2024-01-01T00:00:00Z',
    updated_at: '2024-01-01T00:00:00Z',
    ...overrides,
  }
}

const MOCK_MODELS_LIST: MockModelResponse[] = [
  makeChatModel({ id: 'model-1', name: 'gpt-4o', type: 'chat' }),
  makeChatModel({ id: 'model-2', name: 'claude-sonnet', type: 'chat', provider: 'anthropic', base_url: 'https://api.anthropic.com' }),
  makeChatModel({ id: 'model-3', name: 'llama-70b', type: 'chat', provider: 'vllm', base_url: 'http://localhost:8000/v1' }),
  makeChatModel({ id: 'model-4', name: 'text-embed-ada', type: 'embedding', provider: 'openai', base_url: 'https://api.openai.com/v1' }),
]

const MOCK_LICENSE_NO_FALLBACK = {
  edition: 'community',
  valid: true,
  features: [] as string[],
  max_orgs: 1,
  max_teams: 3,
}

const MOCK_LICENSE_WITH_FALLBACK = {
  edition: 'enterprise',
  valid: true,
  features: ['fallback_chains', 'audit_logs'],
  max_orgs: -1,
  max_teams: -1,
  fallback_max_depth: 3,
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

function renderModelsPage() {
  const { Wrapper } = makeWrapper()
  return render(<ModelsPage />, { wrapper: Wrapper })
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

function defaultEntries(licensePayload = MOCK_LICENSE_NO_FALLBACK, modelsPayload = { data: MOCK_MODELS_LIST, has_more: false }): FetchMockEntry[] {
  return [
    {
      // GET-only models list (no method guard needed here — POST entries placed BEFORE
      // defaultEntries() in the entries array so they match first)
      matcher: (u) => u.includes('/api/v1/models') && !u.includes('/health'),
      method: 'GET',
      response: modelsPayload,
    },
    {
      matcher: (u) => u.includes('/api/v1/license'),
      response: licensePayload,
    },
    {
      matcher: (u) => u.includes('/api/v1/models/health'),
      response: { models: [] },
    },
  ]
}

// ---------------------------------------------------------------------------
// Dialog helpers
// ---------------------------------------------------------------------------

/** Opens the "Add Model" dialog via the page header button. */
async function openCreateDialog() {
  // The page header button is the first "Add Model" button on the page.
  const buttons = screen.getAllByRole('button', { name: /add model/i })
  await userEvent.click(buttons[0])
}

/** Returns the dialog element rendered in the portal. */
function getDialog(titleText: string | RegExp) {
  // Dialogs are portalled to document.body. Find by the dialog role or by title text.
  const heading = screen.getByRole('heading', { name: titleText })
  // Walk up to the dialog container
  const dialog = heading.closest('[role="dialog"]')
  if (!dialog) throw new Error(`Could not find dialog with title matching ${String(titleText)}`)
  return dialog as HTMLElement
}

/** Clicks the submit button inside the currently-open dialog. */
async function submitDialog(dialog: HTMLElement, buttonName: string | RegExp) {
  await userEvent.click(within(dialog).getByRole('button', { name: buttonName }))
}

/**
 * Adds a minimal deployment entry via the inline deployment form in the
 * Load Balanced tab. Required because the form validates that at least one
 * deployment is present before submitting.
 */
async function addMinimalDeployment(dialog: HTMLElement) {
  await userEvent.click(within(dialog).getByRole('button', { name: /\+ add deployment/i }))

  // After clicking "+ Add Deployment", the inline form appears with Name, Base URL, etc.
  // There are now two "Name" inputs in the dialog: the top-level model name and the
  // deployment name. Use getAllByRole and pick the last (inline form).
  const allNameInputs = within(dialog).getAllByRole('textbox', { name: /^name$/i })
  const depNameInput = allNameInputs[allNameInputs.length - 1]
  await userEvent.type(depNameInput, 'primary')

  // Similarly for Base URL
  const allUrlInputs = within(dialog).getAllByRole('textbox', { name: /base url/i })
  const depUrlInput = allUrlInputs[allUrlInputs.length - 1]
  await userEvent.type(depUrlInput, 'https://api.openai.com/v1')

  // Click "Add" to save the deployment entry
  await userEvent.click(within(dialog).getByRole('button', { name: /^add$/i }))
}

/** Switches to the Load Balanced tab within the Add Model dialog. */
async function switchToLoadBalancedTab() {
  await userEvent.click(screen.getByRole('tab', { name: /load balanced/i }))
}

/** Finds the Fallback Model combobox inside a given container element. */
function getFallbackCombobox(container: HTMLElement | Document = document): HTMLElement {
  // The Select has aria-labelledby pointing to its label id. RTL resolves this for getByRole.
  const all = within(container as HTMLElement).getAllByRole('combobox')
  // Find the one whose accessible name contains "Fallback Model"
  const match = all.find((el) => {
    const labelledBy = el.getAttribute('aria-labelledby')
    if (!labelledBy) return false
    const labelEl = document.getElementById(labelledBy)
    return labelEl?.textContent?.toLowerCase().includes('fallback model')
  })
  if (!match) throw new Error('Could not find Fallback Model combobox')
  return match
}

// ---------------------------------------------------------------------------
// Tests: CreateModelDialog — without Enterprise license
// ---------------------------------------------------------------------------

describe('CreateModelDialog — Fallback Model field', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  describe('without fallback_chains license feature', () => {
    it('disables the Fallback Model select when license does not include fallback_chains', async () => {
      setupFetchMock(defaultEntries(MOCK_LICENSE_NO_FALLBACK))
      renderModelsPage()

      await openCreateDialog()
      await switchToLoadBalancedTab()

      // Wait for the models list to load (options depend on it)
      await waitFor(() => {
        const dialog = getDialog(/add model/i)
        const select = getFallbackCombobox(dialog)
        expect(select).toBeDisabled()
      })
    })

    it('shows "Enterprise" helper text when license is missing fallback_chains', async () => {
      setupFetchMock(defaultEntries(MOCK_LICENSE_NO_FALLBACK))
      renderModelsPage()

      await openCreateDialog()
      await switchToLoadBalancedTab()

      await waitFor(() => {
        expect(screen.getByText(/available with an enterprise license/i)).toBeInTheDocument()
      })
    })

    it('does not send fallback_model_name on submit when license is missing and field is left at default', async () => {
      const capturedBodies = new Map<string, string>()
      const createdModel = makeChatModel({ id: 'new-model', name: 'my-lb-model' })
      setupFetchMock(
        [
          {
            matcher: (u) => u.includes('/api/v1/models') && !u.includes('/health'),
            method: 'POST',
            response: createdModel,
          },
          ...defaultEntries(MOCK_LICENSE_NO_FALLBACK),
        ],
        capturedBodies,
      )
      renderModelsPage()

      await openCreateDialog()
      await switchToLoadBalancedTab()

      // Fill in required field: name
      await userEvent.type(screen.getByRole('textbox', { name: /^name$/i }), 'my-lb-model')

      const dialog = getDialog(/add model/i)

      // Load balanced mode requires at least one deployment
      await addMinimalDeployment(dialog)

      // Submit
      await submitDialog(dialog, /add model/i)

      await waitFor(() => expect(capturedBodies.has('POST:/api/v1/models')).toBe(true))

      const body = JSON.parse(capturedBodies.get('POST:/api/v1/models')!)
      // When license is missing and field untouched, fallback_model_name is absent
      // because `if (fallbackModelName) params.fallback_model_name = fallbackModelName`
      // and the default value is '' (falsy).
      expect(body).not.toHaveProperty('fallback_model_name')
    })
  })

  // ---------------------------------------------------------------------------
  // CreateModelDialog — with Enterprise license
  // ---------------------------------------------------------------------------

  describe('with fallback_chains license feature', () => {
    it('enables the Fallback Model select when license includes fallback_chains', async () => {
      setupFetchMock(defaultEntries(MOCK_LICENSE_WITH_FALLBACK))
      renderModelsPage()

      await openCreateDialog()
      await switchToLoadBalancedTab()

      await waitFor(() => {
        const dialog = getDialog(/add model/i)
        const select = getFallbackCombobox(dialog)
        expect(select).not.toBeDisabled()
      })
    })

    it('shows helpful description text when license includes fallback_chains', async () => {
      setupFetchMock(defaultEntries(MOCK_LICENSE_WITH_FALLBACK))
      renderModelsPage()

      await openCreateDialog()
      await switchToLoadBalancedTab()

      await waitFor(() => {
        expect(screen.getByText(/automatically retry on the fallback model/i)).toBeInTheDocument()
      })
    })

    it('shows None option and other chat models, excludes embedding models and current model name', async () => {
      setupFetchMock(defaultEntries(MOCK_LICENSE_WITH_FALLBACK))
      renderModelsPage()

      await openCreateDialog()
      await switchToLoadBalancedTab()

      // Type a model name so the self-exclusion logic has a value to compare against
      await userEvent.type(screen.getByRole('textbox', { name: /^name$/i }), 'gpt-4o')

      // Wait for the select to be enabled
      let fallbackSelect: HTMLElement
      await waitFor(() => {
        const dialog = getDialog(/add model/i)
        fallbackSelect = getFallbackCombobox(dialog)
        expect(fallbackSelect).not.toBeDisabled()
      })

      // Open the dropdown
      await userEvent.click(fallbackSelect!)

      // "None" must be present
      expect(screen.getByRole('option', { name: 'None' })).toBeInTheDocument()

      // Other chat models must be present
      expect(screen.getByRole('option', { name: 'claude-sonnet' })).toBeInTheDocument()
      expect(screen.getByRole('option', { name: 'llama-70b' })).toBeInTheDocument()

      // Current model name (gpt-4o) must NOT appear in the options
      const allOptions = screen.getAllByRole('option')
      const optionNames = allOptions.map((o) => o.textContent)
      expect(optionNames).not.toContain('gpt-4o')

      // The embedding model must NOT appear (type mismatch; current type defaults to 'chat')
      expect(optionNames).not.toContain('text-embed-ada')
    })

    it('sends fallback_model_name when user picks a model', async () => {
      const capturedBodies = new Map<string, string>()
      const createdModel = makeChatModel({ id: 'new-lb', name: 'new-lb' })
      setupFetchMock(
        [
          {
            matcher: (u) => u.includes('/api/v1/models') && !u.includes('/health'),
            method: 'POST',
            response: createdModel,
          },
          ...defaultEntries(MOCK_LICENSE_WITH_FALLBACK),
        ],
        capturedBodies,
      )
      renderModelsPage()

      await openCreateDialog()
      await switchToLoadBalancedTab()

      await userEvent.type(screen.getByRole('textbox', { name: /^name$/i }), 'new-lb')

      // Wait for and select a fallback model
      let fallbackSelect: HTMLElement
      await waitFor(() => {
        const dialog = getDialog(/add model/i)
        fallbackSelect = getFallbackCombobox(dialog)
        expect(fallbackSelect).not.toBeDisabled()
      })

      await userEvent.click(fallbackSelect!)
      await userEvent.click(screen.getByRole('option', { name: 'claude-sonnet' }))

      const dialog = getDialog(/add model/i)

      // Load balanced mode requires at least one deployment
      await addMinimalDeployment(dialog)

      // Submit
      await submitDialog(dialog, /add model/i)

      await waitFor(() => expect(capturedBodies.has('POST:/api/v1/models')).toBe(true))

      const body = JSON.parse(capturedBodies.get('POST:/api/v1/models')!)
      expect(body.fallback_model_name).toBe('claude-sonnet')
    })

    it('does not send fallback_model_name when user leaves the default None', async () => {
      const capturedBodies = new Map<string, string>()
      const createdModel = makeChatModel({ id: 'new-lb2', name: 'new-lb2' })
      setupFetchMock(
        [
          {
            matcher: (u) => u.includes('/api/v1/models') && !u.includes('/health'),
            method: 'POST',
            response: createdModel,
          },
          ...defaultEntries(MOCK_LICENSE_WITH_FALLBACK),
        ],
        capturedBodies,
      )
      renderModelsPage()

      await openCreateDialog()
      await switchToLoadBalancedTab()

      await userEvent.type(screen.getByRole('textbox', { name: /^name$/i }), 'new-lb2')

      // Wait for the select to be ready but do NOT change it (leave as "None")
      const dialog = getDialog(/add model/i)
      await waitFor(() => {
        const select = getFallbackCombobox(dialog)
        expect(select).not.toBeDisabled()
      })

      // Load balanced mode requires at least one deployment
      await addMinimalDeployment(dialog)

      // Submit without touching the fallback select
      await submitDialog(dialog, /add model/i)

      await waitFor(() => expect(capturedBodies.has('POST:/api/v1/models')).toBe(true))

      const body = JSON.parse(capturedBodies.get('POST:/api/v1/models')!)
      // Empty string is falsy: `if (fallbackModelName)` guard omits the field
      expect(body).not.toHaveProperty('fallback_model_name')
    })
  })
})

// ---------------------------------------------------------------------------
// Tests: EditModelDialog — Fallback Model field
// ---------------------------------------------------------------------------

describe('EditModelDialog — Fallback Model field', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  /**
   * Clicks the "Edit model" button for a given model name.
   * The edit button has title="Edit model" and lives in the same table row
   * as the model name text.
   */
  async function openEditDialogForModel(modelName: string) {
    // Wait for the model name to appear in the table
    const modelNameEl = await screen.findByText(modelName)
    const row = modelNameEl.closest('tr')
    if (!row) throw new Error(`Could not find table row for model "${modelName}"`)
    const editBtn = within(row).getByTitle('Edit model')
    await userEvent.click(editBtn)
  }

  it('loads existing fallback_model_name into the Select', async () => {
    const modelWithFallback = makeChatModel({ id: 'model-1', name: 'gpt-4o', fallback_model_name: 'claude-sonnet' })
    const modelsData = {
      data: [modelWithFallback, ...MOCK_MODELS_LIST.slice(1)],
      has_more: false,
    }
    setupFetchMock(defaultEntries(MOCK_LICENSE_WITH_FALLBACK, modelsData))
    renderModelsPage()

    await openEditDialogForModel('gpt-4o')

    // The edit dialog opens — wait for it
    await waitFor(() => expect(screen.getByRole('heading', { name: /edit model/i })).toBeInTheDocument())

    const dialog = getDialog(/edit model/i)
    const fallbackSelect = getFallbackCombobox(dialog)

    // The pre-loaded value should display as 'claude-sonnet'
    expect(fallbackSelect).toHaveTextContent('claude-sonnet')
  })

  it('sends updated fallback_model_name on submit', async () => {
    const capturedBodies = new Map<string, string>()
    const modelWithFallback = makeChatModel({ id: 'model-1', name: 'gpt-4o', fallback_model_name: 'claude-sonnet' })
    const modelsData = {
      data: [modelWithFallback, ...MOCK_MODELS_LIST.slice(1)],
      has_more: false,
    }
    setupFetchMock(
      [
        ...defaultEntries(MOCK_LICENSE_WITH_FALLBACK, modelsData),
        {
          matcher: (u) => u.includes('/api/v1/models/model-1'),
          method: 'PATCH',
          response: { ...modelWithFallback, fallback_model_name: 'llama-70b' },
        },
      ],
      capturedBodies,
    )
    renderModelsPage()

    await openEditDialogForModel('gpt-4o')
    await waitFor(() => expect(screen.getByRole('heading', { name: /edit model/i })).toBeInTheDocument())

    const dialog = getDialog(/edit model/i)
    const fallbackSelect = getFallbackCombobox(dialog)

    // Change the fallback from claude-sonnet to llama-70b
    await userEvent.click(fallbackSelect)
    await userEvent.click(screen.getByRole('option', { name: 'llama-70b' }))

    await submitDialog(dialog, /save changes/i)

    await waitFor(() => expect(capturedBodies.has('PATCH:/api/v1/models/model-1')).toBe(true))

    const body = JSON.parse(capturedBodies.get('PATCH:/api/v1/models/model-1')!)
    expect(body.fallback_model_name).toBe('llama-70b')
  })

  it('sends empty string to clear fallback on submit', async () => {
    const capturedBodies = new Map<string, string>()
    const modelWithFallback = makeChatModel({ id: 'model-1', name: 'gpt-4o', fallback_model_name: 'claude-sonnet' })
    const modelsData = {
      data: [modelWithFallback, ...MOCK_MODELS_LIST.slice(1)],
      has_more: false,
    }
    setupFetchMock(
      [
        ...defaultEntries(MOCK_LICENSE_WITH_FALLBACK, modelsData),
        {
          matcher: (u) => u.includes('/api/v1/models/model-1'),
          method: 'PATCH',
          response: { ...modelWithFallback, fallback_model_name: '' },
        },
      ],
      capturedBodies,
    )
    renderModelsPage()

    await openEditDialogForModel('gpt-4o')
    await waitFor(() => expect(screen.getByRole('heading', { name: /edit model/i })).toBeInTheDocument())

    const dialog = getDialog(/edit model/i)
    const fallbackSelect = getFallbackCombobox(dialog)

    // Change to "None" (value='') to clear the existing fallback
    await userEvent.click(fallbackSelect)
    await userEvent.click(screen.getByRole('option', { name: 'None' }))

    await submitDialog(dialog, /save changes/i)

    await waitFor(() => expect(capturedBodies.has('PATCH:/api/v1/models/model-1')).toBe(true))

    const body = JSON.parse(capturedBodies.get('PATCH:/api/v1/models/model-1')!)
    // Empty string signals "clear" to the backend
    expect(body.fallback_model_name).toBe('')
  })

  it('does NOT send fallback_model_name when unchanged on submit', async () => {
    const capturedBodies = new Map<string, string>()
    const modelWithFallback = makeChatModel({
      id: 'model-1',
      name: 'gpt-4o',
      fallback_model_name: 'claude-sonnet',
      timeout: '30s',
    })
    const modelsData = {
      data: [modelWithFallback, ...MOCK_MODELS_LIST.slice(1)],
      has_more: false,
    }
    setupFetchMock(
      [
        ...defaultEntries(MOCK_LICENSE_WITH_FALLBACK, modelsData),
        {
          matcher: (u) => u.includes('/api/v1/models/model-1'),
          method: 'PATCH',
          response: { ...modelWithFallback, timeout: '60s' },
        },
      ],
      capturedBodies,
    )
    renderModelsPage()

    await openEditDialogForModel('gpt-4o')
    await waitFor(() => expect(screen.getByRole('heading', { name: /edit model/i })).toBeInTheDocument())

    const dialog = getDialog(/edit model/i)

    // Change the timeout field only — leave fallback untouched
    const timeoutInput = within(dialog).getByRole('textbox', { name: /timeout/i })
    await userEvent.clear(timeoutInput)
    await userEvent.type(timeoutInput, '60s')

    await submitDialog(dialog, /save changes/i)

    await waitFor(() => expect(capturedBodies.has('PATCH:/api/v1/models/model-1')).toBe(true))

    const body = JSON.parse(capturedBodies.get('PATCH:/api/v1/models/model-1')!)
    // fallback_model_name was not changed — must be absent from the diff
    expect(body).not.toHaveProperty('fallback_model_name')
    // The changed field must be present
    expect(body.timeout).toBe('60s')
  })
})
