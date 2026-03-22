import { useState } from 'react'
import { Navigate } from 'react-router-dom'
import { PageHeader } from '../components/ui/PageHeader'
import { Badge } from '../components/ui/Badge'
import { Button } from '../components/ui/Button'
import { Input } from '../components/ui/Input'
import { StatCard } from '../components/ui/StatCard'
import { useMe } from '../hooks/useMe'
import { useLicense, useActivateLicense } from '../hooks/useLicense'
import type { LicenseInfo } from '../hooks/useLicense'
import { useToast } from '../hooks/useToast'
import { formatDate } from '../lib/utils'

// Human-readable labels for feature flag keys returned by the API.
const FEATURE_LABELS: Record<string, string> = {
  multi_org:     'Multi-org management',
  cost_reports:  'Cost reports + budget alerts',
  audit_logs:    'Audit logs',
  sso_oidc:      'SSO / OIDC integration',
  custom_roles:  'Custom roles',
  otel_tracing:  'OpenTelemetry tracing',
}

const proFeatures = [
  'Multi-org management',
  'Cost reports + budget alerts',
  'Usage export (CSV/JSON)',
  'Cross-org analytics',
  'Unlimited data retention',
  'Priority email support (48h)',
]

const enterpriseFeatures = [
  'SSO / OIDC integration',
  'Audit logs',
  'Custom roles',
  'OpenTelemetry tracing',
  'Distributed rate limiting (Redis)',
  'Unlimited data retention',
  'Dedicated Slack support (24h)',
]

// Community plan always-on capabilities shown regardless of feature flags.
const communityCapabilities = [
  'Unlimited users',
  'Full proxy with all providers',
  'Usage tracking + analytics',
  'Model access control',
  'RBAC (4 built-in roles)',
  'Invite system',
  'Playground',
  'API documentation',
]

function planBadgeVariant(plan: string): 'muted' | 'default' | 'success' {
  if (plan === 'enterprise') return 'success'
  if (plan === 'pro') return 'default'
  return 'muted'
}

function planLabel(plan: string): string {
  if (plan === 'enterprise') return 'Enterprise'
  if (plan === 'pro') return 'Pro'
  return 'Community'
}

function statusBadgeVariant(status: string): 'success' | 'error' | 'muted' {
  if (status === 'active') return 'success'
  if (status === 'expired') return 'error'
  return 'muted'
}

function statusLabel(status: string): string {
  if (status === 'active') return 'Active'
  if (status === 'expired') return 'Expired'
  return 'Community'
}

function limitLabel(n: number): string {
  return n < 0 ? 'Unlimited' : String(n)
}

function CheckIcon() {
  return <span className="text-success" aria-hidden="true">✓</span>
}

function CrossIcon() {
  return <span className="text-text-tertiary" aria-hidden="true">✗</span>
}

function FeatureRow({ label, enabled }: { label: string; enabled: boolean }) {
  return (
    <div className={['flex items-center gap-2 text-sm', enabled ? 'text-text-secondary' : 'text-text-tertiary'].join(' ')}>
      {enabled ? <CheckIcon /> : <CrossIcon />}
      {label}
    </div>
  )
}

interface CurrentPlanPanelProps {
  license: LicenseInfo
  licenseKey: string
  onLicenseKeyChange: (v: string) => void
  onActivate: () => void
  activating: boolean
  activateError: string | null
}

