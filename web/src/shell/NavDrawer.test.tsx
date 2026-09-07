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

const outsiders: HTMLButtonElement[] = []
afterEach(() => {
  for (const el of outsiders.splice(0)) el.remove()
})

/**
 * A real focusable element OUTSIDE the panel.
 *
 * 🔴 This used to be `document.body.focus()`, and that call is a NO-OP in jsdom:
 * `document.body.tabIndex` is -1, so it leaves `document.activeElement` exactly
 * where the previous line put it — inside the panel. The "focus is already
 * outside" case therefore never ran: the handler took its `inside` branch, changed
 * nothing, and the assertion passed because focus had not moved. Measured on the
 * commit that introduced it, both directions: deleting `!inside ||` from EITHER
 * branch of NavDrawer.tsx's Tab handler left the whole suite green.
 *
 * A `<button>` appended to `document.body` is a sibling of testing-library's
 * container, so `panel.contains(document.activeElement)` is genuinely false — and
 * the cases below assert that it took focus BEFORE firing the key, because an
 * assertion built on a focus primitive that silently does nothing is the whole
 * defect being fixed here.
 */
function outsideStop(): HTMLButtonElement {
  const el = document.createElement('button')
  el.textContent = 'outside the drawer'
  document.body.appendChild(el)
  outsiders.push(el)
  return el
}

/**
 * Fires a key on `document` and reports whether the handler cancelled it.
 *
 * `fireEvent` returns `dispatchEvent`'s value, which is false exactly when
 * `preventDefault()` was called on a cancelable event. Cancellation is half of
 * what the trap has to do: without it the browser performs its own focus move as
 * well, so focus lands two stops on rather than where the trap put it.
 */
function tabWasCancelled(shiftKey = false): boolean {
  return !fireEvent.keyDown(document, { key: 'Tab', shiftKey })
}

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
  //     git grep -nE 'inert|aria-hidden|FocusTrap' c96a069 -- web/src/shell/
  //
  // matched no production CODE before this trap existed. That command — pinned to
  // the commit, so its output cannot drift — does return lines, two of them in
  // production files, and every one is prose using "inert" as an adjective rather
  // than the attribute. The distinction is the point rather than a quibble: this
  // sentence used to read "no production hit at all", and running the command it
  // prints is what falsifies that.
  //
  // What is asserted below is the AUTHOR-SIDE behaviour the attribute
  // presupposes; what a screen reader does with `aria-modal` itself is not
  // observable from jsdom and is not claimed.
  //
  // Check, both branches: delete `!inside ||` from either arm of NavDrawer.tsx's
  // Tab handler and one of the two cases below fails.
  it('aria-modal: the panel really is modal to the keyboard, forwards', () => {
    const outside = outsideStop()
    render(<NavDrawer open current="chat" onSelect={noop} onClose={noop} />)
    const panel = screen.getByRole('dialog', { name: 'Panes' })
    expect(panel.getAttribute('aria-modal')).toBe('true')
    const items = within(panel).getAllByRole('button')
    const first = items[0]!
    const last = items[items.length - 1]!

    // Tab from the last stop wraps to the first instead of leaving the dialog.
    last.focus()
    expect(tabWasCancelled()).toBe(true)
    expect(document.activeElement).toBe(first)

    // Tab from OUTSIDE the panel is pulled back in. This is the case that fires
    // when the browser has already moved focus past the panel — without it the
    // trap only works while focus happens to still be inside.
    outside.focus()
    // 🔴 The precondition, asserted rather than assumed: the previous version of
    // this case used `document.body.focus()`, which moves nothing, so the lines
    // below ran with focus still on `first` and passed without exercising the
    // branch they name.
    expect(document.activeElement).toBe(outside)
    expect(panel.contains(document.activeElement)).toBe(false)
    expect(tabWasCancelled()).toBe(true)
    expect(document.activeElement).toBe(first)
  })

  it('aria-modal: and backwards, including from the panel itself', () => {
    const outside = outsideStop()
    render(<NavDrawer open current="chat" onSelect={noop} onClose={noop} />)
    const panel = screen.getByRole('dialog', { name: 'Panes' })
    const items = within(panel).getAllByRole('button')
    const first = items[0]!
    const last = items[items.length - 1]!

    // The panel is where focus starts, and it contains itself — so the generic
    // "is focus inside" test passes and the browser's default would then move
    // focus BACKWARDS out of the dialog. Its own case, asserted first.
    expect(document.activeElement).toBe(panel)
    expect(tabWasCancelled(true)).toBe(true)
    expect(document.activeElement).toBe(last)

    first.focus()
    expect(tabWasCancelled(true)).toBe(true)
    expect(document.activeElement).toBe(last)

    // And Shift+Tab from outside, the mirror of the forward case above. This is
    // the `!inside` half of the BACKWARDS branch, and it was the other one that
    // nothing pinned.
    outside.focus()
    expect(document.activeElement).toBe(outside)
    expect(panel.contains(document.activeElement)).toBe(false)
    expect(tabWasCancelled(true)).toBe(true)
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
