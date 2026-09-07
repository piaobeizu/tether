// Narrow-screen navigation (tether#195, owner ruling ①).
//
// ── the decision, and why it is a drawer and not a bottom tab bar ───────────
//
// Ruling ① fixes that Chat is the narrow main view and that the other five panes
// go to "a drawer or a bottom switch". It does not pick between them. This is the
// pick, and only one of the two is built.
//
//  1. A bottom tab bar is a PEER control and the ruling is a HIERARCHY. Six equal
//     items say "these are six things you switch between"; the ruling says one of
//     them is the surface and five are somewhere you go. A drawer draws that.
//  2. Six is over what a phone bottom bar holds. The usual ceiling is five, and
//     the usual escape is a sixth "More" item that opens — a drawer. Building the
//     drawer builds one control instead of one and a half.
//  3. The bottom edge is contested and this is a mobile-first wi. The composer,
//     the browser's own chrome and the software keyboard all live there; with
//     `100dvh` the visual viewport shrinks under the keyboard and a fixed bottom
//     bar lands on the composer. A drawer costs no permanent vertical space and
//     does not interact with the keyboard at all.
//  4. Phase 2 adds panes (tether#173 §8). A drawer absorbs them; a bottom bar has
//     to be redesigned at the seventh.
//  5. The archived prototype already chose it — .repo/tether-app/v0/src/components/
//     mobile/MobileMain.tsx: "Main chat view (always rendered) / Workspace drawer
//     (overlay, slides from left when drawerOpen)". Shape taken, code not
//     (tether#173 §7). ⚠️ That file's sibling in §7's citation, AppShell.tsx, is
//     NOT in that directory — it is at v0/src/AppShell.tsx.
//
// Accepted cost: one extra tap to a secondary pane, plus dismissal semantics. The
// dismissal rules below are the ones the invariant doc already wrote down for
// tether's other drawer (WORK-20: backdrop click closes, a click on the panel
// does not, Escape closes when the drawer is the active layer). They are cited as
// prior art rather than claimed as a taken ID — WORK-20 belongs to
// panes/work/DetailDrawer, which phase 2 owns.

import { useEffect, useRef } from 'react'
import { PANE_IDS, PANE_LABEL, type PaneId } from './panes'

export interface NavDrawerProps {
  readonly open: boolean
  readonly current: PaneId
  readonly onSelect: (pane: PaneId) => void
  readonly onClose: () => void
}

export function NavDrawer({ open, current, onSelect, onClose }: NavDrawerProps) {
  const panelRef = useRef<HTMLDivElement | null>(null)

  useEffect(() => {
    if (!open) return
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    document.addEventListener('keydown', onKeyDown)
    return () => document.removeEventListener('keydown', onKeyDown)
  }, [open, onClose])

  // Move focus into the panel when it opens, so a keyboard or screen-reader user
  // is not left behind on the toggle with an overlay covering the page.
  useEffect(() => {
    if (open) panelRef.current?.focus()
  }, [open])

  if (!open) return null

  return (
    <div
      className="sh-drawer-backdrop"
      data-testid="sh-drawer-backdrop"
      // The backdrop closes; the panel below stops the event before it gets here.
      // Written as one handler on the backdrop plus a stopPropagation on the panel
      // rather than as a target check, because a target check is wrong the moment
      // the panel gains a child: the click's target is then the child, which is
      // not the panel, and the drawer closes when the user clicks its own list.
      onClick={onClose}
    >
      <div
        ref={panelRef}
        className="sh-drawer-panel"
        role="dialog"
        aria-modal="true"
        aria-label="Panes"
        tabIndex={-1}
        onClick={e => e.stopPropagation()}
      >
        <nav aria-label="Panes">
          {/* PANE_IDS, derived — the drawer lists every surface the shell has,
              including the one currently showing, so the list cannot fall behind
              a pane being added. */}
          {PANE_IDS.map(pane => (
            <button
              key={pane}
              type="button"
              className="sh-drawer-item"
              aria-current={pane === current ? 'page' : undefined}
              onClick={() => onSelect(pane)}
            >
              {PANE_LABEL[pane]}
            </button>
          ))}
        </nav>
      </div>
    </div>
  )
}

export default NavDrawer