function CurrentPlanPanel({
  license,
  licenseKey,
  onLicenseKeyChange,
  onActivate,
  activating,
  activateError,
}: CurrentPlanPanelProps) {
  const isCommunity = license.edition === 'community'

  return (
    <div className="rounded-lg border border-border bg-bg-secondary p-6">
      {/* Plan heading */}
      <div className="flex items-center gap-3 mb-1">
        <h2 className="text-lg font-semibold text-text-primary">Current Plan</h2>
        <Badge variant={planBadgeVariant(license.edition)}>{planLabel(license.edition)}</Badge>
        <Badge variant={statusBadgeVariant(license.valid ? 'active' : 'expired')}>{statusLabel(license.valid ? 'active' : 'expired')}</Badge>
      </div>

      {/* Expiry */}
      {license.expires_at != null && (
        <p className="text-xs text-text-tertiary mb-4">
          {license.valid ? 'Expires' : 'Expired'} {formatDate(license.expires_at)}
        </p>
      )}

      {/* Limits */}
      <div className="flex gap-4 mb-6 mt-4">
        <div className="flex-1 rounded-md bg-bg-tertiary px-3 py-2">
          <div className="text-xs text-text-tertiary mb-0.5">Max Orgs</div>
          <div className="text-sm font-semibold text-text-primary">{limitLabel(license.max_orgs)}</div>
        </div>
        <div className="flex-1 rounded-md bg-bg-tertiary px-3 py-2">
          <div className="text-xs text-text-tertiary mb-0.5">Max Teams</div>
          <div className="text-sm font-semibold text-text-primary">{limitLabel(license.max_teams)}</div>
        </div>
      </div>

      {/* Community capabilities (always enabled) */}
      <div className="space-y-2 mb-4">
        {communityCapabilities.map(f => (
          <div key={f} className="flex items-center gap-2 text-sm text-text-secondary">
            <CheckIcon />
            {f}
          </div>
        ))}
      </div>

      {/* Licensed feature flags */}
      {Object.keys(FEATURE_LABELS).length > 0 && (
        <div className="space-y-2 mb-6 border-t border-border pt-4">
          {Object.entries(FEATURE_LABELS).map(([key, label]) => (
            <FeatureRow
              key={key}
              label={label}
              enabled={license.features.includes(key)}
            />
          ))}
        </div>
      )}

      {/* Customer ID (admin-visible) */}
      {license.customer_id != null && (
        <p className="text-xs text-text-tertiary mb-4">
          Customer ID: <span className="font-mono text-text-secondary">{license.customer_id}</span>
        </p>
      )}

      {/* License key input */}
      <div className="border-t border-border pt-4">
        {!isCommunity && (
          <p className="text-xs text-text-tertiary mb-3">
            Active license: <span className="font-mono text-success">{planLabel(license.edition)}</span>
          </p>
        )}
        <Input
          label={isCommunity ? 'License Key' : 'Replace License Key'}
          type="password"
          value={licenseKey}
          onChange={(e) => onLicenseKeyChange(e.target.value)}
          placeholder="eyJhbGciOiJFZERTQSJ9..."
          description={isCommunity
            ? 'Paste your license key to activate Pro or Enterprise features.'
            : 'Paste a new license key to change your plan.'}
          disabled={activating}
          error={activateError ?? undefined}
          autoComplete="off"
        />
        <Button
          variant="primary"
          size="sm"
          className="mt-2"
          disabled={!licenseKey.trim()}
          loading={activating}
          onClick={onActivate}
        >
          Activate License
        </Button>
      </div>
    </div>
  )
}

function LoadingSkeleton() {
  return (
    <div className="rounded-lg border border-border bg-bg-secondary p-6 space-y-4 animate-pulse">
      <div className="flex items-center gap-3">
        <div className="h-5 w-28 rounded bg-bg-tertiary" />
        <div className="h-5 w-20 rounded bg-bg-tertiary" />
      </div>
      <div className="flex gap-4">
        <div className="flex-1 h-12 rounded bg-bg-tertiary" />
        <div className="flex-1 h-12 rounded bg-bg-tertiary" />
      </div>
      {[...Array(6)].map((_, i) => (
        <div key={i} className="h-4 rounded bg-bg-tertiary" style={{ width: `${70 + (i % 3) * 10}%` }} />
      ))}
    </div>
  )
}

