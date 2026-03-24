import { useEffect, useId, useRef, useState } from 'react'
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

interface AvailableModel {
  name: string
  type: string
}

interface AvailableModelsResponse {
  models: AvailableModel[]
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
const DEFAULT_TEMPERATURE = 0.7
const DEFAULT_MAX_TOKENS = 4096

// ---------------------------------------------------------------------------
// Simple markdown renderer: splits on ``` code fences, no external library
// ---------------------------------------------------------------------------
function AssistantMessageContent({ content }: { content: string }) {
  if (!content) {
    // Typing indicator while empty assistant message awaits first delta
    return (
      <div className="flex gap-1 items-center py-0.5">
        <span
          className="w-2 h-2 rounded-full bg-accent animate-bounce"
          style={{ animationDelay: '0ms' }}
        />
        <span
          className="w-2 h-2 rounded-full bg-accent animate-bounce"
          style={{ animationDelay: '150ms' }}
        />
        <span
          className="w-2 h-2 rounded-full bg-accent animate-bounce"
          style={{ animationDelay: '300ms' }}
        />
      </div>
    )
  }

  // Split on triple-backtick fences (optionally with language hint)
  const parts = content.split(/(```[\s\S]*?```)/g)

  return (
    <div className="space-y-2">
      {parts.map((part, idx) => {
        if (part.startsWith('```')) {
          // Strip leading ``` + optional language tag and trailing ```
          const inner = part.replace(/^```[^\n]*\n?/, '').replace(/```$/, '')
          return (
            <pre
              key={idx}
              className="bg-bg-primary rounded-lg p-4 font-mono text-xs border border-border overflow-x-auto leading-relaxed"
            >
              {inner}
            </pre>
          )
        }
        return part ? (
          <p key={idx} className="whitespace-pre-wrap leading-relaxed">
            {part}
          </p>
        ) : null
      })}
    </div>
  )
}

// ---------------------------------------------------------------------------
// Label used above controls in the config panel
// ---------------------------------------------------------------------------
function ConfigLabel({ htmlFor, children }: { htmlFor?: string; children: React.ReactNode }) {
  return (
    <label
      htmlFor={htmlFor}
      className="block text-[10px] font-medium tracking-widest uppercase text-text-tertiary mb-1.5"
    >
      {children}
    </label>
  )
}

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
  const [temperature, setTemperature] = useState(DEFAULT_TEMPERATURE)
  const [maxTokens, setMaxTokens] = useState(DEFAULT_MAX_TOKENS)

  const chatEndRef = useRef<HTMLDivElement>(null)
  const abortRef = useRef<AbortController | null>(null)
  const textareaRef = useRef<HTMLTextAreaElement>(null)

  const tempLabelId = useId()
  const maxTokensLabelId = useId()

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

  // Auto-select the first chat/completion model when the list loads
  useEffect(() => {
    if (modelsData?.models && modelsData.models.length > 0 && model === '') {
      const chatModels = modelsData.models.filter(
        (m) => m.type === 'chat' || m.type === 'completion',
      )
      const first = chatModels[0]
      if (first) setModel(first.name)
    }
  }, [modelsData, model])

  // Scroll to bottom when new messages arrive
  useEffect(() => {
    chatEndRef.current?.scrollIntoView({ behavior: 'smooth' })
  }, [chatHistory, loading])

  const chatModels = (modelsData?.models ?? []).filter(
    (m) => m.type === 'chat' || m.type === 'completion',
  )

