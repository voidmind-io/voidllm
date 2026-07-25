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
    // TRIGGER_GAP (4), VIEWPORT_MARGIN (8), MIN_MENU_HEIGHT (96) and
    // ESTIMATED_MENU_HEIGHT (240) below mirror the module-private
    // constants of the same names in Select.tsx (not exported). Expected
    // values are derived from the stubbed rect and these constants rather
    // than hardcoded, so a future change to the margins or the clamp band
    // fails these tests loudly instead of leaving them silently out of
    // sync. The inline maxHeight style is clamped into
    // [MIN_MENU_HEIGHT, ESTIMATED_MENU_HEIGHT]: it tracks the measured
    // available space only inside that band, floors at MIN_MENU_HEIGHT
    // when there is little or no room on either side, and caps at
    // ESTIMATED_MENU_HEIGHT (matching the max-h-60 class) when there is
    // far more room than that.
    // -------------------------------------------------------------------

    it('clamps the below-placed menu maxHeight to the measured space below, not an unbounded value', async () => {
      const TRIGGER_GAP = 4
      const VIEWPORT_MARGIN = 8
      const MIN_MENU_HEIGHT = 96
      const ESTIMATED_MENU_HEIGHT = 240
      // innerHeight is chosen so spaceBelow lands strictly inside the
      // clamp band — proving the height tracks the measured space rather
      // than just hitting the floor or the cap.
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
      // the floor or the cap) if spaceBelow is strictly inside the band.
      expect(spaceBelow).toBeGreaterThan(MIN_MENU_HEIGHT)
      expect(spaceBelow).toBeLessThan(ESTIMATED_MENU_HEIGHT)
      expect(listbox.style.top).toBe(`${rect.bottom + TRIGGER_GAP}px`)
      expect(listbox.style.maxHeight).toBe(
        `${Math.min(Math.max(spaceBelow, MIN_MENU_HEIGHT), ESTIMATED_MENU_HEIGHT)}px`,
      )
    })

    it('caps the maxHeight at ESTIMATED_MENU_HEIGHT when the viewport offers far more room than that', async () => {
      // The inline maxHeight style would otherwise beat the max-h-60
      // class in the cascade and let the menu grow past 240px on a tall
      // viewport — this is what pins the cap back in place after the
      // move from a class-only cap to an inline style.
      const TRIGGER_GAP = 4
      const VIEWPORT_MARGIN = 8
      const MIN_MENU_HEIGHT = 96
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
        `${Math.min(Math.max(spaceBelow, MIN_MENU_HEIGHT), ESTIMATED_MENU_HEIGHT)}px`,
      )
    })

    it('clamps the above-flipped menu maxHeight to the measured space above', async () => {
      const TRIGGER_GAP = 4
      const VIEWPORT_MARGIN = 8
      const MIN_MENU_HEIGHT = 96
      const ESTIMATED_MENU_HEIGHT = 240
      // innerHeight/rect are chosen so spaceBelow doesn't fit the
      // pre-paint estimate (triggering the flip to "above") while
      // spaceAbove lands strictly inside the clamp band.
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
      // test only proves tracking if spaceAbove is strictly inside the
      // clamp band.
      expect(spaceBelow).toBeLessThan(ESTIMATED_MENU_HEIGHT)
      expect(spaceAbove).toBeGreaterThan(spaceBelow)
      expect(spaceAbove).toBeGreaterThan(MIN_MENU_HEIGHT)
      expect(spaceAbove).toBeLessThan(ESTIMATED_MENU_HEIGHT)
      expect(listbox.style.top).toBe('')
      expect(listbox.style.bottom).toBe(`${innerHeight - rect.top + TRIGGER_GAP}px`)
      expect(listbox.style.maxHeight).toBe(
        `${Math.min(Math.max(spaceAbove, MIN_MENU_HEIGHT), ESTIMATED_MENU_HEIGHT)}px`,
      )
    })

    it('flips above and clamps to that side when the menu fits on neither side', async () => {
      // Both sides are smaller than the pre-paint height estimate, so
      // under the previous logic ("above" was only chosen when space
      // fit above that estimate) this would have fallen through to
      // "below" with no clamp at all — a fixed max-h-60 (240px) menu
      // inside a short viewport, overflowing it. innerHeight/rect are
      // chosen so spaceAbove (the side picked) still lands strictly
      // inside the clamp band, proving it tracks rather than just floors.
      const TRIGGER_GAP = 4
      const VIEWPORT_MARGIN = 8
      const MIN_MENU_HEIGHT = 96
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
      // the pre-paint estimate, above truly has more room than below, and
      // above lands inside the clamp band (not just at its floor).
      expect(spaceBelow).toBeLessThan(ESTIMATED_MENU_HEIGHT)
      expect(spaceAbove).toBeLessThan(ESTIMATED_MENU_HEIGHT)
      expect(spaceAbove).toBeGreaterThan(spaceBelow)
      expect(spaceAbove).toBeGreaterThan(MIN_MENU_HEIGHT)

      // Flipped above (bottom set, top unset) ...
      expect(listbox.style.top).toBe('')
      expect(listbox.style.bottom).toBe(`${innerHeight - rect.top + TRIGGER_GAP}px`)
      // ...and clamped to the larger (above) side's space, not left
      // unbounded at the old fixed 240px.
      expect(listbox.style.maxHeight).toBe(
        `${Math.min(Math.max(spaceAbove, MIN_MENU_HEIGHT), ESTIMATED_MENU_HEIGHT)}px`,
      )
    })

    it('floors the maxHeight at MIN_MENU_HEIGHT when the available space on both sides is at or below zero', async () => {
      // A very short viewport (or a trigger scrolled partly toward an
      // edge but not far enough to count as fully out of view — see the
      // "closes the dropdown when the trigger scrolls above/below the
      // viewport" tests above for the fully-out-of-view case) can leave
      // zero or negative space on both sides. The menu must still get a
      // usable, visible height rather than staying open with maxHeight: 0.
      const TRIGGER_GAP = 4
      const VIEWPORT_MARGIN = 8
      const MIN_MENU_HEIGHT = 96
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
      // for this to actually exercise the floor.
      expect(spaceBelow).toBeLessThanOrEqual(0)
      expect(spaceAbove).toBeLessThanOrEqual(0)
      // Whichever side has (marginally) more room is the one the
      // component picks and clamps — here that is "above", since
      // spaceAbove > spaceBelow.
      expect(listbox.style.top).toBe('')
      expect(listbox.style.bottom).toBe(`${innerHeight - rect.top + TRIGGER_GAP}px`)
      expect(listbox.style.maxHeight).toBe(
        `${Math.min(Math.max(Math.max(spaceAbove, spaceBelow), MIN_MENU_HEIGHT), ESTIMATED_MENU_HEIGHT)}px`,
      )
    })

    it('recomputes the maxHeight clamp on resize and shrinks it when the viewport shrinks', async () => {
      const TRIGGER_GAP = 4
      const VIEWPORT_MARGIN = 8
      const MIN_MENU_HEIGHT = 96
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
        `${Math.min(Math.max(spaceBelowTall, MIN_MENU_HEIGHT), ESTIMATED_MENU_HEIGHT)}px`,
      )

      innerHeightSpy.mockReturnValue(300)
      fireEvent.resize(window)
      // The re-measure reads the menu's real (unstubbed) height from
      // menuRef, which jsdom reports as 0 — still enough to exercise the
      // same "clamp to available space" path, just recomputed against the
      // shrunk viewport. At innerHeight 300 the raw available space now
      // lands inside the clamp band, so this leg proves the height
      // actually follows the shrunk viewport down instead of staying
      // pinned at the earlier cap.
      const spaceBelowShrunk = 300 - rect.bottom - TRIGGER_GAP - VIEWPORT_MARGIN
      expect(spaceBelowShrunk).toBeGreaterThan(MIN_MENU_HEIGHT)
      expect(spaceBelowShrunk).toBeLessThan(ESTIMATED_MENU_HEIGHT)
      expect(listbox.style.maxHeight).toBe(
        `${Math.min(Math.max(spaceBelowShrunk, MIN_MENU_HEIGHT), ESTIMATED_MENU_HEIGHT)}px`,
      )
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
