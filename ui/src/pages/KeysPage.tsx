import React, { useState, useEffect } from 'react'
import { useQuery } from '@tanstack/react-query'
import { PageHeader } from '../components/ui/PageHeader'
import { Table } from '../components/ui/Table'
import type { Column } from '../components/ui/Table'
import { Dialog, ConfirmDialog } from '../components/ui/Dialog'
import { Badge } from '../components/ui/Badge'
import { Button } from '../components/ui/Button'
import { Input } from '../components/ui/Input'
import { Select } from '../components/ui/Select'
import { Toggle } from '../components/ui/Toggle'
import { KeyHint } from '../components/ui/KeyHint'
import { TimeAgo } from '../components/ui/TimeAgo'
import { CopyButton } from '../components/ui/CopyButton'
import { useMe } from '../hooks/useMe'
import { useAPIKeys, useCreateAPIKey, useDeleteAPIKey, useUpdateAPIKey, useRotateAPIKey } from '../hooks/useAPIKeys'
import type { APIKeyResponse, CreateAPIKeyParams } from '../hooks/useAPIKeys'
import { useTeams } from '../hooks/useTeams'
import { useServiceAccounts } from '../hooks/useServiceAccounts'
import { useToast } from '../hooks/useToast'
import apiClient from '../api/client'

// ---------------------------------------------------------------------------
// Module-level constants
// ---------------------------------------------------------------------------

const keyTypeBadgeVariant: Record<string, 'default' | 'info' | 'warning' | 'muted'> = {
  user_key: 'default',
  team_key: 'info',
  sa_key: 'warning',
  session_key: 'muted',
}

const keyTypeLabels: Record<string, string> = {
  user_key: 'User',
  team_key: 'Team',
  sa_key: 'Service Acct',
  session_key: 'Session',
}

const KEY_TYPE_OPTIONS = [
  { value: 'user_key', label: 'User Key' },
  { value: 'team_key', label: 'Team Key' },
  { value: 'sa_key', label: 'Service Account' },
]

const EXPIRES_OPTIONS = [
  { value: '30d', label: '30 days' },
  { value: '90d', label: '90 days' },
  { value: '1y', label: '1 year' },
  { value: 'never', label: 'Never' },
]

function expiresAtFromOption(opt: string): string | undefined {
  const days: Record<string, number> = { '30d': 30, '90d': 90, '1y': 365 }
  if (opt === 'never' || !days[opt]) return undefined
  return new Date(Date.now() + days[opt] * 86400000).toISOString()
}

// ---------------------------------------------------------------------------
// CreateKeyDialog
// ---------------------------------------------------------------------------

interface CreateKeyDialogProps {
  open: boolean
  onClose: () => void
  onCreated: (key: string) => void
  orgId: string
}

