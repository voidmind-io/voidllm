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
      <div className="flex items-center gap-2 mb-6">
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
        <div className="sm:ml-auto flex gap-2">
          <Button
            variant="secondary"
            size="sm"
            onClick={() => exportData(modelRows as unknown as Record<string, unknown>[], COST_MODEL_HEADERS, `voidllm-cost-by-model-${range}`, 'csv')}
            disabled={modelRows.length === 0}
          >
            CSV
          </Button>
          <Button
            variant="secondary"
            size="sm"
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
          value={isModelLoading ? '...' : formatCost(totalCost)}
        />
        <StatCard
          label="Avg Cost / Day"
          value={isModelLoading ? '...' : formatCost(avgCostPerDay)}
        />
        <StatCard
          label="Top Model by Cost"
          value={isModelLoading ? '...' : topModel}
        />
      </div>

      {/* Cost by Model */}
      <div className="mb-8">
        <h2 className="text-lg font-semibold text-text-primary mb-4">Cost by Model</h2>
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
        <h2 className="text-lg font-semibold text-text-primary mb-4">Daily Cost Trend</h2>
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
