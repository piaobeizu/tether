// Invariant IDs are docs/tether-ui-invariants.md §3.10.
//
// Every assertion here reads the RENDERED tree. The pure semantics are already
// pinned in selection.test.ts; what this file adds is that the projection onto DOM
// agrees with them — which is the half LAY-11..LAY-21 were originally about, since
// they were extracted from the old App.test.tsx.

import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react'
import { Shell, type ColumnLayout } from './Shell'
import { PANE_LABEL, panesInColumn, type PaneId } from './panes'
import { STORAGE_KEY_FOCUS, STORAGE_KEY_PANE, type SelectionStore } from './selection'
import type { WideSubscription } from './breakpoint'

afterEach(cleanup)

function store(seed: Record<string, string> = {}): SelectionStore {
  const map = new Map(Object.entries(seed))
  return { getItem: k => map.get(k) ?? null, setItem: (k, v) => void map.set(k, v) }
}

function widthSub(initial: boolean) {
  let wide = initial
  const listeners = new Set<() => void>()
  const sub: WideSubscription = {
    isWide: () => wide,
    subscribe(cb) {
      listeners.add(cb)
      return () => listeners.delete(cb)
    },
  }
  return {
    sub,
    set(next: boolean) {
      wide = next
      act(() => {
        for (const l of listeners) l()
      })
    },
  }
}

/** The activity-bar button for a middle-column pane. Scoped to the nav because
 *  a pane's own placeholder is also labelled with its name, and an unscoped
 *  query matches both. */
function activityBtn(label: string): HTMLElement {
  return within(screen.getByRole('navigation', { name: 'Main views' })).getByLabelText(label)
}

/** The pane element the shell is actually showing in a column, or null. */
function showing(column: string): HTMLElement | null {
  return document.querySelector(`[data-column="${column}"] [data-showing="true"]`)
}

function mounted(pane: PaneId): HTMLElement | null {
  return document.querySelector(`[data-pane="${pane}"].sh-pane`)
}

