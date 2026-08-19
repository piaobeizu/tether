import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { TetherWT } from '../../lib/wt'
import { ControlClient } from '../../lib/control'
import { chatURL } from '../../lib/chatUrl'
import { useStore, historyEntryToMessage, mergeTranscript, type HistoryEntry, type ToolCall } from '../../lib/store'
import { CopyButton } from '../../lib/CopyButton'
import { Icon } from '../../lib/icons'
import type { FencedBlock, ProviderListResponse } from '../../lib/wire.gen'
import { ClientFrameAction, ErrCodeSessionHeldByBackgroundAgent } from '../../lib/wire.gen'
import { authedFetch } from '../../lib/auth'
import { loadEarlierTranscript, refreshTranscript, REFRESH_TRANSCRIPT_EVENT } from '../../lib/session'
import { noteTranscriptVersion, readTranscriptBounds, readTranscriptVersion, transcriptPath, watchTranscript } from '../../lib/transcriptWatch'
import {
  SESSION_ACTIVITY_IDLE,
  SESSION_ACTIVITY_WORKING,
  useSessionActivityAnswer,
  type SessionActivityState,
} from '../../lib/sessionActivity'
import { DagBlock } from '../../fenced-blocks/DagBlock'
import { FormBlock } from '../../fenced-blocks/FormBlock'
import { CandidatesBlock } from '../../fenced-blocks/CandidatesBlock'
import { MediaBlock } from '../../fenced-blocks/MediaBlock'
import { PermissionQueue, postDecide } from '../../fenced-blocks/PermissionBlock'
import Markdown from '../canvas/Markdown'
import SessionList from './SessionList'

type ConnState = 'connecting' | 'connected' | 'reconnecting' | 'failed'

const RECONNECT_BASE_MS = 1_000
const RECONNECT_MAX_MS = 16_000
const RECONNECT_MAX_ATTEMPTS = 5
// tether#52 — how long the FIRST connect waits for the workspace list before
// giving up and connecting without a workspace. A backstop, not the normal path:
// WorkspacePane releases the gate as soon as its fetch settles either way, so this
// only fires if that request hangs — and connecting late is better than a chat pane
// that never connects at all.
const WORKSPACE_GATE_TIMEOUT_MS = 2_000

const SLASH_CMDS = [
  { cmd: '/spec',   desc: 'write a spec for this task' },
  { cmd: '/plan',   desc: 'decompose into ordered steps' },
  { cmd: '/review', desc: 'review pending changes' },
  { cmd: '/diff',   desc: 'show current diff' },
]

function fmtTime(ts: number) {
  return new Date(ts).toLocaleTimeString('en-GB', { hour: '2-digit', minute: '2-digit' })
}

function fmtElapsed(start: number) {
  const mins = Math.floor((Date.now() - start) / 60000)
  if (mins < 1) return 'now'
  if (mins < 60) return `${mins}m`
  return `${Math.floor(mins / 60)}h ${mins % 60}m`
}

// tether#46 — multi-line composer. The composer is a <textarea>: Enter sends,
// Shift+Enter inserts a newline. shouldSendOnEnter and growHeight are extracted
// pure so they unit-test without mounting ChatPane (which opens a WebTransport
// connection). MAX_COMPOSER_LINES / COMPOSER_LINE_PX must match .composer-input
// line-height + max-height in index.css.
const MAX_COMPOSER_LINES = 8
const COMPOSER_LINE_PX = 20

// tether#47 — max @-mention file suggestions shown at once.
const AT_MENU_MAX = 10

// shouldSendOnEnter decides whether an Enter keypress sends the message. It does
// NOT send when: Shift is held (newline), an IME composition is active, a turn is
// streaming (the button is Stop — tether#42), or the slash menu is open (which
// owns Enter). Any non-Enter key never sends.
export function shouldSendOnEnter(o: {
  key: string; shiftKey: boolean; isComposing: boolean; streaming: boolean; slashActive: boolean
}): boolean {
  return o.key === 'Enter' && !o.shiftKey && !o.isComposing && !o.streaming && !o.slashActive
}

// growHeight clamps a textarea's measured scrollHeight to [minLines, maxLines]
// line-heights and reports whether content overflows (so the caller turns on the
// internal scrollbar). Pure — the caller measures scrollHeight and applies the
// result — so it tests without a real DOM.
export function growHeight(
  scrollHeight: number,
  o: { lineHeightPx: number; maxLines: number; minLines?: number },
): { height: number; scroll: boolean } {
  const min = (o.minLines ?? 1) * o.lineHeightPx
  const max = o.maxLines * o.lineHeightPx
  return { height: Math.max(min, Math.min(scrollHeight, max)), scroll: scrollHeight > max }
}

// tether#52 — first-connect ordering. A brand-new session's cwd is pinned at
// spawn (chatUrl.ts), so if the pane connects before the browsed workspace is
// known, a fresh session locks into the daemon's default directory for its
// entire life — there's no fixing it after the fact (see chatUrl.ts's doc
// comment on why `ws` can't just be resent later). WorkspacePane publishes
// `activeWorkspace`/`workspacesLoaded` only once its own GET
// /api/v1/workspaces resolves, which happens strictly AFTER ChatPane mounts,
// so "just connect on mount" races that fetch and — on a cold browser profile
// — normally loses.
//
// The gate applies ONLY to the sid-less path: with a remembered `tether_last_
// sid`, the daemon already knows that session's workspace and ignores
// anything we'd send (chatUrl.ts), so making the overwhelmingly common
// reconnect wait on `workspacesLoaded` would add latency for zero behavioral
// effect. Extracted pure (mirrors shouldSendOnEnter/growHeight above) so this
// ordering decision is unit-testable without mounting the pane (WebTransport).
export function shouldDeferFirstConnect(o: { hasLastSid: boolean; workspacesLoaded: boolean }): boolean {
  return !o.hasLastSid && !o.workspacesLoaded
}

// tether#63 — decides whether a dropped WebTransport connection's onClose
// should schedule the reconnect ladder. Extracted pure (mirrors
// shouldDeferFirstConnect above) so this is unit-testable without mounting
// the pane. Two independent reasons say no:
//   - unmounted: the pane is gone: nothing is left to hand a reconnected
//     socket to, and the cleanup effect has already cancelled any pending
//     timer (pre-existing behaviour, unchanged by tether#63).
//   - fatal: the daemon's LAST word on this connection was a terminal
//     wire.ErrorPayload (session.Refusal with Terminal=true — see
//     wire/errors.go) — e.g. an unknown workspace or a session owned by
//     another tab. The WebTransport handshake itself succeeded (a refusal is
//     sent AFTER wts.Upgrade in wt_chat.go), so retrying reopens the same
//     handshake only to be refused again, once a second, forever — the exact
//     silent-loop bug this slice exists to fix. Both false only when NEITHER
//     condition holds: a live pane whose most recent envelope carried no
//     terminal refusal is the one case an ordinary transient drop should
//     still be retried.
export function shouldReconnectAfterClose(o: { unmounted: boolean; fatal: boolean }): boolean {
  return !o.unmounted && !o.fatal
}

// tether#63 — MIN_USABLE_CONN_MS is how long a connection must stay open before
// it counts as an attempt that WORKED and refunds the reconnect budget.
//
// This is the structural half of the fix, and the reason RECONNECT_MAX_ATTEMPTS
// above was decorative rather than a bound. `wt.connect()` resolving means the
// WebTransport HANDSHAKE succeeded — nothing more. A daemon-side refusal is
// sent AFTER wts.Upgrade has already returned (wt_chat.go), so a refused
// connection completes a perfect handshake and then dies. Refunding the budget
// on that made every cycle attempt #1 again, and the ladder retried once a
// second for as long as the page stayed open. Measured on an unpatched build:
// 27 reconnects in a 30-second window, with no upper bound in sight.
//
// The classification (a terminal wire.ErrorPayload) is what stops such a
// connection on the FIRST attempt, and it is the mechanism that normally runs.
// This threshold is what keeps the loop bounded when that reason does not
// arrive — an older daemon, or a link slow enough to lose the envelope inside
// refusalDrainGrace. It has to be a duration rather than something sharper
// because at close time a refusal and a genuine early drop are the same event;
// what separates them is that a refused session is torn down in well under a
// second (the daemon's own grace is 300ms) while a usable one lives for as long
// as the user is there. 2s sits an order of magnitude above the former and
// orders of magnitude below the latter.
//
// WHO ELSE PAYS, stated rather than left to be discovered: every RETRYABLE
// refusal is also sub-2s by construction (the daemon's grace is the only thing
// holding the session open), so spawn_failed / connection_closed /
// session_unconfirmed now consume budget too. Those used to retry indefinitely;
// they now get five attempts across ~31s and then a Retry button. That is a
// real reduction, and it is the one Attachment.resolve's concurrent-attach note
// leans on — but five spaced retries is a fair allowance for a spawn that keeps
// dying, and "the daemon cannot start an agent" is a state a human should be
// told about rather than one the browser should hammer at forever.
//
// Being wrong in the conservative direction costs a user on a link that cannot
// hold two seconds five quick retries and then a Retry button — which is the
// honest report for a link that cannot hold two seconds.
//
// Kept comfortably above internal/server/wt_chat.go's refusalDrainGrace. That
// relationship is load-bearing and nothing enforces it across the two
// languages; if the grace ever grows towards a second, this must grow with it.
const MIN_USABLE_CONN_MS = 2_000

export function shouldRefundAttemptBudget(uptimeMs: number): boolean {
  return uptimeMs >= MIN_USABLE_CONN_MS
}

// transcriptTextLength is the autoscroll effect's "did any answer grow?" signal
// (tether#88). Pure, and extracted for the same reason as its neighbours above:
// the effect itself needs a mounted pane and a scrollable element, so the only
// part that can be pinned by a test is the decision of when to re-run it.
//
// It sums EVERY message rather than reading the last one, and that is the whole
// change. The old dep was `transcript[length-1].text`, which is the same thing
// while the growing bubble is last — and until tether#88 it always was, because
// sending a prompt ended the open turn, so whatever streamed next opened a bubble
// below the user's message. Now the running turn keeps its bubble ABOVE the
// prompt the user just sent, and text arriving into it changed neither the array
// length nor the last element: the effect stopped firing, the view stopped
// following the answer, and the growing bubble pushed the rest down past the
// viewport. Nothing about the fix is visible if you cannot see it happen.
//
// Summing is safe as a change-detector here because `messages` is append-only
// per turn and a message's text only ever grows; the one in-place replacement
// (a re-emitted fenced block, store.ts's 'fenced' branch) swaps `block` and
// leaves `text` at '', so it neither triggers nor suppresses a scroll — exactly
// as before. It is O(messages) on each render of an already-memoised array,
// against the markdown render the same commit is doing.
export function transcriptTextLength(messages: { text: string }[]): number {
  let n = 0
  for (const m of messages) n += m.text.length
  return n
}

// tether#107 — where the scroll container must land after an older page is prepended.
//
// Pure and extracted for the same reason as its four neighbours above: the effect that
// applies it needs a mounted pane and a real scrollable element, so the only part a unit
// test can reach is the arithmetic.
//
// Prepending grows the content ABOVE the viewport, so keeping the reader on the message
// they were looking at means moving scrollTop down by exactly what was inserted. Doing
// nothing leaves them looking at the top of the newly-arrived page — which is not
// nothing, but it is not where they were, and on a 25-bubble page it is a long way from
// it. The clamp at 0 is for the degenerate case where the container SHRANK (it cannot
// here, but a negative scrollTop is silently clamped by the browser and loudly wrong in
// a test, and the test is the thing that has to be able to tell).
export function scrollAfterPrepend(prev: { top: number; height: number }, nextHeight: number): number {
  return Math.max(0, prev.top + (nextHeight - prev.height))
}

// tether#110 — how close to an end of the transcript counts as being AT it.
//
// One number for both ends, because it means one thing: "the reader has arrived here".
// It is deliberately not tied to the 120px `nearBottom` threshold below, which answers a
// different question (should a new message pull the view down) and would be wrong to
// change while pretending to reuse it.
export const TRANSCRIPT_EDGE_PX = 48

// The floor between two automatic loads at the same end. The latch below already makes
// parking at an end fire once, so this covers the shape the latch cannot: a shaky flick —
// or iOS momentum plus rubber-band — oscillating across the threshold. A frame is 16ms,
// so this is ~30 frames, and a determined reader can still page back twice a second.
export const TRANSCRIPT_EDGE_MIN_INTERVAL_MS = 500

/** What a scroll event should do about one end of the transcript. */
export type EdgeAction = 'idle' | 'arm' | 'load'

/**
 * transcriptEdgeAction — tether#110's whole trigger, for BOTH ends.
 *
 * Pure and extracted for the same reason as its five neighbours above: the thing that
 * calls it needs a mounted pane and a real scrollable element, so the arithmetic is the
 * only part a unit test can reach directly. Both ends share it so that "the loop is
 * bounded" is one argument proved once rather than two that can drift — the caller passes
 * `scrollTop` for the top and `scrollHeight - scrollTop - clientHeight` for the bottom.
 *
 * # Why there is a latch at all
 *
 * Auto-loading forms a loop with the prepend anchor. load → prepend →
 * `scrollAfterPrepend` writes `el.scrollTop` to put the reader back → **a programmatic
 * scrollTop write is a scroll**, and CSSOM View runs the scroll steps for it on the next
 * frame, so the browser fires `scroll` again → and if that restored position still reads
 * as "at the top", it loads again. On the 117 MiB session that prompted tether#107 that is
 * a dozen pages per flick, each ~1 MiB read on the daemon. The bottom has the same shape:
 * the `nearBottom` guard that keeps a reader glued to the newest message is ALSO an
 * `el.scrollTop = el.scrollHeight` write, and if reaching the bottom refetches, and the
 * refetch appends, and appending sticks to the bottom, that is the second loop.
 *
 * So `armed` starts FALSE and is only ever set by an observed position FURTHER than
 * `TRANSCRIPT_EDGE_PX` from that end. Parking at an end therefore fires exactly once; you
 * have to leave and come back. That also disposes of both loops by construction rather
 * than by luck: the anchor correction and the autoscroll both leave the reader at a
 * distance the latch has already consumed.
 *
 * Starting false rather than true is load-bearing in its own right. The transcript's first
 * load autoscrolls to the bottom, which fires a scroll event at distance ≈ 0 — so an
 * initially-armed bottom would refetch the page it had just fetched, every time a session
 * was opened.
 *
 * # `available` and `inFlight` are the caller's facts, not this function's
 *
 * `available` is "this end has something to fetch" — a cursor at the top, and the
 * held-session state at the bottom (see the handler for why the bottom is gated at all).
 * `inFlight` covers BOTH ends and the button too, so at most one transcript request
 * exists at a time; that is what keeps one end's indicator from changing the scroll
 * height the other end's anchor arithmetic was measured against.
 *
 * Neither is folded in here, because a `'load'` this function returned for an end with
 * nothing to fetch would be a bug that only the caller could see.
 */
export function transcriptEdgeAction(o: {
  distance: number
  armed: boolean
  available: boolean
  inFlight: boolean
  sinceLastMs: number
}): EdgeAction {
  // Away from this end: re-arm, whatever else is true. Arming is a record that the reader
  // moved, so an in-flight request is no reason to withhold it.
  if (o.distance > TRANSCRIPT_EDGE_PX) return 'arm'
  if (!o.armed) return 'idle'
  if (!o.available) return 'idle'
  if (o.inFlight) return 'idle'
  if (o.sinceLastMs < TRANSCRIPT_EDGE_MIN_INTERVAL_MS) return 'idle'
  return 'load'
}

// tether#107 — the two things the top of a transcript can say when there is nothing
// earlier to fetch, which before this change were the same thing: nothing.
//
// Exported so the render tests can pin them by identity as well as by literal, the same
// reason HELD_SESSION_PLACEHOLDER is.
export const TRANSCRIPT_START_COMPLETE = 'the beginning of this conversation'

