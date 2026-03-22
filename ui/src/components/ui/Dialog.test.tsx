import { useState } from 'react'
import type { ComponentProps } from 'react'
import { render, screen, within, act, fireEvent } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, it, expect, vi, afterEach } from 'vitest'
import { Dialog, ConfirmDialog } from './Dialog'

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function renderDialog(props: Partial<ComponentProps<typeof Dialog>> = {}) {
  const defaults = {
    open: true,
    onClose: vi.fn(),
    title: 'Test Dialog',
    children: <p>Dialog body</p>,
  }
  return render(<Dialog {...defaults} {...props} />)
}

function renderConfirm(props: Partial<ComponentProps<typeof ConfirmDialog>> = {}) {
  const defaults = {
    open: true,
    onClose: vi.fn(),
    onConfirm: vi.fn(),
    title: 'Confirm action',
    description: 'Are you sure?',
  }
  return render(<ConfirmDialog {...defaults} {...props} />)
}

// ---------------------------------------------------------------------------
// Dialog
// ---------------------------------------------------------------------------

describe('Dialog', () => {
  describe('Rendering', () => {
    it('open=false → nothing in DOM (no role="dialog")', () => {
      renderDialog({ open: false })
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    })

    it('open=true → role="dialog" present', () => {
      renderDialog({ open: true })
      expect(screen.getByRole('dialog')).toBeInTheDocument()
    })

    it('title text is displayed', () => {
      renderDialog({ title: 'My Dialog Title' })
      expect(screen.getByText('My Dialog Title')).toBeInTheDocument()
    })

    it('children are rendered inside dialog', () => {
      renderDialog({ children: <span data-testid="child-content">Hello</span> })
      expect(screen.getByTestId('child-content')).toBeInTheDocument()
    })

    it('custom footer is rendered', () => {
      renderDialog({ footer: <button>Custom Action</button> })
      expect(screen.getByRole('button', { name: 'Custom Action' })).toBeInTheDocument()
    })

    it('aria-modal="true" is present on the dialog panel', () => {
      renderDialog()
      expect(screen.getByRole('dialog')).toHaveAttribute('aria-modal', 'true')
    })

    it('aria-labelledby links the dialog panel to the h2 title', () => {
      renderDialog({ title: 'Linked Title' })
      const dialog = screen.getByRole('dialog')
      const labelledById = dialog.getAttribute('aria-labelledby')
      expect(labelledById).toBeTruthy()
      const heading = document.getElementById(labelledById!)
      expect(heading).not.toBeNull()
      expect(heading?.tagName).toBe('H2')
      expect(heading?.textContent).toBe('Linked Title')
    })

    it('no footer rendered when footer prop is omitted', () => {
      renderDialog({ footer: undefined })
      // close button is the only button when no footer
      const buttons = screen.getAllByRole('button')
      expect(buttons).toHaveLength(1)
      expect(buttons[0]).toHaveAttribute('aria-label', 'Close')
    })
  })

  describe('Close behavior', () => {
    it('Escape key calls onClose', async () => {
      const onClose = vi.fn()
      renderDialog({ onClose })
      await userEvent.keyboard('{Escape}')
      expect(onClose).toHaveBeenCalledOnce()
    })

    it('closeOnEscape=false → Escape does NOT call onClose', async () => {
      const onClose = vi.fn()
      renderDialog({ onClose, closeOnEscape: false })
      await userEvent.keyboard('{Escape}')
      expect(onClose).not.toHaveBeenCalled()
    })

    it('backdrop mousedown calls onClose', async () => {
      const onClose = vi.fn()
      renderDialog({ onClose })
      const backdrop = screen.getByRole('dialog').parentElement!
      fireEvent.mouseDown(backdrop)
      expect(onClose).toHaveBeenCalledOnce()
    })

    it('closeOnBackdrop=false → backdrop mousedown does NOT call onClose', async () => {
      const onClose = vi.fn()
      renderDialog({ onClose, closeOnBackdrop: false })
      const backdrop = screen.getByRole('dialog').parentElement!
      fireEvent.mouseDown(backdrop)
      expect(onClose).not.toHaveBeenCalled()
    })

    it('mousedown inside panel does NOT call onClose', async () => {
      const onClose = vi.fn()
      renderDialog({ onClose })
      fireEvent.mouseDown(screen.getByRole('dialog'))
      expect(onClose).not.toHaveBeenCalled()
    })

    it('click inside panel does NOT call onClose', async () => {
      const onClose = vi.fn()
      renderDialog({ onClose })
      await userEvent.click(screen.getByRole('dialog'))
      expect(onClose).not.toHaveBeenCalled()
    })

    it('close button (X) calls onClose', async () => {
      const onClose = vi.fn()
      renderDialog({ onClose })
      await userEvent.click(screen.getByRole('button', { name: 'Close' }))
      expect(onClose).toHaveBeenCalledOnce()
    })
  })

  describe('Body scroll lock', () => {
    afterEach(() => {
      document.body.style.overflow = ''
    })

    it('body scroll is locked (overflow=hidden) when dialog is open', () => {
      renderDialog({ open: true })
      expect(document.body.style.overflow).toBe('hidden')
    })

    it('body scroll is restored when dialog closes', () => {
      document.body.style.overflow = 'auto'
      function Fixture() {
        const [open, setOpen] = useState(true)
        return (
          <Dialog open={open} onClose={() => setOpen(false)} title="Scroll test">
            <button data-testid="inner-close" onClick={() => setOpen(false)}>
              Dismiss
            </button>
          </Dialog>
        )
      }
      render(<Fixture />)
      expect(document.body.style.overflow).toBe('hidden')
      fireEvent.click(screen.getByTestId('inner-close'))
      expect(document.body.style.overflow).toBe('auto')
    })
  })

  describe('Accessibility — focus management', () => {
    it('focus moves into the dialog on open (first focusable element)', async () => {
      // The panel renders: [Close button] then [children].
      // The Close button (aria-label="Close") is the first focusable element in DOM order.
      function Fixture() {
        const [open, setOpen] = useState(false)
        return (
          <>
            <button onClick={() => setOpen(true)}>Open</button>
            <Dialog open={open} onClose={() => setOpen(false)} title="Focus test">
              <button>Inner action</button>
            </Dialog>
          </>
        )
      }
      render(<Fixture />)
      await userEvent.click(screen.getByRole('button', { name: 'Open' }))

      // requestAnimationFrame in jsdom needs act to flush
      await act(async () => {
        await new Promise((r) => requestAnimationFrame(r))
      })

      // Focus lands on the Close button — first focusable inside the panel
      expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Close' }))
    })

    it('focus returns to previous element after close', async () => {
      function Fixture() {
        const [open, setOpen] = useState(false)
        return (
          <>
            <button onClick={() => setOpen(true)}>Open</button>
            <Dialog open={open} onClose={() => setOpen(false)} title="Focus restore">
              <button>Inner</button>
            </Dialog>
          </>
        )
      }
      render(<Fixture />)
      const trigger = screen.getByRole('button', { name: 'Open' })
      trigger.focus()
      await userEvent.click(trigger)
      await new Promise((r) => setTimeout(r, 0))

      // close via Escape
      await userEvent.keyboard('{Escape}')
      expect(document.activeElement).toBe(trigger)
    })

    it('Tab wraps from last focusable to first', async () => {
      function Fixture() {
        return (
          <Dialog open onClose={vi.fn()} title="Tab trap">
            <button>First</button>
            <button>Last</button>
          </Dialog>
        )
      }
      render(<Fixture />)

      await act(async () => {
        await new Promise((r) => requestAnimationFrame(r))
      })

      // Focus the last focusable element manually: Close, First, Last — index 2
      const buttons = screen.getAllByRole('button')
      const lastBtn = buttons[buttons.length - 1]
      lastBtn.focus()
      expect(document.activeElement).toBe(lastBtn)

      await userEvent.tab()
      // Should wrap to the first focusable (Close button)
      expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Close' }))
    })

    it('Shift+Tab wraps from first focusable to last', async () => {
      function Fixture() {
        return (
          <Dialog open onClose={vi.fn()} title="Tab trap">
            <button>First</button>
            <button>Last</button>
          </Dialog>
        )
      }
      render(<Fixture />)

      await act(async () => {
        await new Promise((r) => requestAnimationFrame(r))
      })

      // Focus the first focusable element (Close button)
      const closeBtn = screen.getByRole('button', { name: 'Close' })
      closeBtn.focus()
      expect(document.activeElement).toBe(closeBtn)

      await userEvent.tab({ shift: true })
      // Should wrap to the last focusable
      const buttons = screen.getAllByRole('button')
      expect(document.activeElement).toBe(buttons[buttons.length - 1])
    })
  })

  describe('Portal', () => {
    it('dialog content is rendered into document.body, not inside test container', () => {
      const { container } = renderDialog()
      // The test container should have no dialog role inside it
      expect(container.querySelector('[role="dialog"]')).toBeNull()
      // But it IS present in the full document
      expect(document.body.querySelector('[role="dialog"]')).not.toBeNull()
    })
  })
})

