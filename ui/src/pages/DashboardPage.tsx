import { PageHeader } from '../components/ui/PageHeader'
import { StatCard } from '../components/ui/StatCard'
import { Table } from '../components/ui/Table'
import type { Column } from '../components/ui/Table'
import { Banner } from '../components/ui/Banner'
import { useMe } from '../hooks/useMe'
import { useDashboardStats } from '../hooks/useDashboardStats'
import type { BudgetWarning } from '../hooks/useDashboardStats'
import { useTopModels } from '../hooks/useTopModels'
import type { UsageDataPoint } from '../hooks/useTopModels'
import { useOrg } from '../hooks/useOrg'
import { formatTokens, formatCost, formatNumber } from '../lib/utils'

const scopeDescriptions: Record<string, string> = {
  org: 'Organization-wide usage overview',
  team: 'Your team usage overview',
  user: 'Your personal usage overview',
}

const columns: Column<UsageDataPoint>[] = [
  {
    key: 'model',
    header: 'Model',
    render: (row) => (
      <span className="font-mono text-text-primary">{row.group_key}</span>
    ),
  },
  {
    key: 'requests',
    header: 'Requests',
    align: 'right',
    render: (row) => (
      <span className="text-text-secondary">{formatTokens(row.total_requests)}</span>
    ),
  },
  {
    key: 'tokens',
    header: 'Tokens',
    align: 'right',
    render: (row) => (
      <span className="text-text-secondary">{formatTokens(row.total_tokens)}</span>
    ),
  },
  {
    key: 'cost',
    header: 'Est. Cost',
    align: 'right',
    render: (row) => (
      <span className="text-text-secondary">{formatCost(row.cost_estimate)}</span>
    ),
  },
]

// ---------------------------------------------------------------------------
// ProgressBar
// ---------------------------------------------------------------------------

interface ProgressBarProps {
  label: string
  used: number
  limit: number
}

function ProgressBar({ label, used, limit }: ProgressBarProps) {
  const pct = limit > 0 ? Math.min((used / limit) * 100, 100) : 0
  const color = pct > 90 ? 'bg-error' : pct > 70 ? 'bg-warning' : 'bg-accent'
  return (
    <div>
      <div className="flex justify-between text-sm mb-1">
        <span className="text-text-secondary">{label}</span>
        <span className="text-text-tertiary">
          {formatNumber(used)} / {formatNumber(limit)}
        </span>
      </div>
      <div className="h-2 bg-bg-tertiary rounded-full overflow-hidden">
        <div
          className={`h-full rounded-full transition-all duration-300 ${color}`}
          style={{ width: `${pct}%` }}
        />
      </div>
    </div>
  )
}

// ---------------------------------------------------------------------------
// BudgetWarningBanners
// ---------------------------------------------------------------------------

interface BudgetWarningBannersProps {
  warnings: BudgetWarning[]
}

function BudgetWarningBanners({ warnings }: BudgetWarningBannersProps) {
  if (warnings.length === 0) return null

  return (
    <div className="mb-4 space-y-2">
      {warnings.map((w) => (
        <Banner
          key={`${w.scope}-${w.window}`}
          variant={w.percent_used > 0.9 ? 'error' : 'warning'}
          title={`${w.window === 'daily' ? 'Daily' : 'Monthly'} token budget: ${formatNumber(w.usage)} / ${formatNumber(w.limit)} (${Math.round(w.percent_used * 100)}% used)`}
        />
      ))}
    </div>
  )
}

// ---------------------------------------------------------------------------
// BudgetSection
// ---------------------------------------------------------------------------

interface BudgetSectionProps {
  orgId: string
  tokens24h: number
}

function BudgetSection({ orgId, tokens24h }: BudgetSectionProps) {
  const { data: org } = useOrg(orgId)

  if (!org) return null

  const hasDailyLimit = org.daily_token_limit > 0

  if (!hasDailyLimit) return null

  return (
    <div className="rounded-lg border border-border bg-bg-secondary p-5 mb-8">
      <h2 className="text-sm font-semibold text-text-primary mb-4">Token Budget</h2>
      <div className="space-y-4">
        <ProgressBar
          label="Daily Token Budget"
          used={tokens24h}
          limit={org.daily_token_limit}
        />
      </div>
    </div>
  )
}

// ---------------------------------------------------------------------------
// DashboardPage
// ---------------------------------------------------------------------------

export default function DashboardPage() {
  const { data: me } = useMe()
  const { data: stats, isLoading: statsLoading } = useDashboardStats()

  const canViewOrgUsage =
    me?.role === 'system_admin' || me?.role === 'org_admin'

  const { data: topModels, isLoading: modelsLoading } = useTopModels(
    me?.org_id ?? '',
    canViewOrgUsage,
  )

  const scope = stats?.scope ?? 'user'
  const description = scopeDescriptions[scope] ?? 'Your VoidLLM usage overview'
  const isOrgScope = scope === 'org'

  return (
    <>
      <PageHeader title="Dashboard" description={description} />

      <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4 mb-4">
        <StatCard
          label="Requests (24h)"
          value={statsLoading ? '...' : formatTokens(stats?.requests_24h ?? 0)}
        />
        <StatCard
          label="Tokens (24h)"
          value={statsLoading ? '...' : formatTokens(stats?.tokens_24h ?? 0)}
        />
        <StatCard
          label="Est. Cost (24h)"
          value={statsLoading ? '...' : formatCost(stats?.cost_estimate_24h ?? 0)}
        />
        <StatCard
          label="Active Keys"
          value={statsLoading ? '...' : (stats?.active_keys ?? 0).toLocaleString()}
        />
      </div>

      {isOrgScope && (
        <div className="grid grid-cols-1 sm:grid-cols-2 gap-4 mb-4">
          <StatCard
            label="Teams"
            value={statsLoading ? '...' : (stats?.total_teams ?? 0).toLocaleString()}
          />
          <StatCard
            label="Members"
            value={statsLoading ? '...' : (stats?.total_members ?? 0).toLocaleString()}
          />
        </div>
      )}

      <BudgetWarningBanners warnings={stats?.budget_warnings ?? []} />

      {me?.org_id && !statsLoading && (
        <div className="mt-4">
          <BudgetSection
            orgId={me.org_id}
            tokens24h={stats?.tokens_24h ?? 0}
          />
        </div>
      )}

      {!isOrgScope && !me?.org_id && <div className="mb-8" />}

      {canViewOrgUsage && (
        <div>
          <h2 className="text-lg font-semibold text-text-primary mb-4">Top Models (24h)</h2>
          <Table<UsageDataPoint>
            columns={columns}
            data={topModels?.data ?? []}
            keyExtractor={(row) => row.group_key}
            compact
            loading={modelsLoading && !!me?.org_id}
            emptyMessage="No usage data in the last 24 hours"
          />
        </div>
      )}
    </>
  )
}
