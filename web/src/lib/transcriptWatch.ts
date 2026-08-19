/**
 * transcriptWatch — "has this conversation been written to since I last read it?"
 * (tether#106), asked cheaply enough to ask every few seconds.
 *
 * # Why anything has to ask
 *
 * New messages normally arrive on the WebTransport stream, so nothing in this app
 * ever re-read a transcript: `openSession` fetched it once and ChatPane's
 * `[sessionId]` effect fetched it once more. A session a background coding agent
 * holds has no stream at all — tether#101 refuses the attach on purpose — so for
 * exactly the sessions a reader most wants to follow, that one fetch was the whole
 * of what they ever saw. tether#104's card said as much ("as it stood when this
 * pane fetched it"), which is how a symptom ends up documented instead of fixed.
 *
 * # Why a probe rather than just re-fetching the transcript
 *
 * Because re-fetching is expensive on the side that looks cheap.
 * `SessionIndex.Messages` prefers tether's OWN history, and `HistoryStore.LoadHistory`
 * is an `os.ReadFile` of the whole `history.jsonl` — no tail, no cap — while only the
 * cc fall-through is bounded (1 MiB, widened once to 16 MiB). The cc transcript of the
 * session that prompted tether#104 was 103,388,175 bytes. A three-second poll of
 * GET /messages is therefore up to a megabyte on the wire every three seconds for one
 * reader, and an unbounded read on the daemon for tether's own sessions. A HEAD of the
 * same route costs a stat or two and returns before the read (SessionIndex.TranscriptUpdatedAt
 * states the exact figure per store; session_api.go is where it returns).
 *
 * Re-fetching is also not free on the CLIENT, which is the half that is easy to miss:
 * `loadHistory` replaces the messages array, and `historyEntryToMessage` mints a fresh
 * `crypto.randomUUID()` per message, so every refetch used to change every React key —
 * remounting the whole transcript, collapsing every expanded block, and clamping the
 * scroll position to the top. store.ts keeps the ids of the unchanged prefix for that
 * reason; this module keeps the refetch itself from happening when nothing changed.
 *
 * # Shape
 *
 * The same shape as sessionActivity.ts (tether#103) — one interval, an `inFlight`
 * dedupe, a deadline so that dedupe cannot freeze the module, and no polling while the
 * tab is hidden — with one deliberate difference: NO reference counting. There is
 * exactly one consumer (ChatPane, which is mounted for the whole app lifetime and
 * watches at most the one session on screen), so a count would be a mechanism with
 * nothing to arbitrate. The watch is last-caller-wins instead, and a stale unsubscribe
 * is a no-op — see the token in watchTranscript.
 */

import { authedFetch } from './auth'

/**
 * The response header the daemon puts the transcript's mtime in, in Unix
 * milliseconds.
 *
 * Mirrored BY HAND from `session.TranscriptUpdatedAtHeader` (Go), and
 * `TestTranscriptUpdatedAtHeaderIsMirroredInTypeScript` reads this file and requires
 * the literal to appear here. Nothing else would notice a rename: header lookups are
 * strings on both sides, so either half can be renamed alone and every compiler, every
 * type-check and every fixture test stays green while the probe silently reads
 * `undefined` forever — i.e. degrades to exactly the frozen transcript this module
 * exists to fix. That failure was measured in tether#101 (mem_mlugObEv) and is why
 * tether#103 has the same guard on its own contract.
 */
export const TRANSCRIPT_UPDATED_AT_HEADER = 'X-Tether-Transcript-Updated-At'

/**
 * The header carrying the byte offset to ask for to read the page BEFORE the one in the
 * response (tether#107). Absent when there is no such page.
 *
 * Mirrored BY HAND from `session.TranscriptEarlierHeader` (Go), and
 * `TestTranscriptPageHeadersAreMirroredInTypeScript` requires the literal to appear
 * here. The consequence of a one-sided rename is worse for this header than for the one
 * above it: the version header going quiet freezes the transcript, which a reader
 * eventually notices, whereas this one going quiet makes the pane stop offering the
 * button AND start asserting, at the top of a 117 MiB transcript, that this is where
 * the conversation began. A confident false statement, at exit code 0.
 */
export const TRANSCRIPT_EARLIER_HEADER = 'X-Tether-Transcript-Earlier'

/**
 * The header naming a store OTHER than the one that answered which also holds a record
 * for this session (tether#107), or absent when there is none.
 *
 * Mirrored by hand, same guard. `cc` is the only value the daemon can currently produce
 * — see session.TranscriptPage.OtherRecord.
 */
