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
  cached_input_price_per_1m?: number
  cache_write_price_per_1m?: number
  is_active: boolean
  source: string
  aliases: string[]
  created_at: string
  updated_at: string
  timeout?: string
  fallback_model_name?: string
  region?: string
  bedrock_model_id?: string
  bedrock_auth?: 'api_key' | 'iam'
  bedrock_role_arn?: string
  strategy?: string
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

// ---------------------------------------------------------------------------
// Tests: AWS Bedrock provider
// ---------------------------------------------------------------------------

function getComboboxByLabel(container: HTMLElement, label: string): HTMLElement {
  const match = within(container).getAllByRole('combobox').find((el) => {
    const labelledBy = el.getAttribute('aria-labelledby')
    if (!labelledBy) return false
    return document.getElementById(labelledBy)?.textContent?.toLowerCase().includes(label.toLowerCase())
  })
  if (!match) throw new Error(`Could not find combobox "${label}"`)
  return match
}

async function pickBedrockProvider(dialog: HTMLElement) {
  await userEvent.click(getComboboxByLabel(dialog, 'provider'))
  await userEvent.click(screen.getByRole('option', { name: 'AWS Bedrock' }))
}

describe('AWS Bedrock provider', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  async function openEditDialogForModel(modelName: string) {
    const modelNameEl = await screen.findByText(modelName)
    const row = modelNameEl.closest('tr')
    if (!row) throw new Error(`Could not find table row for model "${modelName}"`)
    await userEvent.click(within(row).getByTitle('Edit model'))
  }

  it('shows region and model ID fields only for bedrock and marks base URL optional', async () => {
    setupFetchMock(defaultEntries())
    renderModelsPage()

    await openCreateDialog()
    const dialog = getDialog(/add model/i)
    expect(within(dialog).queryByRole('textbox', { name: /aws region/i })).not.toBeInTheDocument()
    expect(within(dialog).queryByRole('textbox', { name: /bedrock model id/i })).not.toBeInTheDocument()

    await pickBedrockProvider(dialog)

    expect(within(dialog).getByRole('textbox', { name: /aws region/i })).toHaveAttribute('placeholder', 'eu-central-1')
    expect(within(dialog).getByRole('textbox', { name: /bedrock model id/i })).toHaveAttribute(
      'placeholder',
      'eu.anthropic.claude-sonnet-4-5-20250929-v1:0',
    )
    expect(within(dialog).getByRole('textbox', { name: /base url/i })).toHaveAttribute(
      'placeholder',
      'Optional - defaults to https://bedrock-runtime.<region>.amazonaws.com',
    )
  })

  it('creates a bedrock model without base_url and with region and bedrock_model_id', async () => {
    const capturedBodies = new Map<string, string>()
    setupFetchMock(
      [
        {
          matcher: (u) => u.includes('/api/v1/models') && !u.includes('/health'),
          method: 'POST',
          response: makeChatModel({ id: 'new-br', name: 'claude-br', provider: 'bedrock' }),
        },
        ...defaultEntries(),
      ],
      capturedBodies,
    )
    renderModelsPage()

    await openCreateDialog()
    const dialog = getDialog(/add model/i)
    await userEvent.type(within(dialog).getByRole('textbox', { name: /^name$/i }), 'claude-br')
    await pickBedrockProvider(dialog)
    await userEvent.type(within(dialog).getByLabelText(/^api key$/i), 'abwk-123')
    await userEvent.type(within(dialog).getByRole('textbox', { name: /aws region/i }), 'eu-central-1')
    await userEvent.type(
      within(dialog).getByRole('textbox', { name: /bedrock model id/i }),
      'eu.anthropic.claude-sonnet-4-5-20250929-v1:0',
    )
    await submitDialog(dialog, /add model/i)

    await waitFor(() => expect(capturedBodies.has('POST:/api/v1/models')).toBe(true))
    const body = JSON.parse(capturedBodies.get('POST:/api/v1/models')!)
    expect(body.provider).toBe('bedrock')
    expect(body.region).toBe('eu-central-1')
    expect(body.bedrock_model_id).toBe('eu.anthropic.claude-sonnet-4-5-20250929-v1:0')
    expect(body).not.toHaveProperty('base_url')
  })

  it('requires region and model ID for bedrock and does not submit', async () => {
    const capturedBodies = new Map<string, string>()
    setupFetchMock(
      [
        {
          matcher: (u) => u.includes('/api/v1/models') && !u.includes('/health'),
          method: 'POST',
          response: makeChatModel(),
        },
        ...defaultEntries(),
      ],
      capturedBodies,
    )
    renderModelsPage()

    await openCreateDialog()
    const dialog = getDialog(/add model/i)
    await userEvent.type(within(dialog).getByRole('textbox', { name: /^name$/i }), 'claude-br')
    await pickBedrockProvider(dialog)
    await submitDialog(dialog, /add model/i)

    expect(await within(dialog).findByText('AWS region is required')).toBeInTheDocument()
    expect(within(dialog).getByText('Bedrock model ID is required')).toBeInTheDocument()
    expect(within(dialog).queryByText('Base URL is required')).not.toBeInTheDocument()
    expect(capturedBodies.has('POST:/api/v1/models')).toBe(false)
  })

  it('rejects non-chat model types for bedrock', async () => {
    const capturedBodies = new Map<string, string>()
    setupFetchMock(
      [
        {
          matcher: (u) => u.includes('/api/v1/models') && !u.includes('/health'),
          method: 'POST',
          response: makeChatModel(),
        },
        ...defaultEntries(),
      ],
      capturedBodies,
    )
    renderModelsPage()

    await openCreateDialog()
    const dialog = getDialog(/add model/i)
    await userEvent.type(within(dialog).getByRole('textbox', { name: /^name$/i }), 'embed-br')
    await userEvent.click(getComboboxByLabel(dialog, 'type'))
    await userEvent.click(screen.getByRole('option', { name: /embedding/i }))
    await pickBedrockProvider(dialog)
    await userEvent.type(within(dialog).getByRole('textbox', { name: /aws region/i }), 'eu-central-1')
    await userEvent.type(within(dialog).getByRole('textbox', { name: /bedrock model id/i }), 'amazon.titan')
    await submitDialog(dialog, /add model/i)

    expect(await within(dialog).findByText('Bedrock supports chat models only')).toBeInTheDocument()
    expect(capturedBodies.has('POST:/api/v1/models')).toBe(false)
  })

  it('sends region and bedrock_model_id in the test-connection request', async () => {
    const capturedBodies = new Map<string, string>()
    setupFetchMock(
      [
        {
          matcher: (u) => u.includes('/api/v1/models/test-connection'),
          method: 'POST',
          response: { success: true, message: 'ok' },
        },
        ...defaultEntries(),
      ],
      capturedBodies,
    )
    renderModelsPage()

    await openCreateDialog()
    const dialog = getDialog(/add model/i)
    await pickBedrockProvider(dialog)
    await userEvent.type(within(dialog).getByRole('textbox', { name: /aws region/i }), 'us-east-1')
    await userEvent.type(within(dialog).getByRole('textbox', { name: /bedrock model id/i }), 'amazon.nova-lite-v1:0')
    await userEvent.click(within(dialog).getByRole('button', { name: /test connection/i }))

    await waitFor(() => expect(capturedBodies.has('POST:/api/v1/models/test-connection')).toBe(true))
    const body = JSON.parse(capturedBodies.get('POST:/api/v1/models/test-connection')!)
    expect(body.provider).toBe('bedrock')
    expect(body.region).toBe('us-east-1')
    expect(body.bedrock_model_id).toBe('amazon.nova-lite-v1:0')
  })

  it('edit dialog prefills bedrock fields and sends changed values', async () => {
    const capturedBodies = new Map<string, string>()
    const bedrockModel = makeChatModel({
      id: 'model-br',
      name: 'claude-br',
      provider: 'bedrock',
      base_url: '',
      region: 'eu-central-1',
      bedrock_model_id: 'eu.anthropic.claude-sonnet-4-5-20250929-v1:0',
    })
    setupFetchMock(
      [
        ...defaultEntries(MOCK_LICENSE_NO_FALLBACK, { data: [bedrockModel], has_more: false }),
        { matcher: (u) => u.includes('/api/v1/models/model-br'), method: 'PATCH', response: bedrockModel },
      ],
      capturedBodies,
    )
    renderModelsPage()

    await openEditDialogForModel('claude-br')
    const dialog = getDialog(/edit model/i)
    const regionInput = within(dialog).getByRole('textbox', { name: /aws region/i })
    expect(regionInput).toHaveValue('eu-central-1')
    expect(within(dialog).getByRole('textbox', { name: /bedrock model id/i })).toHaveValue(
      'eu.anthropic.claude-sonnet-4-5-20250929-v1:0',
    )

    await userEvent.clear(regionInput)
    await userEvent.type(regionInput, 'us-east-1')
    await submitDialog(dialog, /save changes/i)

    await waitFor(() => expect(capturedBodies.has('PATCH:/api/v1/models/model-br')).toBe(true))
    const body = JSON.parse(capturedBodies.get('PATCH:/api/v1/models/model-br')!)
    expect(body).toEqual({ region: 'us-east-1' })
  })

  it('"Use regional endpoint" clears the base URL and sends base_url as empty string', async () => {
    const capturedBodies = new Map<string, string>()
    const bedrockModel = makeChatModel({
      id: 'model-br',
      name: 'claude-br',
      provider: 'bedrock',
      base_url: 'https://vpce-1.bedrock-runtime.eu-central-1.vpce.amazonaws.com',
      region: 'eu-central-1',
      bedrock_model_id: 'eu.anthropic.claude-sonnet-4-5-20250929-v1:0',
    })
    setupFetchMock(
      [
        ...defaultEntries(MOCK_LICENSE_NO_FALLBACK, { data: [bedrockModel], has_more: false }),
        { matcher: (u) => u.includes('/api/v1/models/model-br'), method: 'PATCH', response: bedrockModel },
      ],
      capturedBodies,
    )
    renderModelsPage()

    await openEditDialogForModel('claude-br')
    const dialog = getDialog(/edit model/i)
    const baseUrlInput = within(dialog).getByRole('textbox', { name: /base url/i })
    expect(baseUrlInput).toHaveValue('https://vpce-1.bedrock-runtime.eu-central-1.vpce.amazonaws.com')

    await userEvent.click(within(dialog).getByRole('button', { name: /use regional endpoint/i }))
    expect(baseUrlInput).toHaveValue('')
    await submitDialog(dialog, /save changes/i)

    await waitFor(() => expect(capturedBodies.has('PATCH:/api/v1/models/model-br')).toBe(true))
    const body = JSON.parse(capturedBodies.get('PATCH:/api/v1/models/model-br')!)
    expect(body.base_url).toBe('')
  })
})

