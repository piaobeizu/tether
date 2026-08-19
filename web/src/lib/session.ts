import { useStore, historyEntryToMessage, type HistoryEntry } from './store'
import { noteTranscriptVersion, readTranscriptVersion, transcriptPath } from './transcriptWatch'

/**
 * The window event a click on the ALREADY-OPEN session raises (tether#106).
 *
 * Deliberately an offer, not an instruction. `openSession` cannot know whether that
 * click has anything to do: whether a live WebTransport exists is ChatPane's fact — it
 * owns the connection — and ChatPane ignores this event in every state where there is
 * one. Same channel and same argument as `tether:retry-connection`, which this module
 * already uses rather than reaching into the pane.
 */
export const REFRESH_TRANSCRIPT_EVENT = 'tether:refresh-transcript'

/**
 * refreshTranscript re-reads one session's transcript into the store.
 *
 * The loader `openSession` has always had, extracted so the two callers cannot drift
 * (which is the whole argument openSession's own doc makes about the switch). Its
 * guards are stated where they are decided:
 *
 *   - `!r.ok` THROWS rather than falling back to `[]`, so "this session has no messages"
 *     (200 []) stays distinguishable from "we could not ask" (5xx / offline). Collapsing
 *     the two forces a `msgs.length > 0` guard downstream, which silently keeps the
 *     PREVIOUS session's transcript — and `loadHistory` is also what clears
 *     pendingPermissions and the turn cursor, so that residue is interactive, not merely
 *     stale text.
 *   - The sid is re-checked after the await, because two loads can be in flight and a
 *     slower earlier one must not land on top of a later one.
 *   - There is NO `!streaming` guard, and its absence is deliberate rather than
 *     overlooked. ChatPane's `[sessionId]` effect has one (tether#42) so that
 *     session_ready's refetch cannot wipe an in-flight turn's optimistic bubble. Here
 *     both callers are states in which a turn cannot be in flight — a deliberate switch
 *     means the user has left that turn, and a session a background agent holds has no
 *     stream to have a turn on — so the check would never be false. An inert guard is
 *     worse than none: it reads like protection at the exact place a future caller would
 *     look for it.
 *
 * A failure leaves what is on screen readable rather than blanking it.
 */
export function refreshTranscript(sid: string): Promise<void> {
  if (!sid) return Promise.resolve()
  return fetch(transcriptPath(sid))
    .then(r => {
      if (!r.ok) throw new Error(`messages: HTTP ${r.status}`)
      // Read BEFORE the body: this is the version the messages below came from, and
      // recording it is what lets the next probe compare against something real
      // instead of establishing its own baseline and losing everything written in
      // between (see transcriptWatch.noteTranscriptVersion).
      const version = readTranscriptVersion(r)
      return r.json().then((msgs: HistoryEntry[]) => ({ version, msgs }))
    })
    .then(({ version, msgs }: { version: number; msgs: HistoryEntry[] }) => {
      if (useStore.getState().sessionId !== sid) return
      useStore.getState().loadHistory(msgs.map(historyEntryToMessage))
      noteTranscriptVersion(sid, version)
    })
    .catch(() => {})
}

/**
 * openSession — THE operation for "the user deliberately opened a different
 * session" (tether#61). Every call site imports it: WorkDetail's click-to-work
 * "resume", and lib/SessionRow, which is the one row rendered by both the chat
 * pane's session list and the wi detail's list of that wi's sessions.
 *
 * (Before tether#91 the list lived at the bottom of the WorkspacePane file tree,
 * which is what the previous version of this sentence named. Keeping the pointer
 * accurate matters more here than usual, because this comment is the record of
 * WHY there is only one of these.)
 *
 * WHY it exists. Those call sites used to each implement the switch inline, and
 * they had drifted apart in ways that are invisible at a glance:
 *
 *   - The workspace list never reconnected the WebTransport channel, so after
 *     switching, the live channel — and therefore the NEXT PROMPT the user
 *     typed — still went to the session they had just left. Losing tokens would
 *     have been bad enough; misrouting writes is the real damage.
 *   - `setSessionId` is what persists `tether_last_sid` (store.ts), and the
 *     workspace list called it INSIDE an `msgs.length > 0` guard. So a target
 *     with no history — or merely a /messages request that failed — left the
 *     sid unchanged and unpersisted, i.e. the whole switch became a no-op that
 *     had nonetheless already discarded the notices.
 *
 * The point of collapsing them into one function is that the invariant is now
 * STRUCTURAL rather than a convention two files are each expected to remember —
 * the same reason tether#57 moved `notices` into their own store slice instead
 * of re-timing the refetch that ate them.
 *
 * Order is load-bearing: the sid must be persisted BEFORE the reconnect is
 * requested, because ChatPane's doConnect builds its `?sid=` from
 * `tether_last_sid`. Reconnecting first would resume the OLD session.
 */