function CreateKeyDialog({ open, onClose, onCreated, orgId }: CreateKeyDialogProps) {
  const [name, setName] = useState('')
  const [keyType, setKeyType] = useState('user_key')
  const [expiresIn, setExpiresIn] = useState('90d')
  const [nameError, setNameError] = useState<string | undefined>()
  const [teamId, setTeamId] = useState('')
  const [serviceAccountId, setServiceAccountId] = useState('')
  const [teamError, setTeamError] = useState<string | undefined>()
  const [serviceAccountError, setServiceAccountError] = useState<string | undefined>()
  const [restrictModels, setRestrictModels] = useState(false)
  const [selectedModels, setSelectedModels] = useState<Set<string>>(new Set())
  const [showAdvancedLimits, setShowAdvancedLimits] = useState(false)
  const [dailyTokenLimit, setDailyTokenLimit] = useState('')
  const [monthlyTokenLimit, setMonthlyTokenLimit] = useState('')
  const [requestsPerMinute, setRequestsPerMinute] = useState('')
  const [requestsPerDay, setRequestsPerDay] = useState('')

  const { data: me } = useMe()
  const { data: teams } = useTeams(orgId)
  const { data: serviceAccounts } = useServiceAccounts(orgId)

  const userTeams = teams?.data ?? []
  const showTeamPickerForUserKey = keyType === 'user_key' && userTeams.length > 1
  const showTeamPicker = keyType === 'team_key' || showTeamPickerForUserKey

  const createKey = useCreateAPIKey(orgId)
  const { toast } = useToast()

  const { data: availableModels } = useQuery({
    queryKey: ['available-models'],
    queryFn: () => apiClient<{ models: string[] }>('/me/available-models'),
  })

  function handleClose() {
    setName('')
    setKeyType('user_key')
    setExpiresIn('90d')
    setNameError(undefined)
    setTeamId('')
    setServiceAccountId('')
    setTeamError(undefined)
    setServiceAccountError(undefined)
    setRestrictModels(false)
    setSelectedModels(new Set())
    setShowAdvancedLimits(false)
    setDailyTokenLimit('')
    setMonthlyTokenLimit('')
    setRequestsPerMinute('')
    setRequestsPerDay('')
    onClose()
  }

  function handleKeyTypeChange(newType: string) {
    setKeyType(newType)
    setTeamId('')
    setServiceAccountId('')
    setTeamError(undefined)
    setServiceAccountError(undefined)
  }

  function toggleModel(modelName: string) {
    setSelectedModels((prev) => {
      const next = new Set(prev)
      if (next.has(modelName)) {
        next.delete(modelName)
      } else {
        next.add(modelName)
      }
      return next
    })
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()

    const trimmedName = name.trim()
    let hasError = false

    if (!trimmedName) {
      setNameError('Name is required')
      hasError = true
    } else {
      setNameError(undefined)
    }

    if (keyType === 'user_key' && userTeams.length > 1 && !teamId) {
      setTeamError('Select a team for this key')
      hasError = true
    } else if (keyType === 'team_key' && !teamId) {
      setTeamError('Team is required')
      hasError = true
    } else {
      setTeamError(undefined)
    }

    if (keyType === 'user_key' && !me?.id) {
      hasError = true
    }

    if (keyType === 'sa_key' && !serviceAccountId) {
      setServiceAccountError('Service account is required')
      hasError = true
    } else {
      setServiceAccountError(undefined)
    }

    if (hasError) return

    let effectiveTeamId = teamId
    if (keyType === 'user_key') {
      if (userTeams.length === 1) {
        effectiveTeamId = userTeams[0].id
      } else if (userTeams.length === 0) {
        effectiveTeamId = ''
      }
    }

    const params: CreateAPIKeyParams = {
      name: trimmedName,
      key_type: keyType,
      expires_at: expiresAtFromOption(expiresIn),
      ...(keyType === 'user_key' ? {
        user_id: me?.id,
      } : {}),
      ...(keyType === 'team_key' && effectiveTeamId ? { team_id: effectiveTeamId } : {}),
      ...(keyType === 'sa_key' && serviceAccountId ? { service_account_id: serviceAccountId } : {}),
    }

    const parsedDailyToken = parseInt(dailyTokenLimit, 10)
    if (dailyTokenLimit.trim() && !isNaN(parsedDailyToken)) {
      params.daily_token_limit = parsedDailyToken
    }
    const parsedMonthlyToken = parseInt(monthlyTokenLimit, 10)
    if (monthlyTokenLimit.trim() && !isNaN(parsedMonthlyToken)) {
      params.monthly_token_limit = parsedMonthlyToken
    }
    const parsedRpm = parseInt(requestsPerMinute, 10)
    if (requestsPerMinute.trim() && !isNaN(parsedRpm)) {
      params.requests_per_minute = parsedRpm
    }
    const parsedRpd = parseInt(requestsPerDay, 10)
    if (requestsPerDay.trim() && !isNaN(parsedRpd)) {
      params.requests_per_day = parsedRpd
    }

    createKey.mutate(params, {
      onSuccess: async (data) => {
        if (restrictModels && selectedModels.size > 0) {
          try {
            await apiClient(`/orgs/${orgId}/keys/${data.id}/model-access`, {
              method: 'PUT',
              body: JSON.stringify({ models: Array.from(selectedModels) }),
            })
          } catch {
            toast({
              variant: 'error',
              message: 'Key created but model access could not be set',
            })
          }
        }
        handleClose()
        if (data.key) {
          onCreated(data.key)
        }
      },
      onError: (err) => {
        toast({
          variant: 'error',
          message: err instanceof Error ? err.message : 'Failed to create API key',
        })
      },
    })
  }

  return (
    <Dialog open={open} onClose={handleClose} title="Create API Key">
      <form onSubmit={handleSubmit} className="space-y-4" noValidate>
        <Input
          label="Name"
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="e.g. Production backend"
          error={nameError}
          disabled={createKey.isPending}
        />
        <Select
          label="Key Type"
          options={KEY_TYPE_OPTIONS}
          value={keyType}
          onChange={handleKeyTypeChange}
          disabled={createKey.isPending}
        />
        {showTeamPicker && (
          <Select
            label="Team"
            options={userTeams.map((t) => ({ value: t.id, label: t.name }))}
            value={teamId}
            onChange={(v) => { setTeamId(v); setTeamError(undefined) }}
            placeholder="Select a team..."
            searchable
            error={teamError}
            disabled={createKey.isPending}
          />
        )}
        {keyType === 'sa_key' && (
          <Select
            label="Service Account"
            options={serviceAccounts?.data?.map((sa) => ({ value: sa.id, label: sa.name })) ?? []}
            value={serviceAccountId}
            onChange={(v) => { setServiceAccountId(v); setServiceAccountError(undefined) }}
            placeholder="Select a service account..."
            searchable
            error={serviceAccountError}
            disabled={createKey.isPending}
          />
        )}
        <Select
          label="Expires In"
          options={EXPIRES_OPTIONS}
          value={expiresIn}
          onChange={setExpiresIn}
          disabled={createKey.isPending}
        />

        {/* Advanced Limits */}
        <div className="border-t border-border pt-4">
          <button
            type="button"
            className="flex items-center gap-2 text-sm text-text-secondary hover:text-text-primary transition-colors"
            onClick={() => setShowAdvancedLimits((v) => !v)}
            disabled={createKey.isPending}
          >
            <svg
              className={['h-4 w-4 transition-transform', showAdvancedLimits ? 'rotate-90' : ''].join(' ')}
              fill="none"
              viewBox="0 0 24 24"
              stroke="currentColor"
              strokeWidth={2}
              aria-hidden="true"
            >
              <path strokeLinecap="round" strokeLinejoin="round" d="M9 5l7 7-7 7" />
            </svg>
            Set rate &amp; token limits
          </button>
          {showAdvancedLimits && (
            <div className="mt-4 space-y-4">
              <div className="grid grid-cols-2 gap-4">
                <Input
                  label="Daily Token Limit"
                  type="number"
                  value={dailyTokenLimit}
                  onChange={(e) => setDailyTokenLimit(e.target.value)}
                  placeholder="0 = unlimited"
                  disabled={createKey.isPending}
                />
                <Input
                  label="Monthly Token Limit"
                  type="number"
                  value={monthlyTokenLimit}
                  onChange={(e) => setMonthlyTokenLimit(e.target.value)}
                  placeholder="0 = unlimited"
                  disabled={createKey.isPending}
                />
              </div>
              <div className="grid grid-cols-2 gap-4">
                <Input
                  label="Requests per Minute"
                  type="number"
                  value={requestsPerMinute}
                  onChange={(e) => setRequestsPerMinute(e.target.value)}
                  placeholder="0 = unlimited"
                  disabled={createKey.isPending}
                />
                <Input
                  label="Requests per Day"
                  type="number"
                  value={requestsPerDay}
                  onChange={(e) => setRequestsPerDay(e.target.value)}
                  placeholder="0 = unlimited"
                  disabled={createKey.isPending}
                />
              </div>
            </div>
          )}
        </div>

        <div className="space-y-2">
          <div className="flex items-center gap-3 border-t border-border pt-4">
            <Toggle
              checked={restrictModels}
              onChange={setRestrictModels}
              label="Restrict model access"
              size="sm"
              disabled={createKey.isPending}
            />
          </div>
          <p className="text-xs text-text-tertiary">
            {restrictModels
              ? 'Only selected models will be accessible with this key.'
              : 'Key inherits model access from team and organization scope.'}
          </p>

          {restrictModels && (
            <div className="space-y-1.5">
              {availableModels?.models && availableModels.models.length > 0 ? (
                <div className="max-h-48 overflow-y-auto rounded-lg border border-border p-1.5">
                  {availableModels.models.map((modelName) => (
                    <label
                      key={modelName}
                      className="flex cursor-pointer items-center gap-3 rounded px-2 py-1.5 hover:bg-bg-tertiary"
                    >
                      <input
                        type="checkbox"
                        checked={selectedModels.has(modelName)}
                        onChange={() => toggleModel(modelName)}
                        className="accent-accent h-4 w-4 shrink-0 cursor-pointer"
                        disabled={createKey.isPending}
                      />
                      <span className="font-mono text-sm text-text-primary">{modelName}</span>
                    </label>
                  ))}
                </div>
              ) : availableModels?.models && availableModels.models.length === 0 ? (
                <p className="text-xs text-text-tertiary">No models available.</p>
              ) : (
                <div className="space-y-1 rounded-lg border border-border p-1.5">
                  {Array.from({ length: 3 }).map((_, i) => (
                    <div key={i} className="flex items-center gap-3 rounded px-2 py-1.5">
                      <div className="h-4 w-4 shrink-0 animate-pulse rounded bg-bg-tertiary" />
                      <div className="h-4 w-32 animate-pulse rounded bg-bg-tertiary" />
                    </div>
                  ))}
                </div>
              )}
            </div>
          )}
        </div>

        <div className="flex justify-end gap-2 pt-2">
          <Button
            variant="secondary"
            onClick={handleClose}
            disabled={createKey.isPending}
          >
            Cancel
          </Button>
          <Button onClick={handleSubmit} loading={createKey.isPending}>
            Create Key
          </Button>
        </div>
      </form>
    </Dialog>
  )
}