// ---------------------------------------------------------------------------
// Tests: AWS Bedrock IAM authentication
// ---------------------------------------------------------------------------

const VALID_ROLE_ARN = 'arn:aws:iam::123456789012:role/voidllm-bedrock'
const IAM_LABEL = 'IAM role (AWS credential chain)'
const ARN_ERROR = 'Role ARN must look like arn:aws:iam::123456789012:role/name'
const MODELS_POST = 'POST:/api/v1/models'
const API_KEY_ERROR = 'API key is required when authentication is set to API key'

async function pickAuth(container: HTMLElement, optionName: string) {
  await userEvent.click(getComboboxByLabel(container, 'authentication'))
  await userEvent.click(screen.getByRole('option', { name: optionName }))
}

function queryApiKey(container: HTMLElement) {
  return within(container).queryByLabelText(/^api key$/i)
}

function getRoleArnInput(container: HTMLElement) {
  return within(container).getByRole('textbox', { name: /role arn/i })
}

async function fillBedrockBasics(dialog: HTMLElement) {
  await userEvent.type(within(dialog).getByRole('textbox', { name: /aws region/i }), 'eu-central-1')
  await userEvent.type(within(dialog).getByRole('textbox', { name: /bedrock model id/i }), 'amazon.nova-lite-v1:0')
}

function postModelEntries(extra: FetchMockEntry[] = []): FetchMockEntry[] {
  return [
    ...extra,
    {
      matcher: (u) => u.includes('/api/v1/models') && !u.includes('/health'),
      method: 'POST',
      response: makeChatModel({ id: 'new-br', name: 'claude-br', provider: 'bedrock' }),
    },
    ...defaultEntries(),
  ]
}

/** Waits one animation frame so the Dialog's deferred autofocus cannot interrupt typing. */
function dialogFocused() {
  return new Promise<void>((resolve) => {
    requestAnimationFrame(() => resolve())
  })
}

/**
 * Renders the page and waits until the initial queries (models and license) have
 * resolved, so later re-renders cannot steal focus from inputs mid-typing.
 */
async function renderSettled(waitForText?: string) {
  const view = renderModelsPage()
  await waitFor(() => {
    const calls = (fetch as unknown as { mock: { calls: unknown[][] } }).mock.calls.map((c) => String(c[0]))
    expect(calls.some((u) => u.includes('/api/v1/license'))).toBe(true)
  })
  await screen.findByText(waitForText ?? 'gpt-4o')
  await new Promise((resolve) => globalThis.setTimeout(resolve, 50))
  return view
}