// The other one, and the only sentence in this file that exists for a population that is
// currently EMPTY.
//
// SessionIndex.Messages prefers tether's own history whenever it has any, and
// `history.jsonl` is written only by this daemon's fan-out — so for a sid tether once
// recorded and a terminal-launched background job later resumed, the transcript served
// is tether's short record while cc holds the long one (sessionlist.go states this
// combination as TranscriptUpdatedAt's second residual). tether#107 measured that
// combination at zero sessions on the reference machine, and built for it anyway,
// because the alternative is for the pane to say TRANSCRIPT_START_COMPLETE there — a
// claim about the conversation that would be false.
//
// It names Claude Code because `cc` is the only value the daemon can emit for that
// header (session.TranscriptPage.OtherRecord says why the symmetric case is
// unreachable); describeOtherRecord below is what keeps an unknown value honest instead
// of printing it.
export const TRANSCRIPT_START_TETHER_RECORD_ONLY =
  'the beginning of what tether recorded for this session — Claude Code has its own, separate record of it'

// The generic form, for a store name this build does not know. Same shape as
// FATAL_CODE_MESSAGES' fallback and for the same reason: a frontend build can be older
// than the daemon it is embedded next to only if someone splits them, but rendering a
// raw identifier at the reader is never the right answer to that.
export const TRANSCRIPT_START_OTHER_RECORD_GENERIC =
  'the beginning of what tether recorded for this session — another agent has its own, separate record of it'

export function describeOtherRecord(store: string): string {
  return store === 'cc' ? TRANSCRIPT_START_TETHER_RECORD_ONLY : TRANSCRIPT_START_OTHER_RECORD_GENERIC
}

// tether#108 — the four things the card can say about what that agent is doing RIGHT
// NOW, and why it is four sentences rather than a spinner.
//
// # The question these answer
//
// A reader who opens a conversation a background agent is holding wants to decide one
// thing: wait, or leave. A permanently-spinning indicator answers neither — it says "you
// are waiting", which they can already see — while the daemon has been able to answer the
// real question every three seconds since tether#103 and nothing in this pane asked.
//
// # Each sentence is checkable against the daemon, and each one is narrower than it
// # would be natural to write
//
//  - `working` says "a turn", not "the model is replying": the agent's status is `busy`
//    for the whole turn, tool execution included (session/activity.go quotes cc's own
//    `k2h`), so the narrower claim would be false three minutes into a test run.
//  - `idle` says "no turn in flight", NOT "between turns", because the daemon also reports
//    it for cc's `waiting` (mid-conversation, blocked on the user) and `shell` (a shell
//    task running while the agent itself is idle).
//  - `held` names the limit rather than picking a side. session.SessionActivityHeld is the
//    fallback for a status this build cannot classify — it is a refusal to claim, and
//    reading it as either "working" or "idle" is the mislabel tether#103 exists to remove.
//  - absence is the fourth answer, and it is a CONCLUSION rather than a report: see
//    heldActivityLine for why it is sound here and why it needs `answered`.
//
// # What none of them says, and this is load-bearing
//
// None promises that new content will appear. The obvious line for `working` — "this pane
// keeps itself up to date" — is a claim tether#106 deliberately removed from
// HELD_SESSION_READABLE_NOTE: SessionIndex.Messages prefers tether's own history.jsonl,
// which only this daemon writes, so for a sid tether once recorded and a background job
// later resumed the served file is stale and STAYS stale while that agent works. These
// lines report what the AGENT is doing; what TETHER does is the next line down, where it
// is already stated with the right hedge.
//
// They also do not repeat WHEN THE HOLD ENDS. FATAL_CODE_MESSAGES' entry for this code
// already says the hold lasts as long as that agent's process and that "an idle job holds
// this conversation exactly as firmly as a busy one" — so the `idle` line's whole job is
// to say that the hypothetical in the sentence above it is the case now.
//
// Exported so the tests can pin them by identity as well as by literal, the same reason
// HELD_SESSION_PLACEHOLDER is.
export const HELD_ACTIVITY_WORKING = 'Right now: a turn is in flight in that agent.'
export const HELD_ACTIVITY_IDLE = 'Right now: no turn is in flight in that agent.'
export const HELD_ACTIVITY_UNKNOWN =
  'Right now: tether cannot see whether a turn is in flight in that agent.'
// "nothing live is holding this conversation" and NOT "that process has exited", which was
// the first draft and is one inference too far. cc's liveness check is pid + /proc start
// token (ccPidHoldsRecord), and it answers "not live" both for a process that exited and
// for a /proc this daemon cannot read — its own doc says so and calls the direction
// deliberate. The sentence therefore reports what tether can see, which is also all the
// reader needs. Residual in that second world: tether stops refusing too, so "Check again"
// opens a FRESH session rather than resuming — a degenerate host condition, named because
// the sentence would otherwise be read as a promise about the resume.
export const HELD_ACTIVITY_GONE =
  'Right now: nothing live is holding this conversation — Check again should open it.'

/**
 * All four, in one place, because the ROW HAS TO BE AS TALL AS THE TALLEST OF THEM
 * (tether#108).
 *
 * This card sits inside the scroll container, so any change in its height moves the
 * transcript under a reader who is reading it. A `min-height` of one line does not
 * deliver that: measured at 11px mono (~0.6em advance) the four sentences need between
 * ~300px and ~540px of text width, while `.dt-right` starts at 640px (lib/layout.ts
 * DEFAULT_RIGHT), drops to 260px (MIN_RIGHT) when dragged, and on a phone is the whole
 * viewport — so at real widths they wrap to different numbers of lines and the row's
 * height would change on the first poll AND on every transition between two states that
 * wrap differently.
 *
 * So the row renders ALL FOUR, stacked in one grid cell, with three of them
 * `visibility: hidden` (and the not-yet case hiding all four). The cell is then as tall
 * as the tallest sentence AT WHATEVER WIDTH THE PANE HAPPENS TO BE, with no hard-coded
 * pixel value to go stale — and its height never changes, so there is nothing to shift.
 * `.session-row-act` makes the same trade for the same reason (its comment: "a list that
 * twitches every three seconds is a worse defect than the one this marker fixes"); it can
 * do it with a fixed 7px because a dot has no text to wrap.
 *
 * The cost is honest and stated: the card permanently reserves the height of the longest
 * sentence, which is two lines at the default width and three on a narrow phone, even
 * before the daemon has answered. `visibility: hidden` also keeps the hidden three out of
 * the accessibility tree, so a screen reader gets one sentence, not four.
 */
export const HELD_ACTIVITY_LINES = [
  HELD_ACTIVITY_WORKING,
  HELD_ACTIVITY_IDLE,
  HELD_ACTIVITY_UNKNOWN,
  HELD_ACTIVITY_GONE,
] as const

/**
 * heldActivityLine turns one poll of the activity endpoint into the line to render, or
 * null when there is nothing true to say yet (tether#108).
 *
 * # Why absence is a claim this card is allowed to make
 *
 * Because the refusal on screen and the activity answer come from the SAME cc registry
 * instance through two filters, one of which is strictly wider:
 *
 *   - the refusal fires only when `ccLiveJob(sid)` finds a live record (attach.go), which
 *     is `forEachLiveRecord` PLUS cc's holder filter `kind != "" && kind != "interactive"`;
 *   - ActivityIndex.States reads `LiveRecords()`, the same walk with NO kind filter;
 *   - mux.go wires ActivityIndex with `reg.CCJobs`, i.e. the instance the attach path used.
 *
 * So while the holding process lives, its sid is necessarily in the map. Absence means
 * cc's liveness check (pid + /proc start token) stopped matching — that process is gone.
 * `fatal` is sticky until a reconnect, so this is not a corner case: it is what a reader
 * watching a background job finish actually sees, and it is the moment "Check again" stops
 * being a shot in the dark.
 *
 * # Why it needs `answered` and cannot read absence alone
 *
 * The poller's map starts empty, so for one round trip after mount every sid is absent.
 * Without the flag this function would announce that the hold had ended, on every open,
 * before anything had been asked. `answered` is set only on a SUCCESSFUL fetch
 * (sessionActivity.ts), so a daemon that never replies keeps this at null rather than
 * degrading into the one answer the reader would act on.
 *
 * # ABSENCE is tested first, and an unrecognised state is NOT absence
 *
 * The order is the whole correctness of the function. Written as a `switch` with the
 * "nothing is holding it" sentence in the `default`, a state string this build has not
 * been taught — a daemon newer than this bundle — would render as "nothing live is
 * holding this conversation any more", which is the one answer a reader acts on and is
 * false: the daemon reported something for that sid, so something IS holding it.
 * `fetchSessionActivity` already drops unknown strings, so the case is unreachable
 * through the poller; this is written to be right without depending on that, because
 * "unreachable" is a property of another module that a future edit can change.
 *
 * Unknown therefore lands with `held`, which is exactly what the daemon does with a
 * status IT cannot classify (ccStatusActivity's own `default`) — one sentence that is
 * true without knowing what the word means.
 */
export function heldActivityLine(o: {
  answered: boolean
  state: SessionActivityState | undefined
}): string | null {
  if (!o.answered) return null
  if (o.state === undefined) return HELD_ACTIVITY_GONE
  if (o.state === SESSION_ACTIVITY_WORKING) return HELD_ACTIVITY_WORKING
  if (o.state === SESSION_ACTIVITY_IDLE) return HELD_ACTIVITY_IDLE
  // Everything left: `held`, and any state string this build has not been taught. They
  // get one sentence because they are one situation — a live process has this open and
  // what it is doing is not readable — so there is nothing for a second branch to say.
  return HELD_ACTIVITY_UNKNOWN
}

/**
 * How long a bubble wears the arrival trace (tether#108). Matched by the CSS animation
 * on `.msg-arrived`; this side is what takes the class back off.
 */
export const ARRIVAL_TRACE_MS = 1_100

/**
 * trailingArrivals — which of these messages JUST ARRIVED (tether#108).
 *
 * tether#106's three-second refresh is completely silent: new content simply appears. This
 * is the rule behind the light trace that fixes that, and it is deliberately "the trailing
 * run of ids that were not on screen before" rather than "every id that is new".
 *
 * Two properties fall out of the SHAPE rather than out of a flag someone has to remember:
 *
 *  - **a prepended page is never traced.** tether#107's "load earlier messages" puts older
 *    bubbles at the FRONT, so the walk from the end stops immediately. Flashing 25 bubbles
 *    the reader deliberately asked for would say "these just landed", which is false.
 * What this function deliberately does NOT decide is whether a WHOLE-ARRAY answer counts.
 * Against an empty previous set it returns everything, which is honest — every id really is
 * new — and is also exactly what the first transcript load and tether#107's disjoint
 * replace look like. The caller drops that case (see the effect), because only the caller
 * knows which sid the set belongs to and whether the array was replaced or appended to.
 *
 * # A message that GROWS is not an arrival, and that is the point
 *
 * `messageKey` excludes `text` on purpose (store.ts), so a message keeps its id while its
 * content changes. Tracing "changed" instead of "new" would therefore re-highlight the same
 * row on every refresh for as long as its content kept moving — which is a spinner wearing
 * a highlight, i.e. the thing this wi rules out. Stated as the rule rather than as a
 * scenario: CCStore emits a message per record group, so how often a served message's text
 * actually grows in place is not something this file should claim to know.
 */
export function trailingArrivals(prevIds: Set<string>, next: { id: string }[]): string[] {
  const out: string[] = []
  for (let i = next.length - 1; i >= 0; i--) {
    if (prevIds.has(next[i].id)) break
    out.push(next[i].id)
  }
  return out.reverse()
}

/**
 * HeldSessionActivity — the state line, and the ONLY thing in this pane that subscribes to
 * the activity poller (tether#108).
 *
 * # Why it is a component and not a hook call in ChatPane
 *
 * Because the subscription has to be able to STOP. Hooks cannot be conditional, so a
 * `useSessionActivityAnswer` in ChatPane would subscribe for the entire life of the app —
 * ChatPane is mounted unconditionally and only hidden with display:none (App.tsx) — and
 * that is not free. The wi's premise was that this costs no new requests because the poller
 * is already running; it is NOT already running on most screens: the chat session list is
 * COLLAPSED BY DEFAULT (SessionList's `open` starts false, and the rows are inside
 * `{open && …}`), so no SessionRow is mounted, so `subscribeSessionActivity` has no
 * subscribers and its interval is stopped. Measured, not assumed — see the pane test that
 * counts requests to SESSION_ACTIVITY_PATH in the connected state and requires zero.
 *
 * So the cost is stated honestly instead: rendered only under `readingHeldSession`, this is
 * a second three-second request alongside tether#106's HEAD probe, for as long as this pane
 * is in that state. Mounting is the whole of opting in, which is SessionRow's argument for
 * putting the subscription in the row rather than in its two parents, applied one level up.
 * Its bound, since "as long as someone is looking" would overstate it: ChatPane stays
 * mounted behind the Skills and Shell tabs (App.tsx hides it with display:none), so the
 * poll continues while the reader is on another right-hand tab. A hidden BROWSER tab does
 * pause it — that half is the poller's own (see reschedule).
 *
 * # Every sentence is rendered, and three of them are hidden
 *
 * Not for thoroughness — for the scroll anchor. See HELD_ACTIVITY_LINES for the whole
 * argument and the measurement behind it; the short version is that the four sentences wrap
 * to different heights at real pane widths, so a single line whose text changes is a card
 * that changes height, and this card is inside the scroll container.
 */
export function HeldSessionActivity({ sid }: { sid: string }) {
  const answer = useSessionActivityAnswer(sid)
  const line = heldActivityLine(answer)
  // No role and no aria-live. It is a plain sentence in a card the reader is already
  // reading, and announcing every three-second transition would talk over them.
  //
  // `on` is what the CSS makes visible and what a test reads, so "which sentence is
  // showing" is one class rather than a comparison a reader has to redo. When `line` is
  // null nothing carries it, which is the not-yet state: full height, no claim.
  return (
    <div className="state-card-activity">
      {HELD_ACTIVITY_LINES.map(l => (
        <span key={l} className={l === line ? 'state-card-activity-line on' : 'state-card-activity-line'}>{l}</span>
      ))}
    </div>
  )
}

// tether#110 — what the reader is told the two labels mean. Exported so the render tests
// pin them by identity, the same reason TRANSCRIPT_START_COMPLETE is.
export const TRANSCRIPT_DOTS_EARLIER_LABEL = 'loading earlier messages'
export const TRANSCRIPT_DOTS_NEWER_LABEL = 'checking for new messages'

/**
 * TranscriptDots — three dots, and ONLY while a request is genuinely in flight.
 *
 * The distinction this repo has settled repeatedly: spinning where nothing more can
 * arrive is a lie, because waiting will never produce anything. So this never renders at
 * the ceiling — the top of a complete transcript keeps tether#107's three sentences — and
 * it never renders on a timer. Its whole population is "a request exists and will settle".
 *
 * THREE ELEMENTS rather than a '···' string in a pseudo-element, and the reason is
 * concrete rather than stylistic: opacity cannot be animated per-character, so the dots
 * have to be separate boxes to travel. `.thinking-dots` a few hundred lines down in
 * index.css is the cautionary case — it puts the glyphs in a `::before` with no animation
 * and hangs the animation on an EMPTY `::after`, i.e. it animates a box with nothing in
 * it, so the "animated dots" its call site advertises have never moved. Not this wi's to
 * fix (tether#34's indicator, its own call site, its own tests), but it is the reason this
 * one is not built by copying it.
 *
 * `role="status"` + an aria-label rather than the bare glyphs: three decorative dots say
 * nothing to a reader who cannot see them, and this is the one moment where what is
 * happening is not otherwise announced.
 */
