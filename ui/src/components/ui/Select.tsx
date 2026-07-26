import React, {
  useCallback,
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
} from 'react'
import ReactDOM from 'react-dom'
import { cn } from '../../lib/utils'

export interface SelectOption {
  value: string
  label: string
  description?: string
}

// Height used to decide whether the menu should render above or below the
// trigger before it has painted (matches max-h-60 = 15rem = 240px). Refined
// against the menu's real measured height once it is mounted.
const ESTIMATED_MENU_HEIGHT = 240

// Gap kept between the menu and the viewport edge: the anchored edge is
// shifted to stay at least this far from the border, and the free edge's
// maxHeight is capped so it does not cross into this margin either.
const VIEWPORT_MARGIN = 8

// Distance between the trigger and the menu.
const TRIGGER_GAP = 4

interface MenuPosition {
  left: number
  width: number
  /** Set when the menu renders below the trigger; null when it renders above. */
  top: number | null
  /** Set when the menu renders above the trigger; null when it renders below. */
  bottom: number | null
  /**
   * Upper bound for the menu height, in pixels, derived from the space
   * actually available at the final (shifted) anchor position, capped at
   * ESTIMATED_MENU_HEIGHT. Caps the max-h-60 class so the menu scrolls
   * internally instead of overflowing the viewport.
   */
  maxHeight: number
}

export interface SelectProps {
  options: SelectOption[]
  value: string
  onChange: (value: string) => void
  placeholder?: string
  label?: string
  error?: string
  searchable?: boolean
  disabled?: boolean
  fullWidth?: boolean
  className?: string
}

