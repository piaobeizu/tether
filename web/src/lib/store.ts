import { create } from 'zustand'
import type { Envelope, ErrorPayload, FencedBlock } from './wire.gen'
// A value import, not a type: the generated const is the single source of the
// code string, so a rename on the Go side breaks this at build time instead of
// silently un-gating the branch below (tether#77, and ErrCodeAgent since
// tether#80).
import { ErrCodeAgent, ErrCodePromptUndelivered } from './wire.gen'

/** A single tool_use content block the daemon already extracts and puts on the
 *  wire ({name,input}); tether#37 is where the frontend finally KEEPS it instead
 *  of discarding it (the old tool_use branch only set the streaming flag). */
export interface ToolCall {
  id: string
  name: string
  input: unknown
  /** The tool's output, hung under the call (matched by tool_use_id) once the
   *  daemon forwards the tool_result (tether#38). Absent until then — but NOT
   *  live-only, which is what this said until tether#98: on tether's own sessions
   *  it rides along in `HistoryMessage.Tools` as `ToolCallRecord.Result`
   *  (internal/session/history.go) since tether#44, so it survives a reload with
   *  the call it hangs on. A cc session keeps only FAILED results — see `tools` on
   *  Message below. */
  result?: { content: string; isError: boolean }
}

/**
 * One chat message as rendered.
 *
 * Several fields below document whether they survive a page reload. That claim
 * is CHECKABLE in two places, and checking it beats inheriting it:
 *
 *   1. does `HistoryMessage` (internal/session/history.go) declare the field, and
 *   2. does historyEntryToMessage (below) copy it onto the Message?
 *
 * Both must hold for "survives a reload"; neither alone is enough. tether#44 made
 * `thinking` and `tools` persist, and these comments went on claiming "live-only"
 * from then until tether#98 — which is the failure the two-place check is cheap
 * enough to prevent. Do not take another field's word for it: tether#98 found
 * `tools` citing `thinking` as its authority while `thinking` was also wrong, so
 * following the citation CONFIRMED the error instead of catching it.
 */
export interface Message {
  id: string
  role: 'user' | 'assistant' | 'system'
  text: string
  ts: number
  /** Optional D-19 fenced block rendered inline in this message bubble. */
  block?: FencedBlock
  /** Accumulated extended-thinking text for this assistant turn (tether#34).
   *  PERSISTED since tether#44 — `HistoryMessage.Thinking` in
   *  internal/session/history.go, restored by historyEntryToMessage below — so it
   *  DOES survive a page reload. (Until tether#98 this said the opposite:
   *  "ephemeral / live-only … absent after a page reload (spec D3)", which
   *  described the pre-#44 daemon.) Still absent on history lines written before
   *  #44, because the Go field did not exist when they were written — not because
   *  of its `omitempty`, which only governs the write side. "Absent on old lines"
   *  is not the same claim as "never persisted". */
  thinking?: string
  /** Wall-clock ms spent thinking before the answer began (tether#34); set when
   *  the first answer delta arrives. Undefined while thinking is still live. */
  thinkingMs?: number
  /** Wall-clock ms spent generating the answer (tether#36): first answer delta →
   *  result. Stamped at result as the turn's "done" signal. Live-only (not
   *  persisted), so absent after a page reload. */
  answerMs?: number
  /** Tool calls (Read/Bash/Edit/…) the agent made during this turn (tether#37),
   *  in arrival order. PERSISTED since tether#44 — `HistoryMessage.Tools`
   *  ([]ToolCallRecord, internal/session/history.go), restored by
   *  historyEntryToMessage below — so it DOES survive a page reload. On tether's
   *  own sessions HistoryStore bounds it by MaxToolsPerTurn and MaxToolResultBytes.
   *
   *  A cc session reaches this same field by a DIFFERENT route since tether#96:
   *  CCStore.Messages (internal/session/ccsessions.go) reads tool activity out of
   *  cc's transcript, under its own caps and with no per-turn count cap, and it
   *  keeps only FAILED tool results. So do not read the two paths as one — on a cc
   *  session the call survives a reload but `result` is usually absent.
   *
   *  Until tether#98 this said "the daemon never persists tool_use to history, so
   *  absent after a page reload (same as thinking/answerMs)". Nothing replaces
   *  that cross-reference on purpose — `thinking` was wrong in the same way, so
   *  the citation confirmed the claim instead of testing it. Check the two places
   *  named above the interface instead. */
  tools?: ToolCall[]
  /** The turn's token usage (tether#48): input/output token counts from cc's
   *  result event, attached to the turn bubble for the "⇅ in↑/out↓" badge.
   *  Live-only — the daemon does not persist usage to history, so absent after
   *  a page reload (same as answerMs; `thinking` was dropped from this list in
   *  tether#98 because it is persisted, not because usage changed). */
  usage?: { input: number; output: number }
  /** Where this message sits in the store's record of the session (tether#109) —
   *  `HistoryMessage.Ord` (internal/session/history.go), 1-based and strictly
   *  increasing in the order the daemon's store wrote the records.
   *
   *  ABSENT on a bubble the browser made: everything `handleEnvelope` and `addMessage`
   *  build is a live bubble the daemon has not recorded yet, and there is no honest
   *  position to give it. That is why `mergeHistory` treats an array holding one as
   *  unverifiable rather than guessing.
   *
   *  It is NOT the pagination cursor. `transcriptEarlier` is a byte offset to send as
   *  `?before=`; this is a rank, deliberately off by one from any cursor, and the only
   *  things done to it are ordered comparison and `Map` lookup — never arithmetic, and
   *  never a request. (An earlier version of this line said "the only operations are
   *  `===` and `<`", which review found to be false in both halves: the code uses `<`,
   *  `>`, `>=` and `<=`, and equality only through `Map`, i.e. SameValueZero.)
   *
   *  It is only comparable against another `ord` from the SAME session, and
   *  `setSessionId` resets `transcriptPagesBack` on a change for exactly that reason —
   *  see there. */
  ord?: number
}

/**
 * One entry as returned by GET /api/v1/sessions/{sid}/messages: either a
 * plain text turn (block undefined) or a persisted D-19 fenced block,
 * mirroring the daemon's session.HistoryMessage JSON shape (tether#8 T7).
 */
export interface HistoryEntry {
  role: string
  text: string
  ts: number
  block?: FencedBlock
  // tether#44 — rich turn content the daemon now persists, so a reload
  // reconstructs the turn as it rendered live (absent on pre-#44 history).
  thinking?: string
  tools?: ToolCall[]
  /** tether#109 — `HistoryMessage.Ord`: where this entry sits in the store's record of
   *  the session, 1-based. Optional in the TYPE because this interface is mirrored by
   *  hand and an older daemon's response would not carry it; the daemon in THIS binary
   *  always does, on both stores. `mergeHistory` treats absent as unverifiable. */
  ord?: number
}

/**
 * historyEntryToMessage converts one /messages history entry into the chat
 * Message shape — the same shape the live 'fenced' envelope produces in
 * handleEnvelope below — so DagBlock (and friends) render identically
 * whether the block arrived live or was reconstructed after a page reload
 * (tether#8 T7).
 */
export function historyEntryToMessage(m: HistoryEntry): Message {
  const msg: Message = {
    id: crypto.randomUUID(),
    role: m.role as Message['role'],
    text: m.text,
    ts: m.ts,
  }
  if (m.block) msg.block = m.block
  // tether#44 — restore persisted thinking + tool activity so ThinkingBlock /
  // ToolCallList render the same after a reload as they did live.
  if (m.thinking) msg.thinking = m.thinking
  if (m.tools && m.tools.length > 0) msg.tools = m.tools
  // tether#109 — carried through so `mergeHistory` can check the order it used to
  // assume. Copied on the `typeof` rather than on truthiness: the guard here has to
  // separate "the daemon did not send one" from a value, and every other field in this
  // function is a string or an array where falsy and absent mean the same thing. A
  // 1-based Ord is never 0, so `if (m.ord)` would behave identically today — and would
  // silently start dropping position 0 the day the +1 is reconsidered.
  if (typeof m.ord === 'number') msg.ord = m.ord
  return msg
}

/**
 * MESSAGE_KEY_SEP is the separator inside `messageKey`.
 *
 * `String.fromCharCode(0)` and NOT the six-character escape it replaces, and that is a
 * tooling decision with a measured cause rather than a style choice. tether#106 wrote
 * the escape inline here; the editor that wrote it folded those six characters into a
 * RAW NUL BYTE on disk, which type-checked, passed all 688 tests, passed a full-diff
 * review, and rendered normally in `git diff` — git only calls a file binary when a NUL
 * lands in its first 8000 bytes. The only thing that caught it was a mutation script
 * reporting "pattern occurs 0 times". `fromCharCode` cannot be folded by anything,
 * because there is no escape in the source to fold.
 *
 * It is a separator rather than nothing because `role` is a closed set that never
 * contains it, so "assistant" + 1 can never collide with "assistant1" + "".
 */
const MESSAGE_KEY_SEP = String.fromCharCode(0)

/**
 * messageKey is THE definition of "these two entries are the same message" for the two
 * reducers that still recognise one across two fetches by CONTENT: `loadHistory` (which
 * carries ids over a reload) and `prependHistory` (tether#107).
 *
 * `mergeHistory` used to be the third, and tether#109 took it off this key: role+ts is
 * not stable across a window slide (see the reducer), which is the defect that wi fixed.
 * The two that remain are unchanged, deliberately — moving them would trade a measured
 * bug for an unmeasured one.
 *
 * # role + ts, and text is deliberately NOT in it
 *
 * Because the message that is GROWING is the one whose identity matters most. The other
 * agent's current turn gains text on every write, so a key including text would give
 * that message a new id every three seconds — collapsing the thinking block of the only
 * turn anyone is watching. A tether entry's ts is `time.Now().UnixMilli()` at append and
 * a cc turn's is its first record's timestamp (internal/session), so the ts identifies
 * the turn while the text is its content.
 *
 * Written once because two reducers depend on it agreeing with itself. Two copies of this
 * rule is how `loadHistory` keeps an id that another reducer then replaces.
 */
export function messageKey(m: { role: string; ts: number }): string {
  return `${m.role}${MESSAGE_KEY_SEP}${m.ts}`
}

/**
 * hasOrd reports whether a message carries the daemon's ordering position (tether#109).
 *
 * `Number.isFinite` and not `!== undefined`: the value crosses JSON, so `null`, `NaN` and
 * a STRING are all shapes a hand-mirrored wire type cannot rule out, and the three fail
 * differently — which is why the loose check is not merely untidy:
 *
 *   - `null`, `NaN` and `undefined` compare false against every ord, so on the INCOMING
 *     side they fall through to a refusal by accident, and on the HELD side they silently
 *     shorten the span the interior case is measured against;
 *   - a string is worse, and it is the one an earlier version of this comment got wrong.
 *     `"4096" > 1000` is TRUE (JavaScript coerces), while `new Map([[4096, 0]]).get("4096")`
 *     is undefined (Map keys do not) — so a string ord matches nothing and then appends,
 *     producing a duplicate bubble at the end. Review found this by reading the claim
 *     rather than the code.
 *
 * Exported because the check belongs to the same contract as `messageKey`, and because a
 * test asserting the refusal has to be able to build the shape that causes it.
 */
export function hasOrd(m: { ord?: number }): boolean {
  return typeof m.ord === 'number' && Number.isFinite(m.ord)
}

/**
 * TURN_JOIN separates the fragments of one assistant turn that a PAGE BOUNDARY split
 * (tether#116). It is `ccTurnJoin` in internal/session/ccsessions.go and the two have to
 * stay the same string: this joins fragments that the daemon would itself have joined
 * had they landed in one window, and the reason it is a blank line rather than a newline
 * is stated there — a lone "\n" is a CommonMark soft break and renders as a SPACE, which
 * runs the end of one fragment onto the start of the next.
 */
export const TURN_JOIN = '\n\n'

