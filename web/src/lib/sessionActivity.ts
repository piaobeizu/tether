import { useEffect, useState } from 'react'

/**
 * sessionActivity — "is a turn in flight on this conversation right now?"
 * (tether#103), and the ONE poller that keeps the answer from freezing.
 *
 * # Why a poller exists at all, and why it is the hard part
 *
 * Before this, the session list had NO refresh of any kind. `fetchSessions` ran on
 * mount and on `WI_BOUND_EVENT`, and nothing else in the app polled anything
 * except the chat countdown and the control-channel heartbeat. A marker painted
 * once at mount would be a marker that is right for a second and then lies — worse
 * than no marker, because a stale indicator is indistinguishable from a working
 * one. So the refresh is not an add-on to this feature; it is most of it.
 *
 * # Why its own endpoint and not a poll of /api/v1/sessions
 *
 * That route reads a bounded transcript prefix PER SESSION — ~1.4 MB at the ~90
 * sessions a real profile has — to derive titles that cannot change between two
 * polls. This one costs the daemon a single scan of the agent's live-session
 * registry (~3 ms) and a walk of its own session map. See
 * session.SessionActivityPath (Go) for the whole cost argument.
 *
 * # Why ONE module-level poller instead of one per row
 *
 * `SessionRow` is rendered by two unrelated panes (the chat session list and the
 * work-item detail), and a list has many rows. A timer per row would multiply the
 * request rate by the number of rows AND give two mounted panes two independent
 * answers that can disagree on screen. So there is one interval for the whole
 * page, reference-counted: it starts when the first consumer subscribes and stops
 * when the last one unsubscribes. "No consumer mounted ⇒ no polling" is therefore
 * a property of the mechanism rather than a rule either pane has to remember —
 * which is the same argument lib/session.ts's `openSession` doc makes about rules
 * that live in one place.
 */

/**
 * ONE state per session, and the fourth answer is ABSENCE from the map.
 *
 * Absence is not a synonym for `idle`, and conflating them would throw away the
 * strongest thing the daemon knows. `idle` means a process is holding the
 * conversation and is not in a turn. Absent means NOTHING live holds the session id
 * at all, so it is necessarily not running — a conclusion, not a report.
 *
 * The literals are the wire contract, mirrored from Go by hand.
 * `TestSessionActivityContractIsMirroredInTypeScript` (internal/session/activity_test.go)
 * reads this file and requires every `SessionActivity*` constant the daemon
 * declares to appear here as a quoted literal, because nothing else would notice:
 * a rename on this side alone type-checks cleanly, passes every typed-fixture test,
 * and silently stops matching what the daemon sends. That exact failure was
 * measured in tether#101 — `tsc -b` exit 0, 24 tests green, feature dead.
 */
export type SessionActivityState =
  | typeof SESSION_ACTIVITY_WORKING
  | typeof SESSION_ACTIVITY_IDLE
  | typeof SESSION_ACTIVITY_HELD

/**
 * A turn is in flight.
 *
 * Declared WITHOUT a `: SessionActivityState` annotation on purpose: the union is
 * derived from these three literals (see SessionActivityState above), so annotating
 * them would widen each one back to the union and make `Record<SessionActivityState,
 * …>` uncheckable — which is what turns the label table in SessionRow from an
 * exhaustive map into three optional keys.
 *
 * Deliberately NOT "the model is emitting output", which is what was asked for and
 * is not obtainable: the agent's registry reports `busy` for the whole turn, tool
 * execution included, so that sentence is false for a row whose agent is three
 * minutes into a test run. This one is true of both kinds of session the list
 * contains, which is the requirement — see session/activity.go for the long form.
 */
export const SESSION_ACTIVITY_WORKING = 'working'

/**
 * A process has this conversation open and no turn is in flight.
 *
 * "No turn in flight", not "between turns": this is also the state the daemon
 * reports for the agent's `waiting` (mid-conversation, blocked on the user) and
 * `shell` (a shell task running while the agent itself is idle), and "between
 * turns" would be false for both.
 */
export const SESSION_ACTIVITY_IDLE = 'idle'

/**
 * A live agent process has this conversation open, and whether a turn is in flight
 * is NOT OBSERVABLE.
 *
 * Not called `unknown`, because "a coding agent has this open" is a fact — only the
 * inside of it is opaque. It is also where a status value this build has not been
 * taught lands, so an unfamiliar word degrades to "cannot tell" rather than to a
 * claim in either direction.
 */
export const SESSION_ACTIVITY_HELD = 'held'

/**
 * The endpoint. A top-level path, NOT a leaf under `/api/v1/sessions/` — see
 * session.SessionActivityPath (Go) for why, and for the neighbour that is the real
 * hazard (`/api/v1/session/`, singular, is a prefix handler one hyphen away).
 */
export const SESSION_ACTIVITY_PATH = '/api/v1/session-activity'

/** How often the state is refreshed while at least one row is on screen. */
export const SESSION_ACTIVITY_POLL_MS = 3000

/** sid -> state. A sid nothing holds is absent. */
export type SessionActivityMap = Record<string, SessionActivityState>

/**
 * fetchSessionActivity returns the daemon's answer, or throws.
 *
 * Unknown state strings are DROPPED rather than passed through. The alternative —
 * letting an unrecognised value reach the UI — would render as no marker anyway,
 * but it would do so by accident: this way `SessionActivityState` is true of
 * everything in the map, and a future daemon state shows up as a row with no
 * marker instead of as a class name nobody styled.
 */