export const Select = React.forwardRef<HTMLButtonElement, SelectProps>(
  function Select(
    {
      options,
      value,
      onChange,
      placeholder = 'Select...',
      label,
      error,
      searchable = false,
      disabled = false,
      fullWidth = true,
      className,
    },
    ref,
  ) {
    const generatedId = useId()
    const listboxId = `${generatedId}-listbox`
    const labelId = `${generatedId}-label`
    const errorId = `${generatedId}-error`

    const [isOpen, setIsOpen] = useState(false)
    const [search, setSearch] = useState('')
    const [highlightIndex, setHighlightIndex] = useState(0)
    const [menuPosition, setMenuPosition] = useState<MenuPosition | null>(null)

    const containerRef = useRef<HTMLDivElement>(null)
    const searchInputRef = useRef<HTMLInputElement>(null)
    // Internal ref for the trigger — needed for focus-return and viewport flip
    const internalRef = useRef<HTMLButtonElement>(null)
    // Portalled menu ref — needed so outside-click detection treats the menu
    // (rendered under document.body, outside containerRef) as "inside"
    const menuRef = useRef<HTMLDivElement>(null)

    // Merge the forwarded ref with our internal ref
    const mergedRef = useMemo(
      () =>
        (node: HTMLButtonElement | null) => {
          internalRef.current = node
          if (typeof ref === 'function') ref(node)
          else if (ref)
            (ref as React.MutableRefObject<HTMLButtonElement | null>).current =
              node
        },
      [ref],
    )

    const selectedOption = options.find((o) => o.value === value) ?? null

    // Fix 1: Memoize filteredOptions
    const filteredOptions = useMemo(
      () =>
        searchable
          ? options.filter((o) =>
              o.label.toLowerCase().includes(search.toLowerCase()),
            )
          : options,
      [options, search, searchable],
    )

    // Clamp an index into the valid range for a list of the given length. The
    // lower bound matters: pressing ArrowDown against an empty list drives the
    // stored index to -1, and options routinely arrive from an in-flight
    // query, so without it a negative index survives into a populated list -
    // aria-activedescendant would reference a nonexistent option and Enter
    // would select nothing.
    const clampIndex = (index: number, length: number) =>
      Math.min(Math.max(index, 0), Math.max(length - 1, 0))

    const clampedHighlight = clampIndex(highlightIndex, filteredOptions.length)

    // Helper to generate stable option ids for aria-activedescendant (Fix 6)
    const optionId = (index: number) => `${generatedId}-option-${index}`

    // Stable close handler — resets transient state on every close
    const closeDropdown = useCallback(() => {
      setIsOpen(false)
      setSearch('')
      setHighlightIndex(0)
      // Drop the measured position too. Every open path measures before it
      // opens, so a stale value is never rendered today - clearing it keeps
      // that true if a future path ever opens without measuring first.
      setMenuPosition(null)
    }, [])

    // Stable ref so document listeners always call the latest version
    const closeDropdownRef = useRef(closeDropdown)
    useEffect(() => {
      closeDropdownRef.current = closeDropdown
    }, [closeDropdown])

    // Outside click closes dropdown (no focus return — user clicked elsewhere).
    // The menu is portalled to document.body, so a click inside it would not
    // be "inside" containerRef — menuRef is checked too so option clicks land.
    useEffect(() => {
      if (!isOpen) return
      const handleMouseDown = (e: MouseEvent) => {
        const target = e.target as Node
        if (containerRef.current?.contains(target)) return
        if (menuRef.current?.contains(target)) return
        closeDropdownRef.current()
      }
      document.addEventListener('mousedown', handleMouseDown)
      return () => document.removeEventListener('mousedown', handleMouseDown)
    }, [isOpen])

    // Escape key closes dropdown and returns focus to trigger (Fix 4)
    useEffect(() => {
      if (!isOpen) return
      const handleKeyDown = (e: KeyboardEvent) => {
        if (e.key === 'Escape' && !e.defaultPrevented) {
          e.preventDefault()
          closeDropdownRef.current()
          internalRef.current?.focus()
        }
      }
      document.addEventListener('keydown', handleKeyDown)
      return () => document.removeEventListener('keydown', handleKeyDown)
    }, [isOpen])

    // Auto-focus search input when dropdown opens in searchable mode
    useEffect(() => {
      if (isOpen && searchable) {
        const rafId = requestAnimationFrame(() => {
          searchInputRef.current?.focus()
        })
        return () => cancelAnimationFrame(rafId)
      }
    }, [isOpen, searchable])

    // Fix 5: Compute the portalled menu's fixed-viewport position from the
    // trigger's current bounding rect. Runs the standard anchor-positioning
    // pipeline: offset (TRIGGER_GAP) -> flip (prefer below, flip above when
    // it does not fit) -> shift (pull the anchored edge back inside the
    // viewport, keeping VIEWPORT_MARGIN from the border) -> size (cap the
    // height to whatever room is left at that final, shifted position).
    // `menuHeight` is either ESTIMATED_MENU_HEIGHT (before the menu has
    // painted) or its real measured height.
    const positionMenu = useCallback((menuHeight: number) => {
      const el = internalRef.current
      if (!el) return
      const rect = el.getBoundingClientRect()
      const spaceBelow = window.innerHeight - rect.bottom - TRIGGER_GAP - VIEWPORT_MARGIN
      const spaceAbove = rect.top - TRIGGER_GAP - VIEWPORT_MARGIN
      // Prefer below. Flip above when the menu does not fit below but does
      // fit above; when it fits on neither side, take whichever side has more
      // room.
      let above = false
      if (spaceBelow < menuHeight) {
        above = spaceAbove >= menuHeight || spaceAbove > spaceBelow
      }

      // Shift: clamp the anchored edge into [VIEWPORT_MARGIN, innerHeight -
      // VIEWPORT_MARGIN] before sizing. This is a no-op whenever the trigger
      // itself is comfortably on-screen (the normal case) - it only moves
      // the anchor when the trigger sits close enough to an edge that a bare
      // offset would place the menu past the window border.
      //
      // Size: cap maxHeight to whatever room remains between the shifted
      // anchor and the opposite margin, capped at ESTIMATED_MENU_HEIGHT. No
      // floor - since the anchor is already shifted into the viewport, this
      // is always >= 0. A cramped viewport yields a small, fully visible,
      // internally-scrollable menu instead of an overflowing one.
      if (above) {
        const bottom = Math.min(
          Math.max(window.innerHeight - rect.top + TRIGGER_GAP, VIEWPORT_MARGIN),
          window.innerHeight - VIEWPORT_MARGIN,
        )
        setMenuPosition({
          left: rect.left,
          width: rect.width,
          top: null,
          bottom,
          maxHeight: Math.min(
            window.innerHeight - VIEWPORT_MARGIN - bottom,
            ESTIMATED_MENU_HEIGHT,
          ),
        })
      } else {
        const top = Math.min(
          Math.max(rect.bottom + TRIGGER_GAP, VIEWPORT_MARGIN),
          window.innerHeight - VIEWPORT_MARGIN,
        )
        setMenuPosition({
          left: rect.left,
          width: rect.width,
          top,
          bottom: null,
          maxHeight: Math.min(
            window.innerHeight - VIEWPORT_MARGIN - top,
            ESTIMATED_MENU_HEIGHT,
          ),
        })
      }
    }, [])

    // Keep the portalled menu anchored to the trigger while open. Refines
    // placement against the menu's real height once mounted (superseding the
    // ESTIMATED_MENU_HEIGHT guess used to open it), then re-measures on any
    // resize or ancestor scroll. The scroll listener is registered on the
    // capture phase so it catches scrolling from any ancestor scroll
    // container (e.g. Table's overflow-x-auto, Dialog's overflow-y-auto),
    // not just window — 'scroll' does not bubble, but capture still reaches
    // it as the event travels down to its target. Closes the menu if the
    // trigger scrolls out of the viewport entirely.
    // The setState calls are deferred via rAF/listener callbacks rather than
    // called synchronously inside the effect body (react-hooks/set-state-in-effect).
    useEffect(() => {
      if (!isOpen) return
      const el = internalRef.current
      if (!el) return

      const measure = () => {
        const rect = el.getBoundingClientRect()
        // Strict inequalities: a rect flush against an edge (bottom/top/left/
        // right exactly 0) still has a sliver of overlap with the viewport
        // and should not be treated as fully scrolled out of view.
        const outOfView =
          rect.bottom < 0 ||
          rect.top > window.innerHeight ||
          rect.right < 0 ||
          rect.left > window.innerWidth
        if (outOfView) {
          closeDropdownRef.current()
          return
        }
        // scrollHeight, not the rendered height: the rendered box is already
        // capped by the maxHeight from the previous pass, so feeding it back
        // in would make a menu that had been shortened to fit look as though
        // it fits, and it would never flip to the roomier side. scrollHeight
        // is the natural content height, which is what the flip decision
        // needs.
        const menuHeight = menuRef.current?.scrollHeight ?? ESTIMATED_MENU_HEIGHT
        positionMenu(menuHeight)
      }

      const rafId = requestAnimationFrame(measure)
      window.addEventListener('resize', measure)
      window.addEventListener('scroll', measure, true)
      return () => {
        cancelAnimationFrame(rafId)
        window.removeEventListener('resize', measure)
        window.removeEventListener('scroll', measure, true)
      }
    }, [isOpen, positionMenu])

    // Highlight the currently selected option when the menu opens, falling
    // back to the first option when nothing is selected or the value is not
    // in the list. Read from `options`, not `filteredOptions` — every path
    // that opens the menu does so with `search` already reset to '' (either
    // it was never touched, or the prior close reset it), so the two are
    // equivalent at open time and `options` avoids depending on a value that
    // is about to be recomputed.
    const getInitialHighlightIndex = () => {
      const idx = options.findIndex((o) => o.value === value)
      return idx >= 0 ? idx : 0
    }

    // Commits the highlighted option, shared by Enter (trigger and listbox)
    // and Space (trigger only, menu open).
    const selectHighlighted = () => {
      const opt = filteredOptions[clampedHighlight]
      if (opt != null) {
        onChange(opt.value)
        closeDropdown()
        // Fix 4: Return focus to trigger after selection via keyboard
        internalRef.current?.focus()
      }
    }

    // Shared arrow/Home/End/Enter navigation, operating on filteredOptions
    // and clampedHighlight. Used by the trigger while the menu is open (the
    // WAI-ARIA combobox pattern — focus stays on the trigger) and by the
    // listbox in searchable mode, where focus sits in the search input and
    // these events reach the listbox's onKeyDown by bubbling. Space is
    // deliberately not handled here — see handleTriggerKeyDown.
    const handleNavigationKeyDown = (e: React.KeyboardEvent<HTMLElement>) => {
      switch (e.key) {
        // Clamp inside the updater, on both the previous value and the result.
        // The functional form keeps the base current when several key events
        // land in the same batch, which a render-scoped value would not;
        // clamping the base stops an out-of-range value from swallowing a
        // press; and clamping the result keeps the stored index valid even for
        // an empty list, where stepping down would otherwise land on -1 again.
        case 'ArrowDown':
          e.preventDefault()
          setHighlightIndex((i) =>
            clampIndex(clampIndex(i, filteredOptions.length) + 1, filteredOptions.length),
          )
          break
        case 'ArrowUp':
          e.preventDefault()
          setHighlightIndex((i) =>
            clampIndex(clampIndex(i, filteredOptions.length) - 1, filteredOptions.length),
          )
          break
        case 'Enter':
          e.preventDefault()
          selectHighlighted()
          break
        case 'Home':
          e.preventDefault()
          setHighlightIndex(0)
          break
        case 'End':
          e.preventDefault()
          setHighlightIndex(Math.max(filteredOptions.length - 1, 0))
          break
      }
    }

    const handleTriggerKeyDown = (e: React.KeyboardEvent<HTMLButtonElement>) => {
      if (disabled) return
      if (!isOpen) {
        if (e.key === 'ArrowDown' || e.key === 'Enter' || e.key === ' ') {
          e.preventDefault()
          // Position synchronously so the portalled menu is placed correctly
          // on its very first paint, before the tracking effect's rAF fires.
          positionMenu(ESTIMATED_MENU_HEIGHT)
          setHighlightIndex(getInitialHighlightIndex())
          setIsOpen(true)
        }
        return
      }
      // Menu open: Space selects like a native <select>, everything else
      // delegates to the shared navigation handler. Space stays trigger-only
      // — in the listbox it is ordinary typing in the search input.
      if (e.key === ' ') {
        e.preventDefault()
        selectHighlighted()
        return
      }
      // Tab closes the menu and then lets focus move on normally. Note the
      // deliberate absence of preventDefault here: the searchable path has to
      // trap Tab because its search input sits in the portal, outside an
      // ancestor Dialog's focus trap. The trigger is inside that trap, so
      // nothing needs intercepting - leaving the menu open behind a moved
      // focus is the only thing to avoid.
      if (e.key === 'Tab') {
        closeDropdown()
        return
      }
      handleNavigationKeyDown(e)
    }

    const handleDropdownKeyDown = (e: React.KeyboardEvent<HTMLDivElement>) => {
      handleNavigationKeyDown(e)
    }

    const handleOptionClick = (optValue: string) => {
      onChange(optValue)
      closeDropdown()
      // Fix 4: Return focus to trigger after option click
      internalRef.current?.focus()
    }

    return (
      <div ref={containerRef} className={cn('relative', fullWidth && 'w-full', className)}>
        {label != null && (
          <label
            id={labelId}
            className="block text-sm font-medium text-text-secondary mb-1.5"
          >
            {label}
          </label>
        )}

        <button
          ref={mergedRef}
          type="button"
          role="combobox"
          aria-haspopup="listbox"
          aria-expanded={isOpen}
          aria-controls={listboxId}
          aria-labelledby={label != null ? labelId : undefined}
          aria-invalid={error ? true : undefined}
          aria-describedby={error ? errorId : undefined}
          // Fix 6: aria-activedescendant on trigger (when not searchable)
          aria-activedescendant={
            isOpen && !searchable && filteredOptions.length > 0
              ? optionId(clampedHighlight)
              : undefined
          }
          disabled={disabled}
          // Fix 3: Skip keyboard-triggered clicks (e.detail === 0) — handled by onKeyDown
          onClick={(e) => {
            if (disabled) return
            if (e.detail === 0) return
            if (isOpen) {
              closeDropdown()
            } else {
              // Position synchronously so the portalled menu is placed
              // correctly on its very first paint.
              positionMenu(ESTIMATED_MENU_HEIGHT)
              setHighlightIndex(getInitialHighlightIndex())
              setIsOpen(true)
            }
          }}
          onKeyDown={handleTriggerKeyDown}
          className={cn(
            'flex items-center justify-between w-full rounded-md bg-bg-secondary border border-border px-3 py-2 text-sm',
            'transition-colors duration-150 cursor-pointer',
            'focus:outline-none focus:border-accent focus:ring-2 focus:ring-accent/40',
            error && 'border-error focus:border-error focus:ring-error/40',
            disabled && 'opacity-50 cursor-not-allowed',
          )}
        >
          <span
            className={cn(
              'truncate',
              selectedOption != null ? 'text-text-primary' : 'text-text-tertiary',
            )}
          >
            {selectedOption != null ? selectedOption.label : placeholder}
          </span>
          <svg
            className={cn(
              'h-4 w-4 shrink-0 text-text-tertiary ml-2 transition-transform duration-150',
              isOpen && 'rotate-180',
            )}
            fill="none"
            viewBox="0 0 24 24"
            stroke="currentColor"
            strokeWidth={2}
            aria-hidden="true"
          >
            <path strokeLinecap="round" strokeLinejoin="round" d="M19 9l-7 7-7-7" />
          </svg>
        </button>

        {isOpen &&
          menuPosition != null &&
          ReactDOM.createPortal(
            <div
              ref={menuRef}
              id={listboxId}
              role="listbox"
              aria-label={label}
              // Fix 5: aria-activedescendant on search input when searchable
              aria-activedescendant={
                searchable && filteredOptions.length > 0
                  ? optionId(clampedHighlight)
                  : undefined
              }
              className="fixed bg-bg-secondary border border-border rounded-md shadow-lg z-50 max-h-60 overflow-y-auto"
              style={{
                left: menuPosition.left,
                width: menuPosition.width,
                top: menuPosition.top ?? undefined,
                bottom: menuPosition.bottom ?? undefined,
                maxHeight: menuPosition.maxHeight,
              }}
              onKeyDown={handleDropdownKeyDown}
              tabIndex={-1}
            >
              {searchable && (
                <div className="sticky top-0 bg-bg-secondary">
                  <input
                    ref={searchInputRef}
                    type="text"
                    value={search}
                    onChange={(e) => {
                      setSearch(e.target.value)
                      setHighlightIndex(0)
                    }}
                    onKeyDown={(e) => {
                      // The portalled menu lives outside any ancestor Dialog's
                      // focusable-elements query, so the Dialog's Tab-trap
                      // cannot wrap focus back into it. Close and return focus
                      // to the trigger (which the trap does track) instead of
                      // letting Tab escape the modal.
                      //
                      // This deliberately diverges from the WAI-ARIA combobox
                      // pattern, which has Tab close the popup and advance to
                      // the next element in one press; here it takes a second
                      // press. Do not "fix" that without also solving the
                      // Dialog focus-trap problem above.
                      if (e.key === 'Tab') {
                        e.preventDefault()
                        closeDropdown()
                        internalRef.current?.focus()
                      }
                    }}
                    placeholder="Search..."
                    className="w-full px-3 py-2 text-sm bg-transparent border-b border-border text-text-primary placeholder:text-text-tertiary focus:outline-none"
                  />
                </div>
              )}

              {filteredOptions.length > 0 ? (
                filteredOptions.map((opt, i) => (
                  <div
                    key={opt.value}
                    id={optionId(i)}
                    role="option"
                    aria-selected={opt.value === value}
                    onClick={() => handleOptionClick(opt.value)}
                    onMouseEnter={() => setHighlightIndex(i)}
                    className={cn(
                      'px-3 py-2 text-sm cursor-pointer transition-colors',
                      i === clampedHighlight && 'bg-bg-tertiary',
                      opt.value === value && 'text-accent bg-accent/5',
                      opt.value !== value && 'text-text-primary',
                    )}
                  >
                    {opt.label}
                    {opt.description != null && (
                      <span className="block text-xs text-text-tertiary mt-0.5">
                        {opt.description}
                      </span>
                    )}
                  </div>
                ))
              ) : (
                <div className="px-3 py-2 text-sm text-text-tertiary">
                  No results
                </div>
              )}
            </div>,
            document.body,
          )}

        {error != null && (
          <p id={errorId} role="alert" className="mt-1.5 text-xs text-error">
            {error}
          </p>
        )}
      </div>
    )
  },
)
