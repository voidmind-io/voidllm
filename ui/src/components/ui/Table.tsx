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
  /** Optional custom empty state node. Takes priority over emptyMessage when data is empty and not loading. */
  emptyState?: React.ReactNode
  className?: string
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
}: TableProps<T>) {
  const cellPadding = compact ? 'px-3 py-2' : 'px-4 py-3'

  return (
    <div className={cn('overflow-x-auto rounded-lg border border-border', className)}>
      <table className="min-w-full">
        <thead>
          <tr className="bg-bg-tertiary/50 border-b border-border">
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
            <SkeletonRows columns={columns.length} rows={5} cellPadding={cellPadding} />
          ) : data.length === 0 ? (
            <tr>
              <td colSpan={columns.length}>
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
            data.map((row, rowIndex) => (
              <tr
                key={keyExtractor(row)}
                className={cn(
                  'hover:bg-bg-tertiary/30 transition-colors',
                  rowIndex < data.length - 1 && 'border-b border-border',
                )}
              >
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
            ))
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
