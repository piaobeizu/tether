// The phase-1 shell (tether#195): routing, the one breakpoint, pane containers,
// pane switching, and the narrow-screen navigation entry points.
//
// Everything about WHICH pane is showing lives in selection.ts; this file is the
// projection of that state onto DOM. Two projections, one state:
//
//   ≥lg   three columns at once, each showing its own `active[column]`
//   <lg   one pane — `active[focus]` — plus the drawer that changes `focus`
//
// Crossing the breakpoint swaps the projection and touches no state, which is
// why there is no migration between the forms and nothing to keep in sync.
//
// 🔴 What is NOT here, deliberately:
//
//   · the Chat panel. It is `web/src/panes/chat/index.tsx` on `main` — one file,
//     large enough that rewriting it is a separate wi (tether#173 §2 lists the
//     shell and the Chat panel as two parallel scope lines). This file routes to
//     it; `renderPane` is the seam that wi plugs into.
//
//     🔴 Deliberately no line count. This comment used to carry one, and a
//     measurement of a file on ANOTHER BRANCH is a sentence nothing in this repo
//     can ever redden on: it was exact when written and drifts silently from the
//     next commit to `main` onwards. "Large enough to be its own wi" is the
//     property the paragraph actually needs, and it stays true as the file moves.
//   · column WIDTHS. They belong to web/src/lib/layout.ts, which tether#196 is
//     porting onto this branch and which is not here yet. `columns` below is the
//     socket it plugs into: when it is absent the columns fall back to their CSS
//     flex behaviour and NO RESIZER IS RENDERED — a divider that cannot move is a
//     control claiming a capability it does not have, which is R10. It is not a
//     local reimplementation of layout.ts and must not become one.

import { useCallback, useEffect, useMemo, useRef, useState, useSyncExternalStore } from 'react'
import type { CSSProperties, ReactNode } from 'react'
// No `import './shell.css'` here on purpose. The stylesheet's single entry point
// is web/index.html's <link> to web/src/ui/index.css, which now @imports
// shell.css — so the built CSS asset does not depend on which JS module happened
// to import it first. That is web/index.html's own stated reason for holding the
// link, and web/src/ui/index.css's header invites the shell wi to add exactly
// this line ("Overrides go BELOW the import when there are any; there are none
// yet, and inventing some here would be inventing design decisions that belong to
// the shell wi").
import { NavDrawer } from './NavDrawer'
import { renderPane as defaultRenderPane } from './renderPane'
import { COLUMN_IDS, PANE_LABEL, panesInColumn, type ColumnId, type PaneId } from './panes'
import {
  loadSelection,
  narrowPane,
  routeSurfaceName,
  saveSelection,
  selectPane,
  type SelectionStore,
  type ShellSelection,
} from './selection'
import { createWideSubscription, type WideSubscription } from './breakpoint'

/**
 * The column-width rules, injected.
 *
 * 🔴 There is no implementation on this branch. It is meant to be a thin
 * `web/src/shell/columns.ts` over `web/src/lib/layout.ts`, and NEITHER FILE
 * EXISTS YET: layout.ts is still only on `main`, and tether#196 is the wi porting
 * it here. The interface is declared rather than imported so this file compiles —
 * and the shell runs — without it.
 *
 * That is also why LAY-10 (the drag path holding MIN_MID exactly, and the left
 * divider not charging the right column) is NOT among the invariants tether#195
 * delivers. It is owed by whoever wires this socket up, and it must import
 * layout.ts rather than restate its arithmetic: `ACTIVITY_W` is deliberately not
 * exported, and writing `+ 48` at a call site is the exact bug tether#102 made
 * unexpressible.
 */
export interface ColumnLayout {
  /** Current pixel width of a fixed-width column ('left' or 'right'). */
  width(column: 'left' | 'right'): number
  /** A divider was dragged by `dx` px. Implementations clamp; this file does not. */
  resize(column: 'left' | 'right', dx: number): void
}

export interface ShellProps {
  /** Renders a pane's contents. Defaults to `./renderPane`. */
  readonly renderPane?: (pane: PaneId) => ReactNode
  /** Where the selection is persisted. Defaults to `localStorage`. */
  readonly store?: SelectionStore
  /** Viewport-width subscription. Defaults to a `matchMedia` one. */
  readonly wide?: WideSubscription
  /** Column widths and drag handling. Absent ⇒ no resizers are rendered. */
  readonly columns?: ColumnLayout
}

const memoryStore = (): SelectionStore => {
  const m = new Map<string, string>()
  return { getItem: k => m.get(k) ?? null, setItem: (k, v) => void m.set(k, v) }
}

