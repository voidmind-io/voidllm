import React, { useState } from 'react'
import { useParams } from 'react-router-dom'
import { Input } from '../components/ui/Input'
import { Button } from '../components/ui/Button'
import { useMe } from '../hooks/useMe'
import { useTeam, useUpdateTeam } from '../hooks/useTeams'
import type { CreateTeamParams, TeamResponse } from '../hooks/useTeams'
import { useToast } from '../hooks/useToast'
import { parseLimitInput } from '../lib/utils'

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

interface FormState {
  name: string
  slug: string
  dailyTokenLimit: string
  monthlyTokenLimit: string
  requestsPerMinute: string
  requestsPerDay: string
}

function formFromTeam(team: TeamResponse): FormState {
  return {
    name: team.name,
    slug: team.slug,
    dailyTokenLimit: String(team.daily_token_limit),
    monthlyTokenLimit: String(team.monthly_token_limit),
    requestsPerMinute: String(team.requests_per_minute),
    requestsPerDay: String(team.requests_per_day),
  }
}

// ---------------------------------------------------------------------------
// TeamSettingsForm
// ---------------------------------------------------------------------------

interface TeamSettingsFormProps {
  team: TeamResponse
  orgId: string
  canEdit: boolean
}

function TeamSettingsForm({ team, orgId, canEdit }: TeamSettingsFormProps) {
  const updateTeam = useUpdateTeam(orgId, team.id)
  const { toast } = useToast()

  const [form, setForm] = useState<FormState>(() => formFromTeam(team))
  // Snapshot of the values the user saw; changes are computed against it so a
  // background refetch cannot cause untouched fields to overwrite newer values.
  const [base, setBase] = useState<TeamResponse>(team)
  const [nameError, setNameError] = useState<string | undefined>()
  const [slugError, setSlugError] = useState<string | undefined>()

  const dailyTokenLimit = parseLimitInput(form.dailyTokenLimit)
  const monthlyTokenLimit = parseLimitInput(form.monthlyTokenLimit)
  const requestsPerMinute = parseLimitInput(form.requestsPerMinute)
  const requestsPerDay = parseLimitInput(form.requestsPerDay)

  const hasLimitError = Boolean(
    dailyTokenLimit.error ||
      monthlyTokenLimit.error ||
      requestsPerMinute.error ||
      requestsPerDay.error,
  )

  const isDirty =
    form.name !== base.name ||
    form.slug !== base.slug ||
    dailyTokenLimit.value !== base.daily_token_limit ||
    monthlyTokenLimit.value !== base.monthly_token_limit ||
    requestsPerMinute.value !== base.requests_per_minute ||
    requestsPerDay.value !== base.requests_per_day

  function patch(field: keyof FormState) {
    return (e: React.ChangeEvent<HTMLInputElement>) =>
      setForm((prev) => ({ ...prev, [field]: e.target.value }))
  }

  function validate(): boolean {
    let valid = true

    if (!form.name.trim()) {
      setNameError('Name is required')
      valid = false
    } else {
      setNameError(undefined)
    }

    if (!form.slug.trim()) {
      setSlugError('Slug is required')
      valid = false
    } else {
      setSlugError(undefined)
    }

    return valid
  }

  function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (!validate() || hasLimitError) return

    const params: Partial<CreateTeamParams> = {}
    const name = form.name.trim()
    const slug = form.slug.trim()
    if (name !== base.name) params.name = name
    if (slug !== base.slug) params.slug = slug
    if (dailyTokenLimit.value !== base.daily_token_limit) params.daily_token_limit = dailyTokenLimit.value
    if (monthlyTokenLimit.value !== base.monthly_token_limit) params.monthly_token_limit = monthlyTokenLimit.value
    if (requestsPerMinute.value !== base.requests_per_minute) params.requests_per_minute = requestsPerMinute.value
    if (requestsPerDay.value !== base.requests_per_day) params.requests_per_day = requestsPerDay.value

    updateTeam.mutate(params,
      {
        onSuccess: () => {
          setBase((prev) => ({ ...prev, ...params }))
          toast({ variant: 'success', message: 'Team settings saved' })
        },
        onError: (err) => {
          toast({
            variant: 'error',
            message: err instanceof Error ? err.message : 'Failed to save settings',
          })
        },
      },
    )
  }

  return (
    <form onSubmit={handleSubmit} noValidate className="space-y-6">
      {!canEdit && (
        <p className="text-sm text-text-secondary">
          Only team admins can modify team settings.
        </p>
      )}

      {/* Card 1: Basic Information */}
      <div className="bg-bg-secondary rounded-xl border border-border p-6 space-y-4">
        <h3 className="text-[10px] font-medium tracking-widest uppercase text-text-tertiary mb-4">
          Basic Information
        </h3>
        <Input
          label="Name"
          value={form.name}
          onChange={patch('name')}
          placeholder="e.g. Backend Engineering"
          error={nameError}
          disabled={!canEdit || updateTeam.isPending}
        />
        <Input
          label="Slug"
          value={form.slug}
          onChange={patch('slug')}
          placeholder="e.g. backend-engineering"
          description="Used in URLs and API references. Lowercase letters, numbers, and hyphens only."
          error={slugError}
          disabled={!canEdit || updateTeam.isPending}
          className="font-mono"
        />
      </div>

      {/* Card 2: Rate & Token Limits */}
      <div className="bg-bg-secondary rounded-xl border border-border p-6 space-y-4">
        <h3 className="text-[10px] font-medium tracking-widest uppercase text-text-tertiary mb-4">
          Rate &amp; Token Limits
        </h3>
        <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
          <Input
            label="Daily Token Limit"
            type="text"
            inputMode="numeric"
            value={form.dailyTokenLimit}
            onChange={patch('dailyTokenLimit')}
            description="0 = unlimited"
            error={dailyTokenLimit.error}
            disabled={!canEdit || updateTeam.isPending}
          />
          <Input
            label="Monthly Token Limit"
            type="text"
            inputMode="numeric"
            value={form.monthlyTokenLimit}
            onChange={patch('monthlyTokenLimit')}
            description="0 = unlimited"
            error={monthlyTokenLimit.error}
            disabled={!canEdit || updateTeam.isPending}
          />
          <Input
            label="Requests / Minute"
            type="text"
            inputMode="numeric"
            value={form.requestsPerMinute}
            onChange={patch('requestsPerMinute')}
            description="0 = unlimited"
            error={requestsPerMinute.error}
            disabled={!canEdit || updateTeam.isPending}
          />
          <Input
            label="Requests / Day"
            type="text"
            inputMode="numeric"
            value={form.requestsPerDay}
            onChange={patch('requestsPerDay')}
            description="0 = unlimited"
            error={requestsPerDay.error}
            disabled={!canEdit || updateTeam.isPending}
          />
        </div>
      </div>

      {canEdit && (
        <div className="flex justify-end">
          <Button
            type="submit"
            loading={updateTeam.isPending}
            disabled={!isDirty || hasLimitError}
          >
            Save Changes
          </Button>
        </div>
      )}
    </form>
  )
}

