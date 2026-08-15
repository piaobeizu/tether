import { useStore } from './store'
import { openSession } from './session'
import { relTime } from './timefmt'
import { sessionLabel, type SessionSummary } from './wiSession'

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

  const onClick = () => {
    // Chat is a tab in the right column, and this row may be rendered from the
    // middle column (the wi detail) while the right column is showing Skills or
    // Shell. Without this the click would tear down the WebTransport, repoint
    // tether_last_sid and redirect the next prompt with nothing visible
    // happening. Dispatched unconditionally — from inside chat it is a no-op.
    window.dispatchEvent(new CustomEvent('tether:select-tab', { detail: 'chat' }))
    // tether#61 — opening a session is ONE operation and it lives in
    // lib/session.ts. Never reimplemented here: the versions that were had
    // stopped reconnecting the WebTransport channel (so the live stream, and the
    // next prompt sent, stayed on the session the user had just left) and hidden
    // setSessionId behind a non-empty history.
    openSession(session.sid)
  }

  return (
    <div
      className={`tree-row${isCurrent ? ' active' : ''}`}
      style={indent == null ? undefined : { paddingLeft: indent }}
      onClick={onClick}
      title={session.title ? `${session.sid}\n${session.title}` : session.sid}
    >
      <span className={`ws-dot${isCurrent ? ' live' : ''}`} />
      <span className="tree-label">{sessionLabel(session, { omitWorkItem })}</span>
      <span className="session-row-when mono">{sessionWhen(session.updatedAt)}</span>
    </div>
  )
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
