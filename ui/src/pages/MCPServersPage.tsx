import React, { useState, useMemo } from 'react'
import { PageHeader } from '../components/ui/PageHeader'
import { Table } from '../components/ui/Table'
import type { Column } from '../components/ui/Table'
import { Dialog, ConfirmDialog } from '../components/ui/Dialog'
import { Badge } from '../components/ui/Badge'
import { Button } from '../components/ui/Button'
import { Input } from '../components/ui/Input'
import { Select } from '../components/ui/Select'
import TabSwitcher from '../components/ui/TabSwitcher'
import { Toggle } from '../components/ui/Toggle'
import { StatCard } from '../components/ui/StatCard'
import {
  useOrgMCPServers,
  useUpdateMCPServer,
  useDeleteMCPServer,
  useToggleMCPServer,
  useTestMCPServer,
  useAddBlocklistEntry,
  useRemoveBlocklistEntry,
  useRefreshMCPServerTools,
  useMCPServerTools,
} from '../hooks/useMCPServers'
import type { MCPServerResponse, CreateMCPServerParams, UpdateMCPServerParams } from '../hooks/useMCPServers'
import { useMCPServerHealth } from '../hooks/useMCPServerHealth'
import type { MCPServerHealth } from '../hooks/useMCPServerHealth'
import { useMe } from '../hooks/useMe'
import { useTeams } from '../hooks/useTeams'
import { useToast } from '../hooks/useToast'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import apiClient from '../api/client'

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const AUTH_TYPE_OPTIONS = [
  { value: 'none', label: 'None' },
  { value: 'bearer', label: 'Bearer Token' },
  { value: 'header', label: 'Custom Header' },
]

// ---------------------------------------------------------------------------
// Icons
// ---------------------------------------------------------------------------

function IconServer() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <rect x="2" y="2" width="20" height="8" rx="2" ry="2" />
      <rect x="2" y="14" width="20" height="8" rx="2" ry="2" />
      <line x1="6" y1="6" x2="6.01" y2="6" />
      <line x1="6" y1="18" x2="6.01" y2="18" />
    </svg>
  )
}

function IconActivity() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <polyline points="22 12 18 12 15 21 9 3 6 12 2 12" />
    </svg>
  )
}

function IconPauseCircle() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <circle cx="12" cy="12" r="10" />
      <line x1="10" y1="15" x2="10" y2="9" />
      <line x1="14" y1="15" x2="14" y2="9" />
    </svg>
  )
}

function IconPencil() {
  return (
    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7" />
      <path d="M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z" />
    </svg>
  )
}

function IconTrash() {
  return (
    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <polyline points="3 6 5 6 21 6" />
      <path d="M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6" />
      <path d="M10 11v6" />
      <path d="M14 11v6" />
      <path d="M9 6V4a1 1 0 0 1 1-1h4a1 1 0 0 1 1 1v2" />
    </svg>
  )
}

function IconPlug() {
  return (
    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M12 22v-5" />
      <path d="M9 7V2" />
      <path d="M15 7V2" />
      <path d="M6 7h12" />
      <path d="M6 7v4a6 6 0 0 0 12 0V7" />
    </svg>
  )
}

function IconRefresh() {
  return (
    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <polyline points="23 4 23 10 17 10" />
      <polyline points="1 20 1 14 7 14" />
      <path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15" />
    </svg>
  )
}

function IconHealthDot() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M22 12h-4l-3 9L9 3l-3 9H2" />
    </svg>
  )
}

