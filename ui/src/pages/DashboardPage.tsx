import { useState, useMemo } from 'react'
import { PageHeader } from '../components/ui/PageHeader'
import { StatCard } from '../components/ui/StatCard'
import { Banner } from '../components/ui/Banner'
import { AreaChart } from '../components/ui/charts/AreaChart'
import { DonutChart } from '../components/ui/charts/DonutChart'
import { HorizontalBar } from '../components/ui/charts/HorizontalBar'
import { MiniTable } from '../components/ui/charts/MiniTable'
import type { MiniTableColumn } from '../components/ui/charts/MiniTable'
import { useMe } from '../hooks/useMe'
import { useDashboardStats } from '../hooks/useDashboardStats'
import type { BudgetWarning } from '../hooks/useDashboardStats'
import { useTopModels } from '../hooks/useTopModels'
import type { UsageDataPoint } from '../hooks/useTopModels'
import { useUsage } from '../hooks/useUsage'
import { useOrg } from '../hooks/useOrg'
import { formatTokens, formatCost, formatNumber } from '../lib/utils'

// ---------------------------------------------------------------------------
// Time range helpers
// ---------------------------------------------------------------------------

type TimeRange = '24h' | '7d' | '30d'

function getTimeRange(range: TimeRange): { from: string; to: string } {
  const to = new Date()
  const from = new Date()
  if (range === '24h') from.setHours(from.getHours() - 24)
  else if (range === '7d') from.setDate(from.getDate() - 7)
  else from.setDate(from.getDate() - 30)
  return { from: from.toISOString(), to: to.toISOString() }
}

// ---------------------------------------------------------------------------
// BudgetWarningBanners
// ---------------------------------------------------------------------------

