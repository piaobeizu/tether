// Invariant IDs are <workspace>/docs/tether-ui-invariants.md §3.10.
//
// Every assertion here reads the RENDERED tree. The pure semantics are already
// pinned in selection.test.ts; what this file adds is that the projection onto DOM
// agrees with them — which is the half LAY-11..LAY-21 were originally about, since
// they were extracted from the old App.test.tsx.

import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react'
import { Shell, type ColumnLayout } from './Shell'
import { PanePlaceholder } from './PanePlaceholder'
import { PANE_LABEL, panesInColumn, type PaneId } from './panes'
import { STORAGE_KEY_FOCUS, STORAGE_KEY_PANE, type SelectionStore } from './selection'
import type { WideSubscription } from './breakpoint'

afterEach(cleanup)

function store(seed: Record<string, string> = {}): SelectionStore {
  const map = new Map(Object.entries(seed))
  return { getItem: k => map.get(k) ?? null, setItem: (k, v) => void map.set(k, v) }
}

// The shell's own default renderer mounts the real WorkspacePane, which fetches.
// These tests are about the shell, so they render placeholders in every pane and
// leave the WorkspacePane to WorkspacePane.test.tsx; renderPane.test.tsx is what
// pins the default wiring.
const placeholders = (pane: PaneId) => <PanePlaceholder pane={pane} />

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

/** A button in the right column's pane strip. */
function tabBtn(label: string): HTMLElement {
  return within(screen.getByRole('navigation', { name: 'Right column panes' })).getByRole(
    'button',
    { name: label },
  )
}

