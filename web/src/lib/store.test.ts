import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  useStore, mergeTranscript, parseErrorPayload, rememberedWorkspaceId, WORKSPACE_ID_KEY,
  type Message, type Notice,
} from './store'
import type { Envelope } from './wire.gen'

// tether#34 — extended-thinking accumulation in the chat store. These drive
// handleEnvelope directly (no WebTransport) and assert the ONE-bubble-per-turn
// message model the ThinkingBlock renderer consumes.

const thinkingEnv = (text: string): Envelope => ({ kind: 'message', payload: { type: 'thinking', text } })
const textEnv = (text: string): Envelope => ({ kind: 'message', payload: text })
const resultEnv = (): Envelope => ({ kind: 'result', payload: 'stop' })
const fencedEnv = (blkKind: string): Envelope => ({ kind: 'fenced', payload: { kind: blkKind } } as unknown as Envelope)

function reset() {
  useStore.setState({ messages: [], notices: [], streaming: false, streamingMsgId: null, curTurnId: null, thinkingStartTs: null, answerStartTs: null, stopped: false, pendingPermissions: [] })
}

describe('store thinking accumulation (tether#34)', () => {
  afterEach(reset)

  it('accumulates thinking deltas into a new assistant bubble', () => {
    const h = useStore.getState().handleEnvelope
    h(thinkingEnv('let me '))
    h(thinkingEnv('think'))
    const s = useStore.getState()
    expect(s.messages).toHaveLength(1)
    expect(s.messages[0].role).toBe('assistant')
    expect(s.messages[0].thinking).toBe('let me think')
    expect(s.messages[0].text).toBe('')
    expect(s.messages[0].thinkingMs).toBeUndefined()
    expect(s.streaming).toBe(true)
    expect(s.curTurnId).toBe(s.messages[0].id)
  })

  it('answer text continues in the same bubble and stamps thinkingMs on the first delta', () => {
    const h = useStore.getState().handleEnvelope
    h(thinkingEnv('pondering'))
    h(textEnv('the '))
    h(textEnv('answer'))
    const s = useStore.getState()
    expect(s.messages).toHaveLength(1) // same bubble, not a second one
    expect(s.messages[0].thinking).toBe('pondering')
    expect(s.messages[0].text).toBe('the answer')
    expect(typeof s.messages[0].thinkingMs).toBe('number')
    expect(s.messages[0].thinkingMs).toBeGreaterThanOrEqual(0)
    expect(s.streamingMsgId).toBe(s.messages[0].id)
    expect(s.curTurnId).toBe(s.messages[0].id) // turn still open until result
  })

  it('interleaved thinking mid-answer stays ONE bubble with continuous text (owner live-verify fix)', () => {
    const h = useStore.getState().handleEnvelope
    h(thinkingEnv('plan A'))
    h(textEnv('doing A'))
    h(thinkingEnv('plan B')) // a second thinking block mid-turn
    h(textEnv(' doing B'))
    const s = useStore.getState()
    expect(s.messages).toHaveLength(1) // NOT split into two bubbles
    expect(s.messages[0].text).toBe('doing A doing B') // answer stays continuous
    expect(s.messages[0].thinking).toBe('plan Aplan B') // thinking merged into one block
    expect(typeof s.messages[0].thinkingMs).toBe('number') // stamped at first answer delta
  })

  it('a turn with no thinking creates a plain answer bubble (no regression)', () => {
    const h = useStore.getState().handleEnvelope
    h(textEnv('hi '))
    h(textEnv('there'))
    const s = useStore.getState()
    expect(s.messages).toHaveLength(1)
    expect(s.messages[0].text).toBe('hi there')
    expect(s.messages[0].thinking).toBeUndefined()
    expect(s.messages[0].thinkingMs).toBeUndefined()
    expect(s.curTurnId).toBe(s.messages[0].id)
  })

  it('result resets the turn pointers; the next turn starts a fresh bubble', () => {
    const h = useStore.getState().handleEnvelope
    h(thinkingEnv('x'))
    h(textEnv('y'))
    h(resultEnv())
    const s = useStore.getState()
    expect(s.streaming).toBe(false)
    expect(s.streamingMsgId).toBeNull()
    expect(s.curTurnId).toBeNull()
    expect(s.thinkingStartTs).toBeNull()
    h(thinkingEnv('next'))
    const s2 = useStore.getState()
    expect(s2.messages).toHaveLength(2)
    expect(s2.messages[1].thinking).toBe('next')
  })

  it('an empty thinking delta creates no bubble but still marks streaming', () => {
    const h = useStore.getState().handleEnvelope
    h(thinkingEnv(''))
    const s = useStore.getState()
    expect(s.messages).toHaveLength(0)
    expect(s.streaming).toBe(true)
    expect(s.curTurnId).toBeNull()
  })
})

