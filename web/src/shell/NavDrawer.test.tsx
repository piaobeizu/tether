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
})
