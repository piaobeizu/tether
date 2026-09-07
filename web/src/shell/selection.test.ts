// Invariant IDs are <workspace>/docs/tether-ui-invariants.md §3.10.

import { describe, expect, it } from 'vitest'
import { COLUMN_OF, DEFAULT_PANE, panesInColumn } from './panes'
import {
  DEFAULT_FOCUS,
  focusForStored,
  loadSelection,
  narrowPane,
  paneForColumn,
  routeSurfaceName,
  saveSelection,
  selectPane,
  STORAGE_KEY_FOCUS,
  STORAGE_KEY_PANE,
  type SelectionStore,
  type ShellSelection,
} from './selection'

function store(seed: Record<string, string> = {}): SelectionStore & { map: Map<string, string> } {
  const map = new Map(Object.entries(seed))
  return { map, getItem: k => map.get(k) ?? null, setItem: (k, v) => void map.set(k, v) }
}

describe('selection', () => {
  it('LAY-16: restores each column from storage', () => {
    const s = loadSelection(
      store({
        [STORAGE_KEY_PANE.middle]: 'work',
        [STORAGE_KEY_PANE.right]: 'shell',
        [STORAGE_KEY_FOCUS]: 'middle',
      }),
    )
    expect(s.active.middle).toBe('work')
    expect(s.active.right).toBe('shell')
    expect(s.focus).toBe('middle')
  })

  it('LAY-16: a value that is not a pane at all falls back to the column default', () => {
    const s = loadSelection(store({ [STORAGE_KEY_PANE.middle]: '{"corrupt":true}' }))
    expect(s.active.middle).toBe(DEFAULT_PANE.middle)
  })

  it('LAY-16: nothing stored gives every column its default', () => {
    const s = loadSelection(store())
    expect(s.active.left).toBe(DEFAULT_PANE.left)
    expect(s.active.middle).toBe(DEFAULT_PANE.middle)
    expect(s.active.right).toBe(DEFAULT_PANE.right)
    expect(s.focus).toBe(DEFAULT_FOCUS)
  })

  // LAY-20. `"work"` was a valid right-pane tab before tether#90 moved Work to the
  // middle column, so a browser that ran the old UI can still hold it under
  // `tether_right_tab` — which is exactly why this module reuses that key rather
  // than inventing a new one. Note the guard cannot be `isPaneId`: 'work' IS a
  // pane. It has to be membership in the COLUMN.
  it('LAY-20: a browser holding the legacy right-tab value "work" mounts to Chat', () => {
    expect(paneForColumn('right', 'work')).toBe('chat')
    expect(loadSelection(store({ [STORAGE_KEY_PANE.right]: 'work' })).active.right).toBe('chat')
  })

  it('LAY-20: every pane id, stored in every column, still yields a pane of that column', () => {
    // Not a list of "the values a browser might hold" — the whole id space,
    // enumerated from the source, crossed with every column. A pane that fell
    // through to something outside its column would be a blank panel.
    for (const column of ['left', 'middle', 'right'] as const) {
      for (const stored of Object.keys(COLUMN_OF)) {
        expect(panesInColumn(column)).toContain(paneForColumn(column, stored))
      }
      expect(panesInColumn(column)).toContain(paneForColumn(column, null))
      expect(panesInColumn(column)).toContain(paneForColumn(column, 'work'))
    }
  })

  it('LAY-22: an unrecognised focus value falls back to a real column', () => {
    expect(focusForStored('nowhere')).toBe(DEFAULT_FOCUS)
    expect(focusForStored(null)).toBe(DEFAULT_FOCUS)
    expect(['left', 'middle', 'right']).toContain(focusForStored('garbage'))
  })

  // LAY-12. The nine rounds of rework this records happened because Work and Chat
  // competed for one column. Here the column is a property of the pane id, so
  // selecting a middle pane has no expression that reaches the right column.
  it('LAY-12: selecting Work leaves the right column showing whatever it was showing', () => {
    const before = loadSelection(store({ [STORAGE_KEY_PANE.right]: 'chat' }))
    const after = selectPane(before, 'work')
    expect(after.active.middle).toBe('work')
    expect(after.active.right).toBe('chat')
    expect(after.active.right).toBe(before.active.right)
  })

  it('LAY-12: selecting any middle pane leaves every other column untouched', () => {
    const before = loadSelection(store({ [STORAGE_KEY_PANE.right]: 'skill' }))
    for (const pane of panesInColumn('middle')) {
      const after = selectPane(before, pane)
      expect(after.active.right).toBe(before.active.right)
      expect(after.active.left).toBe(before.active.left)
    }
  })

  it('selecting a pane focuses its own column and does not mutate the input', () => {
    const before: ShellSelection = loadSelection(store())
    const after = selectPane(before, 'skill')
    expect(after.focus).toBe(COLUMN_OF.skill)
    expect(after.active.right).toBe('skill')
    expect(before.active.right).toBe(DEFAULT_PANE.right)
  })

  // LAY-21. The old handler had to special-case 'work' because it only knew about
  // right tabs; with one id space the special case is gone and the rule is the
  // whole of it: a name is a pane or it is ignored.
  it('LAY-21: routes a surface name to whichever column owns it', () => {
    const before = loadSelection(store())
    const after = routeSurfaceName(before, 'work')
    expect(after?.active.middle).toBe('work')
    expect(after?.focus).toBe('middle')
  })

  it('LAY-21: a name matching no surface is ignored rather than cast', () => {
    const before = loadSelection(store())
    for (const name of ['', 'Work', 'settings', 'toString', 42, null, undefined, {}]) {
      expect(routeSurfaceName(before, name)).toBeNull()
    }
  })

  it('narrowPane is the focused column pane, which for a fresh browser is Chat (ruling ①)', () => {
    expect(narrowPane(loadSelection(store()))).toBe('chat')
  })

  it('persists what it restores — a round trip through storage is the identity', () => {
    const s = store()
    const chosen = selectPane(loadSelection(s), 'shell')
    saveSelection(s, chosen)
    expect(loadSelection(s)).toEqual(chosen)
  })

  it('survives a storage that throws in both directions', () => {
    const throwing: SelectionStore = {
      getItem() {
        throw new Error('private mode')
      },
      setItem() {
        throw new Error('quota')
      },
    }
    expect(loadSelection(throwing).active.right).toBe(DEFAULT_PANE.right)
    expect(() => saveSelection(throwing, loadSelection(throwing))).not.toThrow()
  })
})
