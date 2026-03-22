import { useMemo, useState } from 'react'
import { PageHeader } from '../components/ui/PageHeader'
import { StatCard } from '../components/ui/StatCard'
import { Table } from '../components/ui/Table'
import type { Column } from '../components/ui/Table'
import { Button } from '../components/ui/Button'
import { Select } from '../components/ui/Select'
import { useMe } from '../hooks/useMe'
import { useUsage, useCrossOrgUsage } from '../hooks/useUsage'
import type { UsageDataPoint } from '../hooks/useUsage'
import { formatNumber, formatTokens, formatCost } from '../lib/utils'

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

import { exportData } from '../lib/export'

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
// CrossOrgData — fetches /usage (global) — only rendered for system_admin
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

  // When switching away from cross-org, reset group_by if it was set to 'org'
  const handleCrossOrgToggle = (next: boolean) => {
    if (next) {
      setGroupBy('org') // default to org view for cross-org
    } else if (groupBy === 'org') {
      setGroupBy('model') // org not available in single-org mode
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

  return (
    <>
      <PageHeader
        title="Usage"
        description="Track token usage and costs"
      />

      {isSystemAdmin && (
        <div className="flex items-center gap-1 mb-5 p-1 rounded-lg bg-bg-secondary border border-border w-fit">
          <button
            type="button"
            onClick={() => handleCrossOrgToggle(false)}
            className={
              !crossOrg
                ? 'px-4 py-1.5 rounded-md text-sm font-medium bg-accent text-white'
                : 'px-4 py-1.5 rounded-md text-sm font-medium text-text-secondary hover:text-text-primary transition-colors'
            }
          >
            My Org
          </button>
          <button
            type="button"
            onClick={() => handleCrossOrgToggle(true)}
            className={
              crossOrg
                ? 'px-4 py-1.5 rounded-md text-sm font-medium bg-accent text-white'
                : 'px-4 py-1.5 rounded-md text-sm font-medium text-text-secondary hover:text-text-primary transition-colors'
            }
          >
            All Orgs
          </button>
        </div>
      )}

      <div className="flex flex-col sm:flex-row sm:items-end gap-4 mb-6">
        <div className="flex items-center gap-2">
          {TIME_RANGES.map((r) => (
            <Button
              key={r}
              variant={range === r ? 'primary' : 'ghost'}
              size="sm"
              onClick={() => setRange(r)}
            >
              {r}
            </Button>
          ))}
        </div>

        <div className="sm:ml-auto flex items-end gap-3">
          <Button
            variant="secondary"
            size="sm"
            onClick={() => exportData(sortedData as unknown as Record<string, unknown>[], USAGE_EXPORT_HEADERS, `voidllm-usage-${groupBy}`, 'csv')}
            disabled={sortedData.length === 0}
          >
            CSV
          </Button>
          <Button
            variant="secondary"
            size="sm"
            onClick={() => exportData(sortedData as unknown as Record<string, unknown>[], USAGE_EXPORT_HEADERS, `voidllm-usage-${groupBy}`, 'json')}
            disabled={sortedData.length === 0}
          >
            JSON
          </Button>
          <div className="w-44">
            <Select
              label="Group by"
              value={groupBy}
              onChange={setGroupBy}
              options={groupByOptions}
              fullWidth
            />
          </div>
        </div>
      </div>

      <div className="grid grid-cols-1 sm:grid-cols-3 gap-4 mb-6">
        <StatCard
          label="Total Requests"
          value={isDataLoading ? '...' : formatTokens(totals.requests)}
        />
        <StatCard
          label="Total Tokens"
          value={isDataLoading ? '...' : formatTokens(totals.tokens)}
        />
        <StatCard
          label="Est. Cost"
          value={isDataLoading ? '...' : formatCost(totals.cost)}
        />
      </div>

      <Table<UsageDataPoint>
        columns={columns}
        data={sortedData}
        keyExtractor={(row) => row.group_key}
        loading={isDataLoading}
        emptyMessage="No usage data for the selected time range"
      />
    </>
  )
}