/**
 * joinTurnAcrossPages folds the last bubble of an older page into the first bubble of
 * what is already on screen, or returns null when it must not (tether#116).
 *
 * # The defect
 *
 * The daemon starts a new assistant bubble only when the previous message it EMITTED was
 * not an assistant one (ccMessagesFromAt), so within one window a turn that stops to call
 * tools stays one bubble. tether#107 made each `?before=` page an independent window, and
 * a window cannot see the record before it — so a turn spanning a page boundary comes back
 * as one bubble per page, and `prependHistory` used to concatenate them. Measured on the
 * transcript that prompted this: a 148 MB session whose tool results are large enough that
 * a 1 MiB page yields ONE message, so every page the reader scrolled back added another
 * "tether" header to a single turn (six in eighteen seconds).
 *
 * This is the client's job because it is the only layer that holds both pages. The rule it
 * applies is the daemon's own, not a new one: pages are contiguous byte ranges of one
 * append-only file (see prependHistory), so "adjacent across the seam" is the same relation
 * as "adjacent within a window", and the daemon merges that.
 *
 * # Identity comes from the NEWER half, position from the OLDER half
 *
 * `id` is the on-screen one's, because `key={m.id}` is what React reconciles on and both
 * `expandedBlocks` and `expandedThinking` are Sets keyed by it: re-minting it would collapse
 * the reader's expansions and clamp the scroll, which is the exact damage this whole path
 * exists to avoid. The older half has never been on screen, so nothing is keyed to it.
 *
 * `ts` and `ord` are the older one's, because a bubble carries its FIRST fragment's stamp —
 * the rule the daemon already follows on both of its paths (ccsessions.go keeps the first
 * fragment's Ts on a merge; history.go stamps the accumulator when it is created). `ord`
 * matters beyond tidiness: `mergeHistory` indexes the transcript BY ord, so a merged bubble
 * carrying the newer fragment's position would let a later refresh match the wrong slot.
 *
 * # It refuses rather than lose anything
 *
 * The result spreads the newer half, so any `thinking` or `block` on the OLDER half would be
 * dropped. Neither can occur here — `MessagePage` only sets `HasEarlier` for the cc source
 * (SourceTether returns the zero value), so the earlier-page header is never emitted for
 * tether's own history, `loadEarlierTranscript` never fires for it, and the cc parser sets
 * neither field. Refusing anyway, because "cannot happen" is a property of another file: the
 * alternative to a refused merge is two bubbles, and the alternative to a silent drop is a
 * missing tool card or a missing thinking block that nothing would report.
 *
 * A half-populated `ord` is refused for the same reason and a sharper one: with only the
 * newer half carrying a position there is no correct value to give the result, and
 * inventing one puts a wrong key into `mergeHistory`'s index.
 */
export function joinTurnAcrossPages(older: Message, newer: Message): Message | null {
  if (older.role !== 'assistant' || newer.role !== 'assistant') return null
  if (older.thinking || older.block) return null
  if (hasOrd(older) !== hasOrd(newer)) return null
  const tools = [...(older.tools ?? []), ...(newer.tools ?? [])]
  const joined: Message = {
    ...newer,
    ts: older.ts,
    text: older.text && newer.text ? older.text + TURN_JOIN + newer.text : older.text || newer.text,
  }
  // Assigned rather than spread so the absent case stays absent: `ord: undefined` is a
  // present key, and `historyEntryToMessage` is deliberate about the difference.
  if (hasOrd(older)) joined.ord = older.ord
  if (tools.length > 0) joined.tools = tools
  return joined
}

/**
 * A daemon session-lifecycle notice (tether#50) — e.g. "the previous
 * conversation's context could not be restored".
 *
 * tether#57 — this lives in its OWN store slice rather than inside `messages`,
 * and that separation is the whole fix. `messages` is server truth: the
 * `[sessionId]` effect refetches GET /messages and `loadHistory` REPLACES the
 * array wholesale. A notice is locally-originated, never persisted by the
 * daemon, and unrecoverable once dropped — so while it shared that array, the
 * refetch that `session_ready` itself triggers could silently wipe the one line
 * explaining why the user's context is gone. Two lists with two owners cannot
 * clobber each other; they are recombined only at render time by
 * `mergeTranscript` below. (Before #57 the notice survived purely by accident,
 * because that refetch is skipped while a turn is streaming.)
 */
export interface Notice {
  id: string
  text: string
  ts: number
  /** Which class of notice this is (tether#80). Every production site sets it,
   *  because each class has a DIFFERENT repeat policy and none of them may be
   *  applied to another: an agent error collapses and is capped
   *  (appendAgentErrorNotice); a tether#77 prompt_undelivered line must never
   *  collapse, since each one is a separate prompt the user lost; a tether#50
   *  session banner collapses against the last banner only. Before this field
   *  those rules were separated by nothing but the text they happened to match,
   *  which is not a boundary — an agent-error line sitting last silently defeated
   *  tether#50's collapse.
   *
   *  OPTIONAL in the type, and that is a deliberate boundary rather than
   *  laziness: a required field would force ~10 Notice literals across four
   *  unrelated test files (session.test.ts, WorkspacePane.test.tsx, …) to be
   *  rewritten for a type-hygiene change, and none of those cases care which
   *  class they hold. An absent kind means exactly "no class-specific rule
   *  applies", which is the correct reading for a bare fixture.
   *
   *  'permission_withdrawn' (tether#137) has the prompt_undelivered policy and
   *  for the same reason: each line stands for a distinct tool request the user
   *  can no longer answer, so collapsing two of them would under-report a loss.
   *  It is NOT a 'session' line, because the tether#50 collapse compares against
   *  the last banner and these are not banners. */
  kind?: 'agent_error' | 'prompt_undelivered' | 'session' | 'permission_withdrawn'
  /** How many arrivals this one line stands for (tether#80) — see
   *  appendAgentErrorNotice for why the count is information rather than
   *  decoration. Absent on a line that has not repeated; mergeTranscript only
   *  renders it above 1, so absent and 1 read identically. */
  repeats?: number
}

/** How many DISTINCT agent-error lines a session keeps (tether#80).
 *
 * The second of two independent bounds. Collapsing by text (below) already
 * answers the case we have actually measured — opencode emits the same
 * "busy: another prompt is running" for every concurrent prompt — but it bounds
 * the list by the number of distinct texts, and those are not constants:
 * opencode's session.error carries whatever the provider said, and several
 * emit sites wrap a varying underlying error. So the collapse alone makes the
 * list bounded by the traffic, and this makes it bounded by the code.
 *
 * The exact number is a judgement call and deliberately not load-bearing: what
 * this file guarantees is that the list cannot grow without limit, and that
 * holds for any small N. Three, because these are quiet centred lines inside a
 * transcript the user is reading for their conversation, and a session on its
 * fourth DISTINCT agent error is not one that a fourth line explains — the most
 * recent few are the ones that describe the state it is in now.
 */
export const AGENT_ERROR_NOTICE_LIMIT = 3

/**
 * appendAgentErrorNotice folds one arriving agent error into the notice list,
 * bounded (tether#80). Pure — the caller supplies the id and the timestamp — so
 * the bounding rules test without a store and without a clock.
 *
 * WHY BOUNDED AT ALL, when tether#77's line right next to it deliberately is
 * not: the two are bounded by different things. A prompt_undelivered notice
 * corresponds one-to-one with the user pressing enter, so the list can only be
 * as long as the user made it. An agent error's arrival rate is decided by the
 * AGENT: opencode emits one per concurrent prompt (opencode_provider.go
 * SendPrompt's busy branch) and per session.error event from its stream, on a
 * connection the daemon does not close, and nothing prunes `notices` except a
 * deliberate session switch or a page reload. tether#80's own body records a
 * 200-frame run measured by tether#77's review (that is where the figure comes
 * from — it is quoted, not re-measured here); appended naively that is 200
 * permanent system lines.
 *
 * WHY A COUNT RATHER THAN JUST SUPPRESSING THE REPEAT: because the repeat is
 * not redundant. opencode's busy branch emits this error and then returns nil —
 * the prompt is DROPPED, not queued — so N arrivals of that text mean N prompts
 * the user typed and lost. That is the very fact tether#77 refuses to collapse
 * for its own line, and it would be inconsistent to keep it there by printing N
 * lines and lose it here by printing one. The count is the bounded encoding of
 * the same information.
 *
 * The count is a LOWER BOUND on what the daemon sent, not a census. Registry's
 * broadcast (internal/session/registry.go) writes to each subscriber channel
 * with a non-blocking send and logs-and-drops when it is full, and the chat
 * subscriber's buffer is 32 deep (internal/server/wt_chat.go), so a burst can
 * lose frames before the browser ever sees them. "At least N" is still the right
 * thing to show — under-counting a flood does not mislead about its nature — but
 * nothing here can promise exactness and this comment is where that stops being
 * a surprise.
 *
 * WHY THREE SLOTS AND NOT ONE, since a single slot whose text is replaced would
 * also be visible and bounded and would delete the cap, the eviction scan and
 * everything below: because a session's LATEST agent error is not reliably its
 * most important one. opencode's watchServeExit emits "opencode serve exited
 * unexpectedly: …" and then closes the event stream, while its busy branch fires
 * on ordinary user impatience — and a single slot lets the trivial one overwrite
 * the fatal one. Keeping a few and evicting by recency preserves both. The cost
 * is this function's second half, and it is a real cost, paid deliberately.
 *
 * WHY THE REPEAT ALSO MOVES: the line's position in the transcript is part of
 * what it says (mergeTranscript orders by ts), so a line explaining the prompt
 * the user just sent has to sit after that prompt. Refreshing ts moves the
 * existing line down to the turn that just triggered it instead of leaving a
 * stale copy far above. It never moves BACKWARDS (Math.max with its own ts), so
 * it cannot be dragged above a message it already reads below.
 *
 * Eviction is least-recently-seen, not first-arrived: a line that keeps
 * refreshing is the session's live complaint, and dropping it to make room for
 * a one-off would discard the more useful of the two.
 *
 * "Least recently seen" is measured by ts and not by a monotonic counter, so
 * several DISTINCT errors arriving inside one millisecond tie, and eviction
 * among them degrades to first-arrived. That is deliberate rather than
 * overlooked: a same-millisecond burst has no meaningful recency order to
 * preserve, and what the rule is actually for is not evicting a line that is
 * still recurring across TURNS, which are milliseconds to seconds apart. Worth
 * writing down because it also means a test must advance the clock to
 * distinguish the two rules at all — one that fires everything in a tight loop
 * cannot, and the mutant that swapped them survived exactly that test.
 *
 * THE ARRIVING LINE IS NEVER A CANDIDATE FOR EVICTION, and that exclusion is
 * load-bearing rather than an optimisation. Recency is read off `ts`, and `ts`
 * does not come from a monotonic clock: nextNoticeTs derives it from the last
 * message in the transcript, which carries the DAEMON's clock after a history
 * refetch and the browser's otherwise. So an EARLIER line can legitimately hold
 * a LATER ts than the one arriving now — refetch a daemon-ahead transcript, take
 * a few errors, then have the transcript replaced by loadHistory, and every
 * subsequent arrival is stamped behind them. Ranked purely by ts the newcomer is
 * then the least-recently-seen entry and evicts itself: the newest agent error
 * silently never reaches the user, which is the precise failure this whole change
 * exists to remove. Found by review (tether#80), pinned by two tests below.
 */
export function appendAgentErrorNotice(
  notices: Notice[],
  incoming: { id: string; text: string; ts: number },
): Notice[] {
  const at = notices.findIndex((n) => n.kind === 'agent_error' && n.text === incoming.text)
  if (at >= 0) {
    const prev = notices[at]
    const next = notices.slice()
    next[at] = { ...prev, ts: Math.max(prev.ts, incoming.ts), repeats: (prev.repeats ?? 1) + 1 }
    return next
  }
  const added: Notice[] = [...notices, { ...incoming, kind: 'agent_error', repeats: 1 }]
  const arrivedAt = added.length - 1
  let kept = 0
  let evictAt = -1
  for (let i = 0; i < added.length; i++) {
    const n = added[i]
    if (n.kind !== 'agent_error') continue
    kept++
    if (i === arrivedAt) continue // never evict what we just appended — see above
    if (evictAt < 0 || n.ts < added[evictAt].ts) evictAt = i
  }
  if (kept <= AGENT_ERROR_NOTICE_LIMIT || evictAt < 0) return added
  return added.filter((_, i) => i !== evictAt)
}

/**
 * nextNoticeTs stamps a notice that must read AFTER everything already in the
 * transcript (tether#77, shared with tether#80).
 *
 * A bare Date.now() is not enough. mergeTranscript inserts a notice before the
 * first message whose ts is at or AFTER its own, so a tie puts the notice
 * FIRST — and a prompt and the error explaining it land in the same millisecond
 * often enough that a test written the obvious way caught it. That tie-break is
 * right for tether#50's session-level banner, which introduces what follows,
 * and backwards for a line that explains what preceded it.
 *
 * It also absorbs the clock skew mergeTranscript's own doc warns about:
 * refetched history carries the DAEMON's clock and live bubbles the browser's,
 * so a transcript can legitimately hold timestamps well ahead of Date.now().
 * Whatever those stamps say, this lands last.
 *
 * Shared by both callers rather than copied, because it is a rule about
 * mergeTranscript's tie-break — one subtlety with one home, not a coincidence
 * two branches happen to need.
 */
export function nextNoticeTs(messages: Message[]): number {
  return Math.max(Date.now(), (messages.at(-1)?.ts ?? 0) + 1)
}

