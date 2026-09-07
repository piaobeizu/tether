// Which pane each column is showing, and which column the narrow layout is on
// (tether#195). Pure: every function takes the storage it reads as an argument.
//
// The whole of the shell's selection state is `ShellSelection` and nothing else.
// At ≥lg all three columns render at once and each shows `active[column]`; below
// lg exactly one pane renders and it is `active[focus]`. Crossing the breakpoint
// changes which projection runs, never the state — so there is no migration step
// between the two forms and nothing that can fall out of sync.
//
// 🔴 LAY-12 ("Work must not take Chat with it") is structural here rather than
// tested-into-place. `selectPane('work')` writes `active.middle`; there is no
// expression in this module that reaches `active.right` from a middle-column
// pane. The nine rounds of rework LAY-12 records happened because Work and Chat
// competed for one column; with the column derived from the pane id, that
// competition is not representable. The test below still asserts it, because a
// future edit could reintroduce it — but the reason it passes today is the shape,
// not the assertion.

import {
  COLUMN_IDS,
  COLUMN_OF,
  DEFAULT_PANE,
  isPaneId,
  panesInColumn,
  type ColumnId,
  type PaneId,
} from './panes'

/** The shell's entire selection state. */
export interface ShellSelection {
  /** The pane each column is showing. */
  readonly active: Readonly<Record<ColumnId, PaneId>>
  /** The column the narrow layout is showing, and the one a ≥lg divider drag is relative to. */
  readonly focus: ColumnId
}

/**
 * localStorage keys.
 *
 * 🔴 `tether_right_tab` and `tether_main_view` are the OLD SPA's keys, reused
 * deliberately. A browser that ran the old UI already holds values under them,
 * and LAY-20 is precisely about one of those values: Work was a right-pane tab
 * until tether#90 moved it to the middle, so `tether_right_tab` can still read
 * `"work"`. Choosing fresh key names would have made LAY-20 unreachable — the
 * migration would be "the new key is empty", which is not a migration — and the
 * stale value would sit in storage waiting for whoever picked the old name back.
 */
export const STORAGE_KEY_PANE: Readonly<Record<ColumnId, string>> = {
  left: 'tether_pane_left',
  middle: 'tether_main_view',
  right: 'tether_right_tab',
}

/** localStorage key for the focused column. */
export const STORAGE_KEY_FOCUS = 'tether_pane_focus'

/**
 * The column a fresh browser focuses.
 *
 * Combined with `DEFAULT_PANE.right` this is owner ruling ① — "the narrow main
 * view is Chat" — expressed as two defaults rather than as a special case in the
 * narrow renderer. A returning browser gets what it last chose instead, which is
 * LAY-16; the two only look like they conflict if ① is read as "always boot to
 * Chat", and it is not. ① is about which pane the shell is built around.
 */
export const DEFAULT_FOCUS: ColumnId = 'right'

/** The subset of `Storage` this module needs, so tests need no jsdom globals. */
export interface SelectionStore {
  getItem(key: string): string | null
  setItem(key: string, value: string): void
}

/**
 * The pane to show in `column`, given whatever was persisted for it.
 *
 * Membership test plus a fallback that is itself a member — the same shape the
 * old `loadRightTab` used, and for the same stated reason: a stored value that is
 * no longer a member of its column fails membership and lands on the default,
 * exactly like a corrupted value. Nothing rewrites the stored key until the next
 * `selectPane`, so this is a read-side migration and not a write-side one.
 *
 * Note what makes it work for LAY-20 specifically: `"work"` IS a valid PaneId, so
 * `isPaneId` alone would accept it into the right column. The membership test is
 * against `panesInColumn(column)`, not against the id space.
 */
export function paneForColumn(column: ColumnId, stored: string | null): PaneId {
  if (stored !== null && isPaneId(stored) && COLUMN_OF[stored] === column) return stored
  return DEFAULT_PANE[column]
}

/** The column to focus, given whatever was persisted. Same guard shape. */
export function focusForStored(stored: string | null): ColumnId {
  return stored !== null && (COLUMN_IDS as readonly string[]).includes(stored)
    ? (stored as ColumnId)
    : DEFAULT_FOCUS
}

/** Reads the whole selection out of storage, applying the guards above. */
export function loadSelection(store: SelectionStore): ShellSelection {
  const active = {} as Record<ColumnId, PaneId>
  for (const column of COLUMN_IDS) {
    active[column] = paneForColumn(column, safeGet(store, STORAGE_KEY_PANE[column]))
  }
  return { active, focus: focusForStored(safeGet(store, STORAGE_KEY_FOCUS)) }
}

/**
 * Selects a pane: it becomes its column's active pane, and its column becomes the
 * focused one.
 *
 * Returns a new object rather than mutating, so React re-renders and so a caller
 * holding the previous selection still sees the previous selection.
 */
export function selectPane(selection: ShellSelection, pane: PaneId): ShellSelection {
  const column = COLUMN_OF[pane]
  return {
    active: { ...selection.active, [column]: pane },
    focus: column,
  }
}

/** Persists a selection. A storage failure is not worth a crash. */
export function saveSelection(store: SelectionStore, selection: ShellSelection): void {
  try {
    for (const column of COLUMN_IDS) {
      store.setItem(STORAGE_KEY_PANE[column], selection.active[column])
    }
    store.setItem(STORAGE_KEY_FOCUS, selection.focus)
  } catch {
    /* private mode / quota — the session keeps working, it just will not persist */
  }
}

/** The single pane the narrow layout renders. */
export function narrowPane(selection: ShellSelection): PaneId {
  return selection.active[selection.focus]
}

/**
 * Routes a `tether:select-tab` surface name (LAY-21).
 *
 * Returns the new selection, or `null` when the name matches no surface — the
 * caller then leaves the shell alone. Ignoring rather than casting is the whole
 * invariant: the old handler's comment says "Unknown names are ignored rather
 * than trusted into a cast", because a cast puts an id nothing renders into
 * `active` and blanks the panel.
 *
 * The old handler special-cased `'work'`, because Work had left the right column
 * and the router only knew about right tabs. With one id space that case is gone:
 * a name is a pane or it is not, and if it is, its column is already known.
 */
export function routeSurfaceName(
  selection: ShellSelection,
  name: unknown,
): ShellSelection | null {
  if (!isPaneId(name)) return null
  return selectPane(selection, name)
}

function safeGet(store: SelectionStore, key: string): string | null {
  try {
    return store.getItem(key)
  } catch {
    return null
  }
}

/** Re-exported so callers do not have to import two modules to build a strip. */
export { panesInColumn }
