import { useCallback, useEffect, useRef, useState } from 'react'
import { Icon } from '../../lib/icons'
import { SessionRow } from '../../lib/SessionRow'
import { useStore } from '../../lib/store'
import {
  WI_BOUND_EVENT,
  fetchSessions,
  migrateLegacyWiSessions,
  type SessionSummary,
} from '../../lib/wiSession'

/**
 * SessionList — the chat pane's "which conversation am I in, and what else is
 * there" list (tether#91).
 *
 * # Why it is here and not in the workspace pane
 *
 * It used to hang off the BOTTOM OF THE FILE TREE, which is a category error: the
 * left column answers "which files", and a session is not a file. It was there for
 * historical reasons only. Chat is where a session is a first-class thing, so this
 * is where the list belongs — and it is deliberately NOT an activity-bar entry:
 * tether#90's rule is that the bar names independent middle-column surfaces, and
 * this one is an attachment to chat.
 *
 * The old list is DELETED, not left in place. Two lists over one endpoint drift,
 * and this repo has the receipts: lib/session.ts exists because three call sites
 * each grew their own "open a session" and two of them were broken.
 *
 * # Why it is its own file
 *
 * panes/chat/index.tsx is ~1450 lines. This one has its own fetch, its own state
 * and no dependency on the connection machinery, so mounting it costs that file
 * one import and one line of JSX.
 *
 * # What a row says
 *
 * The work item when the session has one, else the session's own opening prompt,
 * else the sid — see sessionLabel. That precedence is the whole point of inverting
 * the wi mapping: with wi -> session there was no way to look at a session and
 * name it. The row itself is lib/SessionRow, shared with the wi detail pane,
 * because two hand-written copies of it had already diverged by the end of the
 * first draft of this change.
 *
 * # Order
 *
 * Exactly the order the daemon sent. The previous version applied
 * `[...sessions].reverse()` to a response that was in UUID-filename order, which
 * reads as "newest first" and is not. Sorting is the daemon's job because the
 * daemon has the timestamps; re-sorting here would restore two owners for one
 * contract.
 */
export default function SessionList() {
  const [rows, setRows] = useState<SessionSummary[]>([])
  const [open, setOpen] = useState(false)
  const currentSid = useStore(s => s.sessionId)
  // What the last successful fetch returned, readable without making `rows` a
  // dependency of the effect that decides whether to fetch again.
  const known = useRef<SessionSummary[]>([])
  const settled = useRef(false)

  const load = useCallback(async (alive: () => boolean) => {
    try {
      const next = await fetchSessions()
      if (alive()) {
        known.current = next
        setRows(next)
      }
    } catch {
      // A list we could not fetch leaves the previous one on screen. There is
      // nothing here the user must act on, and the chat below it still works.
    } finally {
      settled.current = true
    }
  }, [])

  // First load. The legacy-mapping migration is awaited FIRST so the very first
  // list already carries the work items it moved, rather than showing bare sids
  // for a moment and rewriting itself.
  useEffect(() => {
    let ok = true
    void (async () => {
      await migrateLegacyWiSessions()
      await load(() => ok)
    })()
    return () => { ok = false }
  }, [load])

  // A session id we have never listed means a NEW session exists; refetch so it
  // gets a row.
  //
  // Deliberately not "refetch on every sessionId change". Switching between
  // sessions the list already contains changes only which row is highlighted, and
  // that is derived from currentSid at render time — no request needed. The
  // daemon's answer costs a directory scan plus a stat and a bounded read PER
  // SESSION (session.SessionIndex.List), so re-running it on every click through
  // a 90-session history is real work for no new information.
  useEffect(() => {
    if (!currentSid || !settled.current) return
    if (known.current.some(r => r.sid === currentSid)) return
    let ok = true
    void load(() => ok)
    return () => { ok = false }
  }, [currentSid, load])

  // A binding just landed — including one this component's own migration wrote.
  // Without this the row for the session you just started work in keeps showing
  // its old label until something else happens to refetch.
  useEffect(() => {
    let ok = true
    const onBound = () => { void load(() => ok) }
    window.addEventListener(WI_BOUND_EVENT, onBound)
    return () => { ok = false; window.removeEventListener(WI_BOUND_EVENT, onBound) }
  }, [load])

  if (rows.length === 0) return null

  return (
    <div className="chat-sessions">
      <div
        className="chat-sessions-head"
        onClick={() => setOpen(o => !o)}
        role="button"
        tabIndex={0}
        aria-expanded={open}
        onKeyDown={e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); setOpen(o => !o) } }}
      >
        <Icon name={open ? 'chev-down' : 'chevron'} size={11} style={{ color: 'var(--ink-quat)', flexShrink: 0 }} />
        <span className="section-label">Sessions</span>
        <span className="chat-sessions-count mono">{rows.length}</span>
      </div>
      {open && (
        <div className="chat-sessions-list scroll-thin">
          {rows.map(s => <SessionRow key={s.sid} session={s} indent={14} />)}
        </div>
      )}
    </div>
  )
}
