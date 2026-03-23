import { useMemo, useState } from 'react'
import { PageHeader } from '../components/ui/PageHeader'
import { StatCard } from '../components/ui/StatCard'
import { Table } from '../components/ui/Table'
import type { Column } from '../components/ui/Table'
import { Button } from '../components/ui/Button'
import { UpgradePrompt } from '../components/ui/UpgradePrompt'
import { useMe } from '../hooks/useMe'
import { useLicense } from '../hooks/useLicense'
import { useUsage } from '../hooks/useUsage'
import type { UsageDataPoint } from '../hooks/useUsage'
import { formatNumber, formatCost } from '../lib/utils'
import { exportData } from '../lib/export'

const COST_MODEL_HEADERS = [
  { key: 'group_key', label: 'Model' },
  { key: 'cost_estimate', label: 'Total Cost' },
  { key: 'pct', label: '% of Total' },
  { key: 'total_requests', label: 'Requests' },
  { key: 'avg_cost_per_request', label: 'Avg Cost / Request' },
]

const TIME_RANGES = ['7d', '30d', '90d'] as const
type TimeRange = (typeof TIME_RANGES)[number]

const RANGE_LABELS: Record<TimeRange, string> = {
  '7d': 'Last 7 days',
  '30d': 'Last 30 days',
  '90d': 'Last 90 days',
}

const RANGE_DAYS: Record<TimeRange, number> = {
  '7d': 7,
  '30d': 30,
  '90d': 90,
}

function getTimeRange(range: TimeRange): { from: string; to: string } {
  const now = new Date()
  const from = new Date(now.getTime() - RANGE_DAYS[range] * 86_400_000)
  return { from: from.toISOString(), to: now.toISOString() }
}

// ---------------------------------------------------------------------------
// Icons
// ---------------------------------------------------------------------------

function IconDollar() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <line x1="12" y1="1" x2="12" y2="23" />
      <path d="M17 5H9.5a3.5 3.5 0 0 0 0 7h5a3.5 3.5 0 0 1 0 7H6" />
    </svg>
  )
}

function IconTrendingDown() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <polyline points="23 18 13.5 8.5 8.5 13.5 1 6" />
      <polyline points="17 18 23 18 23 12" />
    </svg>
  )
}

function IconCpu() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <rect x="4" y="4" width="16" height="16" rx="2" ry="2" />
      <rect x="9" y="9" width="6" height="6" />
      <line x1="9" y1="1" x2="9" y2="4" />
      <line x1="15" y1="1" x2="15" y2="4" />
      <line x1="9" y1="20" x2="9" y2="23" />
      <line x1="15" y1="20" x2="15" y2="23" />
      <line x1="20" y1="9" x2="23" y2="9" />
      <line x1="20" y1="14" x2="23" y2="14" />
      <line x1="1" y1="9" x2="4" y2="9" />
      <line x1="1" y1="14" x2="4" y2="14" />
    </svg>
  )
}

function IconDownload() {
  return (
    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4" />
      <polyline points="7 10 12 15 17 10" />
      <line x1="12" y1="15" x2="12" y2="3" />
    </svg>
  )
}

// ---------------------------------------------------------------------------
// Cost by Model table
// ---------------------------------------------------------------------------

interface ModelCostRow extends UsageDataPoint {
  pct: number
  avg_cost_per_request: number
}

