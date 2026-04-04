import { useMemo, useState } from 'react'
import { StatCard } from '../../components/ui/StatCard'
import { Table } from '../../components/ui/Table'
import type { Column } from '../../components/ui/Table'
import { Button } from '../../components/ui/Button'
import { Select } from '../../components/ui/Select'
import { AreaChart, DonutChart, HorizontalBar } from '../../components/ui/charts'
import { useMe } from '../../hooks/useMe'
import { useMCPUsage, useCrossOrgMCPUsage } from '../../hooks/useMCPUsage'
import type { MCPUsageDataPoint } from '../../hooks/useMCPUsage'
import { formatNumber } from '../../lib/utils'
import { exportData } from '../../lib/export'

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
  { value: 'server', label: 'Server' },
  { value: 'tool', label: 'Tool' },
  { value: 'team', label: 'Team' },
  { value: 'key', label: 'Key' },
  { value: 'user', label: 'User' },
  { value: 'day', label: 'Day' },
  { value: 'hour', label: 'Hour' },
  { value: 'status', label: 'Status' },
]

const CROSS_ORG_GROUP_BY_OPTIONS = [
  ...BASE_GROUP_BY_OPTIONS,
  { value: 'org', label: 'Org' },
]

const GROUP_BY_HEADERS: Record<string, string> = {
  server: 'Server',
  tool: 'Tool',
  team: 'Team',
  key: 'Key',
  user: 'User',
  day: 'Date',
  hour: 'Hour',
  status: 'Status',
  org: 'Org',
}

function buildColumns(groupBy: string): Column<MCPUsageDataPoint>[] {
  return [
    {
      key: 'group_key',
      header: GROUP_BY_HEADERS[groupBy] ?? 'Group',
      render: (row) => (
        <span className="font-mono text-text-primary">{row.group_key}</span>
      ),
    },
    {
      key: 'total_calls',
      header: 'Total Calls',
      align: 'right',
      render: (row) => (
        <span className="text-text-primary font-medium">{formatNumber(row.total_calls)}</span>
      ),
    },
    {
      key: 'success_count',
      header: 'Success',
      align: 'right',
      render: (row) => (
        <span className="text-success">{formatNumber(row.success_count)}</span>
      ),
    },
    {
      key: 'error_count',
      header: 'Errors',
      align: 'right',
      render: (row) => (
        <span className={row.error_count > 0 ? 'text-error' : 'text-text-tertiary'}>
          {formatNumber(row.error_count)}
        </span>
      ),
    },
    {
      key: 'timeout_count',
      header: 'Timeouts',
      align: 'right',
      render: (row) => (
        <span className={row.timeout_count > 0 ? 'text-warning' : 'text-text-tertiary'}>
          {formatNumber(row.timeout_count)}
        </span>
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
    {
      key: 'code_mode_calls',
      header: 'Code Mode',
      align: 'right',
      render: (row) => (
        <span className="text-text-secondary">{formatNumber(row.code_mode_calls)}</span>
      ),
    },
  ]
}

const MCP_EXPORT_HEADERS = [
  { key: 'group_key', label: 'Group' },
  { key: 'total_calls', label: 'Total Calls' },
  { key: 'success_count', label: 'Success' },
  { key: 'error_count', label: 'Errors' },
  { key: 'timeout_count', label: 'Timeouts' },
  { key: 'avg_duration_ms', label: 'Avg Duration (ms)' },
  { key: 'code_mode_calls', label: 'Code Mode Calls' },
]

// ---------------------------------------------------------------------------
// Icons
// ---------------------------------------------------------------------------

function ToolIcon() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <path d="M14.7 6.3a1 1 0 000 1.4l1.6 1.6a1 1 0 001.4 0l3.77-3.77a6 6 0 01-7.94 7.94l-6.91 6.91a2.12 2.12 0 01-3-3l6.91-6.91a6 6 0 017.94-7.94l-3.76 3.76z" />
    </svg>
  )
}

