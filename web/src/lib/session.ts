import { useStore, historyEntryToMessage, type HistoryEntry } from './store'
import { authedFetch } from './auth'
import { noteTranscriptVersion, readTranscriptBounds, readTranscriptVersion, transcriptPath } from './transcriptWatch'

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

/** The load currently in flight, so two requests for one sid cannot overlap. */
let inFlightSid: string | null = null
let inFlightLoad: Promise<void> | null = null

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
 *   - The sid is re-checked after the await, so a load for a session the user has
 *     already left cannot land on the one they are now looking at.
 *   - A second call for the SAME sid joins the first instead of racing it. That was not
 *     needed before tether#106, because the only caller was a switch and two in-flight
 *     loads were therefore for different sids by construction. Now three callers share
 *     one sid — the watcher's reload, the click on the open row, and "Check again" —
 *     and two overlapping loads can settle in either order, so the older one could land
 *     last and take `noteTranscriptVersion` with it, leaving the recorded version
 *     describing neither what is on screen nor what the daemon has.
 *   - There is NO `!streaming` guard, and its absence is deliberate rather than
 *     overlooked. ChatPane's `[sessionId]` effect has one (tether#42) so that
 *     session_ready's refetch cannot wipe an in-flight turn's optimistic bubble. Here
 *     every caller is a state in which a turn cannot be in flight — a deliberate switch
 *     means the user has left that turn, and a session a background agent holds has no
 *     stream to have a turn on — so the check would never be false. An inert guard is
 *     worse than none: it reads like protection at the exact place a future caller would
 *     look for it.
 *
 * A failure leaves what is on screen readable rather than blanking it. `authedFetch`
 * rather than `fetch` because this now runs repeatedly for as long as a held session is
 * open: with a bare fetch, a session cookie that expires turns the whole mechanism into
 * a silent loop of 401s while the card keeps promising the transcript is being re-read.
 */
export function refreshTranscript(sid: string): Promise<void> {
  if (!sid) return Promise.resolve()
  if (inFlightSid === sid && inFlightLoad) return inFlightLoad
  const load = authedFetch(transcriptPath(sid))
    .then(r => {
      if (!r.ok) throw new Error(`messages: HTTP ${r.status}`)
      // Off THIS response. The version and the messages come from one request, so the
      // version describes a file this call actually read — a version taken from a
      // separate probe would describe whatever the file was at some other moment.
      // (It is a LOWER BOUND on the body's age, not an identity: the daemon stats
      // before it reads, so a write landing in between makes the body newer than the
      // header. That direction costs one redundant reload; the other direction would
      // lose a write outright. See session_api.go.)
      const version = readTranscriptVersion(r)
      // tether#107 — off the SAME response, for the same reason as the version. These
      // describe the page in the body; taken from any other request they would describe
      // a different one.
      const bounds = readTranscriptBounds(r)
      return r.json().then((msgs: HistoryEntry[]) => ({ version, bounds, msgs }))
    })
    .then(({ version, bounds, msgs }: { version: number; bounds: ReturnType<typeof readTranscriptBounds>; msgs: HistoryEntry[] }) => {
      const store = useStore.getState()
      if (store.sessionId !== sid) return
      const next = msgs.map(historyEntryToMessage)
      // tether#107 — this is the NEWEST page. Whether it may replace the array depends
      // on whether the reader has paged back, and only the store knows that.
      //
      // `pagesBack === 0` keeps the byte-for-byte behaviour every caller had before:
      // the wholesale server-truth replace, which is what a deliberate session switch
      // needs. Above zero, replacing would throw away pages the reader loaded on
      // purpose — every three seconds, while they read them — so the page is merged in
      // instead, and `mergeHistory` reporting disjoint windows sends us back to the
      // replace because a visible jump beats an invisible hole.
      const pagesBack = store.transcriptPagesBack
      const merged = pagesBack > 0 && store.mergeHistory(next)
      if (!merged) store.loadHistory(next)
      // The CURSOR is kept on a successful merge and taken on a replace, and getting
      // this backwards is a real defect rather than untidiness: the cursor in the store
      // describes the OLDEST page on screen, while this response's cursor describes the
      // newest. Overwriting it with the newest page's would make the next "load earlier"
      // jump forward and re-serve pages the reader is already looking at.
      //
      // `otherRecord` is taken either way — it is a fact about the sid, not the page.
      //
      // Re-read rather than using the snapshot above: `store` is the state as it was
      // BEFORE the merge, and a reducer that ever does touch this field would make the
      // snapshot silently one update stale.
      store.setTranscriptBounds({
        earlier: merged ? useStore.getState().transcriptEarlier : bounds.earlier,
        otherRecord: bounds.otherRecord,
      })
      noteTranscriptVersion(sid, version)
    })
    .catch(() => {})
    .finally(() => {
      // Only if this call is still the one being tracked: a later call for a DIFFERENT
      // sid has already replaced it, and clearing then would drop that one's dedupe.
      if (inFlightLoad === load) { inFlightSid = null; inFlightLoad = null }
    })
  inFlightSid = sid
  inFlightLoad = load
  return load
}