function IconCopy() {
  return (
    <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <rect x="9" y="9" width="13" height="13" rx="2" ry="2" />
      <path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1" />
    </svg>
  )
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function authBadgeVariant(authType: string): 'muted' | 'default' | 'info' {
  if (authType === 'none') return 'muted'
  if (authType === 'bearer') return 'default'
  return 'info'
}

function authLabel(authType: string): string {
  if (authType === 'none') return 'None'
  if (authType === 'bearer') return 'Bearer'
  if (authType === 'header') return 'Header'
  return authType
}

function scopeBadgeVariant(scope: string): 'default' | 'info' | 'success' {
  if (scope === 'global') return 'default'
  if (scope === 'org') return 'info'
  return 'success'
}

function scopeLabel(scope: string): string {
  if (scope === 'global') return 'Global'
  if (scope === 'org') return 'Org'
  if (scope === 'team') return 'Team'
  return scope
}

function sourceBadgeVariant(source: string): 'default' | 'muted' | 'warning' {
  if (source === 'yaml') return 'warning'
  if (source === 'builtin') return 'default'
  return 'muted'
}

function sourceLabel(source: string): string {
  if (source === 'yaml') return 'YAML'
  if (source === 'builtin') return 'Built-in'
  return 'API'
}

// ---------------------------------------------------------------------------
// HealthIndicator
// ---------------------------------------------------------------------------

function formatRelativeTime(isoString: string): string {
  const diffMs = Date.now() - new Date(isoString).getTime()
  const diffS = Math.floor(diffMs / 1000)
  if (diffS < 60) return `${diffS}s ago`
  const diffM = Math.floor(diffS / 60)
  if (diffM < 60) return `${diffM}m ago`
  const diffH = Math.floor(diffM / 60)
  return `${diffH}h ago`
}

interface HealthIndicatorProps {
  health: MCPServerHealth | undefined
}

function HealthIndicator({ health }: HealthIndicatorProps) {
  if (health === undefined) {
    return (
      <div className="flex items-center gap-1.5">
        <span
          className="w-2 h-2 rounded-full shrink-0"
          style={{ backgroundColor: '#6b7280' }}
          aria-hidden="true"
        />
        <span className="text-text-tertiary text-sm">...</span>
      </div>
    )
  }

  const dotColor =
    health.status === 'healthy'
      ? '#22c55e'
      : health.status === 'unhealthy'
      ? '#ef4444'
      : '#6b7280'

  const label =
    health.status === 'healthy'
      ? 'Healthy'
      : health.status === 'unhealthy'
      ? 'Unhealthy'
      : '...'

  const tooltipParts: string[] = []
  if (health.last_check) tooltipParts.push(`Last check: ${formatRelativeTime(health.last_check)}`)
  if (health.latency_ms > 0) tooltipParts.push(`Latency: ${health.latency_ms}ms`)
  if (health.tool_count > 0) tooltipParts.push(`Tools: ${health.tool_count}`)
  if (health.last_error) tooltipParts.push(`Error: ${health.last_error}`)
  const tooltip = tooltipParts.join('\n')

  return (
    <div
      className="flex items-center gap-1.5"
      title={tooltip || undefined}
    >
      <span
        className="w-2 h-2 rounded-full shrink-0"
        style={{ backgroundColor: dotColor }}
        aria-hidden="true"
      />
      <span
        className="text-sm"
        style={{ color: health.status === 'healthy' ? '#a1afc4' : health.status === 'unhealthy' ? '#ef4444' : '#8494a8' }}
      >
        {label}
      </span>
      {health.status === 'healthy' && health.latency_ms > 0 && (
        <span className="text-text-tertiary text-xs tabular-nums">{health.latency_ms}ms</span>
      )}
    </div>
  )
}

// ---------------------------------------------------------------------------
// CreateMCPServerDialog
// ---------------------------------------------------------------------------

interface CreateMCPServerDialogProps {
  open: boolean
  onClose: () => void
  orgId: string
  isSystemAdmin: boolean
  isOrgAdmin: boolean
}

interface CreateFormErrors {
  name?: string
  alias?: string
  url?: string
  auth_header?: string
  team?: string
}

function CreateMCPServerDialog({
  open,
  onClose,
  orgId,
  isSystemAdmin,
  isOrgAdmin,
}: CreateMCPServerDialogProps) {
  // Determine which scope options this user can choose
  const scopeOptions = isSystemAdmin
    ? [
        { value: 'global', label: 'Global' },
        { value: 'org', label: 'Organization' },
        { value: 'team', label: 'Team' },
      ]
    : isOrgAdmin
    ? [
        { value: 'org', label: 'Organization' },
        { value: 'team', label: 'Team' },
      ]
    : [{ value: 'team', label: 'Team' }]

  const defaultScope = scopeOptions[0].value

  const [scope, setScope] = useState(defaultScope)
  const [teamId, setTeamId] = useState('')
  const [name, setName] = useState('')
  const [alias, setAlias] = useState('')
  const [url, setUrl] = useState('')
  const [authType, setAuthType] = useState('none')
  const [authToken, setAuthToken] = useState('')
  const [authHeader, setAuthHeader] = useState('')
  const [errors, setErrors] = useState<CreateFormErrors>({})

  const { data: teams } = useTeams(orgId)
  const queryClient = useQueryClient()
  const { toast } = useToast()

  const teamOptions = teams?.data?.map((t) => ({ value: t.id, label: t.name })) ?? []

  const [isPending, setIsPending] = useState(false)

  function handleClose() {
    setScope(defaultScope)
    setTeamId('')
    setName('')
    setAlias('')
    setUrl('')
    setAuthType('none')
    setAuthToken('')
    setAuthHeader('')
    setErrors({})
    onClose()
  }

  function validate(): boolean {
    const next: CreateFormErrors = {}
    if (!name.trim()) next.name = 'Name is required'
    if (!alias.trim()) next.alias = 'Alias is required'
    if (!url.trim()) next.url = 'URL is required'
    if (authType === 'header' && !authHeader.trim()) {
      next.auth_header = 'Header name is required for custom header auth'
    }
    if (scope === 'team' && !teamId) {
      next.team = 'Team is required'
    }
    setErrors(next)
    return Object.keys(next).length === 0
  }

  function handleSubmit(e: React.MouseEvent) {
    e.preventDefault()
    if (!validate()) return

    const params: CreateMCPServerParams = {
      name: name.trim(),
      alias: alias.trim(),
      url: url.trim(),
      auth_type: authType,
    }
    if (authType !== 'none' && authToken.trim()) {
      params.auth_token = authToken.trim()
    }
    if (authType === 'header' && authHeader.trim()) {
      params.auth_header = authHeader.trim()
    }

    let endpoint: string
    if (scope === 'global') {
      endpoint = '/mcp-servers'
    } else if (scope === 'org') {
      if (!orgId) { toast({ variant: 'error', message: 'Organization context required' }); return }
      endpoint = `/orgs/${orgId}/mcp-servers`
    } else {
      if (!orgId || !teamId) { toast({ variant: 'error', message: 'Organization and team required' }); return }
      endpoint = `/orgs/${orgId}/teams/${teamId}/mcp-servers`
    }

    setIsPending(true)
    apiClient<MCPServerResponse>(endpoint, {
      method: 'POST',
      body: JSON.stringify(params),
    })
      .then(() => {
        queryClient.invalidateQueries({ queryKey: ['mcp-servers'] })
        toast({ variant: 'success', message: 'MCP server added' })
        handleClose()
      })
      .catch((err: unknown) => {
        toast({
          variant: 'error',
          message: err instanceof Error ? err.message : 'Failed to add MCP server',
        })
      })
      .finally(() => {
        setIsPending(false)
      })
  }

  return (
    <Dialog open={open} onClose={handleClose} title="Add MCP Server">
      <div className="space-y-4">
        {scopeOptions.length > 1 && (
          <Select
            label="Scope"
            options={scopeOptions}
            value={scope}
            onChange={(val) => {
              setScope(val)
              setTeamId('')
              setErrors((prev) => ({ ...prev, team: undefined }))
            }}
            disabled={isPending}
          />
        )}
        {scope === 'team' && (
          <Select
            label="Team"
            options={teamOptions}
            value={teamId}
            onChange={(val) => {
              setTeamId(val)
              if (val) setErrors((prev) => ({ ...prev, team: undefined }))
            }}
            placeholder="Select a team..."
            error={errors.team}
            disabled={isPending}
          />
        )}
        {scope === 'team' && teamOptions.length === 0 && (
          <p className="text-xs text-text-tertiary -mt-2">
            No teams found. Create a team first before adding a team-scoped server.
          </p>
        )}
        <Input
          label="Name"
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="e.g. GitHub MCP"
          error={errors.name}
          disabled={isPending}
        />
        <Input
          label="Alias"
          value={alias}
          onChange={(e) => setAlias(e.target.value)}
          placeholder="my-github-mcp"
          description="Used to reference this server in tool configurations. Must be unique."
          error={errors.alias}
          disabled={isPending}
        />
        <Input
          label="URL"
          value={url}
          onChange={(e) => setUrl(e.target.value)}
          placeholder="https://mcp.example.com/sse"
          error={errors.url}
          disabled={isPending}
        />
        <div>
          <label className="block text-xs font-medium tracking-wider uppercase text-text-tertiary mb-2">Auth Type</label>
          <TabSwitcher
            tabs={AUTH_TYPE_OPTIONS.map(o => ({ key: o.value, label: o.label }))}
            activeKey={authType}
            onChange={setAuthType}
            className="mb-0"
          />
        </div>
        {authType !== 'none' && (
          <Input
            label="Auth Token"
            type="password"
            value={authToken}
            onChange={(e) => setAuthToken(e.target.value)}
            placeholder="Encrypted at rest, never shown again"
            disabled={isPending}
          />
        )}
        {authType === 'header' && (
          <Input
            label="Header Name"
            value={authHeader}
            onChange={(e) => setAuthHeader(e.target.value)}
            placeholder="X-API-Key"
            error={errors.auth_header}
            disabled={isPending}
          />
        )}
        <div className="flex justify-end gap-2 pt-2">
          <Button variant="secondary" onClick={handleClose} disabled={isPending}>
            Cancel
          </Button>
          <Button onClick={handleSubmit} loading={isPending}>
            Add Server
          </Button>
        </div>
      </div>
    </Dialog>
  )
}

// ---------------------------------------------------------------------------
// EditMCPServerDialog
// ---------------------------------------------------------------------------

interface EditMCPServerDialogProps {
  server: MCPServerResponse
  onClose: () => void
}

interface EditFormErrors {
  name?: string
  alias?: string
  url?: string
  auth_header?: string
}

function EditMCPServerDialog({ server, onClose }: EditMCPServerDialogProps) {
  const [name, setName] = useState(server.name)
  const [alias, setAlias] = useState(server.alias)
  const [url, setUrl] = useState(server.url)
  const [authType, setAuthType] = useState(server.auth_type)
  const [authToken, setAuthToken] = useState('')
  const [authHeader, setAuthHeader] = useState(server.auth_header ?? '')
  const [errors, setErrors] = useState<EditFormErrors>({})

  const updateMCPServer = useUpdateMCPServer()
  const { toast } = useToast()

  const isPending = updateMCPServer.isPending

  function validate(): boolean {
    const next: EditFormErrors = {}
    if (!name.trim()) next.name = 'Name is required'
    if (!alias.trim()) next.alias = 'Alias is required'
    if (!url.trim()) next.url = 'URL is required'
    if (authType === 'header' && !authHeader.trim()) {
      next.auth_header = 'Header name is required for custom header auth'
    }
    setErrors(next)
    return Object.keys(next).length === 0
  }

  function handleSubmit(e: React.MouseEvent) {
    e.preventDefault()
    if (!validate()) return

    const params: UpdateMCPServerParams = {}

    if (name.trim() !== server.name) params.name = name.trim()
    if (alias.trim() !== server.alias) params.alias = alias.trim()
    if (url.trim() !== server.url) params.url = url.trim()
    if (authType !== server.auth_type) params.auth_type = authType
    if (authToken.trim()) params.auth_token = authToken.trim()
    if (authHeader.trim() !== (server.auth_header ?? '')) {
      params.auth_header = authHeader.trim() || undefined
    }

    if (Object.keys(params).length === 0) {
      onClose()
      return
    }

    updateMCPServer.mutate(
      { serverId: server.id, params },
      {
        onSuccess: () => {
          toast({ variant: 'success', message: 'MCP server updated' })
          onClose()
        },
        onError: (err) => {
          toast({
            variant: 'error',
            message: err instanceof Error ? err.message : 'Failed to update MCP server',
          })
        },
      },
    )
  }

  return (
    <Dialog open onClose={onClose} title="Edit MCP Server">
      <div className="space-y-4">
        <Input
          label="Name"
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="e.g. GitHub MCP"
          error={errors.name}
          disabled={isPending}
        />
        <Input
          label="Alias"
          value={alias}
          onChange={(e) => setAlias(e.target.value)}
          placeholder="my-github-mcp"
          error={errors.alias}
          disabled={isPending}
        />
        <Input
          label="URL"
          value={url}
          onChange={(e) => setUrl(e.target.value)}
          placeholder="https://mcp.example.com/sse"
          error={errors.url}
          disabled={isPending}
        />
        <div>
          <label className="block text-xs font-medium tracking-wider uppercase text-text-tertiary mb-2">Auth Type</label>
          <TabSwitcher
            tabs={AUTH_TYPE_OPTIONS.map(o => ({ key: o.value, label: o.label }))}
            activeKey={authType}
            onChange={setAuthType}
            className="mb-0"
          />
        </div>
        {authType !== 'none' && (
          <Input
            label="Auth Token"
            type="password"
            value={authToken}
            onChange={(e) => setAuthToken(e.target.value)}
            placeholder="Leave empty to keep current"
            description="Leave empty to keep current token. Enter a new value to replace."
            disabled={isPending}
          />
        )}
        {authType === 'header' && (
          <Input
            label="Header Name"
            value={authHeader}
            onChange={(e) => setAuthHeader(e.target.value)}
            placeholder="X-API-Key"
            error={errors.auth_header}
            disabled={isPending}
          />
        )}
        <div className="flex justify-end gap-2 pt-2">
          <Button variant="secondary" onClick={onClose} disabled={isPending}>
            Cancel
          </Button>
          <Button onClick={handleSubmit} loading={isPending}>
            Save Changes
          </Button>
        </div>
      </div>
    </Dialog>
  )
}

// ---------------------------------------------------------------------------
// ClientConfigSnippet
// ---------------------------------------------------------------------------

interface ClientConfigSnippetProps {
  label: string | null
  config: string
  onCopy: () => void
}

function ClientConfigSnippet({ label, config, onCopy }: ClientConfigSnippetProps) {
  function handleCopy() {
    navigator.clipboard.writeText(config).then(onCopy).catch(() => {
      /* clipboard unavailable — fail silently */
    })
  }

  return (
    <div className="space-y-1">
      {label !== null && (
        <span className="text-xs text-text-tertiary">{label}</span>
      )}
      <div className="relative group">
        <pre className="bg-bg-tertiary rounded-md px-3 py-2.5 text-xs font-mono text-text-secondary overflow-x-auto leading-relaxed whitespace-pre">
          {config}
        </pre>
        <button
          type="button"
          onClick={handleCopy}
          className="absolute top-2 right-2 p-1.5 rounded-md text-text-tertiary hover:text-text-primary hover:bg-bg-primary/60 transition-colors opacity-0 group-hover:opacity-100 focus:opacity-100"
          aria-label="Copy config to clipboard"
        >
          <IconCopy />
        </button>
      </div>
    </div>
  )
}

// ---------------------------------------------------------------------------
// ServerExpandedRow
// ---------------------------------------------------------------------------

interface ServerExpandedRowProps {
  server: MCPServerResponse
  canModify: boolean
  health?: MCPServerHealth
}

function truncateText(text: string, maxLength: number): string {
  if (text.length <= maxLength) return text
  return text.slice(0, maxLength) + '…'
}

function cn(...classes: (string | false | undefined | null)[]): string {
  return classes.filter(Boolean).join(' ')
}

function getBaseUrl(): string {
  const origin = window.location.origin
  if (origin.includes('localhost') || origin.includes('127.0.0.1')) {
    return 'https://your-voidllm-domain'
  }
  return origin
}

function generateMCPConfig(alias: string, isCodeMode?: boolean): string {
  const base = getBaseUrl()
  if (isCodeMode) {
    return JSON.stringify(
      {
        mcpServers: {
          'voidllm-code': {
            url: `${base}/api/v1/mcp`,
            headers: { Authorization: 'Bearer <your-api-key>' },
          },
        },
      },
      null,
      2,
    )
  }
  return JSON.stringify(
    {
      mcpServers: {
        [alias]: {
          url: `${base}/api/v1/mcp/${alias}`,
          headers: { Authorization: 'Bearer <your-api-key>' },
        },
      },
    },
    null,
    2,
  )
}

function IconChevronDown() {
  return (
    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <polyline points="6 9 12 15 18 9" />
    </svg>
  )
}

function ServerExpandedRow({ server, canModify, health }: ServerExpandedRowProps) {
  const [configOpen, setConfigOpen] = useState(false)
  const { data: tools, isLoading: toolsLoading } = useMCPServerTools(server.id)
  const addEntry = useAddBlocklistEntry()
  const removeEntry = useRemoveBlocklistEntry()
  const refreshTools = useRefreshMCPServerTools()
  const { toast } = useToast()

  function handleToggleTool(toolName: string, currentlyBlocked: boolean) {
    if (currentlyBlocked) {
      removeEntry.mutate(
        { serverId: server.id, toolName },
        {
          onSuccess: () => {
            toast({ variant: 'success', message: `Tool "${toolName}" unblocked` })
          },
          onError: (err) => {
            toast({
              variant: 'error',
              message: err instanceof Error ? err.message : 'Failed to unblock tool',
            })
          },
        },
      )
    } else {
      addEntry.mutate(
        { serverId: server.id, toolName },
        {
          onSuccess: () => {
            toast({ variant: 'success', message: `Tool "${toolName}" blocked` })
          },
          onError: (err) => {
            toast({
              variant: 'error',
              message: err instanceof Error ? err.message : 'Failed to block tool',
            })
          },
        },
      )
    }
  }

  function handleRefresh() {
    refreshTools.mutate(server.id, {
      onSuccess: (result) => {
        toast({ variant: 'success', message: `Tools refreshed — ${result.tool_count} tool${result.tool_count === 1 ? '' : 's'} found` })
      },
      onError: (err) => {
        toast({
          variant: 'error',
          message: err instanceof Error ? err.message : 'Failed to refresh tools',
        })
      },
    })
  }

  const toolList = tools ?? []

  function isToolPending(toolName: string): boolean {
    const addPending = addEntry.isPending && addEntry.variables?.toolName === toolName
    const removePending = removeEntry.isPending && removeEntry.variables?.toolName === toolName
    return addPending || removePending
  }

  function handleCopyConfig(isCodeMode?: boolean) {
    const config = generateMCPConfig(server.alias, isCodeMode)
    navigator.clipboard.writeText(config).then(() => {
      toast({ variant: 'success', message: 'Copied to clipboard' })
    }).catch(() => {})
  }

  return (
    <div className="px-6 py-4 space-y-3">
      {/* Health error banner */}
      {health?.status === 'unhealthy' && health.last_error && (
        <div className="flex items-start gap-2.5 px-3.5 py-2.5 rounded-lg bg-error/10 border border-error/20">
          <span className="w-2 h-2 rounded-full bg-error shrink-0 mt-1" aria-hidden="true" />
          <div className="min-w-0">
            <p className="text-xs font-medium text-error">Last error</p>
            <p className="text-xs text-error/80 mt-0.5 break-words">{health.last_error}</p>
          </div>
        </div>
      )}

      {/* Client Config — collapsible, above tools */}
      <div>
        <div className="flex items-center justify-between">
          <button
            type="button"
            onClick={() => setConfigOpen(!configOpen)}
            className="inline-flex items-center gap-1.5 text-xs font-medium uppercase tracking-wider text-text-tertiary hover:text-text-secondary transition-colors"
          >
            <span className={cn('transition-transform', configOpen && 'rotate-180')}>
              <IconChevronDown />
            </span>
            Client Config
          </button>
          <div className="flex items-center gap-1">
            {server.alias === 'voidllm' && (
              <button
                type="button"
                onClick={() => handleCopyConfig(true)}
                className="inline-flex items-center gap-1 px-2 py-1 rounded-md text-xs text-text-tertiary hover:text-text-primary hover:bg-bg-tertiary transition-colors"
                title="Copy Code Mode config"
              >
                <IconCopy />
                Code Mode
              </button>
            )}
            <button
              type="button"
              onClick={() => handleCopyConfig()}
              className="inline-flex items-center gap-1 px-2 py-1 rounded-md text-xs text-text-tertiary hover:text-text-primary hover:bg-bg-tertiary transition-colors"
              title="Copy config to clipboard"
            >
              <IconCopy />
              Copy
            </button>
          </div>
        </div>
        {configOpen && (
          <div className="mt-2 space-y-2">
            <ClientConfigSnippet
              label={null}
              config={generateMCPConfig(server.alias)}
              onCopy={() => toast({ variant: 'success', message: 'Copied to clipboard' })}
            />
            {server.alias === 'voidllm' && (
              <ClientConfigSnippet
                label="Code Mode (all tools)"
                config={generateMCPConfig(server.alias, true)}
                onCopy={() => toast({ variant: 'success', message: 'Copied to clipboard' })}
              />
            )}
          </div>
        )}
      </div>

      {/* Tools header */}
      <div className="flex items-center justify-between border-t border-border pt-3">
        <span className="text-xs font-medium uppercase tracking-wider text-text-tertiary">
          Tools
        </span>
        <button
          type="button"
          onClick={handleRefresh}
          disabled={refreshTools.isPending}
          className="inline-flex items-center gap-1.5 px-2.5 py-1.5 rounded-md text-xs text-text-secondary hover:text-text-primary hover:bg-bg-tertiary transition-colors disabled:opacity-40"
        >
          <IconRefresh />
          {refreshTools.isPending ? 'Refreshing…' : 'Refresh Tools'}
        </button>
      </div>

      {/* Tools list */}
      {toolsLoading ? (
        <div className="space-y-1.5">
          {[1, 2, 3].map((i) => (
            <div key={i} className="h-10 bg-bg-tertiary rounded animate-pulse" />
          ))}
        </div>
      ) : toolList.length === 0 ? (
        <p className="text-xs text-text-tertiary py-1">
          No tools cached. Click Refresh to fetch.
        </p>
      ) : (
        <ul className="space-y-0.5">
          {toolList.map((tool) => (
            <li key={tool.name}>
              <div className="flex items-center justify-between py-2 px-3 rounded-md hover:bg-bg-tertiary transition-colors">
                <div className="flex-1 min-w-0">
                  <span className="font-mono text-sm text-text-primary">{tool.name}</span>
                  {tool.description && (
                    <p className="text-xs text-text-tertiary truncate mt-0.5">
                      {truncateText(tool.description, 80)}
                    </p>
                  )}
                </div>
                {canModify && (
                  <button
                    type="button"
                    onClick={() => handleToggleTool(tool.name, tool.blocked)}
                    disabled={isToolPending(tool.name)}
                    className={cn(
                      'ml-3 px-2 py-1 text-xs rounded-md font-medium shrink-0 transition-colors disabled:opacity-40',
                      tool.blocked
                        ? 'bg-red-500/10 text-red-400 hover:bg-red-500/20'
                        : 'text-text-tertiary hover:text-text-secondary hover:bg-bg-tertiary',
                    )}
                  >
                    {tool.blocked ? 'Blocked' : 'Block'}
                  </button>
                )}
                {!canModify && tool.blocked && (
                  <span className="ml-3 px-2 py-1 text-xs rounded-md font-medium shrink-0 bg-red-500/10 text-red-400">
                    Blocked
                  </span>
                )}
              </div>
            </li>
          ))}
        </ul>
      )}

    </div>
  )
}

// ---------------------------------------------------------------------------
// MCPServersPage
// ---------------------------------------------------------------------------

export default function MCPServersPage() {
  const [showCreateDialog, setShowCreateDialog] = useState(false)
  const [editServer, setEditServer] = useState<MCPServerResponse | null>(null)
  const [deleteServerId, setDeleteServerId] = useState<string | null>(null)
  const [expandedKeys, setExpandedKeys] = useState<Set<string>>(new Set())

  const { data: me } = useMe()
  const orgId = me?.org_id ?? ''
  const isSystemAdmin = me?.is_system_admin === true
  const isOrgAdmin = me?.role === 'org_admin' || isSystemAdmin
  const isTeamAdmin = me?.role === 'team_admin' || isOrgAdmin
  const canCreate = isTeamAdmin

  // System admins without an org use the global list; everyone else uses the org-scoped
  // list which returns global + org + team servers visible to the caller.
  const useGlobal = isSystemAdmin && !orgId
  const { data: globalServers, isLoading: globalLoading } = useQuery({
    queryKey: ['mcp-servers'],
    queryFn: () => apiClient<MCPServerResponse[]>('/mcp-servers'),
    enabled: useGlobal,
  })
  const { data: orgServers, isLoading: orgLoading } = useOrgMCPServers(orgId)

  const servers = isSystemAdmin && !orgId ? globalServers : orgServers
  const isLoading = isSystemAdmin && !orgId ? globalLoading : orgLoading

  const deleteServer = useDeleteMCPServer()
  const toggleServer = useToggleMCPServer()
  const updateServer = useUpdateMCPServer()
  const testServer = useTestMCPServer()
  const { toast, update } = useToast()

  function handleToggleExpand(key: string) {
    setExpandedKeys((prev) => {
      const next = new Set(prev)
      if (next.has(key)) {
        next.delete(key)
      } else {
        next.add(key)
      }
      return next
    })
  }

  const { data: healthData } = useMCPServerHealth()
  const healthMap = useMemo(
    () => new Map((healthData ?? []).map((h) => [h.server_id, h])),
    [healthData],
  )

  const allServers = servers ?? []
  const activeCount = allServers.filter((s) => s.is_active).length
  const inactiveCount = allServers.length - activeCount
  const healthyCount = allServers.filter((s) => healthMap.get(s.id)?.status === 'healthy').length

  const columns: Column<MCPServerResponse>[] = [
    {
      key: 'name',
      header: 'Name',
      render: (row) => (
        <span className="font-mono text-text-primary text-sm">{row.name}</span>
      ),
    },
    {
      key: 'alias',
      header: 'Alias',
      render: (row) => (
        <Badge variant="muted">{row.alias}</Badge>
      ),
    },
    {
      key: 'url',
      header: 'URL',
      render: (row) => (
        <span
          className="text-text-tertiary text-sm truncate max-w-[260px] block"
          title={row.url}
        >
          {row.url}
        </span>
      ),
    },
    {
      key: 'auth_type',
      header: 'Auth',
      render: (row) => (
        <Badge variant={authBadgeVariant(row.auth_type)}>
          {authLabel(row.auth_type)}
        </Badge>
      ),
    },
    {
      key: 'scope',
      header: 'Scope',
      render: (row) => (
        <Badge variant={scopeBadgeVariant(row.scope)}>
          {scopeLabel(row.scope)}
        </Badge>
      ),
    },
    {
      key: 'source',
      header: 'Source',
      render: (row) => (
        <Badge variant={sourceBadgeVariant(row.source)}>
          {sourceLabel(row.source)}
        </Badge>
      ),
    },
    {
      key: 'is_active',
      header: 'Status',
      render: (row) => {
        // Only allow toggling if the user has permission for that server's scope.
        // yaml-sourced servers cannot be toggled through the Admin API.
        const canToggle =
          row.source !== 'yaml' && row.source !== 'builtin' &&
          ((row.scope === 'global' && isSystemAdmin) ||
          (row.scope === 'org' && isOrgAdmin) ||
          (row.scope === 'team' && isTeamAdmin))
        return (
          <Toggle
            checked={row.is_active}
            onChange={(activate) =>
              toggleServer.mutate(
                { serverId: row.id, activate },
                {
                  onError: (err) => {
                    toast({
                      variant: 'error',
                      message: err instanceof Error ? err.message : 'Failed to update server status',
                    })
                  },
                },
              )
            }
            disabled={
              !canToggle ||
              (toggleServer.isPending && toggleServer.variables?.serverId === row.id)
            }
            size="sm"
          />
        )
      },
    },
    {
      key: 'code_mode_enabled',
      header: 'Code Mode',
      render: (row) => {
        const canToggle =
          ((row.scope === 'global' && isSystemAdmin) ||
          (row.scope === 'org' && isOrgAdmin) ||
          (row.scope === 'team' && isTeamAdmin))
        return (
          <Toggle
            checked={row.code_mode_enabled}
            onChange={(enabled) =>
              updateServer.mutate(
                { serverId: row.id, params: { code_mode_enabled: enabled } },
                {
                  onError: (err) => {
                    toast({
                      variant: 'error',
                      message: err instanceof Error ? err.message : 'Failed to update code mode',
                    })
                  },
                },
              )
            }
            disabled={
              !canToggle ||
              (updateServer.isPending && updateServer.variables?.serverId === row.id)
            }
            size="sm"
          />
        )
      },
    },
    {
      key: 'health',
      header: 'Health',
      render: (row) => <HealthIndicator health={healthMap.get(row.id)} />,
    },
    {
      key: 'actions',
      header: '',
      align: 'right',
      render: (row) => {
        // yaml-sourced servers are managed via config file and cannot be
        // edited or deleted through the Admin API.
        const canModify =
          row.source !== 'yaml' && row.source !== 'builtin' &&
          ((row.scope === 'global' && isSystemAdmin) ||
          (row.scope === 'org' && isOrgAdmin) ||
          (row.scope === 'team' && isTeamAdmin))
        return (
          <div className="flex items-center justify-end gap-1">
            <button
              type="button"
              onClick={() => {
                const tid = toast({ variant: 'info', message: `Testing connection to ${row.name}...`, duration: 60000 })
                testServer.mutate(row.id, {
                  onSuccess: (result) => {
                    if (result.success) {
                      const toolCount = result.tools != null ? ` (${result.tools} tools)` : ''
                      update(tid, { variant: 'success', message: `Connection successful${toolCount}` })
                    } else {
                      update(tid, {
                        variant: 'error',
                        message: result.error ?? 'Connection test failed',
                      })
                    }
                  },
                  onError: (err) => {
                    update(tid, {
                      variant: 'error',
                      message: err instanceof Error ? err.message : 'Connection test failed',
                    })
                  },
                })
              }}
              disabled={testServer.isPending && testServer.variables === row.id}
              title="Test connection"
              className="p-1.5 rounded-md text-text-tertiary hover:text-accent hover:bg-accent/10 transition-colors disabled:opacity-40"
            >
              <IconPlug />
            </button>
            {canModify && (
              <>
                <button
                  type="button"
                  onClick={() => setEditServer(row)}
                  title="Edit server"
                  className="p-1.5 rounded-md text-text-tertiary hover:text-text-primary hover:bg-bg-tertiary transition-colors"
                >
                  <IconPencil />
                </button>
                <button
                  type="button"
                  onClick={() => setDeleteServerId(row.id)}
                  disabled={deleteServer.isPending && deleteServerId === row.id}
                  title="Delete server"
                  className="p-1.5 rounded-md text-text-tertiary hover:text-error hover:bg-error/10 transition-colors disabled:opacity-40"
                >
                  <IconTrash />
                </button>
              </>
            )}
          </div>
        )
      },
    },
  ]

  function handleDelete() {
    if (!deleteServerId) return
    deleteServer.mutate(deleteServerId, {
      onSuccess: () => {
        toast({ variant: 'success', message: 'MCP server deleted' })
        setDeleteServerId(null)
      },
      onError: (err) => {
        toast({
          variant: 'error',
          message: err instanceof Error ? err.message : 'Failed to delete MCP server',
        })
        setDeleteServerId(null)
      },
    })
  }

  return (
    <>
      <PageHeader
        title="MCP Servers"
        description="Manage Model Context Protocol server connections"
        actions={
          canCreate ? (
            <Button onClick={() => setShowCreateDialog(true)}>Add Server</Button>
          ) : undefined
        }
      />

      <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4 mb-6">
        <StatCard
          label="Total Servers"
          value={isLoading ? '—' : allServers.length}
          icon={<IconServer />}
          iconColor="purple"
        />
        <StatCard
          label="Active"
          value={isLoading ? '—' : activeCount}
          icon={<IconActivity />}
          iconColor="green"
        />
        <StatCard
          label="Inactive"
          value={isLoading ? '—' : inactiveCount}
          icon={<IconPauseCircle />}
          iconColor="yellow"
        />
        <StatCard
          label="Healthy"
          value={healthData === undefined ? '—' : healthyCount}
          icon={<IconHealthDot />}
          iconColor="green"
        />
      </div>

      <Table<MCPServerResponse>
        columns={columns}
        data={allServers}
        keyExtractor={(row) => row.id}
        loading={isLoading}
        emptyMessage="No MCP servers configured"
        expandedKeys={expandedKeys}
        onToggleExpand={handleToggleExpand}
        renderExpandedRow={(row) => {
          // Blocklist management is allowed regardless of source (YAML or API)
          // — only the RBAC role matters. Server config editing (name, URL, etc.)
          // is still restricted for YAML-sourced servers.
          const canBlockTools =
            (row.scope === 'global' && isSystemAdmin) ||
            (row.scope === 'org' && isOrgAdmin) ||
            (row.scope === 'team' && isTeamAdmin)
          return (
            <ServerExpandedRow
              server={row}
              canModify={canBlockTools}
              health={healthMap.get(row.id)}
            />
          )
        }}
      />

      {showCreateDialog && (
        <CreateMCPServerDialog
          open={showCreateDialog}
          onClose={() => setShowCreateDialog(false)}
          orgId={orgId}
          isSystemAdmin={isSystemAdmin}
          isOrgAdmin={isOrgAdmin}
        />
      )}

      {editServer !== null && (
        <EditMCPServerDialog
          server={editServer}
          onClose={() => setEditServer(null)}
        />
      )}

      <ConfirmDialog
        open={deleteServerId !== null}
        onClose={() => setDeleteServerId(null)}
        onConfirm={handleDelete}
        title="Delete MCP Server"
        description="This MCP server will be permanently removed. Any integrations using this server will stop working."
        confirmLabel="Delete"
        loading={deleteServer.isPending}
      />
    </>
  )
}
