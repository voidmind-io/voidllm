import React, { useMemo, useState, useEffect } from 'react'
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
import { StatCard } from '../components/ui/StatCard'
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
// Inline SVG icons
// ---------------------------------------------------------------------------

function IconKey({ className }: { className?: string }) {
  return (
    <svg
      className={className}
      fill="none"
      viewBox="0 0 24 24"
      stroke="currentColor"
      strokeWidth={1.5}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d="M21 2l-2 2m-7.61 7.61a5.5 5.5 0 1 1-7.778 7.778 5.5 5.5 0 0 1 7.777-7.777zm0 0L15.5 7.5m0 0l3 3L22 7l-3-3m-3.5 3.5L19 4" />
    </svg>
  )
}

function IconCheck({ className }: { className?: string }) {
  return (
    <svg
      className={className}
      fill="none"
      viewBox="0 0 24 24"
      stroke="currentColor"
      strokeWidth={1.5}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d="M20 6L9 17l-5-5" />
    </svg>
  )
}

function IconClock({ className }: { className?: string }) {
  return (
    <svg
      className={className}
      fill="none"
      viewBox="0 0 24 24"
      stroke="currentColor"
      strokeWidth={1.5}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <circle cx="12" cy="12" r="10" />
      <path d="M12 6v6l4 2" />
    </svg>
  )
}

function IconPencil({ className }: { className?: string }) {
  return (
    <svg
      className={className}
      fill="none"
      viewBox="0 0 24 24"
      stroke="currentColor"
      strokeWidth={1.5}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d="M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7" />
      <path d="M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z" />
    </svg>
  )
}

function IconRefresh({ className }: { className?: string }) {
  return (
    <svg
      className={className}
      fill="none"
      viewBox="0 0 24 24"
      stroke="currentColor"
      strokeWidth={1.5}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d="M1 4v6h6" />
      <path d="M23 20v-6h-6" />
      <path d="M20.49 9A9 9 0 0 0 5.64 5.64L1 10m22 4l-4.64 4.36A9 9 0 0 1 3.51 15" />
    </svg>
  )
}

function IconTrash({ className }: { className?: string }) {
  return (
    <svg
      className={className}
      fill="none"
      viewBox="0 0 24 24"
      stroke="currentColor"
      strokeWidth={1.5}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <polyline points="3 6 5 6 21 6" />
      <path d="M19 6l-1 14H6L5 6" />
      <path d="M10 11v6M14 11v6" />
      <path d="M9 6V4a1 1 0 0 1 1-1h4a1 1 0 0 1 1 1v2" />
    </svg>
  )
}

function IconCheckCircle({ className }: { className?: string }) {
  return (
    <svg
      className={className}
      fill="none"
      viewBox="0 0 24 24"
      stroke="currentColor"
      strokeWidth={1.5}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d="M22 11.08V12a10 10 0 1 1-5.93-9.14" />
      <polyline points="22 4 12 14.01 9 11.01" />
    </svg>
  )
}

