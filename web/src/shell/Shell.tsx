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
//   · the Chat panel. It is `web/src/panes/chat/index.tsx` on `main`, a single
//     3,241-line file, and rewriting it is a separate wi (tether#173 §2 lists the
//     shell and the Chat panel as two parallel scope lines). This file routes to
//     it; `renderPane` is the seam that wi plugs into.
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
      <header className="sh-header">
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
 * control on screen is a claim that the capability is there.
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