export const TRANSCRIPT_OTHER_RECORD_HEADER = 'X-Tether-Transcript-Other-Record'

/** How often the probe runs while a session with no live stream is on screen. */
export const TRANSCRIPT_POLL_MS = 3000

/**
 * The transcript route, for the probe (HEAD), the load (GET) and — since tether#107 —
 * one page further back (`before`).
 *
 * One function so those cannot address different resources. The daemon refuses any
 * sid session.ValidSessionID rejects — outside [A-Za-z0-9_-], or outside 8..128
 * characters (history.go) — so the encoding is defence in depth, but a sid reaching
 * this module from localStorage or a work-item record is not one this module verified.
 *
 * `before` is omitted entirely when it is not given, rather than sent as an empty or
 * zero value. Not cosmetic: the daemon reads an absent parameter as "the newest page"
 * and `before=0` as "the page ending at byte zero", which is empty — so a caller that
 * always appended the parameter would blank the transcript of every session it opened.
 */
export function transcriptPath(sid: string, before?: number): string {
  const base = `/api/v1/sessions/${encodeURIComponent(sid)}/messages`
  return before === undefined ? base : `${base}?before=${encodeURIComponent(String(before))}`
}

/**
 * readTranscriptVersion pulls the version out of a response, or 0 when there is none.
 *
 * 0 means "unknown", never "the epoch": the daemon omits the header when neither store
 * has a transcript for the sid, and a caller holding a stub Response (the fetch mocks in
 * this repo's tests, a non-browser import) has no headers at all. Unknown compares
 * unequal to any real version, which is the behaviour that matters — a reader whose
 * baseline is unknown refreshes once and learns it, rather than sitting on a transcript
 * it cannot vouch for.
 */
export function readTranscriptVersion(res: { headers?: { get(name: string): string | null } }): number {
  const raw = res.headers?.get(TRANSCRIPT_UPDATED_AT_HEADER)
  if (!raw) return 0
  const n = Number(raw)
  return Number.isFinite(n) && n > 0 ? n : 0
}

/** A response's two transcript BOUNDARY facts (tether#107) — see readTranscriptBounds. */
export interface TranscriptBounds {
  earlier: number | null
  otherRecord: string | null
}

/**
 * readTranscriptBounds pulls the boundary facts out of a response.
 *
 * `null` means "no such page" / "no other record", NOT "unknown", and the difference
 * from readTranscriptVersion's 0 is deliberate. A version of 0 is a value the reader
 * refreshes to learn; these two have no such fallback — the pane RENDERS the absence,
 * as "the beginning of this conversation". So the parse is strict in the one direction
 * that matters: anything that is not a positive finite integer reads as null, because
 * showing "the beginning of the conversation" for a header we could not parse is the
 * same claim as showing it for a header that was legitimately absent, and both are
 * better than a button whose cursor is NaN and whose click serves the newest page again.
 *
 * Both are read off ONE response together, and returned as one object, because they and
 * the messages describe the same page. Reading them separately is how a page's cursor
 * ends up recorded next to another page's messages.
 */
export function readTranscriptBounds(res: { headers?: { get(name: string): string | null } }): TranscriptBounds {
  const rawEarlier = res.headers?.get(TRANSCRIPT_EARLIER_HEADER)
  let earlier: number | null = null
  if (rawEarlier) {
    const n = Number(rawEarlier)
    if (Number.isInteger(n) && n > 0) earlier = n
  }
  const other = res.headers?.get(TRANSCRIPT_OTHER_RECORD_HEADER)
  return { earlier, otherRecord: other ? other : null }
}

/**
 * fetchTranscriptVersion asks the daemon for the transcript's version, or throws.
 *
 * HEAD, and that is the whole point: the daemon answers it from a `stat` and returns
 * before touching the transcript. A GET here would cost what the load costs — see this
 * file's header — and nothing in the response would show it.
 *
 * `authedFetch` rather than `fetch` because this runs every few seconds for as long as
 * the session is open. With a bare fetch, a session cookie that expires mid-read turns
 * this into a silent 401 loop: the catch in `probe` keeps the last answer, the pane
 * never updates, and the card goes on saying tether is re-reading the conversation.
 * One request that 401s is a redirect to /auth; a thousand are a lie on screen.
 */
