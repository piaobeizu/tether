import { create } from 'zustand'
import type { Envelope } from './wire.gen'

export interface Message {
  id: string
  role: 'user' | 'assistant' | 'system'
  text: string
  ts: number
}

export interface PermissionRequest {
  id: string
  toolName: string
  input: unknown
}

interface AppState {
  sessionId: string | null
  messages: Message[]
  pendingPermission: PermissionRequest | null
  connected: boolean
  /** True while the daemon is mid-turn (text arrived but result not yet received).
   *  Drives the "…" typing indicator in the chat pane. */
  streaming: boolean

  setSessionId: (id: string) => void
  addMessage: (msg: Message) => void
  setPendingPermission: (req: PermissionRequest | null) => void
  setConnected: (v: boolean) => void
  handleEnvelope: (env: Envelope) => void
}

export const useStore = create<AppState>((set) => ({
  sessionId: null,
  messages: [],
  pendingPermission: null,
  connected: false,
  streaming: false,

  setSessionId: (id) => set({ sessionId: id }),
  addMessage: (msg) => set((s) => ({ messages: [...s.messages, msg] })),
  setPendingPermission: (req) => set({ pendingPermission: req }),
  setConnected: (v) => v ? set({ connected: true }) : set({ connected: false, streaming: false }),

  handleEnvelope: (env) => {
    switch (env.kind) {
      case 'message': {
        const p = env.payload
        if (p && typeof p === 'object' && (p as Record<string, unknown>)['type'] === 'session_ready') {
          set({ sessionId: (p as Record<string, unknown>)['sessionId'] as string })
          break
        }
        // Only stream text chunks to chat. Structured payloads (tool_use, tool_result)
        // will be surfaced in the DAG/fenced-block pane (F.1). Coercing them to JSON
        // strings here produces unreadable noise in the chat bubble.
        if (typeof p !== 'string') break
        set((s) => ({
          streaming: true,
          messages: [...s.messages, {
            id: crypto.randomUUID(),
            role: 'assistant',
            text: p,
            ts: Date.now(),
          }],
        }))
        break
      }
      case 'result':
        // Turn complete — clear the streaming indicator.
        set({ streaming: false })
        break
      case 'permission':
        set({ pendingPermission: env.payload as PermissionRequest })
        break
      default:
        break
    }
  },
}))
