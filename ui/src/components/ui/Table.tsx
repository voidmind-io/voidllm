import React from 'react'
import { cn } from '../../lib/utils'
import { Button } from './Button'

export interface Column<T> {
  key: string
  header: string
  render: (row: T) => React.ReactNode
  sortable?: boolean
  width?: string
  align?: 'left' | 'center' | 'right'
}

export interface PaginationState {
  cursor: string | null
  hasMore: boolean
  onNext: () => void
  onPrevious: () => void
  hasPrevious: boolean
}

export interface SortState {
  column: string
  direction: 'asc' | 'desc'
}

export interface TableProps<T> {
  columns: Column<T>[]
  data: T[]
  keyExtractor: (row: T) => string
  pagination?: PaginationState
  sort?: SortState
  onSort?: (column: string) => void
  compact?: boolean
  loading?: boolean
  emptyMessage?: string
  emptyState?: React.ReactNode
  className?: string
  /** Render expanded content below a row. Return null for non-expandable rows. */
  renderExpandedRow?: (row: T) => React.ReactNode
  /** Set of keys (from keyExtractor) that are currently expanded. */
  expandedKeys?: Set<string>
  /** Called when a row's expand/collapse state is toggled. */
  onToggleExpand?: (key: string) => void
}

function SortIndicator({ column, sort }: { column: string; sort?: SortState }) {
  if (!sort || sort.column !== column) {
    return <span aria-hidden="true" className="text-text-tertiary/50">↕</span>
  }
  return <span aria-hidden="true">{sort.direction === 'asc' ? '↑' : '↓'}</span>
}

function SkeletonRows({
  columns,
  rows,
  cellPadding,
}: {
  columns: number
  rows: number
  cellPadding: string
}) {
  return (
    <>
      {Array.from({ length: rows }).map((_, i) => (
        <tr key={i} className="animate-pulse border-b border-border">
          {Array.from({ length: columns }).map((_, j) => (
            <td key={j} className={cellPadding}>
              <div className="h-4 bg-bg-tertiary rounded w-3/4" />
            </td>
          ))}
        </tr>
      ))}
    </>
  )
}

const alignClass: Record<NonNullable<Column<unknown>['align']>, string> = {
  left: 'text-left',
  center: 'text-center',
  right: 'text-right',
}

function ChevronIcon({ expanded }: { expanded: boolean }) {
  return (
    <svg
      width="14"
      height="14"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2.5"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      className={cn('transition-transform duration-150', expanded && 'rotate-90')}
    >
      <polyline points="9 18 15 12 9 6" />
    </svg>
  )
}

export function Table<T>({
  columns,
  data,
  keyExtractor,
  pagination,
  sort,
  onSort,
  compact = false,
  loading = false,
  emptyMessage = 'No results found.',
  emptyState,
  className,
  renderExpandedRow,
  expandedKeys,
  onToggleExpand,
}: TableProps<T>) {
  const cellPadding = compact ? 'px-3 py-2' : 'px-4 py-3'
  const hasExpand = renderExpandedRow != null
  const totalCols = hasExpand ? columns.length + 1 : columns.length

  return (
    <div className={cn('overflow-x-auto rounded-lg border border-border', className)}>
      <table className="min-w-full">
        <thead>
          <tr className="bg-bg-tertiary/50 border-b border-border">
            {hasExpand && (
              <th scope="col" className="w-8 px-2" aria-label="Expand" />
            )}
            {columns.map((col) => {
              const isSortable = col.sortable === true && onSort != null
              return (
                <th
                  key={col.key}
                  scope="col"
                  className={cn(
                    cellPadding,
                    'text-xs font-medium text-text-tertiary uppercase tracking-wider text-left',
                    col.align != null && alignClass[col.align],
                    col.width,
                    isSortable && 'cursor-pointer select-none hover:text-text-secondary',
                  )}
                  aria-sort={
                    sort?.column === col.key
                      ? sort.direction === 'asc'
                        ? 'ascending'
                        : 'descending'
                      : col.sortable
                        ? 'none'
                        : undefined
                  }
                  tabIndex={isSortable ? 0 : undefined}
                  onClick={isSortable ? () => onSort!(col.key) : undefined}
                  onKeyDown={
                    isSortable
                      ? (e: React.KeyboardEvent) => {
                          if (e.key === 'Enter' || e.key === ' ') {
                            e.preventDefault()
                            onSort!(col.key)
                          }
                        }
                      : undefined
                  }
                >
                  <span className="inline-flex items-center gap-1">
                    {col.header}
                    {col.sortable && <SortIndicator column={col.key} sort={sort} />}
                  </span>
                </th>
              )
            })}
          </tr>
        </thead>
        <tbody>
          {loading ? (
            <SkeletonRows columns={totalCols} rows={5} cellPadding={cellPadding} />
          ) : data.length === 0 ? (
            <tr>
              <td colSpan={totalCols}>
                {emptyState != null ? (
                  emptyState
                ) : (
                  <div className="text-center text-text-tertiary text-sm py-12">
                    {emptyMessage}
                  </div>
                )}
              </td>
            </tr>
          ) : (
            data.map((row, rowIndex) => {
              const key = keyExtractor(row)
              const expandedContent = hasExpand ? renderExpandedRow!(row) : null
              const isExpandable = expandedContent != null
              const isExpanded = isExpandable && (expandedKeys?.has(key) ?? false)
              const isLastRow = rowIndex === data.length - 1
              const showRowBorder = !isLastRow || isExpanded

              return (
                <React.Fragment key={key}>
                  <tr
                    className={cn(
                      'hover:bg-bg-tertiary/30 transition-colors',
                      showRowBorder && 'border-b border-border',
                    )}
                  >
                    {hasExpand && (
                      <td className="w-8 px-2">
                        {isExpandable ? (
                          <button
                            type="button"
                            onClick={() => onToggleExpand?.(key)}
                            className="flex items-center justify-center w-6 h-6 rounded text-text-tertiary hover:text-text-primary hover:bg-bg-tertiary transition-colors"
                            aria-label={isExpanded ? 'Collapse row' : 'Expand row'}
                            aria-expanded={isExpanded}
                          >
                            <ChevronIcon expanded={isExpanded} />
                          </button>
                        ) : null}
                      </td>
                    )}
                    {columns.map((col) => (
                      <td
                        key={col.key}
                        className={cn(
                          cellPadding,
                          'text-sm',
                          col.align != null && alignClass[col.align],
                          col.width,
                        )}
                      >
                        {col.render(row)}
                      </td>
                    ))}
                  </tr>
                  {isExpanded && (
                    <tr className={cn(!isLastRow && 'border-b border-border')}>
                      <td colSpan={totalCols} className="p-0 bg-bg-secondary/30">
                        {expandedContent}
                      </td>
                    </tr>
                  )}
                </React.Fragment>
              )
            })
          )}
        </tbody>
      </table>
      {pagination != null && (
        <div className="flex items-center justify-end gap-2 px-4 py-3 border-t border-border">
          <Button
            variant="ghost"
            size="sm"
            disabled={!pagination.hasPrevious}
            onClick={pagination.onPrevious}
          >
            Previous
          </Button>
          <Button
            variant="ghost"
            size="sm"
            disabled={!pagination.hasMore}
            onClick={pagination.onNext}
          >
            Next
          </Button>
        </div>
      )}
    </div>
  )
}