describe('AWS Bedrock IAM authentication', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  async function openEditDialogForModel(modelName: string) {
    const modelNameEl = await screen.findByText(modelName)
    const row = modelNameEl.closest('tr')
    if (!row) throw new Error(`Could not find table row for model "${modelName}"`)
    await userEvent.click(within(row).getByTitle('Edit model'))
  }

  describe('create dialog (single endpoint)', () => {
    it('defaults the Authentication select to API key and shows the API key input', async () => {
      setupFetchMock(defaultEntries())
      await renderSettled()

      await openCreateDialog()
      const dialog = getDialog(/add model/i)
      await dialogFocused()
      await pickBedrockProvider(dialog)

      expect(getComboboxByLabel(dialog, 'authentication')).toHaveTextContent('API key')
      expect(queryApiKey(dialog)).toBeInTheDocument()
      expect(within(dialog).queryByRole('textbox', { name: /role arn/i })).not.toBeInTheDocument()
    })

    it('switching to IAM hides the API key input and shows the Role ARN field with hint', async () => {
      setupFetchMock(defaultEntries())
      await renderSettled()

      await openCreateDialog()
      const dialog = getDialog(/add model/i)
      await dialogFocused()
      await pickBedrockProvider(dialog)
      await pickAuth(dialog, IAM_LABEL)

      expect(queryApiKey(dialog)).not.toBeInTheDocument()
      expect(within(dialog).getByRole('textbox', { name: 'Role ARN (optional)' })).toBeInTheDocument()
      expect(
        within(dialog).getByText(
          'Leave empty to use the credentials of the VoidLLM process: environment, profile, IRSA or instance role.',
        ),
      ).toBeInTheDocument()

      await pickAuth(dialog, 'API key')
      expect(queryApiKey(dialog)).toBeInTheDocument()
      expect(within(dialog).queryByRole('textbox', { name: /role arn/i })).not.toBeInTheDocument()
    })

    it('shows an inline error for an invalid role ARN and blocks submit', async () => {
      const capturedBodies = new Map<string, string>()
      setupFetchMock(postModelEntries(), capturedBodies)
      await renderSettled()

      await openCreateDialog()
      const dialog = getDialog(/add model/i)
      await dialogFocused()
      await userEvent.type(within(dialog).getByRole('textbox', { name: /^name$/i }), 'claude-br')
      await pickBedrockProvider(dialog)
      await fillBedrockBasics(dialog)
      await pickAuth(dialog, IAM_LABEL)
      await userEvent.type(getRoleArnInput(dialog), 'not-an-arn')
      await submitDialog(dialog, /add model/i)

      expect(await within(dialog).findByText(ARN_ERROR)).toBeInTheDocument()
      expect(capturedBodies.has(MODELS_POST)).toBe(false)
    })

    it('does not validate the role ARN when the mode is API key', async () => {
      const capturedBodies = new Map<string, string>()
      setupFetchMock(postModelEntries(), capturedBodies)
      await renderSettled()

      await openCreateDialog()
      const dialog = getDialog(/add model/i)
      await dialogFocused()
      await userEvent.type(within(dialog).getByRole('textbox', { name: /^name$/i }), 'claude-br')
      await pickBedrockProvider(dialog)
      await fillBedrockBasics(dialog)
      await pickAuth(dialog, IAM_LABEL)
      await userEvent.type(getRoleArnInput(dialog), 'not-an-arn')
      await pickAuth(dialog, 'API key')
      await userEvent.type(within(dialog).getByLabelText(/^api key$/i), 'abwk-123')
      await submitDialog(dialog, /add model/i)

      await waitFor(() => expect(capturedBodies.has(MODELS_POST)).toBe(true))
      const body = JSON.parse(capturedBodies.get(MODELS_POST)!)
      expect(body.bedrock_auth).toBe('api_key')
      expect(body).not.toHaveProperty('bedrock_role_arn')
    })

    it('blocks a keyless api_key bedrock model with an inline error and sends no request', async () => {
      const capturedBodies = new Map<string, string>()
      setupFetchMock(postModelEntries(), capturedBodies)
      await renderSettled()

      await openCreateDialog()
      const dialog = getDialog(/add model/i)
      await dialogFocused()
      await userEvent.type(within(dialog).getByRole('textbox', { name: /^name$/i }), 'claude-br')
      await pickBedrockProvider(dialog)
      await fillBedrockBasics(dialog)
      await submitDialog(dialog, /add model/i)

      expect(await within(dialog).findByText(API_KEY_ERROR)).toBeInTheDocument()
      expect(capturedBodies.has(MODELS_POST)).toBe(false)

      await userEvent.type(within(dialog).getByLabelText(/^api key$/i), 'abwk-123')
      await submitDialog(dialog, /add model/i)
      await waitFor(() => expect(capturedBodies.has(MODELS_POST)).toBe(true))
    })

    it('creates in IAM mode with a trimmed valid role ARN and no api_key', async () => {
      const capturedBodies = new Map<string, string>()
      setupFetchMock(postModelEntries(), capturedBodies)
      await renderSettled()

      await openCreateDialog()
      const dialog = getDialog(/add model/i)
      await dialogFocused()
      await userEvent.type(within(dialog).getByRole('textbox', { name: /^name$/i }), 'claude-br')
      await pickBedrockProvider(dialog)
      await userEvent.type(within(dialog).getByLabelText(/^api key$/i), 'secret-typed-before-switch')
      await fillBedrockBasics(dialog)
      await pickAuth(dialog, IAM_LABEL)
      await userEvent.type(getRoleArnInput(dialog), `  ${VALID_ROLE_ARN}  `)
      await submitDialog(dialog, /add model/i)

      await waitFor(() => expect(capturedBodies.has(MODELS_POST)).toBe(true))
      const body = JSON.parse(capturedBodies.get(MODELS_POST)!)
      expect(body.bedrock_auth).toBe('iam')
      expect(body.bedrock_role_arn).toBe(VALID_ROLE_ARN)
      expect(body).not.toHaveProperty('api_key')
    })

    it('creates in IAM mode with an empty role ARN and omits bedrock_role_arn', async () => {
      const capturedBodies = new Map<string, string>()
      setupFetchMock(postModelEntries(), capturedBodies)
      await renderSettled()

      await openCreateDialog()
      const dialog = getDialog(/add model/i)
      await dialogFocused()
      await userEvent.type(within(dialog).getByRole('textbox', { name: /^name$/i }), 'claude-br')
      await pickBedrockProvider(dialog)
      await fillBedrockBasics(dialog)
      await pickAuth(dialog, IAM_LABEL)
      await submitDialog(dialog, /add model/i)

      await waitFor(() => expect(capturedBodies.has(MODELS_POST)).toBe(true))
      const body = JSON.parse(capturedBodies.get(MODELS_POST)!)
      expect(body.bedrock_auth).toBe('iam')
      expect(body).not.toHaveProperty('bedrock_role_arn')
      expect(body).not.toHaveProperty('api_key')
    })

    it('accepts other valid ARN partitions such as aws-us-gov', async () => {
      const capturedBodies = new Map<string, string>()
      setupFetchMock(postModelEntries(), capturedBodies)
      await renderSettled()

      await openCreateDialog()
      const dialog = getDialog(/add model/i)
      await dialogFocused()
      await userEvent.type(within(dialog).getByRole('textbox', { name: /^name$/i }), 'claude-br')
      await pickBedrockProvider(dialog)
      await fillBedrockBasics(dialog)
      await pickAuth(dialog, IAM_LABEL)
      await userEvent.type(getRoleArnInput(dialog), 'arn:aws-us-gov:iam::123456789012:role/path/team-role')
      await submitDialog(dialog, /add model/i)

      await waitFor(() => expect(capturedBodies.has(MODELS_POST)).toBe(true))
      expect(JSON.parse(capturedBodies.get(MODELS_POST)!).bedrock_role_arn).toBe(
        'arn:aws-us-gov:iam::123456789012:role/path/team-role',
      )
    })

    it('default api_key mode sends bedrock_auth api_key and the typed api_key', async () => {
      const capturedBodies = new Map<string, string>()
      setupFetchMock(postModelEntries(), capturedBodies)
      await renderSettled()

      await openCreateDialog()
      const dialog = getDialog(/add model/i)
      await dialogFocused()
      await userEvent.type(within(dialog).getByRole('textbox', { name: /^name$/i }), 'claude-br')
      await pickBedrockProvider(dialog)
      await userEvent.type(within(dialog).getByLabelText(/^api key$/i), 'abwk-123')
      await fillBedrockBasics(dialog)
      await submitDialog(dialog, /add model/i)

      await waitFor(() => expect(capturedBodies.has(MODELS_POST)).toBe(true))
      const body = JSON.parse(capturedBodies.get(MODELS_POST)!)
      expect(body.bedrock_auth).toBe('api_key')
      expect(body.api_key).toBe('abwk-123')
      expect(body).not.toHaveProperty('bedrock_role_arn')
    })

    it('Test Connection in IAM mode sends bedrock_auth and role ARN but no api_key', async () => {
      const capturedBodies = new Map<string, string>()
      setupFetchMock(
        [
          {
            matcher: (u) => u.includes('/api/v1/models/test-connection'),
            method: 'POST',
            response: { success: true, message: 'ok' },
          },
          ...defaultEntries(),
        ],
        capturedBodies,
      )
      await renderSettled()

      await openCreateDialog()
      const dialog = getDialog(/add model/i)
      await dialogFocused()
      await pickBedrockProvider(dialog)
      await userEvent.type(within(dialog).getByLabelText(/^api key$/i), 'should-not-be-sent')
      await fillBedrockBasics(dialog)
      await pickAuth(dialog, IAM_LABEL)
      await userEvent.type(getRoleArnInput(dialog), VALID_ROLE_ARN)
      await userEvent.click(within(dialog).getByRole('button', { name: /test connection/i }))

      const key = 'POST:/api/v1/models/test-connection'
      await waitFor(() => expect(capturedBodies.has(key)).toBe(true))
      const body = JSON.parse(capturedBodies.get(key)!)
      expect(body.provider).toBe('bedrock')
      expect(body.bedrock_auth).toBe('iam')
      expect(body.bedrock_role_arn).toBe(VALID_ROLE_ARN)
      expect(body).not.toHaveProperty('api_key')
    })

    it('Test Connection in default mode sends bedrock_auth api_key', async () => {
      const capturedBodies = new Map<string, string>()
      setupFetchMock(
        [
          {
            matcher: (u) => u.includes('/api/v1/models/test-connection'),
            method: 'POST',
            response: { success: true, message: 'ok' },
          },
          ...defaultEntries(),
        ],
        capturedBodies,
      )
      await renderSettled()

      await openCreateDialog()
      const dialog = getDialog(/add model/i)
      await dialogFocused()
      await pickBedrockProvider(dialog)
      await fillBedrockBasics(dialog)
      await userEvent.click(within(dialog).getByRole('button', { name: /test connection/i }))

      const key = 'POST:/api/v1/models/test-connection'
      await waitFor(() => expect(capturedBodies.has(key)).toBe(true))
      const body = JSON.parse(capturedBodies.get(key)!)
      expect(body.bedrock_auth).toBe('api_key')
      expect(body).not.toHaveProperty('bedrock_role_arn')
    })
  })

  describe('non-bedrock providers', () => {
    it('shows no Authentication select or Role ARN field and keeps the API key input', async () => {
      setupFetchMock(defaultEntries())
      await renderSettled()

      await openCreateDialog()
      const dialog = getDialog(/add model/i)
      await dialogFocused()

      expect(within(dialog).queryByText('Authentication')).not.toBeInTheDocument()
      expect(within(dialog).queryByRole('textbox', { name: /role arn/i })).not.toBeInTheDocument()
      expect(queryApiKey(dialog)).toBeInTheDocument()
    })

    it('create payload contains no bedrock fields', async () => {
      const capturedBodies = new Map<string, string>()
      setupFetchMock(postModelEntries(), capturedBodies)
      await renderSettled()

      await openCreateDialog()
      const dialog = getDialog(/add model/i)
      await dialogFocused()
      await userEvent.type(within(dialog).getByRole('textbox', { name: /^name$/i }), 'gpt-x')
      await userEvent.type(within(dialog).getByRole('textbox', { name: /base url/i }), 'https://api.openai.com/v1')
      await userEvent.type(within(dialog).getByLabelText(/^api key$/i), 'sk-test')
      await submitDialog(dialog, /add model/i)

      await waitFor(() => expect(capturedBodies.has(MODELS_POST)).toBe(true))
      const body = JSON.parse(capturedBodies.get(MODELS_POST)!)
      expect(body.api_key).toBe('sk-test')
      expect(body).not.toHaveProperty('bedrock_auth')
      expect(body).not.toHaveProperty('bedrock_role_arn')
    })

    it('Test Connection payload contains no bedrock fields', async () => {
      const capturedBodies = new Map<string, string>()
      setupFetchMock(
        [
          {
            matcher: (u) => u.includes('/api/v1/models/test-connection'),
            method: 'POST',
            response: { success: true, message: 'ok' },
          },
          ...defaultEntries(),
        ],
        capturedBodies,
      )
      await renderSettled()

      await openCreateDialog()
      const dialog = getDialog(/add model/i)
      await dialogFocused()
      await userEvent.type(within(dialog).getByRole('textbox', { name: /base url/i }), 'https://api.openai.com/v1')
      await userEvent.type(within(dialog).getByLabelText(/^api key$/i), 'sk-test')
      await userEvent.click(within(dialog).getByRole('button', { name: /test connection/i }))

      const key = 'POST:/api/v1/models/test-connection'
      await waitFor(() => expect(capturedBodies.has(key)).toBe(true))
      const body = JSON.parse(capturedBodies.get(key)!)
      expect(body.api_key).toBe('sk-test')
      expect(body).not.toHaveProperty('bedrock_auth')
      expect(body).not.toHaveProperty('bedrock_role_arn')
    })

    it('edit payload for a non-bedrock model has no bedrock fields', async () => {
      const capturedBodies = new Map<string, string>()
      const model = makeChatModel({ id: 'model-oa', name: 'gpt-oa' })
      setupFetchMock(
        [
          ...defaultEntries(MOCK_LICENSE_NO_FALLBACK, { data: [model], has_more: false }),
          { matcher: (u) => u.includes('/api/v1/models/model-oa'), method: 'PATCH', response: model },
        ],
        capturedBodies,
      )
      await renderSettled('gpt-oa')

      await openEditDialogForModel('gpt-oa')
      const dialog = getDialog(/edit model/i)
      await dialogFocused()
      expect(within(dialog).queryByText('Authentication')).not.toBeInTheDocument()
      await userEvent.type(within(dialog).getByLabelText(/^api key$/i), 'sk-new')
      await submitDialog(dialog, /save changes/i)

      await waitFor(() => expect(capturedBodies.has('PATCH:/api/v1/models/model-oa')).toBe(true))
      expect(JSON.parse(capturedBodies.get('PATCH:/api/v1/models/model-oa')!)).toEqual({ api_key: 'sk-new' })
    })
  })

  describe('edit dialog', () => {
    const bedrockBase = {
      id: 'model-br',
      name: 'claude-br',
      provider: 'bedrock',
      base_url: '',
      region: 'eu-central-1',
      bedrock_model_id: 'amazon.nova-lite-v1:0',
    }

    function patchSetup(model: MockModelResponse, capturedBodies: Map<string, string>) {
      setupFetchMock(
        [
          ...defaultEntries(MOCK_LICENSE_NO_FALLBACK, { data: [model], has_more: false }),
          { matcher: (u) => u.includes('/api/v1/models/model-br'), method: 'PATCH', response: model },
        ],
        capturedBodies,
      )
    }

    it('switching an api_key model to IAM sends api_key "" and bedrock_auth iam', async () => {
      const capturedBodies = new Map<string, string>()
      patchSetup(makeChatModel({ ...bedrockBase, bedrock_auth: 'api_key' }), capturedBodies)
      await renderSettled('claude-br')

      await openEditDialogForModel('claude-br')
      const dialog = getDialog(/edit model/i)
      await dialogFocused()
      expect(getComboboxByLabel(dialog, 'authentication')).toHaveTextContent('API key')
      await pickAuth(dialog, IAM_LABEL)
      expect(queryApiKey(dialog)).not.toBeInTheDocument()
      await submitDialog(dialog, /save changes/i)

      await waitFor(() => expect(capturedBodies.has('PATCH:/api/v1/models/model-br')).toBe(true))
      expect(JSON.parse(capturedBodies.get('PATCH:/api/v1/models/model-br')!)).toEqual({
        api_key: '',
        bedrock_auth: 'iam',
      })
    })

    it('treats a bedrock model without bedrock_auth as api_key', async () => {
      const capturedBodies = new Map<string, string>()
      patchSetup(makeChatModel(bedrockBase), capturedBodies)
      await renderSettled('claude-br')

      await openEditDialogForModel('claude-br')
      const dialog = getDialog(/edit model/i)
      await dialogFocused()
      expect(getComboboxByLabel(dialog, 'authentication')).toHaveTextContent('API key')
      await pickAuth(dialog, IAM_LABEL)
      await userEvent.type(getRoleArnInput(dialog), VALID_ROLE_ARN)
      await submitDialog(dialog, /save changes/i)

      await waitFor(() => expect(capturedBodies.has('PATCH:/api/v1/models/model-br')).toBe(true))
      expect(JSON.parse(capturedBodies.get('PATCH:/api/v1/models/model-br')!)).toEqual({
        api_key: '',
        bedrock_auth: 'iam',
        bedrock_role_arn: VALID_ROLE_ARN,
      })
    })

    it('prefills an IAM model and sends no request when unchanged', async () => {
      const capturedBodies = new Map<string, string>()
      patchSetup(
        makeChatModel({ ...bedrockBase, bedrock_auth: 'iam', bedrock_role_arn: VALID_ROLE_ARN }),
        capturedBodies,
      )
      await renderSettled('claude-br')

      await openEditDialogForModel('claude-br')
      const dialog = getDialog(/edit model/i)
      await dialogFocused()
      expect(getComboboxByLabel(dialog, 'authentication')).toHaveTextContent(IAM_LABEL)
      expect(getRoleArnInput(dialog)).toHaveValue(VALID_ROLE_ARN)
      expect(queryApiKey(dialog)).not.toBeInTheDocument()
      await submitDialog(dialog, /save changes/i)

      await waitFor(() => expect(screen.queryByRole('heading', { name: /edit model/i })).not.toBeInTheDocument())
      expect(capturedBodies.has('PATCH:/api/v1/models/model-br')).toBe(false)
    })

    it('switching an IAM model back to api_key sends bedrock_auth api_key and empty role ARN', async () => {
      const capturedBodies = new Map<string, string>()
      patchSetup(
        makeChatModel({ ...bedrockBase, bedrock_auth: 'iam', bedrock_role_arn: VALID_ROLE_ARN }),
        capturedBodies,
      )
      await renderSettled('claude-br')

      await openEditDialogForModel('claude-br')
      const dialog = getDialog(/edit model/i)
      await dialogFocused()
      await pickAuth(dialog, 'API key')
      await userEvent.type(within(dialog).getByLabelText(/^api key$/i), 'new-key')
      await submitDialog(dialog, /save changes/i)

      await waitFor(() => expect(capturedBodies.has('PATCH:/api/v1/models/model-br')).toBe(true))
      expect(JSON.parse(capturedBodies.get('PATCH:/api/v1/models/model-br')!)).toEqual({
        api_key: 'new-key',
        bedrock_auth: 'api_key',
        bedrock_role_arn: '',
      })
    })

    it('requires a new api key when switching an IAM model back to api_key', async () => {
      const capturedBodies = new Map<string, string>()
      patchSetup(makeChatModel({ ...bedrockBase, bedrock_auth: 'iam' }), capturedBodies)
      await renderSettled('claude-br')

      await openEditDialogForModel('claude-br')
      const dialog = getDialog(/edit model/i)
      await dialogFocused()
      await pickAuth(dialog, 'API key')
      await submitDialog(dialog, /save changes/i)

      expect(await within(dialog).findByText(API_KEY_ERROR)).toBeInTheDocument()
      expect(capturedBodies.has('PATCH:/api/v1/models/model-br')).toBe(false)
    })

    it('does not require an api key when editing an existing api_key model', async () => {
      const capturedBodies = new Map<string, string>()
      patchSetup(makeChatModel({ ...bedrockBase, bedrock_auth: 'api_key' }), capturedBodies)
      await renderSettled('claude-br')

      await openEditDialogForModel('claude-br')
      const dialog = getDialog(/edit model/i)
      await dialogFocused()
      const region = within(dialog).getByRole('textbox', { name: /aws region/i })
      await userEvent.clear(region)
      await userEvent.type(region, 'us-east-1')
      await submitDialog(dialog, /save changes/i)

      await waitFor(() => expect(capturedBodies.has('PATCH:/api/v1/models/model-br')).toBe(true))
      expect(within(dialog).queryByText(API_KEY_ERROR)).not.toBeInTheDocument()
    })

    it('clearing the role ARN of an IAM model sends bedrock_role_arn ""', async () => {
      const capturedBodies = new Map<string, string>()
      patchSetup(
        makeChatModel({ ...bedrockBase, bedrock_auth: 'iam', bedrock_role_arn: VALID_ROLE_ARN }),
        capturedBodies,
      )
      await renderSettled('claude-br')

      await openEditDialogForModel('claude-br')
      const dialog = getDialog(/edit model/i)
      await dialogFocused()
      await userEvent.clear(getRoleArnInput(dialog))
      await submitDialog(dialog, /save changes/i)

      await waitFor(() => expect(capturedBodies.has('PATCH:/api/v1/models/model-br')).toBe(true))
      expect(JSON.parse(capturedBodies.get('PATCH:/api/v1/models/model-br')!)).toEqual({ bedrock_role_arn: '' })
    })

    it('blocks save with an invalid role ARN', async () => {
      const capturedBodies = new Map<string, string>()
      patchSetup(makeChatModel({ ...bedrockBase, bedrock_auth: 'iam' }), capturedBodies)
      await renderSettled('claude-br')

      await openEditDialogForModel('claude-br')
      const dialog = getDialog(/edit model/i)
      await dialogFocused()
      await userEvent.type(getRoleArnInput(dialog), 'arn:aws:iam::123:role/short-account')
      await submitDialog(dialog, /save changes/i)

      expect(await within(dialog).findByText(ARN_ERROR)).toBeInTheDocument()
      expect(capturedBodies.has('PATCH:/api/v1/models/model-br')).toBe(false)
    })
  })

  describe('inline load-balanced deployment form', () => {
    const DEPLOYMENTS_POST = 'POST:/api/v1/models/new-br/deployments'

    function lbEntries(): FetchMockEntry[] {
      return [
        {
          matcher: (u) => u.includes('/api/v1/models/new-br/deployments'),
          method: 'POST',
          response: { id: 'dep-1', name: 'br-1', provider: 'bedrock' },
        },
        ...postModelEntries(),
      ]
    }

    async function openInlineBedrockForm(dialog: HTMLElement) {
      await userEvent.type(within(dialog).getByRole('textbox', { name: /^name$/i }), 'lb-br')
      await switchToLoadBalancedTab()
      await userEvent.click(within(dialog).getByRole('button', { name: /\+ add deployment/i }))
      await pickBedrockProvider(dialog)
      const names = within(dialog).getAllByRole('textbox', { name: /^name$/i })
      await userEvent.type(names[names.length - 1], 'br-1')
      await fillBedrockBasics(dialog)
    }

    it('defaults to API key and toggles the API key and Role ARN fields', async () => {
      setupFetchMock(lbEntries())
      await renderSettled()

      await openCreateDialog()
      const dialog = getDialog(/add model/i)
      await dialogFocused()
      await openInlineBedrockForm(dialog)

      expect(getComboboxByLabel(dialog, 'authentication')).toHaveTextContent('API key')
      expect(queryApiKey(dialog)).toBeInTheDocument()

      await pickAuth(dialog, IAM_LABEL)
      expect(queryApiKey(dialog)).not.toBeInTheDocument()
      expect(getRoleArnInput(dialog)).toBeInTheDocument()
    })

    it('blocks adding a deployment with an invalid role ARN', async () => {
      setupFetchMock(lbEntries())
      await renderSettled()

      await openCreateDialog()
      const dialog = getDialog(/add model/i)
      await dialogFocused()
      await openInlineBedrockForm(dialog)
      await pickAuth(dialog, IAM_LABEL)
      await userEvent.type(getRoleArnInput(dialog), 'bad-arn')
      await userEvent.click(within(dialog).getByRole('button', { name: /^add$/i }))

      expect(await within(dialog).findByText(ARN_ERROR)).toBeInTheDocument()
      expect(within(dialog).getByRole('button', { name: /^add$/i })).toBeInTheDocument()
      expect(within(dialog).queryByText('br-1')).not.toBeInTheDocument()
    })

    it('sends bedrock_auth iam, trimmed role ARN and no api_key for an IAM deployment', async () => {
      const capturedBodies = new Map<string, string>()
      setupFetchMock(lbEntries(), capturedBodies)
      await renderSettled()

      await openCreateDialog()
      const dialog = getDialog(/add model/i)
      await dialogFocused()
      await openInlineBedrockForm(dialog)
      await userEvent.type(within(dialog).getByLabelText(/^api key$/i), 'typed-then-hidden')
      await pickAuth(dialog, IAM_LABEL)
      await userEvent.type(getRoleArnInput(dialog), `  ${VALID_ROLE_ARN} `)
      await userEvent.click(within(dialog).getByRole('button', { name: /^add$/i }))
      await submitDialog(dialog, /add model/i)

      await waitFor(() => expect(capturedBodies.has(DEPLOYMENTS_POST)).toBe(true))
      const body = JSON.parse(capturedBodies.get(DEPLOYMENTS_POST)!)
      expect(body.provider).toBe('bedrock')
      expect(body.bedrock_auth).toBe('iam')
      expect(body.bedrock_role_arn).toBe(VALID_ROLE_ARN)
      expect(body).not.toHaveProperty('api_key')
    })

    it('omits bedrock_role_arn for an IAM deployment with an empty ARN', async () => {
      const capturedBodies = new Map<string, string>()
      setupFetchMock(lbEntries(), capturedBodies)
      await renderSettled()

      await openCreateDialog()
      const dialog = getDialog(/add model/i)
      await dialogFocused()
      await openInlineBedrockForm(dialog)
      await pickAuth(dialog, IAM_LABEL)
      await userEvent.click(within(dialog).getByRole('button', { name: /^add$/i }))
      await submitDialog(dialog, /add model/i)

      await waitFor(() => expect(capturedBodies.has(DEPLOYMENTS_POST)).toBe(true))
      const body = JSON.parse(capturedBodies.get(DEPLOYMENTS_POST)!)
      expect(body.bedrock_auth).toBe('iam')
      expect(body).not.toHaveProperty('bedrock_role_arn')
    })

    it('sends bedrock_auth api_key and the api key for a default bedrock deployment', async () => {
      const capturedBodies = new Map<string, string>()
      setupFetchMock(lbEntries(), capturedBodies)
      await renderSettled()

      await openCreateDialog()
      const dialog = getDialog(/add model/i)
      await dialogFocused()
      await openInlineBedrockForm(dialog)
      await userEvent.type(within(dialog).getByLabelText(/^api key$/i), 'abwk-1')
      await userEvent.click(within(dialog).getByRole('button', { name: /^add$/i }))
      await submitDialog(dialog, /add model/i)

      await waitFor(() => expect(capturedBodies.has(DEPLOYMENTS_POST)).toBe(true))
      const body = JSON.parse(capturedBodies.get(DEPLOYMENTS_POST)!)
      expect(body.bedrock_auth).toBe('api_key')
      expect(body.api_key).toBe('abwk-1')
      expect(body).not.toHaveProperty('bedrock_role_arn')
    })

    it('blocks adding a keyless api_key bedrock deployment with an inline error and sends no request', async () => {
      const capturedBodies = new Map<string, string>()
      setupFetchMock(lbEntries(), capturedBodies)
      await renderSettled()

      await openCreateDialog()
      const dialog = getDialog(/add model/i)
      await dialogFocused()
      await openInlineBedrockForm(dialog)
      await userEvent.click(within(dialog).getByRole('button', { name: /^add$/i }))

      expect(await within(dialog).findByText(API_KEY_ERROR)).toBeInTheDocument()
      expect(within(dialog).getByRole('button', { name: /^add$/i })).toBeInTheDocument()
      await submitDialog(dialog, /add model/i)
      expect(
        await within(dialog).findByText('At least one deployment is required'),
      ).toBeInTheDocument()
      expect(capturedBodies.size).toBe(0)
    })

    it('adds a bedrock deployment once an api key is provided and creates it', async () => {
      const capturedBodies = new Map<string, string>()
      setupFetchMock(lbEntries(), capturedBodies)
      await renderSettled()

      await openCreateDialog()
      const dialog = getDialog(/add model/i)
      await dialogFocused()
      await openInlineBedrockForm(dialog)
      await userEvent.click(within(dialog).getByRole('button', { name: /^add$/i }))
      expect(await within(dialog).findByText(API_KEY_ERROR)).toBeInTheDocument()
      await userEvent.type(within(dialog).getByLabelText(/^api key$/i), 'abwk-1')
      await userEvent.click(within(dialog).getByRole('button', { name: /^add$/i }))
      await submitDialog(dialog, /add model/i)

      await waitFor(() => expect(capturedBodies.has(DEPLOYMENTS_POST)).toBe(true))
      expect(JSON.parse(capturedBodies.get(DEPLOYMENTS_POST)!).api_key).toBe('abwk-1')
    })

    it('non-bedrock deployments send no bedrock fields', async () => {
      const capturedBodies = new Map<string, string>()
      setupFetchMock(lbEntries(), capturedBodies)
      await renderSettled()

      await openCreateDialog()
      const dialog = getDialog(/add model/i)
      await dialogFocused()
      await userEvent.type(within(dialog).getByRole('textbox', { name: /^name$/i }), 'lb-oa')
      await switchToLoadBalancedTab()
      await addMinimalDeployment(dialog)
      expect(within(dialog).queryByText('Authentication')).not.toBeInTheDocument()
      await submitDialog(dialog, /add model/i)

      await waitFor(() => expect(capturedBodies.has(DEPLOYMENTS_POST)).toBe(true))
      const body = JSON.parse(capturedBodies.get(DEPLOYMENTS_POST)!)
      expect(body).not.toHaveProperty('bedrock_auth')
      expect(body).not.toHaveProperty('bedrock_role_arn')
    })
  })

  describe('deployment dialog', () => {
    const lbModel = (deployments: unknown[]) =>
      makeChatModel({ id: 'model-lb', name: 'lb-model', strategy: 'round-robin', deployments } as Partial<MockModelResponse>)

    function bedrockDeployment(overrides: Record<string, unknown> = {}) {
      return {
        id: 'dep-1',
        name: 'br-1',
        provider: 'bedrock',
        base_url: '',
        region: 'eu-central-1',
        bedrock_model_id: 'amazon.nova-lite-v1:0',
        weight: 1,
        priority: 0,
        is_active: true,
        ...overrides,
      }
    }

    async function openEditDeployment(modelName: string) {
      const nameEl = await screen.findByText(modelName)
      const row = nameEl.closest('tr')
      if (!row) throw new Error('row not found')
      await userEvent.click(within(row).getByRole('button', { name: 'Expand row' }))
      await userEvent.click(await screen.findByTitle('Edit deployment'))
      const dialog = getDialog(/edit deployment/i)
      await dialogFocused()
      return dialog
    }

    it('add: IAM mode hides the API key, validates the ARN and sends iam payload', async () => {
      const capturedBodies = new Map<string, string>()
      const model = lbModel([])
      setupFetchMock(
        [
          {
            matcher: (u) => u.includes('/api/v1/models/model-lb/deployments'),
            method: 'POST',
            response: bedrockDeployment(),
          },
          ...defaultEntries(MOCK_LICENSE_NO_FALLBACK, { data: [model], has_more: false }),
        ],
        capturedBodies,
      )
      await renderSettled('lb-model')

      await screen.findByText('lb-model')
      await userEvent.click(screen.getByTitle('Add deployment'))
      const dialog = getDialog(/add deployment/i)
      await dialogFocused()
      await userEvent.type(within(dialog).getByRole('textbox', { name: /^name$/i }), 'br-1')
      await pickBedrockProvider(dialog)
      expect(getComboboxByLabel(dialog, 'authentication')).toHaveTextContent('API key')
      await fillBedrockBasics(dialog)
      await pickAuth(dialog, IAM_LABEL)
      expect(queryApiKey(dialog)).not.toBeInTheDocument()

      await userEvent.type(getRoleArnInput(dialog), 'nope')
      await submitDialog(dialog, /add deployment/i)
      expect(await within(dialog).findByText(ARN_ERROR)).toBeInTheDocument()
      expect(capturedBodies.size).toBe(0)

      await userEvent.clear(getRoleArnInput(dialog))
      await userEvent.type(getRoleArnInput(dialog), ` ${VALID_ROLE_ARN} `)
      await submitDialog(dialog, /add deployment/i)

      const key = 'POST:/api/v1/models/model-lb/deployments'
      await waitFor(() => expect(capturedBodies.has(key)).toBe(true))
      const body = JSON.parse(capturedBodies.get(key)!)
      expect(body.bedrock_auth).toBe('iam')
      expect(body.bedrock_role_arn).toBe(VALID_ROLE_ARN)
      expect(body).not.toHaveProperty('api_key')
    })

    it('add: blocks a keyless api_key bedrock deployment and sends no request', async () => {
      const capturedBodies = new Map<string, string>()
      const model = lbModel([])
      setupFetchMock(
        [
          {
            matcher: (u) => u.includes('/api/v1/models/model-lb/deployments'),
            method: 'POST',
            response: bedrockDeployment(),
          },
          ...defaultEntries(MOCK_LICENSE_NO_FALLBACK, { data: [model], has_more: false }),
        ],
        capturedBodies,
      )
      await renderSettled('lb-model')

      await userEvent.click(screen.getByTitle('Add deployment'))
      const dialog = getDialog(/add deployment/i)
      await dialogFocused()
      await userEvent.type(within(dialog).getByRole('textbox', { name: /^name$/i }), 'br-1')
      await pickBedrockProvider(dialog)
      await fillBedrockBasics(dialog)
      await submitDialog(dialog, /add deployment/i)

      expect(await within(dialog).findByText(API_KEY_ERROR)).toBeInTheDocument()
      expect(capturedBodies.size).toBe(0)

      await userEvent.type(within(dialog).getByLabelText(/^api key$/i), 'abwk-1')
      await submitDialog(dialog, /add deployment/i)
      const key = 'POST:/api/v1/models/model-lb/deployments'
      await waitFor(() => expect(capturedBodies.has(key)).toBe(true))
    })

    it('add: IAM mode with empty ARN omits bedrock_role_arn', async () => {
      const capturedBodies = new Map<string, string>()
      const model = lbModel([])
      setupFetchMock(
        [
          {
            matcher: (u) => u.includes('/api/v1/models/model-lb/deployments'),
            method: 'POST',
            response: bedrockDeployment(),
          },
          ...defaultEntries(MOCK_LICENSE_NO_FALLBACK, { data: [model], has_more: false }),
        ],
        capturedBodies,
      )
      await renderSettled('lb-model')

      await screen.findByText('lb-model')
      await userEvent.click(screen.getByTitle('Add deployment'))
      const dialog = getDialog(/add deployment/i)
      await dialogFocused()
      await userEvent.type(within(dialog).getByRole('textbox', { name: /^name$/i }), 'br-1')
      await pickBedrockProvider(dialog)
      await fillBedrockBasics(dialog)
      await pickAuth(dialog, IAM_LABEL)
      await submitDialog(dialog, /add deployment/i)

      const key = 'POST:/api/v1/models/model-lb/deployments'
      await waitFor(() => expect(capturedBodies.has(key)).toBe(true))
      const body = JSON.parse(capturedBodies.get(key)!)
      expect(body.bedrock_auth).toBe('iam')
      expect(body).not.toHaveProperty('bedrock_role_arn')
      expect(body).not.toHaveProperty('api_key')
    })

    it('edit: switching an api_key deployment to IAM sends api_key "" and bedrock_auth iam', async () => {
      const capturedBodies = new Map<string, string>()
      const model = lbModel([bedrockDeployment({ bedrock_auth: 'api_key' })])
      setupFetchMock(
        [
          {
            matcher: (u) => u.includes('/api/v1/models/model-lb/deployments/dep-1'),
            method: 'PATCH',
            response: bedrockDeployment(),
          },
          ...defaultEntries(MOCK_LICENSE_NO_FALLBACK, { data: [model], has_more: false }),
        ],
        capturedBodies,
      )
      await renderSettled('lb-model')

      const dialog = await openEditDeployment('lb-model')
      expect(getComboboxByLabel(dialog, 'authentication')).toHaveTextContent('API key')
      await pickAuth(dialog, IAM_LABEL)
      expect(queryApiKey(dialog)).not.toBeInTheDocument()
      await submitDialog(dialog, /save changes/i)

      const key = 'PATCH:/api/v1/models/model-lb/deployments/dep-1'
      await waitFor(() => expect(capturedBodies.has(key)).toBe(true))
      expect(JSON.parse(capturedBodies.get(key)!)).toEqual({ api_key: '', bedrock_auth: 'iam' })
    })

    it('edit: switching an IAM deployment back sends bedrock_auth api_key and empty role ARN', async () => {
      const capturedBodies = new Map<string, string>()
      const model = lbModel([bedrockDeployment({ bedrock_auth: 'iam', bedrock_role_arn: VALID_ROLE_ARN })])
      setupFetchMock(
        [
          {
            matcher: (u) => u.includes('/api/v1/models/model-lb/deployments/dep-1'),
            method: 'PATCH',
            response: bedrockDeployment(),
          },
          ...defaultEntries(MOCK_LICENSE_NO_FALLBACK, { data: [model], has_more: false }),
        ],
        capturedBodies,
      )
      await renderSettled('lb-model')

      const dialog = await openEditDeployment('lb-model')
      expect(getComboboxByLabel(dialog, 'authentication')).toHaveTextContent(IAM_LABEL)
      expect(getRoleArnInput(dialog)).toHaveValue(VALID_ROLE_ARN)
      await pickAuth(dialog, 'API key')
      await userEvent.type(within(dialog).getByLabelText(/^api key$/i), 'new-key')
      await submitDialog(dialog, /save changes/i)

      const key = 'PATCH:/api/v1/models/model-lb/deployments/dep-1'
      await waitFor(() => expect(capturedBodies.has(key)).toBe(true))
      expect(JSON.parse(capturedBodies.get(key)!)).toEqual({
        api_key: 'new-key',
        bedrock_auth: 'api_key',
        bedrock_role_arn: '',
      })
    })

    it('edit: blocks save with an invalid role ARN', async () => {
      const capturedBodies = new Map<string, string>()
      const model = lbModel([bedrockDeployment({ bedrock_auth: 'iam' })])
      setupFetchMock(
        [
          {
            matcher: (u) => u.includes('/api/v1/models/model-lb/deployments/dep-1'),
            method: 'PATCH',
            response: bedrockDeployment(),
          },
          ...defaultEntries(MOCK_LICENSE_NO_FALLBACK, { data: [model], has_more: false }),
        ],
        capturedBodies,
      )
      await renderSettled('lb-model')

      const dialog = await openEditDeployment('lb-model')
      await userEvent.type(getRoleArnInput(dialog), 'arn:aws:s3:::bucket')
      await submitDialog(dialog, /save changes/i)

      expect(await within(dialog).findByText(ARN_ERROR)).toBeInTheDocument()
      expect(capturedBodies.size).toBe(0)
    })

    it('edit: non-bedrock deployment shows no Authentication select and sends no bedrock fields', async () => {
      const capturedBodies = new Map<string, string>()
      const model = lbModel([
        { id: 'dep-1', name: 'oa-1', provider: 'openai', base_url: 'https://api.openai.com/v1', weight: 1, priority: 0, is_active: true },
      ])
      setupFetchMock(
        [
          {
            matcher: (u) => u.includes('/api/v1/models/model-lb/deployments/dep-1'),
            method: 'PATCH',
            response: {},
          },
          ...defaultEntries(MOCK_LICENSE_NO_FALLBACK, { data: [model], has_more: false }),
        ],
        capturedBodies,
      )
      await renderSettled('lb-model')

      const dialog = await openEditDeployment('lb-model')
      expect(within(dialog).queryByText('Authentication')).not.toBeInTheDocument()
      await userEvent.type(within(dialog).getByLabelText(/^api key$/i), 'sk-new')
      await submitDialog(dialog, /save changes/i)

      const key = 'PATCH:/api/v1/models/model-lb/deployments/dep-1'
      await waitFor(() => expect(capturedBodies.has(key)).toBe(true))
      expect(JSON.parse(capturedBodies.get(key)!)).toEqual({ api_key: 'sk-new' })
    })
  })
})

