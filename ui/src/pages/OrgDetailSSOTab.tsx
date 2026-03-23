import React, { useState } from 'react'
import { useParams } from 'react-router-dom'
import { Input } from '../components/ui/Input'
import { Button } from '../components/ui/Button'
import { Banner } from '../components/ui/Banner'
import { Toggle } from '../components/ui/Toggle'
import { Select } from '../components/ui/Select'
import { Badge } from '../components/ui/Badge'
import { ConfirmDialog } from '../components/ui/Dialog'
import { UpgradePrompt } from '../components/ui/UpgradePrompt'
import { useToast } from '../hooks/useToast'
import { useLicense } from '../hooks/useLicense'
import {
  useOrgSSO,
  useGlobalSSO,
  useSaveOrgSSO,
  useDeleteOrgSSO,
  useTestSSOConnection,
  type SSOConfigResponse,
  type SaveSSOConfigParams,
} from '../hooks/useOrgSSO'

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const DEFAULT_ROLE_OPTIONS = [
  { value: 'member', label: 'Member' },
  { value: 'team_admin', label: 'Team Admin' },
]

const DEFAULT_REDIRECT_URL = `${window.location.origin}/api/v1/auth/oidc/callback`

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function parseCSV(value: string): string[] {
  return value
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
}

function toCSV(values: string[]): string {
  return values.join(', ')
}

// ---------------------------------------------------------------------------
// Read-only SSO config display
// ---------------------------------------------------------------------------

interface ReadOnlyFieldProps {
  label: string
  value: React.ReactNode
}

function ReadOnlyField({ label, value }: ReadOnlyFieldProps) {
  return (
    <div>
      <p className="text-[10px] font-medium tracking-widest uppercase text-text-tertiary mb-1">
        {label}
      </p>
      <div className="text-sm text-text-primary">{value}</div>
    </div>
  )
}

interface GlobalSSODisplayProps {
  config: SSOConfigResponse
}

function GlobalSSODisplay({ config }: GlobalSSODisplayProps) {
  return (
    <div className="space-y-4">
      <ReadOnlyField
        label="Status"
        value={
          config.enabled ? (
            <Badge variant="success">Enabled</Badge>
          ) : (
            <Badge variant="muted">Disabled</Badge>
          )
        }
      />
      <ReadOnlyField label="Issuer URL" value={<span className="font-mono">{config.issuer || '—'}</span>} />
      <ReadOnlyField label="Client ID" value={<span className="font-mono">{config.client_id || '—'}</span>} />
      <ReadOnlyField
        label="Client Secret"
        value={config.has_secret ? <span className="font-mono text-text-secondary">configured</span> : '—'}
      />
      <ReadOnlyField label="Redirect URL" value={<span className="font-mono break-all">{config.redirect_url || '—'}</span>} />
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
        <ReadOnlyField label="Group Claim" value={<span className="font-mono">{config.group_claim || '—'}</span>} />
      )}
    </div>
  )
}

// ---------------------------------------------------------------------------
// SSO form state
// ---------------------------------------------------------------------------

interface SSOFormState {
  enabled: boolean
  issuer: string
  clientId: string
  clientSecret: string
  redirectUrl: string
  scopes: string
  allowedDomains: string
  autoProvision: boolean
  defaultRole: string
  groupSync: boolean
  groupClaim: string
}

function formFromConfig(config: SSOConfigResponse): SSOFormState {
  return {
    enabled: config.enabled,
    issuer: config.issuer,
    clientId: config.client_id,
    clientSecret: '',
    redirectUrl: config.redirect_url,
    scopes: toCSV(config.scopes ?? []),
    allowedDomains: toCSV(config.allowed_domains ?? []),
    autoProvision: config.auto_provision,
    defaultRole: config.default_role,
    groupSync: config.group_sync,
    groupClaim: config.group_claim,
  }
}

function defaultForm(): SSOFormState {
  return {
    enabled: true,
    issuer: '',
    clientId: '',
    clientSecret: '',
    redirectUrl: DEFAULT_REDIRECT_URL,
    scopes: 'openid, email, profile',
    allowedDomains: '',
    autoProvision: true,
    defaultRole: 'member',
    groupSync: false,
    groupClaim: '',
  }
}