// tether#36 — answer duration badge: answerMs = first answer delta -> result,
// stamped on the turn's bubble at result (mirrors thinkingMs). Frontend-timed.
describe('store answer duration (tether#36)', () => {
  afterEach(reset)

  it('stamps answerMs on the bubble at result (thinking then answer)', () => {
    const h = useStore.getState().handleEnvelope
    h(thinkingEnv('ponder'))
    h(textEnv('the answer'))
    expect(useStore.getState().messages[0].answerMs).toBeUndefined() // not until result
    h(resultEnv())
    const s = useStore.getState()
    expect(s.messages).toHaveLength(1)
    expect(typeof s.messages[0].answerMs).toBe('number')
    expect(s.messages[0].answerMs).toBeGreaterThanOrEqual(0)
    expect(s.answerStartTs).toBeNull()
  })

  it('stamps answerMs for a no-thinking answer too', () => {
    const h = useStore.getState().handleEnvelope
    h(textEnv('hi'))
    h(resultEnv())
    expect(typeof useStore.getState().messages[0].answerMs).toBe('number')
  })

  it('does NOT stamp answerMs for a thinking-only turn (no answer text)', () => {
    const h = useStore.getState().handleEnvelope
    h(thinkingEnv('just thinking'))
    h(resultEnv())
    const s = useStore.getState()
    expect(s.messages).toHaveLength(1)
    expect(s.messages[0].answerMs).toBeUndefined()
  })

  it('measures only the final answer segment across an intra-turn fenced block (MAJOR leak regression)', () => {
    vi.useFakeTimers()
    try {
      const h = useStore.getState().handleEnvelope
      h(textEnv('plan'))            // answer starts (answerStartTs = T0)
      vi.advanceTimersByTime(5000)  // 5s on card + thinking
      h(fencedEnv('dag'))           // fenced block resets curTurnId AND answerStartTs
      h(thinkingEnv('hmm'))         // new thinking bubble (does not set answerStartTs)
      vi.advanceTimersByTime(1000)
      h(textEnv('done'))            // final answer segment starts here (answerStartTs = now)
      vi.advanceTimersByTime(2000)  // 2s answer
      h(resultEnv())
      const last = useStore.getState().messages.at(-1)!
      expect(last.text).toBe('done')
      expect(last.answerMs).toBe(2000) // only the final segment, NOT 8000 (leaked total)
    } finally {
      vi.useRealTimers()
    }
  })

  it('does NOT stamp answerMs on a thinking-only bubble that follows a fenced block', () => {
    const h = useStore.getState().handleEnvelope
    h(textEnv('a'))
    h(fencedEnv('dag'))
    h(thinkingEnv('hmm'))  // last bubble has thinking but no answer text
    h(resultEnv())
    const last = useStore.getState().messages.at(-1)!
    expect(last.thinking).toBe('hmm')
    expect(last.text).toBe('')
    expect(last.answerMs).toBeUndefined()
  })
})

// tether#37 — tool-call visibility: the daemon already puts {name,input} on the
// wire (registry.go translateEvent); the store now KEEPS them on the turn's
// bubble instead of discarding them. These assert accumulation AND that a tool
// call never pollutes the answer/thinking clocks (would break the #36 badge).
const toolEnv = (name: string, input?: unknown, id = ''): Envelope =>
  ({ kind: 'message', payload: { type: 'tool_use', id, name, input } } as unknown as Envelope)

describe('store tool-call visibility (tether#37)', () => {
  afterEach(reset)

  it('appends tool calls to the current turn bubble after thinking', () => {
    const h = useStore.getState().handleEnvelope
    h(thinkingEnv('reading files'))
    h(toolEnv('Read', { file_path: 'a.ts' }, 't1'))
    h(toolEnv('Bash', { command: 'go test' }, 't2'))
    const s = useStore.getState()
    expect(s.messages).toHaveLength(1)
    expect(s.messages[0].tools).toHaveLength(2)
    expect(s.messages[0].tools![0]).toMatchObject({ name: 'Read', id: 't1' })
    expect(s.messages[0].tools![1].name).toBe('Bash')
    expect(s.streaming).toBe(true)
  })

  it('a tool call arriving first opens the bubble WITHOUT starting the answer/thinking clocks', () => {
    const h = useStore.getState().handleEnvelope
    h(toolEnv('Read', { file_path: 'a.ts' }))
    const s = useStore.getState()
    expect(s.messages).toHaveLength(1)
    expect(s.messages[0].tools).toHaveLength(1)
    expect(s.messages[0].text).toBe('')
    expect(s.curTurnId).toBe(s.messages[0].id)
    expect(s.answerStartTs).toBeNull()   // tool != answer
    expect(s.thinkingStartTs).toBeNull() // tool != thinking
    expect(s.streamingMsgId).toBeNull()  // cursor only follows answer text
  })

  it('a tools-only turn (tool then result, no answer text) gets NO answer badge', () => {
    const h = useStore.getState().handleEnvelope
    h(toolEnv('Read', { file_path: 'a.ts' }))
    h(resultEnv())
    const s = useStore.getState()
    expect(s.messages).toHaveLength(1)
    expect(s.messages[0].tools).toHaveLength(1)
    expect(s.messages[0].answerMs).toBeUndefined()
  })

  it('tool call then answer text: same bubble keeps tools AND gets an answer badge', () => {
    const h = useStore.getState().handleEnvelope
    h(toolEnv('Read', { file_path: 'a.ts' }))
    h(textEnv('done reading'))
    h(resultEnv())
    const s = useStore.getState()
    expect(s.messages).toHaveLength(1)
    expect(s.messages[0].tools).toHaveLength(1)
    expect(s.messages[0].text).toBe('done reading')
    expect(typeof s.messages[0].answerMs).toBe('number')
  })

  it('a tool_use with no name marks streaming but creates no bubble', () => {
    const h = useStore.getState().handleEnvelope
    h(toolEnv(''))
    const s = useStore.getState()
    expect(s.messages).toHaveLength(0)
    expect(s.streaming).toBe(true)
    expect(s.curTurnId).toBeNull()
  })

  it('a tool call BEFORE thinking still stamps thinkingMs (review MINOR: #34 badge regression)', () => {
    // tool_use opens the bubble first, so thinking takes the append path — which
    // must still start the thinking clock, else the "thought Xs" badge never measures.
    const h = useStore.getState().handleEnvelope
    h(toolEnv('Read', { file_path: 'a.ts' }))
    h(thinkingEnv('now pondering'))
    h(textEnv('answer'))
    const s = useStore.getState()
    expect(s.messages).toHaveLength(1)
    expect(s.messages[0].tools).toHaveLength(1)
    expect(s.messages[0].thinking).toBe('now pondering')
    expect(typeof s.messages[0].thinkingMs).toBe('number')
    expect(s.messages[0].thinkingMs).toBeGreaterThanOrEqual(0)
  })
})

// tether#38 — tool-result inlining: the daemon now forwards tool_result keyed by
// tool_use_id; the store hangs it on the matching ToolCall (from an earlier
// tool_use). These assert match-by-id + that a result never opens a bubble or
// touches the turn clocks/cursor.
const toolResultEnv = (toolUseId: string, content: string, isError = false): Envelope =>
  ({ kind: 'message', payload: { type: 'tool_result', tool_use_id: toolUseId, content, is_error: isError } } as unknown as Envelope)