// ---------------------------------------------------------------------------
// Tests: cache price fields
// ---------------------------------------------------------------------------

describe('Cache price fields', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  async function openEditDialogForModel(modelName: string) {
    const modelNameEl = await screen.findByText(modelName)
    const row = modelNameEl.closest('tr')
    if (!row) throw new Error(`Could not find table row for model "${modelName}"`)
    await userEvent.click(within(row).getByTitle('Edit model'))
    await waitFor(() => expect(screen.getByRole('heading', { name: /edit model/i })).toBeInTheDocument())
    return getDialog(/edit model/i)
  }

  function editSetup(model: MockModelResponse, capturedBodies: Map<string, string>) {
    setupFetchMock(
      [
        ...defaultEntries(MOCK_LICENSE_NO_FALLBACK, {
          data: [model, ...MOCK_MODELS_LIST.slice(1)],
          has_more: false,
        }),
        {
          matcher: (u) => u.includes('/api/v1/models/model-1'),
          method: 'PATCH',
          response: model,
        },
      ],
      capturedBodies,
    )
  }

  async function createAndCapture(fill: (dialog: HTMLElement) => Promise<void>) {
    const capturedBodies = new Map<string, string>()
    setupFetchMock(
      [
        {
          matcher: (u) => u.includes('/api/v1/models') && !u.includes('/health'),
          method: 'POST',
          response: makeChatModel({ id: 'new-model', name: 'cache-model' }),
        },
        ...defaultEntries(),
      ],
      capturedBodies,
    )
    renderModelsPage()
    await screen.findByText('gpt-4o')
    await openCreateDialog()
    const dialog = getDialog(/add model/i)
    const nameInput = within(dialog).getByRole('textbox', { name: /^name$/i })
    await userEvent.type(nameInput, 'cache-model')
    await waitFor(() => expect(nameInput).toHaveValue('cache-model'))
    await userEvent.type(within(dialog).getByRole('textbox', { name: /base url/i }), 'https://api.openai.com/v1')
    await fill(dialog)
    await submitDialog(dialog, /add model/i)
    await waitFor(() => expect(capturedBodies.has('POST:/api/v1/models')).toBe(true), { timeout: 3000 })
    return JSON.parse(capturedBodies.get('POST:/api/v1/models')!) as Record<string, unknown>
  }

  it('create sends both cache prices when filled', async () => {
    const body = await createAndCapture(async (dialog) => {
      await userEvent.type(within(dialog).getByLabelText(/cached input price/i), '0.3')
      await userEvent.type(within(dialog).getByLabelText(/cache write price/i), '3.75')
    })
    expect(body.cached_input_price_per_1m).toBe(0.3)
    expect(body.cache_write_price_per_1m).toBe(3.75)
  })

  it('create omits cache prices when empty', async () => {
    const body = await createAndCapture(async () => {})
    expect(body).not.toHaveProperty('cached_input_price_per_1m')
    expect(body).not.toHaveProperty('cache_write_price_per_1m')
  })

  it('edit preloads existing cache prices', async () => {
    const model = makeChatModel({ cached_input_price_per_1m: 0.3, cache_write_price_per_1m: 3.75 })
    editSetup(model, new Map())
    renderModelsPage()
    const dialog = await openEditDialogForModel('gpt-4o')
    expect(within(dialog).getByLabelText(/cached input price/i)).toHaveValue(0.3)
    expect(within(dialog).getByLabelText(/cache write price/i)).toHaveValue(3.75)
  })

  it('edit sends 0 when a cache price is cleared', async () => {
    const captured = new Map<string, string>()
    editSetup(makeChatModel({ cached_input_price_per_1m: 0.3, cache_write_price_per_1m: 3.75 }), captured)
    renderModelsPage()
    const dialog = await openEditDialogForModel('gpt-4o')
    await userEvent.clear(within(dialog).getByLabelText(/cached input price/i))
    await submitDialog(dialog, /save changes/i)
    await waitFor(() => expect(captured.has('PATCH:/api/v1/models/model-1')).toBe(true))
    const body = JSON.parse(captured.get('PATCH:/api/v1/models/model-1')!)
    expect(body.cached_input_price_per_1m).toBe(0)
    expect(body).not.toHaveProperty('cache_write_price_per_1m')
  })

  it('edit omits cache prices when unchanged', async () => {
    const captured = new Map<string, string>()
    editSetup(makeChatModel({ cached_input_price_per_1m: 0.3, timeout: '30s' }), captured)
    renderModelsPage()
    const dialog = await openEditDialogForModel('gpt-4o')
    const timeoutInput = within(dialog).getByRole('textbox', { name: /timeout/i })
    await userEvent.clear(timeoutInput)
    await userEvent.type(timeoutInput, '60s')
    await submitDialog(dialog, /save changes/i)
    await waitFor(() => expect(captured.has('PATCH:/api/v1/models/model-1')).toBe(true))
    const body = JSON.parse(captured.get('PATCH:/api/v1/models/model-1')!)
    expect(body).not.toHaveProperty('cached_input_price_per_1m')
    expect(body).not.toHaveProperty('cache_write_price_per_1m')
  })
})

