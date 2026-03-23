import { useMemo, useState } from 'react'
import { PageHeader } from '../components/ui/PageHeader'
import { Table } from '../components/ui/Table'
import type { Column } from '../components/ui/Table'
import { Badge, type BadgeProps } from '../components/ui/Badge'
import { Button } from '../components/ui/Button'
import { Select } from '../components/ui/Select'
import { StatCard } from '../components/ui/StatCard'
import { TimeAgo } from '../components/ui/TimeAgo'
import { UpgradePrompt } from '../components/ui/UpgradePrompt'
import { useMe } from '../hooks/useMe'
import { useLicense } from '../hooks/useLicense'
import { useAuditLog } from '../hooks/useAuditLog'
import type { AuditEvent } from '../hooks/useAuditLog'

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const TIME_RANGES = ['24h', '7d', '30d'] as const
type TimeRange = (typeof TIME_RANGES)[number]

const RANGE_LABELS: Record<TimeRange, string> = {
  '24h': 'Last 24h',
  '7d': 'Last 7d',
  '30d': 'Last 30d',
}

const RANGE_HOURS: Record<TimeRange, number> = {
  '24h': 24,
  '7d': 168,
  '30d': 720,
}

function getTimeRange(range: TimeRange): { from: string; to: string } {
  const now = new Date()
  const from = new Date(now.getTime() - RANGE_HOURS[range] * 3_600_000)
  return { from: from.toISOString(), to: now.toISOString() }
}

const RESOURCE_TYPE_OPTIONS = [
  { value: '', label: 'All Resources' },
  { value: 'org', label: 'Org' },
  { value: 'team', label: 'Team' },
  { value: 'user', label: 'User' },
  { value: 'key', label: 'Key' },
  { value: 'model', label: 'Model' },
  { value: 'service_account', label: 'Service Account' },
]

const ACTION_OPTIONS = [
  { value: '', label: 'All Actions' },
  { value: 'create', label: 'Create' },
  { value: 'update', label: 'Update' },
  { value: 'delete', label: 'Delete' },
  { value: 'revoke', label: 'Revoke' },
  { value: 'activate', label: 'Activate' },
  { value: 'deactivate', label: 'Deactivate' },
  { value: 'login', label: 'Login' },
]

const PAGE_SIZE_OPTIONS = [
  { value: '25', label: '25 / page' },
  { value: '50', label: '50 / page' },
  { value: '100', label: '100 / page' },
]

// ---------------------------------------------------------------------------
// Icons
// ---------------------------------------------------------------------------

function IconList() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <line x1="8" y1="6" x2="21" y2="6" />
      <line x1="8" y1="12" x2="21" y2="12" />
      <line x1="8" y1="18" x2="21" y2="18" />
      <line x1="3" y1="6" x2="3.01" y2="6" />
      <line x1="3" y1="12" x2="3.01" y2="12" />
      <line x1="3" y1="18" x2="3.01" y2="18" />
    </svg>
  )
}

function IconUser() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M20 21v-2a4 4 0 0 0-4-4H8a4 4 0 0 0-4 4v2" />
      <circle cx="12" cy="7" r="4" />
    </svg>
  )
}

function IconActivity() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <polyline points="22 12 18 12 15 21 9 3 6 12 2 12" />
    </svg>
  )
}

// ---------------------------------------------------------------------------
// Badge helpers
// ---------------------------------------------------------------------------

type BadgeVariant = NonNullable<BadgeProps['variant']>

const ACTION_BADGE: Record<string, BadgeVariant> = {
  create: 'success',
  update: 'info',
  delete: 'error',
  revoke: 'warning',
  activate: 'success',
  deactivate: 'muted',
  login: 'default',
}

function actionBadgeVariant(action: string): BadgeVariant {
  return ACTION_BADGE[action.toLowerCase()] ?? 'muted'
}