/** The earlier-page load in flight, keyed by sid — see loadEarlierTranscript. */
let earlierSid: string | null = null
let earlierLoad: Promise<void> | null = null

/**
 * loadEarlierTranscript fetches the page BEFORE the oldest one on screen and prepends
 * it (tether#107).
 *
 * This is the whole point of the wi: before it, the top of a cc-served transcript was a
 * hard ceiling with nothing on screen to say so, and the reader of the 117 MiB session
 * that prompted it could see the last 0.85%.
 *
 * # It reads the cursor from the store, not from an argument
 *
 * So that the cursor spent is provably the one the response that installed the oldest
 * page reported. A caller-supplied offset is a caller-computed offset, and a cursor
 * computed anywhere but on the daemon's side of this route is one that can land
 * mid-record.
 *
 * # Guards, and why each is here
 *
 *  - No cursor: a no-op that resolves. The pane does not render the button in that
 *    state, so reaching this means something raced (a refresh landed between the render
 *    and the click) and the honest answer is "there is nothing earlier".
 *  - `!r.ok` THROWS rather than falling back to an empty page, the same reasoning
 *    refreshTranscript states: an empty page would advance nothing but WOULD look like
 *    "you have reached the beginning", turning a failed request into a false claim.
 *  - The sid is re-checked after the await, so a page for a session the reader has left
 *    cannot be prepended to the one they are now looking at.
 *  - One load at a time per sid: the button is disabled while one is in flight, but the
 *    disable is a render away and the cursor only advances when a response lands, so two
 *    fast clicks would otherwise spend the SAME cursor twice — prepending the same page
 *    twice, which prependHistory would then discard, i.e. a request that cost a megabyte
 *    and did nothing.
 *
 * The returned promise resolves either way; the caller uses it to re-enable the button.
 */
export function loadEarlierTranscript(sid: string): Promise<void> {
  if (!sid) return Promise.resolve()
  if (earlierSid === sid && earlierLoad) return earlierLoad
  const before = useStore.getState().transcriptEarlier
  if (before === null) return Promise.resolve()
  const load = authedFetch(transcriptPath(sid, before))
    .then(r => {
      if (!r.ok) throw new Error(`messages: HTTP ${r.status}`)
      const bounds = readTranscriptBounds(r)
      return r.json().then((msgs: HistoryEntry[]) => ({ bounds, msgs }))
    })
    .then(({ bounds, msgs }: { bounds: ReturnType<typeof readTranscriptBounds>; msgs: HistoryEntry[] }) => {
      const store = useStore.getState()
      if (store.sessionId !== sid) return
      store.prependHistory(msgs.map(historyEntryToMessage))
      // The cursor MOVES BACK to this page's own, so the next click goes one page
      // further rather than re-fetching this one. `otherRecord` is carried through
      // unchanged — it is a fact about the sid, and this response reports the same one.
      store.setTranscriptBounds({ earlier: bounds.earlier, otherRecord: bounds.otherRecord })
      // Deliberately NOT noteTranscriptVersion: that records which version of the
      // transcript is ON SCREEN for the three-second probe to compare against, and an
      // older page says nothing about how recently the file was written. Recording it
      // here would let a "load earlier" suppress the next reload of NEW messages.
    })
    .catch(() => {})
    .finally(() => {
      if (earlierLoad === load) { earlierSid = null; earlierLoad = null }
    })
  earlierSid = sid
  earlierLoad = load
  return load
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