describe('Shell — wide form', () => {
  const wide = () => widthSub(true).sub

  it('LAY-11: the activity bar has one named item per middle-column pane', () => {
    render(<Shell wide={wide()} store={store()} />)
    const bar = screen.getByRole('navigation', { name: 'Main views' })
    const names = within(bar)
      .getAllByRole('button')
      .map(b => b.getAttribute('aria-label'))
    // Derived from the pane map, not from a literal list: this is the assertion
    // the old App.tsx wanted when its comment said the two lists "agreed only by
    // hand".
    expect(names).toEqual(panesInColumn('middle').map(p => PANE_LABEL[p]))
  })

  it('LAY-11: the selected marker moves with the selection', () => {
    render(<Shell wide={wide()} store={store()} />)
    const bar = screen.getByRole('navigation', { name: 'Main views' })
    const marked = () =>
      within(bar)
        .getAllByRole('button')
        .filter(b => b.getAttribute('aria-current') === 'page')
        .map(b => b.getAttribute('aria-label'))

    expect(marked()).toEqual([PANE_LABEL.canvas])
    fireEvent.click(within(bar).getByLabelText(PANE_LABEL.work))
    expect(marked()).toEqual([PANE_LABEL.work])
  })

  it('LAY-12: selecting Work does not take Chat off the right column', () => {
    render(<Shell wide={wide()} store={store()} />)
    expect(showing('right')?.getAttribute('data-pane')).toBe('chat')

    fireEvent.click(activityBtn(PANE_LABEL.work))

    expect(showing('middle')?.getAttribute('data-pane')).toBe('work')
    expect(showing('right')?.getAttribute('data-pane')).toBe('chat')
  })

  it('LAY-13: the active pane of every column is published for responsive rules', () => {
    const { container } = render(<Shell wide={wide()} store={store()} />)
    const body = container.querySelector('.sh-body')!
    expect(body.getAttribute('data-active-middle')).toBe('canvas')

    fireEvent.click(activityBtn(PANE_LABEL.work))
    expect(body.getAttribute('data-active-middle')).toBe('work')
    expect(body.getAttribute('data-active-right')).toBe('chat')
  })

  it('LAY-14: a visited pane stays mounted and hidden; a never-visited one is not mounted', () => {
    render(<Shell wide={wide()} store={store()} />)
    // Fresh browser: canvas / chat / workspace are the columns' restored panes.
    expect(mounted('canvas')).not.toBeNull()
    expect(mounted('work')).toBeNull()

    fireEvent.click(activityBtn(PANE_LABEL.work))
    expect(mounted('work')?.getAttribute('data-showing')).toBe('true')

    fireEvent.click(activityBtn(PANE_LABEL.canvas))
    // Still in the DOM — unmounting would lose its scroll position and state.
    expect(mounted('work')).not.toBeNull()
    expect(mounted('work')?.getAttribute('data-showing')).toBe('false')
  })

  it('LAY-14: a pane in another column that was never selected is not mounted either', () => {
    render(<Shell wide={wide()} store={store()} />)
    expect(mounted('skill')).toBeNull()
    fireEvent.click(screen.getByRole('tab', { name: PANE_LABEL.skill }))
    expect(mounted('skill')).not.toBeNull()
  })

  // LAY-15. The crumb is compared with the pane the shell is really showing —
  // read out of the DOM — rather than with a second copy of the label table. A
  // crumb agreeing with a copy of itself is the WIRE-8 defect.
  it('LAY-15: the breadcrumb names the pane that is actually showing', () => {
    render(<Shell wide={wide()} store={store()} />)
    const crumb = () => screen.getByTestId('sh-crumb').textContent
    const focused = () => document.querySelector('[data-focus]')!.getAttribute('data-focus')!

    expect(crumb()).toBe(PANE_LABEL[showing(focused())!.getAttribute('data-pane') as PaneId])

    fireEvent.click(activityBtn(PANE_LABEL.work))
    expect(crumb()).toBe(PANE_LABEL[showing(focused())!.getAttribute('data-pane') as PaneId])

    fireEvent.click(screen.getByRole('tab', { name: PANE_LABEL.shell }))
    expect(crumb()).toBe(PANE_LABEL[showing(focused())!.getAttribute('data-pane') as PaneId])
  })

  it('LAY-19: the right tab strip is exactly the right column, and has no Work tab', () => {
    render(<Shell wide={wide()} store={store()} />)
    const rendered = screen.getAllByRole('tab').map(t => t.textContent)
    expect(rendered).toEqual(panesInColumn('right').map(p => PANE_LABEL[p]))
    expect(rendered).not.toContain(PANE_LABEL.work)
  })

  it('LAY-20: exactly one tab is selected, for every value a browser could hold', () => {
    for (const stored of ['work', 'chat', 'skill', 'shell', 'canvas', 'garbage', '']) {
      cleanup()
      render(<Shell wide={wide()} store={store({ [STORAGE_KEY_PANE.right]: stored })} />)
      const selected = screen.getAllByRole('tab').filter(t => t.getAttribute('aria-selected') === 'true')
      expect(selected.length, `stored=${stored}`).toBe(1)
    }
  })

  it('LAY-20: the legacy "work" right-tab value mounts to Chat', () => {
    render(<Shell wide={wide()} store={store({ [STORAGE_KEY_PANE.right]: 'work' })} />)
    expect(showing('right')?.getAttribute('data-pane')).toBe('chat')
  })

  it('LAY-21: tether:select-tab routes by surface name, across columns', () => {
    render(<Shell wide={wide()} store={store()} />)
    act(() => {
      window.dispatchEvent(new CustomEvent('tether:select-tab', { detail: 'work' }))
    })
    expect(showing('middle')?.getAttribute('data-pane')).toBe('work')

    act(() => {
      window.dispatchEvent(new CustomEvent('tether:select-tab', { detail: 'shell' }))
    })
    expect(showing('right')?.getAttribute('data-pane')).toBe('shell')
  })

  it('LAY-21: a name matching no surface is ignored and does not blank the panel', () => {
    render(<Shell wide={wide()} store={store()} />)
    const before = showing('right')?.getAttribute('data-pane')
    act(() => {
      window.dispatchEvent(new CustomEvent('tether:select-tab', { detail: 'settings' }))
    })
    expect(showing('right')?.getAttribute('data-pane')).toBe(before)
    expect(showing('middle')).not.toBeNull()
  })

  // R10 at the shell level. lib/layout.ts owns MIN_MID and the drag bounds; with
  // no implementation injected there is no rule to clamp against, so the divider
  // is absent rather than present and inert.
  it('R10: no resizer is rendered when no column-layout rule is available', () => {
    render(<Shell wide={wide()} store={store()} />)
    expect(screen.queryAllByRole('separator')).toHaveLength(0)
  })

  it('renders a resizer per fixed column once a layout rule is supplied', () => {
    const columns: ColumnLayout = { width: () => 240, resize: vi.fn() }
    render(<Shell wide={wide()} store={store()} columns={columns} />)
    const separators = screen.getAllByRole('separator')
    expect(separators.map(s => s.getAttribute('data-resizer'))).toEqual(['left', 'right'])
  })

  it('hands a divider drag to the layout rule without clamping it here', () => {
    const resize = vi.fn()
    const columns: ColumnLayout = { width: () => 240, resize }
    render(<Shell wide={wide()} store={store()} columns={columns} />)
    const left = screen.getAllByRole('separator')[0]!
    // jsdom has no PointerEvent capture API on elements by default.
    left.setPointerCapture = () => {}
    left.releasePointerCapture = () => {}
    fireEvent.pointerDown(left, { clientX: 100, pointerId: 1 })
    fireEvent.pointerMove(left, { clientX: 137, pointerId: 1 })
    expect(resize).toHaveBeenCalledWith('left', 37)
  })
})

