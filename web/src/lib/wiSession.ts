import { nextNoticeTs, useStore } from './store'

/**
 * wiSession — THE operation for "this chat session is the one working on that
 * work item" (tether#91).
 *
 * WHY it exists, and why it is one module rather than two lines in WorkDetail.
 *
 * The mapping used to be two `localStorage` calls in WorkDetail.tsx: a
 * `tether_wi_sid:<slug>` key holding one sid. That had four problems, and only
 * the first is about storage:
 *
 *   1. it lived in one browser profile, so a different device — or a cleared
 *      cache — lost it, and the daemon (the thing that outlives both) never knew;
 *   2. it only pointed wi -> session, so nothing could look at a session and say
 *      what it was for, which is exactly the question a readable session list has
 *      to answer;
 *   3. one wi held one sid, overwritten by the next Start;
 *   4. it wrote the empty string when there was no session yet, so "not bound"
 *      and "bound to nothing" were the same recorded value.
 *
 * The daemon now stores the INVERSE — session -> work item, one file per session
 * (PUT /api/v1/sessions/<sid>/wi). That fixes 1 and 2 outright and makes 3 free:
 * a work item's sessions are every session whose record names it, which is a
 * filter over the list the UI already fetches, with no index to keep consistent.
 * 4 is fixed by bindWorkItem below.
 *
 * Everything that binds goes through here, for the reason lib/session.ts's doc
 * gives about opening a session: a rule that lives in one function is structural,
 * and a rule that two call sites are each expected to remember is a rule that will
 * be right in one of them.
 */

/** localStorage key prefix written by the pre-tether#91 forward mapping. */
const LEGACY_PREFIX = 'tether_wi_sid:'

/**
 * Fired after a binding is successfully recorded. The session list listens so a
 * row's label becomes the work item without a reload — which is also what makes
 * "bind a wi, see it in the list" one assertion instead of two.
 */
export const WI_BOUND_EVENT = 'tether:wi-bound'

/**
 * What happened to a binding attempt. Three outcomes and not two, because the
 * two failures need OPPOSITE handling and a boolean cannot tell them apart:
 *
 *   - 'refused'    — the daemon answered, and said no (4xx). Retrying sends the
 *                    same request to the same daemon; it will say no again. The
 *                    caller should stop, and should say so.
 *   - 'unreachable' — offline, or the daemon failed (5xx, including the 503 a
 *                    daemon with no wi store returns). The request is still
 *                    valid; a later attempt may well work, so anything the
 *                    caller was holding should be kept.
 *
 * Collapsing them is what makes a migration key that the daemon will never
 * accept get retried on every page load, forever.
 */
export type BindResult = 'recorded' | 'refused' | 'unreachable'

/** PUT the binding, and announce it when the daemon recorded it. */
export async function putWiBinding(sid: string, workItem: string): Promise<BindResult> {
  // Nothing to send, and the daemon would answer 400: treat it as the refusal it
  // would be, so a caller cannot mistake it for something worth retrying.
  if (!sid || !workItem) return 'refused'
  try {
    const res = await fetch(`/api/v1/sessions/${encodeURIComponent(sid)}/wi`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ workItem }),
    })
    if (!res.ok) return res.status >= 500 ? 'unreachable' : 'refused'
  } catch {
    return 'unreachable'
  }
  window.dispatchEvent(new CustomEvent(WI_BOUND_EVENT, { detail: { sid, workItem } }))
  return 'recorded'
}

/**
 * armed holds the work item waiting for a session to exist, and the unsubscribe
 * that will deliver it. Module-level rather than component state because the
 * component that starts the wait (the wi detail pane) is unmounted the moment the
 * user is switched to the chat tab, which is the same click.
 */
let armed: { workItem: string; off: () => void } | null = null

function disarm() {
  armed?.off()
  armed = null
}

/**
 * bindWorkItem records that the CURRENT session is working on `workItem`.
 *
 * When there is no session yet it arms instead of writing, and the first session
 * id to appear claims the binding. That is the fix for problem 4 above: the old
 * code wrote `sessionId ?? ''` at this exact moment, so clicking Start before a
 * session existed recorded a mapping to nothing — and `resumeWi`'s `if (sid)`
 * then quietly fell through to the "no mapping" branch forever.
 *
 * The wait ends at the FIRST session id, whatever produced it. In the flow this
 * is built for that is the session the Start click is about to create. It is not
 * airtight: while armed, deliberately opening some OTHER existing session from
 * the list would claim the binding instead. That is accepted rather than closed by
 * hooking `openSession`, because the alternative is a second opinion inside the
 * one function this codebase has already had to consolidate once (tether#61) — and
 * the window is one click wide. A later Start replaces the armed work item rather
 * than queueing, so the last thing the user asked for is the one that lands.
 */
export function bindWorkItem(workItem: string): void {
  if (!workItem) return
  disarm()

  const sid = useStore.getState().sessionId
  if (sid) {
    void bindAndReport(sid, workItem)
    return
  }

  const off = useStore.subscribe((state) => {
    const next = state.sessionId
    if (!next) return
    // Read the work item before disarming — disarm() clears `armed`.
    const pending = armed?.workItem
    disarm()
    if (pending) void bindAndReport(next, pending)
  })
  armed = { workItem, off }
}