function BudgetWarningBanners({ warnings }: { warnings: BudgetWarning[] }) {
  if (warnings.length === 0) return null
  return (
    <div className="space-y-2">
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
// ProgressBar (token budget)
// ---------------------------------------------------------------------------

function ProgressBar({ label, used, limit }: { label: string; used: number; limit: number }) {
  const pct = limit > 0 ? Math.min((used / limit) * 100, 100) : 0
  const color = pct > 90 ? 'bg-error' : pct > 70 ? 'bg-warning' : 'bg-accent'
  return (
    <div>
      <div className="flex justify-between text-sm mb-1">
        <span className="text-text-secondary">{label}</span>
        <span className="text-text-tertiary tabular-nums">
          {formatNumber(used)} / {formatNumber(limit)}
        </span>
      </div>
      <div className="h-2 bg-bg-tertiary rounded-full overflow-hidden">
        <div className={`h-full rounded-full transition-all duration-300 ${color}`} style={{ width: `${pct}%` }} />
      </div>
    </div>
  )
}

// ---------------------------------------------------------------------------
// BudgetSection
// ---------------------------------------------------------------------------

function BudgetSection({ orgId, tokens24h }: { orgId: string; tokens24h: number }) {
  const { data: org } = useOrg(orgId)
  if (!org || org.daily_token_limit <= 0) return null
  return (
    <div className="bg-bg-secondary rounded-xl border border-border p-6">
      <h2 className="text-lg font-semibold text-text-primary mb-6">Token Budget</h2>
      <div className="space-y-4">
        <ProgressBar label="Daily Token Budget" used={tokens24h} limit={org.daily_token_limit} />
      </div>
    </div>
  )
}

// ---------------------------------------------------------------------------
// TimeRangePills
// ---------------------------------------------------------------------------

function TimeRangePills({
  value,
  onChange,
}: {
  value: TimeRange
  onChange: (r: TimeRange) => void
}) {
  const ranges: TimeRange[] = ['24h', '7d', '30d']
  return (
    <div className="flex items-center gap-1">
      {ranges.map((r) => (
        <button
          key={r}
          type="button"
          onClick={() => onChange(r)}
          className={
            value === r
              ? 'px-3 py-1 rounded-md text-xs font-medium bg-accent/20 text-accent border border-accent/30'
              : 'px-3 py-1 rounded-md text-xs font-medium text-text-tertiary hover:text-text-secondary hover:bg-bg-tertiary border border-transparent transition-colors'
          }
        >
          {r}
        </button>
      ))}
    </div>
  )
}

// ---------------------------------------------------------------------------
// Performance badge
// ---------------------------------------------------------------------------

function PerfBadge({ ms }: { ms: number }) {
  if (ms < 500) {
    return (
      <span className="inline-flex items-center px-2 py-0.5 rounded-full text-[10px] font-medium bg-success/10 text-success">
        Fast
      </span>
    )
  }
  if (ms < 2000) {
    return (
      <span className="inline-flex items-center px-2 py-0.5 rounded-full text-[10px] font-medium bg-warning/10 text-warning">
        Moderate
      </span>
    )
  }
  return (
    <span className="inline-flex items-center px-2 py-0.5 rounded-full text-[10px] font-medium bg-error/10 text-error">
      Slow
    </span>
  )
}

// ---------------------------------------------------------------------------
// MiniTable column definitions
// ---------------------------------------------------------------------------

const performanceColumns: MiniTableColumn<UsageDataPoint>[] = [
  {
    key: 'model',
    header: 'Model',
    render: (row) => (
      <span className="font-mono text-text-primary text-xs">{row.group_key}</span>
    ),
  },
  {
    key: 'latency',
    header: 'Avg Latency',
    align: 'right',
    render: (row) => (
      <div className="flex items-center justify-end gap-2">
        <span className="text-text-secondary tabular-nums">{Math.round(row.avg_duration_ms)}ms</span>
        <PerfBadge ms={row.avg_duration_ms} />
      </div>
    ),
  },
  {
    key: 'tpr',
    header: 'Tokens/Req',
    align: 'right',
    render: (row) => {
      const tpr = row.total_requests > 0 ? Math.round(row.total_tokens / row.total_requests) : 0
      return <span className="text-text-secondary tabular-nums">{formatNumber(tpr)}</span>
    },
  },
]

// ---------------------------------------------------------------------------
// Icons (inline SVGs to avoid a dependency)
// ---------------------------------------------------------------------------

function IconActivity() {
  return (
    <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <polyline points="22 12 18 12 15 21 9 3 6 12 2 12" />
    </svg>
  )
}

function IconZap() {
  return (
    <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <polygon points="13 2 3 14 12 14 11 22 21 10 12 10 13 2" />
    </svg>
  )
}

function IconDollarSign() {
  return (
    <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <line x1="12" y1="1" x2="12" y2="23" />
      <path d="M17 5H9.5a3.5 3.5 0 0 0 0 7h5a3.5 3.5 0 0 1 0 7H6" />
    </svg>
  )
}

function IconKey() {
  return (
    <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <circle cx="7.5" cy="15.5" r="5.5" />
      <path d="M21 2l-9.6 9.6" />
      <path d="M15.5 7.5l3 3L22 7l-3-3" />
    </svg>
  )
}

// ---------------------------------------------------------------------------
// DashboardPage
// ---------------------------------------------------------------------------

const scopeDescriptions: Record<string, string> = {
  org: 'Organization-wide usage overview',
  team: 'Your team usage overview',
  user: 'Your personal usage overview',
}

export default function DashboardPage() {
  const { data: me } = useMe()
  const { data: stats, isLoading: statsLoading } = useDashboardStats()
  const [timeRange, setTimeRange] = useState<TimeRange>('7d')

  const canViewOrgUsage = me?.role === 'system_admin' || me?.role === 'org_admin'
  const orgId = me?.org_id ?? ''

  const { data: topModels, isLoading: modelsLoading } = useTopModels(
    orgId,
    canViewOrgUsage,
  )

  // Time-series data for the area chart
  const { from, to } = useMemo(() => getTimeRange(timeRange), [timeRange])
  const { from: from24h } = useMemo(() => getTimeRange('24h'), [])

  const { data: usageSeries, isLoading: seriesLoading } = useUsage(
    orgId,
    from,
    to,
    'day',
  )

  // Team usage (admin only)
  const { data: teamUsage, isLoading: teamUsageLoading } = useUsage(
    orgId,
    from24h,
    to,
    'team',
  )

  const scope = stats?.scope ?? 'user'
  const description = scopeDescriptions[scope] ?? 'Your VoidLLM usage overview'
  const isOrgScope = scope === 'org'

  // Build area chart data from usage series
  const areaData = useMemo(() => {
    if (!usageSeries?.data) return []
    return usageSeries.data.map((d) => ({
      label: d.group_key,
      value: d.total_requests,
    }))
  }, [usageSeries])

  // Build horizontal bar data for top models
  const topModelsBars = useMemo(() => {
    if (!topModels?.data) return []
    return topModels.data.slice(0, 6).map((m) => ({
      label: m.group_key,
      value: m.total_tokens,
      detail: `${formatTokens(m.total_tokens)} Tokens`,
    }))
  }, [topModels])

  // Build donut segments from prompt/completion token split
  const donutSegments = useMemo(() => {
    if (!topModels?.data || topModels.data.length === 0) return []
    const totalPrompt = topModels.data.reduce((acc, m) => acc + m.prompt_tokens, 0)
    const totalCompletion = topModels.data.reduce((acc, m) => acc + m.completion_tokens, 0)
    if (totalPrompt + totalCompletion === 0) return []
    return [
      { label: 'Prompt', value: totalPrompt, color: '#8b5cf6' },
      { label: 'Completion', value: totalCompletion, color: '#25252d' },
    ]
  }, [topModels])

  // Build team usage bars (admin only)
  const teamBars = useMemo(() => {
    if (!teamUsage?.data) return []
    return teamUsage.data.slice(0, 6).map((t) => ({
      label: t.group_key,
      value: t.total_requests,
      detail: `${formatNumber(t.total_requests)} Req`,
    }))
  }, [teamUsage])

  const skeletonValue = '...'

  return (
    <>
      <PageHeader title="Dashboard" description={description} />

      <div className="space-y-6">
        {/* Budget warnings */}
        {(stats?.budget_warnings?.length ?? 0) > 0 && (
          <BudgetWarningBanners warnings={stats?.budget_warnings ?? []} />
        )}

        {/* Stat cards */}
        <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-4 gap-5">
          <StatCard
            label="Requests (24h)"
            value={statsLoading ? skeletonValue : formatNumber(stats?.requests_24h ?? 0)}
            icon={<IconActivity />}
            iconColor="purple"
          />
          <StatCard
            label="Tokens (24h)"
            value={statsLoading ? skeletonValue : formatTokens(stats?.tokens_24h ?? 0)}
            icon={<IconZap />}
            iconColor="blue"
          />
          <StatCard
            label="Est. Cost (24h)"
            value={statsLoading ? skeletonValue : formatCost(stats?.cost_estimate_24h ?? 0)}
            icon={<IconDollarSign />}
            iconColor="green"
          />
          <StatCard
            label="Active Keys"
            value={statsLoading ? skeletonValue : formatNumber(stats?.active_keys ?? 0)}
            icon={<IconKey />}
            iconColor="pink"
          />
        </div>

        {/* Token budget section */}
        {me?.org_id != null && !statsLoading && (
          <BudgetSection orgId={me.org_id} tokens24h={stats?.tokens_24h ?? 0} />
        )}

        {/* Requests over time */}
        {orgId !== '' && (
          <div className="bg-bg-secondary rounded-xl border border-border p-6">
            <div className="flex items-center justify-between mb-6">
              <h2 className="text-lg font-semibold text-text-primary">Requests over Time</h2>
              <TimeRangePills value={timeRange} onChange={setTimeRange} />
            </div>
            {seriesLoading ? (
              <div
                className="rounded-lg bg-bg-tertiary animate-pulse"
                style={{ height: 220 }}
                aria-label="Loading chart"
              />
            ) : areaData.length > 0 ? (
              <AreaChart
                data={areaData}
                height={220}
                color="#8b5cf6"
                formatValue={formatNumber}
              />
            ) : (
              <div
                className="flex items-center justify-center text-text-tertiary text-sm"
                style={{ height: 220 }}
              >
                No data for this period
              </div>
            )}
          </div>
        )}

        {/* Top models + token distribution (admin only) */}
        {canViewOrgUsage && (
          <div className="grid grid-cols-1 lg:grid-cols-2 gap-6">
            {/* Top Models */}
            <div className="bg-bg-secondary rounded-xl border border-border p-6">
              <h2 className="text-lg font-semibold text-text-primary mb-6">Top Models</h2>
              {modelsLoading ? (
                <div className="space-y-5">
                  {[1, 2, 3].map((i) => (
                    <div key={i} className="space-y-1.5">
                      <div className="h-3 w-32 bg-bg-tertiary rounded animate-pulse" />
                      <div className="h-2.5 bg-bg-tertiary rounded-full animate-pulse" />
                    </div>
                  ))}
                </div>
              ) : topModelsBars.length > 0 ? (
                <HorizontalBar items={topModelsBars} />
              ) : (
                <p className="text-sm text-text-tertiary">No model usage in the last 24 hours</p>
              )}
            </div>

            {/* Token Distribution */}
            <div className="bg-bg-secondary rounded-xl border border-border p-6">
              <h2 className="text-lg font-semibold text-text-primary mb-6">Token Distribution</h2>
              {modelsLoading ? (
                <div className="flex justify-center">
                  <div className="w-48 h-48 rounded-full bg-bg-tertiary animate-pulse" />
                </div>
              ) : donutSegments.length > 0 ? (
                <div className="flex justify-center">
                  <DonutChart
                    segments={donutSegments}
                    centerLabel="Total tokens"
                    centerValue={formatTokens(
                      donutSegments.reduce((acc, s) => acc + s.value, 0),
                    )}
                    size={192}
                    strokeWidth={20}
                  />
                </div>
              ) : (
                <p className="text-sm text-text-tertiary">No token data available</p>
              )}
            </div>
          </div>
        )}

        {/* Team usage + model performance (admin only) */}
        {canViewOrgUsage && isOrgScope && (
          <div className="grid grid-cols-1 lg:grid-cols-2 gap-6">
            {/* Usage by Team */}
            <div className="bg-bg-secondary rounded-xl border border-border p-6">
              <h2 className="text-lg font-semibold text-text-primary mb-6">Usage by Team</h2>
              {teamUsageLoading ? (
                <div className="space-y-5">
                  {[1, 2, 3].map((i) => (
                    <div key={i} className="space-y-1.5">
                      <div className="h-3 w-24 bg-bg-tertiary rounded animate-pulse" />
                      <div className="h-2.5 bg-bg-tertiary rounded-full animate-pulse" />
                    </div>
                  ))}
                </div>
              ) : teamBars.length > 0 ? (
                <HorizontalBar items={teamBars} color="#6366f1" />
              ) : (
                <p className="text-sm text-text-tertiary">No team usage in the last 24 hours</p>
              )}
            </div>

            {/* Model Performance */}
            <div className="bg-bg-secondary rounded-xl border border-border p-6">
              <h2 className="text-lg font-semibold text-text-primary mb-6">Model Performance</h2>
              {modelsLoading ? (
                <div className="space-y-3">
                  {[1, 2, 3].map((i) => (
                    <div key={i} className="h-8 bg-bg-tertiary rounded animate-pulse" />
                  ))}
                </div>
              ) : (topModels?.data?.length ?? 0) > 0 ? (
                <MiniTable<UsageDataPoint>
                  columns={performanceColumns}
                  data={topModels?.data ?? []}
                />
              ) : (
                <p className="text-sm text-text-tertiary">No model data available</p>
              )}
            </div>
          </div>
        )}
      </div>
    </>
  )
}