// ---------------------------------------------------------------------------
// TeamSettingsCard — handles loading state, then delegates to TeamSettingsForm.
// ---------------------------------------------------------------------------

interface TeamSettingsCardProps {
  orgId: string
  teamId: string
  canEdit: boolean
}

function TeamSettingsCard({ orgId, teamId, canEdit }: TeamSettingsCardProps) {
  const { data: team, isLoading } = useTeam(orgId, teamId)

  return (
    <div className="max-w-3xl">
      {isLoading || !team ? (
        <div className="space-y-6">
          {/* Basic Information skeleton */}
          <div className="bg-bg-secondary rounded-xl border border-border p-6 space-y-4">
            <div className="h-3 w-32 rounded bg-bg-tertiary animate-pulse" />
            <div className="h-9 w-full rounded bg-bg-tertiary animate-pulse" />
            <div className="h-9 w-full rounded bg-bg-tertiary animate-pulse" />
          </div>
          {/* Rate & Token Limits skeleton */}
          <div className="bg-bg-secondary rounded-xl border border-border p-6 space-y-4">
            <div className="h-3 w-40 rounded bg-bg-tertiary animate-pulse" />
            <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
              <div className="h-9 rounded bg-bg-tertiary animate-pulse" />
              <div className="h-9 rounded bg-bg-tertiary animate-pulse" />
              <div className="h-9 rounded bg-bg-tertiary animate-pulse" />
              <div className="h-9 rounded bg-bg-tertiary animate-pulse" />
            </div>
          </div>
        </div>
      ) : (
        <TeamSettingsForm key={team.id} team={team} orgId={orgId} canEdit={canEdit} />
      )}
    </div>
  )
}

// ---------------------------------------------------------------------------
// TeamSettingsTab
// ---------------------------------------------------------------------------

export default function TeamSettingsTab() {
  const { teamId = '' } = useParams<{ teamId: string }>()
  const { data: me } = useMe()
  const orgId = me?.org_id ?? ''
  const canEdit =
    me?.role === 'org_admin' ||
    me?.role === 'system_admin' ||
    me?.role === 'team_admin'

  return <TeamSettingsCard orgId={orgId} teamId={teamId} canEdit={canEdit} />
}