/**
 * mergeTranscript recombines the two independently-owned lists into the single
 * ordered transcript the chat pane renders — a pure projection, computed at
 * render time, owning no state of its own.
 *
 * Notices are projected into the `role: 'system'` Message shape so the existing
 * `.msg-system` render branch (tether#50) handles them unchanged.
 *
 * WHY interleave chronologically instead of pinning notices to the top? Because
 * in the common case the history refetch is skipped (it is guarded on
 * `!streaming`), so the list the user is looking at still holds the OLD
 * session's conversation plus their just-sent prompt — and "we started a new
 * session" belongs AFTER those, not above them. Head-of-list is only the right
 * answer in the post-refetch state, where every message does belong to the new
 * session. Chronological placement is right in both.
 *
 * Ordering: each notice is INSERTED before the first message whose ts is at or
 * after the notice's (ties put the notice first) — messages are never reordered
 * relative to one another. That distinction matters because `messages` is not
 * guaranteed ts-monotonic: live bubbles are stamped with the BROWSER clock while
 * refetched history carries the DAEMON's, so a naive sort of the union could
 * reshuffle the user's actual conversation.
 *
 * Be honest about the limit of that: because the two clocks are different (tether
 * is routinely driven from a phone against a remote daemon), skew is not bounded,
 * and a badly skewed clock can land a notice anywhere in the list, including
 * either end. What it can never do is reorder the messages themselves — the
 * transcript stays intact, only the banner's position degrades.
 *
 * The common case (no notices) returns the SAME array reference, so it is inert
 * for memoisation and re-render identity.
 */
export function mergeTranscript(messages: Message[], notices: Notice[]): Message[] {
  if (notices.length === 0) return messages
  const pending = [...notices].sort((a, b) => a.ts - b.ts)
  // tether#80 — a collapsed agent error says how many arrivals it stands for.
  // Formatted HERE, in the projection, and not baked into Notice.text, so that
  // the stored text stays the key appendAgentErrorNotice matches repeats on; a
  // counted text would never match itself again and the collapse would silently
  // stop collapsing at two. Rendered only above 1, so every other notice class
  // (which never sets repeats) is byte-identical to before.
  const asMessage = (n: Notice): Message => ({
    id: n.id,
    role: 'system',
    text: (n.repeats ?? 1) > 1 ? `${n.text} (×${n.repeats})` : n.text,
    ts: n.ts,
  })
  const out: Message[] = []
  let i = 0
  for (const m of messages) {
    while (i < pending.length && pending[i].ts <= m.ts) { out.push(asMessage(pending[i])); i++ }
    out.push(m)
  }
  while (i < pending.length) { out.push(asMessage(pending[i])); i++ }
  return out
}

export interface PermissionRequest {
  id: string
  toolName: string
  input: unknown
}

/**
 * PendingPermission is a PermissionRequest plus the sid of the chat connection
 * that delivered it (tether#132).
 *
 * # Why the sid is kept at all
 *
 * Because `loadHistory` has to answer a question it could not previously ask.
 * That reducer is the server-truth replace, and it cleared this whole queue on
 * the grounds that the array it installs may belong to ANOTHER session — true for
 * a deliberate switch, and wrong for every refetch of the session already on
 * screen. There are several of those and none of them is a switch: a click on the
 * already-open row in the session list (lib/session.ts's REFRESH_TRANSCRIPT_EVENT
 * -> refreshTranscript), the held-session watcher's three-second reload, and a
 * page reload. So a live permission card could be dismissed by a misclick, with
 * nothing anywhere to bring it back. Without the sid, "the session I am
 * reloading" and "the session I have left" are indistinguishable.
 *
 * # Not folded into PermissionRequest
 *
 * PermissionRequest is the WIRE payload (internal/server/mux.go builds exactly
 * its three fields); this is store bookkeeping about an envelope. Keeping them
 * apart is what stops a future reader from looking for `sessionId` in the daemon's
 * payload.
 *
 * # Why the tag is OPTIONAL, which is a decision and not laziness
 *
 * The one writer — handleEnvelope's 'permission' case — always sets it, so no
 * value this store produces is ever absent. What is optional for is the queue
 * entries assembled DIRECTLY by tests that predate the tag, and the reducer reads
 * absent and explicit-null identically: "claimed by no session", therefore not
 * kept by a load for a named one. That is the pre-tether#132 outcome, which is
 * the only safe thing an untagged entry can mean — so requiring the field would
 * buy nothing here and cost a rewrite of fixtures in files this change does not
 * own. Do not "tidy" it into a required field without checking those.
 *
 * Consumers keep taking PermissionRequest[] (fenced-blocks/PermissionBlock): this
 * type is assignable to it, and the extra field is not theirs to read.
 */
export interface PendingPermission extends PermissionRequest {
  sessionId?: string | null
}

export type ConnState = 'connecting' | 'live' | 'reconnecting' | 'dropped'

export interface Connection {
  state: ConnState
  latency: number
  attempt: number
}

/**
 * FatalRefusal is what the store keeps of a terminal wire.ErrorPayload
 * (tether#63) — just enough for the failed-connection card to explain WHY and
 * for a code→sentence lookup (ChatPane) to render something better than the
 * raw message. Deliberately does NOT keep `terminal`: by the time a value
 * exists in this field the caller (the 'error' handler below) has already
 * decided it was true, and re-checking a stale bool off a struct nobody
 * mutates would only invite the two to drift.
 */
export interface FatalRefusal {
  code: string
  message: string
}

/**
 * parseErrorPayload defensively narrows a KindError envelope's `payload` to
 * wire.ErrorPayload's shape. Pure and exported so it tests without a store —
 * "defensive" here means a payload that ISN'T this shape (the pre-tether#63
 * bare string a stale daemon binary might still send, or `null`/garbage) is
 * simply not a classified error, not a crash: the caller treats a null return
 * as "nothing to update `fatal` with," which is exactly the old un-classified
 * behaviour for a payload this build doesn't recognize.
 */
export function parseErrorPayload(payload: unknown): ErrorPayload | null {
  if (!payload || typeof payload !== 'object') return null
  const p = payload as Record<string, unknown>
  if (typeof p['code'] !== 'string') return null
  if (typeof p['message'] !== 'string') return null
  if (typeof p['terminal'] !== 'boolean') return null
  return { code: p['code'], message: p['message'], terminal: p['terminal'] }
}

/** A selected file within a workspace, identified by workspace id + path
 *  relative to the workspace root (matches fetchFile's `path` param). */
export interface SelectedFile {
  wsId: string
  path: string
}

interface AppState {
  sessionId: string | null
  messages: Message[]
  // Daemon session-lifecycle notices (tether#50), kept OUT of `messages` so the
  // history refetch cannot wipe them (tether#57 — see the Notice doc comment).
  // Live-only, page-lifetime: the daemon never persists them.
  notices: Notice[]
  // Pending PreToolUse permission requests (tether#40). A QUEUE, not one slot:
  // parallel tools each send their own KindPermission, so a single slot let the
  // later request clobber the earlier one → all-but-one timed out.
  //
  // Live-only in the sense that nothing persists it here — but since tether#132
  // it is no longer live-ONCE: the daemon re-sends whatever is still outstanding
  // to a client that attaches, so a second device or tab is given the requests it
  // was never told about. See PendingPermission for the sid each entry carries.
  pendingPermissions: PendingPermission[]
  connected: boolean
  streaming: boolean
  connection: Connection
  // tether#63 — a terminal wire.ErrorPayload (session.Refusal classified
  // Terminal=true), or null when there is none. Deliberately a TOP-LEVEL
  // field, not nested inside `connection`: `connection` is reconnect-ladder
  // bookkeeping that setConnected/setConnection reset wholesale on every
  // state change, and a fatal refusal must survive exactly those resets — it
  // is what TELLS the ladder to stop, not something the ladder's own state
  // transitions get to clear as a side effect. Only clearFatal (deliberate)
  // and doConnect (a fresh attempt deserves a fresh chance) reset it.
  fatal: FatalRefusal | null
  streamingMsgId: string | null   // id of the bubble receiving ANSWER text (drives the cursor)
  // tether#34 — ONE assistant bubble per turn: thinking + answer text accumulate
  // into it, so a turn with interleaved thinking blocks (thinking→text→thinking→
  // text) stays a single bubble instead of fragmenting. Both transient, never persisted.
  curTurnId: string | null        // the current turn's assistant bubble (null between turns)
  thinkingStartTs: number | null  // ts of the first thinking delta this turn
  answerStartTs: number | null    // ts of the first answer delta this turn (tether#36)
  // tether#42 — true after a manual stop, until the next user turn; drops the
  // late buffered deltas cc flushes post-interrupt so they don't spawn a new
  // bubble / resume streaming ("output another chunk after Stop").
  stopped: boolean

  // Selection (tether#28): the middle file view (selectedFile) and the right
  // Work wi drawer (selectedWiId) are INDEPENDENT and can be open at once.
  // `select` only touches the field(s) passed; pass an explicit null field to
  // clear just that one (e.g. drawer close), or `null` to clear both (reset).
  selectedWiId: string | null
  selectedFile: SelectedFile | null

  // Currently-focused Work project (tether#23): shared by the middle
  // knowledge-graph (WorkGraphView) and the right Work detail pane so both
  // render the same project. Empty string = none picked yet.
  workProject: string

  // The workspace the user has selected in the left WorkspacePane (tether#47).
  // Chat's @-mention picker queries this workspace's files, and — since
  // tether#52 — a brand-new chat session's cwd is pinned to it via `?ws=<id>`
  // (chatUrl.ts). Carries the abspath so @ can insert @<abspath> (cc reads it
  // regardless of cwd). Persisted since tether#66 — see rememberWorkspace.
  activeWorkspace: { id: string; path: string } | null

  // tether#52 — gates ChatPane's FIRST connect (see index.tsx's mount effect
  // and chatUrl.ts). WorkspacePane's GET /api/v1/workspaces resolves strictly
  // after ChatPane mounts, so on a cold profile (no remembered sid) connecting
  // immediately would pin a brand-new session's cwd before the browsed
  // workspace is known. Set true on BOTH the success and the error path of
  // that fetch (see workspace/index.tsx load()) — a failed/offline fetch must
  // still release the gate, else a sid-less first connect would wait forever
  // (the 2s fallback timer in ChatPane is the other half of that guarantee).
  workspacesLoaded: boolean

  // ── The transcript's own boundaries (tether#107) ────────────────────────────
  //
  // In the store rather than in ChatPane because three parties read them: the pane
  // renders the top-of-transcript marker, `loadEarlierTranscript` spends the cursor,
  // and `refreshTranscript` needs to know whether the reader has paged back before it
  // decides whether it may replace the array.

  /** The byte offset to ask for to get the page before the oldest one loaded, or null
   *  when the oldest message on screen is the beginning of this store's record.
   *  Straight off X-Tether-Transcript-Earlier. */
  transcriptEarlier: number | null
  /** A store OTHER than the one that served this transcript which also holds a record
   *  for this sid (X-Tether-Transcript-Other-Record), or null. What makes the
   *  difference between "the beginning of the conversation" and "the beginning of what
   *  tether recorded" sayable. */
  transcriptOtherRecord: string | null
  /** How many earlier pages the reader has deliberately loaded. The refresh path reads
   *  this to decide between replacing the array (0) and merging into it (>0); a
   *  boolean would do today, but the count is what the pane would need to say how far
   *  back it has gone and it costs the same.
   *
   *  It describes the pages of ONE session, so `setSessionId` clears it on a change
   *  (tether#109) — see there. */
  transcriptPagesBack: number

  setSessionId: (id: string) => void
  loadHistory: (msgs: Message[]) => void
  /** Record the two boundary facts off the response that installed the messages they
   *  describe (tether#107) — the argument noteTranscriptVersion makes about the version
   *  header. */
  setTranscriptBounds: (b: { earlier: number | null; otherRecord: string | null }) => void
  /** Put an older page in FRONT of the transcript (tether#107). */
  prependHistory: (msgs: Message[]) => void
  /** Fold a freshly-fetched NEWEST page into a transcript that already holds older
   *  pages (tether#107). Returns false when the merge cannot be shown to be safe —
   *  four causes since tether#109, enumerated on the reducer; the original one was
   *  "the two pages do not overlap at all". Either way, false is the caller's signal
   *  to fall back to loadHistory. */
  mergeHistory: (msgs: Message[]) => boolean
  /** Drop the notice list when the USER deliberately opens a different session
   *  (tether#57). Not wired to setSessionId: the resume-fallback path also
   *  changes the sid, and clearing there would discard the very notice that
   *  explains the change. */
  clearNotices: () => void
  /** Drop a terminal refusal once the caller is giving the connection a fresh
   *  chance (tether#63) — see `fatal`'s doc comment on why nothing else clears it. */
  clearFatal: () => void
  addMessage: (msg: Message) => void
  /** Remove one request from the queue after it's decided (tether#40). */
  resolvePermission: (id: string) => void
  setConnected: (v: boolean) => void
  setConnection: (patch: Partial<Connection>) => void
  handleEnvelope: (env: Envelope) => void
  /** Finalize the current turn on a manual interrupt/stop (tether#42). */
  stopTurn: () => void
  select: (sel: { wiId?: string | null; file?: SelectedFile | null } | null) => void
  setWorkProject: (p: string) => void
  setActiveWorkspace: (ws: { id: string; path: string } | null) => void
  setWorkspacesLoaded: (v: boolean) => void
  settleWorkspaces: (ws: { id: string; path: string } | null) => void
}