// ---------------------------------------------------------------------------
// ConfirmDialog
// ---------------------------------------------------------------------------

describe('ConfirmDialog', () => {
  describe('Rendering', () => {
    it('open=false → nothing in DOM', () => {
      renderConfirm({ open: false })
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    })

    it('description text is displayed', () => {
      renderConfirm({ description: 'This will permanently delete the record.' })
      expect(screen.getByText('This will permanently delete the record.')).toBeInTheDocument()
    })

    it('confirm button has default label "Delete"', () => {
      renderConfirm()
      expect(screen.getByRole('button', { name: 'Delete' })).toBeInTheDocument()
    })

    it('custom confirmLabel is displayed on confirm button', () => {
      renderConfirm({ confirmLabel: 'Remove' })
      expect(screen.getByRole('button', { name: 'Remove' })).toBeInTheDocument()
      expect(screen.queryByRole('button', { name: 'Delete' })).not.toBeInTheDocument()
    })

    it('title text is displayed', () => {
      renderConfirm({ title: 'Delete API Key?' })
      expect(screen.getByText('Delete API Key?')).toBeInTheDocument()
    })
  })

  describe('Interactions', () => {
    it('Cancel button calls onClose', async () => {
      const onClose = vi.fn()
      renderConfirm({ onClose })
      await userEvent.click(screen.getByRole('button', { name: 'Cancel' }))
      expect(onClose).toHaveBeenCalledOnce()
    })

    it('Confirm button calls onConfirm', async () => {
      const onConfirm = vi.fn()
      renderConfirm({ onConfirm })
      await userEvent.click(screen.getByRole('button', { name: 'Delete' }))
      expect(onConfirm).toHaveBeenCalledOnce()
    })

    it('Cancel button is disabled when loading=true', () => {
      renderConfirm({ loading: true })
      expect(screen.getByRole('button', { name: 'Cancel' })).toBeDisabled()
    })

    it('Confirm button is disabled (shows spinner) when loading=true', () => {
      renderConfirm({ loading: true })
      const confirmBtn = screen.getByRole('button', { name: /delete/i })
      expect(confirmBtn).toBeDisabled()
      expect(within(confirmBtn).getByRole('status', { name: 'Loading' })).toBeInTheDocument()
    })
  })

  describe('Destructive variant', () => {
    it('Confirm button uses destructive variant (has bg-error class)', () => {
      renderConfirm()
      const confirmBtn = screen.getByRole('button', { name: 'Delete' })
      expect(confirmBtn.className).toContain('bg-error')
    })

    it('Cancel button uses secondary variant (has border class, no bg-error)', () => {
      renderConfirm()
      const cancelBtn = screen.getByRole('button', { name: 'Cancel' })
      expect(cancelBtn.className).toContain('border')
      expect(cancelBtn.className).not.toContain('bg-error')
    })
  })
})
