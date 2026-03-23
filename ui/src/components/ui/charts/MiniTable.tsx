import React from 'react'

export interface MiniTableColumn<T> {
  key: string
  header: string
  render?: (row: T) => React.ReactNode
  align?: 'left' | 'right'
}

export interface MiniTableProps<T> {
  columns: MiniTableColumn<T>[]
  data: T[]
}

export function MiniTable<T>({ columns, data }: MiniTableProps<T>) {
  return (
    <div className="w-full overflow-x-auto">
      <table className="w-full">
        <thead>
          <tr>
            {columns.map((col) => (
              <th
                key={col.key}
                className={`text-[10px] uppercase tracking-widest text-text-tertiary font-medium pb-3 ${
                  col.align === 'right' ? 'text-right' : 'text-left'
                }`}
              >
                {col.header}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {data.map((row, rowIdx) => (
            <tr key={rowIdx} className="border-b border-border/50 last:border-0">
              {columns.map((col) => (
                <td
                  key={col.key}
                  className={`py-3 text-sm ${col.align === 'right' ? 'text-right' : 'text-left'}`}
                >
                  {col.render ? col.render(row) : null}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}