export function openSession(sid: string): void {
  if (!sid) return

  // Opening the session you are already in is NOT A SWITCH — and performing the
  // switch anyway is destructive: the reconnect below would tear down a live
  // WebTransport mid-turn, and the reload would drop the in-flight turn's
  // bubble. The session list renders the current session highlighted, so that
  // click is an easy one to make. A connection that has genuinely dropped is
  // the error banner / WT pill's job; they dispatch the reconnect themselves.
  //
  // tether#106 — but "not a switch" is not the same as "nothing", and reading it
  // as nothing is what left a session a background agent holds frozen at the
  // moment it was opened. That session has no live stream to protect (tether#101
  // refuses the attach), so the transcript below it is a still frame and the one
  // thing the click can still mean is "re-read it". This function does not get
  // to decide that: whether a live stream exists is ChatPane's fact, so the click
  // is OFFERED on the same window-event channel the reconnect uses, and ChatPane
  // ignores it in every state where there is something to protect.
  //
  // Everything below this line still does not happen: no clearNotices, no
  // setSessionId, no reconnect, no unconditional reload. That is the part
  // SessionRow's misclick depends on, and the part
  // "is a no-op when that session is already open" pins.
  if (sid === useStore.getState().sessionId) {
    window.dispatchEvent(new CustomEvent(REFRESH_TRANSCRIPT_EVENT))
    return
  }

  // tether#57 — a notice describes the session you are LEAVING, so a deliberate
  // switch retires it. Cleared here, synchronously, NOT in the .then below: a
  // notice arriving while the fetch is in flight belongs to the session being
  // opened. And it must NOT be folded into setSessionId/loadHistory, because
  // the resume-fallback path changes the sid too, and clearing there would wipe
  // the very notice explaining why it changed.
  useStore.getState().clearNotices()

  // Also writes tether_last_sid (store.ts setSessionId) — deliberately NOT
  // guarded on the history below, so a session with nothing in it, or a
  // /messages request that fails, still switches.
  useStore.getState().setSessionId(sid)

  // The load itself lives in refreshTranscript (tether#106) — same request, same
  // guards, one copy. Could not reach the daemon: it leaves what is on screen
  // readable rather than blanking it. The sid has still moved, and ChatPane's own
  // [sessionId] effect re-requests the same URL (see below), so this self-heals.
  void refreshTranscript(sid)

  // NOTE on the second request: setSessionId re-fires ChatPane's [sessionId]
  // effect, which fetches the same URL again. That effect is guarded on
  // `!streaming` and this one deliberately is not — tether#42's guard exists so
  // that session_ready's refetch cannot wipe an in-flight turn's optimistic
  // bubble, whereas a DELIBERATE switch means the user has left that turn and
  // its bubble must go. So the effect cannot own this load, and the duplicate
  // request is the price of that difference. (It is also the self-heal above.)

  // Rebind the WebTransport channel to the new sid. ChatPane owns the
  // connection, so this goes over the app's existing "reconnect now" channel
  // (the same one App's error banner and catch-up modal use) rather than
  // reaching into the pane. ChatPane is mounted for the whole app lifetime
  // (App.tsx renders it unconditionally and only hides it with display:none),
  // so the listener is always there; even if it were not, the sid is already
  // persisted, so the next mount's doConnect would resume the right session.
  window.dispatchEvent(new CustomEvent('tether:retry-connection'))
}