// ---------------------------------------------------------------------------
// Tests: AWS Bedrock endpoint validation
// ---------------------------------------------------------------------------

const ENDPOINT_ERROR =
  'In IAM mode the endpoint must be the Bedrock Runtime endpoint of the selected region (standard, FIPS, dual-stack or VPC endpoint)'
const QUERY_ERROR = 'Endpoint must not contain a query or fragment'

const VALID_IAM_ENDPOINTS = [
  'https://bedrock-runtime.eu-central-1.amazonaws.com',
  'https://bedrock-runtime-fips.eu-central-1.amazonaws.com/',
  'https://bedrock-runtime.eu-central-1.amazonaws.com.cn',
  'https://bedrock-runtime.eu-central-1.api.aws',
  'https://bedrock-runtime-fips.eu-central-1.api.aws:443',
  'https://vpce-0a1b2c.bedrock-runtime.eu-central-1.vpce.amazonaws.com',
  'https://vpce-0a1b2c.bedrock-runtime-fips.eu-central-1.vpce.amazonaws.com.cn',
]

const INVALID_IAM_ENDPOINTS = [
  'https://abc123.execute-api.eu-central-1.amazonaws.com',
  'https://my-lb-123.eu-central-1.elb.amazonaws.com',
  'https://bucket.s3.eu-central-1.amazonaws.com',
  'https://bedrock-runtime.us-east-1.amazonaws.com',
  'http://bedrock-runtime.eu-central-1.amazonaws.com',
  'https://bedrock-runtime.eu-central-1.amazonaws.com:8443',
  'https://bedrock-runtime.eu-central-1.amazonaws.com.evil.com',
]