describe('Shell — narrow form', () => {
  it('ruling ①: a fresh narrow browser opens on Chat', () => {
    render(<Shell wide={widthSub(false).sub} store={store()} />)
    expect(screen.getByTestId('sh-crumb').textContent).toBe(PANE_LABEL.chat)
    expect(document.querySelector('[data-showing="true"]')?.getAttribute('data-pane')).toBe('chat')
  })

  it('renders exactly one pane, and no wide-form chrome', () => {
    render(<Shell wide={widthSub(false).sub} store={store()} />)
    expect(document.querySelectorAll('[data-showing="true"]')).toHaveLength(1)
    expect(document.querySelectorAll('.sh-column')).toHaveLength(1)
    expect(screen.queryByRole('navigation', { name: 'Main views' })).toBeNull()
    expect(screen.queryAllByRole('tab')).toHaveLength(0)
  })

  it('LAY-16: restores the pane it was left on', () => {
    render(
      <Shell
        wide={widthSub(false).sub}
        store={store({ [STORAGE_KEY_PANE.middle]: 'work', [STORAGE_KEY_FOCUS]: 'middle' })}
      />,
    )
    expect(document.querySelector('[data-showing="true"]')?.getAttribute('data-pane')).toBe('work')
  })

  it('reaches the five non-Chat panes through the drawer', () => {
    render(<Shell wide={widthSub(false).sub} store={store()} />)
    fireEvent.click(screen.getByRole('button', { name: 'Panes' }))
    const dialog = screen.getByRole('dialog', { name: 'Panes' })
    for (const pane of ['canvas', 'shell', 'skill', 'work', 'workspace'] as PaneId[]) {
      expect(within(dialog).getByRole('button', { name: PANE_LABEL[pane] })).toBeTruthy()
    }
  })

  it('selecting from the drawer switches the pane and closes the drawer', () => {
    render(<Shell wide={widthSub(false).sub} store={store()} />)
    fireEvent.click(screen.getByRole('button', { name: 'Panes' }))
    fireEvent.click(
      within(screen.getByRole('dialog', { name: 'Panes' })).getByRole('button', {
        name: PANE_LABEL.workspace,
      }),
    )
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(document.querySelector('[data-showing="true"]')?.getAttribute('data-pane')).toBe(
      'workspace',
    )
  })
})

describe('Shell — crossing the breakpoint', () => {
  // The two forms are projections of ONE state, so a rotation must not lose the
  // selection or need a migration step. This is the assertion that the shape in
  // selection.ts actually bought that.
  it('keeps the selection when the viewport crosses the breakpoint in either direction', () => {
    const w = widthSub(false)
    render(<Shell wide={w.sub} store={store()} />)
    fireEvent.click(screen.getByRole('button', { name: 'Panes' }))
    fireEvent.click(
      within(screen.getByRole('dialog', { name: 'Panes' })).getByRole('button', {
        name: PANE_LABEL.work,
      }),
    )
    expect(document.querySelector('[data-showing="true"]')?.getAttribute('data-pane')).toBe('work')

    w.set(true)
    expect(showing('middle')?.getAttribute('data-pane')).toBe('work')
    expect(showing('right')?.getAttribute('data-pane')).toBe('chat')

    w.set(false)
    expect(document.querySelectorAll('[data-showing="true"]')).toHaveLength(1)
    expect(document.querySelector('[data-showing="true"]')?.getAttribute('data-pane')).toBe('work')
  })
})
