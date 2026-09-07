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
//
// ── `aria-modal="true"` is a CONTRACT, so it is implemented ─────────────────
//
// 🔴 The first version of this file set `aria-modal="true"` with no modal
// behaviour behind it: Tab walked straight out of the panel into the page the
// backdrop was covering, and closing the drawer left focus wherever the DOM
// happened to put it. That is the same defect `ea2093e` removed from the pane
// strip's `role="tablist"` — announcing a contract to assistive technology that
// the code does not implement — which is R10 aimed at the accessibility tree
// instead of at a sentence.
//
// The author's half of `aria-modal` is that interaction with what is behind the
// dialog is actually prevented. All three channels are now answered:
//
//   keyboard  the Tab handler below cycles within the panel, and focus is
//             restored to whatever had it when the drawer opened.
//   pointer   `.sh-drawer-backdrop` is `position: fixed; inset: 0`, so a click
//             aimed at the page behind lands on the backdrop (and closes).
//   AT        the shell marks its header and body `inert` while the drawer is
//             open (Shell.tsx), which takes them out of the accessibility tree
//             for assistive technology that does not honour `aria-modal` itself.
//
// NavDrawer.test.tsx pins the keyboard half; Shell.test.tsx pins the `inert`
// half, because it is the shell that owns the elements being inerted. Neither
// jsdom nor a unit test can observe what a screen reader does with `aria-modal`,
// so the attribute is not "tested" — what is tested is that the author-side
// behaviour it presupposes is there. If a later edit drops the trap, the
// attribute goes with it.

import { useEffect, useRef } from 'react'
import { PANE_IDS, PANE_LABEL, type PaneId } from './panes'

/**
 * Everything a browser would make a tab stop, in DOM order.
 *
 * The `tabIndex >= 0` filter is not redundant with the selector: the selector
 * cannot express "an element whose tabindex attribute is absent but whose
 * default tab index is -1", and the panel itself carries `tabIndex={-1}` so that
 * it can be focused programmatically without becoming a stop.
 */
const FOCUSABLE = [
  'a[href]',
  'area[href]',
  'button:not([disabled])',
  'input:not([disabled])',
  'select:not([disabled])',
  'textarea:not([disabled])',
  'iframe',
  '[contenteditable]',
  '[tabindex]:not([tabindex="-1"])',
].join(', ')

function focusStops(panel: HTMLElement): HTMLElement[] {
  return [...panel.querySelectorAll<HTMLElement>(FOCUSABLE)].filter(el => el.tabIndex >= 0)
}

export interface NavDrawerProps {
  readonly open: boolean
  readonly current: PaneId
  readonly onSelect: (pane: PaneId) => void
  readonly onClose: () => void
}

export function NavDrawer({ open, current, onSelect, onClose }: NavDrawerProps) {
  const panelRef = useRef<HTMLDivElement | null>(null)
  const returnFocusTo = useRef<Element | null>(null)

  useEffect(() => {
    if (!open) return
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        onClose()
        return
      }
      if (e.key !== 'Tab') return
      const panel = panelRef.current
      if (panel === null) return
      const stops = focusStops(panel)
      // A panel with no tab stops at all still must not leak focus outward; the
      // panel itself is focusable programmatically, so it absorbs the Tab.
      if (stops.length === 0) {
        e.preventDefault()
        panel.focus()
        return
      }
      const first = stops[0]!
      const last = stops[stops.length - 1]!
      const active = document.activeElement
      const inside = active !== null && panel.contains(active)
      if (e.shiftKey) {
        // `active === panel` is its own case: the panel contains itself, so the
        // generic "inside" test passes and the browser's default would then move
        // focus BACKWARDS past the panel and out of the dialog.
        if (!inside || active === first || active === panel) {
          e.preventDefault()
          last.focus()
        }
        return
      }
      if (!inside || active === last) {
        e.preventDefault()
        first.focus()
      }
    }
    document.addEventListener('keydown', onKeyDown)
    return () => document.removeEventListener('keydown', onKeyDown)
  }, [open, onClose])

  // Move focus into the panel when it opens, so a keyboard or screen-reader user
  // is not left behind on the toggle with an overlay covering the page — and put
  // it back where it was on close, because the drawer is a detour and not a
  // destination. Without the restore, dismissing with Escape drops the keyboard
  // user at the top of the document.
  useEffect(() => {
    if (!open) return
    returnFocusTo.current = document.activeElement
    panelRef.current?.focus()
    return () => {
      const previous = returnFocusTo.current
      if (previous instanceof HTMLElement && previous.isConnected) previous.focus()
    }
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
