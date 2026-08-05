import { create } from 'zustand'
import type { Envelope, ErrorPayload, FencedBlock } from './wire.gen'

/** A single tool_use content block the daemon already extracts and puts on the
 *  wire ({name,input}); tether#37 is where the frontend finally KEEPS it instead
 *  of discarding it (the old tool_use branch only set the streaming flag). */
export interface ToolCall {
  id: string
  name: string
  input: unknown
  /** The tool's output, hung under the call (matched by tool_use_id) once the
   *  daemon forwards the tool_result (tether#38). Absent until then; live-only. */
  result?: { content: string; isError: boolean }
}

export interface Message {
  id: string
  role: 'user' | 'assistant' | 'system'
  text: string
  ts: number
  /** Optional D-19 fenced block rendered inline in this message bubble. */
  block?: FencedBlock
  /** Accumulated extended-thinking text for this assistant turn (tether#34).
   *  Ephemeral / live-only — the daemon never persists it to history, so it is
   *  absent after a page reload (spec D3). */
  thinking?: string
  /** Wall-clock ms spent thinking before the answer began (tether#34); set when
   *  the first answer delta arrives. Undefined while thinking is still live. */
  thinkingMs?: number
  /** Wall-clock ms spent generating the answer (tether#36): first answer delta →
   *  result. Stamped at result as the turn's "done" signal. Live-only (not
   *  persisted), so absent after a page reload. */
  answerMs?: number
  /** Tool calls (Read/Bash/Edit/…) the agent made during this turn (tether#37),
   *  in arrival order. Live-only — the daemon never persists tool_use to history,
   *  so absent after a page reload (same as thinking/answerMs). */
  tools?: ToolCall[]
  /** The turn's token usage (tether#48): input/output token counts from cc's
   *  result event, attached to the turn bubble for the "⇅ in↑/out↓" badge.
   *  Live-only — the daemon does not persist usage to history, so absent after
   *  a page reload (same as thinking/answerMs). */
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
  const asMessage = (n: Notice): Message => ({ id: n.id, role: 'system', text: n.text, ts: n.ts })
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

  // The workspace the user is currently browsing in the left WorkspacePane
  // (tether#47). Chat's @-mention picker queries this workspace's files; the
  // chat session isn't itself bound to a workspace (ActiveSID is unwired and
  // cc's cwd is decoupled), so following the browsed workspace is the binding.
  // Carries the abspath so @ can insert @<abspath> (cc reads it regardless of cwd).
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
  addMessage: (msg) => set((s) => ({
    messages: [...s.messages, msg],
    // A new user turn ends the prior assistant turn's accumulation (tether#34).
    ...(msg.role === 'user' ? { curTurnId: null, thinkingStartTs: null, answerStartTs: null, stopped: false } : {}),
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
              set((s) => (s.notices[s.notices.length - 1]?.text === noticeText ? {} : ({
                notices: [...s.notices, { id: crypto.randomUUID(), text: noticeText, ts: Date.now() }],
              })))
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
                  // delta so the collapsed "思考 Xs" badge still measures (tether#34
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
            // "思考中…" block collapses to "思考 Xs" in place (tether#34).
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
        // Clear the thinking/streaming indicator on a daemon-surfaced error so
        // the UI doesn't get stuck showing "Claude is thinking…" forever. This
        // clear happens regardless of the payload's classification below —
        // even a terminal refusal ends whatever turn was in flight.
        set({ streaming: false, streamingMsgId: null, curTurnId: null, thinkingStartTs: null, answerStartTs: null, stopped: false })
        // tether#63 — record a TERMINAL refusal so ChatPane's onClose can tell
        // "the daemon just refused this connection for good" apart from an
        // ordinary transient drop and stop the reconnect ladder instead of
        // retrying forever (see wire/errors.go's package doc for why Terminal
        // travels as its own field). A non-terminal or unparsable payload
        // (including the pre-tether#63 bare-string shape a stale daemon build
        // might still send) leaves `fatal` untouched — there is nothing new to
        // act on, not a reason to clear a refusal that may already be set.
        const parsed = parseErrorPayload(env.payload)
        if (parsed && parsed.terminal) {
          set({ fatal: { code: parsed.code, message: parsed.message } })
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
  setActiveWorkspace: (ws) => set({ activeWorkspace: ws }),
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
  settleWorkspaces: (ws) => set({ activeWorkspace: ws, workspacesLoaded: true }),
}))