export function TranscriptDots({ label, className }: { label: string; className?: string }) {
  return (
    <span className={className ? `transcript-dots ${className}` : 'transcript-dots'} role="status" aria-label={label}>
      <span className="transcript-dot" />
      <span className="transcript-dot" />
      <span className="transcript-dot" />
    </span>
  )
}

// tether#63 — code→sentence map for the failed-connection card. Only the
// codes wire.ErrorCode classifies Terminal=true (errors.go's
// terminalCodes) need an entry; any other code (including one this frontend
// build predates — see wire.ErrorCode.Terminal's "unclassified defaults
// false" doc comment, which is about retryability, not this map) falls back
// to FATAL_GENERIC_MESSAGE below rather than rendering `undefined`.
//
// ("the four codes" until tether#101 added a fifth. The count is not load-bearing
// — the map is looked up, not iterated — but a stale number in a comment is how
// the next reader concludes the list is complete when it is not.)
export const FATAL_CODE_MESSAGES: Record<string, string> = {
  unknown_workspace: 'This workspace no longer exists on the daemon.',
  no_workspace_registry: "The daemon's workspace registry failed to load.",
  unknown_provider: 'The requested agent provider is not available on this daemon.',
  // NOT "another tab": clientID is per browser profile (persisted in
  // localStorage by AuthPage) and every tab shares it, so admitChat admits
  // them all. Reaching this means a different DEVICE holds the session — see
  // wire.ErrCodeSessionOwned's doc comment.
  session_owned_by_other: 'This session is open on another device.',
  // tether#101 — the ONE terminal code that describes a temporary state, so its
  // sentence is the only one here that has to point forward instead of closing the
  // door. Three things it must say, and each is checkable:
  //
  //  - it is being USED, not broken. The daemon reached this by asking the agent's
  //    own live-session registry after the agent refused the resume.
  //  - what the user can do about it, leading with the one thing that always works.
  //  - the ways out are the AGENT's own advice, quoted rather than invented:
  //    `claude agents` to take the session over, or --fork-session to branch a copy.
  //    Naming --fork-session is not offering it — tether deliberately does not fork
  //    on any path, because a fork mints a new id and diverges rather than resuming
  //    (see tether#101's rejected option C). It is here because the user may well
  //    want it and would otherwise have to be told by the agent's stderr, which goes
  //    to the daemon's log and not to them.
  //
  // # tether#104 — the order of the advice was wrong, and only the live case showed it
  //
  // This read "It becomes resumable when that finishes — or run `claude agents`…",
  // i.e. WAIT first and take over second. Measured against the session that
  // prompted tether#104: pid alive, kind bg, cc status `idle`, started three days
  // earlier, last status change hours before. Telling that user to wait is telling
  // them to wait for something that is not happening.
  //
  // The order is only the symptom. The sentence had the WRONG UNIT: cc refuses a
  // `--resume` on `kind && kind !== "interactive"` (ccregistry.go's
  // ccInteractiveKind), which is a property of the PROCESS, not of what it is
  // doing. So the hold ends when that process exits and at no other moment — a turn
  // finishing releases nothing. Saying so is also why this needs no busy/idle
  // branch: cc's status is not in the refusal rule, so both statuses have exactly
  // the same remedy and the same waiting condition. (CCLiveJob carries Kind and
  // JobID only, and FatalRefusal carries {code, message} — a branch here would
  // need a new field threaded through the daemon, which is a change to the fetch
  // path, for a distinction that changes no advice.)
  session_held_by_background_agent:
    'A background agent is using this conversation, so tether cannot send into it. Run '
    + '`claude agents` in a terminal to take it over, or `--fork-session` to work on a copy. '
    + 'Waiting works too, but the hold lasts as long as that agent’s process, not just its '
    + 'current turn — an idle job holds this conversation exactly as firmly as a busy one.',
}

// tether#104 — what the composer says while a background agent holds the session.
//
// The generic 'not connected' branch was true and useless: it named a transport
// the user has no model of, and said nothing about the two things that are
// actually on screen — that the conversation below IS readable, and that the box
// is disabled for a reason that has nothing to do with the network.
//
// Exported so the render test can pin it by identity as well as by literal, which
// is how a rewrite that changes the string has to change the test too rather than
// slipping past a `toMatch`.
export const HELD_SESSION_PLACEHOLDER = 'read-only — a background agent is using this conversation'

// tether#104 — the line that says the conversation below is there to be read.
// Nothing said this before; the card only ever talked about the connection.
//
// # Why it makes no claim about HOW MUCH of the conversation is below
//
// Because the answer depends on which store served it, and the card cannot see
// which. SessionIndex.Messages (sessionlist.go) prefers tether's own history —
// HistoryStore.LoadHistory is an os.ReadFile over the whole history.jsonl, no tail
// and no cap — and only falls through to CCStore, which reads a bounded tail
// (ccMessagesTailBytes, ccMessagesMax). Both are reachable under this refusal:
// Attachment.resolve computes hadConversation() and logs it as `had_history` on
// this very branch, and hadConversation is true when tether's HistoryStore has the
// sid. So "you are seeing recent messages only" would be false for a session
// tether recorded in full.
//
// The extent claim already exists where it is always true: SessionList renders
// EXTERNAL_SESSION_PROMISE ("recent messages only, with the calls it made; their
// output only where a call failed") gated on the session being external, i.e.
// exactly when the transcript came from CCStore — and it renders ABOVE this card,
// so on a cc-held session the user reads both. This line therefore states an upper
// bound that is true under either store and refines into that one rather than
// contradicting it.
//
// The second half used to read "as it stood when this pane fetched it", and it was
// true: the transcript was one HTTP GET at open time and, with the connection refused,
// nothing updated it. tether#106 made that false — the pane now probes the transcript's
// mtime every few seconds (transcriptWatch.ts) and re-reads it when it moves — so the
// sentence had to change with the behaviour. Leaving it would have been the worse
// defect of the two: a reader who believes they are looking at a still frame goes and
// reloads the page to check, which is the exact effort this change removes.
//
// # It says what TETHER does, not what the reader will see, and that is not hedging
//
// "new messages appear as that agent writes them" was the first draft and it is a claim
// this pane cannot keep. SessionIndex.Messages prefers tether's OWN history whenever it
// has one for the sid (sessionlist.go), and `history.jsonl` is written only by this
// daemon's fan-out — a foreign background agent writes cc's transcript and never
// touches it. So for a sid tether once recorded and a background job later resumed, the
// file this route serves is stale and STAYS stale: the probe correctly reports
// unchanged, forever. That combination is not hypothetical — the refusal branch logs it
// as `had_history` (attach.go) — and preferring the fuller transcript there is
// sessionlist.go's open question, not this change's to settle.
//
// So the sentence claims the two things that are true under both stores: tether keeps
// re-reading, and the reader does not have to reload the page. Which is also the whole
// of what they were doing by hand.
//
// "every few seconds" and not the number: TRANSCRIPT_POLL_MS is a tuning decision, and
// a sentence naming it is false the moment it is tuned.
export const HELD_SESSION_READABLE_NOTE =
  'You can read it: what tether has of this conversation is below, and tether keeps re-reading it every few seconds while this pane is open — you do not have to reload.'
const FATAL_GENERIC_MESSAGE = 'This connection was refused and cannot be retried automatically.'

// tether#47 — @-file mention. parseAtQuery locates the @token the caret is
// currently inside: scanning back from the caret, the token is valid only if an
// `@` is reached with no whitespace in between AND that `@` sits at the start of
// text or right after whitespace (so `a@b` — an email — is NOT a mention).
// Returns the `@` position and the query (text between `@` and the caret; empty
// right after typing `@`), or null when the caret isn't in a mention token.
export function parseAtQuery(text: string, caret: number): { atPos: number; query: string } | null {
  for (let i = caret - 1; i >= 0; i--) {
    const ch = text[i]
    if (ch === '@') {
      if (i === 0 || /\s/.test(text[i - 1])) return { atPos: i, query: text.slice(i + 1, caret) }
      return null // @ preceded by a non-space → not a mention (e.g. email)
    }
    if (/\s/.test(ch)) return null // whitespace before any @ → caret isn't in a mention
  }
  return null
}

// subseqScore returns -1 if q is not a case-insensitive subsequence of s, else a
// score where SMALLER is a better match: matches within the basename beat
// directory-only matches, then tighter spans, then earlier starts.
function subseqScore(s: string, q: string): number {
  let si = 0, first = -1, last = -1
  for (let qi = 0; qi < q.length; qi++) {
    const found = s.indexOf(q[qi], si)
    if (found < 0) return -1
    if (first < 0) first = found
    last = found
    si = found + 1
  }
  const base = s.lastIndexOf('/') + 1
  const inBasenamePenalty = first >= base ? 0 : 1000
  return inBasenamePenalty + (last - first) * 10 + first
}

// fuzzyRankFiles filters `files` to fuzzy (subsequence) matches of `query` and
// returns the best `limit`, ranked by subseqScore. An empty query returns the
// first `limit` files unchanged (show-all on a bare `@`). Pure — no DOM/fetch.
export function fuzzyRankFiles(files: string[], query: string, limit: number): string[] {
  const q = query.trim().toLowerCase()
  if (!q) return files.slice(0, limit)
  const scored: { f: string; score: number }[] = []
  for (const f of files) {
    const score = subseqScore(f.toLowerCase(), q)
    if (score >= 0) scored.push({ f, score })
  }
  scored.sort((a, b) => a.score - b.score || a.f.length - b.f.length || (a.f < b.f ? -1 : 1))
  return scored.slice(0, limit).map(x => x.f)
}

interface Props {
  onMenuClick?: () => void
}