// finalizeTurn closes the current assistant turn — stamps the answer duration
// (tether#36) and resets all turn-transient pointers. Shared by the natural
// 'result' path and the manual interrupt (tether#42 stopTurn): an interrupted
// cc turn emits NO EventResult, so the frontend finalizes locally. Idempotent —
// if a late result still arrives, curTurnId is already null and it's a no-op.
function finalizeTurn(s: AppState): Partial<AppState> {
  const id = s.curTurnId
  const started = s.answerStartTs
  const messages = (id && started != null)
    ? s.messages.map(m => (m.id === id ? { ...m, answerMs: Date.now() - started } : m))
    : s.messages
  return { messages, streaming: false, streamingMsgId: null, curTurnId: null, thinkingStartTs: null, answerStartTs: null }
}

/** localStorage key holding the id of the selected workspace (tether#66) —
 *  deliberately the same layer as `tether_last_sid`, because it has to survive
 *  exactly the same event: App's startNewSession drops the sid and calls
 *  `location.reload()`, so anything about the user's intent that lives only in
 *  React state is gone by the time the new session is created. */
export const WORKSPACE_ID_KEY = 'tether_ws_id'

/** The workspace id remembered from a previous page (tether#66). Null when this
 *  profile has never selected one; may name a workspace that has since been
 *  removed from the registry — resolveSelection (panes/workspace/index.tsx)
 *  treats an id it cannot find as "not remembered". */
export function rememberedWorkspaceId(): string | null {
  return localStorage.getItem(WORKSPACE_ID_KEY)
}

// rememberWorkspace persists a selection so it survives a reload (tether#66).
// Hung off the store mutators rather than off the click handler for the same
// reason setSessionId owns `tether_last_sid`: every path that changes the
// selection — initial resolve, row click, delete — goes through one of these
// two setters, so none of them can forget.
//
// It only ever RECORDS a selection, never erases one, and that asymmetry is
// load-bearing. WorkspacePane's publishing effect fires on mount with an empty
// registry — i.e. `setActiveWorkspace(null)` — strictly BEFORE
// GET /api/v1/workspaces resolves. An erase-on-null would therefore wipe the
// remembered id on every single page load, reintroducing tether#66 inside the
// code that fixes it (and the suite would stay green, since every unit test
// starts from a cleared localStorage). Not erasing costs nothing: a stale id is
// ignored by resolveSelection, and an empty registry has nothing to remember.
//
// The write is best-effort and MUST NOT be able to throw into a caller. Two
// reasons, both specific to where it is called from:
//   - `setItem` throws for real (QuotaExceededError; Safari private browsing),
//     and unlike every other localStorage write in this app — all of which sit
//     in DOM event handlers or the WT reader — this one is reached from a React
//     effect, i.e. the commit phase. There is no ErrorBoundary anywhere in the
//     tree, so a throw there unmounts the whole root: a blank app, because a
//     preference could not be saved.
//   - in settleWorkspaces it would pre-empt the `set` that opens ChatPane's
//     first-connect gate (see below), which is the one thing in this file that
//     must not be skipped. Callers set state FIRST and persist after; this
//     swallow is the second half of that ordering.
function rememberWorkspace(ws: { id: string; path: string } | null): void {
  if (!ws) return
  try {
    localStorage.setItem(WORKSPACE_ID_KEY, ws.id)
  } catch { /* preference not saved; the session still runs in the right place */ }
}

