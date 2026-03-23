import { useMemo, useState } from 'react'
import { PageHeader } from '../components/ui/PageHeader'
import { StatCard } from '../components/ui/StatCard'
import { Table } from '../components/ui/Table'
import type { Column } from '../components/ui/Table'
import { Button } from '../components/ui/Button'
import { Select } from '../components/ui/Select'
import { AreaChart, DonutChart, HorizontalBar } from '../components/ui/charts'
import { useMe } from '../hooks/useMe'
import { useUsage, useCrossOrgUsage } from '../hooks/useUsage'
import type { UsageDataPoint } from '../hooks/useUsage'
import { formatNumber, formatTokens, formatCost } from '../lib/utils'
import { exportData } from '../lib/export'

const TIME_RANGES = ['24h', '7d', '30d', '90d'] as const
type TimeRange = (typeof TIME_RANGES)[number]

const RANGE_HOURS: Record<TimeRange, number> = {
  '24h': 24,
  '7d': 168,
  '30d': 720,
  '90d': 2160,
}

function getTimeRange(range: TimeRange): { from: string; to: string } {
  const now = new Date()
  const from = new Date(now.getTime() - RANGE_HOURS[range] * 3_600_000)
  return { from: from.toISOString(), to: now.toISOString() }
}

const BASE_GROUP_BY_OPTIONS = [
  { value: 'model', label: 'Model' },
  { value: 'team', label: 'Team' },
  { value: 'user', label: 'User' },
  { value: 'key', label: 'Key' },
  { value: 'day', label: 'Day' },
  { value: 'hour', label: 'Hour' },
]

const CROSS_ORG_GROUP_BY_OPTIONS = [
  ...BASE_GROUP_BY_OPTIONS,
  { value: 'org', label: 'Org' },
]

const GROUP_BY_HEADERS: Record<string, string> = {
  model: 'Model',
  team: 'Team',
  user: 'User',
  key: 'Key',
  day: 'Date',
  hour: 'Hour',
  org: 'Org',
}

function buildColumns(groupBy: string): Column<UsageDataPoint>[] {
  return [
    {
      key: 'group_key',
      header: GROUP_BY_HEADERS[groupBy] ?? 'Group',
      render: (row) => (
        <span className="font-mono text-text-primary">{row.group_key}</span>
      ),
    },
    {
      key: 'total_requests',
      header: 'Requests',
      align: 'right',
      render: (row) => (
        <span className="text-text-secondary">{formatNumber(row.total_requests)}</span>
      ),
    },
    {
      key: 'prompt_tokens',
      header: 'Prompt Tokens',
      align: 'right',
      render: (row) => (
        <span className="text-text-secondary">{formatTokens(row.prompt_tokens)}</span>
      ),
    },
    {
      key: 'completion_tokens',
      header: 'Completion Tokens',
      align: 'right',
      render: (row) => (
        <span className="text-text-secondary">{formatTokens(row.completion_tokens)}</span>
      ),
    },
    {
      key: 'total_tokens',
      header: 'Total Tokens',
      align: 'right',
      render: (row) => (
        <span className="text-text-primary font-medium">{formatTokens(row.total_tokens)}</span>
      ),
    },
    {
      key: 'cost_estimate',
      header: 'Cost',
      align: 'right',
      render: (row) => (
        <span className="text-text-secondary">{formatCost(row.cost_estimate)}</span>
      ),
    },
    {
      key: 'avg_duration_ms',
      header: 'Avg Duration',
      align: 'right',
      render: (row) => (
        <span className="text-text-tertiary">{formatNumber(Math.round(row.avg_duration_ms))} ms</span>
      ),
    },
  ]
}

const USAGE_EXPORT_HEADERS = [
  { key: 'group_key', label: 'Group' },
  { key: 'total_requests', label: 'Requests' },
  { key: 'prompt_tokens', label: 'Prompt Tokens' },
  { key: 'completion_tokens', label: 'Completion Tokens' },
  { key: 'total_tokens', label: 'Total Tokens' },
  { key: 'cost_estimate', label: 'Cost' },
  { key: 'avg_duration_ms', label: 'Avg Duration (ms)' },
]

// ---------------------------------------------------------------------------
// Icons
// ---------------------------------------------------------------------------

function ActivityIcon() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <polyline points="22 12 18 12 15 21 9 3 6 12 2 12" />
    </svg>
  )
}

function SparklesIcon() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <path d="M12 3l1.88 5.76a1 1 0 00.95.69H21l-5.12 3.72a1 1 0 00-.36 1.12L17.4 20 12 16.28 6.6 20l1.88-5.71a1 1 0 00-.36-1.12L3 9.45h6.17a1 1 0 00.95-.69L12 3z" />
    </svg>
  )
}

