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
  multi_org:        'Multi-org management',
  cost_reports:     'Cost reports + budget alerts',
  audit_logs:       'Audit logs',
  sso_oidc:         'SSO / OIDC integration',
  custom_roles:     'Custom roles',
  otel_tracing:     'OpenTelemetry tracing',
  fallback_chains:  'Model fallback chains',
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
  'Model fallback chains',
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

// ---------------------------------------------------------------------------
// Icons
// ---------------------------------------------------------------------------

function IconTag() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M20.59 13.41l-7.17 7.17a2 2 0 0 1-2.83 0L2 12V2h10l8.59 8.59a2 2 0 0 1 0 2.82z" />
      <line x1="7" y1="7" x2="7.01" y2="7" />
    </svg>
  )
}

function IconCheckCircle() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M22 11.08V12a10 10 0 1 1-5.93-9.14" />
      <polyline points="22 4 12 14.01 9 11.01" />
    </svg>
  )
}

function IconCalendar() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <rect x="3" y="4" width="18" height="18" rx="2" ry="2" />
      <line x1="16" y1="2" x2="16" y2="6" />
      <line x1="8" y1="2" x2="8" y2="6" />
      <line x1="3" y1="10" x2="21" y2="10" />
    </svg>
  )
}

function IconCheck() {
  return (
    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <polyline points="20 6 9 17 4 12" />
    </svg>
  )
}

function IconX() {
  return (
    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <line x1="18" y1="6" x2="6" y2="18" />
      <line x1="6" y1="6" x2="18" y2="18" />
    </svg>
  )
}

// ---------------------------------------------------------------------------
// Helper functions
// ---------------------------------------------------------------------------

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

function FeatureRow({ label, enabled }: { label: string; enabled: boolean }) {
  return (
    <div className={['flex items-center gap-2 text-sm', enabled ? 'text-text-secondary' : 'text-text-tertiary'].join(' ')}>
      <span className={enabled ? 'text-success' : 'text-text-tertiary'} aria-hidden="true">
        {enabled ? <IconCheck /> : <IconX />}
      </span>
      {label}
    </div>
  )
}

// ---------------------------------------------------------------------------
// CurrentPlanPanel
// ---------------------------------------------------------------------------

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
            <span className="text-success" aria-hidden="true"><IconCheck /></span>
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

// ---------------------------------------------------------------------------
// LoadingSkeleton
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// LicensePage
// ---------------------------------------------------------------------------

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
          <StatCard
            label="Plan"
            value={planLabel(license.edition)}
            icon={<IconTag />}
            iconColor="purple"
          />
          <StatCard
            label="Status"
            value={statusLabel(license.valid ? 'active' : 'expired')}
            icon={<IconCheckCircle />}
            iconColor={license.valid ? 'green' : 'red'}
          />
          <StatCard
            label="Expires"
            value={license.expires_at != null ? formatDate(license.expires_at) : 'Never'}
            icon={<IconCalendar />}
            iconColor="blue"
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
                      <span className="text-success" aria-hidden="true"><IconCheck /></span>
                      {f}
                    </div>
                  ))}
                </div>

                <Button
                  variant="primary"
                  onClick={() => window.open('https://voidllm.ai/pricing', '_blank')}
                >
                  Upgrade to Pro
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
                    <span className="text-success" aria-hidden="true"><IconCheck /></span>
                    {f}
                  </div>
                ))}
              </div>

              <Button
                variant="secondary"
                onClick={() => window.open('https://voidllm.ai/pricing', '_blank')}
              >
                Contact Sales
              </Button>
            </div>
          </div>
        )}
      </div>
    </>
  )
}