function statusBadgeVariant(code: number): BadgeVariant {
  if (code >= 200 && code < 300) return 'success'
  if (code >= 400 && code < 500) return 'warning'
  if (code >= 500) return 'error'
  return 'muted'
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function shortenId(id: string): string {
  if (!id) return '—'
  if (id.length <= 12) return id
  return `${id.slice(0, 8)}…`
}

// ---------------------------------------------------------------------------
// Table columns
// ---------------------------------------------------------------------------

const columns: Column<AuditEvent>[] = [
  {
    key: 'timestamp',
    header: 'Time',
    render: (row) => <TimeAgo date={row.timestamp} />,
  },
  {
    key: 'actor',
    header: 'Actor',
    render: (row) => (
      <span className="font-mono text-xs text-text-secondary" title={row.actor_id}>
        <span className="text-text-tertiary mr-1">{row.actor_type}</span>
        {shortenId(row.actor_id)}
      </span>
    ),
  },
  {
    key: 'action',
    header: 'Action',
    render: (row) => (
      <Badge variant={actionBadgeVariant(row.action)}>
        {row.action}
      </Badge>
    ),
  },
  {
    key: 'resource_type',
    header: 'Resource',
    render: (row) => (
      <Badge variant="muted">
        {row.resource_type}
      </Badge>
    ),
  },
  {
    key: 'description',
    header: 'Details',
    render: (row) => (
      row.description
        ? <code className="text-xs font-mono bg-bg-tertiary px-1.5 py-0.5 rounded text-text-secondary">{row.description}</code>
        : <span className="text-text-tertiary">—</span>
    ),
  },
  {
    key: 'ip_address',
    header: 'IP',
    render: (row) => (
      <span className="font-mono text-xs text-text-tertiary">
        {row.ip_address || '—'}
      </span>
    ),
  },
  {
    key: 'status_code',
    header: 'Status',
    align: 'right',
    render: (row) => (
      <Badge variant={statusBadgeVariant(row.status_code)}>
        {row.status_code}
      </Badge>
    ),
  },
]

// ---------------------------------------------------------------------------
// AuditLogPage
// ---------------------------------------------------------------------------

export default function AuditLogPage() {
  const [range, setRange] = useState<TimeRange>('24h')
  const [resourceType, setResourceType] = useState('')
  const [action, setAction] = useState('')
  const [pageSize, setPageSize] = useState(50)
  // '' represents the first page (no cursor); each subsequent entry is the
  // cursor returned by the previous page response.
  const [cursors, setCursors] = useState<string[]>([''])
  const currentCursor = cursors[cursors.length - 1]

  const { data: me } = useMe()
  const { data: license } = useLicense()
  const orgId = me?.org_id ?? ''

  const { from, to } = useMemo(() => getTimeRange(range), [range])

  const { data, isLoading } = useAuditLog({
    orgId,
    resourceType,
    action,
    from,
    to,
    limit: pageSize,
    cursor: currentCursor,
  })

  if (license && !license.features.includes('audit_logs')) {
    return (
      <UpgradePrompt
        title="Audit Log"
        description="Audit logging requires an Enterprise license."
      />
    )
  }

  const events = data?.data ?? []
  const hasPrevious = cursors.length > 1
  const hasNext = data?.has_more ?? false

  // Compute unique actors from current page
  const uniqueActors = useMemo(() => {
    const ids = new Set(events.map((e) => e.actor_id))
    return ids.size
  }, [events])

  function handleNext() {
    if (data?.cursor) {
      setCursors((prev) => [...prev, data.cursor!])
    }
  }

  function handlePrevious() {
    setCursors((prev) => (prev.length > 1 ? prev.slice(0, -1) : prev))
  }

  function handleRangeChange(newRange: TimeRange) {
    setRange(newRange)
    setCursors([''])
  }

  function handleResourceTypeChange(value: string) {
    setResourceType(value)
    setCursors([''])
  }

  function handleActionChange(value: string) {
    setAction(value)
    setCursors([''])
  }

  function handlePageSizeChange(value: string) {
    setPageSize(Number(value))
    setCursors([''])
  }

  const isDataLoading = isLoading && !!orgId

  const activeFilterCount = [resourceType, action].filter(Boolean).length
  const emptyMessage = activeFilterCount > 0
    ? `No audit events match the selected filters`
    : `No audit events found for the ${RANGE_LABELS[range].toLowerCase()} time range`

  return (
    <>
      <PageHeader
        title="Audit Log"
        description="Full audit trail of admin actions across your organization"
      />

      {/* Stat cards */}
      <div className="grid grid-cols-1 sm:grid-cols-3 gap-4 mb-6">
        <StatCard
          label="Total Events"
          value={isDataLoading ? '—' : events.length}
          icon={<IconList />}
          iconColor="purple"
        />
        <StatCard
          label="Unique Actors"
          value={isDataLoading ? '—' : uniqueActors}
          icon={<IconUser />}
          iconColor="blue"
        />
        <StatCard
          label="Active Filters"
          value={activeFilterCount}
          icon={<IconActivity />}
          iconColor={activeFilterCount > 0 ? 'yellow' : 'purple'}
        />
      </div>

      {/* Filter bar */}
      <div className="flex flex-col sm:flex-row sm:items-end gap-4 mb-6">
        {/* Time range segmented pills */}
        <div className="flex items-center gap-1 p-1 rounded-lg bg-bg-tertiary">
          {TIME_RANGES.map((r) => (
            <button
              key={r}
              type="button"
              onClick={() => handleRangeChange(r)}
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

        {/* Dropdowns pushed to the right */}
        <div className="flex items-end gap-3 sm:ml-auto">
          <div className="w-44">
            <Select
              label="Resource"
              value={resourceType}
              onChange={handleResourceTypeChange}
              options={RESOURCE_TYPE_OPTIONS}
              fullWidth
            />
          </div>
          <div className="w-40">
            <Select
              label="Action"
              value={action}
              onChange={handleActionChange}
              options={ACTION_OPTIONS}
              fullWidth
            />
          </div>
        </div>
      </div>

      {/* Table */}
      <Table<AuditEvent>
        columns={columns}
        data={events}
        keyExtractor={(row) => row.id}
        loading={isDataLoading}
        emptyMessage={emptyMessage}
      />

      {/* Pagination footer — always show when there are events so the
          page-size selector remains accessible on single-page results */}
      {events.length > 0 && (
        <div className="flex items-center justify-between mt-4">
          <div className="flex items-center gap-3">
            <span className="text-sm text-text-tertiary">
              {events.length} events
            </span>
            <div className="w-32">
              <Select
                value={String(pageSize)}
                onChange={handlePageSizeChange}
                options={PAGE_SIZE_OPTIONS}
                fullWidth
              />
            </div>
          </div>

          {(hasPrevious || hasNext) && (
            <div className="flex items-center gap-2">
              <Button
                variant="ghost"
                size="sm"
                disabled={!hasPrevious || isDataLoading}
                onClick={handlePrevious}
              >
                Previous
              </Button>
              <Button
                variant="ghost"
                size="sm"
                disabled={!hasNext || isDataLoading}
                onClick={handleNext}
              >
                Next
              </Button>
            </div>
          )}
        </div>
      )}
    </>
  )
}