describe('store tool-result inlining (tether#38)', () => {
  afterEach(reset)

  it('hangs the result on the matching ToolCall by tool_use_id', () => {
    const h = useStore.getState().handleEnvelope
    h(toolEnv('Read', { file_path: 'a.ts' }, 'tu1'))
    h(toolEnv('Bash', { command: 'go test' }, 'tu2'))
    h(toolResultEnv('tu2', 'ok\nPASS'))
    const s = useStore.getState()
    expect(s.messages[0].tools).toHaveLength(2)
    expect(s.messages[0].tools![0].result).toBeUndefined() // tu1 has no result yet
    expect(s.messages[0].tools![1].result).toEqual({ content: 'ok\nPASS', isError: false })
  })

  it('carries is_error through', () => {
    const h = useStore.getState().handleEnvelope
    h(toolEnv('Bash', { command: 'false' }, 'tu9'))
    h(toolResultEnv('tu9', 'boom', true))
    expect(useStore.getState().messages[0].tools![0].result).toEqual({ content: 'boom', isError: true })
  })

  it('a tool_result with no matching id leaves messages unchanged (no crash)', () => {
    const h = useStore.getState().handleEnvelope
    h(toolEnv('Read', { file_path: 'a.ts' }, 'tu1'))
    h(toolResultEnv('nonexistent', 'orphan result'))
    const s = useStore.getState()
    expect(s.messages).toHaveLength(1)
    expect(s.messages[0].tools![0].result).toBeUndefined()
    expect(s.streaming).toBe(true)
  })

  it('a tool_result does NOT open a bubble or touch the turn clocks', () => {
    const h = useStore.getState().handleEnvelope
    h(toolResultEnv('tuX', 'x')) // result with no prior tool_use (edge)
    const s = useStore.getState()
    expect(s.messages).toHaveLength(0)
    expect(s.curTurnId).toBeNull()
    expect(s.answerStartTs).toBeNull()
    expect(s.thinkingStartTs).toBeNull()
    expect(s.streamingMsgId).toBeNull()
  })
})

// tether#40 — parallel permission requests queue. The daemon sends ONE
// KindPermission per parallel tool; the store now APPENDS each to a queue
// (dedup by id) instead of overwriting a single slot, so every request stays
// approvable (the old single-slot pendingPermission let a later request clobber
// an earlier one → all-but-one timed out). resolvePermission(id) drops one after
// it's decided; loadHistory (a session reset) clears the queue, but turn
// boundaries do NOT — a permission's lifecycle is decide-driven, not turn-driven.
const permEnv = (id: string, toolName = 'Read', input: unknown = {}): Envelope =>
  ({ kind: 'permission', payload: { id, toolName, input } } as unknown as Envelope)

describe('store permission queue (tether#40)', () => {
  afterEach(reset)

  it('appends parallel permission requests instead of overwriting (the bug)', () => {
    const h = useStore.getState().handleEnvelope
    h(permEnv('r1', 'Read', { file_path: 'go.mod' }))
    h(permEnv('r2', 'Read', { file_path: 'README.md' }))
    h(permEnv('r3', 'Grep', { pattern: 'X' }))
    const q = useStore.getState().pendingPermissions
    expect(q).toHaveLength(3)
    expect(q.map((p) => p.id)).toEqual(['r1', 'r2', 'r3']) // arrival order preserved
    expect(q[2]).toMatchObject({ toolName: 'Grep' })
  })

  it('dedups a re-emitted request id (no duplicate block)', () => {
    const h = useStore.getState().handleEnvelope
    h(permEnv('r1'))
    h(permEnv('r1')) // same id again
    expect(useStore.getState().pendingPermissions).toHaveLength(1)
  })

  it('resolvePermission removes only that id, leaving the rest', () => {
    const h = useStore.getState().handleEnvelope
    h(permEnv('r1')); h(permEnv('r2')); h(permEnv('r3'))
    useStore.getState().resolvePermission('r2')
    expect(useStore.getState().pendingPermissions.map((p) => p.id)).toEqual(['r1', 'r3'])
  })

  it('resolvePermission on an unknown id is a no-op (no crash)', () => {
    const h = useStore.getState().handleEnvelope
    h(permEnv('r1'))
    useStore.getState().resolvePermission('nope')
    expect(useStore.getState().pendingPermissions).toHaveLength(1)
  })

  it('a turn boundary (result) does NOT clear pending permissions (decide-driven)', () => {
    const h = useStore.getState().handleEnvelope
    h(permEnv('r1'))
    h(resultEnv())
    expect(useStore.getState().pendingPermissions).toHaveLength(1)
  })

  it('loadHistory (session reset) clears the queue', () => {
    const h = useStore.getState().handleEnvelope
    h(permEnv('r1')); h(permEnv('r2'))
    useStore.getState().loadHistory([])
    expect(useStore.getState().pendingPermissions).toHaveLength(0)
  })
})

// tether#42 — stopTurn finalizes an interrupted turn locally. cc emits NO
// EventResult after an interrupt, so the store closes the turn itself (mirrors
// the 'result' path): stamp answerMs, keep the partial text, reset turn pointers.
describe('store stopTurn (tether#42)', () => {
  afterEach(reset)

  it('finalizes the current turn: stops streaming, keeps partial text, stamps answerMs', () => {
    const h = useStore.getState().handleEnvelope
    h(textEnv('partial ans'))   // answer started, streaming
    expect(useStore.getState().streaming).toBe(true)
    useStore.getState().stopTurn()
    const s = useStore.getState()
    expect(s.streaming).toBe(false)
    expect(s.streamingMsgId).toBeNull()
    expect(s.curTurnId).toBeNull()
    expect(s.messages).toHaveLength(1)
    expect(s.messages[0].text).toBe('partial ans')       // partial answer preserved
    expect(typeof s.messages[0].answerMs).toBe('number') // stamped like result
  })

  it('is idempotent — a late result after stopTurn does not double-finalize or crash', () => {
    const h = useStore.getState().handleEnvelope
    h(textEnv('x'))
    useStore.getState().stopTurn()
    const before = useStore.getState().messages.length
    h(resultEnv())  // late result: curTurnId already null → no-op
    const s = useStore.getState()
    expect(s.messages).toHaveLength(before)
    expect(s.streaming).toBe(false)
    expect(s.curTurnId).toBeNull()
  })

  it('stopTurn with no active turn is a safe no-op', () => {
    useStore.getState().stopTurn()
    const s = useStore.getState()
    expect(s.messages).toHaveLength(0)
    expect(s.streaming).toBe(false)
  })

  it('drops late deltas after stop — no new bubble, no resumed streaming (owner: output-after-stop)', () => {
    const h = useStore.getState().handleEnvelope
    h(textEnv('partial'))          // streaming turn
    useStore.getState().stopTurn() // user hits Stop
    h(textEnv(' more buffered'))   // cc flushes late buffered deltas after the interrupt
    h(thinkingEnv('late think'))
    h(toolEnv('Read', { file_path: 'x' }, 'late'))
    const s = useStore.getState()
    expect(s.messages).toHaveLength(1)          // NO new bubble spawned
    expect(s.messages[0].text).toBe('partial')  // late text ignored
    expect(s.streaming).toBe(false)             // streaming NOT resumed
  })

  it('a new user turn clears the stopped flag so the next turn streams normally', () => {
    const h = useStore.getState().handleEnvelope
    h(textEnv('a'))
    useStore.getState().stopTurn()
    useStore.getState().addMessage({ id: 'u1', role: 'user', text: 'again', ts: 1 })
    h(textEnv('fresh answer'))
    const s = useStore.getState()
    expect(s.messages.some(m => m.role === 'assistant' && m.text === 'fresh answer')).toBe(true)
    expect(s.streaming).toBe(true)
  })

  it('a terminal result clears the stopped flag', () => {
    const h = useStore.getState().handleEnvelope
    h(textEnv('a'))
    useStore.getState().stopTurn()
    h(resultEnv())
    expect(useStore.getState().stopped).toBe(false)
  })
})

