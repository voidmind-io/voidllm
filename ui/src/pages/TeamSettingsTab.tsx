import React, { useState } from 'react'
import { useParams } from 'react-router-dom'
import { Input } from '../components/ui/Input'
import { Button } from '../components/ui/Button'
import { useMe } from '../hooks/useMe'
import { useTeam, useUpdateTeam } from '../hooks/useTeams'
import type { TeamResponse } from '../hooks/useTeams'
import { useToast } from '../hooks/useToast'

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function parseLimit(value: string): number {
  const n = parseInt(value, 10)
  return isNaN(n) || n < 0 ? 0 : n
}

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
  const [nameError, setNameError] = useState<string | undefined>()
  const [slugError, setSlugError] = useState<string | undefined>()

  const isDirty =
    form.name !== team.name ||
    form.slug !== team.slug ||
    parseLimit(form.dailyTokenLimit) !== team.daily_token_limit ||
    parseLimit(form.monthlyTokenLimit) !== team.monthly_token_limit ||
    parseLimit(form.requestsPerMinute) !== team.requests_per_minute ||
    parseLimit(form.requestsPerDay) !== team.requests_per_day

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
    if (!validate()) return

    updateTeam.mutate(
      {
        name: form.name.trim(),
        slug: form.slug.trim(),
        daily_token_limit: parseLimit(form.dailyTokenLimit),
        monthly_token_limit: parseLimit(form.monthlyTokenLimit),
        requests_per_minute: parseLimit(form.requestsPerMinute),
        requests_per_day: parseLimit(form.requestsPerDay),
      },
      {
        onSuccess: () => {
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
            type="number"
            min="0"
            value={form.dailyTokenLimit}
            onChange={patch('dailyTokenLimit')}
            description="0 = unlimited"
            disabled={!canEdit || updateTeam.isPending}
          />
          <Input
            label="Monthly Token Limit"
            type="number"
            min="0"
            value={form.monthlyTokenLimit}
            onChange={patch('monthlyTokenLimit')}
            description="0 = unlimited"
            disabled={!canEdit || updateTeam.isPending}
          />
          <Input
            label="Requests / Minute"
            type="number"
            min="0"
            value={form.requestsPerMinute}
            onChange={patch('requestsPerMinute')}
            description="0 = unlimited"
            disabled={!canEdit || updateTeam.isPending}
          />
          <Input
            label="Requests / Day"
            type="number"
            min="0"
            value={form.requestsPerDay}
            onChange={patch('requestsPerDay')}
            description="0 = unlimited"
            disabled={!canEdit || updateTeam.isPending}
          />
        </div>
      </div>

      {canEdit && (
        <div className="flex justify-end">
          <Button
            type="submit"
            loading={updateTeam.isPending}
            disabled={!isDirty}
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
