import { describe, it, expect, beforeEach } from 'vitest'
import { useStore } from '../src/lib/store'
import type { Envelope } from '../src/lib/wire.gen'

const reset = () => useStore.setState({ messages: [], sessionId: null, streaming: false, connected: false })

describe('useStore.handleEnvelope', () => {
  beforeEach(reset)

  it('text string payload appends an assistant message and sets streaming=true', () => {
    const env: Envelope = { kind: 'message', payload: 'Hello there' }
    useStore.getState().handleEnvelope(env)
    const { messages, streaming } = useStore.getState()
    expect(messages).toHaveLength(1)
    expect(messages[0]?.role).toBe('assistant')
    expect(messages[0]?.text).toBe('Hello there')
    expect(streaming).toBe(true)
  })

  it('result envelope clears streaming', () => {
    useStore.setState({ streaming: true })
    const env: Envelope = { kind: 'result', payload: 'end_turn' }
    useStore.getState().handleEnvelope(env)
    expect(useStore.getState().streaming).toBe(false)
  })

  it('session_ready payload sets sessionId without adding a message', () => {
    const env: Envelope = {
      kind: 'message',
      payload: { type: 'session_ready', sessionId: 'sess-abc' },
    }
    useStore.getState().handleEnvelope(env)
    expect(useStore.getState().sessionId).toBe('sess-abc')
    expect(useStore.getState().messages).toHaveLength(0)
  })

  it('tool_use object payload sets streaming=true but adds no chat message', () => {
    const env: Envelope = {
      kind: 'message',
      payload: { type: 'tool_use', id: 'toolu_01abc', name: 'Read', input: { file_path: '/tmp/x' } },
    }
    useStore.getState().handleEnvelope(env)
    expect(useStore.getState().messages).toHaveLength(0)
    // streaming stays on: Claude is mid-turn executing a tool.
    expect(useStore.getState().streaming).toBe(true)
  })

  it('unknown structured payload is ignored without changing state', () => {
    const env: Envelope = {
      kind: 'message',
      payload: { type: 'future_kind', data: 42 },
    }
    useStore.getState().handleEnvelope(env)
    expect(useStore.getState().messages).toHaveLength(0)
    expect(useStore.getState().streaming).toBe(false)
  })

  it('setConnected(false) resets streaming', () => {
    useStore.setState({ connected: true, streaming: true })
    useStore.getState().setConnected(false)
    expect(useStore.getState().streaming).toBe(false)
    expect(useStore.getState().connected).toBe(false)
  })

  it('setConnected(true) does not reset streaming mid-turn', () => {
    useStore.setState({ connected: false, streaming: false })
    useStore.getState().setConnected(true)
    expect(useStore.getState().connected).toBe(true)
    expect(useStore.getState().streaming).toBe(false) // was already false
  })

  it('permission envelope sets pendingPermission', () => {
    const env: Envelope = {
      kind: 'permission',
      payload: { id: 'req-1', toolName: 'Bash', input: { command: 'ls' } },
    }
    useStore.getState().handleEnvelope(env)
    expect(useStore.getState().pendingPermission?.id).toBe('req-1')
  })
})