function DollarIcon() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <line x1="12" y1="1" x2="12" y2="23" />
      <path d="M17 5H9.5a3.5 3.5 0 000 7h5a3.5 3.5 0 010 7H6" />
    </svg>
  )
}

function DownloadIcon() {
  return (
    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <path d="M21 15v4a2 2 0 01-2 2H5a2 2 0 01-2-2v-4" />
      <polyline points="7 10 12 15 17 10" />
      <line x1="12" y1="15" x2="12" y2="3" />
    </svg>
  )
}

// ---------------------------------------------------------------------------
// UsagePage
// ---------------------------------------------------------------------------

export default function UsagePage() {
  const [range, setRange] = useState<TimeRange>('24h')
  const [groupBy, setGroupBy] = useState('model')
  const [crossOrg, setCrossOrg] = useState(false)

  const { data: me } = useMe()
  const orgId = me?.org_id ?? ''
  const isSystemAdmin = me?.is_system_admin === true

  const { from, to } = useMemo(() => getTimeRange(range), [range])

  const orgUsage = useUsage(orgId, from, to, groupBy)
  const crossOrgUsage = useCrossOrgData({ from, to, groupBy, enabled: isSystemAdmin })

  const activeResult = crossOrg && isSystemAdmin ? crossOrgUsage : orgUsage

  const { data: usage, isLoading } = activeResult

  // Daily trend data — only when groupBy is not already 'day'/'hour', and not cross-org
  const needsDailyTrend = !crossOrg && groupBy !== 'day' && groupBy !== 'hour'
  const dailyUsage = useUsage(orgId, from, to, 'day')
  // Use main data directly when groupBy is already day or hour
  const trendData = needsDailyTrend ? dailyUsage.data?.data : usage?.data

  // When switching away from cross-org, reset group_by if it was set to 'org'
  const handleCrossOrgToggle = (next: boolean) => {
    if (next) {
      setGroupBy('org')
    } else if (groupBy === 'org') {
      setGroupBy('model')
    }
    setCrossOrg(next)
  }

  const groupByOptions = crossOrg && isSystemAdmin
    ? CROSS_ORG_GROUP_BY_OPTIONS
    : BASE_GROUP_BY_OPTIONS

  const totals = useMemo(() => {
    if (!usage?.data) return { requests: 0, tokens: 0, cost: 0 }
    return usage.data.reduce(
      (acc, d) => ({
        requests: acc.requests + d.total_requests,
        tokens: acc.tokens + d.total_tokens,
        cost: acc.cost + d.cost_estimate,
      }),
      { requests: 0, tokens: 0, cost: 0 },
    )
  }, [usage])

  const sortedData = useMemo(() => {
    if (!usage?.data) return []
    return [...usage.data].sort((a, b) => b.total_tokens - a.total_tokens)
  }, [usage])

  const columns = useMemo(() => buildColumns(groupBy), [groupBy])

  const isDataLoading = isLoading && (crossOrg ? isSystemAdmin : !!orgId)

  const totalPrompt = usage?.data?.reduce((s, d) => s + d.prompt_tokens, 0) ?? 0
  const totalCompletion = usage?.data?.reduce((s, d) => s + d.completion_tokens, 0) ?? 0

  const top5 = sortedData.slice(0, 5)

  return (
    <>
      <PageHeader
        title="Usage"
        description="Track token usage and costs"
      />

      {/* Top controls: scope toggle + time range pills */}
      <div className="flex items-center gap-4 mb-6 flex-wrap">
        {isSystemAdmin && (
          <div className="inline-flex gap-1 p-1 rounded-lg bg-bg-tertiary">
            <button
              type="button"
              onClick={() => handleCrossOrgToggle(false)}
              className={
                !crossOrg
                  ? 'px-4 py-1.5 rounded-md text-sm font-medium bg-bg-secondary text-text-primary shadow-sm transition-colors'
                  : 'px-4 py-1.5 rounded-md text-sm font-medium text-text-tertiary hover:text-text-secondary transition-colors'
              }
            >
              My Organization
            </button>
            <button
              type="button"
              onClick={() => handleCrossOrgToggle(true)}
              className={
                crossOrg
                  ? 'px-4 py-1.5 rounded-md text-sm font-medium bg-bg-secondary text-text-primary shadow-sm transition-colors'
                  : 'px-4 py-1.5 rounded-md text-sm font-medium text-text-tertiary hover:text-text-secondary transition-colors'
              }
            >
              All Organizations
            </button>
          </div>
        )}

        <div className="inline-flex gap-1 p-1 rounded-lg bg-bg-tertiary">
          {TIME_RANGES.map((r) => (
            <button
              key={r}
              type="button"
              onClick={() => setRange(r)}
              className={
                range === r
                  ? 'px-3 py-1.5 rounded-md text-sm font-medium bg-bg-secondary text-text-primary shadow-sm transition-colors'
                  : 'px-3 py-1.5 rounded-md text-sm font-medium text-text-tertiary hover:text-text-secondary transition-colors'
              }
            >
              {r}
            </button>
          ))}
        </div>
      </div>

      {/* Stat cards */}
      <div className="grid grid-cols-1 sm:grid-cols-3 gap-4 mb-6">
        <StatCard
          label="Total Requests"
          value={isDataLoading ? '...' : formatTokens(totals.requests)}
          icon={<ActivityIcon />}
          iconColor="purple"
        />
        <StatCard
          label="Total Tokens"
          value={isDataLoading ? '...' : formatTokens(totals.tokens)}
          icon={<SparklesIcon />}
          iconColor="blue"
        />
        <StatCard
          label="Est. Cost"
          value={isDataLoading ? '...' : formatCost(totals.cost)}
          icon={<DollarIcon />}
          iconColor="green"
        />
      </div>

      {/* Usage over Time chart — not shown in cross-org mode */}
      {!crossOrg && (
        <div className="bg-bg-secondary rounded-xl border border-border p-6 mb-6">
          <h3 className="text-sm font-semibold text-text-primary mb-4">Usage over Time</h3>
          <AreaChart
            data={(trendData ?? []).map((d) => ({
              label: d.group_key.length > 10 ? d.group_key.slice(5) : d.group_key,
              value: d.total_requests,
            }))}
            height={220}
            formatValue={formatNumber}
          />
        </div>
      )}

      {/* Controls bar */}
      <div className="flex items-center gap-3 mb-6">
        <div className="ml-auto flex items-center gap-3">
          {/* Inline Group by label + select */}
          <div className="flex items-center gap-2">
            <span className="text-xs text-text-tertiary whitespace-nowrap">Group by</span>
            <div className="w-36">
              <Select
                value={groupBy}
                onChange={setGroupBy}
                options={groupByOptions}
                fullWidth
              />
            </div>
          </div>

          {/* Export buttons */}
          <Button
            variant="secondary"
            size="sm"
            onClick={() =>
              exportData(
                sortedData as unknown as Record<string, unknown>[],
                USAGE_EXPORT_HEADERS,
                `voidllm-usage-${groupBy}`,
                'csv',
              )
            }
            disabled={sortedData.length === 0}
          >
            <span className="flex items-center gap-1.5">
              <DownloadIcon />
              CSV
            </span>
          </Button>
          <Button
            variant="secondary"
            size="sm"
            onClick={() =>
              exportData(
                sortedData as unknown as Record<string, unknown>[],
                USAGE_EXPORT_HEADERS,
                `voidllm-usage-${groupBy}`,
                'json',
              )
            }
            disabled={sortedData.length === 0}
          >
            <span className="flex items-center gap-1.5">
              <DownloadIcon />
              JSON
            </span>
          </Button>
        </div>
      </div>

      {/* Main table */}
      <Table<UsageDataPoint>
        columns={columns}
        data={sortedData}
        keyExtractor={(row) => row.group_key}
        loading={isDataLoading}
        emptyMessage="No usage data for the selected time range"
      />

      {/* Bottom row — Top by Tokens + Token Distribution */}
      <div className="grid grid-cols-1 lg:grid-cols-2 gap-6 mt-6">
        <div className="bg-bg-secondary rounded-xl border border-border p-6">
          <h3 className="text-sm font-semibold text-text-primary mb-4">Top by Tokens</h3>
          <HorizontalBar
            items={top5.map((d) => ({
              label: d.group_key,
              value: d.total_tokens,
              detail: formatTokens(d.total_tokens),
            }))}
          />
        </div>

        <div className="bg-bg-secondary rounded-xl border border-border p-6">
          <h3 className="text-sm font-semibold text-text-primary mb-4">Token Distribution</h3>
          <DonutChart
            segments={[
              { label: 'Prompt', value: totalPrompt, color: '#8b5cf6' },
              { label: 'Completion', value: totalCompletion, color: '#25252d' },
            ]}
            centerLabel="Total"
            centerValue={formatTokens(totalPrompt + totalCompletion)}
          />
        </div>
      </div>
    </>
  )
}

// ---------------------------------------------------------------------------
// CrossOrgData helper — kept local to avoid leaking the type
// ---------------------------------------------------------------------------

interface CrossOrgDataProps {
  from: string
  to: string
  groupBy: string
  enabled: boolean
}

function useCrossOrgData({ from, to, groupBy, enabled }: CrossOrgDataProps) {
  return useCrossOrgUsage({ from, to, groupBy }, enabled)
}
