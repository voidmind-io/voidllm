import { useEffect, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { PageHeader } from '../components/ui/PageHeader'
import { Button } from '../components/ui/Button'
import { Input } from '../components/ui/Input'
import { Textarea } from '../components/ui/Textarea'
import { Select } from '../components/ui/Select'
import type { SelectOption } from '../components/ui/Select'
import { Toggle } from '../components/ui/Toggle'
import apiClient from '../api/client'
import { LOCAL_STORAGE_KEY } from '../lib/constants'
import { cn } from '../lib/utils'

interface AvailableModelsResponse {
  models: string[]
}

interface ChatMessage {
  id: string
  role: 'user' | 'assistant'
  content: string
}

interface UsageInfo {
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  duration: number
}

const MAX_MESSAGES = 50

export default function PlaygroundPage() {
  const [model, setModel] = useState('')
  const [apiKey, setApiKey] = useState('')
  const [systemPrompt, setSystemPrompt] = useState('You are a helpful assistant.')
  const [message, setMessage] = useState('')
  const [chatHistory, setChatHistory] = useState<ChatMessage[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [lastUsage, setLastUsage] = useState<UsageInfo | null>(null)
  const [streaming, setStreaming] = useState(true)

  const chatEndRef = useRef<HTMLDivElement>(null)
  const abortRef = useRef<AbortController | null>(null)
  const textareaRef = useRef<HTMLTextAreaElement>(null)

  // Cancel any in-flight request on unmount
  useEffect(() => {
    return () => {
      abortRef.current?.abort()
    }
  }, [])

  const { data: modelsData } = useQuery({
    queryKey: ['available-models'],
    queryFn: () => apiClient<AvailableModelsResponse>('/me/available-models'),
  })

  // Auto-select the first model when the list loads
  useEffect(() => {
    if (modelsData?.models && modelsData.models.length > 0 && model === '') {
      setModel(modelsData.models[0])
    }
  }, [modelsData, model])

  // Scroll to bottom when new messages arrive
  useEffect(() => {
    chatEndRef.current?.scrollIntoView({ behavior: 'smooth' })
  }, [chatHistory, loading])

  const modelOptions: SelectOption[] = (modelsData?.models ?? []).map((m) => ({
    value: m,
    label: m,
  }))

  async function handleSend() {
    if (!model || !message.trim() || loading) return

    // Cancel any previous in-flight request
    abortRef.current?.abort()
    const controller = new AbortController()
    abortRef.current = controller

    const userMessage: ChatMessage = {
      id: crypto.randomUUID(),
      role: 'user',
      content: message.trim(),
    }

    // Cap stored history at MAX_MESSAGES
    const newHistory = [...chatHistory, userMessage].slice(-MAX_MESSAGES)
    setChatHistory(newHistory)
    setMessage('')
    setLoading(true)
    setError(null)
    // Focus after React re-render from state updates above
    requestAnimationFrame(() => textareaRef.current?.focus())

    // Strip `id` from messages sent to the API
    const messages = [
      ...(systemPrompt.trim()
        ? [{ role: 'system' as const, content: systemPrompt.trim() }]
        : []),
      ...newHistory.map(({ role, content }) => ({ role, content })),
    ]

    const token = apiKey.trim() || localStorage.getItem(LOCAL_STORAGE_KEY) || ''
    const startTime = Date.now()

    try {
      const res = await fetch('/v1/chat/completions', {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          Authorization: `Bearer ${token}`,
        },
        body: JSON.stringify({ model, messages, stream: streaming }),
        signal: controller.signal,
      })

      if (res.status === 401) {
        if (!apiKey.trim()) {
          // Session key expired — clear it and redirect to login
          localStorage.removeItem(LOCAL_STORAGE_KEY)
          window.location.href = '/login'
          return
        }
        // Custom key is invalid — show error, do NOT touch the session
        setError('Invalid or expired API key')
        return
      }

      if (!res.ok) {
        const body = await res.json().catch(() => null)
        const raw =
          (body as { error?: { message?: string } } | null)?.error?.message ??
          (body as { message?: string } | null)?.message ??
          `HTTP ${res.status}: ${res.statusText}`
        const errMessage = raw.length > 200 ? raw.slice(0, 200) + '...' : raw
        setError(errMessage)
        return
      }

      if (streaming) {
        const reader = res.body?.getReader()
        if (!reader) {
          setError('Streaming not supported')
          return
        }

        const decoder = new TextDecoder()
        let buffer = ''
        const assistantId = crypto.randomUUID()

        // Add an empty assistant message that will be filled by deltas
        setChatHistory((prev) => [
          ...prev,
          { id: assistantId, role: 'assistant', content: '' },
        ])

        let finalUsage: {
          prompt_tokens: number
          completion_tokens: number
          total_tokens: number
        } | null = null

        let fullContent = ''

        try {
          while (true) {
            const { done, value } = await reader.read()
            if (done) break

            buffer += decoder.decode(value, { stream: true })
            const lines = buffer.split('\n')
            buffer = lines.pop() ?? ''

            let chunkContent = ''
            for (const line of lines) {
              const trimmed = line.trim()
              if (!trimmed || trimmed === 'data: [DONE]') continue
              if (!trimmed.startsWith('data: ')) continue

              try {
                const json = JSON.parse(trimmed.slice(6)) as {
                  choices?: { delta?: { content?: string } }[]
                  usage?: {
                    prompt_tokens?: number
                    completion_tokens?: number
                    total_tokens?: number
                  }
                }
                const delta = json.choices?.[0]?.delta?.content
                if (delta) {
                  chunkContent += delta
                }
                if (json.usage) {
                  finalUsage = {
                    prompt_tokens: json.usage.prompt_tokens ?? 0,
                    completion_tokens: json.usage.completion_tokens ?? 0,
                    total_tokens: json.usage.total_tokens ?? 0,
                  }
                }
              } catch {
                // Skip unparseable SSE lines
              }
            }

            if (chunkContent) {
              fullContent += chunkContent
              const snapshot = fullContent
              setChatHistory((prev) =>
                prev.map((msg) =>
                  msg.id === assistantId
                    ? { ...msg, content: snapshot }
                    : msg,
                ),
              )
            }
          }
        } finally {
          reader.releaseLock()
        }

        const duration = (Date.now() - startTime) / 1000
        setLastUsage(finalUsage ? { ...finalUsage, duration } : null)
      } else {
        const data = (await res.json()) as {
          choices?: { message?: { content?: string } }[]
          usage?: {
            prompt_tokens?: number
            completion_tokens?: number
            total_tokens?: number
          }
        }

        const duration = (Date.now() - startTime) / 1000
        const assistantContent = data.choices?.[0]?.message?.content ?? ''
        setChatHistory((prev) => [
          ...prev,
          { id: crypto.randomUUID(), role: 'assistant', content: assistantContent },
        ])

        if (data.usage) {
          setLastUsage({
            prompt_tokens: data.usage.prompt_tokens ?? 0,
            completion_tokens: data.usage.completion_tokens ?? 0,
            total_tokens: data.usage.total_tokens ?? 0,
            duration,
          })
        }
      }
    } catch (err) {
      if (err instanceof DOMException && err.name === 'AbortError') return
      const raw = err instanceof Error ? err.message : 'Request failed'
      setError(raw.length > 200 ? raw.slice(0, 200) + '...' : raw)
    } finally {
      setLoading(false)
    }
  }

  function handleClear() {
    setChatHistory([])
    setLastUsage(null)
    setError(null)
  }

  const canSend = !!model && !!message.trim() && !loading

  return (
    <>
      <PageHeader title="Playground" description="Test models interactively" />

      <div className="flex gap-6 h-[calc(100vh-180px)]">
        {/* LEFT PANEL — 40% */}
        <div className="w-[40%] shrink-0 flex flex-col gap-4 overflow-y-auto p-1">
          {/* API Key input */}
          <Input
            label="API Key"
            type="password"
            value={apiKey}
            onChange={(e) => setApiKey(e.target.value)}
            placeholder="Session key (default)"
            description="Leave empty to use your session key. Paste a different key to test its permissions."
          />

          {/* Model selector */}
          <Select
            label="Model"
            options={modelOptions}
            value={model}
            onChange={setModel}
            placeholder={
              modelsData
                ? modelOptions.length === 0
                  ? 'No models available'
                  : 'Select a model...'
                : 'Loading models...'
            }
            searchable={modelOptions.length > 8}
            disabled={modelOptions.length === 0}
            fullWidth
          />

          {/* Streaming toggle */}
          <Toggle
            checked={streaming}
            onChange={setStreaming}
            label="Stream response"
            size="sm"
          />

          {/* System prompt */}
          <Textarea
            label="System Prompt"
            value={systemPrompt}
            onChange={(e) => setSystemPrompt(e.target.value)}
            placeholder="You are a helpful assistant."
            rows={4}
            className="font-mono"
            disabled={loading}
          />

          {/* Message + Send */}
          <Textarea
            ref={textareaRef}
            label="Message"
            value={message}
            onChange={(e) => setMessage(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && !e.shiftKey) {
                e.preventDefault()
                void handleSend()
              }
            }}
            placeholder="Type your message..."
            rows={6}
            disabled={loading}
            description="Enter to send, Shift+Enter for new line"
            className="flex-1 min-h-[120px]"
            wrapperClassName="flex-1"
          />

          {/* Error */}
          {error !== null && (
            <div
              role="alert"
              className="rounded-lg border border-error/40 bg-error/10 px-4 py-3 text-sm text-error"
            >
              {error}
            </div>
          )}

          <div className="flex justify-end">
            <Button
              variant="primary"
              loading={loading}
              disabled={!canSend}
              onClick={() => {
                void handleSend()
              }}
            >
              {!loading && (
                <svg
                  className="h-4 w-4"
                  fill="none"
                  viewBox="0 0 24 24"
                  stroke="currentColor"
                  strokeWidth={2}
                  aria-hidden="true"
                >
                  <path
                    strokeLinecap="round"
                    strokeLinejoin="round"
                    d="M13 7l5 5m0 0l-5 5m5-5H6"
                  />
                </svg>
              )}
              Send
            </Button>
          </div>
        </div>

        {/* RIGHT PANEL — 60% */}
        <div className="flex-1 flex flex-col rounded-lg border border-border bg-bg-secondary overflow-hidden">
          {/* Chat messages — scrollable */}
          <div className="flex-1 overflow-y-auto p-4 space-y-4">
            {chatHistory.length === 0 && !loading && (
              <div className="flex h-full items-center justify-center">
                <p className="text-sm text-text-tertiary">
                  Send a message to start chatting
                </p>
              </div>
            )}

            {chatHistory.map((msg) => (
              <div
                key={msg.id}
                className={cn(
                  'flex gap-3',
                  msg.role === 'user' ? 'justify-end' : 'justify-start',
                )}
              >
                <div
                  className={cn(
                    'max-w-[80%] rounded-lg px-4 py-2.5 text-sm',
                    msg.role === 'user'
                      ? 'bg-accent/15 text-accent'
                      : 'bg-bg-tertiary text-text-primary',
                  )}
                >
                  {msg.content ? (
                    <p className="whitespace-pre-wrap">{msg.content}</p>
                  ) : (
                    <div className="flex gap-1 items-center">
                      <span
                        className="w-2 h-2 rounded-full bg-text-tertiary animate-bounce"
                        style={{ animationDelay: '0ms' }}
                      />
                      <span
                        className="w-2 h-2 rounded-full bg-text-tertiary animate-bounce"
                        style={{ animationDelay: '150ms' }}
                      />
                      <span
                        className="w-2 h-2 rounded-full bg-text-tertiary animate-bounce"
                        style={{ animationDelay: '300ms' }}
                      />
                    </div>
                  )}
                </div>
              </div>
            ))}

            {loading && !streaming && (
              <div className="flex justify-start">
                <div className="bg-bg-tertiary rounded-lg px-4 py-3">
                  <div className="flex gap-1 items-center">
                    <span
                      className="w-2 h-2 rounded-full bg-text-tertiary animate-bounce"
                      style={{ animationDelay: '0ms' }}
                    />
                    <span
                      className="w-2 h-2 rounded-full bg-text-tertiary animate-bounce"
                      style={{ animationDelay: '150ms' }}
                    />
                    <span
                      className="w-2 h-2 rounded-full bg-text-tertiary animate-bounce"
                      style={{ animationDelay: '300ms' }}
                    />
                  </div>
                </div>
              </div>
            )}

            <div ref={chatEndRef} />
          </div>

          {/* Usage bar + Clear */}
          <div className="shrink-0 border-t border-border px-4 py-2 flex items-center justify-between gap-4">
            <span className="text-xs text-text-tertiary truncate">
              {lastUsage !== null ? (
                <>
                  {lastUsage.prompt_tokens.toLocaleString()} prompt
                  {' + '}
                  {lastUsage.completion_tokens.toLocaleString()} completion
                  {' = '}
                  {lastUsage.total_tokens.toLocaleString()} total
                  {' · '}
                  {lastUsage.duration.toFixed(1)}s
                </>
              ) : (
                <span className="opacity-0" aria-hidden="true">
                  &nbsp;
                </span>
              )}
            </span>
            <Button
              variant="ghost"
              size="sm"
              onClick={handleClear}
              disabled={chatHistory.length === 0}
            >
              Clear Chat
            </Button>
          </div>
        </div>
      </div>
    </>
  )
}