export function Shell({ renderPane, store, wide, columns }: ShellProps) {
  const selectionStore = useMemo<SelectionStore>(
    () => store ?? (typeof localStorage !== 'undefined' ? localStorage : memoryStore()),
    [store],
  )
  const wideSub = useMemo(() => wide ?? createWideSubscription(), [wide])
  const isWide = useSyncExternalStore(
    useCallback(cb => wideSub.subscribe(cb), [wideSub]),
    () => wideSub.isWide(),
    () => false,
  )

  const [selection, setSelection] = useState<ShellSelection>(() => loadSelection(selectionStore))

  // LAY-14. A pane is mounted once it has been visited and stays mounted, hidden,
  // afterwards; a pane never visited is not in the DOM at all. Seeded with each
  // column's restored pane, because those are selected from the first paint.
  const [visited, setVisited] = useState<readonly PaneId[]>(() =>
    COLUMN_IDS.map(c => selection.active[c]),
  )

  const [drawerOpen, setDrawerOpen] = useState(false)

  // The live selection, readable from an event listener that was registered once.
  // A ref rather than the functional `setState` form because committing a
  // selection ALSO persists it and marks the pane visited, and doing either of
  // those inside a state updater makes the updater impure.
  const selectionRef = useRef(selection)
  selectionRef.current = selection

  const commit = useCallback(
    (next: ShellSelection | null) => {
      if (next === null) return
      selectionRef.current = next
      setSelection(next)
      saveSelection(selectionStore, next)
      const pane = next.active[next.focus]
      setVisited(prev => (prev.includes(pane) ? prev : [...prev, pane]))
    },
    [selectionStore],
  )

  const choose = useCallback(
    (pane: PaneId) => {
      setDrawerOpen(false)
      commit(selectPane(selectionRef.current, pane))
    },
    [commit],
  )

  // LAY-21. A router over surface NAMES, not a setter for one column: with a
  // single id space a name either is a pane — in which case its column is already
  // known — or it is not, in which case `routeSurfaceName` returns null and
  // `commit` does nothing. Ignoring rather than casting is the invariant; a cast
  // puts an id nothing renders into `active` and leaves the panel blank.
  useEffect(() => {
    const onSelectSurface = (e: Event) => {
      commit(routeSurfaceName(selectionRef.current, (e as CustomEvent<string>).detail))
    }
    window.addEventListener('tether:select-tab', onSelectSurface)
    return () => window.removeEventListener('tether:select-tab', onSelectSurface)
  }, [commit])

  const render = renderPane ?? defaultRenderPane
  const current = narrowPane(selection)

  return (
    // `data-wide` sits on the ROOT rather than on `.sh-body` because shell.css
    // selects on it for chrome outside the body too — the drawer's entry point in
    // the header has no job once every column is on screen. It is the one switch
    // the stylesheet has; the breakpoint behind it is LG_MIN_WIDTH and lives
    // nowhere else.
    <div className="sh-root" data-wide={String(isWide)}>
      {/* `inert` while the drawer is open, which is the third channel
          NavDrawer's `aria-modal="true"` presupposes — see that file's header.
          The keyboard channel is the drawer's own focus trap and the pointer
          channel is its full-viewport backdrop; this is the one that has to be
          done from OUT HERE, because the elements being taken out of the
          accessibility tree are the shell's, not the drawer's.

          ⚠️ jsdom does not implement what `inert` DOES (it neither blocks focus
          nor prunes the a11y tree there), so Shell.test.tsx can only assert that
          the attribute tracks `drawerOpen`. The behaviour is the browser's. */}
      <header className="sh-header" inert={drawerOpen}>
        <button
          type="button"
          className="sh-nav-toggle"
          aria-label="Panes"
          aria-expanded={drawerOpen}
          onClick={() => setDrawerOpen(true)}
        >
          Panes
        </button>
        {/* LAY-15. One crumb, naming `active[focus]` — the pane the shell
            considers current, which is the only pane on screen in the narrow form
            and is on screen in the wide one. One rule rather than one per form:
            two crumb rules is how a crumb comes to name something that is not
            showing. */}
        <span className="sh-crumb" data-testid="sh-crumb">
          {PANE_LABEL[current]}
        </span>
      </header>

      {/* LAY-13. The active view is published here for responsive rules to select
          on, the way `.dt-grid.mv-work` did. Published as data attributes for all
          three columns rather than as one class, because the wide form has three
          active panes and a single `mv-` class can only describe one of them. */}
      <div
        className="sh-body"
        inert={drawerOpen}
        data-focus={selection.focus}
        data-active-left={selection.active.left}
        data-active-middle={selection.active.middle}
        data-active-right={selection.active.right}
      >
        {isWide ? (
          <>
            {/* LAY-11. One item per middle-column pane, derived from the pane map
                — not a second list beside it. Each carries the pane's label as its
                accessible name, and `aria-current` is the selected marker, so the
                marker moves with the selection by construction. */}
            <nav className="sh-activity" aria-label="Main views">
              {panesInColumn('middle').map(pane => (
                <button
                  key={pane}
                  type="button"
                  className="sh-activity-btn"
                  aria-label={PANE_LABEL[pane]}
                  title={PANE_LABEL[pane]}
                  aria-current={selection.active.middle === pane ? 'page' : undefined}
                  onClick={() => choose(pane)}
                >
                  {PANE_LABEL[pane].slice(0, 1)}
                </button>
              ))}
            </nav>
            <ColumnView
              column="left"
              selection={selection}
              visited={visited}
              render={render}
              onChoose={choose}
              width={columns?.width('left')}
            />
            <Resizer column="left" columns={columns} />
            <ColumnView
              column="middle"
              selection={selection}
              visited={visited}
              render={render}
              onChoose={choose}
            />
            <Resizer column="right" columns={columns} />
            <ColumnView
              column="right"
              selection={selection}
              visited={visited}
              render={render}
              onChoose={choose}
              width={columns?.width('right')}
              showTabs
            />
          </>
        ) : (
          // 🔴 KNOWN LIMITATION, and it bites hardest on the form this phase is
          // primarily for. LAY-14 ("a visited pane stays mounted, hidden") holds
          // only WITHIN a column: the narrow form renders exactly one
          // `ColumnView`, for `selection.focus`, so a switch that changes the
          // focused column — Chat (right) → Canvas (middle) → Chat — unmounts and
          // remounts the pane and loses the scroll position and local state that
          // LAY-14 exists to preserve. Inside a column it holds: Chat → Skills →
          // Chat keeps both mounted, because both are `panesInColumn('right')`.
          //
          // Not claimed for the narrow form anywhere — LAY-14's test is wide-form
          // and mutation-proven, and the invariant was extracted from a
          // desktop-only suite (<workspace>/docs/tether-ui-invariants.md §3.10)
          // — but silence is how a reader concludes it holds everywhere. It is
          // written here rather than fixed because the fix is a design decision this
          // wi does not own: keeping all three columns mounted on a phone means every
          // visited pane's subscriptions and DOM stay live behind the one on
          // screen — a whole Chat transcript among them, with its own streaming
          // subscription. Whoever rewrites Chat measures that and decides.
          //
          // Reproduce: switch Chat → Canvas → Chat in the narrow form and the
          // Chat pane's `.sh-pane` element is a new node.
          <ColumnView
            column={selection.focus}
            selection={selection}
            visited={visited}
            render={render}
            onChoose={choose}
          />
        )}
      </div>

      <NavDrawer
        open={drawerOpen}
        current={current}
        onSelect={choose}
        onClose={() => setDrawerOpen(false)}
      />
    </div>
  )
}