// tether#50 — the daemon's "started a new session" notice. Sent right after
// session_ready when a `cc --resume` failed AND the dead session had history.
const noticeEnv = (text: string): Envelope => ({ kind: 'message', payload: { type: 'notice', text } })

/** What ChatPane actually renders: the two lists recombined (tether#57). */
const rendered = () => {
  const s = useStore.getState()
  return mergeTranscript(s.messages, s.notices)
}

describe('store session notice (tether#50)', () => {
  afterEach(reset)

  it('records the notice as a system line in the rendered transcript', () => {
    const h = useStore.getState().handleEnvelope
    h(noticeEnv('Started a new session — the previous context could not be restored.'))
    // tether#57 — it is kept OUT of `messages` (server truth) …
    expect(useStore.getState().messages).toHaveLength(0)
    expect(useStore.getState().notices).toHaveLength(1)
    // … but is still what the pane renders.
    const t = rendered()
    expect(t).toHaveLength(1)
    expect(t[0].role).toBe('system')
    expect(t[0].text).toBe('Started a new session — the previous context could not be restored.')
  })

  it('does not claim the turn cursor, so the replayed answer gets its own bubble', () => {
    // This is the whole reason the notice is an OBJECT payload rather than a
    // plain string: a string would go through the text-accumulation branch, set
    // curTurnId, and the daemon's immediately-following replayed answer would
    // then append INTO the notice's bubble — the notice and the answer fused
    // into one message.
    const h = useStore.getState().handleEnvelope
    h(noticeEnv('context lost'))
    expect(useStore.getState().curTurnId).toBeNull()
    expect(useStore.getState().streaming).toBe(false)
    expect(useStore.getState().streamingMsgId).toBeNull()

    h(textEnv('here is the real answer'))
    const t = rendered()
    expect(t).toHaveLength(2)
    expect(t[0].role).toBe('system')
    expect(t[0].text).toBe('context lost')
    expect(t[1].role).toBe('assistant')
    expect(t[1].text).toBe('here is the real answer')
  })

  it('ignores a notice with no text', () => {
    const h = useStore.getState().handleEnvelope
    h({ kind: 'message', payload: { type: 'notice' } } as unknown as Envelope)
    expect(useStore.getState().notices).toHaveLength(0)
    expect(rendered()).toHaveLength(0)
  })

  it('is delivered even after a manual stop (session lifecycle, not turn content)', () => {
    // The `stopped` gate drops late turn deltas so they can't spawn a new
    // bubble (tether#42). A notice is not turn content — it explains why the
    // session changed — so it must survive that gate, exactly like
    // session_ready.
    const h = useStore.getState().handleEnvelope
    useStore.setState({ stopped: true })
    h(noticeEnv('context lost'))
    const t = rendered()
    expect(t).toHaveLength(1)
    expect(t[0].role).toBe('system')
  })
})

// tether#57 — the notice must survive the history refetch that session_ready
// itself sets off. Chain: session_ready(newSid) → setSessionId → ChatPane's
// [sessionId] effect → GET /messages → loadHistory, which REPLACES `messages`.
// While the notice lived in that array the refetch could silently eat it, and
// the daemon never persists the notice, so there was no way to get it back.
describe('session notice survives the history refetch (tether#57)', () => {
  afterEach(reset)

  const histMsg = (role: Message['role'], text: string, ts: number): Message =>
    ({ id: `h-${role}-${ts}`, role, text, ts })

  it('is still rendered after loadHistory replaces the whole message list', () => {
    const h = useStore.getState().handleEnvelope
    h(noticeEnv('context lost'))
    // The refetch for the NEW sid resolves in a quiet moment and replaces
    // everything with server truth (the replayed prompt + its answer). A realistic
    // ±1s window around the notice, not absurd 1970/2286 sentinels that would make
    // the ordering assertion below true by construction.
    useStore.getState().loadHistory([
      histMsg('user', 'what did we decide?', Date.now() - 1_000),
      histMsg('assistant', 'starting fresh', Date.now() + 1_000),
    ])
    expect(useStore.getState().messages).toHaveLength(2)
    const t = rendered()
    // THE point of tether#57: the notice outlived the wholesale replace.
    expect(t.filter(m => m.role === 'system').map(m => m.text)).toEqual(['context lost'])
    // Secondary: with clocks agreeing it lands between the prompt and the answer.
    // (Under real cross-machine skew the position can degrade — see mergeTranscript.)
    expect(t.map(m => m.role)).toEqual(['user', 'system', 'assistant'])
  })

  // The two edits most likely to silently restore the bug with a green suite:
  // "tidying up" a session reset by clearing notices in setSessionId (which the
  // resume-fallback path itself calls) or in the disconnect reducer.
  it('setSessionId does NOT clear notices — the fallback path changes the sid too', () => {
    useStore.getState().handleEnvelope(noticeEnv('context lost'))
    useStore.getState().setSessionId('a-brand-new-sid')
    expect(useStore.getState().notices).toHaveLength(1)
    expect(rendered().some(m => m.role === 'system')).toBe(true)
  })

  it('a disconnect does NOT clear notices', () => {
    useStore.getState().handleEnvelope(noticeEnv('context lost'))
    useStore.getState().setConnected(false)
    expect(useStore.getState().notices).toHaveLength(1)
  })

  it('collapses a repeat of the notice already showing (constant text, no pile-up)', () => {
    const h = useStore.getState().handleEnvelope
    h(noticeEnv('context lost'))
    h(noticeEnv('context lost'))
    expect(useStore.getState().notices).toHaveLength(1)
  })

  it('survives repeated refetches (loadHistory can never own the notice list)', () => {
    const h = useStore.getState().handleEnvelope
    h(noticeEnv('context lost'))
    useStore.getState().loadHistory([histMsg('user', 'a', 1)])
    useStore.getState().loadHistory([histMsg('user', 'a', 1), histMsg('assistant', 'b', 2)])
    expect(useStore.getState().notices).toHaveLength(1)
    expect(rendered().some(m => m.role === 'system' && m.text === 'context lost')).toBe(true)
  })

  it('clearNotices retires it on a deliberate session switch', () => {
    const h = useStore.getState().handleEnvelope
    h(noticeEnv('context lost'))
    expect(rendered()).toHaveLength(1)
    useStore.getState().clearNotices()
    expect(useStore.getState().notices).toHaveLength(0)
    expect(rendered()).toHaveLength(0)
  })
})