// ---------------------------------------------------------------------------
// KeyCreatedDialog
// ---------------------------------------------------------------------------

interface KeyCreatedDialogProps {
  keyValue: string | null
  onClose: () => void
}

function KeyCreatedDialog({ keyValue, onClose }: KeyCreatedDialogProps) {
  return (
    <Dialog
      open={keyValue !== null}
      onClose={onClose}
      title="API Key Created"
      closeOnBackdrop={false}
    >
      <div className="space-y-4">
        <div className="flex items-center gap-2 rounded-lg border border-warning/30 bg-warning/5 px-3 py-2">
          <svg
            className="h-4 w-4 shrink-0 text-warning"
            fill="none"
            viewBox="0 0 24 24"
            stroke="currentColor"
            strokeWidth={2}
            aria-hidden="true"
          >
            <path
              strokeLinecap="round"
              strokeLinejoin="round"
              d="M12 9v2m0 4h.01M10.29 3.86L1.82 18a2 2 0 001.71 3h16.94a2 2 0 001.71-3L13.71 3.86a2 2 0 00-3.42 0z"
            />
          </svg>
          <span className="text-xs text-warning">
            This key will only be shown once. Copy it now.
          </span>
        </div>
        <div className="rounded-lg border border-border bg-bg-primary p-3 font-mono text-sm text-text-primary break-all">
          {keyValue}
        </div>
        <div className="flex justify-end gap-2">
          <CopyButton text={keyValue ?? ''} label="Copy Key" />
          <Button variant="secondary" onClick={onClose}>
            Close
          </Button>
        </div>
      </div>
    </Dialog>
  )
}