interface ColumnViewProps {
  readonly column: ColumnId
  readonly selection: ShellSelection
  readonly visited: readonly PaneId[]
  readonly render: (pane: PaneId) => ReactNode
  readonly onChoose: (pane: PaneId) => void
  readonly width?: number
  readonly showTabs?: boolean
}

function ColumnView({
  column,
  selection,
  visited,
  render,
  onChoose,
  width,
  showTabs,
}: ColumnViewProps) {
  const active = selection.active[column]
  const style: CSSProperties | undefined = width === undefined ? undefined : { width }

  return (
    <section className="sh-column" data-column={column} data-active-pane={active} style={style}>
      {showTabs && (
        // LAY-19. The strip maps `panesInColumn('right')`. Work cannot appear here
        // because its column is 'middle' — there is no second list that could
        // disagree, which is what the old App.tsx's own comment asked for.
        //
        // 🔴 Deliberately NOT `role="tablist"` / `role="tab"`, though it looks
        // like a tab strip and the old SPA called it one. That role carries a
        // keyboard contract — arrow keys move between tabs, Home/End jump to the
        // ends, and the strip is one tab stop — and none of that is implemented
        // here. Announcing the role without the behaviour tells assistive
        // technology the widget works a way it does not, which is R10 aimed at an
        // accessibility tree instead of at a sentence. A labelled nav of buttons
        // with `aria-current` is a complete pattern at this size, and it is the
        // same one the activity bar uses. Whoever implements the keyboard contract
        // can add the roles then, and the roles will be true.
        <nav className="sh-tabstrip" aria-label="Right column panes">
          {panesInColumn(column).map(pane => (
            <button
              key={pane}
              type="button"
              className="sh-tab"
              aria-current={active === pane ? 'page' : undefined}
              onClick={() => onChoose(pane)}
            >
              {PANE_LABEL[pane]}
            </button>
          ))}
        </nav>
      )}
      {panesInColumn(column)
        .filter(pane => visited.includes(pane))
        .map(pane => (
          <div
            key={pane}
            className="sh-pane"
            data-pane={pane}
            data-showing={String(pane === active)}
          >
            {render(pane)}
          </div>
        ))}
    </section>
  )
}