function CheckCircleIcon() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <path d="M22 11.08V12a10 10 0 11-5.93-9.14" />
      <polyline points="22 4 12 14.01 9 11.01" />
    </svg>
  )
}

function ClockIcon() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <circle cx="12" cy="12" r="10" />
      <polyline points="12 6 12 12 16 14" />
    </svg>
  )
}

function CodeIcon() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <polyline points="16 18 22 12 16 6" />
      <polyline points="8 6 2 12 8 18" />
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
// MCPUsagePage
// ---------------------------------------------------------------------------

export default function MCPUsagePage() {
  const [range, setRange] = useState<TimeRange>('24h')
  const [groupBy, setGroupBy] = useState('server')
  const [crossOrg, setCrossOrg] = useState(false)

  const { data: me } = useMe()
  const orgId = me?.org_id ?? ''
  const isSystemAdmin = me?.is_system_admin === true

  const { from, to } = useMemo(() => getTimeRange(range), [range])

  const orgUsage = useMCPUsage(orgId, from, to, groupBy)
  const crossOrgUsage = useCrossOrgMCPUsage({ from, to, groupBy }, crossOrg && isSystemAdmin)

  const activeResult = crossOrg && isSystemAdmin ? crossOrgUsage : orgUsage

  const { data: usage, isLoading } = activeResult

  // Daily trend data - only when groupBy is not already 'day'/'hour'
  const needsDailyTrend = groupBy !== 'day' && groupBy !== 'hour'
  const dailyUsage = useMCPUsage(orgId, from, to, 'day')
  const trendData = needsDailyTrend ? dailyUsage.data?.data : usage?.data

  const handleCrossOrgToggle = (next: boolean) => {
    if (next) {
      setGroupBy('org')
    } else if (groupBy === 'org') {
      setGroupBy('server')
    }
    setCrossOrg(next)
  }

  const groupByOptions = crossOrg && isSystemAdmin
    ? CROSS_ORG_GROUP_BY_OPTIONS
    : BASE_GROUP_BY_OPTIONS

  const totals = useMemo(() => {
    if (!usage?.data) return { calls: 0, success: 0, errors: 0, timeouts: 0, codeModes: 0, totalDurationMs: 0 }
    return usage.data.reduce(
      (acc, d) => ({
        calls: acc.calls + d.total_calls,
        success: acc.success + d.success_count,
        errors: acc.errors + d.error_count,
        timeouts: acc.timeouts + d.timeout_count,
        codeModes: acc.codeModes + d.code_mode_calls,
        totalDurationMs: acc.totalDurationMs + d.avg_duration_ms * d.total_calls,
      }),
      { calls: 0, success: 0, errors: 0, timeouts: 0, codeModes: 0, totalDurationMs: 0 },
    )
  }, [usage])

  const successRate = totals.calls > 0 ? (totals.success / totals.calls) * 100 : 0
  const avgDurationMs = totals.calls > 0 ? totals.totalDurationMs / totals.calls : 0

  const sortedData = useMemo(() => {
    if (!usage?.data) return []
    return [...usage.data].sort((a, b) => b.total_calls - a.total_calls)
  }, [usage])

  const columns = useMemo(() => buildColumns(groupBy), [groupBy])

  const isDataLoading = isLoading && (crossOrg ? isSystemAdmin : !!orgId)

  // Get top 10 by tool groupBy - use a separate query when current groupBy isn't 'tool'
  const toolGroupUsage = useMCPUsage(orgId, from, to, 'tool')
  const topTools = useMemo(() => {
    const source = groupBy === 'tool' ? sortedData : (toolGroupUsage.data?.data ?? [])
    return [...source].sort((a, b) => b.total_calls - a.total_calls).slice(0, 10)
  }, [groupBy, sortedData, toolGroupUsage.data])

  // For server groupBy - use a separate query when current groupBy isn't 'server'
  const serverGroupUsage = useMCPUsage(orgId, from, to, 'server')
  const topServers = useMemo(() => {
    const source = groupBy === 'server' ? sortedData : (serverGroupUsage.data?.data ?? [])
    return [...source].sort((a, b) => b.total_calls - a.total_calls).slice(0, 10)
  }, [groupBy, sortedData, serverGroupUsage.data])

  return (
    <>
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
      <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4 mb-6">
        <StatCard
          label="Tool Calls"
          value={isDataLoading ? '...' : formatNumber(totals.calls)}
          icon={<ToolIcon />}
          iconColor="purple"
        />
        <StatCard
          label="Success Rate"
          value={isDataLoading ? '...' : `${successRate.toFixed(1)}%`}
          icon={<CheckCircleIcon />}
          iconColor="green"
        />
        <StatCard
          label="Avg Duration"
          value={isDataLoading ? '...' : `${formatNumber(Math.round(avgDurationMs))} ms`}
          icon={<ClockIcon />}
          iconColor="blue"
        />
        <StatCard
          label="Code Mode Calls"
          value={isDataLoading ? '...' : formatNumber(totals.codeModes)}
          icon={<CodeIcon />}
          iconColor="yellow"
        />
      </div>

      {/* Calls over Time chart - hidden in cross-org mode */}
      {!crossOrg && (
        <div className="bg-bg-secondary rounded-xl border border-border p-6 mb-6">
          <h3 className="text-sm font-semibold text-text-primary mb-4">Calls over Time</h3>
          <AreaChart
            data={(trendData ?? []).map((d) => ({
              label: d.group_key.length > 10 ? d.group_key.slice(5) : d.group_key,
              value: d.total_calls,
            }))}
            height={220}
            formatValue={formatNumber}
          />
        </div>
      )}

      {/* Controls bar */}
      <div className="flex items-center gap-3 mb-6">
        <div className="ml-auto flex items-center gap-3">
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

          <Button
            variant="secondary"
            size="sm"
            onClick={() =>
              exportData(
                sortedData as unknown as Record<string, unknown>[],
                MCP_EXPORT_HEADERS,
                `voidllm-mcp-usage-${groupBy}`,
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
                MCP_EXPORT_HEADERS,
                `voidllm-mcp-usage-${groupBy}`,
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
      <Table<MCPUsageDataPoint>
        columns={columns}
        data={sortedData}
        keyExtractor={(row) => row.group_key}
        loading={isDataLoading}
        emptyMessage="No MCP usage data for the selected time range"
      />

      {/* Bottom charts - hidden in cross-org mode */}
      {!crossOrg && (
        <div className="grid grid-cols-1 lg:grid-cols-2 gap-6 mt-6">
          <div className="bg-bg-secondary rounded-xl border border-border p-6">
            <h3 className="text-sm font-semibold text-text-primary mb-4">Top Servers by Calls</h3>
            <HorizontalBar
              items={topServers.map((d) => ({
                label: d.group_key,
                value: d.total_calls,
                detail: formatNumber(d.total_calls),
              }))}
            />
          </div>

          <div className="bg-bg-secondary rounded-xl border border-border p-6">
            <h3 className="text-sm font-semibold text-text-primary mb-4">Top Tools by Calls</h3>
            <HorizontalBar
              items={topTools.map((d) => ({
                label: d.group_key,
                value: d.total_calls,
                detail: formatNumber(d.total_calls),
              }))}
            />
          </div>
        </div>
      )}

      <div className="mt-6">
        <div className="bg-bg-secondary rounded-xl border border-border p-6 max-w-sm">
          <h3 className="text-sm font-semibold text-text-primary mb-4">Status Distribution</h3>
          <DonutChart
            segments={[
              { label: 'Success', value: totals.success, color: '#22c55e' },
              { label: 'Error', value: totals.errors, color: '#ef4444' },
              { label: 'Timeout', value: totals.timeouts, color: '#f59e0b' },
            ]}
            centerLabel="Total"
            centerValue={formatNumber(totals.calls)}
          />
        </div>
      </div>
    </>
  )
}

