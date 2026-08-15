import { useStore, historyEntryToMessage, type HistoryEntry } from './store'

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

  // Opening the session you are already in is nothing to do — and doing it
  // anyway is destructive: the reconnect below would tear down a live
  // WebTransport mid-turn, and the reload would drop the in-flight turn's
  // bubble. The session list renders the current session highlighted, so that
  // click is an easy one to make. A connection that has genuinely dropped is
  // the error banner / WT pill's job; they dispatch the reconnect themselves.
  if (sid === useStore.getState().sessionId) return

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

  void fetch(`/api/v1/sessions/${encodeURIComponent(sid)}/messages`)
    .then(r => {
      // Tell "this session has no messages" (200 []) apart from "we could not
      // ask" (5xx / offline). Collapsing the two — `r.ok ? r.json() : []` —
      // forces a `msgs.length > 0` guard downstream, which silently keeps the
      // PREVIOUS session's transcript. And loadHistory is also what clears
      // pendingPermissions and the turn cursor (store.ts), so that residue is
      // interactive, not merely stale text: the old session's permission cards
      // stay clickable, and the new session's first delta accumulates into the
      // old session's assistant bubble.
      if (!r.ok) throw new Error(`messages: HTTP ${r.status}`)
      return r.json()
    })
    .then((msgs: HistoryEntry[]) => {
      // Two switches in quick succession: a slower earlier response must not
      // land on top of a later one. Also covers the sid moving underneath us
      // via the resume-fallback (session_ready).
      if (useStore.getState().sessionId !== sid) return
      useStore.getState().loadHistory(msgs.map(historyEntryToMessage))
    })
    // Could not reach the daemon: leave what is on screen readable rather than
    // blanking it. The sid has still moved, and ChatPane's own [sessionId]
    // effect re-requests the same URL (see below), so this self-heals.
    .catch(() => {})

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