/** Every button in the right column's pane strip, in order. */
function tabBtns(): HTMLElement[] {
  return within(screen.getByRole('navigation', { name: 'Right column panes' })).getAllByRole(
    'button',
  )
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
    render(<Shell wide={wide()} store={store()} renderPane={placeholders} />)
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
    render(<Shell wide={wide()} store={store()} renderPane={placeholders} />)
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
    render(<Shell wide={wide()} store={store()} renderPane={placeholders} />)
    expect(showing('right')?.getAttribute('data-pane')).toBe('chat')

    fireEvent.click(activityBtn(PANE_LABEL.work))

    expect(showing('middle')?.getAttribute('data-pane')).toBe('work')
    expect(showing('right')?.getAttribute('data-pane')).toBe('chat')
  })

  it('LAY-13: the active pane of every column is published for responsive rules', () => {
    const { container } = render(<Shell wide={wide()} store={store()} renderPane={placeholders} />)
    const body = container.querySelector('.sh-body')!
    expect(body.getAttribute('data-active-middle')).toBe('canvas')

    fireEvent.click(activityBtn(PANE_LABEL.work))
    expect(body.getAttribute('data-active-middle')).toBe('work')
    expect(body.getAttribute('data-active-right')).toBe('chat')
  })

  it('LAY-14: a visited pane stays mounted and hidden; a never-visited one is not mounted', () => {
    render(<Shell wide={wide()} store={store()} renderPane={placeholders} />)
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
    render(<Shell wide={wide()} store={store()} renderPane={placeholders} />)
    expect(mounted('skill')).toBeNull()
    fireEvent.click(tabBtn(PANE_LABEL.skill))
    expect(mounted('skill')).not.toBeNull()
  })

  // LAY-15. The crumb is compared with the pane the shell is really showing —
  // read out of the DOM — rather than with a second copy of the label table. A
  // crumb agreeing with a copy of itself is the WIRE-8 defect.
  it('LAY-15: the breadcrumb names the pane that is actually showing', () => {
    render(<Shell wide={wide()} store={store()} renderPane={placeholders} />)
    const crumb = () => screen.getByTestId('sh-crumb').textContent
    const focused = () => document.querySelector('[data-focus]')!.getAttribute('data-focus')!

    expect(crumb()).toBe(PANE_LABEL[showing(focused())!.getAttribute('data-pane') as PaneId])

    fireEvent.click(activityBtn(PANE_LABEL.work))
    expect(crumb()).toBe(PANE_LABEL[showing(focused())!.getAttribute('data-pane') as PaneId])

    fireEvent.click(tabBtn(PANE_LABEL.shell))
    expect(crumb()).toBe(PANE_LABEL[showing(focused())!.getAttribute('data-pane') as PaneId])
  })

  // 🔴 LAY-16's WRITE half, which nothing pinned. selection.test.ts pins
  // `saveSelection` as a pure function, and the restore cases below hand the
  // component a PRE-SEEDED store — so "the shell never persists anything" was a
  // GREEN state. Measured: deleting `saveSelection(selectionStore, next)` from
  // Shell.tsx left the whole suite at 25 files / 247 passed. The wiring hop across
  // that seam was untested from both sides at once.
  //
  // So the assertion crosses the seam: it writes through the component and reads
  // back through a SECOND mount of the component over the same store, with nothing
  // seeded by the test. That is the reload, not a stand-in for one, and it does
  // not name a storage key — a rename would still be caught.
  //
  // Check: delete `saveSelection(selectionStore, next)` from Shell.tsx and this
  // case fails.
  it('LAY-16: the shell PERSISTS what it commits, so a remount restores it', () => {
    const s = store()
    render(<Shell wide={wide()} store={s} renderPane={placeholders} />)
    fireEvent.click(activityBtn(PANE_LABEL.work))
    fireEvent.click(tabBtn(PANE_LABEL.shell))
    cleanup()

    render(<Shell wide={wide()} store={s} renderPane={placeholders} />)
    expect(showing('middle')?.getAttribute('data-pane')).toBe('work')
    expect(showing('right')?.getAttribute('data-pane')).toBe('shell')
    // The focused column is persisted too, and it is what the narrow form reads.
    expect(document.querySelector('[data-focus]')!.getAttribute('data-focus')).toBe('right')
  })

  // W3. WHICH element carries `data-wide` is load-bearing, not cosmetic. The rule
  // it drives is `[data-wide='true'] { .sh-nav-toggle { display: none } }`, which
  // compiles to the DESCENDANT selector `[data-wide=true] .sh-nav-toggle` — and
  // `.sh-nav-toggle` lives in `.sh-header`, a SIBLING of `.sh-body`. On `.sh-body`
  // the rule matches nothing and the drawer's entry point stays visible in the
  // wide form. shell.css's header said `.sh-body`, and breakpoint.test.ts matches
  // the attribute SELECTOR only, so nothing reddened either way: measured, moving
  // the attribute to `.sh-body` left the suite at 25 files / 247 passed.
  //
  // Check: move `data-wide` from `.sh-root` to `.sh-body` in Shell.tsx and this
  // case fails.
  it('publishes data-wide on .sh-root, the ancestor of every element the wide rules select', () => {
    const { container } = render(<Shell wide={wide()} store={store()} renderPane={placeholders} />)
    expect([...container.querySelectorAll('[data-wide]')].map(e => e.className)).toEqual([
      'sh-root',
    ])

    // …and the containment fact that makes that the only workable choice.
    const root = container.querySelector('.sh-root')!
    const header = container.querySelector('.sh-header')!
    const body = container.querySelector('.sh-body')!
    const toggle = container.querySelector('.sh-nav-toggle')!
    expect(header.parentElement).toBe(root)
    expect(body.parentElement).toBe(root)
    expect(header.contains(toggle)).toBe(true)
    expect(body.contains(toggle), '.sh-body cannot select the drawer toggle').toBe(false)
  })

  it('LAY-19: the right tab strip is exactly the right column, and has no Work tab', () => {
    render(<Shell wide={wide()} store={store()} renderPane={placeholders} />)
    const rendered = tabBtns().map(t => t.textContent)
    expect(rendered).toEqual(panesInColumn('right').map(p => PANE_LABEL[p]))
    expect(rendered).not.toContain(PANE_LABEL.work)
  })

  it('LAY-20: exactly one tab is selected, for every value a browser could hold', () => {
    for (const stored of ['work', 'chat', 'skill', 'shell', 'canvas', 'garbage', '']) {
      cleanup()
      render(<Shell wide={wide()} store={store({ [STORAGE_KEY_PANE.right]: stored })} renderPane={placeholders} />)
      const selected = tabBtns().filter(t => t.getAttribute('aria-current') === 'page')
      expect(selected.length, `stored=${stored}`).toBe(1)
    }
  })

  it('LAY-20: the legacy "work" right-tab value mounts to Chat', () => {
    render(<Shell wide={wide()} store={store({ [STORAGE_KEY_PANE.right]: 'work' })} renderPane={placeholders} />)
    expect(showing('right')?.getAttribute('data-pane')).toBe('chat')
  })

  it('LAY-21: tether:select-tab routes by surface name, across columns', () => {
    render(<Shell wide={wide()} store={store()} renderPane={placeholders} />)
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
    render(<Shell wide={wide()} store={store()} renderPane={placeholders} />)
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
    render(<Shell wide={wide()} store={store()} renderPane={placeholders} />)
    expect(screen.queryAllByRole('separator')).toHaveLength(0)
  })

  it('renders a resizer per fixed column once a layout rule is supplied', () => {
    const columns: ColumnLayout = { width: () => 240, resize: vi.fn() }
    render(<Shell wide={wide()} store={store()} columns={columns} renderPane={placeholders} />)
    const separators = screen.getAllByRole('separator')
    expect(separators.map(s => s.getAttribute('data-resizer'))).toEqual(['left', 'right'])
  })

  it('hands a divider drag to the layout rule without clamping it here', () => {
    const resize = vi.fn()
    const columns: ColumnLayout = { width: () => 240, resize }
    render(<Shell wide={wide()} store={store()} columns={columns} renderPane={placeholders} />)
    const left = screen.getAllByRole('separator')[0]!
    // jsdom has no PointerEvent capture API on elements by default.
    left.setPointerCapture = () => {}
    left.releasePointerCapture = () => {}
    fireEvent.pointerDown(left, { clientX: 100, pointerId: 1 })
    fireEvent.pointerMove(left, { clientX: 137, pointerId: 1 })
    expect(resize).toHaveBeenCalledWith('left', 37)
  })

  // 🔴 `role="separator"` has two forms in ARIA and they are distinguished by
  // exactly one thing: focusability. A non-focusable separator is a boundary and
  // promises no keys; a focusable one is a WIDGET, and its contract is BOTH that
  // the arrow keys move it and that it publishes its position in `aria-valuenow`,
  // updated as the position changes.
  //
  // This branch had the widget form — `tabIndex={0}` plus arrow keys, and no value
  // state at all — and took it back out. Shell.tsx's `Resizer` docblock has the
  // reasoning; the short version is that the range `aria-valuenow` would sit in is
  // not on `ColumnLayout` (and writing it as a literal here is the tether#102
  // bug), and that `ColumnLayout` has no change notification, so a value published
  // from Shell.tsx would be frozen rather than tracking the divider.
  //
  // 🔴 So this case asserts the ABSENCE of a capability, on purpose, and it is
  // written to be the thing that has to be edited when the capability arrives —
  // the same shape as WorkspacePane.test.tsx's "claims no role whose keyboard
  // contract is unimplemented". The two halves move together: whoever adds the
  // `tabIndex` back owes the value state in the same change, and this goes red if
  // they add only the first.
  //
  // Check, both directions: add `tabIndex={0}` back to `Resizer` in Shell.tsx and
  // the first assertion fails; add an `onKeyDown` that calls `e.preventDefault()`
  // and the cancellation loop fails.
  it('R10: the separator stays STRUCTURAL, because its position cannot be published here', () => {
    const resize = vi.fn()
    const columns: ColumnLayout = { width: () => 240, resize }
    render(<Shell wide={wide()} store={store()} columns={columns} renderPane={placeholders} />)
    const separators = screen.getAllByRole('separator')
    expect(separators).toHaveLength(2)

    for (const s of separators) {
      const which = s.getAttribute('data-resizer')
      // `.tabIndex` rather than the attribute: a `<div>` with no tabindex reads
      // -1 and cannot become `activeElement`, which is what "structural" means
      // operationally. Measured in jsdom: `.focus()` on such a div is a no-op.
      expect(s.tabIndex, `the ${which} separator is a tab stop`).toBeLessThan(0)
      // The widget state, which a structural separator must not carry either —
      // announcing a position without being operable is the mirror defect.
      for (const attr of ['aria-valuenow', 'aria-valuemin', 'aria-valuemax']) {
        expect(s.hasAttribute(attr), `the ${which} separator publishes ${attr}`).toBe(false)
      }
    }

    // …and it swallows no key, which is the other half of what the widget form
    // would have promised. 🔴 Read off CANCELLATION and not off `resize`: the
    // previous version of this case asserted only `expect(resize).not
    // .toHaveBeenCalled()`, and a divider that called `preventDefault()` on every
    // key and resized on none satisfied that while trapping the keyboard on a
    // divider — measured, whole-suite green. `fireEvent` returns `dispatchEvent`'s
    // value, which is false exactly when a cancelable event was prevented.
    for (const key of ['ArrowLeft', 'ArrowRight', 'Tab', 'Enter', ' ', 'Home']) {
      const notCancelled = fireEvent.keyDown(separators[0]!, { key })
      expect(notCancelled, `the separator cancelled ${JSON.stringify(key)}`).toBe(true)
    }
    expect(resize).not.toHaveBeenCalled()
  })
})