export default function ChatPane({ onMenuClick: _onMenuClick }: Props) {
  const { messages, notices, sessionId, pendingPermissions, resolvePermission, streaming, streamingMsgId, curTurnId, fatal,
    transcriptEarlier, transcriptOtherRecord } = useStore()
  // tether#57 — what the pane actually renders: server-truth `messages` and
  // locally-originated `notices` recombined here, at render time. They are kept
  // apart in the store precisely so the history refetch that session_ready
  // triggers cannot replace a notice out of existence.
  const transcript = useMemo(() => mergeTranscript(messages, notices), [messages, notices])
  const [input, setInput] = useState('')
  const [connState, setConnState] = useState<ConnState>('connecting')
  const [connError, setConnError] = useState<string | null>(null)
  // tether#104 — the read-only READING state, named once because three places ask:
  // the card's dressing, the line that points at the transcript, and the
  // composer's placeholder. It is deliberately the CONJUNCTION rather than the
  // code alone — the presentation below is only honest once the ladder has
  // actually stopped (connState 'failed'), and reading it off `fatal` alone would
  // let a mid-reconnect frame claim a state the pane is not in.
  const readingHeldSession = connState === 'failed' && fatal?.code === ErrCodeSessionHeldByBackgroundAgent
  const [reconnectIn, setReconnectIn] = useState(0)
  const [sessionStart, setSessionStart] = useState<number | null>(null)
  const [_elapsed, setElapsed] = useState('')
  const [slashOpen, setSlashOpen] = useState(false)
  const [slashIndex, setSlashIndex] = useState(0)
  const [isComposing, setIsComposing] = useState(false)
  // tether#47 — @-file mention menu (workspace file fuzzy picker).
  const [atOpen, setAtOpen] = useState(false)
  const [atItems, setAtItems] = useState<string[]>([])
  const [atIndex, setAtIndex] = useState(0)
  const [atTruncated, setAtTruncated] = useState(false) // workspace file list hit the cap
  const activeWorkspace = useStore(s => s.activeWorkspace)
  // Per-workspace file-list cache, keyed by workspace id and holding the fetch
  // PROMISE (not the resolved array) so concurrent onChange+onSelect calls share
  // one request; resolves to {files,truncated}. Filtered client-side thereafter.
  const treeCacheRef = useRef<Map<string, Promise<{ files: string[]; truncated: boolean }>>>(new Map())
  const [showEmpty, setShowEmpty] = useState(false)
  // Which message ids have their fenced block expanded to the full variant.
  const [expandedBlocks, setExpandedBlocks] = useState<Set<string>>(() => new Set())
  const toggleBlock = (id: string) => setExpandedBlocks(prev => {
    const next = new Set(prev)
    if (next.has(id)) next.delete(id); else next.add(id)
    return next
  })
  // Which message ids have their collapsed thinking block expanded (tether#34).
  // Live thinking (before the answer) always renders expanded; once the answer
  // starts it collapses to a one-line "thought Xs" summary that this Set re-opens.
  const [expandedThinking, setExpandedThinking] = useState<Set<string>>(() => new Set())
  const toggleThinking = (id: string) => setExpandedThinking(prev => {
    const next = new Set(prev)
    if (next.has(id)) next.delete(id); else next.add(id)
    return next
  })
  const writerRef = useRef<WritableStreamDefaultWriter<Uint8Array> | null>(null)
  const wtRef = useRef<TetherWT | null>(null)
  const controlRef = useRef<ControlClient | null>(null)
  const attemptRef = useRef(0)
  // tether#63 — when the current connection's handshake completed, or 0 when
  // none is open. Read once in onClose to decide whether that connection lasted
  // long enough to refund the attempt budget (shouldRefundAttemptBudget).
  const connectedAtRef = useRef(0)
  const reconnectTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)
  const countdownRef = useRef<ReturnType<typeof setInterval> | null>(null)
  const unmountedRef = useRef(false)
  // tether#52 — guards the mount effect's deferred-first-connect path so the
  // store-subscription and the 2s fallback timer (both of which can fire)
  // trigger doConnect at most once. Reset at the top of that effect on each
  // run (relevant under StrictMode's dev double-invoke).
  const firstConnectedRef = useRef(false)
  const chatRef = useRef<HTMLDivElement>(null)
  const taRef = useRef<HTMLTextAreaElement>(null)
  // tether#107 — "load earlier messages" is in flight. Local, not store state: it is
  // about this pane's button, and nothing else has any use for it.
  const [loadingEarlier, setLoadingEarlier] = useState(false)
  // tether#110 — the same, for the newest page: a re-read the reader asked for by
  // arriving at the bottom. Its own flag rather than a shared one because the two put
  // dots at opposite ends of the transcript.
  const [loadingNewer, setLoadingNewer] = useState(false)
  // …and the SYNCHRONOUS half of those two, covering both ends and the button as one
  // fact. The state flags above are what render; this ref is what decides, and the
  // difference is not tidiness: `setLoadingEarlier(true)` is visible a render away, so two
  // scroll events landing in one tick would both read `loadingEarlier === false` and both
  // fire. Holding at most one transcript request at a time is also what keeps one end's
  // indicator from changing the scroll height the other end's anchor was measured against.
  const requestInFlightRef = useRef(false)
  // tether#110 — the two latches, one per end. `false` means "the reader has not moved
  // away from this end since it last fired", so an end fires once per visit rather than
  // once per scroll event. See transcriptEdgeAction for why they start false and why they
  // are refs. `…AtRef` is when that end last fired, for the interval floor.
  const topArmedRef = useRef(false)
  const topFiredAtRef = useRef(0)
  const bottomArmedRef = useRef(false)
  const bottomFiredAtRef = useRef(0)
  // Where the scroll box was standing when the reader asked for an older page, so the
  // layout effect below can put them back on the same message. Null when no prepend is
  // pending, which is also what makes that effect a no-op on every other commit.
  const prependAnchorRef = useRef<{ top: number; height: number } | null>(null)
  // One-shot: the commit that lands a prepended page must not also autoscroll. Consumed
  // by the autoscroll effect — see the anchor effect for why the flag is necessary and
  // not merely defensive.
  const skipAutoscrollRef = useRef(false)
  // tether#108 — which message ids are currently wearing the arrival trace, and what was
  // on screen the last time this pane looked. The seen-set is keyed by sid so a session
  // switch cannot carry one conversation's ids into another and count the whole of the new
  // transcript as having "arrived"; null means this pane has not seen a transcript yet.
  const [arrived, setArrived] = useState<Set<string>>(() => new Set())
  const seenRef = useRef<{ sid: string; ids: Set<string> } | null>(null)
  const [providers, setProviders] = useState<string[]>(['claude-code'])
  const [selectedProvider, setSelectedProvider] = useState(
    () => localStorage.getItem('tether_default_provider') ?? 'claude-code'
  )

  useEffect(() => {
    authedFetch('/api/v1/providers')
      .then(r => r.json() as Promise<ProviderListResponse>)
      .then(d => { if (d.providers?.length > 0) setProviders(d.providers) })
      .catch(() => {})
  }, [])

  // Sync default provider when changed from Settings panel (same-window custom event)
  useEffect(() => {
    const onProviderChange = (e: Event) => {
      const p = (e as CustomEvent<string>).detail
      if (p) setSelectedProvider(p)
    }
    window.addEventListener('tether:provider-changed', onProviderChange)
    return () => window.removeEventListener('tether:provider-changed', onProviderChange)
  }, [])

  // tether#45 — restore the last session on (re)mount so history loads from
  // /messages IMMEDIATELY, without waiting for session_ready. session_ready is
  // sent only after cc emits system/init, which in stream-json input mode needs
  // a fresh prompt (wt_chat.go) and is unreliable under cc --resume contention
  // (zombie spawn, mem_ruSB7HHI) — so a plain reload otherwise showed an empty
  // "new" session. Setting sessionId here fires the history-load effect below,
  // which fetches /messages over HTTP (independent of cc). A later session_ready
  // re-confirms the same sid (cc --resume keeps its id) as a no-op; a different
  // sid re-fires the effect, but its msgs.length>0 guard drops an EMPTY /messages
  // so it can't wipe restored history (a non-empty payload for that sid replaces,
  // intentionally). A DELIBERATE switch is the other case and does not come
  // through here — see lib/session.ts openSession, which owns that load and
  // explains why its guards differ from this effect's (tether#61).
  useEffect(() => {
    if (!useStore.getState().sessionId) {
      const last = localStorage.getItem('tether_last_sid')
      if (last) useStore.getState().setSessionId(last)
    }
  }, [])

  // Load chat history when session ID is first established.
  useEffect(() => {
    if (!sessionId) return
    fetch(transcriptPath(sessionId))
      // tether#106 — read the version off the SAME response, because this effect is
      // the other way a transcript gets on screen (a page reload restores
      // tether_last_sid and lands here, never in openSession). Without it the watcher
      // that starts moments later has no baseline, treats the daemon's version as a
      // change, and pays a full transcript GET to learn what this request already
      // knew. Recorded only where loadHistory actually runs: the version describes
      // what is ON SCREEN, so recording it next to a load that was skipped would be a
      // claim about a transcript this effect declined to install.
      // tether#107 — the boundary headers ride along on the same response, because this
      // effect is the other way a transcript gets on screen (a page reload restores
      // tether_last_sid and lands here, never in openSession). Without them a reloaded
      // page would render no top-of-transcript marker at all until something else
      // refetched — i.e. the reader would be back to an unlabelled ceiling.
      .then(r => r.ok
        ? r.json().then((msgs: HistoryEntry[]) => ({ v: readTranscriptVersion(r), b: readTranscriptBounds(r), msgs }))
        : { v: 0, b: { earlier: null, otherRecord: null }, msgs: [] as HistoryEntry[] })
      .then(({ v, b, msgs }: { v: number; b: { earlier: number | null; otherRecord: string | null }; msgs: HistoryEntry[] }) => {
        // Don't clobber an in-flight turn (tether#42 fix). On the FIRST send of
        // a new session, session_ready sets sessionId and fires this effect;
        // /messages already has the just-persisted user msg, so loadHistory
        // would run and reset streaming/curTurn — wiping the optimistic
        // "thinking…" indicator (streaming set in sendMessage) during the gap
        // before the first token. While a turn is streaming the live stream is
        // authoritative, so skip the reload.
        if (msgs.length > 0 && !useStore.getState().streaming) {
          useStore.getState().loadHistory(msgs.map(historyEntryToMessage))
          // Recorded only where loadHistory actually runs, for the reason the version
          // comment above gives: these describe what is ON SCREEN, and recording them
          // next to a load this effect declined to install would be a claim about a
          // transcript that is not there.
          useStore.getState().setTranscriptBounds(b)
          noteTranscriptVersion(sessionId, v)
        }
      })
      .catch(() => {})
  }, [sessionId])

  useEffect(() => {
    if (sessionId) setSessionStart(Date.now())
    else setSessionStart(null)
  }, [sessionId])

  useEffect(() => {
    if (!sessionStart) { setElapsed(''); return }
    setElapsed(fmtElapsed(sessionStart))
    const id = setInterval(() => setElapsed(fmtElapsed(sessionStart)), 30_000)
    return () => clearInterval(id)
  }, [sessionStart])

  // tether#107 — put the reader back on the message they were looking at after an older
  // page lands above it.
  //
  // A LAYOUT effect, and that is the whole reason it can work: it runs after the DOM has
  // the new bubbles and before the browser paints, so the correction is invisible. In a
  // passive effect the reader would see the transcript jump and then jump back.
  //
  // It also sets skipAutoscrollRef, which is NECESSARY rather than defensive. The
  // autoscroll effect below fires on this same commit (both `transcript.length` and
  // `grown` moved) and decides from the post-correction geometry. For a transcript that
  // FITTED THE VIEWPORT before the prepend, oldScrollHeight === clientHeight, so after
  // the correction `scrollHeight - scrollTop - clientHeight` is exactly 0 — under the
  // 120px `nearBottom` threshold — and the pane would snap to the bottom of the page the
  // reader just asked to see the top of. The threshold itself is untouched: this skips
  // one commit, the one whose entire purpose is to not move.
  useLayoutEffect(() => {
    const anchor = prependAnchorRef.current
    if (anchor === null) return
    prependAnchorRef.current = null
    const el = chatRef.current
    if (!el) return
    el.scrollTop = scrollAfterPrepend(anchor, el.scrollHeight)
    skipAutoscrollRef.current = true
  }, [messages])

  // Scroll to bottom on new messages AND when streaming text accumulates
  const grown = transcriptTextLength(transcript)
  useEffect(() => {
    const el = chatRef.current
    if (!el) return
    // tether#107 — the prepend commit already decided where this container stands. One
    // commit only, consumed here, so the next arriving message autoscrolls exactly as it
    // always has.
    if (skipAutoscrollRef.current) {
      skipAutoscrollRef.current = false
      return
    }
    // Always scroll during streaming; otherwise only if already near bottom
    const nearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 120
    if (streaming || nearBottom) {
      el.scrollTop = el.scrollHeight
    }
  }, [transcript.length, grown, streaming])

  // tether#108 — let an arrival be SEEN.
  //
  // tether#106 gave a held session's transcript a three-second refresh and made it
  // completely silent: the other agent's new turn simply materialises, and a reader who
  // looked away has no way to tell that anything happened. This marks what arrived so the
  // CSS can fade a tint out behind it. It is not a spinner and the distinction is not
  // rhetorical: a spinner says "you are waiting", which is a state, while a trace says "one
  // just landed", which is an event and stops being shown.
  //
  // Its population is bounded by the refresh's, and that bound is worth naming because it
  // is invisible from here: SessionIndex.MessagePage prefers tether's own history.jsonl
  // whenever it has one, and only this daemon writes that file — so for a sid tether once
  // recorded and a foreign background job later resumed, the served transcript never
  // changes, the probe never fires, and nothing here can ever run. That is the same
  // residual HELD_SESSION_READABLE_NOTE is hedged around, not a new one.
  //
  // Gated on `readingHeldSession`, the same gate tether#106 put on the refresh — which is
  // what makes the population exactly right rather than approximately right: that state is
  // the ONLY one in this app where messages arrive with no other signal. A connected session
  // announces arrivals already (the thinking dots, the streaming body, and the reader's own
  // send), and tracing there would flash the user's own message back at them.
  //
  // # Two things are NOT arrivals, and both are excluded by one rule
  //
  // A commit whose arrivals are the WHOLE array is a (re)population rather than an
  // arrival, and there are two of those. The first transcript load is one — the pane can
  // reach this state before or after the mount fetch lands, and flashing an entire
  // conversation because it just appeared on screen would be a trace that fires exactly
  // when nothing arrived. tether#107's disjoint fallback is the other: when over a
  // megabyte is written between two probes, `mergeHistory` reports no overlap and
  // `loadHistory` replaces the window wholesale, so every id is new. That case already
  // announces itself — the transcript visibly jumps — and lighting all of it up would say
  // "twenty-five messages just landed" when what happened was one replacement.
  //
  // Checked on the OUTPUT rather than by a flag each path has to remember to set, so a
  // future third way of replacing the array is covered without being enumerated.
  //
  // What that rule COSTS, since it is a real case and not a corner: the first message to
  // arrive into an EMPTY held transcript is also a whole-array answer, so it is not traced.
  // Kept rather than special-cased, because the two are the same array shape and telling
  // them apart would mean threading "this was a refresh" through from the watcher — and
  // because that is the one arrival a reader cannot miss anyway: the pane goes from having
  // no conversation to having one. The trace is for content appearing AMONG content, which
  // is where you cannot see what moved. The second message is traced.
  useEffect(() => {
    if (!readingHeldSession || !sessionId) {
      // Forget what was on screen. Re-entering the state later then counts as a first
      // sight, which is the honest answer: whatever arrived while nobody was looking did
      // not "just land".
      seenRef.current = null
      return
    }
    const prev = seenRef.current
    const ids = new Set(messages.map(m => m.id))
    seenRef.current = { sid: sessionId, ids }
    // "Have we seen THIS sid's transcript?" — the definition of first sight, and the
    // reason the ref carries a sid at all rather than a bare Set.
    //
    // Measured honestly: dropping the `prev.sid !== sessionId` half is a mutant that
    // SURVIVES this repo's suite, and it survives because it cannot be reached rather
    // than because nothing looks. Message ids are per-load `crypto.randomUUID()`s
    // (historyEntryToMessage) and `loadHistory` only ever carries an id forward to a
    // message with the same role+ts, so no id can cross a session switch — which makes
    // the arrivals for a freshly-switched sid the WHOLE array, which the repopulation
    // rule below already drops. Kept anyway because it says the invariant where it is
    // decided instead of leaving it to be re-derived from two other facts, and noted
    // here so the next reader does not have to rediscover that it is not falsifiable.
    if (prev === null || prev.sid !== sessionId) return
    const fresh = trailingArrivals(prev.ids, messages)
    if (fresh.length === 0 || fresh.length === messages.length) return
    setArrived(new Set(fresh))
  }, [messages, sessionId, readingHeldSession])

  // Take the trace back off. Keyed on `arrived` itself, so a second arrival inside the
  // window restarts the timer via the cleanup rather than letting the first one cut the
  // second one short. The functional update keeps an already-empty set's identity, which is
  // what stops this effect from re-arming a timer for a set it just cleared.
  useEffect(() => {
    if (arrived.size === 0) return
    const t = setTimeout(() => setArrived(prev => (prev.size === 0 ? prev : new Set())), ARRIVAL_TRACE_MS)
    return () => clearTimeout(t)
  }, [arrived])

  // tether#46 — auto-grow the composer textarea to fit its content, up to
  // MAX_COMPOSER_LINES then scroll internally. Reset to 'auto' first so the
  // measured scrollHeight shrinks when text is deleted (and after send clears
  // `input`, this floors it back to one line). growHeight owns the clamp.
  const growComposer = () => {
    const ta = taRef.current
    if (!ta) return
    ta.style.height = 'auto'
    const { height, scroll } = growHeight(ta.scrollHeight, { lineHeightPx: COMPOSER_LINE_PX, maxLines: MAX_COMPOSER_LINES })
    ta.style.height = `${height}px`
    ta.style.overflowY = scroll ? 'auto' : 'hidden'
  }
  useEffect(() => { growComposer() }, [input]) // eslint-disable-line react-hooks/exhaustive-deps
  // Recompute on WIDTH changes (right column is user-resizable via ColResizer;
  // sidebar/drawer toggle; window resize) so a multi-line draft rewrapping taller
  // doesn't clip under overflow-y:hidden until the next keystroke. Width-guarded
  // so our own height writes (which ResizeObserver would otherwise echo) can't
  // feedback-loop. jsdom lacks ResizeObserver → guard keeps tests/SSR safe.
  useEffect(() => {
    const ta = taRef.current
    if (!ta || typeof ResizeObserver === 'undefined') return
    let lastW = ta.clientWidth
    const ro = new ResizeObserver(() => {
      if (ta.clientWidth !== lastW) { lastW = ta.clientWidth; growComposer() }
    })
    ro.observe(ta)
    return () => ro.disconnect()
  }, []) // eslint-disable-line react-hooks/exhaustive-deps

  // Empty-state hint, debounced so it doesn't flash on session resume before
  // history arrives (connState flips to 'connected' before /messages loads).
  useEffect(() => {
    const empty = transcript.length === 0 && connState === 'connected' && !streaming && pendingPermissions.length === 0
    if (!empty) { setShowEmpty(false); return }
    const t = setTimeout(() => setShowEmpty(true), 500)
    return () => clearTimeout(t)
  }, [transcript.length, connState, streaming, pendingPermissions.length])

  const cancelPendingReconnect = () => {
    if (reconnectTimerRef.current !== null) { clearTimeout(reconnectTimerRef.current); reconnectTimerRef.current = null }
    if (countdownRef.current !== null) { clearInterval(countdownRef.current); countdownRef.current = null }
  }

  const scheduleReconnect = () => {
    if (unmountedRef.current) return
    // tether#63 — clear any timer/countdown already pending before arming new
    // ones. Since a close can now be reported by either the `closed` handler or
    // connect()'s own chain (wt.ts), two calls in quick succession are possible,
    // and the second used to overwrite countdownRef without clearing the first
    // interval — a leaked setInterval double-decrementing the countdown.
    cancelPendingReconnect()
    attemptRef.current += 1
    useStore.getState().setConnection({ state: 'reconnecting', attempt: attemptRef.current })
    if (attemptRef.current > RECONNECT_MAX_ATTEMPTS) {
      setConnState('failed')
      useStore.getState().setConnection({ state: 'dropped' })
      return
    }
    const delayMs = Math.min(RECONNECT_BASE_MS * 2 ** (attemptRef.current - 1), RECONNECT_MAX_MS)
    setConnState('reconnecting')
    setReconnectIn(Math.ceil(delayMs / 1000))
    countdownRef.current = setInterval(() => setReconnectIn(prev => Math.max(0, prev - 1)), 1000)
    reconnectTimerRef.current = setTimeout(() => { cancelPendingReconnect(); if (!unmountedRef.current) doConnect() }, delayMs)
  }

  // eslint-disable-next-line react-hooks/exhaustive-deps
  const doConnect = () => {
    // tether#52 — mark the first connect as done HERE, not only in the mount
    // effect's startOnce. manualRetry (the `tether:retry-connection` event, which
    // openSession, the error banner and the WT pill all dispatch) calls doConnect
    // directly, so a retry that lands inside the deferred window would otherwise
    // still be followed by the gate's own connect — tearing down the connection
    // the user's action just opened.
    firstConnectedRef.current = true
    cancelPendingReconnect()
    setConnState('connecting')
    setConnError(null)
    useStore.getState().setConnection({ state: 'connecting' })
    // tether#63 — a fresh attempt deserves a fresh chance: whatever refused
    // the PREVIOUS connection (e.g. a since-closed other tab holding the
    // session) may no longer apply, and this is a deliberate new handshake,
    // not the reconnect ladder retrying the same one automatically.
    useStore.getState().clearFatal()

    const old = wtRef.current
    wtRef.current = null
    writerRef.current?.releaseLock()
    writerRef.current = null
    old?.close()

    controlRef.current?.stop()
    controlRef.current = null

    // Resume last session if available — keeps history consistent across refreshes.
    const lastSid = localStorage.getItem('tether_last_sid') ?? ''
    // tether#52 — read the browsed workspace via getState(), NOT the reactive
    // `activeWorkspace` selector below (that one exists for the @-mention
    // picker's re-renders). A plain read here means a workspace switch while
    // connected can never retrigger this closure — the only thing that can
    // start a new connection is a fresh mount or an explicit retry/reconnect,
    // which is exactly what must stay true: a live session's workspace is
    // immutable, so browsing elsewhere must not tear down the WebTransport
    // (see the mount effect below and chatUrl.ts's doc comment for why).
    const wsID = useStore.getState().activeWorkspace?.id ?? ''
    const url = chatURL({ host: location.host, provider: selectedProvider, sid: lastSid, wsID })
    const wt = new TetherWT({
      url,
      onEnvelope: useStore.getState().handleEnvelope,
      onClose: () => {
        useStore.getState().setConnected(false)
        controlRef.current?.stop()
        controlRef.current = null
        // tether#63 — refund the attempt budget only for a connection that
        // lasted long enough to have been usable, THEN decide about retrying.
        // Order matters: refunding after the decision would let a refused
        // connection hand its successor a full budget.
        const upMs = connectedAtRef.current === 0 ? 0 : Date.now() - connectedAtRef.current
        connectedAtRef.current = 0
        if (shouldRefundAttemptBudget(upMs)) attemptRef.current = 0
        // A terminal refusal recorded by handleEnvelope's 'error' case (see
        // store.ts) means retrying THIS connection is pointless.
        // shouldReconnectAfterClose is the one place that decision is made, so
        // it can be pinned by a unit test without mounting the pane.
        const isFatal = useStore.getState().fatal !== null
        if (!shouldReconnectAfterClose({ unmounted: unmountedRef.current, fatal: isFatal })) {
          if (isFatal) {
            // Stop the ladder outright rather than let scheduleReconnect fire
            // once more and immediately refuse again — same daemon, same
            // workspace/session, same answer. 'dropped' (not 'reconnecting')
            // reflects that nothing is scheduled to retry.
            cancelPendingReconnect()
            setConnState('failed')
            useStore.getState().setConnection({ state: 'dropped' })
          }
          return
        }
        scheduleReconnect()
      },
    })
    wtRef.current = wt

    wt.connect().then(async () => {
      // tether#63 — the attempt budget is NOT refunded here. See
      // shouldRefundAttemptBudget: a handshake is not evidence that the
      // connection is usable, and refunding on one is what made the bounded
      // ladder unbounded. connectedAtRef starts the clock the close handler
      // measures against.
      connectedAtRef.current = Date.now()
      useStore.getState().setConnected(true)
      setConnState('connected')
      const stream = await wt.openBidiStream()
      writerRef.current = stream.writable.getWriter()

      // Start the control channel (ping/pong RTT) only after the main
      // connection is live — setConnected(true) already reset latency:0,
      // so pushing samples now won't be immediately clobbered.
      const control = new ControlClient()
      controlRef.current = control
      void control.start()
    }).catch((err: unknown) => {
      const msg = err instanceof Error ? err.message : String(err)
      console.error('[tether] chat connect failed:', msg)
      setConnError(msg)
      // tether#63 — the same terminal check as onClose, because this chain can
      // report the death first: before the daemon held refused sessions open
      // (refusalDrainGrace), `openBidiStream()` threw here in the same tick the
      // refusal killed the session, and that — not onClose — is what drove the
      // endless loop. Gating only onClose would have left this path retrying a
      // refusal the daemon has already explained.
      const isFatal = useStore.getState().fatal !== null
      if (!shouldReconnectAfterClose({ unmounted: unmountedRef.current, fatal: isFatal })) {
        // Only an unmounted pane gets no bookkeeping — there is no UI left to
        // put a state on, and the cleanup effect already cancelled the timer.
        if (isFatal && !unmountedRef.current) {
          cancelPendingReconnect()
          setConnState('failed')
          useStore.getState().setConnection({ state: 'dropped' })
        }
        return
      }
      scheduleReconnect()
    })
  }

  const manualRetry = () => { attemptRef.current = 0; doConnect() }

  // Keep a live ref to manualRetry so the window listener (attached once) always
  // invokes the latest closure without re-binding on every render.
  const manualRetryRef = useRef(manualRetry)
  manualRetryRef.current = manualRetry

  // App-level error UI (banner "retry now", catch-up modal "reconnect",
  // WT pill) asks this pane — owner of the WT connection — to retry.
  useEffect(() => {
    const onRetry = () => manualRetryRef.current()
    window.addEventListener('tether:retry-connection', onRetry)
    return () => window.removeEventListener('tether:retry-connection', onRetry)
  }, [])

  // tether#106 — follow a transcript that has no live stream behind it.
  //
  // Gated on `readingHeldSession` and nothing weaker. That flag is #104's conjunction
  // (the ladder has actually stopped AND the daemon said a background agent holds this
  // conversation), so it is the one state in which two things are simultaneously true:
  // there is provably no stream that could deliver the new messages, and there is
  // provably no in-flight turn whose optimistic bubble a reload could wipe. Every other
  // state — connected, reconnecting, or failed for one of the other four terminal codes
  // — keeps today's behaviour exactly, which is what makes this change unable to
  // reintroduce the tether#42 / tether#57 class of regression.
  //
  // The watcher owns the interval, the visibility pause and the "did it actually
  // change" comparison; all that is left here is what to do when it did.
  useEffect(() => {
    if (!readingHeldSession || !sessionId) return
    return watchTranscript(sessionId, () => { void refreshTranscript(sessionId) })
  }, [readingHeldSession, sessionId])

  // tether#106 — the same gate, for the click on the already-open row.
  //
  // lib/session.ts's openSession raises this rather than reloading itself, because
  // "is there a live stream" is a fact this pane holds and that module does not (see
  // REFRESH_TRANSCRIPT_EVENT). Ignoring the event outside this state is therefore not
  // a missing feature — it is the tether#61 guard still doing its job: clicking the
  // highlighted row while connected must remain, as it always was, nothing at all.
  //
  // Unconditional where it does fire: this is a deliberate act by the reader, and it is
  // the path that still works when the automatic one could not complete (a probe that
  // saw a change and a reload that then failed leaves the version already advanced —
  // see transcriptWatch's probe).
  useEffect(() => {
    if (!readingHeldSession || !sessionId) return
    const onRefresh = () => { void refreshTranscript(sessionId) }
    window.addEventListener(REFRESH_TRANSCRIPT_EVENT, onRefresh)
    return () => window.removeEventListener(REFRESH_TRANSCRIPT_EVENT, onRefresh)
  }, [readingHeldSession, sessionId])

  // tether#104 named this button "Check again" because the question it asks is "has
  // that agent's process exited yet". tether#106 makes it ask the other question the
  // reader has at the same moment — "and is there anything new to read" — because the
  // two are one gesture, and the connection attempt alone answers only the first.
  const checkAgain = () => {
    if (sessionId) void refreshTranscript(sessionId)
    manualRetry()
  }

  // tether#107 — the action the top of a truncated transcript offers.
  //
  // The scroll box is measured BEFORE the request, not in the .then: by the time the
  // response lands the reader may have scrolled on, and the anchor has to be where they
  // were when they asked. It is read straight off the element rather than from React
  // state because scroll position is not React's to know.
  //
  // tether#110 — the guard moved from `loadingEarlier` to `requestInFlightRef`, and that
  // is a strengthening rather than a rename: the state flag is a render away, so it could
  // not stop two calls in one tick, and since this is now reached from a scroll handler
  // as well as from the button, two calls in one tick are ordinary rather than a double
  // click. The ref also covers the OTHER end, which is what makes "at most one transcript
  // request exists" true rather than approximately true.
  const loadEarlier = () => {
    if (!sessionId || requestInFlightRef.current) return
    const el = chatRef.current
    prependAnchorRef.current = el ? { top: el.scrollTop, height: el.scrollHeight } : null
    // Read BEFORE the request, so the settle below can tell whether a page actually
    // landed. `transcriptPagesBack` is the honest measure of that: prependHistory is the
    // only thing that raises it, and it raises it only when it prepends.
    const pagesAtClick = useStore.getState().transcriptPagesBack
    requestInFlightRef.current = true
    setLoadingEarlier(true)
    void loadEarlierTranscript(sessionId).finally(() => {
      requestInFlightRef.current = false
      setLoadingEarlier(false)
      // A request that failed must leave NO anchor pending, or the next unrelated
      // commit would apply a stale correction to a scroll position it has nothing to do
      // with. loadEarlierTranscript swallows its own errors, so this is the only place
      // that can tell — and it works whichever side of React's commit this callback
      // lands on: after it, the layout effect has already cleared the anchor, and
      // clearing an already-null ref is nothing.
      //
      // The residual, stated: a DISJOINT refresh landing inside this window resets the
      // counter to 0, so a click made at pagesBack > 0 leaves the anchor pending. The
      // next commit then shifts the reader by that commit's height delta. Cosmetic, in
      // a state where the transcript has just visibly reset anyway.
      if (useStore.getState().transcriptPagesBack === pagesAtClick) prependAnchorRef.current = null
    })
  }

  /**
   * tether#110 — the action arriving at the BOTTOM of the transcript takes: re-read the
   * newest page now, rather than waiting out the rest of the three-second poll.
   *
   * # It is `refreshTranscript` and nothing else
   *
   * Not a second fetch that happens to hit the same URL. tether#109 changed that function
   * to CHECK the ordering it used to assume — `mergeHistory` compares `ord`s and refuses
   * four ways — and a parallel path would bypass that check and put the out-of-order
   * defect back. It is also what keeps `transcriptPagesBack` meaningful: a reader who has
   * paged back gets a merge, not the wholesale replace that would throw those pages away.
   *
   * # Why this end is gated on `readingHeldSession` and the other end is not
   *
   * Two reasons, and either alone would be enough.
   *
   * `refreshTranscript` has NO `!streaming` guard, and its doc says why: "every caller is
   * a state in which a turn cannot be in flight". A scroll to the bottom of a CONNECTED
   * session is not such a state — it is the single most ordinary thing to do while a turn
   * streams — so wiring this end in every state would break that function's stated
   * precondition and hand back tether#42's regression, the refetch wiping an in-flight
   * turn's optimistic bubble.
   *
   * And there would be nothing to short-circuit. The three-second poll this exists to
   * pre-empt (watchTranscript, below) only runs under `readingHeldSession`; everywhere
   * else new messages arrive on the WebTransport stream as they are produced, so the
   * newest page is already on screen and a re-read is a megabyte spent to learn nothing.
   *
   * The cost where it DOES run, stated rather than implied: GET /messages is up to a
   * megabyte on the wire and an unbounded `os.ReadFile` on the daemon (transcriptWatch's
   * header states the figures). Bounded to at most one per arrival at the bottom by the
   * latch, one per 500ms by the floor, and one at a time by both `requestInFlightRef` and
   * `refreshTranscript`'s own per-sid dedupe. That is the same trade `checkAgain` and the
   * click-the-open-row path already make for a deliberate gesture, and arriving at the
   * bottom is one.
   */
  //
  // WHICH of the two in-flight checks is doing the work, stated because a mutation
  // battery had to establish it and the answer is "not this one": deleting
  // `requestInFlightRef.current` from the line below leaves the whole suite green,
  // because the only caller — `onTranscriptScroll` — has already refused through
  // `transcriptEdgeAction`'s `inFlight`. Kept anyway, and the distinction from the inert
  // `!streaming` guard `refreshTranscript` argues against is that this condition is
  // reachable rather than dead (the ref is genuinely set for the duration of a load at
  // the other end): it makes "this may not be entered while a request is in flight" a
  // property of the function that sets the flag, not of whoever calls it, and it keeps
  // this the same shape as `loadEarlier`, where the duplicate IS live because the button
  // is a second caller. The decision-site check is not redundant with it either — that
  // one is also what stops the latch being consumed and the floor stamped for a request
  // that is never going to happen.
  const refreshNewest = () => {
    if (!sessionId || requestInFlightRef.current) return
    requestInFlightRef.current = true
    setLoadingNewer(true)
    void refreshTranscript(sessionId).finally(() => {
      requestInFlightRef.current = false
      setLoadingNewer(false)
    })
  }

  /**
   * tether#110 — both ends, one scroll event.
   *
   * Kept in a ref-to-latest rather than re-bound on every render, the same construction
   * `manualRetryRef` uses a few dozen lines up: the listener is attached once, to the
   * element, and a listener that had to be re-attached whenever `loadingEarlier` or
   * `transcriptEarlier` changed would be re-attached on exactly the commits where a scroll
   * event is most likely to be in flight.
   *
   * Both ends are evaluated on every event, and that is deliberate: on a transcript barely
   * taller than the viewport a reader can be within `TRANSCRIPT_EDGE_PX` of both at once,
   * and `transcriptEdgeAction`'s `inFlight` (shared, synchronous) is what decides which
   * one actually fires rather than an ordering rule that would silently prefer one end.
   */
  const onTranscriptScroll = () => {
    const el = chatRef.current
    if (!el) return
    const now = Date.now()
    const inFlight = requestInFlightRef.current

    const top = transcriptEdgeAction({
      distance: el.scrollTop,
      armed: topArmedRef.current,
      // A cursor is the whole of "there is an earlier page". At the ceiling this is null,
      // which is what stops the pane from spinning where nothing more can arrive.
      available: transcriptEarlier !== null && !!sessionId,
      inFlight,
      sinceLastMs: now - topFiredAtRef.current,
    })
    if (top === 'arm') topArmedRef.current = true
    if (top === 'load') {
      topArmedRef.current = false
      topFiredAtRef.current = now
      loadEarlier()
    }

    const bottom = transcriptEdgeAction({
      distance: el.scrollHeight - el.scrollTop - el.clientHeight,
      armed: bottomArmedRef.current,
      available: readingHeldSession && !!sessionId,
      // Re-read, not the snapshot above: `loadEarlier` sets the ref synchronously, so an
      // event that fired the top end must not also fire this one.
      inFlight: requestInFlightRef.current,
      sinceLastMs: now - bottomFiredAtRef.current,
    })
    if (bottom === 'arm') bottomArmedRef.current = true
    if (bottom === 'load') {
      bottomArmedRef.current = false
      bottomFiredAtRef.current = now
      refreshNewest()
    }
  }
  const onTranscriptScrollRef = useRef(onTranscriptScroll)
  onTranscriptScrollRef.current = onTranscriptScroll

  // Attach once, to the scroll container itself.
  //
  // `addEventListener` rather than React's `onScroll` prop for two reasons that are not
  // interchangeable: `{ passive: true }` (a scroll listener the browser must wait on
  // before scrolling is a jank source on touch, and this one never calls
  // preventDefault), and because a listener this file owns is one a test can reach with
  // `dispatchEvent` without depending on how React attaches non-delegated events.
  //
  // NOT an IntersectionObserver, which is the other obvious shape. jsdom does not
  // implement one, so an observer-based trigger would be untestable in this repo — and
  // tether#108 already paid for shipping scroll behaviour that no test in this repo could
  // express. Scroll position also covers wheel, trackpad, touch and Home/End alike, which
  // is why nothing here hand-rolls a pull gesture.
  useEffect(() => {
    const el = chatRef.current
    if (!el) return
    const handler = () => onTranscriptScrollRef.current()
    el.addEventListener('scroll', handler, { passive: true })
    return () => el.removeEventListener('scroll', handler)
  }, [])

  // A session switch retires both latches. They describe where the reader has been in ONE
  // conversation, and carrying "you have already loaded at this end" into a different one
  // would suppress the first automatic load of the session they just opened.
  useEffect(() => {
    topArmedRef.current = false
    topFiredAtRef.current = 0
    bottomArmedRef.current = false
    bottomFiredAtRef.current = 0
  }, [sessionId])

  // tether#52 — first-connect ordering (see shouldDeferFirstConnect above).
  // Only the sid-less path defers, and only until `workspacesLoaded` flips
  // true or a 2s fallback elapses — whichever comes first — so a hung/failed
  // /api/v1/workspaces degrades to today's behaviour (connect with no `ws`)
  // rather than never connecting. `activeWorkspace` itself is intentionally
  // NOT in this effect's reactive surface (no dependency, no selector) — this
  // effect decides WHEN the first connect fires, never RE-fires it, which is
  // what guarantees browsing a different workspace later can't tear down a
  // live WebTransport (see doConnect's wsID comment / chatUrl.ts).
  useEffect(() => {
    unmountedRef.current = false
    firstConnectedRef.current = false
    const startOnce = () => {
      if (firstConnectedRef.current) return // double-connect guard
      firstConnectedRef.current = true
      doConnect()
    }
    const cleanupConnection = () => {
      unmountedRef.current = true
      cancelPendingReconnect()
      writerRef.current?.releaseLock()
      wtRef.current?.close()
      controlRef.current?.stop()
      controlRef.current = null
    }

    const hasLastSid = !!(localStorage.getItem('tether_last_sid') ?? '')
    if (!shouldDeferFirstConnect({ hasLastSid, workspacesLoaded: useStore.getState().workspacesLoaded })) {
      startOnce()
      return cleanupConnection
    }

    // Deferred: wait for WorkspacePane's fetch to settle, or bail out after
    // WORKSPACE_GATE_TIMEOUT_MS so a hung request can't block chat forever.
    //
    // The store update that opens the gate ALSO carries the workspace
    // (store.ts settleWorkspaces) — this listener runs synchronously inside it and
    // connects immediately, so a gate that opened one update before the value was
    // published would connect with no workspace. That was this slice's original bug;
    // see settleWorkspaces.
    let fallback: ReturnType<typeof setTimeout> | undefined
    const unsub = useStore.subscribe((s) => {
      if (s.workspacesLoaded) { unsub(); clearTimeout(fallback); startOnce() }
    })
    fallback = setTimeout(() => { unsub(); startOnce() }, WORKSPACE_GATE_TIMEOUT_MS)

    return () => {
      unsub()
      clearTimeout(fallback)
      cleanupConnection()
    }
  }, [])

  const sendMessage = async () => {
    const text = input.trim()
    if (!text || !writerRef.current) return
    setSlashOpen(false)
    setAtOpen(false) // tether#47 review MINOR-1 — don't leave a stale @ menu after send
    useStore.getState().addMessage({ id: crypto.randomUUID(), role: 'user', text, ts: Date.now() })
    // Light up the "thinking" indicator immediately: `streaming` otherwise
    // only flips true on the first agent event, leaving a blind gap after send
    // where the user can't tell whether the agent is working or stalled.
    // streamingMsgId stays null so the thinking-dots (not a text cursor) show.
    useStore.setState({ streaming: true, streamingMsgId: null })
    const line = JSON.stringify({ text }) + '\n'
    try { await writerRef.current.write(new TextEncoder().encode(line)) } catch (err) { console.error('[tether] send failed:', err) }
    setInput('')
  }

  // T12 click-to-work (tether#20) — programmatic send, bypassing the `input`
  // React state entirely (it's async; setInput() then sendMessage() would
  // race and send the PREVIOUS value). Mirrors sendMessage's write path.
  const doInjectAndSend = (text: string) => {
    if (!writerRef.current) return
    useStore.getState().addMessage({ id: crypto.randomUUID(), role: 'user', text, ts: Date.now() })
    useStore.setState({ streaming: true, streamingMsgId: null })
    const line = JSON.stringify({ text }) + '\n'
    writerRef.current.write(new TextEncoder().encode(line))
      .catch(err => console.error('[tether] inject send failed:', err))
  }

  // Queued text waiting for a live writer — set whenever injectAndSend is
  // called before the WT connection (and its writer) is ready. Flushed by
  // the connState effect below, with a bounded retry loop for the narrow
  // race where connState flips to 'connected' just BEFORE writerRef.current
  // is assigned in doConnect (see the .then() there).
  const pendingInjectRef = useRef<string | null>(null)
  const pendingInjectDeadlineRef = useRef(0)

  const tryFlushPendingInject = () => {
    const text = pendingInjectRef.current
    if (text === null) return
    if (writerRef.current) {
      pendingInjectRef.current = null
      doInjectAndSend(text)
      return
    }
    if (Date.now() > pendingInjectDeadlineRef.current) {
      console.error('[tether] inject-prompt timed out waiting for connection')
      pendingInjectRef.current = null
      return
    }
    setTimeout(tryFlushPendingInject, 150)
  }

  useEffect(() => {
    if (connState === 'connected') tryFlushPendingInject()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [connState])

  const injectAndSend = (text: string) => {
    const trimmed = text.trim()
    if (!trimmed) return
    if (writerRef.current && connState === 'connected') {
      doInjectAndSend(trimmed)
      return
    }
    // Not connected (or writer not yet assigned) — queue it and start
    // polling; up to ~5s, same budget as the composer disabling itself
    // while reconnecting.
    pendingInjectRef.current = trimmed
    pendingInjectDeadlineRef.current = Date.now() + 5_000
    tryFlushPendingInject()
  }

  // Live ref so the once-attached window listener always calls the latest
  // closure (mirrors manualRetryRef below).
  const injectAndSendRef = useRef(injectAndSend)
  injectAndSendRef.current = injectAndSend

  useEffect(() => {
    const onInject = (e: Event) => injectAndSendRef.current((e as CustomEvent<string>).detail)
    window.addEventListener('tether:inject-prompt', onInject)
    return () => window.removeEventListener('tether:inject-prompt', onInject)
  }, [])

  // tether#61 — ChatPane used to own "switch to session X" (switchSession) and
  // publish it as a `tether:switch-session` window event for WorkDetail's
  // click-to-work to call. That operation now lives in lib/session.ts
  // openSession, which its callers import directly, so both the local copy and
  // the event relay are gone: one implementation, reached one way. ChatPane's
  // remaining part in a switch is the reconnect, which arrives on the
  // pre-existing `tether:retry-connection` channel above — it owns the WT.

  // D-19 §5 / tether#8 T8 — DagBlock's approve button. Sends an "action"
  // ClientFrame on the /wt/control channel, which is not otherwise
  // session-scoped, so the current sessionId travels in the frame itself;
  // the daemon routes it to that session's agent (Registry.DeliverAction).
  // Best-effort like the ping/pong RTT probe: no ack is awaited, and if
  // sessionId or blockId aren't known yet the click is a no-op.
  const sendApprove = (block: FencedBlock) => {
    const sessionId = useStore.getState().sessionId
    if (!sessionId || !block.blockId) return
    void controlRef.current?.sendAction({
      kind: ClientFrameAction,
      sessionId,
      blockId: block.blockId,
      action: 'approve',
      skill: block.skill,
    })
  }

  // D-19 §5 / tether#8 T9 — DagBlock's pause button. Mirrors sendApprove
  // exactly (same frame shape, same best-effort no-ack semantics); only the
  // `action` value differs. The daemon routes "pause" to
  // Registry.InterruptSession (agent.Session.Interrupt) instead of
  // DeliverAction/SendPrompt — see docs/wire/fenced-contract.md §5.
  const sendPause = (block: FencedBlock) => {
    const sessionId = useStore.getState().sessionId
    if (!sessionId || !block.blockId) return
    void controlRef.current?.sendAction({
      kind: ClientFrameAction,
      sessionId,
      blockId: block.blockId,
      action: 'pause',
      skill: block.skill,
    })
  }

  // tether#42 — session-level interrupt (stop the streaming turn). Unlike
  // sendPause (DAG-card scoped, needs blockId), the daemon's "pause" action
  // routes by SessionID alone (control.go handleActionFrame → InterruptSession
  // → cc control_request{interrupt}), so no blockId. cc aborts the turn and
  // stays resumable; it emits no EventResult, so we finalize locally too.
  const sendStop = () => {
    const sessionId = useStore.getState().sessionId
    if (sessionId) {
      void controlRef.current?.sendAction({ kind: ClientFrameAction, sessionId, action: 'pause' })
    }
    useStore.getState().stopTurn()
  }

  const handleInputChange = (v: string) => {
    setInput(v)
    // Only while typing the command token itself (no space yet). Once args begin,
    // close the menu so Enter sends the message instead of re-picking the command.
    setSlashOpen(v.startsWith('/') && !v.includes(' '))
    setSlashIndex(0)
    refreshAtMenu() // tether#47 — recompute the @-mention menu from the new value + caret
  }

  const filteredSlash = SLASH_CMDS.filter(c => c.cmd.startsWith(input.split(' ')[0]))

  const pickSlash = (c: { cmd: string }) => {
    setInput(c.cmd + ' ')
    setSlashOpen(false)
    setSlashIndex(0)
  }

  // tether#47 — fetch the active workspace's flat file list for the @-mention
  // picker, memoized by workspace as a PROMISE so onChange+onSelect firing in the
  // same tick share ONE request (review MINOR-2 in-flight dedup). Resolves to
  // {files,truncated}; on error resolves empty AND drops the cache so a later @
  // retries. Successful results stay cached for the session.
  const ensureTree = (wsId: string): Promise<{ files: string[]; truncated: boolean }> => {
    const existing = treeCacheRef.current.get(wsId)
    if (existing) return existing
    const p = (async () => {
      try {
        const r = await fetch(`/api/v1/workspaces/${encodeURIComponent(wsId)}/tree`)
        if (!r.ok) { treeCacheRef.current.delete(wsId); return { files: [], truncated: false } }
        const data = (await r.json()) as { files?: string[]; truncated?: boolean }
        return { files: data.files ?? [], truncated: data.truncated === true }
      } catch { treeCacheRef.current.delete(wsId); return { files: [], truncated: false } }
    })()
    treeCacheRef.current.set(wsId, p)
    return p
  }

  // refreshAtMenu recomputes the @ menu from the textarea's live value + caret.
  // Called on input and on caret moves (onSelect). No active @token / no browsed
  // workspace → menu closes. First @ in a workspace awaits one fetch; after that
  // the promise cache resolves synchronously-fast. Re-parses the query when the
  // fetch resolves so late-arriving files rank against the CURRENT query, not the
  // one captured when the fetch started (review MINOR-2 stale-query race).
  const refreshAtMenu = () => {
    const ta = taRef.current
    const ws = useStore.getState().activeWorkspace
    if (!ta || !ws) { setAtOpen(false); return }
    if (!parseAtQuery(ta.value, ta.selectionStart ?? ta.value.length)) { setAtOpen(false); return }
    void ensureTree(ws.id).then(({ files, truncated }) => {
      // Re-read the query NOW (the user may have typed on during the fetch).
      const t = taRef.current
      const q = t ? parseAtQuery(t.value, t.selectionStart ?? t.value.length) : null
      if (!q) { setAtOpen(false); return }
      const ranked = fuzzyRankFiles(files, q.query, AT_MENU_MAX)
      setAtItems(ranked); setAtIndex(0); setAtTruncated(truncated); setAtOpen(ranked.length > 0)
    })
  }

  // pickAt inserts the chosen file as an absolute @<path> mention, splicing it
  // over the active @query and restoring the caret after the inserted token.
  // Absolute so cc resolves it regardless of its (decoupled) cwd (tether#47).
  const pickAt = (rel: string) => {
    const ta = taRef.current
    const ws = useStore.getState().activeWorkspace
    if (!ta || !ws) return
    const caret = ta.selectionStart ?? ta.value.length
    const q = parseAtQuery(ta.value, caret)
    if (!q) return
    const token = '@' + ws.path.replace(/\/+$/, '') + '/' + rel + ' '
    const next = ta.value.slice(0, q.atPos) + token + ta.value.slice(caret)
    setInput(next)
    setAtOpen(false)
    const newCaret = q.atPos + token.length
    requestAnimationFrame(() => {
      const t = taRef.current
      if (t) { t.focus(); t.setSelectionRange(newCaret, newCaret) }
    })
  }

  return (
    <>
      {/* ── Session list (tether#91) ──────────────────────────
          Moved here from the bottom of the file tree, where it was a category
          error: the left column is about files. Collapsed by default so it costs
          the transcript no height until asked for. Its own component because this
          file is long enough. */}
      <SessionList />

      {/* ── Message list ──────────────────────────────────── */}
      <div className="dt-chat scroll-thin" ref={chatRef}>

        {connState === 'reconnecting' && (
          <div className="reconnect-banner">
            <span style={{ width: 6, height: 6, borderRadius: 999, background: 'var(--warn)', flexShrink: 0 }} />
            <span>reconnecting in {reconnectIn}s</span>
            <span
              onClick={manualRetry}
              style={{ marginLeft: 'auto', color: 'var(--ink-tertiary)', cursor: 'pointer', textDecoration: 'underline', fontSize: 11 }}
            >retry now</span>
          </div>
        )}

        {/* tether#104 — one code's dressing changes here, and only its dressing.
            `readingHeldSession` gates every difference; every other terminal code
            renders exactly the bytes it rendered before. The card itself is NOT
            restructured — FATAL_CODE_MESSAGES is looked up by code, so the way to
            give one code a different presentation is a conditional at the three
            points that differ, not a second card. */}
        {connState === 'failed' && (
          <div className={readingHeldSession ? 'failed-card state-card' : 'failed-card'}>
            {fatal ? (
              // tether#63 — the daemon told us WHY, and it was terminal: lead
              // with the code→sentence translation (falling back to a generic
              // sentence for a code this frontend build doesn't recognize —
              // see FATAL_CODE_MESSAGES' doc comment), then the daemon's own
              // message text for anyone who wants the raw detail.
              <>
                {/* tether#104 — --danger is an assertion, and for this one code it
                    is a false one: nothing failed, something else is using the
                    conversation. The sentence carries the meaning either way, so
                    losing the red costs no information to a reader who cannot
                    separate the two tints. */}
                <div className="failed-card-headline" style={{ color: readingHeldSession ? 'var(--ink-primary)' : 'var(--danger)', fontWeight: 600, marginBottom: 4 }}>
                  {FATAL_CODE_MESSAGES[fatal.code] ?? FATAL_GENERIC_MESSAGE}
                </div>
                {/* tether#108 — is that agent actually doing anything? The daemon has
                    answered this every three seconds since tether#103 and this pane asked
                    nothing. Above the read-only note because it is the more urgent of the
                    two: the note says the conversation is readable, this says whether
                    waiting for more of it is worth anything.

                    NOT gated on transcript.length, unlike the note below. The note makes a
                    claim about the screen and needs something on it; this makes a claim
                    about the other process, and an empty transcript is precisely where a
                    reader most needs to know whether to wait.

                    Mounted only in this state, and that is the whole cost control — see
                    HeldSessionActivity. `sessionId` is guarded because the component keys
                    its subscription on it, and an empty sid would ask about no session. */}
                {readingHeldSession && sessionId && <HeldSessionActivity sid={sessionId} />}
                {/* Gated on there BEING a transcript. "The conversation is below"
                    is a claim about the screen, and an empty transcript is a
                    normal answer here — SessionIndex.Messages returns SourceNone
                    with a nil slice for a sid neither store has — so making it
                    unconditionally would trade one false sentence for another. */}
                {readingHeldSession && transcript.length > 0 && (
                  <div className="state-card-read">{HELD_SESSION_READABLE_NOTE}</div>
                )}
                <div style={{ color: 'var(--ink-tertiary)', fontSize: 11, marginBottom: 8, wordBreak: 'break-all' }}>{fatal.message}</div>
              </>
            ) : (
              <>
                <div className="failed-card-headline" style={{ color: 'var(--danger)', fontWeight: 600, marginBottom: 4 }}>WebTransport connection failed</div>
                {connError && <div style={{ color: 'var(--ink-tertiary)', fontSize: 11, marginBottom: 6, wordBreak: 'break-all' }}>{connError}</div>}
                <div style={{ color: 'var(--ink-tertiary)', fontSize: 11, marginBottom: 8 }}>UDP/QUIC may be blocked — see K.8.1 in README.</div>
              </>
            )}
            {/* Same button, different word — and since tether#106, a different
                handler. "Retry" names a failed thing to attempt again; here the
                attempt is a question — has that agent's process exited yet — and the
                answer is not tether's to predict. `checkAgain` adds the other question
                the reader has at that moment (see its definition); every other code
                still gets `manualRetry` and nothing else. */}
            <button onClick={readingHeldSession ? checkAgain : manualRetry} className="btn-ghost-sm">{readingHeldSession ? 'Check again' : 'Retry'}</button>
          </div>
        )}

        {/* ── The top of the transcript (tether#107) ──────────────────────────
            Before this there was NOTHING here, and that absence was the bug: a
            reader who scrolled to the top of a 117 MiB conversation saw the same
            screen whether they had reached the beginning or hit a 1 MiB ceiling.

            Three states, and it renders in ALL THREE. That is the point rather than
            thoroughness: if the "you have reached the beginning" line were
            conditional, its absence would be ambiguous again, which is the state
            this element exists to end.

            Gated on there being messages. "The beginning of this conversation" above
            an empty transcript is a new false claim, and an empty transcript is a
            normal answer here — SessionIndex.MessagePage returns SourceNone with no
            messages for a sid neither store has, which is exactly what openSession
            fetches for a session created moments ago. Same gate, same argument, as
            HELD_SESSION_READABLE_NOTE.

            Gated on `messages` and not `transcript`: a session whose only content is
            a locally-originated notice has no transcript to be at the top of.

            tether#110 — the loading state moved OUT of the button and became the dots,
            at the top of the transcript, where the page is about to appear. The ceiling
            branch below is untouched, and deliberately so: those are the three sentences
            #107 exists for, and dots there would be the exact lie this lane keeps
            re-deciding against. */}
        {messages.length > 0 && (
          <div className="transcript-top">
            {transcriptEarlier !== null ? (
              /* A ONE-CELL GRID holding the dots and the button, one of them `on`, and
                 that is the load-bearing part rather than a layout convenience.

                 This element sits ABOVE the point an older page is inserted, so its
                 height is an input to `scrollAfterPrepend`. A height change that lands
                 ON the prepend commit is already absorbed — that function reads the
                 post-commit `scrollHeight`, so the delta it computes includes it. The
                 dangerous one is the OTHER commit: button → dots at request start,
                 which nothing corrects, so a reader standing at the top would be shoved
                 by the difference between a button and three dots at the moment they
                 asked for more.

                 tether#108's construction, for tether#108's reason: `min-height: <px>`
                 does not hold a height (what wraps at 640px does not wrap at 260px, and
                 the phone is narrower still), and jsdom computes no layout while iOS
                 Safari has no scroll anchoring, so this class of defect is not
                 observable from any test in this repo. Stacked in one cell the height is
                 the taller of the two at whatever width the pane happens to be, it
                 contains no pixel value to go stale, and it never changes.

                 The #107 note stays OUTSIDE this grid. Its swap with this cell happens
                 only when `transcriptEarlier` goes non-null → null, which is a prepend
                 commit and therefore already absorbed — and keeping it out is what keeps
                 "there is no `.transcript-top-note` while a page is available" an
                 assertion a test can make. */
              <div className="transcript-top-slots">
                <TranscriptDots label={TRANSCRIPT_DOTS_EARLIER_LABEL} className={loadingEarlier ? 'on' : ''} />
                {/* Still here, and not because removing it was forgotten. Scrolling is
                    now the way to page back, so this is the fallback — but it is a
                    fallback that has to exist: a transcript whose newest page does not
                    overfill the viewport CANNOT SCROLL, so no scroll event ever fires and
                    every earlier page would be unreachable without it. It is also the
                    only keyboard-reachable path, since `.dt-chat` is a plain div and
                    takes no focus. `disabled` is redundant with `visibility: hidden`
                    while loading and kept anyway: it is the correct state for a control
                    that is present and unavailable, and it costs one attribute. */}
                <button
                  className={loadingEarlier ? 'transcript-more' : 'transcript-more on'}
                  onClick={loadEarlier}
                  disabled={loadingEarlier}
                >load earlier messages</button>
              </div>
            ) : (
              <span className="transcript-top-note">
                {transcriptOtherRecord ? describeOtherRecord(transcriptOtherRecord) : TRANSCRIPT_START_COMPLETE}
              </span>
            )}
          </div>
        )}

        {/* tether#108 — `arrivalTrace` is appended to the ROW's class (the .msg-user /
            .msg-system / .msg-ai container, not the bubble inside it) and nothing else
            changes. Deliberately a class and not a wrapper element: the trace animates
            background-color only (see .msg-arrived), so it cannot reflow, and an extra
            wrapper around a row WOULD be able to. `key={m.id}` is untouched, so React
            reconciles exactly as before and neither expansion Set notices. */}
        {transcript.map((m) => {
          const arrivalTrace = arrived.has(m.id) ? ' msg-arrived' : ''
          if (m.role === 'user') {
            return (
              <div key={m.id} className={`msg-user${arrivalTrace}`}>
                <div className="msg-user-bubble">{m.text}</div>
                <div className="msg-user-time">you · {fmtTime(m.ts)}</div>
                <CopyButton className="msg-copy" getText={() => m.text} label="Copy message" />
              </div>
            )
          }
          if (m.role === 'system') {
            // tether#50 — a daemon notice (e.g. "the previous context could not
            // be restored"). Rendered as its own quiet centred line rather than
            // an assistant bubble: it did not come from the model, and dressing
            // it up with the tether avatar would read as the agent saying it.
            return (
              <div key={m.id} className={`msg-system${arrivalTrace}`}>
                <span className="msg-system-text">{m.text}</span>
              </div>
            )
          }
          return (
            <div key={m.id} className={`msg-ai${arrivalTrace}`}>
              <div className="msg-ai-header">
                <span className="msg-ai-avatar">
                  <Icon name="tether" size={10} style={{ color: 'white' }} />
                </span>
                <AnswerMeta ts={m.ts} answerMs={m.answerMs} usage={m.usage} />
                {m.text && <CopyButton className="msg-copy" getText={() => m.text} label="Copy answer" />}
              </div>
              {m.thinking && (
                <ThinkingBlock
                  thinking={m.thinking}
                  thinkingMs={m.thinkingMs}
                  live={streaming && m.id === curTurnId && !m.text}
                  expanded={expandedThinking.has(m.id)}
                  onToggle={() => toggleThinking(m.id)}
                />
              )}
              {m.tools && m.tools.length > 0 && <ToolCallList tools={m.tools} />}
              {m.block && (
                <div className="msg-ai-block">
                  <FencedBlockView
                    block={m.block}
                    expanded={expandedBlocks.has(m.id)}
                    onToggle={() => toggleBlock(m.id)}
                    onApprove={sendApprove}
                    onPause={sendPause}
                  />
                </div>
              )}
              {(m.text || (!m.block && streaming && m.id === streamingMsgId)) && (
                <AnswerBody text={m.text} streaming={streaming && m.id === streamingMsgId} />
              )}
            </div>
          )
        })}

        {/* ── The bottom of the transcript (tether#110) ──────────────────────
            Immediately after the last bubble, because that is where a new one will
            appear — the same argument the top marker makes about its own position.

            Only while a request is in flight, so this is an EVENT and not a state:
            arriving at the bottom asks the daemon now instead of waiting out the rest
            of the three-second poll, and the dots last exactly as long as that ask.
            There is nothing to render when the answer is "nothing new", which is the
            common case and correctly silent.

            Nothing here reserves height. It is BELOW everything, so it cannot move
            content the reader is looking at, and `requestInFlightRef` makes it
            impossible for these dots to appear or vanish between the top end's anchor
            capture and its prepend commit.

            One constraint this DOES carry, named because it is invisible from the CSS:
            these dots must stay SHORTER than TRANSCRIPT_EDGE_PX. A reader standing at
            the bottom has the scroll height grow underneath them when they appear, and
            an indicator taller than the threshold would push the distance past it and
            re-arm the latch it had just consumed. Three 5px dots in a 4px/2px padded
            row is ~13px against a 48px threshold. */}
        {loadingNewer && (
          <div className="transcript-bottom">
            <TranscriptDots label={TRANSCRIPT_DOTS_NEWER_LABEL} />
          </div>
        )}

        {showEmpty && (
          <div className="chat-empty mono">message tether to start a session</div>
        )}

        {/* Thinking indicator: animated dots while waiting for the first token —
            suppressed once the turn has a bubble (tether#34: thinking block or
            answer text), since that is itself the "working" signal. */}
        {streaming && !streamingMsgId && !curTurnId && (
          <div className="msg-ai">
            <div className="msg-ai-header">
              <span className="msg-ai-avatar">
                <Icon name="tether" size={10} style={{ color: 'white' }} />
              </span>
              <span className="msg-ai-name">tether</span>
            </div>
            <div className="msg-ai-body">
              <span className="thinking-dots" aria-label="Claude is thinking" />
            </div>
          </div>
        )}

        {pendingPermissions.length > 0 && (
          <PermissionQueue
            requests={pendingPermissions}
            onDecide={(id, allow) => { void postDecide(id, allow); resolvePermission(id) }}
            onDecideAll={(allow) => {
              // Snapshot ids first (resolvePermission mutates the queue as we go).
              for (const id of pendingPermissions.map((p) => p.id)) {
                void postDecide(id, allow)
                resolvePermission(id)
              }
            }}
          />
        )}
      </div>

      {/* ── Composer ──────────────────────────────────────── */}
      <div className="dt-composer">
        {slashOpen && filteredSlash.length > 0 && (
          <div className="slash-pop">
            <div className="slash-head">
              <span className="mono">/ commands</span>
              <span className="kbd">esc</span>
            </div>
            {filteredSlash.map((c, i) => (
              <div
                key={c.cmd}
                className={`slash-row${i === slashIndex ? ' on' : ''}`}
                onMouseEnter={() => setSlashIndex(i)}
                onClick={() => pickSlash(c)}
              >
                <span className="slash-cmd">{c.cmd}</span>
                <span className="slash-desc">{c.desc}</span>
                {i === slashIndex && <span className="kbd">↵</span>}
              </div>
            ))}
          </div>
        )}

        {/* tether#47 — @-mention file picker (reuses .slash-pop styling). */}
        {atOpen && atItems.length > 0 && (
          <div className="slash-pop at-pop">
            <div className="slash-head">
              <span className="mono">@ files{activeWorkspace ? ` · ${activeWorkspace.id.slice(0, 6)}` : ''}</span>
              <span className="kbd">esc</span>
            </div>
            {atItems.map((f, i) => (
              <div
                key={f}
                className={`slash-row${i === atIndex ? ' on' : ''}`}
                onMouseEnter={() => setAtIndex(i)}
                onClick={() => pickAt(f)}
              >
                <span className="slash-cmd at-file">{f}</span>
                {i === atIndex && <span className="kbd">↵</span>}
              </div>
            ))}
            {atTruncated && (
              <div className="at-more mono">workspace has more files — refine your query</div>
            )}
          </div>
        )}

        <div className="composer-box">
          {providers.length > 1 && (
            <div style={{ padding: '0 4px 6px', display: 'flex', alignItems: 'center', gap: 6 }}>
              <span style={{ fontSize: 11, color: 'var(--ink-quat)', fontFamily: 'var(--font-mono)' }}>provider</span>
              <select
                value={selectedProvider}
                onChange={e => setSelectedProvider(e.target.value)}
                style={{ background: 'transparent', color: 'var(--ink-secondary)', border: '1px solid var(--line-soft)', borderRadius: 3, padding: '2px 4px', fontSize: 11, fontFamily: 'var(--font-mono)' }}
              >
                {providers.map(p => <option key={p} value={p}>{p}</option>)}
              </select>
            </div>
          )}
          <div className="composer-row">
            <span className="composer-prefix">/</span>
            <textarea
              ref={taRef}
              rows={1}
              className="composer-input"
              disabled={connState !== 'connected'}
              value={input}
              onChange={e => handleInputChange(e.target.value)}
              onSelect={refreshAtMenu}
              onCompositionStart={() => setIsComposing(true)}
              onCompositionEnd={() => setIsComposing(false)}
              onKeyDown={e => {
                const slashActive = slashOpen && filteredSlash.length > 0
                const atActive = atOpen && atItems.length > 0
                // tether#47 — @-mention menu owns nav keys while open (checked
                // before the slash menu; they can't both be active).
                if (atActive && e.key === 'ArrowDown') {
                  e.preventDefault(); setAtIndex(i => (i + 1) % atItems.length); return
                }
                if (atActive && e.key === 'ArrowUp') {
                  e.preventDefault(); setAtIndex(i => (i - 1 + atItems.length) % atItems.length); return
                }
                if (atActive && (e.key === 'Tab' || e.key === 'Enter') && !isComposing) {
                  e.preventDefault(); pickAt(atItems[Math.min(atIndex, atItems.length - 1)]); return
                }
                if (atActive && e.key === 'Escape') { e.preventDefault(); setAtOpen(false); return }
                if (slashActive && e.key === 'ArrowDown') {
                  e.preventDefault(); setSlashIndex(i => (i + 1) % filteredSlash.length); return
                }
                if (slashActive && e.key === 'ArrowUp') {
                  e.preventDefault(); setSlashIndex(i => (i - 1 + filteredSlash.length) % filteredSlash.length); return
                }
                if (slashActive && (e.key === 'Tab' || e.key === 'Enter') && !isComposing) {
                  e.preventDefault(); pickSlash(filteredSlash[Math.min(slashIndex, filteredSlash.length - 1)]); return
                }
                // tether#46 — Enter sends, Shift+Enter inserts a newline (the
                // textarea handles the newline natively when we don't
                // preventDefault). shouldSendOnEnter also refuses to send during
                // IME composition, while streaming (the button is Stop then —
                // tether#42 review N1), or while a menu (slash/@) is open.
                if (shouldSendOnEnter({ key: e.key, shiftKey: e.shiftKey, isComposing, streaming, slashActive: slashActive || atActive })) {
                  e.preventDefault(); void sendMessage()
                } else if (e.key === 'Enter' && !e.shiftKey && !isComposing && streaming) {
                  // While a turn streams the button is Stop; swallow plain Enter
                  // so it neither sends nor inserts a stray newline (parity with
                  // the old single-line input; tether#46 review MINOR-1).
                  // Shift+Enter still adds a newline for composing the next msg.
                  e.preventDefault()
                }
                if (e.key === 'Escape') { setSlashOpen(false); setAtOpen(false) }
              }}
              // tether#104 — one branch added, and it is the only one that names a
              // REASON. 'not connected' stays the answer for every other way the
              // box can be disabled; it is not wrong there, it is just the most
              // general true thing, and this state has a specific one available.
              placeholder={
                connState !== 'connected'
                  ? connState === 'connecting' ? 'connecting…'
                    : readingHeldSession ? HELD_SESSION_PLACEHOLDER : 'not connected'
                  : streaming ? 'Claude is thinking…' : 'message tether…'
              }
            />
            {streaming ? (
              // tether#42 — while a turn streams, the send button becomes a stop
              // button (cc/ChatGPT-style) that interrupts the current turn.
              <button
                type="button"
                className="send-btn stop-btn"
                onClick={() => sendStop()}
                aria-label="Stop generating"
                title="Stop generating"
              >
                <span className="stop-glyph" aria-hidden="true" />
              </button>
            ) : (
              <button
                type="button"
                className="send-btn"
                disabled={connState !== 'connected'}
                onClick={() => void sendMessage()}
                aria-label="Send message"
                title="Send message"
              >
                <Icon name="arrow-up" size={13} />
              </button>
            )}
          </div>
          <div className="composer-foot">
            <span className="mono" style={{ fontSize: 10.5, color: 'var(--ink-tertiary)' }}>↵ send · ⇧↵ newline · / for commands</span>
            {sessionId && (
              <span className="mono" style={{ fontSize: 10.5, color: 'var(--ink-tertiary)', marginLeft: 'auto' }}>
                {selectedProvider}
              </span>
            )}
          </div>
        </div>
      </div>

    </>
  )
}

