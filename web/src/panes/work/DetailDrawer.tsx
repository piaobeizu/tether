// DetailDrawer — bottom slide-up overlay for the selected wi's detail
// (tether#26). Lives inside the right Work pane, above the Work map: clicking
// a card selects a wi, which mounts this drawer sliding up from the bottom
// with the WorkDetail (goal/status/content + scenario step DAG + action bar).
// Dismiss via the × button, Esc, or clicking the dimmed map backdrop above.
import { useEffect } from 'react'
import { Icon } from '../../lib/icons'
import WorkDetail from './WorkDetail'

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
  useEffect(() => {
    if (!escActive) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [onClose, escActive])

  return (
    <div className="work-drawer-overlay" onClick={onClose}>
      <div
        className="work-drawer-panel"
        role="dialog"
        aria-modal="true"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="work-drawer-grip">
          <span className="work-drawer-handle" />
          <button className="icon-btn-sm" title="关闭" aria-label="关闭详情" onClick={onClose}>
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
