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

  setSessionId: (id) => set({ sessionId: id }),
  addMessage: (msg) => set((s) => ({ messages: [...s.messages, msg] })),
  setPendingPermission: (req) => set({ pendingPermission: req }),
  setConnected: (v) => set({ connected: v }),

  handleEnvelope: (env) => {
    switch (env.kind) {
      case 'message': {
        const p = env.payload as Record<string, unknown> | string
        if (p && typeof p === 'object' && p['type'] === 'session_ready') {
          set({ sessionId: p['sessionId'] as string })
          break
        }
        set((s) => ({
          messages: [...s.messages, {
            id: crypto.randomUUID(),
            role: 'assistant',
            text: typeof env.payload === 'string' ? env.payload : JSON.stringify(env.payload),
            ts: Date.now(),
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