// fmtThinkMs formats a thinking duration for the collapsed summary: whole
// seconds as "8s", sub-10s with one decimal ("1.2s", "0.5s"), and >=1min as
// "Xm Ys". Empty string for undefined/negative (no duration to show yet).
export function fmtThinkMs(ms: number | undefined): string {
  if (ms == null || ms < 0) return ''
  if (ms < 60000) {
    const s = ms / 1000
    // >= ~10s (incl. 9.95–9.999 that would otherwise render "10.0s") and whole
    // seconds show without a decimal; otherwise one decimal ("1.2s", "0.5s").
    const str = s >= 9.95 || Number.isInteger(s) ? String(Math.round(s)) : s.toFixed(1)
    return `${str}s`
  }
  // Round to whole seconds FIRST, then split — avoids "1m 60s" at the boundary.
  const totalSec = Math.round(ms / 1000)
  return `${Math.floor(totalSec / 60)}m ${totalSec % 60}s`
}

// fmtTokens renders a token count compactly for the usage badge (tether#48):
// under 1k verbatim ("856"), then "k" ("1.2k", "46k"), then "M" ("1.4M").
// BOTH tiers apply fmtThinkMs's decimal rule (>= ~10 of a unit, or a whole
// value, drops the decimal — "10.0k"→"10k"). The k-tier stops at 999_500, not
// 1_000_000: above that, round(n/1000) would hit 1000 and render an ugly
// "1000k", so those roll into the M-tier ("1.0M") instead. With 1M-context
// models, near-1M input counts are realistic, so this seam matters. Empty
// string for undefined/negative.
export function fmtTokens(n: number | undefined): string {
  if (n == null || n < 0) return ''
  if (n < 1000) return String(n)
  if (n < 999_500) {
    const k = n / 1000
    return `${k >= 9.95 || Number.isInteger(k) ? String(Math.round(k)) : k.toFixed(1)}k`
  }
  const m = n / 1_000_000
  return `${m >= 9.95 || Number.isInteger(m) ? String(Math.round(m)) : m.toFixed(1)}M`
}