describe('mergeTranscript (tether#57)', () => {
  const msg = (id: string, ts: number): Message => ({ id, role: 'user', text: id, ts })
  const notice = (id: string, ts: number): Notice => ({ id, text: id, ts })

  it('returns the SAME array reference when there are no notices', () => {
    const msgs = [msg('a', 1), msg('b', 2)]
    expect(mergeTranscript(msgs, [])).toBe(msgs)
  })

  it('inserts a notice before the first message at or after its ts', () => {
    const out = mergeTranscript([msg('a', 10), msg('b', 30)], [notice('n', 20)])
    expect(out.map(m => m.id)).toEqual(['a', 'n', 'b'])
  })

  it('on an exact ts tie the notice goes FIRST (Date.now() is only ms-granular)', () => {
    expect(mergeTranscript([msg('a', 10)], [notice('n', 10)]).map(m => m.id)).toEqual(['n', 'a'])
  })

  it('appends a notice newer than every message', () => {
    const out = mergeTranscript([msg('a', 10)], [notice('n', 99)])
    expect(out.map(m => m.id)).toEqual(['a', 'n'])
  })

  it('renders a notice with no messages at all', () => {
    expect(mergeTranscript([], [notice('n', 5)]).map(m => m.id)).toEqual(['n'])
  })

  it('keeps multiple notices in ts order', () => {
    const out = mergeTranscript([msg('a', 10)], [notice('n2', 40), notice('n1', 20)])
    expect(out.map(m => m.id)).toEqual(['a', 'n1', 'n2'])
  })

  it('never reorders messages among themselves, even with non-monotonic ts', () => {
    // `messages` mixes browser-stamped live bubbles with daemon-stamped history,
    // so it is NOT guaranteed ts-sorted. A naive sort of the union would
    // reshuffle the user's real conversation; insertion must not.
    const msgs = [msg('a', 100), msg('b', 5), msg('c', 200)]
    const out = mergeTranscript(msgs, [notice('n', 50)])
    expect(out.filter(m => m.id !== 'n').map(m => m.id)).toEqual(['a', 'b', 'c'])
    expect(out.map(m => m.id)).toEqual(['n', 'a', 'b', 'c'])
  })

  it('projects a notice into the system-role Message shape the renderer expects', () => {
    const [only] = mergeTranscript([], [{ id: 'n1', text: 'context lost', ts: 7 }])
    expect(only).toEqual({ id: 'n1', role: 'system', text: 'context lost', ts: 7 })
  })

  it('does not mutate either input list', () => {
    const msgs = [msg('a', 10)]
    const nots = [notice('n2', 40), notice('n1', 20)]
    mergeTranscript(msgs, nots)
    expect(msgs.map(m => m.id)).toEqual(['a'])
    expect(nots.map(n => n.id)).toEqual(['n2', 'n1'])
  })
})

// tether#52 — workspacesLoaded gates ChatPane's first connect (see
// panes/chat/index.tsx's mount effect and shouldDeferFirstConnect). It starts
// false so a cold load defers a sid-less first connect until WorkspacePane's
// GET /api/v1/workspaces settles (or the 2s fallback fires).
describe('store workspacesLoaded (tether#52)', () => {
  afterEach(() => useStore.setState({ workspacesLoaded: false }))

  it('starts false', () => {
    expect(useStore.getState().workspacesLoaded).toBe(false)
  })

  it('setWorkspacesLoaded(true) flips it, and is idempotent', () => {
    useStore.getState().setWorkspacesLoaded(true)
    expect(useStore.getState().workspacesLoaded).toBe(true)
    useStore.getState().setWorkspacesLoaded(true)
    expect(useStore.getState().workspacesLoaded).toBe(true)
  })

  it('setWorkspacesLoaded(false) can flip it back', () => {
    useStore.getState().setWorkspacesLoaded(true)
    useStore.getState().setWorkspacesLoaded(false)
    expect(useStore.getState().workspacesLoaded).toBe(false)
  })
})

