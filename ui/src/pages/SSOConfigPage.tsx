import { Link, Navigate } from 'react-router-dom'
import { PageHeader } from '../components/ui/PageHeader'
import { Badge } from '../components/ui/Badge'
import { useMe } from '../hooks/useMe'
import { useGlobalSSO } from '../hooks/useOrgSSO'
import { useOrgs } from '../hooks/useOrgs'

// ---------------------------------------------------------------------------
// ReadOnlyField
// ---------------------------------------------------------------------------

interface ReadOnlyFieldProps {
  label: string
  value: React.ReactNode
}

function ReadOnlyField({ label, value }: ReadOnlyFieldProps) {
  return (
    <div>
      <p className="text-xs font-medium text-text-tertiary uppercase tracking-wider mb-1">
        {label}
      </p>
      <div className="text-sm text-text-primary">{value}</div>
    </div>
  )
}

// ---------------------------------------------------------------------------
// GlobalSSOCard — shown to system admins
// ---------------------------------------------------------------------------

function GlobalSSOCard() {
  const { data: config, isLoading, isError } = useGlobalSSO()

  return (
    <div className="rounded-lg border border-border bg-bg-secondary">
      <div className="px-6 py-4 border-b border-border flex items-center justify-between">
        <div>
          <h2 className="text-sm font-semibold text-text-primary">Global Configuration</h2>
          <p className="text-xs text-text-tertiary mt-0.5">
            Set via <span className="font-mono">voidllm.yaml</span> — read-only from the UI
          </p>
        </div>
        {config && (
          <Badge variant={config.enabled ? 'success' : 'muted'}>
            {config.enabled ? 'Enabled' : 'Disabled'}
          </Badge>
        )}
      </div>

      <div className="p-6">
        {isLoading && (
          <div className="space-y-4 animate-pulse">
            <div className="h-4 w-48 rounded bg-bg-tertiary" />
            <div className="h-4 w-full rounded bg-bg-tertiary" />
            <div className="h-4 w-3/4 rounded bg-bg-tertiary" />
            <div className="h-4 w-2/3 rounded bg-bg-tertiary" />
          </div>
        )}

        {isError && (
          <p className="text-sm text-text-tertiary">
            No global SSO configuration found. Configure one in{' '}
            <span className="font-mono text-xs">voidllm.yaml</span>.
          </p>
        )}

        {config && (
          <div className="space-y-4">
            <ReadOnlyField
              label="Issuer URL"
              value={<span className="font-mono">{config.issuer || '—'}</span>}
            />
            <ReadOnlyField
              label="Client ID"
              value={<span className="font-mono">{config.client_id || '—'}</span>}
            />
            <ReadOnlyField
              label="Client Secret"
              value={
                config.has_secret ? (
                  <span className="font-mono text-text-secondary">configured</span>
                ) : (
                  '—'
                )
              }
            />
            <ReadOnlyField
              label="Redirect URL"
              value={<span className="font-mono break-all">{config.redirect_url || '—'}</span>}
            />
            <ReadOnlyField
              label="Scopes"
              value={
                config.scopes?.length ? (
                  <div className="flex flex-wrap gap-1">
                    {config.scopes.map((s) => (
                      <Badge key={s} variant="muted">{s}</Badge>
                    ))}
                  </div>
                ) : (
                  '—'
                )
              }
            />
            <ReadOnlyField
              label="Allowed Domains"
              value={
                config.allowed_domains?.length ? (
                  <div className="flex flex-wrap gap-1">
                    {config.allowed_domains.map((d) => (
                      <Badge key={d} variant="muted">{d}</Badge>
                    ))}
                  </div>
                ) : (
                  <span className="text-text-secondary">All domains</span>
                )
              }
            />
            <ReadOnlyField
              label="Auto-Provision"
              value={config.auto_provision ? 'Yes' : 'No'}
            />
            <ReadOnlyField label="Default Role" value={config.default_role || '—'} />
            {config.group_sync && (
              <ReadOnlyField
                label="Group Claim"
                value={<span className="font-mono">{config.group_claim || '—'}</span>}
              />
            )}
          </div>
        )}
      </div>
    </div>
  )
}

// ---------------------------------------------------------------------------
// OrgSSOLinksCard — quick links to per-org SSO tabs
// ---------------------------------------------------------------------------

function OrgSSOLinksCard() {
  const { data, isLoading } = useOrgs()
  const orgs = data?.data ?? []

  return (
    <div className="rounded-lg border border-border bg-bg-secondary">
      <div className="px-6 py-4 border-b border-border">
        <h2 className="text-sm font-semibold text-text-primary">Per-Org Configuration</h2>
        <p className="text-xs text-text-tertiary mt-0.5">
          Override the global config for individual organizations
        </p>
      </div>

      <div className="divide-y divide-border">
        {isLoading && (
          <div className="p-6 space-y-3 animate-pulse">
            <div className="h-4 w-40 rounded bg-bg-tertiary" />
            <div className="h-4 w-56 rounded bg-bg-tertiary" />
            <div className="h-4 w-36 rounded bg-bg-tertiary" />
          </div>
        )}

        {!isLoading && orgs.length === 0 && (
          <p className="px-6 py-4 text-sm text-text-tertiary">No organizations found.</p>
        )}

        {orgs.map((org) => (
          <Link
            key={org.id}
            to={`/orgs/${org.id}/sso`}
            className="flex items-center justify-between px-6 py-3 no-underline hover:bg-bg-tertiary transition-colors"
          >
            <div>
              <p className="text-sm font-medium text-text-primary">{org.name}</p>
              <p className="text-xs text-text-tertiary font-mono">{org.slug}</p>
            </div>
            <svg
              aria-hidden="true"
              className="h-4 w-4 text-text-tertiary"
              fill="none"
              viewBox="0 0 24 24"
              stroke="currentColor"
              strokeWidth={2}
            >
              <path strokeLinecap="round" strokeLinejoin="round" d="M9 5l7 7-7 7" />
            </svg>
          </Link>
        ))}
      </div>
    </div>
  )
}

// ---------------------------------------------------------------------------
// SSOConfigPage
// ---------------------------------------------------------------------------

export default function SSOConfigPage() {
  const { data: me, isLoading } = useMe()

  // While loading, render nothing to avoid a flash redirect
  if (isLoading) return null

  // org_admin (non-system-admin): redirect to their org's SSO tab
  if (me && !me.is_system_admin && me.org_id) {
    return <Navigate to={`/orgs/${me.org_id}/sso`} replace />
  }

  return (
    <>
      <PageHeader
        title="SSO Configuration"
        description="Manage OIDC single sign-on settings globally and per organization"
      />
      <div className="max-w-3xl space-y-6">
        <GlobalSSOCard />
        <OrgSSOLinksCard />
      </div>
    </>
  )
}
