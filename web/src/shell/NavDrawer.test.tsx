// The drawer is the narrow form's navigation (owner ruling ①; the drawer-vs-
// bottom-tabs decision and its reasoning are in NavDrawer.tsx's header).
//
// The dismissal assertions restate WORK-20's semantics for this drawer. They do
// NOT claim WORK-20: that invariant belongs to panes/work/DetailDrawer, which
// phase 2 owns, and naming it here would put a tick against an invariant whose
// subject is a different component.

import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, within } from '@testing-library/react'
import { NavDrawer } from './NavDrawer'
import { PANE_IDS, PANE_LABEL } from './panes'

afterEach(cleanup)

const noop = () => {}

describe('NavDrawer', () => {
  it('renders nothing while closed', () => {
    const { container } = render(
      <NavDrawer open={false} current="chat" onSelect={noop} onClose={noop} />,
    )
    expect(container.innerHTML).toBe('')
  })

  it('lists every pane, including the one currently showing', () => {
    render(<NavDrawer open current="chat" onSelect={noop} onClose={noop} />)
    const nav = within(screen.getByRole('dialog', { name: 'Panes' }))
    // Derived from PANE_IDS. A pane added to the shell appears here without the
    // drawer having to be edited — which is the failure the old app's parallel
    // literal lists produced.
    expect(nav.getAllByRole('button').map(b => b.textContent)).toEqual(
      PANE_IDS.map(p => PANE_LABEL[p]),
    )
  })

  it('marks the current pane and only the current pane', () => {
    render(<NavDrawer open current="skill" onSelect={noop} onClose={noop} />)
    const marked = screen
      .getAllByRole('button')
      .filter(b => b.getAttribute('aria-current') === 'page')
    expect(marked.map(b => b.textContent)).toEqual([PANE_LABEL.skill])
  })

  it('reports the pane that was chosen', () => {
    const onSelect = vi.fn()
    render(<NavDrawer open current="chat" onSelect={onSelect} onClose={noop} />)
    fireEvent.click(screen.getByRole('button', { name: PANE_LABEL.workspace }))
    expect(onSelect).toHaveBeenCalledWith('workspace')
  })

  it('closes on a backdrop click', () => {
    const onClose = vi.fn()
    render(<NavDrawer open current="chat" onSelect={noop} onClose={onClose} />)
    fireEvent.click(screen.getByTestId('sh-drawer-backdrop'))
    expect(onClose).toHaveBeenCalledTimes(1)
  })

  it('does NOT close on a click inside the panel, including on a child of it', () => {
    const onClose = vi.fn()
    render(<NavDrawer open current="chat" onSelect={noop} onClose={onClose} />)
    fireEvent.click(screen.getByRole('dialog', { name: 'Panes' }))
    // The child case is the one a `event.target === backdrop` check gets wrong:
    // clicking a list item makes the target the item, not the panel.
    fireEvent.click(screen.getByRole('button', { name: PANE_LABEL.canvas }))
    expect(onClose).not.toHaveBeenCalled()
  })

  it('closes on Escape while open, and stops listening once closed', () => {
    const onClose = vi.fn()
    const { rerender } = render(
      <NavDrawer open current="chat" onSelect={noop} onClose={onClose} />,
    )
    fireEvent.keyDown(document, { key: 'Escape' })
    expect(onClose).toHaveBeenCalledTimes(1)

    rerender(<NavDrawer open={false} current="chat" onSelect={noop} onClose={onClose} />)
    fireEvent.keyDown(document, { key: 'Escape' })
    expect(onClose).toHaveBeenCalledTimes(1)
  })

  it('ignores other keys', () => {
    const onClose = vi.fn()
    render(<NavDrawer open current="chat" onSelect={noop} onClose={onClose} />)
    fireEvent.keyDown(document, { key: 'Enter' })
    fireEvent.keyDown(document, { key: 'Esc' })
    expect(onClose).not.toHaveBeenCalled()
  })

  it('moves focus into the panel so a keyboard user is not left under the overlay', () => {
    render(<NavDrawer open current="chat" onSelect={noop} onClose={noop} />)
    expect(document.activeElement).toBe(screen.getByRole('dialog', { name: 'Panes' }))
  })

  // 🔴 `aria-modal="true"` is a CONTRACT, and these three cases are the reason the
  // attribute is allowed to stay. It was set with no modal behaviour at all: Tab
  // walked from the drawer straight into the page the backdrop was covering, which
  // is R10 aimed at the accessibility tree — the same defect `ea2093e` removed
  // from the pane strip's `role="tablist"`, and the PR body had already recorded
  // "no focus trap" as accepted without connecting it to the attribute.
  //
  //     grep -rnE 'inert|aria-hidden|FocusTrap' web/src/shell/
  //
  // used to return no production hit at all. What is asserted here is the
  // AUTHOR-SIDE behaviour the attribute presupposes; what a screen reader does
  // with `aria-modal` itself is not observable from jsdom and is not claimed.
  it('aria-modal: the panel really is modal to the keyboard, forwards', () => {
    render(<NavDrawer open current="chat" onSelect={noop} onClose={noop} />)
    const panel = screen.getByRole('dialog', { name: 'Panes' })
    expect(panel.getAttribute('aria-modal')).toBe('true')
    const items = within(panel).getAllByRole('button')
    const first = items[0]!
    const last = items[items.length - 1]!

    // Tab from the last stop wraps to the first instead of leaving the dialog.
    last.focus()
    fireEvent.keyDown(document, { key: 'Tab' })
    expect(document.activeElement).toBe(first)

    // Tab from OUTSIDE the panel is pulled back in. This is the case that fires
    // when the browser has already moved focus past the panel — without it the
    // trap only works while focus happens to still be inside.
    document.body.focus()
    fireEvent.keyDown(document, { key: 'Tab' })
    expect(document.activeElement).toBe(first)
  })

  it('aria-modal: and backwards, including from the panel itself', () => {
    render(<NavDrawer open current="chat" onSelect={noop} onClose={noop} />)
    const panel = screen.getByRole('dialog', { name: 'Panes' })
    const items = within(panel).getAllByRole('button')
    const first = items[0]!
    const last = items[items.length - 1]!

    // The panel is where focus starts, and it contains itself — so the generic
    // "is focus inside" test passes and the browser's default would then move
    // focus BACKWARDS out of the dialog. Its own case, asserted first.
    expect(document.activeElement).toBe(panel)
    fireEvent.keyDown(document, { key: 'Tab', shiftKey: true })
    expect(document.activeElement).toBe(last)

    first.focus()
    fireEvent.keyDown(document, { key: 'Tab', shiftKey: true })
    expect(document.activeElement).toBe(last)
  })

  it('returns focus to whatever had it when the drawer opened', () => {
    // The toggle that opens the drawer, standing in for Shell's.
    const opener = document.createElement('button')
    document.body.appendChild(opener)
    try {
      opener.focus()
      const { rerender } = render(
        <NavDrawer open current="chat" onSelect={noop} onClose={noop} />,
      )
      expect(document.activeElement).toBe(screen.getByRole('dialog', { name: 'Panes' }))

      rerender(<NavDrawer open={false} current="chat" onSelect={noop} onClose={noop} />)
      // Without this, dismissing with Escape drops the keyboard user at the top of
      // the document with no way back to where they were.
      expect(document.activeElement).toBe(opener)
    } finally {
      opener.remove()
    }
  })
})