export async function fetchSessionActivity(): Promise<SessionActivityMap> {
  const res = await fetch(SESSION_ACTIVITY_PATH)
  if (!res.ok) throw new Error(`session activity: HTTP ${res.status}`)
  const body: unknown = await res.json()
  // `null` and an array are both `typeof 'object'`; neither is the map shape.
  if (body === null || typeof body !== 'object' || Array.isArray(body)) return {}
  const known = new Set<string>([
    SESSION_ACTIVITY_WORKING,
    SESSION_ACTIVITY_IDLE,
    SESSION_ACTIVITY_HELD,
  ])
  const out: SessionActivityMap = {}
  for (const [sid, state] of Object.entries(body as Record<string, unknown>)) {
    if (typeof state === 'string' && known.has(state)) out[sid] = state as SessionActivityState
  }
  return out
}

// ---------------------------------------------------------------------------
// The single shared poller.
// ---------------------------------------------------------------------------

let current: SessionActivityMap = {}
let subscribers = new Set<(m: SessionActivityMap) => void>()
let timer: ReturnType<typeof setInterval> | null = null
let visibilityBound = false
let inFlight = false

function publish(next: SessionActivityMap) {
  current = next
  for (const fn of subscribers) fn(current)
}

async function poll(): Promise<void> {
  // One request at a time. Without this, a daemon slower than the interval would
  // accumulate overlapping requests and the newest answer could be overwritten by
  // an older one that happened to land last.
  if (inFlight) return
  inFlight = true
  try {
    publish(await fetchSessionActivity())
  } catch {
    // A poll we could not complete leaves the previous answer on screen and
    // retries on the next tick — the same policy SessionList.load applies to the
    // list itself, for the same reason: there is nothing here the user must act
    // on, and a connection this broken already has its own indicator.
  } finally {
    inFlight = false
  }
}

/**
 * hidden reports whether the tab is currently in the background.
 *
 * Guarded rather than read directly because `document.visibilityState` is not
 * guaranteed in every environment this bundle is loaded in (a jsdom-less test
 * runner, a non-browser import), and the safe default is "visible" — polling when
 * we cannot tell is a cost, whereas not polling when we cannot tell is a frozen
 * marker.
 */
function hidden(): boolean {
  return typeof document !== 'undefined' && document.visibilityState === 'hidden'
}

/**
 * Restart the interval to match the current state of the world.
 *
 * Idempotent, and called from every edge that can change the answer: a subscriber
 * arriving or leaving, and the tab being hidden or shown.
 */
function reschedule() {
  const wanted = subscribers.size > 0 && !hidden()
  if (wanted && timer === null) {
    timer = setInterval(() => { void poll() }, SESSION_ACTIVITY_POLL_MS)
  } else if (!wanted && timer !== null) {
    clearInterval(timer)
    timer = null
  }
}

function onVisibilityChange() {
  reschedule()
  // Coming BACK to the tab refetches immediately rather than waiting out the
  // interval. Without this, returning to a backgrounded tab shows a marker up to
  // one whole poll stale — which is the exact defect the pause introduces if it is
  // implemented without this half, and it looks like the frozen marker the pause
  // was traded for.
  if (subscribers.size > 0 && !hidden()) void poll()
}

/**
 * subscribeSessionActivity registers fn and returns its unsubscribe.
 *
 * Exported for the tests and for any future non-React consumer; components use
 * `useSessionActivity` below.
 */
export function subscribeSessionActivity(fn: (m: SessionActivityMap) => void): () => void {
  subscribers.add(fn)
  if (!visibilityBound && typeof document !== 'undefined') {
    document.addEventListener('visibilitychange', onVisibilityChange)
    visibilityBound = true
  }
  // The first subscriber gets an immediate answer rather than waiting a full
  // interval for the row to mean anything.
  if (subscribers.size === 1) void poll()
  reschedule()
  return () => {
    subscribers.delete(fn)
    reschedule()
  }
}

/**
 * useSessionActivity returns the state of one session, or undefined when nothing
 * live holds it.
 *
 * The hook is where the subscription lives — in the ROW rather than in either
 * pane — so that mounting a row is the whole of opting in. Putting it in the two
 * parents instead would mean a third caller of `SessionRow` silently renders rows
 * that never update, which is precisely the class of omission this repo has paid
 * for before (see SessionRow's own doc on the three-way divergence that created
 * it).
 */
export function useSessionActivity(sid: string): SessionActivityState | undefined {
  const [map, setMap] = useState<SessionActivityMap>(current)
  useEffect(() => subscribeSessionActivity(setMap), [])
  return map[sid]
}

/**
 * Test seam: drop every subscriber, stop the timer and forget the last answer.
 *
 * Module state outlives a component tree, so without this one test file's poller
 * would still be running — and its `fetch` still being counted — inside the next
 * one. Not used by the app.
 */
export function resetSessionActivityForTests(): void {
  subscribers = new Set()
  if (timer !== null) {
    clearInterval(timer)
    timer = null
  }
  if (visibilityBound && typeof document !== 'undefined') {
    document.removeEventListener('visibilitychange', onVisibilityChange)
    visibilityBound = false
  }
  current = {}
  inFlight = false
}

/**
 * Test seam: is a timer currently running, and how many consumers are subscribed?
 *
 * Exposed so a test can assert the SHARING itself — that two mounted rows produce
 * one timer, and that the last unmount stops it. Asserting only on request counts
 * would leave "the timer is still running with nobody listening" invisible, and
 * that leak is the failure mode a reference count exists to prevent.
 */
export function sessionActivityPollerState(): { running: boolean; subscribers: number } {
  return { running: timer !== null, subscribers: subscribers.size }
}