function formToParams(form: SSOFormState): SaveSSOConfigParams {
  const params: SaveSSOConfigParams = {
    enabled: form.enabled,
    issuer: form.issuer.trim(),
    client_id: form.clientId.trim(),
    redirect_url: form.redirectUrl.trim(),
    scopes: parseCSV(form.scopes),
    allowed_domains: parseCSV(form.allowedDomains),
    auto_provision: form.autoProvision,
    default_role: form.defaultRole,
    group_sync: form.groupSync,
    group_claim: form.groupClaim.trim(),
  }
  if (form.clientSecret.trim()) {
    params.client_secret = form.clientSecret.trim()
  }
  return params
}

// ---------------------------------------------------------------------------
// Form validation errors
// ---------------------------------------------------------------------------

interface FormErrors {
  issuer?: string
  clientId?: string
}

function validate(form: SSOFormState): FormErrors {
  const errors: FormErrors = {}
  if (!form.issuer.trim()) errors.issuer = 'Issuer URL is required'
  if (!form.clientId.trim()) errors.clientId = 'Client ID is required'
  return errors
}

// ---------------------------------------------------------------------------
// TestConnectionBadge
// ---------------------------------------------------------------------------

type TestStatus = 'idle' | 'loading' | 'success' | 'error'

interface TestConnectionBadgeProps {
  status: TestStatus
  message?: string
}

function TestConnectionBadge({ status, message }: TestConnectionBadgeProps) {
  if (status === 'idle') return null
  if (status === 'loading') {
    return (
      <Badge variant="muted">
        <span
          role="status"
          aria-label="Testing"
          className="inline-block h-3 w-3 animate-spin rounded-full border-2 border-current border-t-transparent"
        />
        Testing…
      </Badge>
    )
  }
  if (status === 'success') {
    return <Badge variant="success">Discovery OK</Badge>
  }
  return <Badge variant="error" title={message}>{message ? 'Failed' : 'Error'}</Badge>
}

// ---------------------------------------------------------------------------
// SSOForm — editable form with save/delete
// ---------------------------------------------------------------------------

interface SSOFormProps {
  orgId: string
  initial: SSOFormState
  hasExistingConfig: boolean
  onDelete?: () => void
}

