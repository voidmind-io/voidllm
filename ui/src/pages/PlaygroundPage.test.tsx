import React from 'react'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { describe, it, expect, vi, beforeEach } from 'vitest'
import PlaygroundPage from './PlaygroundPage'

// ---------------------------------------------------------------------------
// Types used in mocks (mirror the production types)
// ---------------------------------------------------------------------------

interface MockAvailableModel {
  name: string
  type: string
}

// ---------------------------------------------------------------------------
// Test fixtures
// ---------------------------------------------------------------------------

const CHAT_MODELS: MockAvailableModel[] = [
  { name: 'chat-alpha', type: 'chat' },
  { name: 'chat-beta', type: 'chat' },
]

const CHAT_AND_COMPLETION_MODELS: MockAvailableModel[] = [
  { name: 'chat-a', type: 'chat' },
  { name: 'chat-b', type: 'chat' },
  { name: 'comp-a', type: 'completion' },
  { name: 'comp-b', type: 'completion' },
]

const COMPLETION_AND_EMBEDDING_MODELS: MockAvailableModel[] = [
  { name: 'comp-1', type: 'completion' },
  { name: 'embed-1', type: 'embedding' },
]

// ---------------------------------------------------------------------------
// Render helpers
// ---------------------------------------------------------------------------

function renderPlaygroundPage() {
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  })
  function Wrapper({ children }: { children: React.ReactNode }) {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  }
  return render(<PlaygroundPage />, { wrapper: Wrapper })
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

function availableModelsEntry(models: MockAvailableModel[]): FetchMockEntry {
  return {
    matcher: (u) => u.includes('/api/v1/me/available-models'),
    method: 'GET',
    response: { models },
  }
}

function getModelSelect(): HTMLElement {
  return screen.getByRole('combobox')
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

describe('PlaygroundPage — default tab and model selection', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
    // jsdom does not implement scrollIntoView; PlaygroundPage calls it on
    // every chat history update.
    Element.prototype.scrollIntoView = vi.fn()
  })

  it('activates the first available tab once models load', async () => {
    setupFetchMock([availableModelsEntry(COMPLETION_AND_EMBEDDING_MODELS)])
    renderPlaygroundPage()

    const completionTab = await screen.findByRole('tab', { name: 'Completion' })
    expect(completionTab).toHaveAttribute('aria-selected', 'true')

    const embeddingTab = screen.getByRole('tab', { name: 'Embedding' })
    expect(embeddingTab).toHaveAttribute('aria-selected', 'false')
  })

  it('selects the first model of the active tab without any user interaction', async () => {
    setupFetchMock([availableModelsEntry(CHAT_MODELS)])
    renderPlaygroundPage()

    await waitFor(() => expect(getModelSelect()).toHaveTextContent('chat-alpha'))
  })

  it('selects the first model of a newly active tab, dropping the previous tab\'s selection', async () => {
    setupFetchMock([availableModelsEntry(CHAT_AND_COMPLETION_MODELS)])
    renderPlaygroundPage()

    await waitFor(() => expect(getModelSelect()).toHaveTextContent('chat-a'))

    // Explicitly select a different model on the chat tab
    await userEvent.click(getModelSelect())
    await userEvent.click(screen.getByRole('option', { name: 'chat-b' }))
    expect(getModelSelect()).toHaveTextContent('chat-b')

    // Switch to the Completion tab
    await userEvent.click(screen.getByRole('tab', { name: 'Completion' }))

    // The new tab shows its own first model, not the model from the old tab
    await waitFor(() => expect(getModelSelect()).toHaveTextContent('comp-a'))
    expect(getModelSelect()).not.toHaveTextContent('chat-b')
  })

  it('restores the previously selected model when switching back to its tab', async () => {
    setupFetchMock([availableModelsEntry(CHAT_AND_COMPLETION_MODELS)])
    renderPlaygroundPage()

    await waitFor(() => expect(getModelSelect()).toHaveTextContent('chat-a'))

    await userEvent.click(getModelSelect())
    await userEvent.click(screen.getByRole('option', { name: 'chat-b' }))
    expect(getModelSelect()).toHaveTextContent('chat-b')

    await userEvent.click(screen.getByRole('tab', { name: 'Completion' }))
    await waitFor(() => expect(getModelSelect()).toHaveTextContent('comp-a'))

    await userEvent.click(screen.getByRole('tab', { name: 'Chat' }))

    await waitFor(() => expect(getModelSelect()).toHaveTextContent('chat-b'))
  })

  it('keeps an explicitly selected model that belongs to the active tab across an unrelated re-render', async () => {
    setupFetchMock([availableModelsEntry(CHAT_MODELS)])
    renderPlaygroundPage()

    await waitFor(() => expect(getModelSelect()).toHaveTextContent('chat-alpha'))

    await userEvent.click(getModelSelect())
    await userEvent.click(screen.getByRole('option', { name: 'chat-beta' }))
    expect(getModelSelect()).toHaveTextContent('chat-beta')

    // Trigger a state update/re-render unrelated to tab or model selection
    await userEvent.click(screen.getByRole('switch'))

    expect(getModelSelect()).toHaveTextContent('chat-beta')
  })

  it('sends the selected model in the chat request body', async () => {
    const capturedBodies = new Map<string, string>()
    setupFetchMock(
      [
        availableModelsEntry(CHAT_MODELS),
        {
          matcher: (u) => u.includes('/api/v1/playground/chat/completions'),
          method: 'POST',
          response: {
            choices: [{ message: { content: 'hello there' } }],
            usage: { prompt_tokens: 3, completion_tokens: 2, total_tokens: 5 },
          },
        },
      ],
      capturedBodies,
    )
    renderPlaygroundPage()

    await waitFor(() => expect(getModelSelect()).toHaveTextContent('chat-alpha'))

    await userEvent.click(getModelSelect())
    await userEvent.click(screen.getByRole('option', { name: 'chat-beta' }))

    // Disable streaming so the mocked response can be a plain JSON body
    await userEvent.click(screen.getByRole('switch'))

    await userEvent.type(screen.getByPlaceholderText('Type your message...'), 'Hello there')
    await userEvent.click(screen.getByRole('button', { name: /send message/i }))

    await waitFor(() =>
      expect(capturedBodies.has('POST:/api/v1/playground/chat/completions')).toBe(true),
    )
    const body = JSON.parse(capturedBodies.get('POST:/api/v1/playground/chat/completions') ?? '{}')
    expect(body.model).toBe('chat-beta')
  })

  it('renders safely with an empty model list and selects no model', async () => {
    setupFetchMock([availableModelsEntry([])])
    renderPlaygroundPage()

    await waitFor(() => expect(getModelSelect()).toHaveTextContent('No models available'))
    expect(getModelSelect()).toBeDisabled()
    expect(screen.getByText('No model selected')).toBeInTheDocument()
    expect(screen.queryByRole('tab')).not.toBeInTheDocument()
  })
})
