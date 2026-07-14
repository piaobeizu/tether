// DetailDrawer — bottom slide-up overlay for the selected wi's detail
// (tether#26). Lives inside the right Work pane, above the Work map: clicking
// a card selects a wi, which mounts this drawer sliding up from the bottom
// with the WorkDetail (goal/status/content + scenario step DAG + action bar).
// Dismiss via the × button, Esc, the dimmed map backdrop above — or by dragging
// the grip down (tether#30).
//
// HEIGHT is draggable (tether#30): grab the grip and drag up/down to resize;
// release below REST_MIN closes it; the resting height is remembered in
// localStorage. Drag uses window pointermove/up (NOT setPointerCapture, which
// ate clicks on an SVG in tether#20) and reads live state from refs so the
// add/remove listener references stay stable.
import { useEffect, useRef, useState } from 'react'
import type { PointerEvent as ReactPointerEvent } from 'react'
import { Icon } from '../../lib/icons'
import WorkDetail from './WorkDetail'

const LS_KEY = 'tether:work-drawer-h'
const MAX_PCT = 92
const REST_MIN = 40 // smallest height you can rest at; release below → close
const DRAG_MIN = 18 // visual floor while dragging (feedback in the close zone)
const DEFAULT_PCT = 85

const clampRest = (p: number) => Math.min(MAX_PCT, Math.max(REST_MIN, p))
function readStored(): number {
  try {
    const n = Number(localStorage.getItem(LS_KEY))
    return Number.isFinite(n) && n >= REST_MIN && n <= MAX_PCT ? n : DEFAULT_PCT
  } catch {
    return DEFAULT_PCT // storage unavailable (sandboxed webview) → default, don't crash render
  }
}

export default function DetailDrawer({
  id,
  onClose,
  escActive = true,
}: {
  id: string
  onClose: () => void
  /** Attach the global Esc-to-close only while the drawer is the visible layer
   *  (the Work tab is the active right tab). The drawer stays MOUNTED behind
   *  another tab (App keeps visited tabs alive), so an unguarded document
   *  listener would swallow Esc from Chat/Shell and silently drop the
   *  selection. A higher modal (Settings / catch-up) opened over the Work tab
   *  still shares document Esc — a minor accepted overlap. */
  escActive?: boolean
}) {
  const [heightPct, setHeightPct] = useState(readStored)
  const overlayRef = useRef<HTMLDivElement>(null)
  const drag = useRef<{ startY: number; startPct: number; H: number } | null>(null)
  const latestPct = useRef(heightPct)
  // a drag that actually moved suppresses the post-drag synthesized click, so
  // over-dragging up into the backdrop above the clamped panel doesn't close the
  // drawer (that click lands on the overlay, not the panel — review WARN #1).
  const didDrag = useRef(false)
  // onClose can change identity between renders; read it through a ref so the
  // once-created drag handlers below never go stale.
  const onCloseRef = useRef(onClose)
  onCloseRef.current = onClose

  // Create the window drag handlers ONCE (stable references for add/remove).
  // They read the live drag state / latest height / onClose from refs.
  const handlers = useRef<{ move: (e: PointerEvent) => void; up: () => void } | null>(null)
  if (!handlers.current) {
    const move = (e: PointerEvent) => {
      const d = drag.current
      if (!d) return
      didDrag.current = true
      const raw = d.startPct - ((e.clientY - d.startY) / d.H) * 100 // drag up → taller
      const pct = Math.min(MAX_PCT, Math.max(DRAG_MIN, raw))
      latestPct.current = pct
      setHeightPct(pct)
    }
    const up = () => {
      window.removeEventListener('pointermove', move)
      window.removeEventListener('pointerup', up)
      window.removeEventListener('pointercancel', up)
      document.body.style.userSelect = ''
      drag.current = null
      const p = latestPct.current
      if (p < REST_MIN) {
        onCloseRef.current() // dragged down into the close zone → dismiss
        return
      }
      const c = clampRest(p)
      latestPct.current = c
      setHeightPct(c)
      try {
        localStorage.setItem(LS_KEY, String(Math.round(c)))
      } catch {
        /* storage unavailable → skip persistence, keep the live height */
      }
    }
    handlers.current = { move, up }
  }

  const onGripPointerDown = (e: ReactPointerEvent) => {
    const H = overlayRef.current?.clientHeight || 1
    drag.current = { startY: e.clientY, startPct: heightPct, H }
    latestPct.current = heightPct
    didDrag.current = false
    document.body.style.userSelect = 'none'
    window.addEventListener('pointermove', handlers.current!.move)
    window.addEventListener('pointerup', handlers.current!.up)
    window.addEventListener('pointercancel', handlers.current!.up)
  }

  useEffect(() => {
    if (!escActive) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [onClose, escActive])

  // Detach a drag in flight if the drawer unmounts mid-gesture.
  useEffect(() => {
    const h = handlers.current!
    return () => {
      window.removeEventListener('pointermove', h.move)
      window.removeEventListener('pointerup', h.up)
      window.removeEventListener('pointercancel', h.up)
      document.body.style.userSelect = ''
    }
  }, [])

  return (
    <div
      className="work-drawer-overlay"
      ref={overlayRef}
      // reset the drag flag at the START of every interaction (a grip drag
      // bubbles here too) so a stale flag from a prior drag that didn't overshoot
      // can't swallow a later plain backdrop click.
      onPointerDown={() => {
        didDrag.current = false
      }}
      onClick={() => {
        // a drag that overshot the clamp lands its synthesized click on THIS
        // overlay (grip↔backdrop common ancestor), not the panel — swallow it so
        // drag-to-max doesn't close the drawer (review WARN #1).
        if (didDrag.current) {
          didDrag.current = false
          return
        }
        onClose()
      }}
    >
      <div
        className="work-drawer-panel"
        role="dialog"
        aria-modal="true"
        style={{ height: `${heightPct}%` }}
        onClick={(e) => e.stopPropagation()}
      >
        <div className="work-drawer-grip" onPointerDown={onGripPointerDown}>
          <span className="work-drawer-handle" />
          <button
            className="icon-btn-sm"
            title="关闭"
            aria-label="关闭详情"
            // don't let a pointerdown on × start a drag on the grip
            onPointerDown={(e) => e.stopPropagation()}
            onClick={onClose}
          >
            <Icon name="x" size={13} />
          </button>
        </div>
        <div className="work-drawer-body scroll-thin">
          <WorkDetail id={id} />
        </div>
      </div>
    </div>
  )
}