export async function fetchTranscriptVersion(sid: string, signal?: AbortSignal): Promise<number> {
  const res = await authedFetch(transcriptPath(sid), signal ? { method: 'HEAD', signal } : { method: 'HEAD' })
  if (!res.ok) throw new Error(`transcript probe: HTTP ${res.status}`)
  return readTranscriptVersion(res)
}

// ---------------------------------------------------------------------------
// The version the store's transcript came from.
// ---------------------------------------------------------------------------

let loadedSid: string | null = null
let loadedVersion = 0

/**
 * noteTranscriptVersion records which version of a transcript is currently loaded.
 *
 * Called by lib/session.ts's `refreshTranscript` after a successful load, so the FIRST
 * probe after opening a session compares against something real. Without it the probe
 * would have to establish its own baseline, and everything the other agent wrote between
 * the open and that first probe would be invisible until the write after it — on a
 * conversation whose next write may be minutes away, that is a transcript that stops at
 * a message the reader can see is not the last one.
 */
export function noteTranscriptVersion(sid: string, version: number): void {
  loadedSid = sid
  loadedVersion = version
}

/** The version this module believes is on screen for sid; 0 when it has no idea. */
function versionOnScreen(sid: string): number {
  return loadedSid === sid ? loadedVersion : 0
}

// ---------------------------------------------------------------------------
// The poller.
// ---------------------------------------------------------------------------

let watchedSid: string | null = null
let onChanged: (() => void) | null = null
let watchToken = 0
let timer: ReturnType<typeof setInterval> | null = null
let visibilityBound = false
// Which watch has a probe in flight, or 0 for none. A TOKEN rather than a boolean, and
// the difference is not cosmetic: a boolean makes the dedupe below span watches, so
// switching sessions while a probe hangs leaves the NEW session unprobed until the old
// request settles or its deadline fires — a fresh transcript stalled behind a stale
// one's timeout. Per-watch, the dedupe means what it says.
let inFlightToken = 0
// The in-flight probe's AbortController, so replacing or stopping a watch really ends
// its request rather than leaving one to be discarded on arrival.
let inFlightAbort: AbortController | null = null

async function probe(): Promise<void> {
  const sid = watchedSid
  const token = watchToken
  if (sid === null) return
  // One request at a time per watch. Without this, a daemon slower than the interval
  // would accumulate overlapping probes and an older answer could land after a newer one.
  if (inFlightToken === token) return
  inFlightToken = token
  // …and a DEADLINE, because that guard is otherwise a way for this module to freeze
  // itself: the token is released only when a request settles, so one that never does
  // would make every later tick a no-op while the timer kept running. Same construction
  // and same reason as sessionActivity.ts — setTimeout + AbortController rather than
  // AbortSignal.timeout, so a test can advance it.
  const ac = new AbortController()
  inFlightAbort = ac
  const deadline = setTimeout(() => ac.abort(), TRANSCRIPT_POLL_MS * 2)
  try {
    const version = await fetchTranscriptVersion(sid, ac.signal)
    // The watch moved (or stopped) while this was in flight, so this answer is about a
    // session nobody is watching. Both lines below would then be actively wrong rather
    // than merely useless: `onChanged` is the NEW watch's callback (it is reassigned,
    // not queued), so calling it reloads the session on screen because a different one
    // changed — and noteTranscriptVersion would record this sid as the loaded one,
    // which makes the next probe for the session actually on screen see no baseline
    // and reload it again.
    if (watchedSid !== sid) return
    if (version === versionOnScreen(sid)) return
    // Recorded whether or not the reload that follows succeeds, so a daemon that
    // answers the probe but not the load cannot turn this into a reload on every tick.
    // The automatic path is best-effort by design; the guaranteed one is the reader
    // clicking the row, which reloads unconditionally. (The ORDER of these two lines is
    // not what buys that — the callback starts an async load either way — the point is
    // that the record happens at all.)
    noteTranscriptVersion(sid, version)
    onChanged?.()
  } catch {
    // A probe we could not complete changes nothing and retries on the next tick — the
    // same policy sessionActivity.ts applies, for the same reason: there is nothing here
    // the user must act on, and a connection this broken already has its own indicator.
  } finally {
    clearTimeout(deadline)
    if (inFlightAbort === ac) inFlightAbort = null
    // Only if this watch is still the one holding the slot: a later watch has already
    // taken it, and clearing here would give that one a second concurrent probe.
    if (inFlightToken === token) inFlightToken = 0
  }
}