// tether#52 — the ordering invariant behind settleWorkspaces, and the one test
// that would have caught the bug this slice shipped and then fixed.
//
// zustand notifies subscribers SYNCHRONOUSLY from inside `set`, and ChatPane's
// gate subscription calls doConnect right there — which reads `activeWorkspace`
// through getState() and bakes it into the connect URL. A brand-new session's cwd
// is pinned at spawn and can never be moved afterwards. So the workspace must
// ALREADY be published in the very notification that reports the gate open;
// publishing it one update (or one React commit) later means every fresh session
// connects with no `ws` and lands in the daemon's default directory forever.
//
// Asserting on the FIRST notification is what makes this falsifiable: splitting
// settleWorkspaces into two `set` calls keeps every other assertion in this file
// green and turns this one red.
describe('store settleWorkspaces ordering (tether#52)', () => {
  afterEach(() => useStore.setState({ workspacesLoaded: false, activeWorkspace: null }))

  it('publishes the workspace in the SAME notification that opens the gate', () => {
    const seen: Array<{ loaded: boolean; ws: string | null }> = []
    const unsub = useStore.subscribe(s => {
      seen.push({ loaded: s.workspacesLoaded, ws: s.activeWorkspace?.id ?? null })
    })
    useStore.getState().settleWorkspaces({ id: 'ws-a', path: '/srv/project-a' })
    unsub()

    expect(seen.length).toBe(1) // one update, not two
    expect(seen[0]).toEqual({ loaded: true, ws: 'ws-a' })
  })

  it('an empty workspace list still opens the gate, with no selection', () => {
    const seen: Array<{ loaded: boolean; ws: string | null }> = []
    const unsub = useStore.subscribe(s => {
      seen.push({ loaded: s.workspacesLoaded, ws: s.activeWorkspace?.id ?? null })
    })
    useStore.getState().settleWorkspaces(null)
    unsub()

    // Chat must still connect (falling back to --workspace-root), not hang.
    expect(seen[0]).toEqual({ loaded: true, ws: null })
  })
})

// tether#66 — the selection has to outlive the page. `ws` only travels on a
// sid-less connect (chatUrl.ts), the only way to get one is App's
// startNewSession → location.reload(), and before this the selection lived in a
// component useState — so the reload that acted on the choice also erased it and
// every new session landed in registry[0]. Persistence hangs off BOTH mutators
// that can move `activeWorkspace`, for the same reason setSessionId owns
// `tether_last_sid`: one funnel, nothing to remember at the call sites.
//
// Both are pinned here because each is a wiring hop — delete either call and the
// store still behaves correctly in every assertion above.
describe('store workspace persistence (tether#66)', () => {
  beforeEach(() => localStorage.clear())
  afterEach(() => {
    // Clear on the way OUT too: these are the first tests in this file to write
    // localStorage, and the tether#52 describe above now writes the key as a
    // side effect of settleWorkspaces. Leaving it set makes later describes
    // order-dependent.
    localStorage.clear()
    useStore.setState({ workspacesLoaded: false, activeWorkspace: null })
  })

  // The literal key string is the contract, not WORKSPACE_ID_KEY — asserting
  // through the constant would let a rename pass, and a rename is what silently
  // forgets every existing user's selection on deploy.
  it('settleWorkspaces records the resolved selection under tether_ws_id', () => {
    useStore.getState().settleWorkspaces({ id: 'ws-a', path: '/srv/project-a' })
    expect(localStorage.getItem('tether_ws_id')).toBe('ws-a')
    expect(rememberedWorkspaceId()).toBe('ws-a')
    expect(WORKSPACE_ID_KEY).toBe('tether_ws_id')
  })

  // Persistence is best-effort; the gate is not. A storage backend that refuses
  // writes (quota, Safari private browsing) must not stop settleWorkspaces from
  // opening ChatPane's first-connect gate — that would hang every new session
  // rather than merely forget a preference.
  it('a throwing localStorage does not break the gate', () => {
    const spy = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new DOMException('quota', 'QuotaExceededError')
    })
    try {
      expect(() => useStore.getState().settleWorkspaces({ id: 'ws-a', path: '/a' })).not.toThrow()
      expect(useStore.getState().workspacesLoaded).toBe(true)
      expect(useStore.getState().activeWorkspace?.id).toBe('ws-a')
      expect(() => useStore.getState().setActiveWorkspace({ id: 'ws-b', path: '/b' })).not.toThrow()
      expect(useStore.getState().activeWorkspace?.id).toBe('ws-b')
    } finally {
      spy.mockRestore()
    }
  })

  it('setActiveWorkspace records a later change', () => {
    useStore.getState().setActiveWorkspace({ id: 'ws-b', path: '/srv/project-b' })
    expect(rememberedWorkspaceId()).toBe('ws-b')
  })

  // The asymmetry, stated as a property. WorkspacePane's publishing effect fires
  // on mount with `null` — before GET /api/v1/workspaces resolves — so erasing on
  // null would blank the remembered id on every page load and put the bug back
  // with the whole suite still green. Callers get "no selection" persisted as
  // "nothing new to say", never as "forget what you knew".
  it('never erases: a null selection leaves the remembered id alone', () => {
    useStore.getState().setActiveWorkspace({ id: 'ws-b', path: '/srv/project-b' })
    useStore.getState().setActiveWorkspace(null)
    expect(rememberedWorkspaceId()).toBe('ws-b')
    useStore.getState().settleWorkspaces(null)
    expect(rememberedWorkspaceId()).toBe('ws-b')
  })
})

// tether#63 — parseErrorPayload defensively narrows a KindError envelope's
// payload to wire.ErrorPayload's shape. Pure, so it tests without a store.
describe('parseErrorPayload (tether#63)', () => {
  it('parses a well-formed ErrorPayload', () => {
    expect(parseErrorPayload({ code: 'unknown_workspace', message: 'nope', terminal: true }))
      .toEqual({ code: 'unknown_workspace', message: 'nope', terminal: true })
  })

  it('returns null for the pre-tether#63 bare-string payload a stale daemon might still send', () => {
    expect(parseErrorPayload('some plain error string')).toBeNull()
  })

  it('returns null for null/undefined', () => {
    expect(parseErrorPayload(null)).toBeNull()
    expect(parseErrorPayload(undefined)).toBeNull()
  })

  it('returns null when a required field is missing or the wrong type', () => {
    expect(parseErrorPayload({ code: 'x', message: 'y' })).toBeNull() // no terminal
    expect(parseErrorPayload({ code: 'x', message: 'y', terminal: 'true' })).toBeNull() // terminal not boolean
    expect(parseErrorPayload({ code: 1, message: 'y', terminal: false })).toBeNull() // code not string
  })
})