// ---------------------------------------------------------------------------
// EditKeyDialog
// ---------------------------------------------------------------------------

const EDIT_EXPIRES_OPTIONS = [
  { value: 'keep', label: 'Keep current' },
  { value: '30d', label: '30 days from now' },
  { value: '90d', label: '90 days from now' },
  { value: '1y', label: '1 year from now' },
  { value: 'never', label: 'Never' },
]

interface EditKeyDialogProps {
  apiKey: APIKeyResponse
  onClose: () => void
  orgId: string
}

function EditKeyDialog({ apiKey, onClose, orgId }: EditKeyDialogProps) {
  const [name, setName] = useState(apiKey.name)
  const [expiresIn, setExpiresIn] = useState('keep')
  const [dailyTokenLimit, setDailyTokenLimit] = useState(
    apiKey.daily_token_limit > 0 ? String(apiKey.daily_token_limit) : '',
  )
  const [monthlyTokenLimit, setMonthlyTokenLimit] = useState(
    apiKey.monthly_token_limit > 0 ? String(apiKey.monthly_token_limit) : '',
  )
  const [requestsPerMinute, setRequestsPerMinute] = useState(
    apiKey.requests_per_minute > 0 ? String(apiKey.requests_per_minute) : '',
  )
  const [requestsPerDay, setRequestsPerDay] = useState(
    apiKey.requests_per_day > 0 ? String(apiKey.requests_per_day) : '',
  )
  const [nameError, setNameError] = useState<string | undefined>()

  const updateKey = useUpdateAPIKey(orgId)
  const { toast } = useToast()

  function handleSubmit(e: React.FormEvent) {
    e.preventDefault()

    const trimmedName = name.trim()
    if (!trimmedName) {
      setNameError('Name is required')
      return
    }
    setNameError(undefined)

    const params: Record<string, unknown> = {}

    if (trimmedName !== apiKey.name) params.name = trimmedName

    if (expiresIn !== 'keep') {
      params.expires_at = expiresAtFromOption(expiresIn) ?? null
    }

    const parsedDailyToken = dailyTokenLimit.trim() ? parseInt(dailyTokenLimit, 10) : 0
    if (!isNaN(parsedDailyToken) && parsedDailyToken !== apiKey.daily_token_limit) {
      params.daily_token_limit = parsedDailyToken
    }

    const parsedMonthlyToken = monthlyTokenLimit.trim() ? parseInt(monthlyTokenLimit, 10) : 0
    if (!isNaN(parsedMonthlyToken) && parsedMonthlyToken !== apiKey.monthly_token_limit) {
      params.monthly_token_limit = parsedMonthlyToken
    }

    const parsedRpm = requestsPerMinute.trim() ? parseInt(requestsPerMinute, 10) : 0
    if (!isNaN(parsedRpm) && parsedRpm !== apiKey.requests_per_minute) {
      params.requests_per_minute = parsedRpm
    }

    const parsedRpd = requestsPerDay.trim() ? parseInt(requestsPerDay, 10) : 0
    if (!isNaN(parsedRpd) && parsedRpd !== apiKey.requests_per_day) {
      params.requests_per_day = parsedRpd
    }

    if (Object.keys(params).length === 0) {
      onClose()
      return
    }

    updateKey.mutate(
      { keyId: apiKey.id, params },
      {
        onSuccess: () => {
          toast({ variant: 'success', message: 'API key updated' })
          onClose()
        },
        onError: (err) => {
          toast({
            variant: 'error',
            message: err instanceof Error ? err.message : 'Failed to update API key',
          })
        },
      },
    )
  }

  return (
    <Dialog open onClose={onClose} title="Edit API Key">
      <form onSubmit={handleSubmit} className="space-y-4" noValidate>
        <Input
          label="Name"
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="e.g. Production backend"
          error={nameError}
          disabled={updateKey.isPending}
        />
        <Select
          label="Expires At"
          options={EDIT_EXPIRES_OPTIONS}
          value={expiresIn}
          onChange={setExpiresIn}
          disabled={updateKey.isPending}
        />
        <div className="grid grid-cols-2 gap-4">
          <Input
            label="Daily Token Limit"
            type="number"
            value={dailyTokenLimit}
            onChange={(e) => setDailyTokenLimit(e.target.value)}
            placeholder="0 = unlimited"
            disabled={updateKey.isPending}
          />
          <Input
            label="Monthly Token Limit"
            type="number"
            value={monthlyTokenLimit}
            onChange={(e) => setMonthlyTokenLimit(e.target.value)}
            placeholder="0 = unlimited"
            disabled={updateKey.isPending}
          />
        </div>
        <div className="grid grid-cols-2 gap-4">
          <Input
            label="Requests per Minute"
            type="number"
            value={requestsPerMinute}
            onChange={(e) => setRequestsPerMinute(e.target.value)}
            placeholder="0 = unlimited"
            disabled={updateKey.isPending}
          />
          <Input
            label="Requests per Day"
            type="number"
            value={requestsPerDay}
            onChange={(e) => setRequestsPerDay(e.target.value)}
            placeholder="0 = unlimited"
            disabled={updateKey.isPending}
          />
        </div>
        <div className="flex justify-end gap-2 pt-2">
          <Button variant="secondary" onClick={onClose} disabled={updateKey.isPending}>
            Cancel
          </Button>
          <Button onClick={handleSubmit} loading={updateKey.isPending}>
            Save Changes
          </Button>
        </div>
      </form>
    </Dialog>
  )
}

