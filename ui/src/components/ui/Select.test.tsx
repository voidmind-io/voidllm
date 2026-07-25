import { createRef } from 'react'
import type { ComponentProps } from 'react'
import { render, screen, fireEvent, act } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, it, expect, vi, afterEach } from 'vitest'
import { Select } from './Select'
import type { SelectOption } from './Select'
import { Table } from './Table'
import type { Column } from './Table'
import { Dialog } from './Dialog'

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

// jsdom's getBoundingClientRect always returns an all-zero rect, so any test
// that asserts a real inline `left`/`top`/`bottom`/`width` value on the
// portalled menu must stub the trigger's (or menu's) rect explicitly —
// otherwise the assertion would pass vacuously against a rect that is all
// zeros regardless of what the positioning code actually does.
function stubRect(
  el: Element,
  rect: {
    left: number
    top: number
    right: number
    bottom: number
    width: number
    height: number
  },
) {
  vi.spyOn(el, 'getBoundingClientRect').mockReturnValue({
    ...rect,
    x: rect.left,
    y: rect.top,
    toJSON: () => rect,
  })
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

    it('ArrowDown and Enter still work while focus is in the search input', async () => {
      // The searchable path was always reachable — focus genuinely sits in
      // the search input (auto-focused on open), and these keydowns bubble
      // from the input up to the listbox's own onKeyDown. This is the one
      // path the trigger-keyboard fix did not need to touch; kept covered
      // here to prove it wasn't disturbed.
      const onChange = vi.fn()
      renderSelect({ searchable: true, onChange })
      await userEvent.click(screen.getByRole('combobox'))
      const searchInput = screen.getByPlaceholderText('Search...')

      fireEvent.keyDown(searchInput, { key: 'ArrowDown' })
      const opts = screen.getAllByRole('option')
      expect(opts[1].className).toContain('bg-bg-tertiary')

      fireEvent.keyDown(searchInput, { key: 'Enter' })
      expect(onChange).toHaveBeenCalledWith('banana')
      expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
    })

    it('Space in the search input types a space instead of selecting the highlighted option', async () => {
      // This is the reason Space is handled on the trigger only
      // (handleTriggerKeyDown) and deliberately left out of the shared
      // handleNavigationKeyDown that the search input's keydowns bubble
      // into — here Space must stay ordinary text entry.
      const onChange = vi.fn()
      renderSelect({ searchable: true, onChange })
      await userEvent.click(screen.getByRole('combobox'))
      const searchInput = screen.getByPlaceholderText('Search...')

      await userEvent.type(searchInput, 'a a')

      expect(searchInput).toHaveValue('a a')
      expect(onChange).not.toHaveBeenCalled()
      expect(screen.getByRole('listbox')).toBeInTheDocument()
    })
  })

  // ---------------------------------------------------------------------------
  // Keyboard Navigation
  //
  // Every keydown below is dispatched to the trigger (getByRole('combobox')),
  // never to the listbox. When `searchable` is false the menu is a portalled
  // sibling of the trigger, not a descendant — a real user has no way to
  // move focus onto it, so a test that fires keys at getByRole('listbox')
  // would pass whether or not the trigger actually forwards those keys to
  // navigation. Open/close and highlight movement while the menu is open
  // live on handleTriggerKeyDown, which delegates to the shared
  // handleNavigationKeyDown; the listbox's own onKeyDown only matters for
  // the searchable path, where focus genuinely sits in the search input
  // inside the menu — that path is covered separately in Searchable below.
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

    it('ArrowDown on the trigger moves highlight to next option', async () => {
      renderSelect()
      const trigger = screen.getByRole('combobox')
      await userEvent.click(trigger)
      // Initially highlight index is 0 (Apple). Move down to Banana (index 1).
      fireEvent.keyDown(trigger, { key: 'ArrowDown' })
      const opts = screen.getAllByRole('option')
      // Index 1 (Banana) should now have the highlighted background class
      expect(opts[1].className).toContain('bg-bg-tertiary')
    })

    it('ArrowUp on the trigger moves highlight to previous option', async () => {
      renderSelect()
      const trigger = screen.getByRole('combobox')
      await userEvent.click(trigger)
      // Move down twice then back up once
      fireEvent.keyDown(trigger, { key: 'ArrowDown' })
      fireEvent.keyDown(trigger, { key: 'ArrowDown' })
      fireEvent.keyDown(trigger, { key: 'ArrowUp' })
      const opts = screen.getAllByRole('option')
      expect(opts[1].className).toContain('bg-bg-tertiary')
    })

    it('Enter on the trigger selects highlighted option and closes dropdown', async () => {
      const onChange = vi.fn()
      renderSelect({ onChange })
      const trigger = screen.getByRole('combobox')
      await userEvent.click(trigger)
      // Highlight index starts at 0 (Apple)
      fireEvent.keyDown(trigger, { key: 'Enter' })
      expect(onChange).toHaveBeenCalledWith('apple')
      expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
    })

    it('Space on the trigger selects the highlighted option when the menu is open', async () => {
      // Space stays trigger-only (handleTriggerKeyDown), distinct from
      // handleNavigationKeyDown which every other case here goes through —
      // see the Searchable describe for the contrasting case where Space
      // must NOT select because it is ordinary typing in the search input.
      const onChange = vi.fn()
      renderSelect({ onChange })
      const trigger = screen.getByRole('combobox')
      await userEvent.click(trigger)
      fireEvent.keyDown(trigger, { key: 'ArrowDown' })
      fireEvent.keyDown(trigger, { key: ' ' })
      expect(onChange).toHaveBeenCalledWith('banana')
      expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
      expect(document.activeElement).toBe(trigger)
    })

    it('Home on the trigger highlights first option', async () => {
      renderSelect()
      const trigger = screen.getByRole('combobox')
      await userEvent.click(trigger)
      // Move to last, then Home back to first
      fireEvent.keyDown(trigger, { key: 'End' })
      fireEvent.keyDown(trigger, { key: 'Home' })
      const opts = screen.getAllByRole('option')
      expect(opts[0].className).toContain('bg-bg-tertiary')
    })

    it('End on the trigger highlights last option', async () => {
      renderSelect()
      const trigger = screen.getByRole('combobox')
      await userEvent.click(trigger)
      fireEvent.keyDown(trigger, { key: 'End' })
      const opts = screen.getAllByRole('option')
      expect(opts[opts.length - 1].className).toContain('bg-bg-tertiary')
    })

    it('ArrowDown on the trigger does not move highlight past the last option', async () => {
      renderSelect()
      const trigger = screen.getByRole('combobox')
      await userEvent.click(trigger)
      // Press ArrowDown many times beyond the list length
      for (let i = 0; i < 10; i++) {
        fireEvent.keyDown(trigger, { key: 'ArrowDown' })
      }
      const opts = screen.getAllByRole('option')
      expect(opts[opts.length - 1].className).toContain('bg-bg-tertiary')
    })

    it('ArrowUp on the trigger does not move highlight before the first option', async () => {
      renderSelect()
      const trigger = screen.getByRole('combobox')
      await userEvent.click(trigger)
      fireEvent.keyDown(trigger, { key: 'ArrowUp' })
      fireEvent.keyDown(trigger, { key: 'ArrowUp' })
      const opts = screen.getAllByRole('option')
      expect(opts[0].className).toContain('bg-bg-tertiary')
    })

    it('opens and navigates entirely from the keyboard, without ever clicking the trigger', () => {
      // ArrowDown on the closed trigger both opens the menu and is the same
      // key a user keeps pressing to move the highlight afterwards — this
      // exercises the whole open-then-navigate flow without a single mouse
      // interaction anywhere in the test.
      renderSelect()
      const trigger = screen.getByRole('combobox')
      fireEvent.keyDown(trigger, { key: 'ArrowDown' })
      expect(screen.getByRole('listbox')).toBeInTheDocument()
      fireEvent.keyDown(trigger, { key: 'ArrowDown' })
      fireEvent.keyDown(trigger, { key: 'ArrowDown' })
      const opts = screen.getAllByRole('option')
      expect(opts[2].className).toContain('bg-bg-tertiary')
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
      // Enter is dispatched to the trigger — see the Keyboard Navigation
      // describe above for why the listbox is never a legitimate keydown
      // target for the non-searchable path.
      fireEvent.keyDown(trigger, { key: 'Enter' })
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
    it('trigger has aria-activedescendant pointing to first option when opened with no value selected', async () => {
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

    it('aria-activedescendant updates when highlight changes via ArrowDown on the trigger', async () => {
      renderSelect()
      const trigger = screen.getByRole('combobox')
      await userEvent.click(trigger)
      // Dispatched to the trigger, not the listbox — see the Keyboard
      // Navigation describe above for why.
      fireEvent.keyDown(trigger, { key: 'ArrowDown' })
      const activeId = trigger.getAttribute('aria-activedescendant')
      expect(activeId).toBeTruthy()
      const activeEl = document.getElementById(activeId!)
      expect(activeEl).toHaveTextContent('Banana')
    })

    it('tracks the highlight on the trigger through a fully keyboard-driven open-and-navigate interaction', () => {
      renderSelect()
      const trigger = screen.getByRole('combobox')
      const activeOption = () =>
        document.getElementById(trigger.getAttribute('aria-activedescendant')!)

      // Open via ArrowDown on the closed trigger — no mouse involved at any
      // point in this test. Starts on Apple (index 0).
      fireEvent.keyDown(trigger, { key: 'ArrowDown' })
      expect(activeOption()).toHaveTextContent('Apple')

      fireEvent.keyDown(trigger, { key: 'ArrowDown' })
      fireEvent.keyDown(trigger, { key: 'ArrowDown' })
      expect(activeOption()).toHaveTextContent('Cherry')

      fireEvent.keyDown(trigger, { key: 'ArrowUp' })
      expect(activeOption()).toHaveTextContent('Banana')
    })
  })

  // ---------------------------------------------------------------------------
  // Initial highlight seeding (getInitialHighlightIndex) — opening the menu
  // seeds the highlight on the currently selected option instead of always
  // starting at index 0, on both paths that can open the menu: a mouse click
  // on the trigger and a keyboard open key (ArrowDown/Enter/Space) on the
  // closed trigger. Falls back to the first option when `value` is empty or
  // does not match any option.
  // ---------------------------------------------------------------------------

  describe('Initial highlight seeding', () => {
    it('click-opening seeds the highlight on the option matching the current value', async () => {
      renderSelect({ value: 'banana' })
      const trigger = screen.getByRole('combobox')
      await userEvent.click(trigger)
      const activeId = trigger.getAttribute('aria-activedescendant')
      expect(document.getElementById(activeId!)).toHaveTextContent('Banana')
    })

    it('keyboard-opening (ArrowDown on the closed trigger) seeds the highlight on the option matching the current value', () => {
      renderSelect({ value: 'cherry' })
      const trigger = screen.getByRole('combobox')
      fireEvent.keyDown(trigger, { key: 'ArrowDown' })
      const activeId = trigger.getAttribute('aria-activedescendant')
      expect(document.getElementById(activeId!)).toHaveTextContent('Cherry')
    })

    it('falls back to the first option when value does not match any option (click-open)', async () => {
      renderSelect({ value: 'durian' })
      const trigger = screen.getByRole('combobox')
      await userEvent.click(trigger)
      const activeId = trigger.getAttribute('aria-activedescendant')
      expect(document.getElementById(activeId!)).toHaveTextContent('Apple')
    })

    it('falls back to the first option when value is empty (keyboard-open)', () => {
      renderSelect({ value: '' })
      const trigger = screen.getByRole('combobox')
      fireEvent.keyDown(trigger, { key: 'Enter' })
      const activeId = trigger.getAttribute('aria-activedescendant')
      expect(document.getElementById(activeId!)).toHaveTextContent('Apple')
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

  // ---------------------------------------------------------------------------
  // Portal (menu renders into document.body via ReactDOM.createPortal —
  // GitHub #183). Escape, focus return after selection, arrow-key navigation
  // and the search box are already exercised through role queries in the
  // suites above (Open/Close, Focus Management, Keyboard Navigation,
  // Searchable) and behave identically now that the menu is portalled —
  // not duplicated here.
  // ---------------------------------------------------------------------------

  describe('Portal', () => {
    it('menu is rendered into document.body, not inside the component container', async () => {
      // Same assertion shape as Dialog.test.tsx's "Portal" describe
      // (lines ~274-282): absent from the local render container, present
      // under document.body.
      const { container } = renderSelect()
      await userEvent.click(screen.getByRole('combobox'))
      expect(container.querySelector('[role="listbox"]')).toBeNull()
      expect(document.body.querySelector('[role="listbox"]')).not.toBeNull()
    })

    it('mousedown inside the portalled menu does not close it', async () => {
      // The blocker (#183): a naive outside-click check only looks at
      // containerRef, which does not contain the portalled menu (it lives
      // under document.body, not inside the component's own wrapper div).
      // Without menuRef in the check, this mousedown alone closes the menu.
      renderSelect()
      await userEvent.click(screen.getByRole('combobox'))
      const listbox = screen.getByRole('listbox')
      fireEvent.mouseDown(listbox)
      expect(screen.getByRole('listbox')).toBeInTheDocument()
    })

    it('clicking an option inside the portalled menu calls onChange with the right value and closes the menu', async () => {
      // The other half of the blocker: a click is mousedown -> mouseup ->
      // click. If the mousedown half above closed the menu first, the
      // option node would already be unmounted by the time the click fires,
      // and onChange would never be called.
      const onChange = vi.fn()
      renderSelect({ onChange })
      await userEvent.click(screen.getByRole('combobox'))
      await userEvent.click(screen.getByRole('option', { name: /Cherry/ }))
      expect(onChange).toHaveBeenCalledOnce()
      expect(onChange).toHaveBeenCalledWith('cherry')
      expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
    })
  })

  // ---------------------------------------------------------------------------
  // Positioning (the portalled menu is `position: fixed`, placed from the
  // trigger's getBoundingClientRect, and re-measured on resize/scroll —
  // GitHub #183)
  // ---------------------------------------------------------------------------

  describe('Positioning', () => {
    afterEach(() => {
      vi.restoreAllMocks()
    })

    it('menu left/width are taken from the trigger rect', async () => {
      renderSelect()
      const trigger = screen.getByRole('combobox')
      stubRect(trigger, { left: 40, top: 100, right: 240, bottom: 130, width: 200, height: 30 })
      await userEvent.click(trigger)
      const listbox = screen.getByRole('listbox')
      expect(listbox.style.left).toBe('40px')
      expect(listbox.style.width).toBe('200px')
    })

    it('menu position updates when the window resizes', async () => {
      renderSelect()
      const trigger = screen.getByRole('combobox')
      stubRect(trigger, { left: 40, top: 100, right: 240, bottom: 130, width: 200, height: 30 })
      await userEvent.click(trigger)
      const listbox = screen.getByRole('listbox')
      expect(listbox.style.left).toBe('40px')

      stubRect(trigger, { left: 90, top: 100, right: 290, bottom: 130, width: 200, height: 30 })
      fireEvent.resize(window)
      expect(listbox.style.left).toBe('90px')
    })

    it('menu position updates when a nested scrollable ancestor (not window) scrolls', async () => {
      // The tracking effect registers its scroll listener on window with
      // useCapture=true specifically so it also catches scrolling from an
      // ancestor scroll container (Table's overflow-x-auto, Dialog's
      // overflow-y-auto). Native `scroll` events do not bubble, so firing
      // scroll on this nested div (rather than window itself) only reaches
      // a *capturing* window listener — a bubble-phase listener would never
      // see it. This is what actually pins the capture-phase requirement
      // down, as opposed to firing scroll on window directly.
      const onChange = vi.fn()
      render(
        <div style={{ overflowY: 'auto' }} data-testid="scroll-ancestor">
          <Select options={options} value="" onChange={onChange} />
        </div>,
      )
      const scrollAncestor = screen.getByTestId('scroll-ancestor')
      const trigger = screen.getByRole('combobox')
      stubRect(trigger, { left: 40, top: 100, right: 240, bottom: 130, width: 200, height: 30 })
      await userEvent.click(trigger)
      const listbox = screen.getByRole('listbox')
      expect(listbox.style.left).toBe('40px')

      stubRect(trigger, { left: 150, top: 60, right: 350, bottom: 90, width: 200, height: 30 })
      fireEvent.scroll(scrollAncestor)
      expect(listbox.style.left).toBe('150px')
    })

    it('closes the dropdown when the trigger scrolls above the viewport', async () => {
      renderSelect()
      const trigger = screen.getByRole('combobox')
      stubRect(trigger, { left: 40, top: 100, right: 240, bottom: 130, width: 200, height: 30 })
      await userEvent.click(trigger)
      expect(screen.getByRole('listbox')).toBeInTheDocument()

      stubRect(trigger, { left: 40, top: -300, right: 240, bottom: -270, width: 200, height: 30 })
      fireEvent.scroll(window)
      expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
    })

    it('closes the dropdown when the trigger scrolls below the viewport', async () => {
      renderSelect()
      const trigger = screen.getByRole('combobox')
      stubRect(trigger, { left: 40, top: 100, right: 240, bottom: 130, width: 200, height: 30 })
      await userEvent.click(trigger)
      expect(screen.getByRole('listbox')).toBeInTheDocument()

      stubRect(trigger, { left: 40, top: 2000, right: 240, bottom: 2030, width: 200, height: 30 })
      fireEvent.scroll(window)
      expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
    })

    it('places the menu above the trigger when there is not enough room below but room above', async () => {
      // window.innerHeight is stubbed so the trigger sits close to the
      // bottom edge: spaceBelow (innerHeight - rect.bottom = 400 - 380 = 20)
      // is smaller than ESTIMATED_MENU_HEIGHT (used for the initial
      // synchronous placement, before the menu has painted), and rect.top
      // (350) leaves enough room above to fit it.
      vi.spyOn(window, 'innerHeight', 'get').mockReturnValue(400)
      renderSelect()
      const trigger = screen.getByRole('combobox')
      stubRect(trigger, { left: 40, top: 350, right: 240, bottom: 380, width: 200, height: 30 })
      await userEvent.click(trigger)
      const listbox = screen.getByRole('listbox')
      // `bottom` (not `top`) is the style that distinguishes "rendered
      // above": bottom = innerHeight(400) - rect.top(350) + 4 = 54.
      expect(listbox.style.bottom).toBe('54px')
      expect(listbox.style.top).toBe('')
    })

    it('places the menu below the trigger when there is enough room', async () => {
      renderSelect()
      const trigger = screen.getByRole('combobox')
      stubRect(trigger, { left: 40, top: 100, right: 240, bottom: 130, width: 200, height: 30 })
      await userEvent.click(trigger)
      const listbox = screen.getByRole('listbox')
      // top = rect.bottom(130) + 4 = 134.
      expect(listbox.style.top).toBe('134px')
      expect(listbox.style.bottom).toBe('')
    })

    // -------------------------------------------------------------------
    // maxHeight clamp (the menu's height must never exceed the space
    // actually available on whichever side it renders — previously it
    // kept the fixed max-h-60 class (240px) regardless, which could
    // overflow a viewport a Dialog's body-scroll-lock made unreachable).
    //
    // TRIGGER_GAP (4), VIEWPORT_MARGIN (8) and ESTIMATED_MENU_HEIGHT (240)
    // below mirror the module-private constants of the same names in
    // Select.tsx (not exported). Expected values are derived from the
    // stubbed rect and these constants rather than hardcoded, so a future
    // change to the margins or the clamp cap fails these tests loudly
    // instead of leaving them silently out of sync. The inline maxHeight
    // style tracks the measured space available at the anchor's final
    // (shifted) position, capped at ESTIMATED_MENU_HEIGHT (matching the
    // max-h-60 class) when there is far more room than that. There is no
    // floor: the shift stage guarantees the anchor is already pulled back
    // inside the viewport, so the space it measures there is always >= 0
    // — see the "keeps the menu fully inside the viewport" test below for
    // the degenerate case where that space is legitimately 0.
    // -------------------------------------------------------------------

    it('clamps the below-placed menu maxHeight to the measured space below, not an unbounded value', async () => {
      const TRIGGER_GAP = 4
      const VIEWPORT_MARGIN = 8
      const ESTIMATED_MENU_HEIGHT = 240
      // innerHeight is chosen so spaceBelow lands strictly below the cap
      // — proving the height tracks the measured space rather than just
      // hitting the cap.
      const innerHeight = 300
      const rect = { left: 40, top: 100, right: 240, bottom: 130, width: 200, height: 30 }
      vi.spyOn(window, 'innerHeight', 'get').mockReturnValue(innerHeight)
      renderSelect()
      const trigger = screen.getByRole('combobox')
      stubRect(trigger, rect)
      await userEvent.click(trigger)
      const listbox = screen.getByRole('listbox')

      const spaceBelow = innerHeight - rect.bottom - TRIGGER_GAP - VIEWPORT_MARGIN
      // Sanity check on the fixture: this only proves tracking (not just
      // the cap) if spaceBelow is strictly positive and below the cap.
      expect(spaceBelow).toBeGreaterThan(0)
      expect(spaceBelow).toBeLessThan(ESTIMATED_MENU_HEIGHT)
      expect(listbox.style.top).toBe(`${rect.bottom + TRIGGER_GAP}px`)
      expect(listbox.style.maxHeight).toBe(
        `${Math.min(spaceBelow, ESTIMATED_MENU_HEIGHT)}px`,
      )
    })

    it('caps the maxHeight at ESTIMATED_MENU_HEIGHT when the viewport offers far more room than that', async () => {
      // The inline maxHeight style would otherwise beat the max-h-60
      // class in the cascade and let the menu grow past 240px on a tall
      // viewport — this is what pins the cap back in place after the
      // move from a class-only cap to an inline style.
      const TRIGGER_GAP = 4
      const VIEWPORT_MARGIN = 8
      const ESTIMATED_MENU_HEIGHT = 240
      const innerHeight = 1000
      const rect = { left: 40, top: 100, right: 240, bottom: 130, width: 200, height: 30 }
      vi.spyOn(window, 'innerHeight', 'get').mockReturnValue(innerHeight)
      renderSelect()
      const trigger = screen.getByRole('combobox')
      stubRect(trigger, rect)
      await userEvent.click(trigger)
      const listbox = screen.getByRole('listbox')

      const spaceBelow = innerHeight - rect.bottom - TRIGGER_GAP - VIEWPORT_MARGIN
      // Sanity check on the fixture: this only proves the cap survived if
      // the raw available space genuinely exceeds it.
      expect(spaceBelow).toBeGreaterThan(ESTIMATED_MENU_HEIGHT)
      expect(listbox.style.top).toBe(`${rect.bottom + TRIGGER_GAP}px`)
      expect(listbox.style.maxHeight).toBe(
        `${Math.min(spaceBelow, ESTIMATED_MENU_HEIGHT)}px`,
      )
    })

    it('clamps the above-flipped menu maxHeight to the measured space above', async () => {
      const TRIGGER_GAP = 4
      const VIEWPORT_MARGIN = 8
      const ESTIMATED_MENU_HEIGHT = 240
      // innerHeight/rect are chosen so spaceBelow doesn't fit the
      // pre-paint estimate (triggering the flip to "above") while
      // spaceAbove lands strictly below the cap.
      const innerHeight = 400
      const rect = { left: 40, top: 200, right: 240, bottom: 230, width: 200, height: 30 }
      vi.spyOn(window, 'innerHeight', 'get').mockReturnValue(innerHeight)
      renderSelect()
      const trigger = screen.getByRole('combobox')
      stubRect(trigger, rect)
      await userEvent.click(trigger)
      const listbox = screen.getByRole('listbox')

      const spaceBelow = innerHeight - rect.bottom - TRIGGER_GAP - VIEWPORT_MARGIN
      const spaceAbove = rect.top - TRIGGER_GAP - VIEWPORT_MARGIN
      // Sanity check on the fixture: the flip only happens because below
      // doesn't fit the pre-paint estimate and above has more room; the
      // test only proves tracking if spaceAbove is strictly positive and
      // below the cap.
      expect(spaceBelow).toBeLessThan(ESTIMATED_MENU_HEIGHT)
      expect(spaceAbove).toBeGreaterThan(spaceBelow)
      expect(spaceAbove).toBeGreaterThan(0)
      expect(spaceAbove).toBeLessThan(ESTIMATED_MENU_HEIGHT)
      expect(listbox.style.top).toBe('')
      expect(listbox.style.bottom).toBe(`${innerHeight - rect.top + TRIGGER_GAP}px`)
      expect(listbox.style.maxHeight).toBe(
        `${Math.min(spaceAbove, ESTIMATED_MENU_HEIGHT)}px`,
      )
    })

    it('flips above and clamps to that side when the menu fits on neither side', async () => {
      // Both sides are smaller than the pre-paint height estimate, so
      // under the previous logic ("above" was only chosen when space
      // fit above that estimate) this would have fallen through to
      // "below" with no clamp at all — a fixed max-h-60 (240px) menu
      // inside a short viewport, overflowing it. innerHeight/rect are
      // chosen so spaceAbove (the side picked) still lands strictly
      // below the cap, proving it tracks rather than just snapping to
      // ESTIMATED_MENU_HEIGHT.
      const TRIGGER_GAP = 4
      const VIEWPORT_MARGIN = 8
      const ESTIMATED_MENU_HEIGHT = 240
      const innerHeight = 300
      const rect = { left: 40, top: 150, right: 240, bottom: 170, width: 200, height: 20 }
      vi.spyOn(window, 'innerHeight', 'get').mockReturnValue(innerHeight)
      renderSelect()
      const trigger = screen.getByRole('combobox')
      stubRect(trigger, rect)
      await userEvent.click(trigger)
      const listbox = screen.getByRole('listbox')

      const spaceBelow = innerHeight - rect.bottom - TRIGGER_GAP - VIEWPORT_MARGIN
      const spaceAbove = rect.top - TRIGGER_GAP - VIEWPORT_MARGIN
      // Sanity check on the fixture itself: this test only pins down the
      // "neither fits, pick the bigger side" branch if neither side fits
      // the pre-paint estimate and above truly has more room than below.
      expect(spaceBelow).toBeLessThan(ESTIMATED_MENU_HEIGHT)
      expect(spaceAbove).toBeLessThan(ESTIMATED_MENU_HEIGHT)
      expect(spaceAbove).toBeGreaterThan(spaceBelow)
      expect(spaceAbove).toBeGreaterThan(0)

      // Flipped above (bottom set, top unset) ...
      expect(listbox.style.top).toBe('')
      expect(listbox.style.bottom).toBe(`${innerHeight - rect.top + TRIGGER_GAP}px`)
      // ...and clamped to the larger (above) side's space, not left
      // unbounded at the old fixed 240px.
      expect(listbox.style.maxHeight).toBe(
        `${Math.min(spaceAbove, ESTIMATED_MENU_HEIGHT)}px`,
      )
    })

    it('keeps the menu fully inside the viewport when the available space on both sides is at or below zero', async () => {
      // A very short viewport (or a trigger scrolled partly toward an
      // edge but not far enough to count as fully out of view — see the
      // "closes the dropdown when the trigger scrolls above/below the
      // viewport" tests above for the fully-out-of-view case) can leave
      // zero or negative space on both sides. There is no MIN_MENU_HEIGHT
      // floor any more — that constant is gone, and it was exactly what
      // used to push the menu's far edge out of the window on a viewport
      // like this one. What the pipeline guarantees instead is that the
      // menu box — anchor and height together — never renders partly
      // off-screen, even when (as asserted below) the honest answer for
      // how much height is left is 0.
      const TRIGGER_GAP = 4
      const VIEWPORT_MARGIN = 8
      const ESTIMATED_MENU_HEIGHT = 240
      const innerHeight = 20
      const rect = { left: 0, top: 10, right: 100, bottom: 15, width: 100, height: 5 }
      vi.spyOn(window, 'innerHeight', 'get').mockReturnValue(innerHeight)
      renderSelect()
      const trigger = screen.getByRole('combobox')
      stubRect(trigger, rect)
      await userEvent.click(trigger)
      const listbox = screen.getByRole('listbox')

      const spaceBelow = innerHeight - rect.bottom - TRIGGER_GAP - VIEWPORT_MARGIN
      const spaceAbove = rect.top - TRIGGER_GAP - VIEWPORT_MARGIN
      // Sanity check on the fixture: both sides must be at or below zero
      // for this to exercise the degenerate case.
      expect(spaceBelow).toBeLessThanOrEqual(0)
      expect(spaceAbove).toBeLessThanOrEqual(0)

      // Whichever side has (marginally) more room is the one the
      // component picks — here that is "above", since spaceAbove(-2) >
      // spaceBelow(-7).
      expect(listbox.style.top).toBe('')

      // Shift: the raw anchor (innerHeight - rect.top + TRIGGER_GAP = 14)
      // falls outside [VIEWPORT_MARGIN, innerHeight - VIEWPORT_MARGIN] on
      // this 20px-tall viewport, so it gets clamped down to the band's
      // upper bound (12) instead of being used as-is.
      const expectedBottom = Math.min(
        Math.max(innerHeight - rect.top + TRIGGER_GAP, VIEWPORT_MARGIN),
        innerHeight - VIEWPORT_MARGIN,
      )
      expect(expectedBottom).toBeGreaterThanOrEqual(VIEWPORT_MARGIN)
      expect(expectedBottom).toBeLessThanOrEqual(innerHeight - VIEWPORT_MARGIN)
      expect(listbox.style.bottom).toBe(`${expectedBottom}px`)

      // Size: maxHeight is derived from the space between the *shifted*
      // anchor and the opposite margin. On this fixture that space is
      // exactly 0 (20 - 8 - 12) — there genuinely is no room left once
      // the anchor has been pulled back on-screen, so 0 is the correct,
      // intended height here, not a defect. This is the same outcome
      // floating-ui's size middleware produces once shift has already
      // consumed all the slack; asserting anything else would mean
      // reintroducing an invented floor.
      const expectedMaxHeight = Math.min(
        innerHeight - VIEWPORT_MARGIN - expectedBottom,
        ESTIMATED_MENU_HEIGHT,
      )
      expect(expectedMaxHeight).toBe(0)
      expect(listbox.style.maxHeight).toBe(`${expectedMaxHeight}px`)

      // The whole box — top edge and bottom edge, both measured from the
      // top of the viewport — must stay inside [VIEWPORT_MARGIN,
      // innerHeight - VIEWPORT_MARGIN]. The old floor-based test only
      // checked maxHeight in isolation, so a version that sized the menu
      // off the *unshifted* anchor (still a small positive number) could
      // have passed even though the box it described sat outside the
      // window. This is the assertion that closes that gap.
      const boxBottomFromTop = innerHeight - expectedBottom
      const boxTopFromTop = boxBottomFromTop - expectedMaxHeight
      expect(boxBottomFromTop).toBeLessThanOrEqual(innerHeight - VIEWPORT_MARGIN)
      expect(boxTopFromTop).toBeGreaterThanOrEqual(VIEWPORT_MARGIN)
    })

    it("sizes the menu to the larger side's available space on a realistically short viewport, staying fully inside it", async () => {
      // A short-but-usable viewport (400px) with the trigger near the
      // middle: neither side has the full ESTIMATED_MENU_HEIGHT (240),
      // but both have real, positive room. This is the case that
      // motivated the shift+size rewrite — unlike the previous test's
      // degenerate 20px viewport, a viewport like this one is exactly
      // where the old MIN_MENU_HEIGHT floor (96) could inflate maxHeight
      // past what was actually available and push the box's far edge
      // past the window edge, even though the trigger itself was nowhere
      // near either border.
      const TRIGGER_GAP = 4
      const VIEWPORT_MARGIN = 8
      const ESTIMATED_MENU_HEIGHT = 240
      const innerHeight = 400
      const rect = { left: 40, top: 162, right: 240, bottom: 178, width: 200, height: 16 }
      vi.spyOn(window, 'innerHeight', 'get').mockReturnValue(innerHeight)
      renderSelect()
      const trigger = screen.getByRole('combobox')
      stubRect(trigger, rect)
      await userEvent.click(trigger)
      const listbox = screen.getByRole('listbox')

      const spaceBelow = innerHeight - rect.bottom - TRIGGER_GAP - VIEWPORT_MARGIN
      const spaceAbove = rect.top - TRIGGER_GAP - VIEWPORT_MARGIN
      // Sanity check on the fixture: neither side fits the full estimate,
      // both have real positive room, and below has more of it — so
      // "below" is the side the component should pick.
      expect(spaceBelow).toBeLessThan(ESTIMATED_MENU_HEIGHT)
      expect(spaceAbove).toBeLessThan(ESTIMATED_MENU_HEIGHT)
      expect(spaceBelow).toBeGreaterThan(0)
      expect(spaceAbove).toBeGreaterThan(0)
      expect(spaceBelow).toBeGreaterThan(spaceAbove)

      // Placed on the side with more room (below: top set, bottom unset).
      expect(listbox.style.bottom).toBe('')
      expect(listbox.style.top).toBe(`${rect.bottom + TRIGGER_GAP}px`)

      // The trigger sits well clear of both edges here, so shift is a
      // no-op — the rendered anchor is exactly the unclamped offset.
      const top = parseFloat(listbox.style.top)
      expect(top).toBe(rect.bottom + TRIGGER_GAP)

      // Height equals that side's available space exactly, proving size
      // still tracks the real room rather than snapping to a constant —
      // neither the old MIN_MENU_HEIGHT floor nor the ESTIMATED_MENU_HEIGHT
      // cap.
      expect(listbox.style.maxHeight).toBe(`${spaceBelow}px`)

      // The whole box stays inside [VIEWPORT_MARGIN, innerHeight -
      // VIEWPORT_MARGIN]: the anchor itself clears the top margin, and
      // the anchor plus height together do not cross the bottom margin.
      // This is the assertion that would have caught the original
      // overflow — a stale height floor inflates maxHeight independently
      // of the anchor, so the anchor alone looking fine is not enough.
      const maxHeight = parseFloat(listbox.style.maxHeight)
      const boxBottomFromTop = top + maxHeight
      expect(top).toBeGreaterThanOrEqual(VIEWPORT_MARGIN)
      expect(boxBottomFromTop).toBeLessThanOrEqual(innerHeight - VIEWPORT_MARGIN)
    })

    it('recomputes the maxHeight clamp on resize and shrinks it when the viewport shrinks', async () => {
      const TRIGGER_GAP = 4
      const VIEWPORT_MARGIN = 8
      const ESTIMATED_MENU_HEIGHT = 240
      const rect = { left: 40, top: 100, right: 240, bottom: 130, width: 200, height: 30 }
      const innerHeightSpy = vi.spyOn(window, 'innerHeight', 'get').mockReturnValue(800)
      renderSelect()
      const trigger = screen.getByRole('combobox')
      stubRect(trigger, rect)
      await userEvent.click(trigger)
      const listbox = screen.getByRole('listbox')
      // At innerHeight 800 the raw available space is far past the cap,
      // so this leg proves the cap holds at the module boundary rather
      // than tracking an unbounded value.
      const spaceBelowTall = 800 - rect.bottom - TRIGGER_GAP - VIEWPORT_MARGIN
      expect(spaceBelowTall).toBeGreaterThan(ESTIMATED_MENU_HEIGHT)
      expect(listbox.style.maxHeight).toBe(
        `${Math.min(spaceBelowTall, ESTIMATED_MENU_HEIGHT)}px`,
      )

      innerHeightSpy.mockReturnValue(300)
      fireEvent.resize(window)
      // The re-measure reads the menu's real (unstubbed) height from
      // menuRef, which jsdom reports as 0 — still enough to exercise the
      // same "clamp to available space" path, just recomputed against the
      // shrunk viewport. At innerHeight 300 the raw available space now
      // lands strictly below the cap, so this leg proves the height
      // actually follows the shrunk viewport down instead of staying
      // pinned at the earlier cap.
      const spaceBelowShrunk = 300 - rect.bottom - TRIGGER_GAP - VIEWPORT_MARGIN
      expect(spaceBelowShrunk).toBeGreaterThan(0)
      expect(spaceBelowShrunk).toBeLessThan(ESTIMATED_MENU_HEIGHT)
      expect(listbox.style.maxHeight).toBe(
        `${Math.min(spaceBelowShrunk, ESTIMATED_MENU_HEIGHT)}px`,
      )
    })

    it("flips to the roomier side using the menu's natural content height, not the previously clamped rendered height", async () => {
      // Regression test: the tracking effect must measure the menu's
      // natural content height (scrollHeight) when deciding whether to
      // flip sides on re-measure, not the CSS-clamped rendered height. A
      // menu that was shortened to fit below on a previous pass would
      // otherwise always look like it still fits below, and never flip to
      // a side with genuinely more room.
      const innerHeight = 200
      vi.spyOn(window, 'innerHeight', 'get').mockReturnValue(innerHeight)
      renderSelect()
      const trigger = screen.getByRole('combobox')

      // Trigger sits near the top: below has plenty of room relative to
      // the pre-paint ESTIMATED_MENU_HEIGHT estimate used for this first,
      // synchronous placement (so the menu opens below, unflipped), but
      // not enough to fit the larger natural content height stubbed below.
      stubRect(trigger, { left: 40, top: 50, right: 240, bottom: 80, width: 200, height: 30 })
      await userEvent.click(trigger)
      const listbox = screen.getByRole('listbox')
      // Opened below, with its maxHeight clamped to the (smaller than a
      // full 240px menu) space actually available below — this is the
      // "clamped rendered height" the old code would have fed straight
      // back into the next measurement.
      expect(listbox.style.top).toBe('84px')
      expect(listbox.style.bottom).toBe('')
      expect(listbox.style.maxHeight).toBe('108px')

      // jsdom performs no layout, so scrollHeight is always 0 on any
      // element unless explicitly stubbed. Define it on the rendered
      // listbox to stand in for its real (natural, unclamped) content
      // height, which is what the flip decision is supposed to use.
      Object.defineProperty(listbox, 'scrollHeight', {
        configurable: true,
        value: 150,
      })

      // Move the trigger down: below now has almost no room (5px) while
      // above has plenty (141px) — clearly the roomier side.
      stubRect(trigger, { left: 40, top: 153, right: 240, bottom: 183, width: 200, height: 30 })
      fireEvent.resize(window)

      // With the natural content height (150) fed into the flip decision,
      // spaceBelow (5) no longer fits it, and spaceAbove (141), while also
      // short of 150, is far roomier — so the menu flips above. Under the
      // old rendered-height measurement, an unstubbed element's rect height
      // is 0 in jsdom, which would make the clamped menu look like it still
      // fits below (5 >= 0) and never flip, even though above has 28x more
      // room.
      expect(listbox.style.top).toBe('')
      expect(listbox.style.bottom).toBe('51px')
      expect(listbox.style.maxHeight).toBe('141px')
    })
  })

  // ---------------------------------------------------------------------------
  // Rendered inside other components (Table, Dialog) — the two contexts
  // that were broken before the menu was portalled: an absolutely
  // positioned menu inside Table's overflow-x-auto wrapper got clipped, and
  // inside Dialog's own stacking context it could render behind the
  // backdrop. GitHub #183.
  // ---------------------------------------------------------------------------

  describe('Rendered inside other components', () => {
    it('Select inside a Table opens and its options are clickable', async () => {
      interface Row {
        id: string
        value: string
      }
      const onChange = vi.fn()
      const rows: Row[] = [{ id: '1', value: '' }]
      const columns: Column<Row>[] = [
        {
          key: 'fruit',
          header: 'Fruit',
          render: (row) => (
            <Select options={options} value={row.value} onChange={onChange} />
          ),
        },
      ]
      render(<Table columns={columns} data={rows} keyExtractor={(r) => r.id} />)

      await userEvent.click(screen.getByRole('combobox'))
      expect(screen.getByRole('listbox')).toBeInTheDocument()
      await userEvent.click(screen.getByRole('option', { name: /Cherry/ }))
      expect(onChange).toHaveBeenCalledWith('cherry')
      expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
    })

    it('Select inside a Dialog opens and its options are clickable', async () => {
      const onChange = vi.fn()
      render(
        <Dialog open onClose={vi.fn()} title="Pick a fruit">
          <Select options={options} value="" onChange={onChange} />
        </Dialog>,
      )

      await userEvent.click(screen.getByRole('combobox'))
      expect(screen.getByRole('listbox')).toBeInTheDocument()
      await userEvent.click(screen.getByRole('option', { name: 'Apple' }))
      expect(onChange).toHaveBeenCalledWith('apple')
      expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
    })

    it('menu and Dialog backdrop are both z-50 siblings under document.body, resolved by DOM order', async () => {
      // The menu's z-50 only beats the Dialog backdrop's z-50 (and the
      // fixed Sidebar's z-50, which is structurally the same case — a
      // fixed z-50 sibling under document.body) because it paints later in
      // DOM order, not because of a higher z-index.
      render(
        <Dialog open onClose={vi.fn()} title="Pick a fruit">
          <Select options={options} value="" onChange={vi.fn()} />
        </Dialog>,
      )
      await userEvent.click(screen.getByRole('combobox'))
      const dialogBackdrop = screen.getByRole('dialog').parentElement
      const listbox = screen.getByRole('listbox')
      expect(dialogBackdrop).not.toBeNull()
      const bodyChildren = Array.from(document.body.children)
      expect(bodyChildren.indexOf(listbox)).toBeGreaterThan(
        bodyChildren.indexOf(dialogBackdrop as Element),
      )
    })
  })

  // ---------------------------------------------------------------------------
  // Tab inside the searchable menu (GitHub #183) — the portalled menu lives
  // outside a Dialog's focusable-elements query, so the Dialog's own Tab
  // trap cannot wrap focus back into it. Tab must instead close the menu
  // and hand focus back to the trigger, which the Dialog's trap does track.
  // ---------------------------------------------------------------------------

  describe('Tab in searchable menu', () => {
    it('Tab closes the dropdown and returns focus to the trigger', async () => {
      renderSelect({ searchable: true })
      const trigger = screen.getByRole('combobox')
      await userEvent.click(trigger)
      const searchInput = screen.getByPlaceholderText('Search...')
      fireEvent.keyDown(searchInput, { key: 'Tab' })
      expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
      expect(document.activeElement).toBe(trigger)
    })

    it('Shift+Tab also closes the dropdown and returns focus to the trigger', async () => {
      renderSelect({ searchable: true })
      const trigger = screen.getByRole('combobox')
      await userEvent.click(trigger)
      const searchInput = screen.getByPlaceholderText('Search...')
      fireEvent.keyDown(searchInput, { key: 'Tab', shiftKey: true })
      expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
      expect(document.activeElement).toBe(trigger)
    })
  })

  // ---------------------------------------------------------------------------
  // Dialog focus trap interaction (GitHub #183) — the searchable Select's
  // Tab handler above closes the menu and focuses the trigger itself. Because
  // the portalled search input is outside the Dialog panel's DOM subtree, the
  // panel's own focusable-elements query never sees it — so when the menu is
  // the last focusable thing in the panel, the trigger it hands focus back to
  // *is* `last` from the panel's point of view. Dialog's own Tab trap must
  // not then re-run and wrap that focus back to `first`. Covered here (not
  // Dialog.test.tsx) because it needs both components wired together.
  // ---------------------------------------------------------------------------

  describe('Dialog focus trap interaction', () => {
    it("Tab in a searchable Select as the last focusable element leaves focus on the Select trigger, not the dialog's first focusable", async () => {
      render(
        <Dialog open onClose={vi.fn()} title="Pick a fruit">
          <button>First</button>
          <Select options={options} value="" onChange={vi.fn()} searchable />
        </Dialog>,
      )
      // Let the Dialog's own open-focus effect settle before we open the
      // Select, so it isn't still pending when we make our assertions.
      await act(async () => {
        await new Promise((r) => requestAnimationFrame(r))
      })

      const trigger = screen.getByRole('combobox')
      await userEvent.click(trigger)
      const searchInput = screen.getByPlaceholderText('Search...')

      fireEvent.keyDown(searchInput, { key: 'Tab' })

      expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
      // The bug this guards against: without the defaultPrevented check,
      // the Dialog's trap would see the Select's own focus move land on
      // `last` (the trigger — the only Select-related element the panel's
      // query can see) and wrap it straight back to `first` (the Close
      // button). Asserting activeElement is the trigger already proves it
      // isn't the Close button, since they are distinct elements.
      expect(document.activeElement).toBe(trigger)
    })

    it('Tab still wraps from the last focusable to the first when a Select is present but closed (guard does not disable the trap in general)', async () => {
      render(
        <Dialog open onClose={vi.fn()} title="Pick a fruit">
          <button>First</button>
          <Select options={options} value="" onChange={vi.fn()} searchable />
        </Dialog>,
      )
      await act(async () => {
        await new Promise((r) => requestAnimationFrame(r))
      })

      const trigger = screen.getByRole('combobox')
      trigger.focus()
      expect(document.activeElement).toBe(trigger)

      await userEvent.tab()

      expect(document.activeElement).toBe(
        screen.getByRole('button', { name: 'Close' }),
      )
    })
  })
})