describe('AWS Bedrock endpoint validation', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  async function openBedrockSingle(capturedBodies: Map<string, string>, iam: boolean) {
    setupFetchMock(postModelEntries(), capturedBodies)
    await renderSettled()
    await openCreateDialog()
    const dialog = getDialog(/add model/i)
    await dialogFocused()
    await userEvent.type(within(dialog).getByRole('textbox', { name: /^name$/i }), 'claude-br')
    await pickBedrockProvider(dialog)
    await fillBedrockBasics(dialog)
    if (iam) await pickAuth(dialog, IAM_LABEL)
    else await userEvent.type(within(dialog).getByLabelText(/^api key$/i), 'abwk-1')
    return dialog
  }

  it.each(INVALID_IAM_ENDPOINTS)('single create blocks %s in IAM mode with zero requests', async (endpoint) => {
    const capturedBodies = new Map<string, string>()
    const dialog = await openBedrockSingle(capturedBodies, true)
    await userEvent.type(within(dialog).getByRole('textbox', { name: /base url/i }), endpoint)
    await submitDialog(dialog, /add model/i)

    expect(await within(dialog).findByText(ENDPOINT_ERROR)).toBeInTheDocument()
    expect(capturedBodies.size).toBe(0)
  })

  it.each(VALID_IAM_ENDPOINTS)('single create accepts %s in IAM mode', async (endpoint) => {
    const capturedBodies = new Map<string, string>()
    const dialog = await openBedrockSingle(capturedBodies, true)
    await userEvent.type(within(dialog).getByRole('textbox', { name: /base url/i }), endpoint)
    await submitDialog(dialog, /add model/i)

    await waitFor(() => expect(capturedBodies.has(MODELS_POST)).toBe(true))
    expect(within(dialog).queryByText(ENDPOINT_ERROR)).not.toBeInTheDocument()
  })

  it('single create accepts an empty endpoint in IAM mode', async () => {
    const capturedBodies = new Map<string, string>()
    const dialog = await openBedrockSingle(capturedBodies, true)
    await submitDialog(dialog, /add model/i)
    await waitFor(() => expect(capturedBodies.has(MODELS_POST)).toBe(true))
  })

  it('single create keeps arbitrary https endpoints allowed in api_key mode', async () => {
    const capturedBodies = new Map<string, string>()
    const dialog = await openBedrockSingle(capturedBodies, false)
    await userEvent.type(within(dialog).getByRole('textbox', { name: /base url/i }), 'https://proxy.internal.example')
    await submitDialog(dialog, /add model/i)
    await waitFor(() => expect(capturedBodies.has(MODELS_POST)).toBe(true))
  })

  it.each([true, false])('single create rejects a query or fragment (iam=%s)', async (iam) => {
    const capturedBodies = new Map<string, string>()
    const dialog = await openBedrockSingle(capturedBodies, iam)
    await userEvent.type(
      within(dialog).getByRole('textbox', { name: /base url/i }),
      'https://bedrock-runtime.eu-central-1.amazonaws.com/x?a=b',
    )
    await submitDialog(dialog, /add model/i)

    expect(await within(dialog).findByText(QUERY_ERROR)).toBeInTheDocument()
    expect(capturedBodies.size).toBe(0)
  })

  it('load-balanced create blocks an invalid IAM endpoint before any request', async () => {
    const capturedBodies = new Map<string, string>()
    setupFetchMock(postModelEntries(), capturedBodies)
    await renderSettled()
    await openCreateDialog()
    const dialog = getDialog(/add model/i)
    await dialogFocused()
    await userEvent.type(within(dialog).getByRole('textbox', { name: /^name$/i }), 'lb-br')
    await switchToLoadBalancedTab()
    await userEvent.click(within(dialog).getByRole('button', { name: /\+ add deployment/i }))
    await pickBedrockProvider(dialog)
    const names = within(dialog).getAllByRole('textbox', { name: /^name$/i })
    await userEvent.type(names[names.length - 1], 'br-1')
    await fillBedrockBasics(dialog)
    await pickAuth(dialog, IAM_LABEL)
    const urls = within(dialog).getAllByRole('textbox', { name: /base url/i })
    await userEvent.type(urls[urls.length - 1], 'https://bucket.s3.eu-central-1.amazonaws.com')
    await userEvent.click(within(dialog).getByRole('button', { name: /^add$/i }))

    expect(await within(dialog).findByText(ENDPOINT_ERROR)).toBeInTheDocument()
    await submitDialog(dialog, /add model/i)
    expect(capturedBodies.size).toBe(0)
  })

  it('load-balanced create accepts a matching VPC endpoint', async () => {
    const capturedBodies = new Map<string, string>()
    setupFetchMock(
      [
        {
          matcher: (u) => u.includes('/api/v1/models/new-br/deployments'),
          method: 'POST',
          response: { id: 'dep-1', name: 'br-1', provider: 'bedrock' },
        },
        ...postModelEntries(),
      ],
      capturedBodies,
    )
    await renderSettled()
    await openCreateDialog()
    const dialog = getDialog(/add model/i)
    await dialogFocused()
    await userEvent.type(within(dialog).getByRole('textbox', { name: /^name$/i }), 'lb-br')
    await switchToLoadBalancedTab()
    await userEvent.click(within(dialog).getByRole('button', { name: /\+ add deployment/i }))
    await pickBedrockProvider(dialog)
    const names = within(dialog).getAllByRole('textbox', { name: /^name$/i })
    await userEvent.type(names[names.length - 1], 'br-1')
    await fillBedrockBasics(dialog)
    await pickAuth(dialog, IAM_LABEL)
    const urls = within(dialog).getAllByRole('textbox', { name: /base url/i })
    await userEvent.type(urls[urls.length - 1], 'https://vpce-0a1b2c.bedrock-runtime.eu-central-1.vpce.amazonaws.com')
    await userEvent.click(within(dialog).getByRole('button', { name: /^add$/i }))
    await submitDialog(dialog, /add model/i)

    await waitFor(() => expect(capturedBodies.has('POST:/api/v1/models/new-br/deployments')).toBe(true))
  })

  it('edit model blocks a region mismatch in IAM mode and sends no PATCH', async () => {
    const capturedBodies = new Map<string, string>()
    const model = makeChatModel({
      id: 'model-br',
      name: 'claude-br',
      provider: 'bedrock',
      base_url: '',
      region: 'eu-central-1',
      bedrock_model_id: 'amazon.nova-lite-v1:0',
      bedrock_auth: 'iam',
    })
    setupFetchMock(
      [
        ...defaultEntries(MOCK_LICENSE_NO_FALLBACK, { data: [model], has_more: false }),
        { matcher: (u) => u.includes('/api/v1/models/model-br'), method: 'PATCH', response: model },
      ],
      capturedBodies,
    )
    await renderSettled('claude-br')

    const row = (await screen.findByText('claude-br')).closest('tr')
    if (!row) throw new Error('row not found')
    await userEvent.click(within(row).getByTitle('Edit model'))
    const dialog = getDialog(/edit model/i)
    await dialogFocused()
    await userEvent.type(
      within(dialog).getByRole('textbox', { name: /base url/i }),
      'https://bedrock-runtime.us-east-1.amazonaws.com',
    )
    await submitDialog(dialog, /save changes/i)

    expect(await within(dialog).findByText(ENDPOINT_ERROR)).toBeInTheDocument()
    expect(capturedBodies.size).toBe(0)
  })

  it('deployment dialog blocks an execute-api host in IAM mode and sends no POST', async () => {
    const capturedBodies = new Map<string, string>()
    const model = makeChatModel({
      id: 'model-lb',
      name: 'lb-model',
      strategy: 'round-robin',
      deployments: [],
    } as Partial<MockModelResponse>)
    setupFetchMock(
      [
        {
          matcher: (u) => u.includes('/api/v1/models/model-lb/deployments'),
          method: 'POST',
          response: {},
        },
        ...defaultEntries(MOCK_LICENSE_NO_FALLBACK, { data: [model], has_more: false }),
      ],
      capturedBodies,
    )
    await renderSettled('lb-model')

    await userEvent.click(screen.getByTitle('Add deployment'))
    const dialog = getDialog(/add deployment/i)
    await dialogFocused()
    await userEvent.type(within(dialog).getByRole('textbox', { name: /^name$/i }), 'br-1')
    await pickBedrockProvider(dialog)
    await fillBedrockBasics(dialog)
    await pickAuth(dialog, IAM_LABEL)
    await userEvent.type(
      within(dialog).getByRole('textbox', { name: /base url/i }),
      'https://abc123.execute-api.eu-central-1.amazonaws.com',
    )
    await submitDialog(dialog, /add deployment/i)

    expect(await within(dialog).findByText(ENDPOINT_ERROR)).toBeInTheDocument()
    expect(capturedBodies.size).toBe(0)
  })
})
