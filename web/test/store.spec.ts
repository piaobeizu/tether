import { describe, it, expect, beforeEach } from 'vitest'
import { useStore, historyEntryToMessage, type HistoryEntry } from '../src/lib/store'
import type { Envelope } from '../src/lib/wire.gen'

const reset = () => useStore.setState({ messages: [], sessionId: null, streaming: false, streamingMsgId: null, curTurnId: null, thinkingStartTs: null, connected: false })

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

  it('error envelope clears streaming (no stuck "thinking" indicator)', () => {
    useStore.setState({ streaming: true, streamingMsgId: null })
    useStore.getState().handleEnvelope({ kind: 'error', payload: 'boom' })
    expect(useStore.getState().streaming).toBe(false)
    expect(useStore.getState().streamingMsgId).toBeNull()
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

  it('tool_use object payload opens an assistant bubble carrying the tool call (tether#37)', () => {
    const env: Envelope = {
      kind: 'message',
      payload: { type: 'tool_use', id: 'toolu_01abc', name: 'Read', input: { file_path: '/tmp/x' } },
    }
    useStore.getState().handleEnvelope(env)
    const { messages, streaming } = useStore.getState()
    // tether#37: the tool call is now KEPT on the turn's bubble (previously this
    // branch discarded name/input and added no message — the visibility gap).
    expect(messages).toHaveLength(1)
    expect(messages[0]?.role).toBe('assistant')
    expect(messages[0]?.tools).toHaveLength(1)
    expect(messages[0]?.tools?.[0]).toMatchObject({ name: 'Read', id: 'toolu_01abc' })
    // streaming stays on: Claude is mid-turn executing a tool.
    expect(streaming).toBe(true)
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

  it('permission envelope appends to the pending queue (tether#40)', () => {
    useStore.setState({ pendingPermissions: [] })
    const env: Envelope = {
      kind: 'permission',
      payload: { id: 'req-1', toolName: 'Bash', input: { command: 'ls' } },
    }
    useStore.getState().handleEnvelope(env)
    const q = useStore.getState().pendingPermissions
    expect(q).toHaveLength(1)
    expect(q[0].id).toBe('req-1')
  })
})

// tether#8 T7 — page-reload persistence: GET /messages now returns fenced
// blocks alongside text, and loadHistory must reconstruct them with the
// same `block` shape the live 'fenced' envelope path produces above, so
// DagBlock renders identically after a reload.
describe('historyEntryToMessage / loadHistory (tether#8 T7 block persistence)', () => {
  beforeEach(reset)

  it('reconstructs a block-message from a /messages payload containing a block', () => {
    const payload: HistoryEntry[] = [
      { role: 'user', text: 'build a plan', ts: 1 },
      { role: 'assistant', text: 'before text\n', ts: 2 },
      {
        role: 'assistant',
        text: '',
        ts: 3,
        block: { kind: 'dag', skill: 's', content: '{"x":1}', blockId: 's-0' },
      },
      { role: 'assistant', text: 'after text', ts: 4 },
    ]

    useStore.getState().loadHistory(payload.map(historyEntryToMessage))
    const { messages } = useStore.getState()

    expect(messages).toHaveLength(4)
    expect(messages[0]?.role).toBe('user')
    expect(messages[0]?.block).toBeUndefined()
    expect(messages[1]?.text).toBe('before text\n')
    expect(messages[1]?.block).toBeUndefined()

    // The block entry: same shape DagBlock consumes from a live 'fenced'
    // envelope (see the 'fenced' case in handleEnvelope above).
    expect(messages[2]?.block).toEqual({
      kind: 'dag', skill: 's', content: '{"x":1}', blockId: 's-0',
    })
    expect(messages[2]?.text).toBe('')

    expect(messages[3]?.text).toBe('after text')
    expect(messages[3]?.block).toBeUndefined()
  })

  it('a text-only payload is unchanged (regression)', () => {
    const payload: HistoryEntry[] = [
      { role: 'user', text: 'hello', ts: 1 },
      { role: 'assistant', text: 'hi there', ts: 2 },
    ]

    useStore.getState().loadHistory(payload.map(historyEntryToMessage))
    const { messages } = useStore.getState()

    expect(messages).toHaveLength(2)
    expect(messages[0]).toMatchObject({ role: 'user', text: 'hello', ts: 1 })
    expect(messages[0]?.block).toBeUndefined()
    expect(messages[1]).toMatchObject({ role: 'assistant', text: 'hi there', ts: 2 })
    expect(messages[1]?.block).toBeUndefined()
  })
})

// tether#8 T8 — live DAG-card updates: a re-emitted 'fenced' envelope with
// the SAME BlockID must update the existing block message in place (so the
// card animates progress), not append a duplicate; a different BlockID
// appends a new card. A NEW block append closes the in-progress text bubble
// (streamingMsgId reset) so the card renders in stream order and the live
// view matches what the reload path reconstructs from history.
describe('handleEnvelope fenced — live replace-by-BlockID (tether#8 T8)', () => {
  beforeEach(reset)

  const dagEnv = (blockId: string, content: string): Envelope => ({
    kind: 'fenced',
    payload: { kind: 'dag', skill: 's', content, blockId },
  })

  it('same BlockID re-emitted updates the existing block message in place (no new bubble)', () => {
    useStore.getState().handleEnvelope(dagEnv('s-0', '{"nodes":[{"id":"a","status":"running"}]}'))
    const idAfterFirst = useStore.getState().messages[0]?.id
    expect(idAfterFirst).toBeTruthy()

    useStore.getState().handleEnvelope(dagEnv('s-0', '{"nodes":[{"id":"a","status":"done"}]}'))
    const { messages } = useStore.getState()

    expect(messages).toHaveLength(1)
    expect(messages[0]?.id).toBe(idAfterFirst) // same message id -> expand state survives
    expect(messages[0]?.block?.content).toBe('{"nodes":[{"id":"a","status":"done"}]}')
  })

  it('a different BlockID appends a second, separate block message', () => {
    useStore.getState().handleEnvelope(dagEnv('s-0', '{}'))
    useStore.getState().handleEnvelope(dagEnv('s-1', '{}'))
    const { messages } = useStore.getState()

    expect(messages).toHaveLength(2)
    expect(messages[0]?.block?.blockId).toBe('s-0')
    expect(messages[1]?.block?.blockId).toBe('s-1')
  })

  it('a NEW block append closes the text bubble; following text is a fresh bubble after the card (stream order)', () => {
    // First text chunk opens a streaming assistant bubble.
    useStore.getState().handleEnvelope({ kind: 'message', payload: 'hello ' })
    const textId = useStore.getState().streamingMsgId
    expect(textId).toBeTruthy()

    // A NEW DAG block appends a card AND resets streamingMsgId, so the card
    // renders in stream position and following text starts a separate bubble.
    useStore.getState().handleEnvelope(dagEnv('s-0', '{"v":1}'))
    expect(useStore.getState().streamingMsgId).toBeNull()

    // Following text delta -> a NEW bubble after the card.
    useStore.getState().handleEnvelope({ kind: 'message', payload: 'world' })
    const afterId = useStore.getState().streamingMsgId
    expect(afterId).toBeTruthy()
    expect(afterId).not.toBe(textId)

    // Re-emit same BlockID -> updates the card in place, streaming untouched.
    useStore.getState().handleEnvelope(dagEnv('s-0', '{"v":2}'))
    expect(useStore.getState().streamingMsgId).toBe(afterId)

    const { messages } = useStore.getState()
    expect(messages).toHaveLength(3) // [before-text, card, after-text]
    expect(messages[0]?.id).toBe(textId)
    expect(messages[0]?.text).toBe('hello ')
    expect(messages[1]?.block?.blockId).toBe('s-0')
    expect(messages[1]?.block?.content).toBe('{"v":2}')
    expect(messages[2]?.id).toBe(afterId)
    expect(messages[2]?.text).toBe('world')
  })

  // Integration invariant (the DAG-lane final review flagged this was untested):
  // the LIVE path (handleEnvelope) and the RELOAD path (loadHistory) must render
  // identical messages — same order, same collapsed cards — for the same stream.
  it('live handleEnvelope and reload loadHistory render identical messages', () => {
    const shape = () => useStore.getState().messages.map((m) => ({
      role: m.role, text: m.text,
      block: m.block?.blockId ?? null, content: m.block?.content ?? null,
    }))

    // Live: before-text, block(v1), after-text, re-emit block(v2).
    useStore.getState().handleEnvelope({ kind: 'message', payload: 'before\n' })
    useStore.getState().handleEnvelope(dagEnv('d0', '{"v":1}'))
    useStore.getState().handleEnvelope({ kind: 'message', payload: 'after\n' })
    useStore.getState().handleEnvelope(dagEnv('d0', '{"v":2}'))
    const live = shape()

    // Reload: history persists the block twice (v1, v2); loadHistory must
    // collapse to one card (last content, first position), matching live.
    reset()
    const history: HistoryEntry[] = [
      { role: 'assistant', text: 'before\n', ts: 1 },
      { role: 'assistant', text: '', ts: 2, block: { kind: 'dag', skill: 's', content: '{"v":1}', blockId: 'd0' } },
      { role: 'assistant', text: 'after\n', ts: 3 },
      { role: 'assistant', text: '', ts: 4, block: { kind: 'dag', skill: 's', content: '{"v":2}', blockId: 'd0' } },
    ]
    useStore.getState().loadHistory(history.map(historyEntryToMessage))
    const reloaded = shape()

    expect(reloaded).toEqual(live)
    expect(live).toHaveLength(3)          // [before, card, after]
    expect(live[1]?.block).toBe('d0')
    expect(live[1]?.content).toBe('{"v":2}') // last content wins
  })
})

// tether#28 — Work selection slice: the middle file view (selectedFile) and
// the right Work wi drawer (selectedWiId) are now INDEPENDENT (they were
// mutually exclusive through tether#27). select() touches only the field(s)
// passed, so a file and a wi drawer can be open at once.
describe('useStore.select (tether#28 independent selection)', () => {
  const resetSelection = () => useStore.setState({ selectedWiId: null, selectedFile: null })
  beforeEach(resetSelection)

  it('selecting a wi leaves the file selection intact (coexist)', () => {
    useStore.setState({ selectedFile: { wsId: 'ws-1', path: 'a.txt' } })
    useStore.getState().select({ wiId: 'wi-1' })
    expect(useStore.getState().selectedWiId).toBe('wi-1')
    expect(useStore.getState().selectedFile).toEqual({ wsId: 'ws-1', path: 'a.txt' })
  })

  it('selecting a file leaves the wi selection intact (coexist)', () => {
    useStore.setState({ selectedWiId: 'wi-1' })
    useStore.getState().select({ file: { wsId: 'ws-1', path: 'a.txt' } })
    expect(useStore.getState().selectedFile).toEqual({ wsId: 'ws-1', path: 'a.txt' })
    expect(useStore.getState().selectedWiId).toBe('wi-1')
  })

  it('select({ wiId: null }) clears only the wi (drawer close), leaving the file', () => {
    useStore.setState({ selectedFile: { wsId: 'ws-1', path: 'a.txt' }, selectedWiId: 'wi-1' })
    useStore.getState().select({ wiId: null })
    expect(useStore.getState().selectedWiId).toBeNull()
    expect(useStore.getState().selectedFile).toEqual({ wsId: 'ws-1', path: 'a.txt' })
  })

  it('select(null) clears both (project reset)', () => {
    useStore.setState({ selectedWiId: 'wi-1', selectedFile: { wsId: 'ws-1', path: 'a.txt' } })
    useStore.getState().select(null)
    expect(useStore.getState().selectedWiId).toBeNull()
    expect(useStore.getState().selectedFile).toBeNull()
  })
})