export default function LicensePage() {
  const { data: me } = useMe()
  const { data: license, isLoading } = useLicense()
  const [licenseKey, setLicenseKey] = useState('')
  const [activateError, setActivateError] = useState<string | null>(null)
  const activateLicense = useActivateLicense()
  const { toast } = useToast()

  if (me && !me.is_system_admin) {
    return <Navigate to="/" replace />
  }

  const plan = license?.edition ?? 'community'
  const isCommunity = plan === 'community'
  const isPro = plan === 'pro'

  function handleActivate() {
    setActivateError(null)
    activateLicense.mutate(licenseKey.trim(), {
      onSuccess: () => {
        setLicenseKey('')
        toast({
          variant: 'success',
          message: 'License saved. Restart VoidLLM to activate.',
        })
      },
      onError: (err) => {
        const msg = err instanceof Error ? err.message : 'Failed to activate license'
        setActivateError(msg)
        toast({ variant: 'error', message: msg })
      },
    })
  }

  return (
    <>
      <PageHeader
        title="License"
        description="Manage your VoidLLM license"
      />

      {/* Stat row */}
      {license != null && (
        <div className="grid grid-cols-3 gap-4 mb-6">
          <StatCard label="Plan" value={planLabel(license.edition)} />
          <StatCard label="Status" value={statusLabel(license.valid ? 'active' : 'expired')} />
          <StatCard
            label="Expires"
            value={license.expires_at != null ? formatDate(license.expires_at) : 'Never'}
          />
        </div>
      )}

      <div className="flex gap-6 items-start">
        {/* Left panel — Current Plan (40%) */}
        <div className="w-[40%] shrink-0">
          {isLoading || license == null ? (
            <LoadingSkeleton />
          ) : (
            <CurrentPlanPanel
              license={license}
              licenseKey={licenseKey}
              onLicenseKeyChange={setLicenseKey}
              onActivate={handleActivate}
              activating={activateLicense.isPending}
              activateError={activateError}
            />
          )}
        </div>

        {/* Right panel — Upgrade CTAs (60%) — shown only for non-enterprise plans */}
        {!(!isCommunity && !isPro) && (
          <div className="flex-1 space-y-4">
            {/* Pro card — shown for community users */}
            {isCommunity && (
              <div className="rounded-lg border border-accent/30 bg-bg-secondary p-6">
                <div className="flex items-center justify-between mb-2">
                  <h3 className="text-lg font-semibold text-text-primary">Pro</h3>
                  <span className="text-sm text-accent font-semibold">$299/mo</span>
                </div>
                <p className="text-xs text-text-tertiary mb-4">$2,990/yr (save 2 months)</p>

                <div className="space-y-2 mb-4">
                  {proFeatures.map(f => (
                    <div key={f} className="flex items-center gap-2 text-sm text-text-secondary">
                      <CheckIcon />
                      {f}
                    </div>
                  ))}
                </div>

                <Button
                  variant="primary"
                  onClick={() => window.open('https://voidllm.ai/pricing', '_blank')}
                >
                  Upgrade to Pro →
                </Button>
              </div>
            )}

            {/* Enterprise card — shown for community and pro users */}
            <div className="rounded-lg border border-border bg-bg-secondary p-6">
              <div className="flex items-center justify-between mb-2">
                <h3 className="text-lg font-semibold text-text-primary">Enterprise</h3>
                <span className="text-sm text-text-secondary font-semibold">$799/mo</span>
              </div>
              <p className="text-xs text-text-tertiary mb-4">
                {isCommunity
                  ? '$7,990/yr (save 2 months) · Everything in Pro, plus:'
                  : '$7,990/yr (save 2 months) · Everything in your current plan, plus:'}
              </p>

              <div className="space-y-2 mb-4">
                {enterpriseFeatures.map(f => (
                  <div key={f} className="flex items-center gap-2 text-sm text-text-secondary">
                    <CheckIcon />
                    {f}
                  </div>
                ))}
              </div>

              <Button
                variant="secondary"
                onClick={() => window.open('https://voidllm.ai/pricing', '_blank')}
              >
                Contact Sales →
              </Button>
            </div>
          </div>
        )}
      </div>
    </>
  )
}