function SSOForm({ orgId, initial, hasExistingConfig, onDelete }: SSOFormProps) {
  const [form, setForm] = useState<SSOFormState>(initial)
  const [errors, setErrors] = useState<FormErrors>({})
  const [showDeleteConfirm, setShowDeleteConfirm] = useState(false)
  // testedIssuer tracks which issuer URL was last tested so the badge
  // automatically disappears when the user edits the field.
  const [testedIssuer, setTestedIssuer] = useState<string | null>(null)
  const [testStatus, setTestStatus] = useState<TestStatus>('idle')
  const [testMessage, setTestMessage] = useState<string | undefined>()

  const saveSSO = useSaveOrgSSO(orgId)
  const deleteSSO = useDeleteOrgSSO(orgId)
  const testSSO = useTestSSOConnection(orgId)
  const { toast } = useToast()

  // Derive whether the test result is still relevant for the current issuer
  const activeTestStatus: TestStatus =
    testedIssuer === form.issuer ? testStatus : 'idle'
  const activeTestMessage =
    testedIssuer === form.issuer ? testMessage : undefined

  function patch<K extends keyof SSOFormState>(key: K) {
    return (value: SSOFormState[K]) =>
      setForm((prev) => ({ ...prev, [key]: value }))
  }

  function patchInput<K extends keyof SSOFormState>(key: K) {
    return (e: React.ChangeEvent<HTMLInputElement>) =>
      setForm((prev) => ({ ...prev, [key]: e.target.value }))
  }

  function handleTest() {
    if (!form.issuer.trim()) {
      setErrors((prev) => ({ ...prev, issuer: 'Issuer URL is required to test' }))
      return
    }
    const issuerAtTest = form.issuer
    setTestedIssuer(issuerAtTest)
    setTestStatus('loading')
    setTestMessage(undefined)
    testSSO.mutate(form.issuer, {
      onSuccess: (result) => {
        if (result.success) {
          setTestStatus('success')
        } else {
          setTestStatus('error')
          setTestMessage(result.message)
        }
      },
      onError: (err) => {
        setTestStatus('error')
        setTestMessage(err instanceof Error ? err.message : 'Connection test failed')
      },
    })
  }

  function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    const errs = validate(form)
    if (Object.keys(errs).length > 0) {
      setErrors(errs)
      return
    }
    setErrors({})
    saveSSO.mutate(formToParams(form), {
      onSuccess: () => {
        toast({ variant: 'success', message: 'SSO configuration saved' })
      },
      onError: (err) => {
        toast({
          variant: 'error',
          message: err instanceof Error ? err.message : 'Failed to save SSO configuration',
        })
      },
    })
  }

  function handleDelete() {
    deleteSSO.mutate(undefined, {
      onSuccess: () => {
        toast({ variant: 'success', message: 'Org SSO configuration removed' })
        setShowDeleteConfirm(false)
        onDelete?.()
      },
      onError: (err) => {
        toast({
          variant: 'error',
          message: err instanceof Error ? err.message : 'Failed to remove SSO configuration',
        })
        setShowDeleteConfirm(false)
      },
    })
  }

  const isPending = saveSSO.isPending

  return (
    <>
      <form onSubmit={handleSubmit} noValidate className="space-y-6">
        {/* Card 1: Status */}
        <div className="bg-bg-secondary rounded-xl border border-border p-6">
          <h3 className="text-[10px] font-medium tracking-widest uppercase text-text-tertiary mb-4">
            Status
          </h3>
          <div className="flex items-center justify-between">
            <div>
              <p className="text-sm font-medium text-text-secondary">Enable SSO</p>
              <p className="text-xs text-text-tertiary mt-0.5">
                Require users to authenticate via OIDC
              </p>
            </div>
            <Toggle
              checked={form.enabled}
              onChange={patch('enabled')}
              disabled={isPending}
            />
          </div>
        </div>

        {/* Card 2: Provider Configuration */}
        <div className="bg-bg-secondary rounded-xl border border-border p-6 space-y-4">
          <h3 className="text-[10px] font-medium tracking-widest uppercase text-text-tertiary mb-4">
            Provider Configuration
          </h3>

          {/* Issuer URL + Test */}
          <div className="space-y-1.5">
            <label className="block text-sm font-medium text-text-secondary">
              Issuer URL
            </label>
            <div className="flex gap-2 items-start">
              <div className="flex-1">
                <input
                  type="url"
                  value={form.issuer}
                  onChange={patchInput('issuer')}
                  placeholder="https://accounts.google.com"
                  disabled={isPending}
                  aria-invalid={!!errors.issuer}
                  className={[
                    'block w-full rounded-md bg-bg-secondary border px-3 py-2 text-sm text-text-primary placeholder:text-text-tertiary',
                    'transition-colors duration-150 focus:outline-none focus:border-accent focus:ring-2 focus:ring-accent/40',
                    errors.issuer
                      ? 'border-error focus:border-error focus:ring-error/40'
                      : 'border-border',
                    isPending ? 'opacity-50 cursor-not-allowed bg-bg-tertiary' : '',
                  ].join(' ')}
                />
                {errors.issuer && (
                  <p role="alert" className="mt-1.5 text-xs text-error">
                    {errors.issuer}
                  </p>
                )}
              </div>
              <div className="flex items-center gap-2 pt-0.5">
                <Button
                  type="button"
                  variant="secondary"
                  size="sm"
                  onClick={handleTest}
                  loading={activeTestStatus === 'loading'}
                  disabled={isPending || activeTestStatus === 'loading'}
                >
                  Test
                </Button>
                <TestConnectionBadge status={activeTestStatus} message={activeTestMessage} />
              </div>
            </div>
            <p className="text-xs text-text-tertiary">
              The OIDC provider's issuer URL. Used to fetch the discovery document.
            </p>
          </div>

          <Input
            label="Client ID"
            value={form.clientId}
            onChange={patchInput('clientId')}
            placeholder="your-client-id"
            error={errors.clientId}
            disabled={isPending}
            className="font-mono"
          />

          <Input
            label="Client Secret"
            type="password"
            value={form.clientSecret}
            onChange={patchInput('clientSecret')}
            placeholder={initial.clientSecret === '' && hasExistingConfig ? '••••••••' : ''}
            description={
              hasExistingConfig
                ? 'Leave blank to keep the existing secret.'
                : undefined
            }
            disabled={isPending}
            className="font-mono"
          />

          <Input
            label="Redirect URL"
            type="url"
            value={form.redirectUrl}
            onChange={patchInput('redirectUrl')}
            placeholder={DEFAULT_REDIRECT_URL}
            description="The callback URL registered with your identity provider."
            disabled={isPending}
            className="font-mono text-xs"
          />

          <Input
            label="Scopes"
            value={form.scopes}
            onChange={patchInput('scopes')}
            placeholder="openid, email, profile"
            description="Comma-separated list of OAuth scopes to request."
            disabled={isPending}
          />

          <Input
            label="Allowed Domains"
            value={form.allowedDomains}
            onChange={patchInput('allowedDomains')}
            placeholder="example.com, acme.org"
            description="Comma-separated. Leave blank to allow all domains."
            disabled={isPending}
          />
        </div>

        {/* Card 3: Provisioning */}
        <div className="bg-bg-secondary rounded-xl border border-border p-6 space-y-4">
          <h3 className="text-[10px] font-medium tracking-widest uppercase text-text-tertiary mb-4">
            Provisioning
          </h3>

          <div className="flex items-center justify-between">
            <div>
              <p className="text-sm font-medium text-text-secondary">Auto-Provision Users</p>
              <p className="text-xs text-text-tertiary mt-0.5">
                Automatically create accounts for new SSO users
              </p>
            </div>
            <Toggle
              checked={form.autoProvision}
              onChange={patch('autoProvision')}
              disabled={isPending}
            />
          </div>

          <Select
            label="Default Role"
            options={DEFAULT_ROLE_OPTIONS}
            value={form.defaultRole}
            onChange={patch('defaultRole')}
            disabled={isPending}
          />
        </div>

        {/* Card 4: Group Sync */}
        <div className="bg-bg-secondary rounded-xl border border-border p-6 space-y-4">
          <h3 className="text-[10px] font-medium tracking-widest uppercase text-text-tertiary mb-4">
            Group Sync
          </h3>

          <div className="flex items-center justify-between">
            <div>
              <p className="text-sm font-medium text-text-secondary">Group Sync</p>
              <p className="text-xs text-text-tertiary mt-0.5">
                Sync user groups from the identity provider
              </p>
            </div>
            <Toggle
              checked={form.groupSync}
              onChange={patch('groupSync')}
              disabled={isPending}
            />
          </div>

          {form.groupSync && (
            <Input
              label="Group Claim"
              value={form.groupClaim}
              onChange={patchInput('groupClaim')}
              placeholder="groups"
              description="The JWT claim that contains group memberships."
              disabled={isPending}
              className="font-mono"
            />
          )}
        </div>

        {/* Actions */}
        <div className="flex items-center justify-between pt-2">
          {hasExistingConfig ? (
            <Button
              type="button"
              variant="ghost"
              size="sm"
              onClick={() => setShowDeleteConfirm(true)}
              disabled={isPending || deleteSSO.isPending}
              className="text-error hover:text-error"
            >
              Remove org SSO config
            </Button>
          ) : (
            <span />
          )}
          <Button type="submit" loading={isPending}>
            Save Configuration
          </Button>
        </div>
      </form>

      <ConfirmDialog
        open={showDeleteConfirm}
        onClose={() => setShowDeleteConfirm(false)}
        onConfirm={handleDelete}
        title="Remove Org SSO Configuration"
        description="This will remove the org-specific SSO configuration. Users will fall back to the global SSO configuration, if one exists."
        confirmLabel="Remove"
        loading={deleteSSO.isPending}
      />
    </>
  )
}

