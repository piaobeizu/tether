// The shell's pane vocabulary — one id space for every surface, plus the map
// that projects it onto the desktop's three columns (tether#195).
//
// The old SPA had TWO selection axes and a fixed third column: `MainView`
// ('canvas' | 'work') chosen from the activity bar, `RightTab`
// ('chat' | 'skill' | 'shell') chosen from the tab strip, and the workspace tree
// nailed to the left. tether#173's owner ruling ① flattens all of them onto one
// narrow surface with Chat primary, so the shell now needs to answer "which pane
// is showing" in a form that works on a phone AND in three columns.
//
// Two axes plus a flat list would be three representations of one fact. This
// module is the single one: a flat `PaneId`, and `COLUMN_OF` as the only place
// that knows a pane belongs in a column. Everything else — the tab strip, the
// activity bar, the drawer, the persisted keys — is derived from it.
//
// The pane list itself is not invented here. It is the directory list of the old
// SPA's panes/, read off the branch that still has them:
//
//     git ls-tree -d --name-only "origin/main:web/src/panes"
//     canvas  chat  shell  skill  work  workspace
//
// (tether#173 §9 carried this list as unverified. It is verified now, and it
// matched.)

/** Every surface the shell can show. */
export type PaneId = 'workspace' | 'canvas' | 'work' | 'chat' | 'skill' | 'shell'

/** The three columns of the ≥lg layout. Below lg exactly one pane renders. */
export type ColumnId = 'left' | 'middle' | 'right'

/**
 * Which column each pane lives in at ≥lg.
 *
 * 🔴 This object is the source, not a description of one. `panesInColumn` filters
 * it, `isPaneId` tests against it, and the tab strip and activity bar map over
 * its output. The old App.tsx kept `RIGHT_TABS` and `MAIN_VIEWS` as separate
 * literals and had to keep them agreeing with the render by hand; its own comment
 * ("RIGHT_TABS, not a second literal list: the two used to be written out
 * separately and agreed only by hand") records how that went.
 *
 * Insertion order is load-bearing and safe to rely on: all keys are non-integer
 * strings, for which `Object.keys` is specified to return insertion order. Within
 * a column the order here is the display order, and it reproduces the old app's:
 * canvas before work, chat before skill before shell.
 */
export const COLUMN_OF: Readonly<Record<PaneId, ColumnId>> = {
  workspace: 'left',
  canvas: 'middle',
  work: 'middle',
  chat: 'right',
  skill: 'right',
  shell: 'right',
}

/** Every pane id, in declaration order. Derived — never write a second list. */
export const PANE_IDS: readonly PaneId[] = Object.keys(COLUMN_OF) as PaneId[]

/** The columns, in left-to-right order. */
export const COLUMN_IDS: readonly ColumnId[] = ['left', 'middle', 'right']

/**
 * The panes a column can show, in display order.
 *
 * LAY-19 ("the right column's tabs are Chat/Skills/Shell and there is no Work
 * tab") is this function applied to 'right'. Work cannot appear there because
 * `COLUMN_OF.work` is 'middle' — there is no second list to disagree with.
 */
export function panesInColumn(column: ColumnId): PaneId[] {
  return PANE_IDS.filter(p => COLUMN_OF[p] === column)
}

/** Narrows an untrusted string (localStorage, a DOM event) to a PaneId. */
export function isPaneId(value: unknown): value is PaneId {
  return typeof value === 'string' && Object.prototype.hasOwnProperty.call(COLUMN_OF, value)
}

/**
 * Each column's pane when nothing usable is stored.
 *
 * `right: 'chat'` is owner ruling ① in its load-bearing form: a browser that has
 * never chosen anything opens on Chat. LAY-22 requires each of these to be a
 * member of its own column, and panes.test.ts checks that against
 * `panesInColumn` rather than against a copy of this object — a fallback that is
 * not itself selectable is how LAY-20's legacy-value migration would silently
 * stop working.
 */
export const DEFAULT_PANE: Readonly<Record<ColumnId, PaneId>> = {
  left: 'workspace',
  middle: 'canvas',
  right: 'chat',
}

/**
 * Human-readable pane names — the accessible name of every control that selects
 * a pane (LAY-11) and the text of the breadcrumb (LAY-15).
 *
 * Deliberately one label per pane rather than the old app's split between
 * `ACTIVITY_ITEMS.label`, `ACTIVITY_ITEMS.crumb` and `RIGHT_TAB_LABEL`: three
 * names for one surface is three chances for the breadcrumb to name something
 * that is not on screen, which is the thing LAY-15 exists to catch.
 */
export const PANE_LABEL: Readonly<Record<PaneId, string>> = {
  workspace: 'Workspace',
  canvas: 'Canvas',
  work: 'Work',
  chat: 'Chat',
  skill: 'Skills',
  shell: 'Shell',
}