interface ThinkingBlockProps {
  thinking: string
  thinkingMs?: number
  /** True while this message is still actively accumulating thinking deltas
   *  (it is the store's curTurnId). Goes false the moment the answer starts OR
   *  the turn ends (result/error) — either way the block collapses, so a
   *  thinking-only turn (e.g. thinking → tool_use with no answer text) does not
   *  get stuck showing "thinking…" forever.
   *
   *  "the turn ends (error)" is why the call site conjoins `streaming`
   *  (tether#83). A non-terminal error no longer clears curTurnId — the turn it
   *  lands on may still be streaming — so curTurnId alone stopped answering this
   *  question on that path, and a cc turn killed mid-thinking (ccSession.abandon)
   *  would have sat on "thinking…" until its stream-end result arrived, or
   *  forever if that result were dropped as a slow-subscriber envelope.
   *
   *  It is therefore NOT monotonic within a turn, and tether#88 is where that
   *  became reachable: sending a prompt no longer ends the open turn, and
   *  sendMessage sets `streaming` back to true, so a thinking-only bubble whose
   *  block collapsed on a non-terminal error re-animates when the user types
   *  again. On the path that error actually describes — the turn is still
   *  running, which is tether#83's whole premise — that is the truth: that turn
   *  IS still thinking. It reads wrong only where the pointer is stale, which is
   *  the case store.ts's addMessage enumerates and accepts. */
  live: boolean
  expanded: boolean
  onToggle: () => void
}