/** End any probe in flight; its answer is about a watch that no longer exists. */
function abortInFlight() {
  inFlightAbort?.abort()
  inFlightAbort = null
}

/**
 * hidden reports whether the tab is currently in the background. Guarded because
 * `document.visibilityState` is not guaranteed in every environment this bundle is
 * imported into, and the safe default is "visible" — polling when we cannot tell costs
 * a stat, not polling when we cannot tell is the frozen transcript again.
 */
function hidden(): boolean {
  return typeof document !== 'undefined' && document.visibilityState === 'hidden'
}

// A named limit, because "hidden" here is the TAB and the pane is a smaller thing than
// that: App.tsx keeps ChatPane mounted under `display: none` while the reader is on the
// Skill or Shell tab, so a held session goes on being probed — and reloaded — for a
// pane nobody can see. Kept rather than fixed, and the reason is the same one the
// visibility-return probe gives: the reader who switches back wants a current
// transcript, not one that starts catching up when they arrive. The cost while they are
// away is one probe every three seconds plus one load per write, i.e. it is bounded by
// what the other agent is doing, not by how long the pane is hidden. Narrowing it would
// mean ChatPane learning whether it is the visible tab, which is App.tsx's shape to
// change, not this module's to guess.

/** Restart the interval to match the state of the world. Idempotent. */
function reschedule() {
  const wanted = watchedSid !== null && !hidden()
  if (wanted && timer === null) {
    timer = setInterval(() => { void probe() }, TRANSCRIPT_POLL_MS)
  } else if (!wanted && timer !== null) {
    clearInterval(timer)
    timer = null
  }
}

function onVisibilityChange() {
  reschedule()
  // Coming BACK to the tab checks immediately rather than waiting out the interval.
  // Without this half, the pause it pairs with shows a transcript up to one whole poll
  // stale on return — which looks exactly like the bug the pause was traded for.
  if (watchedSid !== null && !hidden()) void probe()
}

/**
 * watchTranscript follows one session's transcript and calls onUpdate when it changes.
 *
 * Returns the stop function. Starting a watch replaces any previous one (last caller
 * wins), and a stop from a superseded caller is a no-op — that is what the token is for.
 * React's StrictMode runs an effect, its cleanup, then the effect again, so a stop that
 * blindly cleared the state would tear down the watch its own re-run had just installed.
 *
 * It probes IMMEDIATELY as well as on the interval, because the state that turns this on
 * (the pane learning its attach was refused) is seconds after the transcript was
 * fetched, and whatever the other agent wrote in that window is already missing.
 */
export function watchTranscript(sid: string, onUpdate: () => void): () => void {
  abortInFlight()
  const token = ++watchToken
  watchedSid = sid
  onChanged = onUpdate
  if (!visibilityBound && typeof document !== 'undefined') {
    document.addEventListener('visibilitychange', onVisibilityChange)
    visibilityBound = true
  }
  reschedule()
  void probe()
  return () => {
    if (watchToken !== token) return
    abortInFlight()
    watchedSid = null
    onChanged = null
    reschedule()
  }
}

/**
 * Test seam: stop the watch, forget the loaded version, unbind the listener.
 *
 * Module state outlives a component tree, so without this one test file's watch would
 * still be running — and its `fetch` still being counted — inside the next one.
 */
export function resetTranscriptWatchForTests(): void {
  watchedSid = null
  onChanged = null
  // Deliberately NOT reset to 0: a live watch holding token N would then be handed
  // `watchToken !== token` by its own stop and leak. Monotonic is what makes every
  // outstanding stop a no-op, which is the property the counter exists for.
  watchToken++
  if (timer !== null) {
    clearInterval(timer)
    timer = null
  }
  if (visibilityBound && typeof document !== 'undefined') {
    document.removeEventListener('visibilitychange', onVisibilityChange)
    visibilityBound = false
  }
  loadedSid = null
  loadedVersion = 0
  abortInFlight()
  inFlightToken = 0
}

/**
 * Test seam: is a timer running, what is it watching, and what version does this module
 * believe is on screen?
 *
 * `running` is exposed so a test can assert the TIMER rather than only the request count.
 * A watch that leaves an interval behind after its stop is invisible to any assertion
 * made right after that stop — the requests it produces arrive later — and that leak is
 * the failure a stop function exists to prevent (tether#103 paid for this one).
 */
export function transcriptWatchState(): { running: boolean; sid: string | null; version: number } {
  return { running: timer !== null, sid: watchedSid, version: loadedVersion }
}
