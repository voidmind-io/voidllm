import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, it, expect, vi } from 'vitest'
import { Table } from './Table'
import type { Column } from './Table'

interface TestRow {
  id: string
  name: string
  count: number
}

const testColumns: Column<TestRow>[] = [
  { key: 'name', header: 'Name', render: (r) => r.name },
  { key: 'count', header: 'Count', render: (r) => r.count, align: 'right' },
]

const testData: TestRow[] = [
  { id: '1', name: 'Alpha', count: 10 },
  { id: '2', name: 'Beta', count: 20 },
  { id: '3', name: 'Charlie', count: 30 },
]

function keyExtractor(row: TestRow) {
  return row.id
}

describe('Table', () => {
  describe('Rendering', () => {
    it('renders headers from column definitions', () => {
      render(<Table columns={testColumns} data={testData} keyExtractor={keyExtractor} />)
      const headers = screen.getAllByRole('columnheader')
      expect(headers).toHaveLength(2)
      expect(headers[0]).toHaveTextContent('Name')
      expect(headers[1]).toHaveTextContent('Count')
    })

    it('renders data rows with correct cell content', () => {
      render(<Table columns={testColumns} data={testData} keyExtractor={keyExtractor} />)
      expect(screen.getByText('Alpha')).toBeInTheDocument()
      expect(screen.getByText('Beta')).toBeInTheDocument()
      expect(screen.getByText('Charlie')).toBeInTheDocument()
      expect(screen.getByText('10')).toBeInTheDocument()
      expect(screen.getByText('20')).toBeInTheDocument()
      expect(screen.getByText('30')).toBeInTheDocument()
      // header row + 3 data rows
      expect(screen.getAllByRole('row')).toHaveLength(4)
    })

    it('shows default empty message when data is empty', () => {
      render(<Table columns={testColumns} data={[]} keyExtractor={keyExtractor} />)
      expect(screen.getByText('No results found.')).toBeInTheDocument()
    })

    it('shows custom emptyMessage when provided', () => {
      render(
        <Table
          columns={testColumns}
          data={[]}
          keyExtractor={keyExtractor}
          emptyMessage="No data"
        />,
      )
      expect(screen.getByText('No data')).toBeInTheDocument()
    })

    it('shows skeleton rows with animate-pulse when loading', () => {
      render(<Table columns={testColumns} data={testData} keyExtractor={keyExtractor} loading />)
      const rows = screen.getAllByRole('row')
      // header row + 5 skeleton rows
      const skeletonRows = rows.slice(1)
      expect(skeletonRows).toHaveLength(5)
      skeletonRows.forEach((row) => {
        expect(row.className).toContain('animate-pulse')
      })
    })

    it('hides data rows when loading', () => {
      render(<Table columns={testColumns} data={testData} keyExtractor={keyExtractor} loading />)
      expect(screen.queryByText('Alpha')).not.toBeInTheDocument()
      expect(screen.queryByText('Beta')).not.toBeInTheDocument()
    })

    it('hides empty state when loading', () => {
      render(
        <Table
          columns={testColumns}
          data={[]}
          keyExtractor={keyExtractor}
          loading
          emptyMessage="No data"
        />,
      )
      expect(screen.queryByText('No data')).not.toBeInTheDocument()
    })
  })

  describe('Columns', () => {
    it('right-aligned column has text-right class on th and td', () => {
      render(<Table columns={testColumns} data={testData} keyExtractor={keyExtractor} />)
      const headers = screen.getAllByRole('columnheader')
      // Count column is align: 'right'
      expect(headers[1].className).toContain('text-right')
      const cells = screen.getAllByRole('cell')
      // cells 1, 3, 5 are the count cells (every second cell)
      expect(cells[1].className).toContain('text-right')
      expect(cells[3].className).toContain('text-right')
      expect(cells[5].className).toContain('text-right')
    })

    it('center-aligned column has text-center class', () => {
      const centeredColumns: Column<TestRow>[] = [
        { key: 'name', header: 'Name', render: (r) => r.name, align: 'center' },
      ]
      render(<Table columns={centeredColumns} data={testData} keyExtractor={keyExtractor} />)
      const header = screen.getByRole('columnheader', { name: 'Name' })
      expect(header.className).toContain('text-center')
    })

    it('left-aligned default column does not have text-right or text-center', () => {
      render(<Table columns={testColumns} data={testData} keyExtractor={keyExtractor} />)
      const headers = screen.getAllByRole('columnheader')
      // Name column has no explicit align — defaults to text-left from base class
      expect(headers[0].className).not.toContain('text-right')
      expect(headers[0].className).not.toContain('text-center')
    })

    it('column width class is applied on th and td', () => {
      const widthColumns: Column<TestRow>[] = [
        { key: 'name', header: 'Name', render: (r) => r.name, width: 'w-1/2' },
      ]
      render(<Table columns={widthColumns} data={testData} keyExtractor={keyExtractor} />)
      const header = screen.getByRole('columnheader', { name: 'Name' })
      expect(header.className).toContain('w-1/2')
      const cells = screen.getAllByRole('cell')
      expect(cells[0].className).toContain('w-1/2')
    })

    it('custom render function output appears in cells', () => {
      const customColumns: Column<TestRow>[] = [
        {
          key: 'name',
          header: 'Name',
          render: (r) => <span data-testid={`badge-${r.id}`}>{r.name.toUpperCase()}</span>,
        },
      ]
      render(<Table columns={customColumns} data={testData} keyExtractor={keyExtractor} />)
      expect(screen.getByTestId('badge-1')).toBeInTheDocument()
      expect(screen.getByTestId('badge-1')).toHaveTextContent('ALPHA')
      expect(screen.getByTestId('badge-2')).toHaveTextContent('BETA')
    })
  })

  describe('Sorting', () => {
    it('clicking a sortable column calls onSort with the column key', async () => {
      const onSort = vi.fn()
      const sortableColumns: Column<TestRow>[] = [
        { key: 'name', header: 'Name', render: (r) => r.name, sortable: true },
        { key: 'count', header: 'Count', render: (r) => r.count },
      ]
      render(
        <Table
          columns={sortableColumns}
          data={testData}
          keyExtractor={keyExtractor}
          onSort={onSort}
        />,
      )
      await userEvent.click(screen.getByRole('columnheader', { name: /name/i }))
      expect(onSort).toHaveBeenCalledOnce()
      expect(onSort).toHaveBeenCalledWith('name')
    })

    it('clicking a non-sortable column does NOT call onSort', async () => {
      const onSort = vi.fn()
      const sortableColumns: Column<TestRow>[] = [
        { key: 'name', header: 'Name', render: (r) => r.name, sortable: true },
        { key: 'count', header: 'Count', render: (r) => r.count },
      ]
      render(
        <Table
          columns={sortableColumns}
          data={testData}
          keyExtractor={keyExtractor}
          onSort={onSort}
        />,
      )
      await userEvent.click(screen.getByRole('columnheader', { name: /count/i }))
      expect(onSort).not.toHaveBeenCalled()
    })

    it('active sort column shows ↑ for asc direction', () => {
      const sortableColumns: Column<TestRow>[] = [
        { key: 'name', header: 'Name', render: (r) => r.name, sortable: true },
      ]
      render(
        <Table
          columns={sortableColumns}
          data={testData}
          keyExtractor={keyExtractor}
          onSort={vi.fn()}
          sort={{ column: 'name', direction: 'asc' }}
        />,
      )
      const header = screen.getByRole('columnheader', { name: /name/i })
      expect(header.textContent).toContain('↑')
    })

    it('active sort column shows ↓ for desc direction', () => {
      const sortableColumns: Column<TestRow>[] = [
        { key: 'name', header: 'Name', render: (r) => r.name, sortable: true },
      ]
      render(
        <Table
          columns={sortableColumns}
          data={testData}
          keyExtractor={keyExtractor}
          onSort={vi.fn()}
          sort={{ column: 'name', direction: 'desc' }}
        />,
      )
      const header = screen.getByRole('columnheader', { name: /name/i })
      expect(header.textContent).toContain('↓')
    })

    it('inactive sortable column shows neutral ↕ indicator', () => {
      const sortableColumns: Column<TestRow>[] = [
        { key: 'name', header: 'Name', render: (r) => r.name, sortable: true },
        { key: 'count', header: 'Count', render: (r) => r.count, sortable: true },
      ]
      render(
        <Table
          columns={sortableColumns}
          data={testData}
          keyExtractor={keyExtractor}
          onSort={vi.fn()}
          sort={{ column: 'name', direction: 'asc' }}
        />,
      )
      // Count is sortable but not the active sort column — should show ↕
      const countHeader = screen.getByRole('columnheader', { name: /count/i })
      expect(countHeader.textContent).toContain('↕')
    })

    it('no sort indicators when column is not sortable and onSort is not provided', () => {
      // testColumns has no sortable:true columns, so no SortIndicator is rendered at all
      render(
        <Table columns={testColumns} data={testData} keyExtractor={keyExtractor} />,
      )
      const headers = screen.getAllByRole('columnheader')
      headers.forEach((th) => {
        expect(th.textContent).not.toContain('↑')
        expect(th.textContent).not.toContain('↓')
        expect(th.textContent).not.toContain('↕')
      })
    })

    it('sorted-ascending header has aria-sort="ascending"', () => {
      const sortableColumns: Column<TestRow>[] = [
        { key: 'name', header: 'Name', render: (r) => r.name, sortable: true },
      ]
      render(
        <Table
          columns={sortableColumns}
          data={testData}
          keyExtractor={keyExtractor}
          onSort={vi.fn()}
          sort={{ column: 'name', direction: 'asc' }}
        />,
      )
      const header = screen.getByRole('columnheader', { name: /name/i })
      expect(header).toHaveAttribute('aria-sort', 'ascending')
    })

    it('sorted-descending header has aria-sort="descending"', () => {
      const sortableColumns: Column<TestRow>[] = [
        { key: 'name', header: 'Name', render: (r) => r.name, sortable: true },
      ]
      render(
        <Table
          columns={sortableColumns}
          data={testData}
          keyExtractor={keyExtractor}
          onSort={vi.fn()}
          sort={{ column: 'name', direction: 'desc' }}
        />,
      )
      const header = screen.getByRole('columnheader', { name: /name/i })
      expect(header).toHaveAttribute('aria-sort', 'descending')
    })

    it('inactive sortable header has aria-sort="none"', () => {
      const sortableColumns: Column<TestRow>[] = [
        { key: 'name', header: 'Name', render: (r) => r.name, sortable: true },
        { key: 'count', header: 'Count', render: (r) => r.count, sortable: true },
      ]
      render(
        <Table
          columns={sortableColumns}
          data={testData}
          keyExtractor={keyExtractor}
          onSort={vi.fn()}
          sort={{ column: 'name', direction: 'asc' }}
        />,
      )
      const countHeader = screen.getByRole('columnheader', { name: /count/i })
      expect(countHeader).toHaveAttribute('aria-sort', 'none')
    })

    it('non-sortable header has no aria-sort attribute', () => {
      render(<Table columns={testColumns} data={testData} keyExtractor={keyExtractor} />)
      const headers = screen.getAllByRole('columnheader')
      headers.forEach((th) => {
        expect(th).not.toHaveAttribute('aria-sort')
      })
    })

    it('sortable header is focusable (tabIndex=0)', () => {
      const sortableColumns: Column<TestRow>[] = [
        { key: 'name', header: 'Name', render: (r) => r.name, sortable: true },
        { key: 'count', header: 'Count', render: (r) => r.count },
      ]
      render(
        <Table
          columns={sortableColumns}
          data={testData}
          keyExtractor={keyExtractor}
          onSort={vi.fn()}
        />,
      )
      expect(screen.getByRole('columnheader', { name: /name/i })).toHaveAttribute('tabindex', '0')
      expect(screen.getByRole('columnheader', { name: /count/i })).not.toHaveAttribute('tabindex')
    })

    it('Enter key on sortable header calls onSort', async () => {
      const onSort = vi.fn()
      const sortableColumns: Column<TestRow>[] = [
        { key: 'name', header: 'Name', render: (r) => r.name, sortable: true },
      ]
      render(
        <Table
          columns={sortableColumns}
          data={testData}
          keyExtractor={keyExtractor}
          onSort={onSort}
        />,
      )
      const header = screen.getByRole('columnheader', { name: /name/i })
      header.focus()
      await userEvent.keyboard('{Enter}')
      expect(onSort).toHaveBeenCalledOnce()
      expect(onSort).toHaveBeenCalledWith('name')
    })

    it('Space key on sortable header calls onSort', async () => {
      const onSort = vi.fn()
      const sortableColumns: Column<TestRow>[] = [
        { key: 'name', header: 'Name', render: (r) => r.name, sortable: true },
      ]
      render(
        <Table
          columns={sortableColumns}
          data={testData}
          keyExtractor={keyExtractor}
          onSort={onSort}
        />,
      )
      const header = screen.getByRole('columnheader', { name: /name/i })
      header.focus()
      await userEvent.keyboard(' ')
      expect(onSort).toHaveBeenCalledOnce()
      expect(onSort).toHaveBeenCalledWith('name')
    })

    it('sort indicator arrows have aria-hidden="true"', () => {
      const sortableColumns: Column<TestRow>[] = [
        { key: 'name', header: 'Name', render: (r) => r.name, sortable: true },
        { key: 'count', header: 'Count', render: (r) => r.count, sortable: true },
      ]
      render(
        <Table
          columns={sortableColumns}
          data={testData}
          keyExtractor={keyExtractor}
          onSort={vi.fn()}
          sort={{ column: 'name', direction: 'asc' }}
        />,
      )
      // Both the active (↑) and inactive (↕) indicators must have aria-hidden
      const nameHeader = screen.getByRole('columnheader', { name: /name/i })
      const countHeader = screen.getByRole('columnheader', { name: /count/i })
      const activeSpan = nameHeader.querySelector('span[aria-hidden="true"]')
      const inactiveSpan = countHeader.querySelector('span[aria-hidden="true"]')
      expect(activeSpan).not.toBeNull()
      expect(inactiveSpan).not.toBeNull()
    })
  })

  describe('Pagination', () => {
    const basePagination = {
      cursor: null,
      hasMore: true,
      hasPrevious: false,
      onNext: vi.fn(),
      onPrevious: vi.fn(),
    }

    it('renders pagination footer when pagination prop is provided', () => {
      render(
        <Table
          columns={testColumns}
          data={testData}
          keyExtractor={keyExtractor}
          pagination={basePagination}
        />,
      )
      expect(screen.getByRole('button', { name: 'Previous' })).toBeInTheDocument()
      expect(screen.getByRole('button', { name: 'Next' })).toBeInTheDocument()
    })

    it('does not render pagination footer when pagination prop is omitted', () => {
      render(<Table columns={testColumns} data={testData} keyExtractor={keyExtractor} />)
      expect(screen.queryByRole('button', { name: 'Previous' })).not.toBeInTheDocument()
      expect(screen.queryByRole('button', { name: 'Next' })).not.toBeInTheDocument()
    })

    it('Previous button is disabled when hasPrevious=false', () => {
      render(
        <Table
          columns={testColumns}
          data={testData}
          keyExtractor={keyExtractor}
          pagination={{ ...basePagination, hasPrevious: false }}
        />,
      )
      expect(screen.getByRole('button', { name: 'Previous' })).toBeDisabled()
    })

    it('Next button is disabled when hasMore=false', () => {
      render(
        <Table
          columns={testColumns}
          data={testData}
          keyExtractor={keyExtractor}
          pagination={{ ...basePagination, hasMore: false }}
        />,
      )
      expect(screen.getByRole('button', { name: 'Next' })).toBeDisabled()
    })

    it('Previous button is enabled when hasPrevious=true', () => {
      render(
        <Table
          columns={testColumns}
          data={testData}
          keyExtractor={keyExtractor}
          pagination={{ ...basePagination, hasPrevious: true }}
        />,
      )
      expect(screen.getByRole('button', { name: 'Previous' })).not.toBeDisabled()
    })

    it('Next button is enabled when hasMore=true', () => {
      render(
        <Table
          columns={testColumns}
          data={testData}
          keyExtractor={keyExtractor}
          pagination={{ ...basePagination, hasMore: true }}
        />,
      )
      expect(screen.getByRole('button', { name: 'Next' })).not.toBeDisabled()
    })

    it('clicking Previous calls onPrevious', async () => {
      const onPrevious = vi.fn()
      render(
        <Table
          columns={testColumns}
          data={testData}
          keyExtractor={keyExtractor}
          pagination={{ ...basePagination, hasPrevious: true, onPrevious }}
        />,
      )
      await userEvent.click(screen.getByRole('button', { name: 'Previous' }))
      expect(onPrevious).toHaveBeenCalledOnce()
    })

    it('clicking Next calls onNext', async () => {
      const onNext = vi.fn()
      render(
        <Table
          columns={testColumns}
          data={testData}
          keyExtractor={keyExtractor}
          pagination={{ ...basePagination, hasMore: true, onNext }}
        />,
      )
      await userEvent.click(screen.getByRole('button', { name: 'Next' }))
      expect(onNext).toHaveBeenCalledOnce()
    })
  })

  describe('Compact mode', () => {
    it('uses px-3 padding on headers in compact mode', () => {
      render(
        <Table columns={testColumns} data={testData} keyExtractor={keyExtractor} compact />,
      )
      const headers = screen.getAllByRole('columnheader')
      headers.forEach((th) => {
        expect(th.className).toContain('px-3')
        expect(th.className).not.toContain('px-4')
      })
    })

    it('uses px-3 padding on cells in compact mode', () => {
      render(
        <Table columns={testColumns} data={testData} keyExtractor={keyExtractor} compact />,
      )
      const cells = screen.getAllByRole('cell')
      cells.forEach((td) => {
        expect(td.className).toContain('px-3')
        expect(td.className).not.toContain('px-4')
      })
    })

    it('uses px-4 padding on headers in default (non-compact) mode', () => {
      render(<Table columns={testColumns} data={testData} keyExtractor={keyExtractor} />)
      const headers = screen.getAllByRole('columnheader')
      headers.forEach((th) => {
        expect(th.className).toContain('px-4')
      })
    })
  })

  describe('Responsive', () => {
    it('wrapper div has overflow-x-auto class', () => {
      const { container } = render(
        <Table columns={testColumns} data={testData} keyExtractor={keyExtractor} />,
      )
      const wrapper = container.firstChild as HTMLElement
      expect(wrapper.className).toContain('overflow-x-auto')
    })
  })

  describe('Accessibility', () => {
    it('uses a semantic <table> element', () => {
      render(<Table columns={testColumns} data={testData} keyExtractor={keyExtractor} />)
      expect(screen.getByRole('table')).toBeInTheDocument()
    })

    it('headers are rendered as <th> elements', () => {
      render(<Table columns={testColumns} data={testData} keyExtractor={keyExtractor} />)
      const headers = screen.getAllByRole('columnheader')
      headers.forEach((th) => {
        expect(th.tagName).toBe('TH')
      })
    })

    it('data cells are rendered as <td> elements', () => {
      render(<Table columns={testColumns} data={testData} keyExtractor={keyExtractor} />)
      const cells = screen.getAllByRole('cell')
      cells.forEach((td) => {
        expect(td.tagName).toBe('TD')
      })
    })
  })
})