// AnswerMeta — assistant bubble header meta (tether#36): name + time, plus an
// answer-duration badge once the turn completes (answerMs is stamped at result).
// Exported as a pure component so ChatPane.test.tsx tests the badge directly
// without mounting ChatPane (WebTransport).
export function AnswerMeta({ ts, answerMs, usage }: {
  ts: number
  answerMs?: number
  /** The turn's token usage (tether#48); renders a "⇅ in↑/out↓" badge when present. */
  usage?: { input: number; output: number }
}) {
  return (
    <>
      <span className="msg-ai-name">tether</span>
      <span className="msg-ai-time">{fmtTime(ts)}</span>
      {answerMs != null && <span className="msg-ai-dur">· {fmtThinkMs(answerMs)}</span>}
      {usage && (
        <span className="msg-ai-usage" title={`${usage.input} input / ${usage.output} output tokens`}>
          · ⇅ {fmtTokens(usage.input)}↑/{fmtTokens(usage.output)}↓
        </span>
      )}
    </>
  )
}

// AnswerBody — assistant answer text rendered as markdown (tether#35). Exported
// as a pure, prop-controlled component so ChatPane.test.tsx tests it directly
// without the WebTransport wiring. While streaming it gets a `.streaming` class;
// index.css paints the blinking cursor via .md-body::after (a block-level markdown
// tree can't host the old inline <span> cursor at the text tail).
export function AnswerBody({ text, streaming }: { text: string; streaming: boolean }) {
  return (
    <div className={streaming ? 'msg-ai-body streaming' : 'msg-ai-body'} aria-busy={streaming}>
      <Markdown text={text} />
    </div>
  )
}

