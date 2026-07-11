import { create } from 'zustand'
import type { Envelope, FencedBlock } from './wire.gen'

export interface Message {
  id: string
  role: 'user' | 'assistant' | 'system'
  text: string
  ts: number
  /** Optional D-19 fenced block rendered inline in this message bubble. */
  block?: FencedBlock
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
  pendingPermission: PermissionRequest | null
  connected: boolean
  streaming: boolean
  connection: Connection
  streamingMsgId: string | null   // id of the in-progress assistant bubble

  // Middle-canvas selection (tether#20): exactly one of the two is ever set.
  // Selecting a work item clears the file selection and vice-versa; passing
  // `null` clears both (e.g. deselect / no artifact focused).
  selectedWiId: string | null
  selectedFile: SelectedFile | null

  // Currently-focused Work project (tether#23): shared by the middle
  // knowledge-graph (WorkGraphView) and the right Work detail pane so both
  // render the same project. Empty string = none picked yet.
  workProject: string

  setSessionId: (id: string) => void
  loadHistory: (msgs: Message[]) => void
  addMessage: (msg: Message) => void
  setPendingPermission: (req: PermissionRequest | null) => void
  setConnected: (v: boolean) => void
  setConnection: (patch: Partial<Connection>) => void
  handleEnvelope: (env: Envelope) => void
  select: (sel: { wiId?: string; file?: SelectedFile } | null) => void
  setWorkProject: (p: string) => void
}

export const useStore = create<AppState>((set) => ({
  sessionId: null,
  messages: [],
  pendingPermission: null,
  connected: false,
  streaming: false,
  streamingMsgId: null,
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
    return { messages: reduced, streamingMsgId: null, streaming: false }
  }),
  addMessage: (msg) => set((s) => ({ messages: [...s.messages, msg] })),
  setPendingPermission: (req) => set({ pendingPermission: req }),
  setConnected: (v) => v
    ? set({ connected: true, connection: { state: 'live', latency: 0, attempt: 0 } })
    : set({ connected: false, streaming: false, streamingMsgId: null, connection: { state: 'dropped', latency: 0, attempt: 0 } }),
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
            set({ streaming: true })
            break
          }
          break
        }
        if (typeof p !== 'string') break
        set((s) => {
          if (s.streamingMsgId) {
            // Append chunk to the current streaming bubble
            return {
              streaming: true,
              messages: s.messages.map(m =>
                m.id === s.streamingMsgId
                  ? { ...m, text: m.text + p }
                  : m
              ),
            }
          }
          // First chunk — create new bubble
          const id = crypto.randomUUID()
          return {
            streaming: true,
            streamingMsgId: id,
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
        set({ streaming: false, streamingMsgId: null })
        break
      case 'error':
        // Clear the thinking/streaming indicator on a daemon-surfaced error so
        // the UI doesn't get stuck showing "Claude is thinking…" forever.
        set({ streaming: false, streamingMsgId: null })
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
      case 'permission':
        set({ pendingPermission: env.payload as PermissionRequest })
        break
      default:
        break
    }
  },

  select: (sel) => {
    if (!sel || sel.wiId === undefined && sel.file === undefined) {
      set({ selectedWiId: null, selectedFile: null })
      return
    }
    if (sel.wiId !== undefined) {
      set({ selectedWiId: sel.wiId, selectedFile: null })
      return
    }
    set({ selectedWiId: null, selectedFile: sel.file ?? null })
  },

  setWorkProject: (p) => set({ workProject: p }),
}))