// tether#63 — the 'error' envelope handler records a TERMINAL wire.ErrorPayload
// into store.fatal, which is what tells ChatPane's onClose to stop the
// reconnect ladder (shouldReconnectAfterClose in panes/chat/index.tsx) instead
// of retrying a refusal that can never succeed on the same connection.
const errorEnv = (payload: unknown): Envelope => ({ kind: 'error', payload } as unknown as Envelope)

describe('store fatal refusal handling (tether#63)', () => {
  // `notices` is in this reset since tether#77: several cases here send
  // NON-terminal payloads, which now leave a line behind, and without clearing
  // it they hand the next describe a store that is not empty.
  afterEach(() => useStore.setState({ fatal: null, notices: [], streaming: false, streamingMsgId: null, curTurnId: null }))

  it('sets fatal from a terminal error payload', () => {
    const h = useStore.getState().handleEnvelope
    h(errorEnv({ code: 'unknown_workspace', message: 'unknown workspace "foo"', terminal: true }))
    expect(useStore.getState().fatal).toEqual({ code: 'unknown_workspace', message: 'unknown workspace "foo"' })
  })

  it('does NOT set fatal for a retryable (non-terminal) error payload', () => {
    const h = useStore.getState().handleEnvelope
    h(errorEnv({ code: 'connection_closed', message: 'connection closed', terminal: false }))
    expect(useStore.getState().fatal).toBeNull()
  })

  it('leaves a previously-set fatal untouched when a later error is non-terminal', () => {
    const h = useStore.getState().handleEnvelope
    h(errorEnv({ code: 'unknown_workspace', message: 'first', terminal: true }))
    h(errorEnv({ code: 'connection_closed', message: 'second', terminal: false }))
    expect(useStore.getState().fatal).toEqual({ code: 'unknown_workspace', message: 'first' })
  })

  it('an unparsable payload (pre-tether#63 bare string) does not set fatal and does not crash', () => {
    const h = useStore.getState().handleEnvelope
    h(errorEnv('legacy plain-string error'))
    expect(useStore.getState().fatal).toBeNull()
  })

  it('still clears the thinking/streaming indicator on a terminal error (pre-existing behaviour)', () => {
    const h = useStore.getState().handleEnvelope
    useStore.setState({ streaming: true, streamingMsgId: 'm1', curTurnId: 'm1' })
    h(errorEnv({ code: 'session_owned_by_other', message: 'owned', terminal: true }))
    const s = useStore.getState()
    expect(s.streaming).toBe(false)
    expect(s.streamingMsgId).toBeNull()
    expect(s.curTurnId).toBeNull()
  })

  it('clearFatal resets fatal to null and is idempotent', () => {
    useStore.setState({ fatal: { code: 'unknown_workspace', message: 'x' } })
    useStore.getState().clearFatal()
    expect(useStore.getState().fatal).toBeNull()
    useStore.getState().clearFatal() // no-op, no throw
    expect(useStore.getState().fatal).toBeNull()
  })

  // loadHistory is the server-truth replace triggered by session_ready's history
  // refetch — it must NOT clear fatal, mirroring the existing notices guarantee
  // (tether#57): a terminal refusal explaining why the connection is dead is not
  // something a history reload should be able to silently wipe.
  it('loadHistory does NOT clear a pending fatal refusal', () => {
    useStore.setState({ fatal: { code: 'unknown_workspace', message: 'x' } })
    useStore.getState().loadHistory([])
    expect(useStore.getState().fatal).toEqual({ code: 'unknown_workspace', message: 'x' })
  })
})

// tether#77 — a RETRYABLE error must still reach the user.
//
// The daemon classifies "this prompt is gone and nothing will retry it"
// (wire.ErrCodePromptUndelivered, from session/attach.go reopen) and sends a
// frame for it. Before this, the handler cleared the spinner and dropped the
// payload on the floor unless it was terminal — so the tab went back to looking
// idle and healthy while having swallowed what the user typed, and every
// subsequent prompt did the same. The daemon-side classification is inert
// without this half: the frame arrives either way, and what changes is whether
// anyone is shown it.
describe('store retryable error visibility (tether#77)', () => {
  // beforeEach as well as afterEach: these assert exact notice COUNTS, so a
  // store left non-empty by an earlier describe reads as this one's own output.
  // That is not hypothetical — it is how this suite first failed.
  beforeEach(() => {
    reset()
    useStore.setState({ fatal: null })
  })
  afterEach(() => {
    reset()
    useStore.setState({ fatal: null })
  })

  it('surfaces a non-terminal error as a system line in the rendered transcript', () => {
    const h = useStore.getState().handleEnvelope
    h(errorEnv({
      code: 'prompt_undelivered',
      message: 'session abc died again and this connection has already used its one re-open',
      terminal: false,
    }))
    const lines = rendered()
    expect(lines).toHaveLength(1)
    expect(lines[0]?.role).toBe('system')
    // Both halves: the daemon's diagnostic, and the sentence the daemon never
    // says — that the words the user just sent did not go anywhere.
    expect(lines[0]?.text).toContain('Message not delivered')
    expect(lines[0]?.text).toContain('already used its one re-open')
  })

  // In `notices`, not `messages`, for tether#57's reason: session_ready fires a
  // history refetch whose loadHistory replaces `messages` wholesale, and the
  // explanation of why the last thing you typed did not happen is precisely
  // what must not vanish when the transcript reloads.
  it('keeps the line where a history refetch cannot eat it', () => {
    const h = useStore.getState().handleEnvelope
    h(errorEnv({ code: 'prompt_undelivered', message: 'gone', terminal: false }))
    expect(useStore.getState().messages).toHaveLength(0)
    expect(useStore.getState().notices).toHaveLength(1)

    useStore.getState().loadHistory([])
    expect(rendered()).toHaveLength(1)
  })

  // The discriminating case. The session notice above collapses a repeat of the
  // line already showing, because its text is a compile-time constant that
  // carries no new information the second time. These do: each one is a prompt
  // the user pressed enter on and lost, so three identical lines mean three
  // lost prompts. Reusing that dedup here would under-report the exact thing
  // this wi exists to report.
  it('records one line per lost prompt, even when the text repeats', () => {
    const h = useStore.getState().handleEnvelope
    const env = errorEnv({ code: 'prompt_undelivered', message: 'session abc is gone', terminal: false })
    h(env)
    h(env)
    h(env)
    expect(rendered()).toHaveLength(3)
  })

  // A terminal refusal takes the other branch: ChatPane renders `fatal` and
  // stops the reconnect ladder, so adding a notice too would say it twice.
  it('does not add a notice for a terminal refusal', () => {
    const h = useStore.getState().handleEnvelope
    h(errorEnv({ code: 'unknown_workspace', message: 'unknown workspace "foo"', terminal: true }))
    expect(useStore.getState().notices).toHaveLength(0)
    expect(useStore.getState().fatal).toEqual({ code: 'unknown_workspace', message: 'unknown workspace "foo"' })
  })

  // The case that makes the early `break` load-bearing rather than tidy. Once
  // the branch below is gated on the CODE, a terminal payload carrying some
  // other code cannot reach it anyway — so only a payload that claims this code
  // AND terminal can tell "break" from "fall through". That is not a contrived
  // input: Terminal travels as its own field precisely so the two sides can
  // disagree across a partial deploy (wire/errors.go's package doc), and the
  // fatal card is the louder of the two answers.
  it('a payload claiming this code AND terminal gets the card, not also a line', () => {
    const h = useStore.getState().handleEnvelope
    h(errorEnv({ code: 'prompt_undelivered', message: 'gone', terminal: true }))
    expect(useStore.getState().fatal).toEqual({ code: 'prompt_undelivered', message: 'gone' })
    expect(useStore.getState().notices).toHaveLength(0)
  })

  // The pre-tether#63 bare-string shape a stale daemon might still send stays
  // exactly as unclassified as it was: nothing to show, nothing to crash on.
  it('adds nothing for an unparsable payload', () => {
    const h = useStore.getState().handleEnvelope
    h(errorEnv('legacy plain-string error'))
    expect(useStore.getState().notices).toHaveLength(0)
  })

  // Pre-existing behaviour that must survive: the spinner clear happens for
  // every error, classified or not.
  it('still clears the streaming indicator on a non-terminal error', () => {
    const h = useStore.getState().handleEnvelope
    useStore.setState({ streaming: true, streamingMsgId: 'm1', curTurnId: 'm1' })
    h(errorEnv({ code: 'prompt_undelivered', message: 'gone', terminal: false }))
    const s = useStore.getState()
    expect(s.streaming).toBe(false)
    expect(s.streamingMsgId).toBeNull()
    expect(s.curTurnId).toBeNull()
  })
})

