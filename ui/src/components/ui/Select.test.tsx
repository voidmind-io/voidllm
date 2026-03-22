import { createRef } from 'react'
import type { ComponentProps } from 'react'
import { render, screen, fireEvent } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, it, expect, vi } from 'vitest'
import { Select } from './Select'
import type { SelectOption } from './Select'

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const options: SelectOption[] = [
  { value: 'apple', label: 'Apple' },
  { value: 'banana', label: 'Banana' },
  { value: 'cherry', label: 'Cherry', description: 'A red fruit' },
]

function renderSelect(props: Partial<ComponentProps<typeof Select>> = {}) {
  const defaults = {
    options,
    value: '',
    onChange: vi.fn(),
  }
  return render(<Select {...defaults} {...props} />)
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

describe('Select', () => {
  describe('Rendering', () => {
    it('shows placeholder when no value selected', () => {
      renderSelect({ placeholder: 'Pick one' })
      expect(screen.getByRole('combobox')).toHaveTextContent('Pick one')
    })

    it('shows default placeholder text when placeholder prop omitted', () => {
      renderSelect()
      expect(screen.getByRole('combobox')).toHaveTextContent('Select...')
    })

    it('shows selected option label when value matches', () => {
      renderSelect({ value: 'banana' })
      expect(screen.getByRole('combobox')).toHaveTextContent('Banana')
    })

    it('label renders when provided', () => {
      renderSelect({ label: 'Favourite fruit' })
      expect(screen.getByText('Favourite fruit')).toBeInTheDocument()
    })

    it('label is absent when not provided', () => {
      const { container } = renderSelect()
      expect(container.querySelector('label')).toBeNull()
    })

    it('error message renders with role="alert"', () => {
      renderSelect({ error: 'Selection required' })
      const alert = screen.getByRole('alert')
      expect(alert).toBeInTheDocument()
      expect(alert).toHaveTextContent('Selection required')
    })

    it('no alert when error is not provided', () => {
      renderSelect()
      expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    })

    it('trigger has disabled attribute when disabled=true', () => {
      renderSelect({ disabled: true })
      expect(screen.getByRole('combobox')).toBeDisabled()
    })

    it('option description renders when provided', async () => {
      renderSelect()
      await userEvent.click(screen.getByRole('combobox'))
      expect(screen.getByText('A red fruit')).toBeInTheDocument()
    })
  })

  // ---------------------------------------------------------------------------
  // Open / Close
  // ---------------------------------------------------------------------------

  describe('Open/Close', () => {
    it('click trigger opens dropdown (role="listbox" appears)', async () => {
      renderSelect()
      expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
      await userEvent.click(screen.getByRole('combobox'))
      expect(screen.getByRole('listbox')).toBeInTheDocument()
    })

    it('click trigger again closes dropdown', async () => {
      renderSelect()
      const trigger = screen.getByRole('combobox')
      await userEvent.click(trigger)
      expect(screen.getByRole('listbox')).toBeInTheDocument()
      await userEvent.click(trigger)
      expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
    })

    it('click outside closes dropdown', async () => {
      renderSelect()
      await userEvent.click(screen.getByRole('combobox'))
      expect(screen.getByRole('listbox')).toBeInTheDocument()
      fireEvent.mouseDown(document.body)
      expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
    })

    it('Escape closes dropdown', async () => {
      renderSelect()
      await userEvent.click(screen.getByRole('combobox'))
      expect(screen.getByRole('listbox')).toBeInTheDocument()
      fireEvent.keyDown(document, { key: 'Escape' })
      expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
    })

    it('click option closes dropdown', async () => {
      renderSelect()
      await userEvent.click(screen.getByRole('combobox'))
      await userEvent.click(screen.getByRole('option', { name: 'Apple' }))
      expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
    })
  })

  // ---------------------------------------------------------------------------
  // Selection
  // ---------------------------------------------------------------------------

  describe('Selection', () => {
    it('click option calls onChange with option value', async () => {
      const onChange = vi.fn()
      renderSelect({ onChange })
      await userEvent.click(screen.getByRole('combobox'))
      await userEvent.click(screen.getByRole('option', { name: 'Banana' }))
      expect(onChange).toHaveBeenCalledOnce()
      expect(onChange).toHaveBeenCalledWith('banana')
    })

    it('selected option has aria-selected="true"', async () => {
      renderSelect({ value: 'apple' })
      await userEvent.click(screen.getByRole('combobox'))
      expect(screen.getByRole('option', { name: 'Apple' })).toHaveAttribute('aria-selected', 'true')
    })

    it('non-selected options have aria-selected="false"', async () => {
      renderSelect({ value: 'apple' })
      await userEvent.click(screen.getByRole('combobox'))
      expect(screen.getByRole('option', { name: 'Banana' })).toHaveAttribute('aria-selected', 'false')
      expect(screen.getByRole('option', { name: /Cherry/ })).toHaveAttribute('aria-selected', 'false')
    })
  })

  // ---------------------------------------------------------------------------
  // Searchable
  // ---------------------------------------------------------------------------

  describe('Searchable', () => {
    it('search input appears when searchable=true', async () => {
      renderSelect({ searchable: true })
      await userEvent.click(screen.getByRole('combobox'))
      expect(screen.getByPlaceholderText('Search...')).toBeInTheDocument()
    })

    it('search input NOT present when searchable=false', async () => {
      renderSelect({ searchable: false })
      await userEvent.click(screen.getByRole('combobox'))
      expect(screen.queryByPlaceholderText('Search...')).not.toBeInTheDocument()
    })

    it('typing filters options — only matching options visible', async () => {
      renderSelect({ searchable: true })
      await userEvent.click(screen.getByRole('combobox'))
      await userEvent.type(screen.getByPlaceholderText('Search...'), 'an')
      const visible = screen.getAllByRole('option')
      expect(visible).toHaveLength(1)
      expect(visible[0]).toHaveTextContent('Banana')
    })

    it('no results message when search matches nothing', async () => {
      renderSelect({ searchable: true })
      await userEvent.click(screen.getByRole('combobox'))
      await userEvent.type(screen.getByPlaceholderText('Search...'), 'zzz')
      expect(screen.queryAllByRole('option')).toHaveLength(0)
      expect(screen.getByText('No results')).toBeInTheDocument()
    })

    it('search cleared when dropdown closes and is reopened', async () => {
      renderSelect({ searchable: true })
      const trigger = screen.getByRole('combobox')

      await userEvent.click(trigger)
      await userEvent.type(screen.getByPlaceholderText('Search...'), 'ban')
      expect(screen.getAllByRole('option')).toHaveLength(1)

      // Close via trigger click
      await userEvent.click(trigger)
      expect(screen.queryByRole('listbox')).not.toBeInTheDocument()

      // Reopen — all options should be visible again
      await userEvent.click(trigger)
      expect(screen.getAllByRole('option')).toHaveLength(options.length)
      expect(screen.getByPlaceholderText('Search...')).toHaveValue('')
    })
  })

  // ---------------------------------------------------------------------------
  // Keyboard Navigation
  // ---------------------------------------------------------------------------

  describe('Keyboard Navigation', () => {
    it('ArrowDown on trigger opens dropdown', () => {
      renderSelect()
      fireEvent.keyDown(screen.getByRole('combobox'), { key: 'ArrowDown' })
      expect(screen.getByRole('listbox')).toBeInTheDocument()
    })

    it('Enter on trigger opens dropdown', () => {
      renderSelect()
      fireEvent.keyDown(screen.getByRole('combobox'), { key: 'Enter' })
      expect(screen.getByRole('listbox')).toBeInTheDocument()
    })

    it('Space on trigger opens dropdown', () => {
      renderSelect()
      fireEvent.keyDown(screen.getByRole('combobox'), { key: ' ' })
      expect(screen.getByRole('listbox')).toBeInTheDocument()
    })

    it('ArrowDown moves highlight to next option', async () => {
      renderSelect()
      await userEvent.click(screen.getByRole('combobox'))
      const listbox = screen.getByRole('listbox')
      // Initially highlight index is 0 (Apple). Move down to Banana (index 1).
      fireEvent.keyDown(listbox, { key: 'ArrowDown' })
      const opts = screen.getAllByRole('option')
      // Index 1 (Banana) should now have the highlighted background class
      expect(opts[1].className).toContain('bg-bg-tertiary')
    })

    it('ArrowUp moves highlight to previous option', async () => {
      renderSelect()
      await userEvent.click(screen.getByRole('combobox'))
      const listbox = screen.getByRole('listbox')
      // Move down twice then back up once
      fireEvent.keyDown(listbox, { key: 'ArrowDown' })
      fireEvent.keyDown(listbox, { key: 'ArrowDown' })
      fireEvent.keyDown(listbox, { key: 'ArrowUp' })
      const opts = screen.getAllByRole('option')
      expect(opts[1].className).toContain('bg-bg-tertiary')
    })

    it('Enter selects highlighted option and closes dropdown', async () => {
      const onChange = vi.fn()
      renderSelect({ onChange })
      await userEvent.click(screen.getByRole('combobox'))
      const listbox = screen.getByRole('listbox')
      // Highlight index starts at 0 (Apple)
      fireEvent.keyDown(listbox, { key: 'Enter' })
      expect(onChange).toHaveBeenCalledWith('apple')
      expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
    })

    it('Home highlights first option', async () => {
      renderSelect()
      await userEvent.click(screen.getByRole('combobox'))
      const listbox = screen.getByRole('listbox')
      // Move to last, then Home back to first
      fireEvent.keyDown(listbox, { key: 'End' })
      fireEvent.keyDown(listbox, { key: 'Home' })
      const opts = screen.getAllByRole('option')
      expect(opts[0].className).toContain('bg-bg-tertiary')
    })

    it('End highlights last option', async () => {
      renderSelect()
      await userEvent.click(screen.getByRole('combobox'))
      const listbox = screen.getByRole('listbox')
      fireEvent.keyDown(listbox, { key: 'End' })
      const opts = screen.getAllByRole('option')
      expect(opts[opts.length - 1].className).toContain('bg-bg-tertiary')
    })

    it('ArrowDown does not move highlight past the last option', async () => {
      renderSelect()
      await userEvent.click(screen.getByRole('combobox'))
      const listbox = screen.getByRole('listbox')
      // Press ArrowDown many times beyond the list length
      for (let i = 0; i < 10; i++) {
        fireEvent.keyDown(listbox, { key: 'ArrowDown' })
      }
      const opts = screen.getAllByRole('option')
      expect(opts[opts.length - 1].className).toContain('bg-bg-tertiary')
    })

    it('ArrowUp does not move highlight before the first option', async () => {
      renderSelect()
      await userEvent.click(screen.getByRole('combobox'))
      const listbox = screen.getByRole('listbox')
      fireEvent.keyDown(listbox, { key: 'ArrowUp' })
      fireEvent.keyDown(listbox, { key: 'ArrowUp' })
      const opts = screen.getAllByRole('option')
      expect(opts[0].className).toContain('bg-bg-tertiary')
    })

    it('disabled trigger ignores keyboard open keys', () => {
      renderSelect({ disabled: true })
      fireEvent.keyDown(screen.getByRole('combobox'), { key: 'ArrowDown' })
      expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
    })
  })

  // ---------------------------------------------------------------------------
  // Accessibility
  // ---------------------------------------------------------------------------

  describe('Accessibility', () => {
    it('trigger has aria-haspopup="listbox"', () => {
      renderSelect()
      expect(screen.getByRole('combobox')).toHaveAttribute('aria-haspopup', 'listbox')
    })

    it('aria-expanded="false" when closed', () => {
      renderSelect()
      expect(screen.getByRole('combobox')).toHaveAttribute('aria-expanded', 'false')
    })

    it('aria-expanded="true" when open', async () => {
      renderSelect()
      await userEvent.click(screen.getByRole('combobox'))
      expect(screen.getByRole('combobox')).toHaveAttribute('aria-expanded', 'true')
    })

    it('dropdown has role="listbox"', async () => {
      renderSelect()
      await userEvent.click(screen.getByRole('combobox'))
      expect(screen.getByRole('listbox')).toBeInTheDocument()
    })

    it('each option has role="option"', async () => {
      renderSelect()
      await userEvent.click(screen.getByRole('combobox'))
      expect(screen.getAllByRole('option')).toHaveLength(options.length)
    })

    it('trigger has aria-invalid="true" when error is set', () => {
      renderSelect({ error: 'Required' })
      expect(screen.getByRole('combobox')).toHaveAttribute('aria-invalid', 'true')
    })

    it('aria-invalid absent when no error', () => {
      renderSelect()
      expect(screen.getByRole('combobox')).not.toHaveAttribute('aria-invalid')
    })

    it('trigger aria-controls points to listbox id', async () => {
      renderSelect()
      const trigger = screen.getByRole('combobox')
      const controlsId = trigger.getAttribute('aria-controls')
      expect(controlsId).toBeTruthy()
      await userEvent.click(trigger)
      expect(document.getElementById(controlsId!)).toHaveAttribute('role', 'listbox')
    })

    it('ref forwarding — ref.current is the trigger button', () => {
      const ref = createRef<HTMLButtonElement>()
      render(<Select options={options} value="" onChange={vi.fn()} ref={ref} />)
      expect(ref.current).not.toBeNull()
      expect(ref.current?.tagName).toBe('BUTTON')
    })
  })

  // ---------------------------------------------------------------------------
  // Focus Management (Fix 3, Fix 4)
  // ---------------------------------------------------------------------------

  describe('Focus Management', () => {
    it('focus returns to trigger after option selection via click', async () => {
      renderSelect()
      const trigger = screen.getByRole('combobox')
      await userEvent.click(trigger)
      await userEvent.click(screen.getByRole('option', { name: 'Apple' }))
      expect(document.activeElement).toBe(trigger)
    })

    it('focus returns to trigger after Enter key selects option', async () => {
      renderSelect()
      const trigger = screen.getByRole('combobox')
      await userEvent.click(trigger)
      const listbox = screen.getByRole('listbox')
      fireEvent.keyDown(listbox, { key: 'Enter' })
      expect(document.activeElement).toBe(trigger)
    })

    it('focus returns to trigger after Escape closes dropdown', async () => {
      renderSelect()
      const trigger = screen.getByRole('combobox')
      await userEvent.click(trigger)
      fireEvent.keyDown(document, { key: 'Escape' })
      expect(document.activeElement).toBe(trigger)
    })

    it('Enter/Space on trigger opens dropdown without double-toggling', () => {
      renderSelect()
      const trigger = screen.getByRole('combobox')
      // Simulate keyboard activation: keyDown fires first (opens), then the
      // browser synthesises a click event with detail === 0. The onClick guard
      // must ignore that synthetic click so the dropdown stays open.
      fireEvent.keyDown(trigger, { key: 'Enter' })
      expect(screen.getByRole('listbox')).toBeInTheDocument()
      // Simulate the browser's synthetic click (detail === 0)
      fireEvent.click(trigger, { detail: 0 })
      // Dropdown must still be open
      expect(screen.getByRole('listbox')).toBeInTheDocument()
    })
  })

  // ---------------------------------------------------------------------------
  // Accessibility — aria-activedescendant (Fix 6)
  // ---------------------------------------------------------------------------

  describe('aria-activedescendant', () => {
    it('trigger has aria-activedescendant pointing to first option when opened', async () => {
      renderSelect()
      const trigger = screen.getByRole('combobox')
      await userEvent.click(trigger)
      const activeId = trigger.getAttribute('aria-activedescendant')
      expect(activeId).toBeTruthy()
      const activeEl = document.getElementById(activeId!)
      expect(activeEl).toHaveAttribute('role', 'option')
      expect(activeEl).toHaveTextContent('Apple')
    })

    it('aria-activedescendant absent when dropdown is closed', () => {
      renderSelect()
      expect(screen.getByRole('combobox')).not.toHaveAttribute(
        'aria-activedescendant',
      )
    })

    it('aria-activedescendant updates when highlight changes via ArrowDown', async () => {
      renderSelect()
      const trigger = screen.getByRole('combobox')
      await userEvent.click(trigger)
      const listbox = screen.getByRole('listbox')
      fireEvent.keyDown(listbox, { key: 'ArrowDown' })
      const activeId = trigger.getAttribute('aria-activedescendant')
      expect(activeId).toBeTruthy()
      const activeEl = document.getElementById(activeId!)
      expect(activeEl).toHaveTextContent('Banana')
    })
  })

  // ---------------------------------------------------------------------------
  // className
  // ---------------------------------------------------------------------------

  describe('className', () => {
    it('additional className merged on wrapper element', () => {
      const { container } = renderSelect({ className: 'my-custom-class' })
      expect(container.firstElementChild?.className).toContain('my-custom-class')
    })
  })
})
