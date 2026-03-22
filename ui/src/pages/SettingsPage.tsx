import React, { useState } from 'react'
import { Input } from '../components/ui/Input'
import { Button } from '../components/ui/Button'
import { useMe } from '../hooks/useMe'
import { useOrg, useUpdateOrg } from '../hooks/useOrg'
import type { OrgResponse, UpdateOrgParams } from '../hooks/useOrg'
import { useToast } from '../hooks/useToast'

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function canEditOrg(role: string | undefined): boolean {
  return role === 'org_admin' || role === 'system_admin'
}

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

function formFromOrg(org: OrgResponse): FormState {
  return {
    name: org.name,
    slug: org.slug,
    dailyTokenLimit: String(org.daily_token_limit),
    monthlyTokenLimit: String(org.monthly_token_limit),
    requestsPerMinute: String(org.requests_per_minute),
    requestsPerDay: String(org.requests_per_day),
  }
}

// ---------------------------------------------------------------------------
// OrgSettingsForm — mounted only once org data is available.
// Receives org as a prop so initial state can be set without useEffect.
// ---------------------------------------------------------------------------

interface OrgSettingsFormProps {
  org: OrgResponse
  readOnly: boolean
}

function OrgSettingsForm({ org, readOnly }: OrgSettingsFormProps) {
  const updateOrg = useUpdateOrg(org.id)
  const { toast } = useToast()

  const [form, setForm] = useState<FormState>(() => formFromOrg(org))
  const [nameError, setNameError] = useState<string | undefined>()
  const [slugError, setSlugError] = useState<string | undefined>()

  const isDirty =
    form.name !== org.name ||
    form.slug !== org.slug ||
    parseLimit(form.dailyTokenLimit) !== org.daily_token_limit ||
    parseLimit(form.monthlyTokenLimit) !== org.monthly_token_limit ||
    parseLimit(form.requestsPerMinute) !== org.requests_per_minute ||
    parseLimit(form.requestsPerDay) !== org.requests_per_day

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

    const params: UpdateOrgParams = {
      name: form.name.trim(),
      slug: form.slug.trim(),
      daily_token_limit: parseLimit(form.dailyTokenLimit),
      monthly_token_limit: parseLimit(form.monthlyTokenLimit),
      requests_per_minute: parseLimit(form.requestsPerMinute),
      requests_per_day: parseLimit(form.requestsPerDay),
    }

    updateOrg.mutate(params, {
      onSuccess: () => {
        toast({ variant: 'success', message: 'Settings saved' })
      },
      onError: (err) => {
        toast({
          variant: 'error',
          message: err instanceof Error ? err.message : 'Failed to save settings',
        })
      },
    })
  }

  return (
    <form onSubmit={handleSubmit} noValidate className="p-6 space-y-6">
      <div className="space-y-4">
        <Input
          label="Name"
          value={form.name}
          onChange={patch('name')}
          placeholder="e.g. VoidMind GmbH"
          error={nameError}
          disabled={readOnly || updateOrg.isPending}
        />
        <Input
          label="Slug"
          value={form.slug}
          onChange={patch('slug')}
          placeholder="e.g. voidmind"
          description="Used in URLs and API references. Lowercase letters, numbers, and hyphens only."
          error={slugError}
          disabled={readOnly || updateOrg.isPending}
          className="font-mono"
        />
      </div>

      <div>
        <div className="flex items-center gap-3 mb-4">
          <div className="h-px flex-1 bg-border" />
          <span className="text-xs font-medium text-text-tertiary uppercase tracking-wider">
            Limits
          </span>
          <div className="h-px flex-1 bg-border" />
        </div>

        <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
          <Input
            label="Daily Token Limit"
            type="number"
            min="0"
            value={form.dailyTokenLimit}
            onChange={patch('dailyTokenLimit')}
            description="0 = unlimited"
            disabled={readOnly || updateOrg.isPending}
          />
          <Input
            label="Monthly Token Limit"
            type="number"
            min="0"
            value={form.monthlyTokenLimit}
            onChange={patch('monthlyTokenLimit')}
            description="0 = unlimited"
            disabled={readOnly || updateOrg.isPending}
          />
          <Input
            label="Requests / Minute"
            type="number"
            min="0"
            value={form.requestsPerMinute}
            onChange={patch('requestsPerMinute')}
            description="0 = unlimited"
            disabled={readOnly || updateOrg.isPending}
          />
          <Input
            label="Requests / Day"
            type="number"
            min="0"
            value={form.requestsPerDay}
            onChange={patch('requestsPerDay')}
            description="0 = unlimited"
            disabled={readOnly || updateOrg.isPending}
          />
        </div>
      </div>

      {readOnly ? (
        <p className="text-sm text-text-tertiary">
          Contact your org admin to change settings.
        </p>
      ) : (
        <div className="flex justify-end">
          <Button
            type="submit"
            loading={updateOrg.isPending}
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
// OrgSettingsCard — handles loading state, then delegates to OrgSettingsForm.
// The `key={org.id}` ensures the form re-initialises if the org ever changes.
// ---------------------------------------------------------------------------

interface OrgSettingsCardProps {
  orgId: string
  readOnly: boolean
}

function OrgSettingsCard({ orgId, readOnly }: OrgSettingsCardProps) {
  const { data: org, isLoading } = useOrg(orgId)

  return (
    <div className="rounded-lg border border-border bg-bg-secondary">
      <div className="px-6 py-4 border-b border-border">
        <h2 className="text-sm font-semibold text-text-primary">Organization</h2>
      </div>

      {isLoading || !org ? (
        <div className="p-6 space-y-4">
          <div className="h-4 w-32 rounded bg-bg-tertiary animate-pulse" />
          <div className="h-9 w-full rounded bg-bg-tertiary animate-pulse" />
          <div className="h-9 w-full rounded bg-bg-tertiary animate-pulse" />
          <div className="h-px bg-border" />
          <div className="h-4 w-24 rounded bg-bg-tertiary animate-pulse" />
          <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
            <div className="h-9 rounded bg-bg-tertiary animate-pulse" />
            <div className="h-9 rounded bg-bg-tertiary animate-pulse" />
            <div className="h-9 rounded bg-bg-tertiary animate-pulse" />
            <div className="h-9 rounded bg-bg-tertiary animate-pulse" />
          </div>
        </div>
      ) : (
        <OrgSettingsForm key={org.id} org={org} readOnly={readOnly} />
      )}
    </div>
  )
}

// ---------------------------------------------------------------------------
// SettingsPage
// ---------------------------------------------------------------------------

export default function SettingsPage() {
  const { data: me } = useMe()
  const orgId = me?.org_id ?? ''
  const readOnly = !canEditOrg(me?.role)

  return (
    <div className="max-w-2xl space-y-6">
      {orgId && <OrgSettingsCard orgId={orgId} readOnly={readOnly} />}
    </div>
  )
}
