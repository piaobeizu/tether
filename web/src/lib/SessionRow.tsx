import { useStore } from './store'
import { openSession } from './session'
import {
  SESSION_ACTIVITY_HELD,
  SESSION_ACTIVITY_IDLE,
  SESSION_ACTIVITY_WORKING,
  useSessionActivity,
  type SessionActivityState,
} from './sessionActivity'
import { relTime } from './timefmt'
import {
  EXTERNAL_SESSION_BADGE,
  EXTERNAL_SESSION_PROVENANCE,
  RUNNING_ELSEWHERE_BADGE,
  isExternalSession,
  isRunningElsewhere,
  runningElsewhereProvenance,
  sessionLabel,
  type SessionSummary,
} from './wiSession'

/**
 * SessionRow — ONE rendering of "a session you can click" (tether#91).
 *
 * It lives in lib/, next to CopyButton, because two unrelated panes render it:
 * the chat pane's session list and the work-item detail's list of the sessions
 * that worked on that wi. The first draft of this slice had them as two inline
 * copies, and by the time it reached review they had already diverged three ways
 * — one marked the current session and the other did not, one bypassed
 * sessionLabel, one forgot to switch to the chat tab. That is the same drift the
 * module this file calls into was created to end (see openSession's doc: three
 * call sites, two of them quietly broken), reproduced inside the change that
 * cites it. So there is one row.
 *
 * What a row owes its caller:
 *
 *   - a LABEL that is the most useful thing known about the session,
 *   - the CURRENT session marked, because clicking it is a deliberate no-op and
 *     the user should not have to discover that by clicking,
 *   - a click that lands the user in front of the conversation — which means the
 *     chat tab, not just the session id.
 */
export function SessionRow({
  session,
  omitWorkItem = false,
  indent,
}: {
  session: SessionSummary
  /**
   * The caller IS the work item, so repeating it on every row says nothing.
   * Passed as an explicit exception rather than by having the caller build its
   * own label, so the precedence stays in sessionLabel — one function, one place
   * to look, one place to change.
   */
  omitWorkItem?: boolean
  indent?: number
}) {
  const currentSid = useStore(s => s.sessionId)
  const isCurrent = session.sid === currentSid
  // tether#92 — a conversation the coding agent recorded and tether never saw.
  const isExternal = isExternalSession(session)
  // tether#101 — a background agent was using this conversation when the daemon
  // built this list. A HINT: the row stays clickable (see below), and the
  // authoritative answer arrives from the attach path if it is still true.
  const isRunning = isRunningElsewhere(session)
  // tether#103 — is a turn in flight RIGHT NOW? Subscribed here, in the row,
  // rather than fetched by either pane: mounting a row is then the whole of opting
  // in, and one module-level poller serves every row on the page. Refreshed on its
  // own clock (see useSessionActivity), which is what stops the marker being a
  // snapshot of whenever the list was last fetched.
  const activity = useSessionActivity(session.sid)

  const onClick = () => {
    // Chat is a tab in the right column, and this row may be rendered from the
    // middle column (the wi detail) while the right column is showing Skills or
    // Shell. Without this the click would tear down the WebTransport, repoint
    // tether_last_sid and redirect the next prompt with nothing visible
    // happening. Dispatched unconditionally — from inside chat it is a no-op.
    window.dispatchEvent(new CustomEvent('tether:select-tab', { detail: 'chat' }))
    // tether#61 — opening a session is ONE operation and it lives in
    // lib/session.ts. Never reimplemented here, and NOT forked for tether#92
    // either: a read-only session is still a session being opened, and the
    // versions that had their own copy of this had stopped reconnecting the
    // WebTransport channel (so the live stream, and the next prompt sent, stayed
    // on the session the user had just left) and hidden setSessionId behind a
    // non-empty history.
    openSession(session.sid)
    // NOTHING ELSE. An earlier version posted the "tether cannot promise to
    // continue this" line from here, which tied a durable property of the SESSION
    // to a transient event: a reload restores the sid (tether_last_sid) but not
    // the notice, so the warning vanished exactly when the user was most likely to
    // type. The promise now lives with the state that implies it — see
    // EXTERNAL_SESSION_PROMISE and the chat pane's session banner — which also
    // means the wi detail's "open in chat" gets it without remembering to.
  }

  return (
    <div
      className={`tree-row${isCurrent ? ' active' : ''}`}
      style={indent == null ? undefined : { paddingLeft: indent }}
      onClick={onClick}
      title={rowTitle(session, activity)}
    >
      <span className={`ws-dot${isCurrent ? ' live' : ''}`} />
      {/* tether#103. Its own visual channel, next to the "this is the one you are
          looking at" dot rather than among the badges: the badges answer "what kind
          of row is this" (provenance, possession) and this answers "is it moving".
          NOTHING is rendered when the daemon reported no state — absence means
          nothing live holds the session, and an element that says so would be a
          second thing on every row to mean the same as silence. */}
      {activity && (
        <span
          className={`session-row-act ${activity}`}
          role="img"
          aria-label={ACTIVITY_LABELS[activity]}
        />
      )}
      <span className="tree-label">{sessionLabel(session, { omitWorkItem })}</span>
      {/* Provenance, on the row, before the click. One word is all there is room
          for, so it is the one that is TRUE of the row — the promise itself needs
          a sentence and lives in the chat pane's banner, where it survives a
          reload. */}
      {isExternal && <span className="session-row-src mono">{EXTERNAL_SESSION_BADGE}</span>}
      {/* tether#101. NOT a disabled row and NOT a replacement for the click: the
          state is temporary (the job finishes) and an unexplained disabled row is
          worse than a click that explains itself. onClick above is untouched,
          which TestSessionRow's "still opens" case pins. */}
      {isRunning && <span className="session-row-running mono">{RUNNING_ELSEWHERE_BADGE}</span>}
      <span className="session-row-when mono">{sessionWhen(session.updatedAt)}</span>
    </div>
  )
}