  const modelOptions: SelectOption[] = chatModels.map((m) => ({
    value: m.name,
    label: m.name,
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
        body: JSON.stringify({
          model,
          messages,
          stream: streaming,
          temperature,
          max_tokens: maxTokens,
        }),
        signal: controller.signal,
      })

      if (res.status === 401) {
        // Show error instead of logging out — the 401 may come from an
        // upstream provider (no API key configured for the model), not
        // from the proxy rejecting our session.
        setError('Authentication failed. Check that the model has a valid upstream API key configured.')
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
    <div className="flex flex-col overflow-hidden" style={{ height: 'calc(100vh - 4rem)' }}>
      <div className="shrink-0 mb-4">
        <PageHeader title="Playground" description="Test models interactively" />
      </div>

      <div className="flex gap-4 flex-1 min-h-0">

        {/* ================================================================
            LEFT PANEL — 35% — Configuration
        ================================================================ */}
        <div className="w-[32%] shrink-0 flex flex-col rounded-xl border border-border bg-bg-secondary overflow-hidden">
          {/* Panel header */}
          <div className="shrink-0 flex items-center gap-2.5 px-5 py-4 border-b border-border">
            <svg
              className="h-4 w-4 text-text-tertiary shrink-0"
              fill="none"
              viewBox="0 0 24 24"
              stroke="currentColor"
              strokeWidth={1.75}
              aria-hidden="true"
            >
              <path
                strokeLinecap="round"
                strokeLinejoin="round"
                d="M10.325 4.317c.426-1.756 2.924-1.756 3.35 0a1.724 1.724 0 002.573 1.066c1.543-.94 3.31.826 2.37 2.37a1.724 1.724 0 001.065 2.572c1.756.426 1.756 2.924 0 3.35a1.724 1.724 0 00-1.066 2.573c.94 1.543-.826 3.31-2.37 2.37a1.724 1.724 0 00-2.572 1.065c-.426 1.756-2.924 1.756-3.35 0a1.724 1.724 0 00-2.573-1.066c-1.543.94-3.31-.826-2.37-2.37a1.724 1.724 0 00-1.065-2.572c-1.756-.426-1.756-2.924 0-3.35a1.724 1.724 0 001.066-2.573c-.94-1.543.826-3.31 2.37-2.37.996.608 2.296.07 2.572-1.065z"
              />
              <path
                strokeLinecap="round"
                strokeLinejoin="round"
                d="M15 12a3 3 0 11-6 0 3 3 0 016 0z"
              />
            </svg>
            <span className="text-sm font-medium text-text-primary">Configuration</span>
          </div>

          {/* Scrollable config body */}
          <div className="flex-1 overflow-y-auto px-5 py-5 space-y-5">

            {/* Model selector */}
            <div>
              <ConfigLabel>Model</ConfigLabel>
              <Select
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
            </div>

            {/* Streaming toggle */}
            <div className="flex items-center justify-between">
              <ConfigLabel>Stream response</ConfigLabel>
              <Toggle
                checked={streaming}
                onChange={setStreaming}
                size="sm"
              />
            </div>

            {/* System prompt */}
            <div>
              <ConfigLabel>System prompt</ConfigLabel>
              <Textarea
                value={systemPrompt}
                onChange={(e) => setSystemPrompt(e.target.value)}
                placeholder="You are a helpful assistant."
                rows={4}
                className="font-mono text-xs"
                disabled={loading}
              />
            </div>

            {/* Advanced Parameters collapsible */}
            <details className="group">
              <summary className="flex items-center justify-between cursor-pointer list-none select-none">
                <span className="text-[10px] font-medium tracking-widest uppercase text-text-tertiary">
                  Advanced parameters
                </span>
                <svg
                  className="h-3.5 w-3.5 text-text-tertiary transition-transform duration-200 group-open:rotate-180"
                  fill="none"
                  viewBox="0 0 24 24"
                  stroke="currentColor"
                  strokeWidth={2}
                  aria-hidden="true"
                >
                  <path strokeLinecap="round" strokeLinejoin="round" d="M19 9l-7 7-7-7" />
                </svg>
              </summary>

              <div className="mt-4 space-y-5">

                {/* Temperature */}
                <div>
                  <div className="flex items-center justify-between mb-1.5">
                    <label
                      id={tempLabelId}
                      className="text-[10px] font-medium tracking-widest uppercase text-text-tertiary"
                    >
                      Temperature
                    </label>
                    <span className="px-2 py-0.5 rounded-full bg-accent/15 text-accent text-xs font-mono">
                      {temperature.toFixed(1)}
                    </span>
                  </div>
                  <input
                    type="range"
                    min={0}
                    max={2}
                    step={0.1}
                    value={temperature}
                    aria-labelledby={tempLabelId}
                    onChange={(e) => setTemperature(parseFloat(e.target.value))}
                    className="w-full h-1.5 rounded-full appearance-none bg-bg-tertiary cursor-pointer accent-accent"
                  />
                  <div className="flex justify-between mt-1">
                    <span className="text-[10px] text-text-tertiary">0</span>
                    <span className="text-[10px] text-text-tertiary">2</span>
                  </div>
                </div>

                {/* Max Tokens */}
                <div>
                  <label
                    id={maxTokensLabelId}
                    className="block text-[10px] font-medium tracking-widest uppercase text-text-tertiary mb-1.5"
                  >
                    Max tokens
                  </label>
                  <input
                    type="number"
                    min={1}
                    max={128000}
                    value={maxTokens}
                    aria-labelledby={maxTokensLabelId}
                    onChange={(e) => {
                      const v = parseInt(e.target.value, 10)
                      if (!isNaN(v) && v > 0) setMaxTokens(v)
                    }}
                    className={cn(
                      'block w-full rounded-md border border-border bg-bg-tertiary px-3 py-2 text-sm text-text-primary placeholder:text-text-tertiary',
                      'transition-colors duration-150',
                      'focus:outline-none focus:border-accent focus:ring-2 focus:ring-accent/40',
                    )}
                  />
                </div>

                {/* API Key override — moved here for power users */}
                <Input
                  label="API key override"
                  type="password"
                  value={apiKey}
                  onChange={(e) => setApiKey(e.target.value)}
                  placeholder="Session key (default)"
                  description="Leave empty to use your session key."
                />

              </div>
            </details>

          </div>
        </div>

        {/* ================================================================
            RIGHT PANEL — 65% — Chat
        ================================================================ */}
        <div className="flex-1 flex flex-col rounded-xl border border-border bg-bg-secondary min-h-0 overflow-hidden">

          {/* Top bar */}
          <div className="shrink-0 flex items-center justify-between px-5 py-3.5 border-b border-border">
            <div className="flex items-center gap-3">
              {model ? (
                <span className="px-2.5 py-1 rounded-full bg-accent/15 border border-accent/20 text-accent text-xs font-medium truncate max-w-[220px]">
                  {model}
                </span>
              ) : (
                <span className="px-2.5 py-1 rounded-full bg-bg-tertiary border border-border text-text-tertiary text-xs">
                  No model selected
                </span>
              )}
              <div className="flex items-center gap-1.5">
                <span className="relative flex h-2 w-2">
                  <span className="animate-ping absolute inline-flex h-full w-full rounded-full bg-success opacity-75" />
                  <span className="relative inline-flex rounded-full h-2 w-2 bg-success" />
                </span>
                <span className="text-xs text-text-tertiary">Ready</span>
              </div>
            </div>
            <Button
              variant="ghost"
              size="sm"
              onClick={handleClear}
              disabled={chatHistory.length === 0}
            >
              Clear chat
            </Button>
          </div>

          {/* Chat messages — scrollable */}
          <div className="flex-1 min-h-0 overflow-y-auto px-5 py-5 space-y-5">
            {chatHistory.length === 0 && !loading && (
              <div className="flex items-center justify-center py-20">
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
                {msg.role === 'assistant' && (
                  <div className="shrink-0 w-8 h-8 rounded-lg bg-bg-tertiary border border-border flex items-center justify-center mt-0.5">
                    <svg
                      className="h-4 w-4 text-accent"
                      fill="none"
                      viewBox="0 0 24 24"
                      stroke="currentColor"
                      strokeWidth={1.75}
                      aria-hidden="true"
                    >
                      <path
                        strokeLinecap="round"
                        strokeLinejoin="round"
                        d="M9.75 3.104v5.714a2.25 2.25 0 01-.659 1.591L5 14.5M9.75 3.104c-.251.023-.501.05-.75.082m.75-.082a24.301 24.301 0 014.5 0m0 0v5.714c0 .597.237 1.17.659 1.591L19.8 15.3M14.25 3.104c.251.023.501.05.75.082M19.8 15.3l-1.57.393A9.065 9.065 0 0112 15a9.065 9.065 0 00-6.23-.693L5 14.5m14.8.8l1.402 1.402c1 1 .03 2.798-1.402 2.798H4.2c-1.432 0-2.402-1.799-1.402-2.798L4.2 15.3"
                      />
                    </svg>
                  </div>
                )}

                <div
                  className={cn(
                    'px-5 py-4 text-sm leading-relaxed max-w-[80%]',
                    msg.role === 'user'
                      ? 'bg-accent/10 border border-accent/20 rounded-2xl rounded-tr-sm text-text-primary'
                      : 'bg-bg-secondary border border-border rounded-2xl rounded-tl-sm text-text-primary',
                  )}
                >
                  {msg.role === 'user' ? (
                    <p className="whitespace-pre-wrap">{msg.content}</p>
                  ) : (
                    <AssistantMessageContent content={msg.content} />
                  )}
                </div>
              </div>
            ))}

            {/* Non-streaming loading indicator */}
            {loading && !streaming && (
              <div className="flex gap-3 justify-start">
                <div className="shrink-0 w-8 h-8 rounded-lg bg-bg-tertiary border border-border flex items-center justify-center">
                  <svg
                    className="h-4 w-4 text-accent"
                    fill="none"
                    viewBox="0 0 24 24"
                    stroke="currentColor"
                    strokeWidth={1.75}
                    aria-hidden="true"
                  >
                    <path
                      strokeLinecap="round"
                      strokeLinejoin="round"
                      d="M9.75 3.104v5.714a2.25 2.25 0 01-.659 1.591L5 14.5M9.75 3.104c-.251.023-.501.05-.75.082m.75-.082a24.301 24.301 0 014.5 0m0 0v5.714c0 .597.237 1.17.659 1.591L19.8 15.3M14.25 3.104c.251.023.501.05.75.082M19.8 15.3l-1.57.393A9.065 9.065 0 0112 15a9.065 9.065 0 00-6.23-.693L5 14.5m14.8.8l1.402 1.402c1 1 .03 2.798-1.402 2.798H4.2c-1.432 0-2.402-1.799-1.402-2.798L4.2 15.3"
                    />
                  </svg>
                </div>
                <div className="px-5 py-4 bg-bg-secondary border border-border rounded-2xl rounded-tl-sm">
                  <div className="flex gap-1 items-center">
                    <span
                      className="w-2 h-2 rounded-full bg-accent animate-bounce"
                      style={{ animationDelay: '0ms' }}
                    />
                    <span
                      className="w-2 h-2 rounded-full bg-accent animate-bounce"
                      style={{ animationDelay: '150ms' }}
                    />
                    <span
                      className="w-2 h-2 rounded-full bg-accent animate-bounce"
                      style={{ animationDelay: '300ms' }}
                    />
                  </div>
                </div>
              </div>
            )}

            <div ref={chatEndRef} />
          </div>

          {/* Input area — sticky bottom */}
          <div className="shrink-0 border-t border-border px-5 py-4">

            {/* Error banner */}
            {error !== null && (
              <div
                role="alert"
                className="mb-3 rounded-lg border border-error/40 bg-error/10 px-4 py-3 text-sm text-error"
              >
                {error}
              </div>
            )}

            <div className="rounded-xl border border-border bg-bg-tertiary focus-within:border-accent/60 focus-within:ring-1 focus-within:ring-accent/30 transition-colors duration-150">
              <textarea
                ref={textareaRef}
                value={message}
                onChange={(e) => setMessage(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter' && !e.shiftKey) {
                    e.preventDefault()
                    void handleSend()
                  }
                }}
                placeholder="Type your message..."
                rows={3}
                disabled={loading}
                className={cn(
                  'block w-full bg-transparent px-4 pt-4 pb-2 text-sm text-text-primary placeholder:text-text-tertiary resize-none',
                  'focus:outline-none',
                  loading && 'opacity-50 cursor-not-allowed',
                )}
              />
              <div className="flex items-center justify-between px-4 pb-3 pt-1">
                <span className="text-xs text-text-tertiary">
                  Enter to send · Shift+Enter for new line
                </span>
                <button
                  type="button"
                  disabled={!canSend}
                  onClick={() => void handleSend()}
                  className={cn(
                    'flex items-center justify-center w-8 h-8 rounded-lg transition-all duration-150',
                    'focus:outline-none focus:ring-2 focus:ring-accent focus:ring-offset-2 focus:ring-offset-bg-tertiary',
                    canSend
                      ? 'bg-gradient-to-br from-[#6366f1] via-[#8b5cf6] to-[#a855f7] text-white hover:brightness-110 hover:shadow-[0_0_16px_rgba(139,92,246,0.5)] cursor-pointer'
                      : 'bg-bg-secondary border border-border text-text-tertiary cursor-not-allowed opacity-50',
                  )}
                  aria-label="Send message"
                >
                  {loading ? (
                    <span
                      role="status"
                      aria-label="Loading"
                      className="inline-block h-3.5 w-3.5 animate-spin rounded-full border-2 border-white border-t-transparent"
                    />
                  ) : (
                    <svg
                      className="h-4 w-4"
                      fill="none"
                      viewBox="0 0 24 24"
                      stroke="currentColor"
                      strokeWidth={2.5}
                      aria-hidden="true"
                    >
                      <path strokeLinecap="round" strokeLinejoin="round" d="M5 15l7-7 7 7" />
                    </svg>
                  )}
                </button>
              </div>
            </div>
            {/* Usage stats — below input */}
            {lastUsage !== null && (
              <div className="flex items-center gap-2.5 px-2 pt-2">
                <svg
                  className="h-3.5 w-3.5 shrink-0 text-accent"
                  fill="currentColor"
                  viewBox="0 0 24 24"
                  aria-hidden="true"
                >
                  <path d="M13 2L3 14h9l-1 8 10-12h-9l1-8z" />
                </svg>
                <span className="font-mono text-xs text-text-tertiary">
                  {lastUsage.prompt_tokens.toLocaleString()} prompt
                  {' + '}
                  {lastUsage.completion_tokens.toLocaleString()} completion
                  {' = '}
                  {lastUsage.total_tokens.toLocaleString()} total
                  {' · '}
                  {lastUsage.duration.toFixed(1)}s
                </span>
              </div>
            )}
          </div>
        </div>

      </div>
    </div>
  )
}
