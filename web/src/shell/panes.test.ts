// Invariant IDs are <workspace>/docs/tether-ui-invariants.md §3.10.

import { describe, expect, it } from 'vitest'
import {
  COLUMN_IDS,
  COLUMN_OF,
  DEFAULT_PANE,
  isPaneId,
  PANE_IDS,
  PANE_LABEL,
  panesInColumn,
} from './panes'

describe('panes', () => {
  // LAY-19's load-bearing half. The old App.test.tsx phrased this as "the right
  // pane has exactly three tabs (Chat/Skills/Shell) and no Work tab" against a
  // hand-written list. Both halves are asserted here against COLUMN_OF itself:
  // the count is not pinned to a literal (a literal makes this a gate people edit
  // when they add a pane, and "no counting claims" — a sentence naming a number
  // changes its own truth value when the thing it counts changes), but the
  // MEMBERSHIP is, and membership is what LAY-19 is really about.
  it('LAY-19: no pane appears in more than one column, and Work is not a right-column pane', () => {
    const seen = COLUMN_IDS.flatMap(c => panesInColumn(c))
    expect([...seen].sort()).toEqual([...PANE_IDS].sort())
    expect(seen.length).toBe(PANE_IDS.length)

    expect(panesInColumn('right')).not.toContain('work')
    expect(COLUMN_OF.work).toBe('middle')
  })

  // LAY-22. The old comment on loadRightTab says what makes a stale stored value
  // safe: "a membership test against RIGHT_TABS *plus* a fallback that is itself a
  // member". If a column's fallback were not in its own column, the guard in
  // paneForColumn would hand back a pane that column never renders, and LAY-20's
  // legacy-value migration would land on a blank panel instead of on Chat.
  it('LAY-22: every column fallback is itself a pane of that column', () => {
    for (const column of COLUMN_IDS) {
      expect(panesInColumn(column)).toContain(DEFAULT_PANE[column])
    }
  })

  it('LAY-22: every column has at least one pane to fall back to', () => {
    for (const column of COLUMN_IDS) {
      expect(panesInColumn(column).length).toBeGreaterThan(0)
    }
  })

  it('preserves declaration order within a column, which is display order', () => {
    // Derived from COLUMN_OF's own key order, not from a second list: the filter
    // must not reorder, because the tab strip and the activity bar map over it.
    const declared = PANE_IDS.filter(p => COLUMN_OF[p] === 'right')
    expect(panesInColumn('right')).toEqual(declared)
  })

  it('isPaneId accepts every declared pane and rejects everything else', () => {
    for (const pane of PANE_IDS) expect(isPaneId(pane)).toBe(true)
    for (const other of ['', 'Chat', 'chats', 'toString', 'constructor', 42, null, undefined]) {
      expect(isPaneId(other)).toBe(false)
    }
  })

  it('every pane has a label, so no control can be rendered without an accessible name', () => {
    for (const pane of PANE_IDS) {
      expect(PANE_LABEL[pane]).toBeTruthy()
    }
  })
})