describe('Shell — narrow form', () => {
  it('ruling ①: a fresh narrow browser opens on Chat', () => {
    render(<Shell wide={widthSub(false).sub} store={store()} renderPane={placeholders} />)
    expect(screen.getByTestId('sh-crumb').textContent).toBe(PANE_LABEL.chat)
    expect(document.querySelector('[data-showing="true"]')?.getAttribute('data-pane')).toBe('chat')
  })

  it('renders exactly one pane, and no wide-form chrome', () => {
    render(<Shell wide={widthSub(false).sub} store={store()} renderPane={placeholders} />)
    expect(document.querySelectorAll('[data-showing="true"]')).toHaveLength(1)
    expect(document.querySelectorAll('.sh-column')).toHaveLength(1)
    expect(screen.queryByRole('navigation', { name: 'Main views' })).toBeNull()
    expect(screen.queryByRole('navigation', { name: 'Right column panes' })).toBeNull()
  })

  it('LAY-16: restores the pane it was left on', () => {
    render(
      <Shell
        wide={widthSub(false).sub}
        store={store({ [STORAGE_KEY_PANE.middle]: 'work', [STORAGE_KEY_FOCUS]: 'middle' })}
        renderPane={placeholders}
      />,
    )
    expect(document.querySelector('[data-showing="true"]')?.getAttribute('data-pane')).toBe('work')
  })

  it('reaches the five non-Chat panes through the drawer', () => {
    render(<Shell wide={widthSub(false).sub} store={store()} renderPane={placeholders} />)
    fireEvent.click(screen.getByRole('button', { name: 'Panes' }))
    const dialog = screen.getByRole('dialog', { name: 'Panes' })
    for (const pane of ['canvas', 'shell', 'skill', 'work', 'workspace'] as PaneId[]) {
      expect(within(dialog).getByRole('button', { name: PANE_LABEL[pane] })).toBeTruthy()
    }
  })

  it('selecting from the drawer switches the pane and closes the drawer', () => {
    render(<Shell wide={widthSub(false).sub} store={store()} renderPane={placeholders} />)
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

  // LAY-16's write half again, on the phone path — the same `commit` as the wide
  // case, reached through the drawer instead of the activity bar. Nothing is
  // seeded; the first mount is what fills the store.
  it('LAY-16: a drawer selection survives a remount', () => {
    const s = store()
    render(<Shell wide={widthSub(false).sub} store={s} renderPane={placeholders} />)
    fireEvent.click(screen.getByRole('button', { name: 'Panes' }))
    fireEvent.click(
      within(screen.getByRole('dialog', { name: 'Panes' })).getByRole('button', {
        name: PANE_LABEL.work,
      }),
    )
    cleanup()

    render(<Shell wide={widthSub(false).sub} store={s} renderPane={placeholders} />)
    expect(document.querySelector('[data-showing="true"]')?.getAttribute('data-pane')).toBe('work')
  })

  // 🔴 The shell's half of NavDrawer's `aria-modal="true"` — see that file's
  // header for why the attribute is a contract and not decoration. The drawer owns
  // the keyboard trap and the backdrop; the elements that have to leave the
  // accessibility tree are the SHELL's, so only the shell can inert them.
  //
  // ⚠️ jsdom implements the attribute, not the behaviour: it neither blocks focus
  // nor prunes the a11y tree, so this asserts that the attribute tracks the
  // drawer's state and no more. The behaviour is the browser's.
  it('inerts its own chrome while the drawer is open, and only while it is open', () => {
    const { container } = render(
      <Shell wide={widthSub(false).sub} store={store()} renderPane={placeholders} />,
    )
    const inerted = () =>
      [...container.querySelectorAll('[inert]')].map(e => e.className).sort()

    expect(inerted()).toEqual([])
    fireEvent.click(screen.getByRole('button', { name: 'Panes' }))
    expect(inerted()).toEqual(['sh-body', 'sh-header'])
    // …and the drawer itself is NOT inside anything inert, or it would be
    // unreachable too.
    const dialog = screen.getByRole('dialog', { name: 'Panes' })
    expect(container.querySelector('.sh-body')!.contains(dialog)).toBe(false)
    expect(container.querySelector('.sh-header')!.contains(dialog)).toBe(false)

    fireEvent.keyDown(document, { key: 'Escape' })
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(inerted()).toEqual([])
  })

  // 🔴 KNOWN LIMITATION, asserted rather than left to a comment nobody reads: in
  // the narrow form LAY-14 holds only WITHIN a column. See Shell.tsx's note beside
  // the narrow `ColumnView`. This case is here so the boundary is a recorded fact
  // and a later "LAY-14 holds everywhere" claim has to argue with a test.
  it('LAY-14 is column-scoped in the narrow form: a cross-column switch remounts', () => {
    render(<Shell wide={widthSub(false).sub} store={store()} renderPane={placeholders} />)
    const paneNode = () => document.querySelector('[data-showing="true"]')
    const chatFirst = paneNode()
    expect(chatFirst?.getAttribute('data-pane')).toBe('chat')

    // Same column (chat and skill are both `panesInColumn('right')`): the pane
    // element survives, which is LAY-14 doing its job.
    fireEvent.click(screen.getByRole('button', { name: 'Panes' }))
    fireEvent.click(
      within(screen.getByRole('dialog', { name: 'Panes' })).getByRole('button', {
        name: PANE_LABEL.skill,
      }),
    )
    expect(mounted('chat')).toBe(chatFirst)

    // Across columns: the whole ColumnView is replaced, so the right column's
    // panes leave the DOM and come back as new nodes on the way home.
    fireEvent.click(screen.getByRole('button', { name: 'Panes' }))
    fireEvent.click(
      within(screen.getByRole('dialog', { name: 'Panes' })).getByRole('button', {
        name: PANE_LABEL.canvas,
      }),
    )
    expect(mounted('chat'), 'the narrow form renders one column, so chat unmounted').toBeNull()

    fireEvent.click(screen.getByRole('button', { name: 'Panes' }))
    fireEvent.click(
      within(screen.getByRole('dialog', { name: 'Panes' })).getByRole('button', {
        name: PANE_LABEL.chat,
      }),
    )
    expect(mounted('chat')).not.toBeNull()
    expect(mounted('chat'), 'and came back as a NEW node — scroll and local state are gone').not.toBe(
      chatFirst,
    )
  })
})

describe('Shell — crossing the breakpoint', () => {
  // The two forms are projections of ONE state, so a rotation must not lose the
  // selection or need a migration step. This is the assertion that the shape in
  // selection.ts actually bought that.
  it('keeps the selection when the viewport crosses the breakpoint in either direction', () => {
    const w = widthSub(false)
    render(<Shell wide={w.sub} store={store()} renderPane={placeholders} />)
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