export const useStore = create<AppState>((set, get) => ({
  sessionId: null,
  messages: [],
  notices: [],
  pendingPermissions: [],
  connected: false,
  streaming: false,
  streamingMsgId: null,
  curTurnId: null,
  thinkingStartTs: null,
  answerStartTs: null,
  stopped: false,
  connection: { state: 'connecting', latency: 0, attempt: 0 },
  fatal: null,
  selectedWiId: null,
  selectedFile: null,
  workProject: '',
  activeWorkspace: null,
  workspacesLoaded: false,
  transcriptEarlier: null,
  transcriptOtherRecord: null,
  transcriptPagesBack: 0,

  /**
   * setSessionId persists the sid and, when it CHANGES, retires the page count that
   * described the session being left (tether#109).
   *
   * # Why the reset is here rather than left to loadHistory
   *
   * Because `transcriptPagesBack > 0` is the flag that sends a refresh through
   * `mergeHistory` instead of `loadHistory`, and `mergeHistory` compares `ord`s — which
   * are positions in ONE store's record of ONE session. Carried across a switch, the
   * count says "merge" while the array belongs to the previous session, and the two
   * sessions' positions are then compared as if they described the same file.
   *
   * That is not a remote possibility, it is arithmetic: `ord` is 1-based and both stores
   * number from the beginning of their record, so `ord === 1` is present in EVERY page
   * that reaches byte 0 — which for tether's own store is every page, since LoadHistory
   * reads the whole file. One matching ord is all `mergeHistory` needs to report success,
   * after which every other position in the arriving page lands inside the previous
   * session's span and is skipped as "already on screen". The result is session A's
   * transcript displayed under session B, with A's cursor still armed. ChatPane's
   * `[sessionId]` effect usually heals it one response later, but that effect is guarded
   * on a non-empty payload, so a switch to a session with nothing in it does not heal.
   *
   * Found by review. Under tether#107's role+ts key the same sequence collided only if
   * two sessions shared a millisecond, so this became reachable when the key changed —
   * which makes it this wi's to fix rather than a pre-existing one to note.
   *
   * # What is deliberately NOT reset
   *
   * `transcriptEarlier` and `transcriptOtherRecord`. They come off the response that
   * installs the messages they describe, and the switch's own `refreshTranscript` records
   * both a moment later; clearing them here would render "this is the beginning of the
   * conversation" over the OUTGOING session's messages for one frame — a false sentence
   * in place of a stale one. With the count at zero the refresh takes `loadHistory` and
   * overwrites both from the new session's response, so the stale cursor is never spent.
   *
   * # Why conditional
   *
   * `handleEnvelope`'s session_ready calls this with the sid it already has (tether#45,
   * for localStorage), and that is not a switch. An unconditional reset would throw away
   * the pages of a reader who is sitting in one session while it re-announces itself.
   */
  setSessionId: (id) => {
    localStorage.setItem('tether_last_sid', id)
    set((s) => (s.sessionId === id ? { sessionId: id } : { sessionId: id, transcriptPagesBack: 0 }))
  },
  loadHistory: (msgs) => set((s) => {
    // Mirror the live 'fenced' replace-by-BlockID reduction (contract §3):
    // a block re-emitted with the same BlockID is persisted as multiple
    // history entries, but must collapse to ONE card — the LAST occurrence's
    // content at the FIRST occurrence's position — so a reloaded viewer sees
    // exactly what the live viewer saw (tether#8 T7/T8 reconciliation).
    const reduced: Message[] = []
    const blockPos = new Map<string, number>()
    for (const m of msgs) {
      const bid = m.block?.blockId
      if (bid) {
        const at = blockPos.get(bid)
        if (at !== undefined) {
          reduced[at] = { ...reduced[at], block: m.block }
          continue
        }
        blockPos.set(bid, reduced.length)
      }
      reduced.push(m)
    }
    // tether#106 — carry the EXISTING ids over to the messages that are already on
    // screen, so a reload REPLACES CONTENT without replacing IDENTITY.
    //
    // This is still the wholesale server-truth replace it has always been. What
    // changes is that a message the previous array already had keeps its id. Before
    // this, `historyEntryToMessage` minted a fresh crypto.randomUUID() for every entry
    // on every load, so a refetch changed every React key (`key={m.id}` in ChatPane)
    // and the whole transcript unmounted and remounted: every expanded fenced block
    // and every expanded thinking block collapsed (both Sets are keyed by message id),
    // and the scroll container's scrollHeight collapsed to its client height
    // mid-commit, clamping scrollTop — the reader was thrown to the top of the
    // conversation. Survivable once per deliberate switch; not survivable now that
    // tether#106 reloads a held session's transcript whenever the other agent writes.
    //
    // # Matched by CONTENT, not by position, and that is the whole design
    //
    // A positional (prefix) match was the first attempt and it is inert for the
    // population this feature exists for. CCStore serves a sliding TAIL —
    // `out[ccTrimFront(out, ccMessagesMax):]` with ccMessagesMax = 200, inside a 1 MiB
    // byte window (ccsessions.go) — so once a cc conversation passes either bound,
    // every append shifts index 0 and a prefix match breaks immediately. That is
    // exactly the case the reader is watching: the cc transcript behind tether#104 was
    // 103,388,175 bytes. Keyed by content, a slide costs only the ids of the messages
    // that actually fell out of the window.
    //
    // # The key is role+ts, and text is deliberately NOT in it
    //
    // Because the message that is GROWING is the one whose identity matters most. The
    // other agent's current turn gains text on every write, so a key including text
    // would give that message a new id every three seconds — collapsing the thinking
    // block of the only turn anyone is watching. role+ts is the natural key here: a
    // tether entry's ts is `time.Now().UnixMilli()` at append and a cc turn's is its
    // first record's timestamp, so it identifies the turn while text is its content.
    //
    // # Each old id is handed out AT MOST ONCE
    //
    // Two messages can share role+ts (coarse clocks, or the same turn re-read), and
    // giving both the same id would put duplicate React keys in one list — a worse
    // failure than the remount this avoids. The queue per key is what makes that
    // impossible: an id is shifted out when it is used, and a message with no match
    // left keeps the fresh uuid historyEntryToMessage gave it.
    //
    // The key itself is `messageKey`, shared with prependHistory and mergeHistory
    // (tether#107) — one definition of "the same message", and one occurrence of the
    // separator. See messageKey for why the separator is now String.fromCharCode(0)
    // rather than the escape that used to be written inline here.
    const idsByKey = new Map<string, string[]>()
    for (const m of s.messages) {
      const key = messageKey(m)
      const q = idsByKey.get(key)
      if (q) q.push(m.id)
      else idsByKey.set(key, [m.id])
    }
    for (let i = 0; i < reduced.length; i++) {
      const q = idsByKey.get(messageKey(reduced[i]))
      const id = q?.shift()
      if (id !== undefined) reduced[i] = { ...reduced[i], id }
    }
    // Stale pending permission requests are dropped — they belong to a session
    // this reducer is replacing (tether#40) — but ONLY the stale ones (tether#132).
    //
    // # What was wrong with dropping all of them
    //
    // "A session reset (page reload / session switch)" named two events and only
    // one of them is a reset. Every other refetch of the session ALREADY on
    // screen came through here too, and there are three: a click on the
    // already-open row in the session list (lib/session.ts's
    // REFRESH_TRANSCRIPT_EVENT -> refreshTranscript), the held-session watcher's
    // three-second reload, and a page reload. A permission request reaches the
    // browser as ONE broadcast envelope and is held nowhere else, so any of those
    // could dismiss a live, undelivered-to-nobody-else permission card, and
    // nothing would ever send it again. The daemon does now re-send what is still
    // outstanding to a client that attaches (session.Entry.backfill) — and this
    // line threw that away too.
    //
    // # Why filtering by sid is the whole fix, and why it is not "just delete the
    // reset"
    //
    // The reset is correct for a SWITCH: those requests were raised in a
    // conversation the reader has left, and leaving them on screen makes them
    // approvable there. Both call sites establish the discriminator for free —
    // ChatPane's `[sessionId]` effect and lib/session.ts's refreshTranscript each
    // check `sessionId` against the response's sid before calling this, so
    // `s.sessionId` here IS the session being installed. A request tagged with it
    // belongs to the conversation on screen; anything else does not.
    //
    // It also removes an ORDERING dependency that neither side controls, and the
    // ordering was measured rather than reasoned about. Against a live daemon,
    // with the transcript GET and the WebTransport connect started at the same
    // instant, the GET returned in 2.6ms and the re-sent permission envelope
    // arrived at 24.4ms — so on a reload this reducer runs FIRST and the request
    // arriving second is safe either way. That is one measurement of one link on
    // one machine, which is exactly why it is not what the fix rests on: the other
    // three refetch paths above run at arbitrary times, long after the request
    // arrived, and they are the order that used to lose it.
    //
    // An entry with NO tag — null from an envelope that named no session, or
    // absent because a test assembled the queue directly — matches nothing: the
    // `!= null` is what keeps `null === s.sessionId` from becoming true for a
    // reader who has no sid either. That preserves the pre-tether#132 outcome for
    // anything the reducer did not tag, rather than inventing a survival rule for
    // it. Nothing the chat route delivers is untagged: serveChat stamps
    // env.SessionID on every envelope it forwards.
    //
    // tether#57 — note what is NOT in this return: `notices`. This reducer is
    // the server-truth replace, and it does not own the notice list, so it
    // cannot drop it. Do not add `notices` here to "reset" it; use clearNotices
    // at the deliberate session-switch call sites instead.
    //
    // tether#107 — transcriptPagesBack IS reset, and that follows from what this
    // reducer is: the array it installs is one page, so no earlier page is on screen
    // any more. The two boundary FACTS are not reset here, because they come off the
    // response and the caller records them with setTranscriptBounds immediately after;
    // clearing them here would blank the marker for one render on every reload.
    const keptPermissions = s.pendingPermissions.filter((p) => p.sessionId != null && p.sessionId === s.sessionId)
    return { messages: reduced, streamingMsgId: null, streaming: false, curTurnId: null, thinkingStartTs: null, answerStartTs: null, stopped: false, pendingPermissions: keptPermissions, transcriptPagesBack: 0 }
  }),
  setTranscriptBounds: ({ earlier, otherRecord }) => set((s) => (
    // A real no-op when neither fact moved: ChatPane subscribes without a selector, so
    // a set() here re-renders it — and invalidates the transcript memo — on every one
    // of the three-second probe's reloads, which is most of the time.
    //
    // It returns `s` ITSELF and not `{}`, and that difference is measured rather than
    // stylistic. zustand's setState compares `Object.is(nextState, state)` and only
    // skips the notify when they are the same object; a returned `{}` is a different
    // object, so it merges (Object.assign({}, state, {})) and notifies anyway. Probed
    // on zustand 5 in this repo: `clearNotices` on an already-empty list produces 1
    // subscriber call and a fresh state object, while `set((s) => s)` produces 0 and
    // preserves identity. The `{}` idiom this file uses elsewhere (clearNotices,
    // clearFatal) therefore does NOT have the property its comments claim — left alone
    // here because changing them is not this wi's, but not copied either.
    s.transcriptEarlier === earlier && s.transcriptOtherRecord === otherRecord
      ? s
      : { transcriptEarlier: earlier, transcriptOtherRecord: otherRecord }
  )),
  /**
   * prependHistory puts an older page in FRONT of what is on screen (tether#107).
   *
   * # What it deliberately does NOT do, and why each omission is load-bearing
   *
   *  - It does not touch the id of anything already in the array. `key={m.id}` is what
   *    React reconciles the transcript on, and both `expandedBlocks` and
   *    `expandedThinking` are Sets keyed by message id, so re-minting an id collapses
   *    the reader's expansions and clamps the scroll container — the exact damage
   *    tether#106 removed from the reload path, which it would be absurd to
   *    reintroduce on the path whose whole purpose is to keep the reader in place.
   *  - It does not reset `streaming`, `curTurnId`, the turn clocks or
   *    `pendingPermissions`. loadHistory resets those because it is the server-truth
   *    REPLACE and the array it installs may belong to another session; this adds
   *    older history to the session already on screen and reports nothing about the
   *    live turn. Resetting here would let a click on "load earlier messages" cancel
   *    the reader's own in-flight turn. (Since tether#132 loadHistory keeps the
   *    pending requests raised in the session it is installing and drops only the
   *    rest, which makes this omission the same rule rather than a stricter one:
   *    every request in the list belongs to the session this page extends.)
   *  - It does not sort. Every page is a contiguous byte range of one append-only file,
   *    so an earlier page is entirely older than what it is prepended to. Sorting the
   *    union by ts would ALSO reorder the messages themselves, which mergeTranscript's
   *    doc explains is never safe here: live bubbles carry the browser's clock and
   *    fetched history the daemon's.
   *
   * It DOES drop anything whose messageKey is already present. The cursor is exact, so
   * an overlap should not happen; if one does — a rewritten transcript, a cursor spent
   * twice — a duplicate React key is a broken list, which is worse than a missing
   * bubble, and this is the cheapest place to make it impossible.
   *
   * It ALSO joins the seam (tether#116). Concatenating the two pages was wrong whenever a
   * turn spanned the boundary: the daemon merges consecutive assistant records into one
   * bubble, but it does that per WINDOW, and since tether#107 each page is its own window.
   * See joinTurnAcrossPages for why the client is the layer that can fix it, what the
   * merged bubble takes from which half, and the four shapes it refuses. The dedupe runs
   * FIRST — the seam is between what survived it and the transcript, not between the raw
   * page and the transcript, or a page whose tail is already on screen would be joined to
   * the bubble it duplicates.
   */
  prependHistory: (msgs) => set((s) => {
    // `s` and not `{}` — see setTranscriptBounds for the measurement.
    if (msgs.length === 0) return s
    const have = new Set(s.messages.map(messageKey))
    const older = msgs.filter((m) => !have.has(messageKey(m)))
    const pagesBack = s.transcriptPagesBack + 1
    if (older.length === 0) return { transcriptPagesBack: pagesBack }
    const seam = s.messages.length > 0
      ? joinTurnAcrossPages(older[older.length - 1], s.messages[0])
      : null
    if (seam) {
      return {
        messages: [...older.slice(0, -1), seam, ...s.messages.slice(1)],
        transcriptPagesBack: pagesBack,
      }
    }
    return { messages: [...older, ...s.messages], transcriptPagesBack: pagesBack }
  }),
  /**
   * mergeHistory folds a freshly-fetched NEWEST page into a transcript that already
   * holds pages the reader loaded (tether#107).
   *
   * # Why the refresh path needs a second reducer at all
   *
   * tether#106's three-second probe reloads a held session's transcript whenever the
   * other agent writes, and it does that through `loadHistory`, which REPLACES the
   * array. Once the reader has paged back, replacing would discard the pages they
   * deliberately loaded — every three seconds, while they are reading them. The
   * alternative considered and rejected was to stop refreshing while paged back, which
   * makes HELD_SESSION_READABLE_NOTE ("tether keeps re-reading it every few seconds")
   * false for exactly that reader.
   *
   * # The rule, and why tether#109 had to replace tether#107's
   *
   * tether#107 matched on messageKey and appended everything that did not match,
   * because "every page is a contiguous range of one append-only file, so everything
   * that does not match is strictly newer than everything that does". The premise about
   * the FILE is true. The conclusion about MESSAGES does not follow, and the counter-
   * example was on the owner's screen: two consecutive bubbles whose lower one was
   * timestamped 3h16m EARLIER than the upper one.
   *
   * What breaks it is that a message is not a record. CCStore merges a run of assistant
   * records into ONE bubble stamped with its FIRST fragment's time
   * (internal/session/ccsessions.go), so the bubble at the LEADING EDGE of the byte
   * window is a SUFFIX of a turn, and which suffix depends on where the window opens.
   * Slide the window one record forward — the ordinary consequence of the file growing —
   * and the same turn comes back stamped with a later fragment: a messageKey the client
   * has never seen, carrying a timestamp from a megabyte back. Appended at the end.
   *
   * Measured by replaying the reported 125 MB transcript through the real window rule:
   * 36 of 1,031 consecutive single-record appends produce one, worst case 3h36m out of
   * order. The window START never moved backwards once in 1,053 samples, so this needs
   * neither the widen-once retry nor the message cap — it is what a normally sliding
   * window does.
   *
   * So the rule is now stated on the ordering key the daemon reports per message
   * (`Message.ord`, tether#109) and CHECKED rather than assumed. For each arriving
   * message, exactly one of:
   *
   *   - an `ord` already on screen — the same record, re-read: update IN PLACE, keeping
   *     the id (that is tether#106's property, and the growing turn depends on it);
   *   - an `ord` strictly greater than every `ord` on screen — genuinely new: append;
   *   - an ASSISTANT `ord` INSIDE the span already on screen but matching nothing — the
   *     re-cut leading-edge bubble above: SKIP it. Those bytes are already rendered,
   *     inside the fuller bubble that swallowed them, and its text is a suffix of that
   *     bubble's text — so nothing is hidden by dropping it and a duplicate is avoided;
   *   - anything else inside the span — refuse. See the role check below;
   *   - an `ord` BELOW everything on screen — the window really did move backwards
   *     (widen-once fires: measured to start 15.6 MiB earlier on that same transcript).
   *     Refuse.
   *
   * # Skipping rather than refusing, for the interior case
   *
   * Because refusing there would fire on 3.5% of appends — every few seconds for a
   * reader of an active session — and each refusal costs the reader the pages they
   * deliberately loaded. That is the trap the cheaper version of this fix falls into:
   * "require the incoming page's first message to match something on screen" false-
   * rejects whenever the file grew by less than the front the daemon trimmed, which at a
   * three-second poll is most refreshes, i.e. it reintroduces the loss tether#107 built
   * this reducer to prevent. The interior case is the one place where we can say
   * something TRUE and cheap — those bytes are on screen — so we say it.
   *
   * # The ROLE check on the skip, which is what makes "already on screen" checkable
   *
   * "Those bytes are already rendered" rests on the pages on screen being contiguous, and
   * nothing in this file enforces contiguity — `prependHistory` still dedupes by
   * messageKey, and the pane documents a race in which a disjoint refresh lands inside an
   * in-flight "load earlier". Review could not reach an actual loss through either, but an
   * invariant maintained by nothing is not one to skip a message on.
   *
   * The role narrows it to the shape the daemon can actually produce. Only a run of
   * ASSISTANT records merges (ccsessions.go's `m.Role == "assistant" && out[n-1].Role ==
   * "assistant"`), so only an assistant bubble's `ord` can move when a window slides. A
   * user or system record is its own bubble at its own offset in every window: if its
   * position is inside a contiguous span on screen then it IS on screen and would have
   * matched, so "interior and unmatched" proves the span is NOT contiguous — and the
   * honest answer to that is the visible reset, not a skipped bubble.
   *
   * The one case where a skip could genuinely lose words is a single assistant run LONGER
   * than the byte window, where the held bubble is a prefix rather than a superstring of
   * the arriving one. It cannot reach the skip: such a run is the only message the window
   * holds, so nothing matches and (1) refuses first.
   *
   * # Returning false, in full
   *
   * Four ways, and all four end at `loadHistory`: a visible reset of the transcript to
   * the newest page. That is the honest failure. A silent hole in a conversation, or a
   * silently scrambled one, is not something a reader can see.
   *
   *   1. NOTHING matched (and the array was not empty) — the two ranges are disjoint,
   *      tether#107's case: over a megabyte written between two probes, so the new
   *      window starts after the array ended and merging would splice two stretches
   *      together with an invisible hole between them.
   *   2. An arriving `ord` sits below everything on screen (above).
   *   3. Any message on either side carries NO `ord` — the premise is then not
   *      checkable, and this reducer's whole job since tether#109 is to check it. Both
   *      daemon stores number every message they serve, so in practice this means the
   *      array holds a bubble the BROWSER made (`handleEnvelope`) and the daemon has not
   *      recorded, or a response came from a daemon older than this SPA.
   *
   *      Both halves of that gate earn their place, but not equally and not for the same
   *      shapes — worth writing down, because the first version of this paragraph got it
   *      wrong in a way only a mutant and a `node -e` caught:
   *
   *        - on SCREEN it is the whole defence. An unnumbered entry simply does not join
   *          the index, so the span that (2) and the interior case are measured against is
   *          silently short and the merge proceeds on it.
   *        - INCOMING, it is the defence against a STRING. `undefined` and `null` do fall
   *          through to (2) on their own — both compare false against any ord — but
   *          `"4096" > 1000` is TRUE while a Map keyed by numbers does not find `"4096"`,
   *          so a string ord matches nothing, passes the append branch and lands as a
   *          duplicate bubble at the end. That is the shape `hasOrd`'s own doc names, and
   *          it is why the gate is `Number.isFinite` rather than `!== undefined`.
   *   4. The APPENDED TAIL of the arriving page is not in `ord` order. Only the tail: a
   *      message that matches, or one that is skipped, is order-independent, so a page
   *      arriving as [3000, 1000, 2000] against those same three on screen merges happily
   *      and correctly. Both readers emit in file order, so neither case can happen from
   *      this daemon; the check is here because "the page is sorted" is exactly the kind of
   *      assumption this wi exists to stop making, and it is stated as narrowly as it is
   *      implemented because review found the wider claim in this comment first.
   *
   * An EMPTY existing array is not the disjoint case — there is nothing to overlap
   * with — so it installs rather than reporting failure. It also cannot be checked, and
   * does not need to be: one page is one contiguous range whatever is in it.
   */
  mergeHistory: (msgs) => {
    const s = get()
    if (msgs.length === 0) return true
    // `.slice()`, so the store never installs an array its caller still holds. Every
    // other exit from this reducer builds a new array; this one would not, and a store
    // that shares an array with a caller can change without notifying any subscriber.
    if (s.messages.length === 0) { set({ messages: msgs.slice() }); return true }
    // (3) above. Read as one pass over each side rather than folded into the loop, so
    // that "every message has a position" is a stated precondition of the arithmetic
    // below instead of a case scattered through it.
    if (!s.messages.every(hasOrd) || !msgs.every(hasOrd)) return false
    const at = new Map<number, number>()
    let lowest = Infinity
    let highest = -Infinity
    s.messages.forEach((m, i) => {
      const o = m.ord as number
      // First index wins, as tether#107's key map did. Unreachable for a well-formed
      // array — an ord identifies one record — and kept because the alternative is for a
      // repeat to silently redirect an update to a later slot.
      if (!at.has(o)) at.set(o, i)
      if (o < lowest) lowest = o
      if (o > highest) highest = o
    })
    const next = s.messages.slice()
    const fresh: Message[] = []
    let matched = 0
    let lastAppended = highest
    for (const m of msgs) {
      const o = m.ord as number
      const i = at.get(o)
      if (i !== undefined) {
        matched++
        // Content from the new page, identity from the old one. Spreading the incoming
        // message first is what makes a field the daemon has STOPPED sending disappear
        // rather than linger from the previous fetch.
        next[i] = { ...m, id: next[i].id }
        continue
      }
      // Strict, and `>=` here would be an EQUIVALENT mutant rather than a bug: an `ord`
      // equal to `highest` is in the index, so it was matched above and never reaches
      // this line. Written strict because that is what the sentence "strictly newer than
      // everything on screen" means, and the equality case being unreachable is a
      // property of the index rather than of this comparison.
      if (o > highest) {
        if (o <= lastAppended) return false // (4)
        lastAppended = o
        fresh.push(m)
        continue
      }
      // The interior case: already on screen, skip — but only for the one role whose
      // position a sliding window can move. See "The ROLE check on the skip" above.
      if (o >= lowest && m.role === 'assistant') continue
      return false // (2), and the non-assistant interior case
    }
    if (matched === 0) return false // (1)
    set({ messages: fresh.length > 0 ? [...next, ...fresh] : next })
    return true
  },
  // No-op when there is nothing to clear: ChatPane subscribes without a selector,
  // so an unconditional set() would re-render it (and invalidate the transcript
  // memo) on every session switch whether or not a notice existed.
  clearNotices: () => set((s) => (s.notices.length === 0 ? {} : { notices: [] })),
  clearFatal: () => set((s) => (s.fatal === null ? {} : { fatal: null })),
  // tether#88 — sending a prompt is a fact about the USER, not about the daemon.
  // This used to also null curTurnId + both turn clocks, on the reading that "a
  // new user turn ends the prior assistant turn's accumulation" (tether#34).
  // That reading is right whenever the prior turn is over, and whether it is over
  // is not something the user's keystroke reports.
  //
  // THE TURN-ENDERS, in full, because the rest of this file argues from the list
  // and a partial one is how it goes wrong. The daemon says it with a KindResult
  // — the turn's own, or the payload-less one registry.go's fanOut broadcasts
  // when an init'd session's stream ends. The browser says it for reasons of its
  // own on four more paths: stopTurn (the user pressed Stop), an error that is
  // terminal OR unparsable, setConnected(false), and loadHistory. All five run
  // finalizeTurn or the same field-for-field reset. A sixth, the 'fenced'
  // new-block branch, nulls curTurnId and both clocks WITHOUT stamping answerMs —
  // deliberately, and the 'usage' branch above documents living with it.
  //
  // Nulling the pointers HERE ends, locally and unilaterally, a turn whose deltas are still
  // arriving; those deltas then find no open bubble, open a SECOND one, and the
  // first is orphaned without its answerMs — nothing merges two bubbles
  // afterwards. Replayed against frames captured off a live daemon (store.test.ts,
  // tether#88), the old code produced exactly that: two assistant bubbles for one
  // answer, the first with no badge.
  //
  // It is also upstream of tether#83's fix: by the time the daemon's rejection of
  // the second prompt gets back, this reset has already run, which is why keeping
  // curTurnId in case 'error' could not reach it.
  //
  // Two paths reach it. injectAndSend (panes/chat/index.tsx, click-to-work) has
  // no streaming gate at all; and the composer's own gate is `streaming`
  // (shouldSendOnEnter), which a non-terminal error clears while deliberately
  // leaving the turn open (tether#83) — so Enter works again mid-turn.
  //
  // A SINGLE curTurnId slot is still enough, and that was checked rather than
  // assumed — the two providers lifecycle.go registers do not agree on what
  // happens to the second prompt, so "one turn at a time" had to be established
  // separately for each:
  //   - opencode rejects it. SendPrompt's busy CompareAndSwap branch
  //     (internal/agent/opencode_provider.go) emits EventError "busy: another
  //     prompt is running" and returns nil, leaving the run in flight untouched.
  //     Read from the source; the resulting frames are captured in store.test.ts.
  //   - cc accepts it. ccSession.SendPrompt (internal/agent/claude_provider.go)
  //     has no gate and writes it straight to cc's stdin. What cc then does is
  //     cc's business and not something this repo can guarantee, so it was
  //     MEASURED, twice, on 2026-08-09: against a bare cc driven with Spawn's
  //     stream-json flags, minus the --session-id production pins (second prompt
  //     written at 7674ms, 2.2s into an answer whose first
  //     delta came at 5500ms; that turn's `result` at 19209ms; only then a fresh
  //     system/init at 19246ms and the second turn's result at 21112ms), and
  //     through the daemon over WebTransport (first result 7064ms, second turn's
  //     delta 10644ms, its result 10650ms). Neither run put a delta of the second
  //     turn before the first turn's result.
  // Both observations, not a law: what the store relies on is that the frames it
  // is fed never interleave two turns, and if a future provider did interleave
  // them, one slot would be the wrong shape and this is the comment to come back
  // to. The measurement is also why the fix is not "block the second prompt in
  // the composer": for cc the queued prompt really does run, so refusing to send
  // it would delete working behaviour to paper over a store bug.
  //
  // `stopped` still clears, and only it: it is the one piece of this reset that
  // is genuinely about the user (tether#42 — the next user turn re-arms delta
  // handling after a manual Stop), and it cannot conflict with an open turn
  // because stopTurn runs finalizeTurn first, so stopped === true implies
  // curTurnId === null. The two clocks are dropped rather than kept-conditionally
  // because neither is ever set unless curTurnId is already set or is being set in
  // the same update, and every path that nulls curTurnId nulls both — so when
  // there is no open turn they are already null and this reset was a no-op, and
  // when there is one it was the bug. (The converse does not hold and does not
  // need to: a tool_use can open a bubble with both clocks still null.)
  //
  // WHAT THIS GIVES UP, stated rather than left to be found. The reset was also,
  // by accident, a BOUND: whatever state curTurnId was in, the next prompt reset
  // it, so a stale pointer could never outlive one send. Two things can leave one
  // stale, and neither is hypothetical:
  //
  //   - A turn's closing frame is droppable. registry.go's broadcast does a
  //     non-blocking send and drops for a subscriber whose channel is full (the
  //     chat subscriber's is 32 deep, wt_chat.go).
  //   - Attachment.adopt (internal/session/attach.go) moves this browser's
  //     subscription off a dead Entry onto its replacement, and its own doc says
  //     why: otherwise the corpse's buffered tail — "and the turn-ender fanOut
  //     broadcasts when the corpse's stream ends" — would arrive after the
  //     replacement has started answering. So on that path the dead turn's
  //     KindResult is withheld on purpose, and no session_ready or notice follows
  //     either (serveChat sends session_ready once, before its drain loop, and the
  //     loop rewrites every env.SessionID to the sid resolved then) — the browser
  //     cannot see that its session was swapped underneath it.
  //
  // With a stale pointer the next turn's text appends to the previous bubble, and
  // the badges go with it: finalizeTurn stamps answerMs from the OLD turn's
  // answerStartTs, so it reports the gap between the turns as well, and 'usage'
  // overwrites the old turn's token counts. A wrong badge, not a missing one.
  //
  // Kept anyway, and the adopt case is why the trade is smaller than it reads: the
  // old reset did not PREVENT that mixing, it only moved the boundary. The corpse
  // goes on draining until adopt runs, so under the old code the tail opened a
  // fresh bubble and the replacement's answer then appended into THAT — corpse tail
  // and new answer in one bubble either way, just a different one. What the old
  // code bought was the dropped-result case, and it bought it by being wrong on
  // every ordinary concurrent prompt, which is the trade this wi refuses.
  addMessage: (msg) => set((s) => ({
    messages: [...s.messages, msg],
    ...(msg.role === 'user' ? { stopped: false } : {}),
  })),
  resolvePermission: (id) => set((s) => ({
    pendingPermissions: s.pendingPermissions.filter((p) => p.id !== id),
  })),
  // tether#42 — manual interrupt: cc aborts the turn with no EventResult, so
  // close it locally (same reducer as 'result'). Idempotent.
  stopTurn: () => set((s) => ({ ...finalizeTurn(s), stopped: true })),
  setConnected: (v) => v
    ? set({ connected: true, connection: { state: 'live', latency: 0, attempt: 0 } })
    : set({ connected: false, streaming: false, streamingMsgId: null, curTurnId: null, thinkingStartTs: null, answerStartTs: null, stopped: false, connection: { state: 'dropped', latency: 0, attempt: 0 } }),
  setConnection: (patch) => set((s) => ({ connection: { ...s.connection, ...patch } })),

  handleEnvelope: (env) => {
    switch (env.kind) {
      case 'message': {
        const p = env.payload
        if (p && typeof p === 'object') {
          const pObj = p as Record<string, unknown>
          if (pObj['type'] === 'session_ready') {
            // tether#45 — PERSIST the sid (via setSessionId → localStorage), not
            // just set state. Previously this did a plain set() that bypassed
            // localStorage, so only the click-to-work paths ever wrote
            // tether_last_sid; a NORMAL chat session's sid was lost on reload →
            // doConnect had no ?sid= to resume and ChatPane mount had nothing to
            // restore → an empty "new" session. Persisting here lets both resume.
            get().setSessionId(pObj['sessionId'] as string)
            break
          }
          if (pObj['type'] === 'notice') {
            // tether#50 — a session-level system line, not turn content: the
            // daemon sends it right after session_ready when a `cc --resume`
            // failed and it started a fresh session instead, AND the dead
            // session actually had history to lose.
            //
            // Deliberately does NOT touch curTurnId / streamingMsgId /
            // streaming / the timing stamps — same reasoning as session_ready
            // above. The daemon replays the buffered prompt onto the new
            // session immediately after this, so claiming the turn cursor here
            // would make the real answer accumulate into the NOTICE's bubble.
            // It is also exempt from the `stopped` gate below for the same
            // reason session_ready is: it is session lifecycle, not a late
            // delta from a turn the user stopped.
            //
            // tether#57 — lands in `notices`, NOT `messages`. session_ready (which
            // the daemon SENDS just before this one) sets the new sid, which fires
            // ChatPane's history refetch, and that refetch's loadHistory replaces
            // `messages` wholesale. Appending here put the notice directly in the
            // path of that replace; it survived only while the refetch happened to
            // be skipped mid-stream. Keeping it in a list loadHistory does not own
            // removes the race rather than re-timing it. Still ignores
            // env.SessionID (send order is not delivery order — see wt_chat.go).
            const noticeText = typeof pObj['text'] === 'string' ? (pObj['text'] as string) : ''
            if (noticeText) {
              // Nothing prunes this list except a deliberate session switch or a
              // page reload (before #57 a quiet refetch pruned it — that WAS the
              // bug, but it also capped the pile). The text is a compile-time
              // constant in wt_chat.go, so a long-lived tab that falls back
              // several times would otherwise stack identical lines: collapse a
              // repeat of the line already showing.
              //
              // "The line already showing" means the last SESSION banner, not the
              // last notice of any kind (tether#80). Comparing the last element
              // outright made an unrelated line an accidental un-gate: a tether#77
              // or tether#80 line landing in between let the very next identical
              // banner through. That was already reachable before, and this change
              // makes it much more so, so the class is now the boundary rather
              // than whatever happened to arrive most recently.
              set((s) => {
                const lastBanner = [...s.notices].reverse().find((n) => n.kind === 'session')
                if (lastBanner?.text === noticeText) return {}
                return {
                  notices: [...s.notices, { id: crypto.randomUUID(), text: noticeText, ts: Date.now(), kind: 'session' as const }],
                }
              })
            }
            break
          }
          if (pObj['type'] === 'permissions_withdrawn') {
            // tether#137 — the daemon has taken these requests back, because the
            // agent whose gate subprocess was the only reader of the decision has
            // been reaped (internal/session/registry.go teardown ->
            // Registry.WithdrawPending). Until this branch existed the card stayed
            // on screen and stayed clickable: the POST succeeded, the gate really
            // did receive `allow:true` and exit 0, and nothing whatsoever acted on
            // it (tether#134 §2.5). An answerable prompt for a tool call that
            // cannot exist is worse than no prompt — see the "a notice a user has
            // caught lying" argument in internal/server/wt_chat.go.
            //
            // Matched on id alone. The envelope carries no sid on purpose (see
            // permissionsWithdrawnEnvelope in internal/server/mux.go: serveChat
            // would overwrite it with the RECEIVING connection's sid), and it does
            // not need one — ids are unique daemon-wide, and BroadcastAll sends
            // this to every client because the backfill means a card for one
            // session can be on screen in a tab attached to another.
            const raw = pObj['ids']
            const ids = Array.isArray(raw) ? raw.filter((v): v is string => typeof v === 'string') : []
            set((s) => {
              const removed = s.pendingPermissions.filter((p) => ids.includes(p.id))
              // Nothing to say to a client that never had the card. This is the
              // ordinary case for the reload that motivated the whole change: the
              // withdrawal happens in teardown, which for a graceful close is
              // typically over long before the new page has finished connecting,
              // so the backfill the fresh tab receives simply no longer contains
              // the request and there is no card to explain. Announcing a removal
              // to a reader who never saw the thing removed is noise, and it is
              // also how a daemon-wide broadcast would put a line in every
              // unrelated tab.
              if (removed.length === 0) return {}
              // Removed, not greyed out. The card is rendered by
              // fenced-blocks/PermissionBlock from a PermissionRequest, and a
              // disabled state there would be a second, weaker answer to "is this
              // answerable" living next to the queue's membership — the same
              // duplicate-rule argument permission.Manager.Pending makes for not
              // re-testing expiry. So the queue is the single answer, and the
              // notice is what stops the removal from being silent.
              const text = removed.length === 1
                ? 'A tool request can no longer be answered — the agent that asked for it has ended.'
                : `${removed.length} tool requests can no longer be answered — the agent that asked for them has ended.`
              return {
                pendingPermissions: s.pendingPermissions.filter((p) => !ids.includes(p.id)),
                notices: [...s.notices, { id: crypto.randomUUID(), text, ts: Date.now(), kind: 'permission_withdrawn' as const }],
              }
            })
            break
          }
          // After a manual stop (tether#42), cc may still flush a few buffered
          // deltas; drop them so they don't spawn a new bubble or resume
          // streaming. Cleared by the next user turn (addMessage) or a terminal
          // result/error. session_ready above is exempt (session-level, not turn
          // content).
          if (get().stopped) break
          if (pObj['type'] === 'tool_use') {
            // Surface the tool call in the current turn's bubble (tether#37).
            // The daemon already sent {name,input} (registry.go translateEvent);
            // earlier this branch threw it away and only kept the streaming flag.
            // Mirror the thinking-branch accumulation, but do NOT touch
            // answerStartTs/thinkingStartTs/streamingMsgId — a tool call is
            // neither the answer nor thinking, so it must not start the answer
            // clock (tether#36) or claim the streaming cursor.
            const name = typeof pObj['name'] === 'string' ? (pObj['name'] as string) : ''
            if (!name) { set({ streaming: true }); break }
            const tc: ToolCall = {
              id: typeof pObj['id'] === 'string' ? (pObj['id'] as string) : '',
              name,
              input: pObj['input'],
            }
            set((s) => {
              if (s.curTurnId) {
                const id = s.curTurnId
                return {
                  streaming: true,
                  messages: s.messages.map(m =>
                    m.id === id ? { ...m, tools: [...(m.tools ?? []), tc] } : m
                  ),
                }
              }
              // Tool call arrived before any thinking/answer — open the turn's
              // bubble now (no timing stamps: not thinking, not answer).
              const id = crypto.randomUUID()
              return {
                streaming: true,
                curTurnId: id,
                messages: [...s.messages, { id, role: 'assistant' as const, text: '', ts: Date.now(), tools: [tc] }],
              }
            })
            break
          }
          if (pObj['type'] === 'tool_result') {
            // The output of a tool cc ran (tether#38). The daemon forwards it
            // keyed by tool_use_id; hang it on the matching ToolCall (from an
            // earlier tool_use), wherever that bubble is. Match-by-id only — do
            // NOT open a bubble or touch curTurnId/timing/streamingMsgId (a
            // result is neither the answer, thinking, nor a new turn segment).
            const tid = typeof pObj['tool_use_id'] === 'string' ? (pObj['tool_use_id'] as string) : ''
            if (!tid) { set({ streaming: true }); break }
            const result = {
              content: typeof pObj['content'] === 'string' ? (pObj['content'] as string) : '',
              isError: pObj['is_error'] === true,
            }
            set((s) => ({
              streaming: true,
              messages: s.messages.map(m =>
                m.tools?.some(t => t.id === tid)
                  ? { ...m, tools: m.tools.map(t => (t.id === tid ? { ...t, result } : t)) }
                  : m
              ),
            }))
            break
          }
          if (pObj['type'] === 'usage') {
            // The turn's token usage (tether#48). The daemon emits it right
            // before the 'result' envelope, so curTurnId still points at the
            // turn's bubble — attach {input,output} there for the badge. Do NOT
            // touch streaming/timing/streamingMsgId: usage is a turn-end signal,
            // not answer/thinking/tool content, and 'result' (arriving next)
            // owns closing the turn. If curTurnId is null (e.g. a turn that
            // ended on a fenced block, which resets curTurnId), there's no
            // bubble to carry the badge — drop it (same as answerMs).
            const input = typeof pObj['input'] === 'number' ? (pObj['input'] as number) : 0
            const output = typeof pObj['output'] === 'number' ? (pObj['output'] as number) : 0
            set((s) => {
              const id = s.curTurnId
              if (!id) return {}
              return { messages: s.messages.map(m => (m.id === id ? { ...m, usage: { input, output } } : m)) }
            })
            break
          }
          if (pObj['type'] === 'thinking') {
            // Extended-thinking delta (tether#34): accumulate into the CURRENT
            // turn's bubble. If the turn already has a bubble (from earlier
            // thinking OR answer text — a model may interleave a second thinking
            // block mid-answer), append to it so the whole turn stays ONE bubble
            // rather than fragmenting the answer. Only start a new bubble when the
            // turn has none yet.
            const delta = typeof pObj['text'] === 'string' ? (pObj['text'] as string) : ''
            if (!delta) { set({ streaming: true }); break }
            set((s) => {
              if (s.curTurnId) {
                const id = s.curTurnId
                return {
                  streaming: true,
                  // If a tool_use opened this bubble before any thinking (tether#37),
                  // thinkingStartTs is still null — stamp it at the first thinking
                  // delta so the collapsed "thought Xs" badge still measures (tether#34
                  // regression fix). No-op in the common thinking-first path where the
                  // new-bubble branch below already set it.
                  ...(s.thinkingStartTs == null ? { thinkingStartTs: Date.now() } : {}),
                  messages: s.messages.map(m =>
                    m.id === id ? { ...m, thinking: (m.thinking ?? '') + delta } : m
                  ),
                }
              }
              const id = crypto.randomUUID()
              return {
                streaming: true,
                curTurnId: id,
                thinkingStartTs: Date.now(),
                messages: [...s.messages, { id, role: 'assistant' as const, text: '', ts: Date.now(), thinking: delta }],
              }
            })
            break
          }
          break
        }
        if (typeof p !== 'string') break
        if (get().stopped) break // drop late answer text after a manual stop (tether#42)
        set((s) => {
          if (s.curTurnId) {
            // Append answer text to the current turn's bubble. On the FIRST answer
            // delta after thinking, stamp the thinking duration so the live
            // "thinking…" block collapses to "thought Xs" in place (tether#34).
            const id = s.curTurnId
            const started = s.thinkingStartTs
            return {
              streaming: true,
              streamingMsgId: id,
              // First answer delta of the turn starts the answer-duration clock (tether#36).
              ...(s.answerStartTs == null ? { answerStartTs: Date.now() } : {}),
              messages: s.messages.map(m => {
                if (m.id !== id) return m
                const firstAnswer = m.text === '' && m.thinking != null && m.thinkingMs == null && started != null
                return { ...m, text: m.text + p, ...(firstAnswer ? { thinkingMs: Date.now() - started } : {}) }
              }),
            }
          }
          // First content of the turn is plain answer text (no thinking).
          const id = crypto.randomUUID()
          return {
            streaming: true,
            curTurnId: id,
            streamingMsgId: id,
            answerStartTs: Date.now(), // no-thinking turn: answer starts now (tether#36)
            messages: [...s.messages, {
              id,
              role: 'assistant',
              text: p,
              ts: Date.now(),
            }],
          }
        })
        break
      }
      case 'result':
        // Stamp the answer-generation duration on the turn's bubble as the
        // "done" signal (tether#36), then close the turn. Shared with the
        // manual interrupt path (stopTurn) via finalizeTurn (tether#42).
        // Clear `stopped` too: a terminal result ends the (possibly
        // interrupted) turn, so later deltas belong to a fresh turn.
        set((s) => ({ ...finalizeTurn(s), stopped: false }))
        break
      case 'error': {
        // Parsed FIRST (tether#83). It used to be parsed after the clear below,
        // which was only tenable while the clear ignored the classification.
        // A null parse still means what tether#63's note below says it means.
        const parsed = parseErrorPayload(env.payload)
        // tether#83 — "the spinner" and "the turn" are two different pieces of
        // state and this frame is not the same news about both. What stood here
        // cleared them together, under an argument ("even a terminal refusal ends
        // whatever turn was in flight") that is true and that covers only the
        // TERMINAL case; the non-terminal case it also governed was never argued.
        //
        // A non-terminal error does not, on its own, say the turn is over — and
        // on some of the paths that raise one it demonstrably is not. Captured off
        // a live daemon + live opencode (the frames are replayed in store.test.ts):
        // the error envelope lands mid-answer and KindMessage deltas for the SAME
        // turn keep arriving after it, for another three seconds. That is visible
        // in the provider too — opencode_provider.go's session.error branch emits
        // from the SSE reader and keeps reading, and its SendPrompt busy branch
        // emits, returns nil, and never touches the run already in flight.
        //
        // Nulling curTurnId there routes the next delta down the "no bubble open"
        // path above, which opens a SECOND assistant bubble; the first keeps the
        // text it had and never gets its answerMs, because finalizeTurn only ever
        // stamps the turn pointer it finds. Nothing merges two bubbles afterwards,
        // so unlike the spinner this one does not come back.
        //
        // `streaming` is cleared EITHER WAY, and that asymmetry is the decision
        // here rather than a leftover:
        //
        //   - The browser cannot tell "the turn is still running" from "the prompt
        //     never started". Both arrive as ErrCodeAgent carrying only free text
        //     — no agent-sourced code is finer than that one — and on three of the
        //     sites that raise it (opencode_provider.go's resume-serve failure and
        //     its two `opencode run` start failures) this frame is the only
        //     turn-closer that will ever be sent: they return without emitting an
        //     EventResult and without a run in flight to emit one later.
        //   - "Is a turn open?" is not the discriminator either, though tether#88
        //     made it a better one than it was: until then addMessage nulled
        //     curTurnId the instant the user sent, so on the busy path the browser
        //     had already ended the first turn itself before the rejection got
        //     back — which is why THIS fix could not repair that split, and it is
        //     no longer true. The pointer now survives there and does say "a turn
        //     is running". What still rules it out is that it can DANGLE (see
        //     addMessage for the two ways), and a stale one would answer "keep the
        //     spinner" forever — which the next bullet says is the unrecoverable
        //     half of this choice.
        //   - The two ways of being wrong are not equally bad. A spinner cleared
        //     while the agent is still working repairs itself on the next delta:
        //     the text and thinking branches above set `streaming: true` on every
        //     one they handle. A spinner left running when nothing is coming does
        //     not repair itself, and it is not cosmetic — shouldSendOnEnter
        //     (panes/chat/index.tsx) requires !streaming, so Enter silently stops
        //     sending until the user works out that the button now says Stop.
        //
        // Where the turn really is over, something else already closes it —
        // addMessage's comment holds the full list of those, kept in one place
        // since tether#88 found this one and the one there disagreeing. What
        // tether#88 removed from it is addMessage itself: a user sending a prompt
        // says nothing about whether the daemon finished the last turn, and
        // treating it as if it did was that wi.
        //
        // What the surviving pointers change before then is small and deliberate.
        // The answer cursor and the waiting dots are gated on `streaming`, which
        // this frame clears; ThinkingBlock's `live` flag is gated on it too since
        // tether#83, for the invariant its own prop doc states. What DOES change:
        // a 'usage' envelope (gated on curTurnId, not on streaming) now lands its
        // token counts on the turn's bubble instead of being dropped, and the
        // turn's eventual 'result' stamps answerMs on the bubble that holds the
        // answer rather than on a fragment of it. Both are the tether#36/#48
        // badges going to the right place.
        set(parsed && !parsed.terminal
          ? { streaming: false, stopped: false }
          : { streaming: false, streamingMsgId: null, curTurnId: null, thinkingStartTs: null, answerStartTs: null, stopped: false })
        // tether#63 — record a TERMINAL refusal so ChatPane's onClose can tell
        // "the daemon just refused this connection for good" apart from an
        // ordinary transient drop and stop the reconnect ladder instead of
        // retrying forever (see wire/errors.go's package doc for why Terminal
        // travels as its own field). A non-terminal or unparsable payload
        // (including the pre-tether#63 bare-string shape a stale daemon build
        // might still send) leaves `fatal` untouched — there is nothing new to
        // act on, not a reason to clear a refusal that may already be set.
        if (parsed && parsed.terminal) {
          set({ fatal: { code: parsed.code, message: parsed.message } })
          break
        }
        // tether#77 — a prompt_undelivered error used to stop here, having done
        // nothing but clear the spinner above. That is not a small omission:
        // the daemon classified an error, decided it was worth a frame, sent
        // it, and the user saw a tab that looked idle and healthy. Every prompt
        // typed afterwards vanished the same way. The daemon-side half of #77
        // (attach.go's reopen now classifying the branches where a prompt is
        // gone for good) would have changed nothing at all without this line —
        // the frame would have arrived and been dropped right here.
        //
        // Gated on the ONE code that means "the words you just sent are gone",
        // not on `!terminal`, which is a much larger set and a different claim.
        // The other retryable codes are all excluded for a reason:
        //
        //   - connection_closed is produced when the browser's own context is
        //     already done, so there is nobody left to show it to.
        //   - spawn_failed and session_unconfirmed accompany a connection the
        //     daemon is closing. The reconnect ladder and its failed card
        //     already speak for those, and each ladder attempt would add
        //     another copy of the same line.
        //   - agent_error has its OWN branch below since tether#80. It was left
        //     out of this one deliberately and that decision stands: the session
        //     is fine and the message is about the turn's content, not about a
        //     prompt being lost, so aliasing it onto this sentence would be
        //     wrong even where it is not noisy — and it is noisy, which is why
        //     it needed a bounded presentation rather than this unbounded one.
        //
        // It lands in `notices`, not `messages`, for tether#57's reason: a
        // history refetch replaces `messages` wholesale, and an explanation of
        // why the last thing you typed did not happen is exactly the thing that
        // must not disappear when the transcript reloads.
        //
        // NOT deduplicated against the previous notice, unlike the session
        // notice above. That dedup exists because its text is a compile-time
        // constant that a long-lived tab can stack identical copies of with no
        // new information. These correspond one-to-one with a prompt the user
        // pressed enter on, so three identical lines are three lost prompts —
        // collapsing them would under-report exactly the thing being reported.
        //
        // Prefixed, because `message` is the daemon's diagnostic text (session
        // ids, `write |1: broken pipe`) and the one thing the user needs from
        // it is the part the daemon never says: their message did not go.
        if (parsed && parsed.code === ErrCodePromptUndelivered) {
          set((s) => ({
            notices: [...s.notices, {
              id: crypto.randomUUID(),
              // Classed so no other branch's repeat policy can reach it — in
              // particular so the tether#80 collapse and cap below cannot fold or
              // evict a line that stands for one specific lost prompt.
              kind: 'prompt_undelivered' as const,
              text: `Message not delivered — ${parsed.message}`,
              // Strictly after everything already in the transcript, which is
              // this line's meaning: it explains the last thing the user sent,
              // so reading above that prompt inverts it. nextNoticeTs owns why
              // a bare Date.now() is not enough (tether#77) — the rule moved
              // there when tether#80's line needed the same one.
              ts: nextNoticeTs(s.messages),
            }],
          }))
          break
        }
        // tether#80 — the gap the comment above named and left open. The agent
        // told the daemon something went wrong, the daemon classified it and sent
        // a frame, and until now the browser answered by clearing the spinner and
        // saying nothing: the turn just stopped, and whatever the agent said about
        // why died here.
        //
        // WHY THIS WORDING claims so little. ErrCodeAgent covers a wide range of
        // conditions and the wire says nothing about WHICH one — established by
        // reading every emit site, not from the code's own description of itself.
        // Eight are in internal/agent/opencode_provider.go:
        //
        //   - session.error from opencode's event stream: a complaint about the
        //     turn, session alive, prompt delivered.
        //   - SendPrompt's busy branch, the resume-serve failure, and the two
        //     `opencode run` start failures: they emit and then return nil, so the
        //     PROMPT IS GONE while the session stays alive and usable.
        //   - watchServeExit: emits and then calls closeEvents, i.e. THE SESSION
        //     IS ALREADY DEAD by the time this frame is read.
        //
        // The ninth is claude_provider.go's ccSession.abandon, which tether#83's
        // review found: cc's stdout read ending in an error kills the subprocess
        // and emits here, on a turn that WAS mid-answer. tether#80 recorded that
        // "claude_provider.go never emits agent.EventError at all" — from the same
        // sentence in wire/errors.go, which was wrong and is corrected there too.
        // It changes nothing about the wording below, which is the point: it is
        // one more materially different situation the wire cannot distinguish.
        //
        // So neither "your prompt is gone" (tether#77's sentence) nor "the session
        // is fine, this is just about the turn" is true across that range, and
        // asserting either would be a guess dressed as a diagnosis. The text
        // therefore states only what is true at every point in it — who spoke, and
        // what they said — and lets the message carry the rest. Note that
        // wire/errors.go's own ErrCodeAgent doc used to over-generalise in exactly
        // that way ("the session … is still alive and usable"); it is corrected in
        // this change rather than cited, because it was where the wrong idea came
        // from.
        //
        // Also in `notices` rather than `messages`, and NOT because notices are
        // the convenient list: because the daemon never persists this text.
        // registry.go's fanOut writes thinking / tool_use / tool_result / text /
        // blocks to HistoryStore and forwards agent.EventError without recording
        // it, so the live frame is the only copy in existence. In `messages` the
        // history refetch that session_ready triggers would replace it away for
        // good — tether#57's bug, re-introduced by the code fixing this one.
        //
        // Bounded by appendAgentErrorNotice, which is where the collapse, the
        // count and the cap are explained.
        if (parsed && parsed.code === ErrCodeAgent) {
          set((s) => ({
            notices: appendAgentErrorNotice(s.notices, {
              id: crypto.randomUUID(),
              text: `The agent reported an error — ${parsed.message}`,
              ts: nextNoticeTs(s.messages),
            }),
          }))
        }
        break
      }
      case 'fenced': {
        // D-19 fenced block, live-replace-by-BlockID (tether#8 T8, contract §3):
        // if a message already carries a block with this BlockID, replace that
        // message's block IN PLACE (same message id, so expandedBlocks/expand
        // state survives) — this is how a re-emitted block (e.g. DAG progress)
        // animates instead of appending a duplicate card. A new/absent BlockID
        // appends a new block message.
        //
        // On a NEW block (append) we close the in-progress text bubble
        // (streamingMsgId: null) so subsequent text deltas start a fresh
        // bubble AFTER the card — this keeps live rendering in stream order
        // ([before][card][after]), matching what the reload path reconstructs
        // from history. On a re-emit (replace-in-place, same BlockID) we leave
        // streaming untouched — a card update is not a text boundary.
        const fb = env.payload as FencedBlock | undefined
        if (!fb || typeof fb.kind !== 'string') break
        if (get().stopped) break // drop late fenced blocks after a manual stop (tether#42)
        set((s) => {
          const idx = fb.blockId
            ? s.messages.findIndex((m) => m.block?.blockId === fb.blockId)
            : -1
          if (idx >= 0) {
            const messages = s.messages.slice()
            messages[idx] = { ...messages[idx], block: fb }
            return { messages }
          }
          return {
            streamingMsgId: null,
            curTurnId: null,
            // A fenced block ends the current text/thinking segment: reset both
            // clocks so the NEXT answer segment times only itself (tether#36 —
            // otherwise answerStartTs leaks across the card and inflates the badge
            // or stamps it onto a no-answer bubble). thinkingStartTs self-heals via
            // the thinking new-bubble path, reset here for a clean boundary.
            thinkingStartTs: null,
            answerStartTs: null,
            messages: [...s.messages, {
              id: crypto.randomUUID(),
              role: 'assistant' as const,
              text: '',
              ts: Date.now(),
              block: fb,
            }],
          }
        })
        break
      }
      case 'permission': {
        // Parallel tools each send their own KindPermission (tether#40): APPEND to
        // the queue instead of overwriting a single slot, so every request stays
        // approvable. Dedup by id so a re-emitted request doesn't duplicate a block.
        //
        // The dedupe is load-bearing rather than defensive since tether#132: a
        // request the daemon re-sends on attach can arrive alongside a live
        // broadcast of the same request (session.Entry.backfill registers the
        // channel before it replays, deliberately — see its doc for why the other
        // order loses requests instead of duplicating them). This is what makes
        // that trade cost nothing.
        const req = env.payload as PermissionRequest
        // Tagged with the sid this envelope came in on, which serveChat stamps on
        // everything leaving a chat connection, so `loadHistory` can tell "the
        // session I am reloading" from "the session I have left". An envelope with
        // no sid is tagged null and is not claimed by any session — see the reducer.
        const pending: PendingPermission = { ...req, sessionId: env.sessionId ?? null }
        set((s) => (
          s.pendingPermissions.some((p) => p.id === req.id)
            ? {}
            : { pendingPermissions: [...s.pendingPermissions, pending] }
        ))
        break
      }
      default:
        break
    }
  },

  select: (sel) => {
    // null clears both (reset / project switch); otherwise only the field(s)
    // present are touched, so a file and a wi drawer can coexist (tether#28).
    if (sel === null) {
      set({ selectedWiId: null, selectedFile: null })
      return
    }
    const patch: { selectedWiId?: string | null; selectedFile?: SelectedFile | null } = {}
    if ('wiId' in sel) patch.selectedWiId = sel.wiId ?? null
    if ('file' in sel) patch.selectedFile = sel.file ?? null
    set(patch)
  },

  setWorkProject: (p) => set({ workProject: p }),
  setActiveWorkspace: (ws) => { set({ activeWorkspace: ws }); rememberWorkspace(ws) },
  setWorkspacesLoaded: (v) => set({ workspacesLoaded: v }),

  // tether#52 — publish the initial workspace selection and release ChatPane's
  // first-connect gate in ONE update. The single `set` is the whole point, not a
  // tidiness: zustand notifies listeners SYNCHRONOUSLY from inside `set`, and
  // ChatPane's gate listener calls doConnect right there, which reads
  // `activeWorkspace` via getState(). So anything that flips `workspacesLoaded`
  // in a separate update from the one that publishes the workspace hands
  // doConnect an EMPTY selection — and a new session's cwd is pinned at spawn,
  // for that session's whole life.
  //
  // That is not hypothetical: the first version of this slice released the gate
  // from load()'s `finally` while `activeWorkspace` was published by a
  // `useEffect`, i.e. one React commit later. The gate fired first, every fresh
  // session connected with no `ws`, and the agent always spawned in
  // --workspace-root — the feature was inert with a fully green test suite.
  // store.test.ts pins the ordering by asserting on a subscriber's FIRST
  // notification; keep the two fields in one `set` and it cannot come back.
  //
  // tether#66 — this also persists the resolved selection (rememberWorkspace),
  // which matters on the very first page of a profile: the id chosen here is the
  // one a plain refresh has to come back to. It runs AFTER the `set`, never
  // before — the gate must open even if persistence fails.
  settleWorkspaces: (ws) => { set({ activeWorkspace: ws, workspacesLoaded: true }); rememberWorkspace(ws) },
}))
