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

interface AppState {
  sessionId: string | null
  messages: Message[]
  pendingPermission: PermissionRequest | null
  connected: boolean
  streaming: boolean
  connection: Connection
  streamingMsgId: string | null   // id of the in-progress assistant bubble

  setSessionId: (id: string) => void
  loadHistory: (msgs: Message[]) => void
  addMessage: (msg: Message) => void
  setPendingPermission: (req: PermissionRequest | null) => void
  setConnected: (v: boolean) => void
  setConnection: (patch: Partial<Connection>) => void
  handleEnvelope: (env: Envelope) => void
}

export const useStore = create<AppState>((set) => ({
  sessionId: null,
  messages: [],
  pendingPermission: null,
  connected: false,
  streaming: false,
  streamingMsgId: null,
  connection: { state: 'connecting', latency: 0, attempt: 0 },

  setSessionId: (id) => {
    localStorage.setItem('tether_last_sid', id)
    set({ sessionId: id })
  },
  loadHistory: (msgs) => set({ messages: msgs, streamingMsgId: null, streaming: false }),
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
      case 'fenced': {
        // D-19 fenced block: append as its own assistant message carrying the
        // block. This ends any in-progress text stream so the block renders as
        // a discrete bubble (mirrors how permission requests interrupt text).
        const fb = env.payload as FencedBlock | undefined
        if (!fb || typeof fb.kind !== 'string') break
        set((s) => ({
          streamingMsgId: null,
          messages: [...s.messages, {
            id: crypto.randomUUID(),
            role: 'assistant' as const,
            text: '',
            ts: Date.now(),
            block: fb,
          }],
        }))
        break
      }
      case 'permission':
        set({ pendingPermission: env.payload as PermissionRequest })
        break
      default:
        break
    }
  },
}))
