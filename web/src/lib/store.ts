import { create } from 'zustand'
import type { Envelope, FencedBlock } from './wire.gen'

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
  return msg
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

/** A selected file within a workspace, identified by workspace id + path
 *  relative to the workspace root (matches fetchFile's `path` param). */
export interface SelectedFile {
  wsId: string
  path: string
}

interface AppState {
  sessionId: string | null
  messages: Message[]
  // Pending PreToolUse permission requests (tether#40). A QUEUE, not one slot:
  // parallel tools each send their own KindPermission, so a single slot let the
  // later request clobber the earlier one → all-but-one timed out. Live-only.
  pendingPermissions: PermissionRequest[]
  connected: boolean
  streaming: boolean
  connection: Connection
  streamingMsgId: string | null   // id of the bubble receiving ANSWER text (drives the cursor)
  // tether#34 — ONE assistant bubble per turn: thinking + answer text accumulate
  // into it, so a turn with interleaved thinking blocks (thinking→text→thinking→
  // text) stays a single bubble instead of fragmenting. Both transient, never persisted.
  curTurnId: string | null        // the current turn's assistant bubble (null between turns)
  thinkingStartTs: number | null  // ts of the first thinking delta this turn
  answerStartTs: number | null    // ts of the first answer delta this turn (tether#36)

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

  setSessionId: (id: string) => void
  loadHistory: (msgs: Message[]) => void
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

export const useStore = create<AppState>((set) => ({
  sessionId: null,
  messages: [],
  pendingPermissions: [],
  connected: false,
  streaming: false,
  streamingMsgId: null,
  curTurnId: null,
  thinkingStartTs: null,
  answerStartTs: null,
  connection: { state: 'connecting', latency: 0, attempt: 0 },
  selectedWiId: null,
  selectedFile: null,
  workProject: '',

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
    return { messages: reduced, streamingMsgId: null, streaming: false, curTurnId: null, thinkingStartTs: null, answerStartTs: null, pendingPermissions: [] }
  }),
  addMessage: (msg) => set((s) => ({
    messages: [...s.messages, msg],
    // A new user turn ends the prior assistant turn's accumulation (tether#34).
    ...(msg.role === 'user' ? { curTurnId: null, thinkingStartTs: null, answerStartTs: null } : {}),
  })),
  resolvePermission: (id) => set((s) => ({
    pendingPermissions: s.pendingPermissions.filter((p) => p.id !== id),
  })),
  // tether#42 — manual interrupt: cc aborts the turn with no EventResult, so
  // close it locally (same reducer as 'result'). Idempotent.
  stopTurn: () => set((s) => finalizeTurn(s)),
  setConnected: (v) => v
    ? set({ connected: true, connection: { state: 'live', latency: 0, attempt: 0 } })
    : set({ connected: false, streaming: false, streamingMsgId: null, curTurnId: null, thinkingStartTs: null, answerStartTs: null, connection: { state: 'dropped', latency: 0, attempt: 0 } }),
  setConnection: (patch) => set((s) => ({ connection: { ...s.connection, ...patch } })),

  handleEnvelope: (env) => {
    switch (env.kind) {
      case 'message': {
        const p = env.payload
        if (p && typeof p === 'object') {
          const pObj = p as Record<string, unknown>
          if (pObj['type'] === 'session_ready') {
            set({ sessionId: pObj['sessionId'] as string })
            break
          }
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
        set((s) => finalizeTurn(s))
        break
      case 'error':
        // Clear the thinking/streaming indicator on a daemon-surfaced error so
        // the UI doesn't get stuck showing "Claude is thinking…" forever.
        set({ streaming: false, streamingMsgId: null, curTurnId: null, thinkingStartTs: null, answerStartTs: null })
        break
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
}))