/**
 * A divider, rendered only when something can act on the drag.
 *
 * With no `columns` there is no rule to clamp against — MIN_MID and the bounds
 * live in `web/src/lib/layout.ts`, which is on `main` and NOT on this branch yet
 * (tether#196) — so the divider is omitted rather than rendered inert. R10: a
 * control on screen is a claim that the capability is there. `main.tsx` renders
 * `<Shell />` with no `columns`, so on this branch that is every shipped
 * configuration: the divider exists in Shell.test.tsx and nowhere else yet.
 *
 * 🔴 `role="separator"` has two forms and this is the STRUCTURAL one, because the
 * element is not a tab stop. ARIA splits them on exactly that: a non-focusable
 * separator is a boundary and carries no keyboard contract, while a focusable one
 * is a WIDGET whose contract is both that the arrow keys move it and that it
 * publishes its position in `aria-valuenow` — kept up to date as the position
 * changes.
 *
 * The widget form was built on this branch (`tabIndex={0}` plus arrow keys) and
 * then taken back out, because the state it requires cannot be published from
 * anything that is here:
 *
 *   the value   `columns.width(column)` is the position, but the RANGE it sits in
 *               is not on the interface, and ARIA defaults a missing
 *               `aria-valuemin`/`aria-valuemax` to 0 and 100 — so a pixel
 *               `aria-valuenow` would announce a position on a scale it is not
 *               on. Writing the range here as literals is the tether#102 bug:
 *               layout.ts's own note says MAX_LEFT is "a preference, not the
 *               guarantee", because the effective maximum is computed from the
 *               live window by clampLeftWidth.
 *   the updates `ColumnLayout` carries no change notification and `resize()`
 *               returns nothing, so whether a move is observable at all is the
 *               INJECTOR's business — and nothing on this branch is that
 *               injector. Measured here: a pointer drag takes the rule's
 *               `width('left')` from 240 to 340 and leaves the rendered inline
 *               width at 240px, because nothing schedules a render. An
 *               `aria-valuenow` published from this file would be frozen at
 *               whatever the last unrelated render saw, which is an attribute
 *               that is present and wrong — worse than absent, and the same
 *               defect one step further in.
 *
 * So the announcement is narrowed rather than the contract half-implemented — the
 * tack `ea2093e` took with the pane strip's `role="tablist"` and the workspace
 * tree took with `role="tree"`. Resizing is pointer-only until the wi that brings
 * layout.ts brings the range and a way to observe a change with it; that wi owes
 * the `tabIndex`, the arrow keys and the value state as ONE change, and
 * Shell.test.tsx's "stays structural" case is what makes adding the first without
 * the third turn red instead of shipping.
 *
 * `aria-label` stays. The element really is the resize affordance for the pointer
 * users who can reach it, and naming it is how a reading-order traversal learns
 * what the boundary is for; it claims no key.
 */
function Resizer({ column, columns }: { column: 'left' | 'right'; columns?: ColumnLayout }) {
  const dragging = useRef(false)
  const lastX = useRef(0)

  if (!columns) return null

  return (
    <div
      className="sh-resizer"
      role="separator"
      aria-orientation="vertical"
      aria-label={`Resize ${column} column`}
      data-resizer={column}
      // No `tabIndex` and no `onKeyDown`, deliberately — see this function's
      // docblock. A `<div>` with no tabindex cannot become `activeElement` at
      // all (`.focus()` on one is a no-op), so a key handler here would be
      // unreachable code claiming a capability.
      onPointerDown={e => {
        dragging.current = true
        lastX.current = e.clientX
        e.currentTarget.setPointerCapture(e.pointerId)
      }}
      onPointerMove={e => {
        if (!dragging.current) return
        const dx = e.clientX - lastX.current
        lastX.current = e.clientX
        columns.resize(column, dx)
      }}
      onPointerUp={e => {
        dragging.current = false
        e.currentTarget.releasePointerCapture(e.pointerId)
      }}
    />
  )
}

export default Shell