/**
 * bindAndReport records the binding and, if that failed, SAYS SO.
 *
 * Not `void putWiBinding(...)`. A dropped binding has no symptom of its own: the
 * next "Open in chat" simply falls back to injecting `/pf-work <slug> --resume`,
 * which is also what a wi nobody has started does. That silent-fallback shape is
 * precisely the bug this whole slice replaces (the old writer stored the empty
 * string and nothing ever said so), and fixing the cause while keeping the
 * silence would leave the failure just as hard to see.
 *
 * The notice goes into the chat transcript because that is where the user is
 * standing: Start switches to the chat tab in the same click. It is stamped with
 * nextNoticeTs so it sorts after everything already on screen, and carries no
 * `kind` — none of the three classes' repeat rules apply to it (see Notice.kind).
 */
async function bindAndReport(sid: string, workItem: string): Promise<void> {
  const result = await putWiBinding(sid, workItem)
  if (result === 'recorded') return
  const text = result === 'refused'
    ? `could not link this session to ${workItem} — the daemon rejected it`
    : `could not link this session to ${workItem} — the daemon could not be reached`
  useStore.setState((s) => ({
    notices: [...s.notices, { id: crypto.randomUUID(), text, ts: nextNoticeTs(s.messages) }],
  }))
}

/** Test seam: drop any pending binding. Not used by the app. */
export function resetArmedBinding(): void {
  disarm()
}

/**
 * migrated is module-level so React's StrictMode double-mount — or two components
 * both wanting the migration to have happened — cannot run it twice. Two runs
 * would be harmless in outcome (the second finds no keys) but would double the
 * requests on the one load where there is something to move.
 */
let migrated = false

/**
 * migrateLegacyWiSessions moves any `tether_wi_sid:<slug>` keys this browser still
 * holds onto the daemon, then deletes them. Returns how many bindings were moved.
 *
 * Three cases, and the middle one is the common one:
 *
 *   - a key with a sid  → PUT it, then delete the key — UNLESS the daemon could
 *     not be reached, in which case the key stays and the next load retries.
 *     A key the daemon REFUSED is deleted too: a 400 is a verdict, not a hiccup,
 *     and keeping it means re-sending the same rejected request on every page
 *     load for the life of the profile.
 *   - NO keys at all    → do nothing, and in particular issue NO requests. Almost
 *     every browser is in this state, and a migration that pings the daemon on
 *     every load to discover it has nothing to do is a migration that never ends.
 *   - a key holding ''  → delete it, no request. The old writer produced these
 *     whenever Start was clicked with no session; there is nothing to migrate,
 *     and sending it would just be a 400.
 *
 * Best-effort by design: this is bookkeeping for data the user can recreate by
 * clicking Start again, so nothing here surfaces an error. (bindWorkItem does
 * report its failures — the difference is that there a human just asked for the
 * thing and is watching.)
 */
export async function migrateLegacyWiSessions(): Promise<number> {
  if (migrated) return 0
  migrated = true

  const legacy: { key: string; slug: string; sid: string }[] = []
  for (let i = 0; i < localStorage.length; i++) {
    const key = localStorage.key(i)
    if (!key?.startsWith(LEGACY_PREFIX)) continue
    legacy.push({ key, slug: key.slice(LEGACY_PREFIX.length), sid: localStorage.getItem(key) ?? '' })
  }
  if (legacy.length === 0) return 0

  let moved = 0
  for (const entry of legacy) {
    if (!entry.sid || !entry.slug) {
      localStorage.removeItem(entry.key)
      continue
    }
    const result = await putWiBinding(entry.sid, entry.slug)
    if (result === 'unreachable') continue // keep the key; try again next load
    localStorage.removeItem(entry.key)
    if (result === 'recorded') moved++
  }
  return moved
}

/** Test seam: let the once-guard run again. Not used by the app. */
export function resetMigrationForTests(): void {
  migrated = false
}

/** One row of GET /api/v1/sessions — mirrors session.SessionSummary (Go). */
export interface SessionSummary {
  sid: string
  workItem?: string
  title?: string
  updatedAt: number
}

/**
 * fetchSessions returns the daemon's session list, newest first.
 *
 * The order is the DAEMON's and is never re-sorted here. That is not fussiness:
 * the list this replaces sorted client-side with `[...sessions].reverse()` over a
 * response that was in UUID-filename order, which looked like "newest first" and
 * was close to random. One owner for the ordering contract, and it is the side
 * that has the timestamps.
 */
export async function fetchSessions(): Promise<SessionSummary[]> {
  const res = await fetch('/api/v1/sessions')
  if (!res.ok) throw new Error(`sessions: HTTP ${res.status}`)
  const rows = await res.json() as SessionSummary[]
  return Array.isArray(rows) ? rows : []
}

/**
 * sessionLabel is what a row shows: the work item a human attached, else the
 * session's own first prompt, else the sid.
 *
 * The precedence lives in the frontend, with both fields on the wire, so the
 * daemon does not have to guess what a list wants to display — and so a session
 * that loses its binding still reads as something.
 *
 * `omitWorkItem` is for the one caller that IS a work item: on a wi's detail
 * page, labelling every row with the wi the page is already about says nothing.
 * It is a parameter here rather than a different expression at the call site so
 * that the exception is NAMED and the precedence stays in one function — the
 * first draft of that caller wrote `s.title || sessionLabel(s)`, which silently
 * inverted the order for a session whose title happened to be empty.
 */
export function sessionLabel(s: SessionSummary, o?: { omitWorkItem?: boolean }): string {
  const wi = o?.omitWorkItem ? '' : s.workItem
  return wi || s.title || `${s.sid.slice(0, 16)}…`
}

/** The sessions bound to one work item, newest first (the daemon's order). */
export function sessionsForWorkItem(rows: SessionSummary[], workItem: string): SessionSummary[] {
  if (!workItem) return []
  return rows.filter(s => s.workItem === workItem)
}