function buildModelColumns(totalCost: number): Column<ModelCostRow>[] {
  void totalCost // referenced through row.pct pre-computed
  return [
    {
      key: 'group_key',
      header: 'Model',
      render: (row) => (
        <span className="font-mono text-text-primary">{row.group_key}</span>
      ),
    },
    {
      key: 'cost_estimate',
      header: 'Total Cost',
      align: 'right',
      render: (row) => (
        <span className="text-text-primary font-medium">{formatCost(row.cost_estimate)}</span>
      ),
    },
    {
      key: 'pct',
      header: '% of Total',
      align: 'right',
      render: (row) => (
        <span className="text-text-secondary">{row.pct.toFixed(1)}%</span>
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
      key: 'avg_cost_per_request',
      header: 'Avg Cost / Request',
      align: 'right',
      render: (row) => (
        <span className="text-text-tertiary">
          {row.total_requests > 0
            ? formatCost(row.cost_estimate / row.total_requests)
            : formatCost(0)}
        </span>
      ),
    },
  ]
}

// ---------------------------------------------------------------------------
// Daily Cost Trend table
// ---------------------------------------------------------------------------

interface DayCostRow extends UsageDataPoint {
  change_pct: number | null
}

const dayColumns: Column<DayCostRow>[] = [
  {
    key: 'group_key',
    header: 'Date',
    render: (row) => (
      <span className="font-mono text-text-primary">{row.group_key}</span>
    ),
  },
  {
    key: 'cost_estimate',
    header: 'Cost',
    align: 'right',
    render: (row) => (
      <span className="text-text-primary font-medium">{formatCost(row.cost_estimate)}</span>
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
    key: 'avg_cost_per_request',
    header: 'Avg Cost / Request',
    align: 'right',
    render: (row) => (
      <span className="text-text-tertiary">
        {row.total_requests > 0
          ? formatCost(row.cost_estimate / row.total_requests)
          : formatCost(0)}
      </span>
    ),
  },
  {
    key: 'change_pct',
    header: 'vs Prior Day',
    align: 'right',
    render: (row) => {
      if (row.change_pct === null) {
        return <span className="text-text-tertiary">—</span>
      }
      const isPositive = row.change_pct > 0
      const isNeutral = row.change_pct === 0
      const colorClass = isNeutral
        ? 'text-text-tertiary'
        : isPositive
          ? 'text-error'
          : 'text-success'
      const arrow = isNeutral ? '—' : isPositive ? '▲' : '▼'
      return (
        <span className={colorClass}>
          {arrow} {Math.abs(row.change_pct).toFixed(1)}%
        </span>
      )
    },
  },
]

// ---------------------------------------------------------------------------
// CostReportsPage
// ---------------------------------------------------------------------------

export default function CostReportsPage() {
  const [range, setRange] = useState<TimeRange>('30d')
  const { data: me } = useMe()
  const { data: license } = useLicense()
  const orgId = me?.org_id ?? ''

  const featureEnabled = !license || license.features.includes('cost_reports')
  const { from, to } = useMemo(() => getTimeRange(range), [range])

  const { data: modelUsage, isLoading: modelLoading } = useUsage(orgId, from, to, 'model')
  const { data: dayUsage, isLoading: dayLoading } = useUsage(orgId, from, to, 'day')

  // Compute totals and model rows
  const { totalCost, modelRows, avgCostPerDay, topModel } = useMemo(() => {
    const data = modelUsage?.data ?? []
    const total = data.reduce((acc, d) => acc + d.cost_estimate, 0)
    const days = RANGE_DAYS[range]
    const avg = days > 0 ? total / days : 0
    const sorted = [...data].sort((a, b) => b.cost_estimate - a.cost_estimate)
    const rows: ModelCostRow[] = sorted.map((d) => ({
      ...d,
      pct: total > 0 ? (d.cost_estimate / total) * 100 : 0,
      avg_cost_per_request:
        d.total_requests > 0 ? d.cost_estimate / d.total_requests : 0,
    }))
    const top = sorted[0]?.group_key ?? '—'
    return { totalCost: total, modelRows: rows, avgCostPerDay: avg, topModel: top }
  }, [modelUsage, range])

  // Compute day rows with change vs prior day
  const dayRows: DayCostRow[] = useMemo(() => {
    const data = dayUsage?.data ?? []
    const sorted = [...data].sort((a, b) => a.group_key.localeCompare(b.group_key))
    return sorted.map((d, i) => {
      const prior = i > 0 ? sorted[i - 1].cost_estimate : null
      let change_pct: number | null = null
      if (prior !== null && prior > 0) {
        change_pct = ((d.cost_estimate - prior) / prior) * 100
      } else if (prior === 0 && d.cost_estimate > 0) {
        change_pct = 100
      } else if (prior !== null) {
        change_pct = 0
      }
      return { ...d, change_pct }
    })
  }, [dayUsage])

  const dayRowsDesc = useMemo(() => [...dayRows].reverse(), [dayRows])
  const modelColumns = useMemo(() => buildModelColumns(totalCost), [totalCost])

  if (!featureEnabled) {
    return (
      <UpgradePrompt
        title="Cost Reports"
        description="Cost reports and budget alerts require a Pro or Enterprise license."
      />
    )
  }

  const isModelLoading = modelLoading && !!orgId
  const isDayLoading = dayLoading && !!orgId

  return (
    <>
      <PageHeader
        title="Cost Reports"
        description="Cost breakdown and trends across models"
      />

      {/* Time range pills + export */}
      <div className="flex items-center gap-3 mb-6">
        {/* Segmented pill container */}
        <div className="flex items-center gap-1 p-1 rounded-lg bg-bg-tertiary">
          {TIME_RANGES.map((r) => (
            <button
              key={r}
              type="button"
              onClick={() => setRange(r)}
              className={[
                'px-3 py-1.5 rounded-md text-sm font-medium transition-colors',
                range === r
                  ? 'bg-bg-secondary text-text-primary shadow-sm'
                  : 'text-text-tertiary hover:text-text-secondary',
              ].join(' ')}
            >
              {RANGE_LABELS[r]}
            </button>
          ))}
        </div>

        <div className="sm:ml-auto flex gap-2">
          <Button
            variant="secondary"
            size="sm"
            icon={<IconDownload />}
            onClick={() => exportData(modelRows as unknown as Record<string, unknown>[], COST_MODEL_HEADERS, `voidllm-cost-by-model-${range}`, 'csv')}
            disabled={modelRows.length === 0}
          >
            CSV
          </Button>
          <Button
            variant="secondary"
            size="sm"
            icon={<IconDownload />}
            onClick={() => exportData(modelRows as unknown as Record<string, unknown>[], COST_MODEL_HEADERS, `voidllm-cost-by-model-${range}`, 'json')}
            disabled={modelRows.length === 0}
          >
            JSON
          </Button>
        </div>
      </div>

      {/* Stat cards */}
      <div className="grid grid-cols-1 sm:grid-cols-3 gap-4 mb-8">
        <StatCard
          label="Total Cost"
          value={isModelLoading ? '—' : formatCost(totalCost)}
          icon={<IconDollar />}
          iconColor="purple"
        />
        <StatCard
          label="Avg Cost / Day"
          value={isModelLoading ? '—' : formatCost(avgCostPerDay)}
          icon={<IconTrendingDown />}
          iconColor="blue"
        />
        <StatCard
          label="Top Model by Cost"
          value={isModelLoading ? '—' : topModel}
          icon={<IconCpu />}
          iconColor="yellow"
        />
      </div>

      {/* Cost by Model */}
      <div className="mb-8">
        <h2 className="text-sm font-semibold text-text-secondary uppercase tracking-wider mb-4">Cost by Model</h2>
        <Table<ModelCostRow>
          columns={modelColumns}
          data={modelRows}
          keyExtractor={(row) => row.group_key}
          loading={isModelLoading}
          emptyMessage="No cost data for the selected time range"
        />
      </div>

      {/* Daily Cost Trend */}
      <div>
        <h2 className="text-sm font-semibold text-text-secondary uppercase tracking-wider mb-4">Daily Cost Trend</h2>
        <Table<DayCostRow>
          columns={dayColumns}
          data={dayRowsDesc}
          keyExtractor={(row) => row.group_key}
          loading={isDayLoading}
          emptyMessage="No daily cost data for the selected time range"
        />
      </div>
    </>
  )
}