function IconChevronDown({ className }: { className?: string }) {
  return (
    <svg
      className={className}
      fill="none"
      viewBox="0 0 24 24"
      stroke="currentColor"
      strokeWidth={2}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d="M6 9l6 6 6-6" />
    </svg>
  )
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

        {/* Rate & Token Limits — collapsible */}
        <div className="border-t border-border pt-4">
          <button
            type="button"
            className="flex w-full items-center justify-between"
            onClick={() => setShowAdvancedLimits((v) => !v)}
            disabled={createKey.isPending}
          >
            <span className="text-[10px] font-medium tracking-widest uppercase text-text-tertiary">
              Rate &amp; Token Limits
            </span>
            <IconChevronDown
              className={[
                'h-3.5 w-3.5 text-text-tertiary transition-transform duration-150',
                showAdvancedLimits ? 'rotate-180' : '',
              ].join(' ')}
            />
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

        {/* Model Access — collapsible */}
        <div className="border-t border-border pt-4">
          <div className="flex items-center justify-between">
            <span className="text-[10px] font-medium tracking-widest uppercase text-text-tertiary">
              Model Access
            </span>
            <Toggle
              checked={restrictModels}
              onChange={setRestrictModels}
              label="Restrict models"
              size="sm"
              disabled={createKey.isPending}
            />
          </div>
          <p className="mt-2 text-xs text-text-tertiary">
            {restrictModels
              ? 'Only selected models will be accessible with this key.'
              : 'Key inherits model access from team and organization scope.'}
          </p>

          {restrictModels && (
            <div className="mt-3 space-y-1.5">
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
      title="Key Created Successfully"
      closeOnBackdrop={false}
    >
      <div className="space-y-5">
        {/* Success icon */}
        <div className="flex justify-center">
          <span className="flex h-14 w-14 items-center justify-center rounded-full bg-success/10">
            <IconCheckCircle className="h-7 w-7 text-success" />
          </span>
        </div>

        {/* Warning banner */}
        <div className="flex items-start gap-2.5 rounded-lg border border-warning/25 bg-warning/5 px-3 py-2.5">
          <svg
            className="mt-0.5 h-4 w-4 shrink-0 text-warning"
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
          <span className="text-xs leading-relaxed text-warning">
            Copy this key now. You won&apos;t be able to see it again.
          </span>
        </div>

        {/* Key display */}
        <div className="rounded-lg border border-border bg-bg-primary px-4 py-3">
          <p className="break-all font-mono text-sm leading-relaxed text-text-primary">
            {keyValue}
          </p>
        </div>

        {/* Actions */}
        <div className="flex items-center justify-end gap-2">
          <CopyButton text={keyValue ?? ''} label="Copy Key" copiedLabel="Copied!" />
          <Button onClick={onClose}>Done</Button>
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

  const allKeys = useMemo(() => keys?.data ?? [], [keys?.data])

  const [totalKeys, activeKeys, expiringSoon] = useMemo(() => {
    // eslint-disable-next-line react-hooks/purity -- Date comparison is intentionally impure
    const now = Date.now()
    const sevenDaysMs = 7 * 24 * 60 * 60 * 1000
    const total = allKeys.length
    const active = allKeys.filter((k) => {
      if (!k.expires_at) return true
      return new Date(k.expires_at).getTime() > now
    }).length
    const expiring = allKeys.filter((k) => {
      if (!k.expires_at) return false
      const exp = new Date(k.expires_at).getTime()
      return exp > now && exp - now <= sevenDaysMs
    }).length
    return [total, active, expiring] as const
  }, [allKeys])

  const columns: Column<APIKeyResponse>[] = [
    {
      key: 'key_hint',
      header: 'Key',
      render: (row) => (
        <span className="font-mono text-xs">
          <KeyHint hint={row.key_hint} />
        </span>
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
          <div className="flex items-center justify-end gap-0.5">
            <Button
              variant="ghost"
              size="sm"
              className="!px-1.5"
              title="Edit key"
              onClick={() => setEditKey(row)}
              disabled={deleteKey.isPending || rotateKey.isPending}
            >
              <IconPencil className="h-4 w-4" />
            </Button>
            <Button
              variant="ghost"
              size="sm"
              className="!px-1.5"
              title="Rotate key"
              onClick={() => setRotateKeyId(row.id)}
              disabled={deleteKey.isPending || rotateKey.isPending}
            >
              <IconRefresh className="h-4 w-4" />
            </Button>
            <Button
              variant="ghost"
              size="sm"
              className="!px-1.5 text-error hover:text-error"
              title="Revoke key"
              onClick={() => setRevokeKeyId(row.id)}
              disabled={deleteKey.isPending || rotateKey.isPending}
            >
              <IconTrash className="h-4 w-4" />
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

  // Empty state (not loading, no keys, orgId resolved)
  const showEmptyState = !isLoading && !!orgId && allKeys.length === 0 && !keys?.has_more

  return (
    <>
      <PageHeader
        title="API Keys"
        description="Manage your API keys"
        actions={
          <Button onClick={() => setShowCreateDialog(true)}>Create Key</Button>
        }
      />

      {/* Stat cards */}
      <div className="grid grid-cols-3 gap-4 mb-6">
        <StatCard
          label="Total Keys"
          value={totalKeys}
          iconColor="purple"
          icon={<IconKey className="h-4 w-4" />}
        />
        <StatCard
          label="Active Keys"
          value={activeKeys}
          iconColor="green"
          icon={<IconCheck className="h-4 w-4" />}
        />
        <StatCard
          label="Expiring Soon"
          value={expiringSoon}
          iconColor="yellow"
          icon={<IconClock className="h-4 w-4" />}
        />
      </div>

      {showEmptyState ? (
        <div className="flex flex-col items-center justify-center rounded-xl border border-border bg-bg-secondary py-16">
          <span className="mb-4 flex h-14 w-14 items-center justify-center rounded-full bg-bg-tertiary">
            <IconKey className="h-7 w-7 text-text-tertiary" />
          </span>
          <h3 className="mb-1 text-base font-medium text-text-primary">No API keys yet</h3>
          <p className="mb-6 text-sm text-text-secondary">
            Create your first key to start using the proxy
          </p>
          <Button onClick={() => setShowCreateDialog(true)}>Create Key</Button>
        </div>
      ) : (
        <Table<APIKeyResponse>
          columns={columns}
          data={allKeys}
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
      )}

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