// ---------------------------------------------------------------------------
// KeysPage
// ---------------------------------------------------------------------------

export default function KeysPage() {
  const { data: me } = useMe()
  const orgId = me?.org_id ?? ''

  const [cursor, setCursor] = useState<string | undefined>()
  const [prevCursors, setPrevCursors] = useState<string[]>([])
  const [showCreateDialog, setShowCreateDialog] = useState(false)
  const [createdKey, setCreatedKey] = useState<string | null>(null)
  const [revokeKeyId, setRevokeKeyId] = useState<string | null>(null)
  const [editKey, setEditKey] = useState<APIKeyResponse | null>(null)
  const [rotateKeyId, setRotateKeyId] = useState<string | null>(null)
  const [rotatedKey, setRotatedKey] = useState<string | null>(null)

  useEffect(() => {
    return () => setCreatedKey(null)
  }, [])

  const { data: keys, isLoading } = useAPIKeys(orgId, cursor)
  const deleteKey = useDeleteAPIKey(orgId)
  const rotateKey = useRotateAPIKey(orgId)
  const { toast } = useToast()

  const columns: Column<APIKeyResponse>[] = [
    {
      key: 'key_hint',
      header: 'Key',
      render: (row) => (
        <KeyHint hint={row.key_hint} />
      ),
    },
    {
      key: 'key_type',
      header: 'Type',
      render: (row) => (
        <Badge variant={keyTypeBadgeVariant[row.key_type] ?? 'muted'}>
          {keyTypeLabels[row.key_type] ?? row.key_type}
        </Badge>
      ),
    },
    {
      key: 'name',
      header: 'Name',
      render: (row) => (
        <span className="text-text-primary">{row.name}</span>
      ),
    },
    {
      key: 'expires_at',
      header: 'Expires',
      render: (row) => (
        <TimeAgo date={row.expires_at ?? ''} fallback="Never" />
      ),
    },
    {
      key: 'created_at',
      header: 'Created',
      render: (row) => <TimeAgo date={row.created_at} />,
    },
    {
      key: 'actions',
      header: '',
      align: 'right',
      render: (row) => {
        if (row.key_type === 'session_key') return null
        return (
          <div className="flex items-center justify-end gap-1">
            <Button
              variant="ghost"
              size="sm"
              onClick={() => setEditKey(row)}
              disabled={deleteKey.isPending || rotateKey.isPending}
            >
              Edit
            </Button>
            <Button
              variant="ghost"
              size="sm"
              onClick={() => setRotateKeyId(row.id)}
              disabled={deleteKey.isPending || rotateKey.isPending}
            >
              Rotate
            </Button>
            <Button
              variant="ghost"
              size="sm"
              onClick={() => setRevokeKeyId(row.id)}
              className="text-error hover:text-error"
              disabled={deleteKey.isPending || rotateKey.isPending}
            >
              Revoke
            </Button>
          </div>
        )
      },
    },
  ]

  function handleRevoke() {
    if (!revokeKeyId) return
    deleteKey.mutate(revokeKeyId, {
      onSuccess: () => {
        toast({ variant: 'success', message: 'API key revoked' })
        setRevokeKeyId(null)
      },
      onError: (err) => {
        toast({
          variant: 'error',
          message: err instanceof Error ? err.message : 'Failed to revoke key',
        })
        setRevokeKeyId(null)
      },
    })
  }

  function handleRotate() {
    if (!rotateKeyId) return
    rotateKey.mutate(rotateKeyId, {
      onSuccess: (data) => {
        setRotateKeyId(null)
        if (data.key) {
          setRotatedKey(data.key)
        }
      },
      onError: (err) => {
        toast({
          variant: 'error',
          message: err instanceof Error ? err.message : 'Failed to rotate key',
        })
        setRotateKeyId(null)
      },
    })
  }

  return (
    <>
      <PageHeader
        title="API Keys"
        description="Manage your API keys"
        actions={
          <Button onClick={() => setShowCreateDialog(true)}>Create Key</Button>
        }
      />

      <Table<APIKeyResponse>
        columns={columns}
        data={keys?.data ?? []}
        keyExtractor={(row) => row.id}
        loading={isLoading && !!orgId}
        emptyMessage="No API keys found"
        pagination={{
          cursor: cursor ?? null,
          hasMore: keys?.has_more ?? false,
          hasPrevious: prevCursors.length > 0,
          onNext: () => {
            if (keys?.next_cursor) {
              setPrevCursors((prev) => [...prev, cursor ?? ''])
              setCursor(keys.next_cursor)
            }
          },
          onPrevious: () => {
            const prev = prevCursors[prevCursors.length - 1]
            setPrevCursors((p) => p.slice(0, -1))
            setCursor(prev || undefined)
          },
        }}
      />

      <CreateKeyDialog
        open={showCreateDialog}
        onClose={() => setShowCreateDialog(false)}
        onCreated={(key) => setCreatedKey(key)}
        orgId={orgId}
      />

      <KeyCreatedDialog
        keyValue={createdKey}
        onClose={() => setCreatedKey(null)}
      />

      {editKey !== null && (
        <EditKeyDialog
          apiKey={editKey}
          onClose={() => setEditKey(null)}
          orgId={orgId}
        />
      )}

      <ConfirmDialog
        open={rotateKeyId !== null}
        onClose={() => setRotateKeyId(null)}
        onConfirm={handleRotate}
        title="Rotate API Key"
        description="This will generate a new key and expire the current key after 24 hours. Any application using the current key will need to be updated."
        confirmLabel="Rotate"
        loading={rotateKey.isPending}
      />

      <KeyCreatedDialog
        keyValue={rotatedKey}
        onClose={() => setRotatedKey(null)}
      />

      <ConfirmDialog
        open={revokeKeyId !== null}
        onClose={() => setRevokeKeyId(null)}
        onConfirm={handleRevoke}
        title="Revoke API Key"
        description="Are you sure you want to revoke this key? This action cannot be undone. Any application using this key will lose access."
        confirmLabel="Revoke"
        loading={deleteKey.isPending}
      />
    </>
  )
}