// The gate is on the code, not on `!terminal` — asserted in BOTH directions
// because a review mutation that widened it back to every retryable code
// survived the entire suite. It is the largest behavioural decision in this
// change and it was, until these two cases, written down nowhere but a comment.
describe('store retryable error visibility is scoped to prompt_undelivered (tether#77)', () => {
  beforeEach(() => {
    reset()
    useStore.setState({ fatal: null })
  })
  afterEach(() => {
    reset()
    useStore.setState({ fatal: null })
  })

  // agent_error is the one that matters. It arrives on a LIVE session, does not
  // close the connection, and is unbounded — opencode emits one per concurrent
  // prompt — so aliasing it onto this line would stack system lines for the
  // life of the tab. It is also a different statement: the turn had a problem,
  // not "your prompt is gone".
  it('says nothing for a retryable code that is not about a lost prompt', () => {
    const h = useStore.getState().handleEnvelope
    for (const code of ['agent_error', 'connection_closed', 'spawn_failed', 'session_unconfirmed']) {
      h(errorEnv({ code, message: `daemon text for ${code}`, terminal: false }))
    }
    expect(useStore.getState().notices).toHaveLength(0)
  })

  it('a busy session emitting many agent errors does not accumulate lines', () => {
    const h = useStore.getState().handleEnvelope
    for (let i = 0; i < 50; i++) {
      h(errorEnv({ code: 'agent_error', message: 'busy: another prompt is running', terminal: false }))
    }
    expect(useStore.getState().notices).toHaveLength(0)
  })

  it('still says it for prompt_undelivered', () => {
    const h = useStore.getState().handleEnvelope
    h(errorEnv({ code: 'prompt_undelivered', message: 'gone', terminal: false }))
    expect(useStore.getState().notices).toHaveLength(1)
  })
})

// Where the line LANDS is part of what it says: it explains a specific prompt,
// so it has to read after that prompt's bubble. mergeTranscript's tie-break puts
// a notice FIRST when timestamps are equal, which is right for tether#50's
// session-level banner and backwards for this one. Nothing else in the suite
// puts a #77 notice into a transcript that already has messages.
describe('store prompt_undelivered ordering (tether#77)', () => {
  beforeEach(() => {
    reset()
    useStore.setState({ fatal: null })
  })
  afterEach(() => {
    reset()
    useStore.setState({ fatal: null })
  })

  it('reads after the prompt it is explaining', () => {
    const s = useStore.getState()
    // Same millisecond, which is the ordinary case: the prompt fails on the very
    // write that sent it. A tie is what mergeTranscript resolves the wrong way
    // round for this kind of notice.
    s.addMessage({ id: 'u1', role: 'user', text: 'the lost prompt', ts: Date.now() })
    s.handleEnvelope(errorEnv({ code: 'prompt_undelivered', message: 'gone', terminal: false }))

    const lines = rendered()
    expect(lines.map((l) => l.role)).toEqual(['user', 'system'])
    expect(lines[1]?.text).toContain('Message not delivered')
  })

  // The other direction mergeTranscript's doc warns about: refetched history
  // carries the DAEMON's clock, which can be well ahead of the browser's. A
  // notice stamped with a bare Date.now() would then sort above a conversation
  // it has nothing to do with.
  it('reads last even when the transcript carries a clock from the future', () => {
    const s = useStore.getState()
    const ahead = Date.now() + 60_000
    s.loadHistory([
      { id: 'h1', role: 'user', text: 'turn 1', ts: ahead },
      { id: 'h2', role: 'assistant', text: 'answer 1', ts: ahead + 1 },
    ])
    s.handleEnvelope(errorEnv({ code: 'prompt_undelivered', message: 'gone', terminal: false }))

    expect(rendered().map((l) => l.role)).toEqual(['user', 'assistant', 'system'])
  })
})
