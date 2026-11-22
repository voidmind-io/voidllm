import React, { useState } from 'react'
import { useParams } from 'react-router-dom'
import { Input } from '../components/ui/Input'
import { Button } from '../components/ui/Button'
import { useOrg, useUpdateOrg } from '../hooks/useOrg'
import type { OrgResponse, UpdateOrgParams } from '../hooks/useOrg'
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
// OrgSettingsForm
// ---------------------------------------------------------------------------

interface OrgSettingsFormProps {
  org: OrgResponse
}

function OrgSettingsForm({ org }: OrgSettingsFormProps) {
  const updateOrg = useUpdateOrg(org.id)
  const { toast } = useToast()

  const [form, setForm] = useState<FormState>(() => formFromOrg(org))
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
    form.name !== org.name ||
    form.slug !== org.slug ||
    dailyTokenLimit.value !== org.daily_token_limit ||
    monthlyTokenLimit.value !== org.monthly_token_limit ||
    requestsPerMinute.value !== org.requests_per_minute ||
    requestsPerDay.value !== org.requests_per_day

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

    const params: UpdateOrgParams = {}
    const name = form.name.trim()
    const slug = form.slug.trim()
    if (name !== org.name) params.name = name
    if (slug !== org.slug) params.slug = slug
    if (dailyTokenLimit.value !== org.daily_token_limit) params.daily_token_limit = dailyTokenLimit.value
    if (monthlyTokenLimit.value !== org.monthly_token_limit) params.monthly_token_limit = monthlyTokenLimit.value
    if (requestsPerMinute.value !== org.requests_per_minute) params.requests_per_minute = requestsPerMinute.value
    if (requestsPerDay.value !== org.requests_per_day) params.requests_per_day = requestsPerDay.value

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
    <form onSubmit={handleSubmit} noValidate className="space-y-6">
      {/* Card 1: Basic Information */}
      <div className="bg-bg-secondary rounded-xl border border-border p-6 space-y-4">
        <h3 className="text-[10px] font-medium tracking-widest uppercase text-text-tertiary mb-4">
          Basic Information
        </h3>
        <Input
          label="Name"
          value={form.name}
          onChange={patch('name')}
          placeholder="e.g. Acme Corp"
          error={nameError}
          disabled={updateOrg.isPending}
        />
        <Input
          label="Slug"
          value={form.slug}
          onChange={patch('slug')}
          placeholder="e.g. acme-corp"
          description="Used in URLs and API references. Lowercase letters, numbers, and hyphens only."
          error={slugError}
          disabled={updateOrg.isPending}
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
            disabled={updateOrg.isPending}
          />
          <Input
            label="Monthly Token Limit"
            type="text"
            inputMode="numeric"
            value={form.monthlyTokenLimit}
            onChange={patch('monthlyTokenLimit')}
            description="0 = unlimited"
            error={monthlyTokenLimit.error}
            disabled={updateOrg.isPending}
          />
          <Input
            label="Requests / Minute"
            type="text"
            inputMode="numeric"
            value={form.requestsPerMinute}
            onChange={patch('requestsPerMinute')}
            description="0 = unlimited"
            error={requestsPerMinute.error}
            disabled={updateOrg.isPending}
          />
          <Input
            label="Requests / Day"
            type="text"
            inputMode="numeric"
            value={form.requestsPerDay}
            onChange={patch('requestsPerDay')}
            description="0 = unlimited"
            error={requestsPerDay.error}
            disabled={updateOrg.isPending}
          />
        </div>
      </div>

      <div className="flex justify-end">
        <Button
          type="submit"
          loading={updateOrg.isPending}
          disabled={!isDirty || hasLimitError}
        >
          Save Changes
        </Button>
      </div>
    </form>
  )
}

// ---------------------------------------------------------------------------
// OrgSettingsCard — handles loading/skeleton, then renders form.
// ---------------------------------------------------------------------------

interface OrgSettingsCardProps {
  orgId: string
}

function OrgSettingsCard({ orgId }: OrgSettingsCardProps) {
  const { data: org, isLoading } = useOrg(orgId)

  if (isLoading || !org) {
    return (
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
    )
  }

  return <OrgSettingsForm key={org.id} org={org} />
}

// ---------------------------------------------------------------------------
// OrgDetailSettingsTab
// ---------------------------------------------------------------------------

export default function OrgDetailSettingsTab() {
  const { orgId = '' } = useParams<{ orgId: string }>()

  return (
    <div className="max-w-3xl">
      {orgId && <OrgSettingsCard orgId={orgId} />}
    </div>
  )
}
