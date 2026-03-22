import React, {
  useCallback,
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
} from 'react'
import { cn } from '../../lib/utils'

export interface SelectOption {
  value: string
  label: string
  description?: string
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
    const [dropAbove, setDropAbove] = useState(false)

    const containerRef = useRef<HTMLDivElement>(null)
    const searchInputRef = useRef<HTMLInputElement>(null)
    // Internal ref for the trigger — needed for focus-return and viewport flip
    const internalRef = useRef<HTMLButtonElement>(null)

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

    // Fix 2: Clamp highlightIndex so stale values never point out-of-bounds
    const clampedHighlight = Math.min(
      highlightIndex,
      Math.max(filteredOptions.length - 1, 0),
    )

    // Helper to generate stable option ids for aria-activedescendant (Fix 6)
    const optionId = (index: number) => `${generatedId}-option-${index}`

    // Stable close handler — resets transient state on every close
    const closeDropdown = useCallback(() => {
      setIsOpen(false)
      setSearch('')
      setHighlightIndex(0)
    }, [])

    // Stable ref so document listeners always call the latest version
    const closeDropdownRef = useRef(closeDropdown)
    useEffect(() => {
      closeDropdownRef.current = closeDropdown
    }, [closeDropdown])

    // Outside click closes dropdown (no focus return — user clicked elsewhere)
    useEffect(() => {
      if (!isOpen) return
      const handleMouseDown = (e: MouseEvent) => {
        if (!containerRef.current?.contains(e.target as Node)) {
          closeDropdownRef.current()
        }
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

    // Fix 5: Check viewport space and flip dropdown above if needed.
    // The setState call is deferred via rAF to avoid calling setState
    // synchronously inside an effect body (react-hooks/set-state-in-effect).
    useEffect(() => {
      if (!isOpen || !internalRef.current) return
      const el = internalRef.current
      const rafId = requestAnimationFrame(() => {
        const rect = el.getBoundingClientRect()
        const spaceBelow = window.innerHeight - rect.bottom
        const dropdownHeight = 240 // max-h-60 = 15rem = 240px
        setDropAbove(spaceBelow < dropdownHeight && rect.top > dropdownHeight)
      })
      return () => cancelAnimationFrame(rafId)
    }, [isOpen])

    const handleTriggerKeyDown = (e: React.KeyboardEvent<HTMLButtonElement>) => {
      if (disabled) return
      if (e.key === 'ArrowDown' || e.key === 'Enter' || e.key === ' ') {
        e.preventDefault()
        setIsOpen(true)
      }
    }

    const handleDropdownKeyDown = (e: React.KeyboardEvent<HTMLDivElement>) => {
      switch (e.key) {
        case 'ArrowDown':
          e.preventDefault()
          setHighlightIndex((i) => Math.min(i + 1, filteredOptions.length - 1))
          break
        case 'ArrowUp':
          e.preventDefault()
          setHighlightIndex((i) => Math.max(i - 1, 0))
          break
        case 'Enter': {
          e.preventDefault()
          const opt = filteredOptions[clampedHighlight]
          if (opt != null) {
            onChange(opt.value)
            closeDropdown()
            // Fix 4: Return focus to trigger after selection via keyboard
            internalRef.current?.focus()
          }
          break
        }
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

        {isOpen && (
          <div
            id={listboxId}
            role="listbox"
            aria-label={label}
            // Fix 5: aria-activedescendant on search input when searchable
            aria-activedescendant={
              searchable && filteredOptions.length > 0
                ? optionId(clampedHighlight)
                : undefined
            }
            className={cn(
              'absolute left-0 w-full bg-bg-secondary border border-border rounded-md shadow-lg z-40 max-h-60 overflow-y-auto',
              // Fix 5: position above or below based on viewport space
              dropAbove ? 'bottom-full mb-1' : 'top-full mt-1',
            )}
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
          </div>
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
