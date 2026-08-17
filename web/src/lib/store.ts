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
  return msg
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
   *  applies", which is the correct reading for a bare fixture. */
  kind?: 'agent_error' | 'prompt_undelivered' | 'session'
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
  // later request clobber the earlier one → all-but-one timed out. Live-only.
  pendingPermissions: PermissionRequest[]
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

  setSessionId: (id: string) => void
  loadHistory: (msgs: Message[]) => void
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

  setSessionId: (id) => {
    localStorage.setItem('tether_last_sid', id)
    set({ sessionId: id })
  },
  loadHistory: (msgs) => set(() => {
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
    // A session reset (page reload / session switch) drops any stale pending
    // permission requests — they belong to the prior session (tether#40).
    //
    // tether#57 — note what is NOT in this return: `notices`. This reducer is
    // the server-truth replace, and it does not own the notice list, so it
    // cannot drop it. Do not add `notices` here to "reset" it; use clearNotices
    // at the deliberate session-switch call sites instead.
    return { messages: reduced, streamingMsgId: null, streaming: false, curTurnId: null, thinkingStartTs: null, answerStartTs: null, stopped: false, pendingPermissions: [] }
  }),
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
        const req = env.payload as PermissionRequest
        set((s) => (
          s.pendingPermissions.some((p) => p.id === req.id)
            ? {}
            : { pendingPermissions: [...s.pendingPermissions, req] }
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