// tether#37 — tool-call visibility. The daemon already forwards each tool_use as
// {name,input} (registry.go translateEvent); the store keeps them on the turn's
// bubble; this renders them as a compact activity log above the answer — one
// line per call: icon + name + a best-effort one-line arg summary. A turn can
// fire 10+ tools, so beyond TOOL_FOLD_THRESHOLD they collapse behind a
// "used N tools" toggle. Results arrived in tether#38 — this paragraph said "no tool
// result (that needs daemon tool_result parsing — a later slice)" until tether#97,
// in the same file as the code that has been rendering them since.
const TOOL_FOLD_THRESHOLD = 5

// The input field worth showing per known tool; unknown tools show name only.
const TOOL_ARG_FIELD: Record<string, string> = {
  Read: 'file_path', Write: 'file_path', Edit: 'file_path', NotebookEdit: 'notebook_path',
  Bash: 'command', Grep: 'pattern', Glob: 'pattern', Task: 'description',
  WebFetch: 'url', WebSearch: 'query',
}

const TOOL_ICON: Record<string, string> = {
  Read: '📖', Write: '📝', Edit: '✏️', NotebookEdit: '✏️', Bash: '⚡',
  Grep: '🔍', Glob: '🔍', Task: '🧩', WebFetch: '🌐', WebSearch: '🌐',
}

// summarizeToolInput derives the one-line arg summary from a tool_use input
// object. Best-effort + defensive: unknown tools, non-object input, or a missing/
// non-string field all yield '' (the row then shows the tool name alone).
// Exported so ChatPane.test.tsx covers it without rendering.
export function summarizeToolInput(name: string, input: unknown): string {
  if (!input || typeof input !== 'object') return ''
  const field = TOOL_ARG_FIELD[name]
  if (!field) return ''
  const val = (input as Record<string, unknown>)[field]
  if (typeof val !== 'string') return ''
  const s = val.trim().replace(/\s+/g, ' ')
  return s.length > 60 ? s.slice(0, 60) + '…' : s
}

// summarizeToolResult derives the one-line RESULT preview at the tool row tail
// (tether#38): Read/Write/Edit → line count, Grep/Glob → match count, errors → a
// short marker, else the first non-empty output line (truncated). Best-effort +
// defensive; '' when there's nothing useful to preview. Exported for tests.
export function summarizeToolResult(name: string, result: { content: string; isError: boolean }): string {
  if (result.isError) return 'error'
  const c = result.content ?? ''
  if (!c) return ''
  if (name === 'Read' || name === 'Write' || name === 'Edit' || name === 'NotebookEdit') {
    const n = c.replace(/\n+$/, '').split('\n').length
    return n === 1 ? '1 line' : `${n} lines`
  }
  if (name === 'Grep' || name === 'Glob') {
    const n = c.split('\n').filter(l => l.trim()).length
    return n === 1 ? '1 match' : `${n} matches`
  }
  const first = c.split('\n').find(l => l.trim()) ?? ''
  const s = first.trim().replace(/\s+/g, ' ')
  return s.length > 48 ? s.slice(0, 48) + '…' : s
}

const RESULT_MAX_LINES = 20
const RESULT_MAX_CHARS = 2000

// truncateResult clamps the expanded result block so a huge file / long stdout
// can't flood the chat; a trailing marker signals truncation. Exported for tests.
export function truncateResult(s: string): string {
  let out = s
  let cut = false
  if (out.length > RESULT_MAX_CHARS) { out = out.slice(0, RESULT_MAX_CHARS); cut = true }
  const lines = out.split('\n')
  if (lines.length > RESULT_MAX_LINES) { out = lines.slice(0, RESULT_MAX_LINES).join('\n'); cut = true }
  return cut ? out + '\n…(truncated)' : out
}

// ToolCallList — the per-turn tool activity log. Each row shows the call
// (icon + name + arg, tether#37); once its result arrives (tether#38) the row
// also shows a one-line result preview at the tail and becomes clickable to
// expand the full (truncated) result block below it. Exported + prop-controlled
// so ChatPane.test.tsx renders it directly (no WebTransport). List-fold (>5) and
// per-tool result-expand are both local state, not in the store.
export function ToolCallList({ tools }: { tools: ToolCall[] }) {
  const [open, setOpen] = useState(false)
  const [expanded, setExpanded] = useState<Set<string>>(() => new Set())
  const toggle = (key: string) => setExpanded(prev => {
    const next = new Set(prev)
    if (next.has(key)) next.delete(key); else next.add(key)
    return next
  })
  if (tools.length === 0) return null
  const foldable = tools.length > TOOL_FOLD_THRESHOLD
  const rowsHidden = foldable && !open
  return (
    <div className="msg-tools">
      {foldable && (
        <button type="button" className="msg-tool-fold" onClick={() => setOpen(o => !o)} aria-expanded={open}>
          <span className="msg-thinking-chevron">{open ? '⌄' : '›'}</span>
          <span>{open ? `${tools.length} tools` : `used ${tools.length} tools`}</span>
        </button>
      )}
      {!rowsHidden && tools.map((t, i) => {
        const key = t.id || String(i)
        const arg = summarizeToolInput(t.name, t.input)
        const preview = t.result ? summarizeToolResult(t.name, t.result) : ''
        const isOpen = expanded.has(key)
        // Expandability is a question about the CONTENT and nothing else: there
        // is a block to open only if there are words to put in it. A
        // present-but-empty result (a command with no stdout) would otherwise be
        // a dead click onto a blank block (review MINOR).
        //
        // `.trim()` and not `.length`, because whitespace is not words and the
        // rendered block would be just as blank. The daemon can serve it: for a cc
        // transcript, session.ccMessage.text keeps a `text` sub-block whenever it
        // is not the EMPTY string, so a result of "   " arrives with length 3.
        //
        // The condition used to be `|| t.result.isError`, which reopened that same
        // dead click for the one result that can be flagged AND empty: cc writes
        // `is_error` with content that flattens to nothing (an empty array, or only
        // image / tool_reference sub-blocks), and the daemon serves it on purpose —
        // dropping it would make a failed call read as a successful one
        // (session.ccMessage.errorResults). So the failure still has to reach the
        // screen, and it does, on the lines below: `preview` and the `err` class are
        // derived from t.result.isError independently of this flag, and
        // summarizeToolResult returns 'error' from the FLAG rather than from the
        // text. The row still says error; it no longer offers to expand into
        // nothing. tether#97.
        const hasResult = !!t.result && t.result.content.trim().length > 0
        return (
          <div key={key}>
            <div
              className={hasResult ? 'msg-tool-row clickable' : 'msg-tool-row'}
              onClick={hasResult ? () => toggle(key) : undefined}
            >
              <span className="msg-tool-icon">{TOOL_ICON[t.name] ?? '🔧'}</span>
              <span className="msg-tool-name">{t.name}</span>
              {arg && <span className="msg-tool-arg">{arg}</span>}
              {preview && (
                <span className={t.result?.isError ? 'msg-tool-preview err' : 'msg-tool-preview'}>{preview}</span>
              )}
              {hasResult && <span className="msg-tool-caret">{isOpen ? '⌄' : '▸'}</span>}
            </div>
            {hasResult && isOpen && (
              <pre className={t.result!.isError ? 'msg-tool-result err' : 'msg-tool-result'}>{truncateResult(t.result!.content)}</pre>
            )}
          </div>
        )
      })}
    </div>
  )
}

// Extended-thinking display (tether#34). While thinking is live it renders
// expanded ("thinking…"); once it stops being live (answer began, or turn ended)
// it collapses to a one-line "thought Xs" summary that clicking re-expands.
// Exported and prop-controlled so it unit-tests directly, without the ChatPane
// WebTransport wiring.
export function ThinkingBlock({ thinking, thinkingMs, live, expanded, onToggle }: ThinkingBlockProps) {
  if (live) {
    return (
      <div className="msg-thinking msg-thinking-live">
        <div className="msg-thinking-label">thinking…</div>
        <div className="msg-thinking-text"><Markdown text={thinking} /></div>
      </div>
    )
  }
  const dur = fmtThinkMs(thinkingMs)
  return (
    <div className="msg-thinking msg-thinking-done">
      <button type="button" className="msg-thinking-toggle" onClick={onToggle} aria-expanded={expanded}>
        <span className="msg-thinking-chevron">{expanded ? '⌄' : '›'}</span>
        <span className="msg-thinking-summary">thought{dur ? ` ${dur}` : ''}</span>
      </button>
      {expanded && <div className="msg-thinking-text"><Markdown text={thinking} /></div>}
    </div>
  )
}

interface FencedBlockViewProps {
  block: FencedBlock
  expanded: boolean
  onToggle: () => void
  /** D-19 §5 approve callback (tether#8 T8); only 'dag' wires it so far. */
  onApprove: (block: FencedBlock) => void
  /** D-19 §5 pause callback (tether#8 T9); only 'dag' wires it so far. */
  onPause: (block: FencedBlock) => void
}

// Dispatch a FencedBlock to its renderer by `kind` (D-19 §10.B.4).
// Unknown kinds fall back to a compact raw view rather than throwing.
function FencedBlockView({ block, expanded, onToggle, onApprove, onPause }: FencedBlockViewProps) {
  switch (block.kind) {
    case 'dag':        return <DagBlock block={block} expanded={expanded} onToggle={onToggle} onApprove={() => onApprove(block)} onPause={() => onPause(block)} />
    case 'form':       return <FormBlock block={block} expanded={expanded} onToggle={onToggle} />
    case 'candidates': return <CandidatesBlock block={block} expanded={expanded} onToggle={onToggle} />
    case 'media':      return <MediaBlock block={block} expanded={expanded} onToggle={onToggle} />
    default:
      return <div className="fb-fallback mono">unknown block: {block.kind}</div>
  }
}