/**
 * What each activity state means, in the words the row is willing to stand behind
 * (tether#103).
 *
 * These are the accessible names AND the hover text — one sentence per state,
 * written once. Each one is checkable against the daemon:
 *
 *  - 'working' says "a turn", not "the model is replying". The agent's own status
 *    is `busy` for the whole turn including tool execution, so the narrower claim
 *    would be false for a row three minutes into a test run.
 *  - 'idle' says "no turn in flight" rather than "between turns", because the
 *    daemon also reports it for the agent's `waiting` (blocked on the user) and
 *    `shell` (a shell task while the agent itself is idle).
 *  - 'held' names the limit instead of hiding it. It is what the daemon sends when
 *    a live agent process has the conversation open but wrote no status — every
 *    `--print` launch, which is most of them — so pretending it meant "idle" would
 *    make the row claim nothing is happening on exactly the sessions it cannot
 *    see into.
 */
const ACTIVITY_LABELS: Record<SessionActivityState, string> = {
  [SESSION_ACTIVITY_WORKING]: 'a turn is in flight',
  [SESSION_ACTIVITY_IDLE]: 'a coding agent has this open — no turn in flight',
  [SESSION_ACTIVITY_HELD]:
    'a coding agent has this open — tether cannot see whether a turn is in flight',
}

/**
 * rowTitle is the hover text: the sid, the opening prompt when there is one, and
 * — for a session tether did not record — where it came from.
 *
 * Hover is a bonus, not the channel: there is no hover on a phone, and tether is
 * driven from one by design. Anything a user MUST see is on the row or in the
 * banner; this is for the pointer that happens to be there.
 *
 * The activity sentence goes here as well as on the marker's aria-label, because
 * a coloured dot cannot say which of three things it means and this row already
 * puts the long form of a fact in the hover text (tether#92, #101).
 */
function rowTitle(session: SessionSummary, activity?: SessionActivityState): string {
  const lines = [session.sid]
  if (session.title) lines.push(session.title)
  if (isExternalSession(session)) lines.push(EXTERNAL_SESSION_PROVENANCE)
  if (isRunningElsewhere(session)) lines.push(runningElsewhereProvenance(session))
  if (activity) lines.push(ACTIVITY_LABELS[activity])
  return lines.join('\n')
}

/**
 * sessionWhen formats the row's timestamp, and guards the two inputs that would
 * otherwise render as nonsense.
 *
 * relTime takes an ISO string and promises never to throw — but `new Date(x)
 * .toISOString()` throws RangeError on a non-finite number BEFORE relTime is
 * reached, so calling it naively moves the failure to a place that promise does
 * not cover. And a zero timestamp (the daemon's value when it could not stat the
 * transcript) is a valid date: it renders as "Jan 1, 1970", which reads as
 * information and is not.
 */
export function sessionWhen(updatedAt: number): string {
  if (!Number.isFinite(updatedAt) || updatedAt <= 0) return ''
  return relTime(new Date(updatedAt).toISOString())
}