// ---------------------------------------------------------------------------
// OrgDetailSSOTab
// ---------------------------------------------------------------------------

export default function OrgDetailSSOTab() {
  const { orgId = '' } = useParams<{ orgId: string }>()
  const [editMode, setEditMode] = useState(false)

  const { data: license } = useLicense()
  const orgSSO = useOrgSSO(orgId)
  const globalSSO = useGlobalSSO()

  if (license && !license.features.includes('sso_oidc')) {
    return (
      <UpgradePrompt
        title="SSO Configuration"
        description="SSO / OIDC integration requires an Enterprise license."
      />
    )
  }

  const hasOrgConfig = orgSSO.data !== undefined && !orgSSO.isError
  const showEdit = editMode || hasOrgConfig

  // When config is deleted, exit edit mode
  function handleDelete() {
    setEditMode(false)
  }

  return (
    <div className="max-w-3xl space-y-6">
      <div className="rounded-xl border border-border bg-bg-secondary">
        <div className="px-6 py-4 border-b border-border flex items-center justify-between">
          <div>
            <h2 className="text-sm font-semibold text-text-primary">
              SSO Configuration
            </h2>
            <p className="text-xs text-text-tertiary mt-0.5">
              {hasOrgConfig
                ? 'Org-specific OIDC configuration'
                : 'Inherits from global configuration'}
            </p>
          </div>
          {hasOrgConfig && (
            <Badge variant={orgSSO.data?.enabled ? 'success' : 'muted'}>
              {orgSSO.data?.enabled ? 'Enabled' : 'Disabled'}
            </Badge>
          )}
        </div>

        <div className="p-6">
          {/* Loading skeleton */}
          {orgSSO.isLoading && (
            <div className="space-y-4 animate-pulse">
              <div className="h-4 w-48 rounded bg-bg-tertiary" />
              <div className="h-9 w-full rounded bg-bg-tertiary" />
              <div className="h-9 w-full rounded bg-bg-tertiary" />
              <div className="h-9 w-full rounded bg-bg-tertiary" />
            </div>
          )}

          {/* No org config, not in edit mode */}
          {!orgSSO.isLoading && !showEdit && (
            <div className="space-y-6">
              {/* Info banner */}
              <Banner
                variant="info"
                title="SSO is inherited from the global configuration."
                description="Configure an org-specific override below."
              />

              {/* Global config display */}
              {globalSSO.isLoading && (
                <div className="space-y-3 animate-pulse">
                  <div className="h-3 w-32 rounded bg-bg-tertiary" />
                  <div className="h-4 w-full rounded bg-bg-tertiary" />
                  <div className="h-4 w-3/4 rounded bg-bg-tertiary" />
                </div>
              )}

              {!globalSSO.isLoading && globalSSO.data && (
                <div>
                  <p className="text-[10px] font-medium tracking-widest uppercase text-text-tertiary mb-4">
                    Global Config
                  </p>
                  <GlobalSSODisplay config={globalSSO.data} />
                </div>
              )}

              {!globalSSO.isLoading && !globalSSO.data && (
                <p className="text-sm text-text-tertiary">
                  No global SSO configuration is set.
                </p>
              )}

              <div className="pt-2">
                <Button variant="secondary" onClick={() => setEditMode(true)}>
                  Configure org-specific SSO
                </Button>
              </div>
            </div>
          )}

          {/* Edit / create form */}
          {!orgSSO.isLoading && showEdit && (
            <SSOForm
              orgId={orgId}
              initial={hasOrgConfig && orgSSO.data ? formFromConfig(orgSSO.data) : defaultForm()}
              hasExistingConfig={hasOrgConfig}
              onDelete={handleDelete}
            />
          )}
        </div>
      </div>
    </div>
  )
}
