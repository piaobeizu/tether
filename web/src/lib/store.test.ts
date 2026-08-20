import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  useStore, mergeTranscript, parseErrorPayload, rememberedWorkspaceId, WORKSPACE_ID_KEY,
  appendAgentErrorNotice, nextNoticeTs, AGENT_ERROR_NOTICE_LIMIT,
  hasOrd, historyEntryToMessage,
  joinTurnAcrossPages, TURN_JOIN,
  type Message, type Notice, type HistoryEntry,
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
  //
  // tether#83 narrowed what "pre-existing behaviour" covers here. This case used
  // to also assert streamingMsgId/curTurnId null, under this same name — a name
  // about the SPINNER guarding a claim about the TURN, which is how the defect
  // #83 fixes stayed pinned. The spinner half is the part that was ever argued
  // for, and it is the part that stays; see the 'error' branch in store.ts for
  // why the two are not the same news.
  it('still clears the streaming indicator on a non-terminal error', () => {
    const h = useStore.getState().handleEnvelope
    useStore.setState({ streaming: true, streamingMsgId: 'm1', curTurnId: 'm1' })
    h(errorEnv({ code: 'prompt_undelivered', message: 'gone', terminal: false }))
    expect(useStore.getState().streaming).toBe(false)
  })
})

// The gate is on the code, not on `!terminal` — asserted in BOTH directions
// because a review mutation that widened it back to every retryable code
// survived the entire suite. It is the largest behavioural decision in this
// change and it was, until these two cases, written down nowhere but a comment.
//
// tether#80 moved one code out of the excluded set: agent_error now has its OWN
// branch and its own sentence (see below). The three that remain are excluded
// for reasons that have nothing to do with noise, so they are still asserted
// here — widening the gate to `!terminal` must still fail.
describe('store retryable error visibility is scoped by code (tether#77, tether#80)', () => {
  beforeEach(() => {
    reset()
    useStore.setState({ fatal: null })
  })
  afterEach(() => {
    reset()
    useStore.setState({ fatal: null })
  })

  // connection_closed arrives when the browser's own context is already done, so
  // there is nobody left to show it to; spawn_failed and session_unconfirmed
  // accompany a connection the daemon is closing, where the reconnect ladder and
  // its failed card already speak and each attempt would add another copy.
  // None of those changed in tether#80.
  it('says nothing for the retryable codes that are about the connection, not the user', () => {
    const h = useStore.getState().handleEnvelope
    for (const code of ['connection_closed', 'spawn_failed', 'session_unconfirmed']) {
      h(errorEnv({ code, message: `daemon text for ${code}`, terminal: false }))
    }
    expect(useStore.getState().notices).toHaveLength(0)
  })

  it('still says it for prompt_undelivered', () => {
    const h = useStore.getState().handleEnvelope
    h(errorEnv({ code: 'prompt_undelivered', message: 'gone', terminal: false }))
    expect(useStore.getState().notices).toHaveLength(1)
  })

  it('and says it for agent_error too, which it did not before tether#80', () => {
    const h = useStore.getState().handleEnvelope
    h(errorEnv({ code: 'agent_error', message: 'busy: another prompt is running', terminal: false }))
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

// ─── tether#80 ────────────────────────────────────────────────────────────────
//
// The agent's OWN error text (wire.ErrCodeAgent, from registry.go translateEvent
// on agent.EventError) reached the browser and was dropped on the floor: the
// handler cleared the spinner and fell through to `break`, so the turn simply
// stopped and nothing said why. tether#77 shipped the visibility MECHANISM but
// deliberately gated it to one code, and its review measured why this one could
// not just join that branch: an agent error arrives on a LIVE connection that
// nothing closes, at a rate the AGENT decides (opencode emits one per concurrent
// prompt), and nothing prunes `notices` short of a session switch or a reload —
// 200 frames would be 200 permanent lines.
//
// So the assertions here come in two halves that have to hold TOGETHER, and
// either one alone is a bug: it must be VISIBLE, and it must be BOUNDED.
describe('agent errors are visible (tether#80)', () => {
  beforeEach(() => {
    reset()
    useStore.setState({ fatal: null })
  })
  afterEach(() => {
    reset()
    useStore.setState({ fatal: null })
  })

  const agentErr = (message: string) => errorEnv({ code: 'agent_error', message, terminal: false })

  it('surfaces the agent\'s error text as a system line in the rendered transcript', () => {
    useStore.getState().handleEnvelope(agentErr('busy: another prompt is running'))
    const lines = rendered()
    expect(lines).toHaveLength(1)
    expect(lines[0]?.role).toBe('system')
    // Both halves: who is speaking (the agent, not tether — the session is still
    // alive), and verbatim what it said.
    expect(lines[0]?.text).toContain('The agent reported an error')
    expect(lines[0]?.text).toContain('busy: another prompt is running')
  })

  // Not `messages`, and for a stronger reason than tether#57's: registry.go's
  // fanOut never writes agent.EventError to HistoryStore at all (it persists
  // thinking / tool_use / tool_result / text / blocks and forwards the error
  // without recording it), so the live frame is the ONLY copy that exists. In
  // `messages`, the history refetch session_ready triggers would replace it away
  // permanently.
  it('keeps the agent error where a history refetch cannot eat it', () => {
    useStore.getState().handleEnvelope(agentErr('opencode serve exited unexpectedly'))
    expect(useStore.getState().messages).toHaveLength(0)
    expect(useStore.getState().notices).toHaveLength(1)

    useStore.getState().loadHistory([])
    expect(rendered()).toHaveLength(1)
  })

  // Where it lands is part of what it says: it is about the turn the user just
  // started, so it has to read after that prompt. Same millisecond is the
  // ordinary case, and mergeTranscript's tie-break resolves a tie the other way
  // round (right for tether#50's banner, backwards for this).
  //
  // The clock is PINNED, not merely read twice. tether#77's equivalent case
  // stamps the message with a live Date.now() and relies on the notice landing in
  // the same millisecond — which is the ordinary case but not a guaranteed one,
  // so it only fails PROBABILISTICALLY against a bare-Date.now() stamp. A
  // mutation run that replaced nextNoticeTs with Date.now() survived it. Freezing
  // the clock makes the tie certain and the mutant dead.
  it('reads after the prompt whose turn it is about, even in the same millisecond', () => {
    const frozen = 1_800_000_000_000
    const now = vi.spyOn(Date, 'now').mockReturnValue(frozen)
    try {
      const s = useStore.getState()
      s.addMessage({ id: 'u1', role: 'user', text: 'do the thing', ts: frozen })
      s.handleEnvelope(agentErr('busy: another prompt is running'))
      const lines = rendered()
      expect(lines.map((l) => l.role)).toEqual(['user', 'system'])
      expect(lines[1]?.text).toContain('The agent reported an error')
    } finally {
      now.mockRestore()
    }
  })

  // Same narrowing as tether#77's copy above, for the same reason: the turn
  // pointers were never what "clears the streaming indicator" meant, and
  // tether#83 keeps them. The spinner assertion is unchanged.
  it('still clears the streaming indicator (pre-existing behaviour for every error)', () => {
    useStore.setState({ streaming: true, streamingMsgId: 'm1', curTurnId: 'm1' })
    useStore.getState().handleEnvelope(agentErr('busy: another prompt is running'))
    expect(useStore.getState().streaming).toBe(false)
  })

  // The self-contradictory payload, same shape as tether#77's: Terminal travels
  // as its own wire field precisely so the two sides CAN disagree across a
  // partial deploy (wire/errors.go's package doc), and the fatal card is the
  // louder answer. This is what makes the terminal branch's early `break`
  // load-bearing rather than tidy for this code too.
  it('a payload claiming agent_error AND terminal gets the card, not also a line', () => {
    useStore.getState().handleEnvelope(errorEnv({ code: 'agent_error', message: 'gone', terminal: true }))
    expect(useStore.getState().fatal).toEqual({ code: 'agent_error', message: 'gone' })
    expect(useStore.getState().notices).toHaveLength(0)
  })

  it('adds nothing for the pre-tether#63 bare-string payload a stale daemon might send', () => {
    useStore.getState().handleEnvelope(errorEnv('legacy plain-string error'))
    expect(useStore.getState().notices).toHaveLength(0)
  })
})

// The other half. Visibility without a bound is the failure tether#77's review
// refused to ship, so these are not "nice to have" cases — remove the collapse
// or the cap and the fix above becomes the bug it was rejected as.
describe('agent error lines stay bounded (tether#80)', () => {
  beforeEach(() => {
    reset()
    useStore.setState({ fatal: null })
  })
  afterEach(() => {
    reset()
    useStore.setState({ fatal: null })
  })

  const agentErr = (message: string) => errorEnv({ code: 'agent_error', message, terminal: false })

  // The measured case: opencode's SendPrompt busy branch fires for every
  // concurrent prompt, on a connection that stays open for the life of the tab.
  it('200 identical agent errors are ONE line carrying the count', () => {
    const h = useStore.getState().handleEnvelope
    for (let i = 0; i < 200; i++) h(agentErr('busy: another prompt is running'))
    expect(useStore.getState().notices).toHaveLength(1)
    const lines = rendered()
    expect(lines).toHaveLength(1)
    expect(lines[0]?.text).toContain('busy: another prompt is running')
    // The count is information, not decoration: that branch DROPS the prompt
    // (opencode_provider.go returns nil after emitting), so 200 arrivals are 200
    // prompts the user typed and lost — the same fact tether#77 keeps for its own
    // line by refusing to dedup it.
    expect(lines[0]?.text).toContain('(×200)')
  })

  it('a single arrival renders no count at all', () => {
    useStore.getState().handleEnvelope(agentErr('busy: another prompt is running'))
    expect(rendered()[0]?.text).toBe('The agent reported an error — busy: another prompt is running')
  })

  // The collapsed line follows the conversation instead of going stale at the
  // position of its first arrival.
  it('a repeat moves the line down to the turn that just triggered it', () => {
    const s = useStore.getState()
    s.handleEnvelope(agentErr('busy: another prompt is running'))
    s.addMessage({ id: 'u1', role: 'user', text: 'try again', ts: Date.now() + 1000 })
    s.handleEnvelope(agentErr('busy: another prompt is running'))
    expect(rendered().map((l) => l.role)).toEqual(['user', 'system'])
  })

  // The second bound, for the case the collapse cannot reach: opencode's
  // session.error carries whatever the provider said, and several emit sites
  // wrap a varying underlying error, so distinct texts are not bounded either.
  it('distinct agent errors are capped, keeping the most recent', () => {
    const h = useStore.getState().handleEnvelope
    const n = AGENT_ERROR_NOTICE_LIMIT + 3
    for (let i = 0; i < n; i++) h(agentErr(`distinct failure ${i}`))
    expect(useStore.getState().notices).toHaveLength(AGENT_ERROR_NOTICE_LIMIT)
    const texts = rendered().map((l) => l.text)
    expect(texts.some((t) => t.includes(`distinct failure ${n - 1}`))).toBe(true)
    expect(texts.some((t) => t.includes('distinct failure 0'))).toBe(false)
  })

  // Eviction is least-recently-SEEN, not first-arrived. A line that keeps
  // refreshing is the session's live complaint; dropping it to make room for a
  // one-off would discard the more useful of the two. Position alone cannot tell
  // these apart, because a refresh does not move the entry in the array.
  //
  // The turns are what advance the clock here (nextNoticeTs stamps a notice at
  // the last message's ts + 1), so no clock mocking is needed — and it has to be
  // advanced by SOMETHING: a first draft fired every arrival in one tight loop,
  // where every ts is the same millisecond, ties make least-recently-seen and
  // first-arrived identical, and the mutant that swapped them survived.
  it('a repeatedly-refreshed line survives eviction by a newer one-off', () => {
    const s = useStore.getState()
    const h = s.handleEnvelope
    const base = Date.now()
    // Each "turn" is a user message dated further into the future, so the notice
    // that follows it is stamped strictly later than the previous one.
    const turn = (k: number) => s.addMessage({ id: `m${k}`, role: 'user', text: `turn ${k}`, ts: base + k * 1000 })

    turn(1); h(agentErr('the recurring one'))
    for (let i = 0; i < AGENT_ERROR_NOTICE_LIMIT - 1; i++) { turn(2 + i); h(agentErr(`one-off ${i}`)) }
    // Refresh the recurring line: now the most recently SEEN, but still the
    // FIRST entry in the array — the only arrangement that tells the two
    // eviction rules apart.
    turn(50); h(agentErr('the recurring one'))
    turn(51); h(agentErr('the newcomer')) // one over the cap

    const texts = rendered().map((l) => l.text)
    expect(useStore.getState().notices).toHaveLength(AGENT_ERROR_NOTICE_LIMIT)
    expect(texts.some((t) => t.includes('the recurring one'))).toBe(true)
    expect(texts.some((t) => t.includes('the newcomer'))).toBe(true)
    expect(texts.some((t) => t.includes('one-off 0'))).toBe(false)
  })

  // The two classes must not touch each other. tether#77's line is deliberately
  // NOT deduplicated (three identical lines mean three lost prompts) and an
  // existing case above asserts that; the cap must not evict one either.
  it('does not collapse, count, or evict a prompt_undelivered line', () => {
    const h = useStore.getState().handleEnvelope
    const lost = errorEnv({ code: 'prompt_undelivered', message: 'session abc is gone', terminal: false })
    h(lost)
    h(lost)
    h(lost)
    for (let i = 0; i < AGENT_ERROR_NOTICE_LIMIT + 3; i++) h(agentErr(`distinct failure ${i}`))
    const lines = rendered()
    expect(lines.filter((l) => l.text.includes('Message not delivered'))).toHaveLength(3)
    expect(lines.filter((l) => l.text.includes('The agent reported an error'))).toHaveLength(AGENT_ERROR_NOTICE_LIMIT)
    // And no count leaks onto them.
    for (const l of lines.filter((x) => x.text.includes('Message not delivered'))) {
      expect(l.text).not.toContain('(×')
    }
  })

  // An agent error whose text happens to match tether#50's session banner must
  // not be collapsed into it (or vice versa) — the classes are matched on `kind`,
  // not on text alone.
  it('does not collapse into a session notice that happens to read the same', () => {
    const shared = 'The agent reported an error — collision'
    useStore.setState({ notices: [{ id: 'n0', text: shared, ts: Date.now(), kind: 'session' }] })
    useStore.getState().handleEnvelope(agentErr('collision'))
    expect(useStore.getState().notices).toHaveLength(2)
    expect(useStore.getState().notices[0]?.repeats).toBeUndefined()
  })

  // The store-level form of the eviction defect the pure test pins below: the
  // clock really can run backwards between arrivals, because nextNoticeTs reads
  // it off the transcript and a history refetch swaps whose clock that is.
  it('shows the newest agent error even after the transcript clock runs backwards', () => {
    const s = useStore.getState()
    // A daemon-ahead history. Every notice stamped now lands far in the future.
    s.loadHistory([{ id: 'h1', role: 'user', text: 'turn 1', ts: Date.now() + 600_000 }])
    for (let i = 0; i < AGENT_ERROR_NOTICE_LIMIT; i++) s.handleEnvelope(agentErr(`early failure ${i}`))
    // …then the transcript is replaced, so the browser clock governs again and is
    // behind every ts already in the notice list.
    s.loadHistory([])
    s.handleEnvelope(agentErr('the newest failure'))

    const texts = rendered().map((l) => l.text)
    expect(useStore.getState().notices).toHaveLength(AGENT_ERROR_NOTICE_LIMIT)
    expect(texts.some((t) => t.includes('the newest failure'))).toBe(true)
  })
})

// ─── tether#83 ────────────────────────────────────────────────────────────────
//
// case 'error' opened with one unconditional reset of every turn pointer, whose
// stated reason ("even a terminal refusal ends whatever turn was in flight")
// argued only the terminal case. A non-terminal error does not tell the browser
// the turn is over, and a turn that is still streaming when one lands is not a
// hypothetical: the daemon goes on sending that turn's deltas afterwards, which
// is asserted here twice — structurally, and against frames captured off a live
// daemon (bottom of this block).
//
// With curTurnId nulled, the next delta finds no open bubble and starts a second
// one. That is the damage that does not heal: nothing merges two bubbles, and
// finalizeTurn can only ever stamp answerMs on the pointer it finds.
describe('a non-terminal error leaves the turn open (tether#83)', () => {
  beforeEach(() => { reset(); useStore.setState({ fatal: null }) })
  afterEach(() => { reset(); useStore.setState({ fatal: null }) })

  const agentErr = (message: string) => errorEnv({ code: 'agent_error', message, terminal: false })

  it('keeps one turn in one bubble when the error arrives mid-answer', () => {
    const h = useStore.getState().handleEnvelope
    h(textEnv('the first half '))
    h(agentErr('busy: another prompt is running'))
    h(textEnv('and the second'))
    h(resultEnv())

    const assistants = useStore.getState().messages.filter((m) => m.role === 'assistant')
    expect(assistants).toHaveLength(1)
    expect(assistants[0].text).toBe('the first half and the second')
  })

  // The turn's CLOCKS, asserted separately from its bubble because keeping
  // curTurnId alone already produces one bubble — so a fix that dropped
  // answerStartTs would look right and quietly under-report every badge on a turn
  // an agent error touched.
  //
  // The elapsed VALUE is what makes this bite, and it took a surviving mutant to
  // find that out. `expect(answerMs).toBeDefined()` does not: the answer branch
  // re-stamps answerStartTs whenever it finds it null (store.ts), so a fix that
  // kept curTurnId and dropped the clock still produces a badge — one that starts
  // at the first delta AFTER the error (here t0+3000, so 2000ms) rather than at
  // the answer's first token.
  it('leaves the turn its answer clock, so the badge measures the whole answer', () => {
    const t0 = 1_800_000_000_000
    const now = vi.spyOn(Date, 'now')
    try {
      const h = useStore.getState().handleEnvelope
      now.mockReturnValue(t0)
      h(textEnv('half '))
      now.mockReturnValue(t0 + 1_000)
      h(agentErr('busy: another prompt is running'))
      now.mockReturnValue(t0 + 3_000)
      h(textEnv('an answer'))
      now.mockReturnValue(t0 + 5_000)
      h(resultEnv())

      const first = useStore.getState().messages.find((m) => m.role === 'assistant')
      expect(first?.answerMs).toBe(5_000)
    } finally {
      now.mockRestore()
    }
  })

  // The thinking clock has no such self-repair, so it is the clock that is lost
  // outright: thinkingMs is stamped once, by the FIRST answer delta, and only if
  // thinkingStartTs survived until then. Without it the live "thinking…" block
  // never collapses to "thought Xs" for the rest of the turn.
  it('leaves the turn its thinking clock, so the block still collapses', () => {
    const h = useStore.getState().handleEnvelope
    h(thinkingEnv('pondering'))
    h(agentErr('busy: another prompt is running'))
    h(textEnv('the answer'))

    const first = useStore.getState().messages.find((m) => m.role === 'assistant')
    expect(first?.thinking).toBe('pondering')
    expect(first?.thinkingMs).toBeDefined()
  })

  // NOT a regression test — this passed before the fix too. It pins the deliberate
  // asymmetry: the turn survives, the spinner does not. The browser cannot tell
  // this frame apart from the ones where the prompt never started and no result
  // will ever follow (opencode_provider.go's resume-serve failure and its two
  // `opencode run` start failures all return before emitting EventResult). A
  // spinner stuck ON disables Enter (shouldSendOnEnter); a spinner cleared early
  // costs until the next delta, which is what the last two lines here pin.
  it('still stops the spinner, and the next delta restarts it', () => {
    const h = useStore.getState().handleEnvelope
    h(textEnv('working'))
    expect(useStore.getState().streaming).toBe(true)
    h(agentErr('busy: another prompt is running'))
    expect(useStore.getState().streaming).toBe(false)
    h(textEnv(' still'))
    expect(useStore.getState().streaming).toBe(true)
  })

  // tether#63's case, unchanged and load-bearing: a terminal refusal means the
  // connection is going away, so nothing will arrive to finish the turn and every
  // pointer must go. This is the branch the fix must NOT widen over.
  //
  // unknown_workspace, not spawn_failed: wire/errors.go's terminalCodes maps
  // spawn_failed to false and NewErrorEnvelope derives the bit from that table,
  // so {spawn_failed, terminal:true} is a frame no daemon can send. A fixture
  // that cannot occur would still exercise the branch and would stop describing
  // anything.
  it('a terminal refusal still ends the turn it lands in', () => {
    const h = useStore.getState().handleEnvelope
    h(textEnv('half an answer'))
    h(errorEnv({ code: 'unknown_workspace', message: 'gone for good', terminal: true }))
    const s = useStore.getState()
    expect(s.streaming).toBe(false)
    expect(s.curTurnId).toBeNull()
    expect(s.streamingMsgId).toBeNull()
    expect(s.answerStartTs).toBeNull()
    expect(s.thinkingStartTs).toBeNull()
    expect(s.fatal).toEqual({ code: 'unknown_workspace', message: 'gone for good' })
  })

  // The pre-tether#63 bare-string shape a stale daemon might still send carries no
  // classification at all, so it cannot be read as "non-terminal" — it keeps the
  // old wipe, which is also the conservative reading (a frame that might have been
  // terminal leaves nothing dangling).
  it('an unparsable payload still ends the turn', () => {
    const h = useStore.getState().handleEnvelope
    h(textEnv('half an answer'))
    h(errorEnv('legacy plain-string error'))
    const s = useStore.getState()
    expect(s.curTurnId).toBeNull()
    expect(s.streamingMsgId).toBeNull()
    expect(s.answerStartTs).toBeNull()
  })

  // The premise the whole branch rests on — "a non-terminal error can land on a
  // turn that is still streaming" — is a claim about the DAEMON, so it is
  // asserted against the daemon's own output rather than against frames written
  // to suit the fix.
  //
  // Captured 2026-08-08 from a tether daemon built at v0.5.0-79-g9bee8d7 talking
  // to a real opencode over the real HTTP/3 + WebTransport stack: one prompt
  // answering, a second prompt sent 200ms after the first token of that answer,
  // and what came back. The busy rejection landed at 15931ms and the SAME turn
  // went on sending text until its result at 18745ms — 231 more message
  // envelopes, of which the four that immediately followed are kept below.
  //
  // These lines are unedited daemon output, contiguous in the capture. They are
  // parsed here rather than written as objects because that is precisely the hop
  // being reproduced: wt.ts reads one uni stream per envelope and hands each line
  // to JSON.parse before handleEnvelope ever sees it.
  //
  // Which is also why the concatenation below reads like nonsense, and why that
  // is left in rather than tidied: the capturing client observed these envelopes
  // in an order that is NOT the order wt_chat.go's drain loop sent them (it sends
  // strictly in sequence). A hand-written fixture would not look like this.
  // Whether a browser's incomingUnidirectionalStreams reorders the same way was
  // not established here and nothing below depends on it — what the capture is
  // for is that an error envelope is interleaved with its own turn's deltas at
  // all.
  //
  // Two things this does NOT claim:
  //   - That a browser driven by a user typing twice would have rendered one
  //     bubble. At the time of this capture it would not, and not because of this
  //     branch — sendMessage's own addMessage ended the turn locally before the
  //     rejection got back. That was tether#88, fixed since, and the tether#88
  //     block below replays this same capture WITH the second prompt in it.
  //   - That this exact frame was produced by opencode's session.error. It was
  //     not; "busy: another prompt is running" is verbatim from SendPrompt's busy
  //     branch. session.error emits from an SSE reader that keeps reading, so the
  //     same interleaving should arise there with no second prompt involved and
  //     with the bubble still open — read from the provider, NOT captured.
  const CAPTURED_LIVE = [
    // 61 envelopes elided before this window (session_ready + the answer so far).
    '{"kind":"message","sessionId":"ses_020277165ffe7MEV6MyeiBJp8w","payload":" Six"}',
    '{"kind":"message","sessionId":"ses_020277165ffe7MEV6MyeiBJp8w","payload":" is"}',
    '{"kind":"message","sessionId":"ses_020277165ffe7MEV6MyeiBJp8w","payload":" the"}',
    '{"kind":"message","sessionId":"ses_020277165ffe7MEV6MyeiBJp8w","payload":" perfect"}',
    '{"kind":"error","sessionId":"ses_020277165ffe7MEV6MyeiBJp8w","payload":{"code":"agent_error","message":"busy: another prompt is running","terminal":false}}',
    '{"kind":"message","sessionId":"ses_020277165ffe7MEV6MyeiBJp8w","payload":" often"}',
    '{"kind":"message","sessionId":"ses_020277165ffe7MEV6MyeiBJp8w","payload":"7"}',
    '{"kind":"message","sessionId":"ses_020277165ffe7MEV6MyeiBJp8w","payload":" number"}',
    '{"kind":"message","sessionId":"ses_020277165ffe7MEV6MyeiBJp8w","payload":"."}',
    // 227 further message envelopes elided, then the turn's own result.
    '{"kind":"result","sessionId":"ses_020277165ffe7MEV6MyeiBJp8w","payload":"stop"}',
  ]

  it('replays a live capture into one bubble', () => {
    const h = useStore.getState().handleEnvelope
    for (const line of CAPTURED_LIVE) h(JSON.parse(line) as Envelope)

    const assistants = useStore.getState().messages.filter((m) => m.role === 'assistant')
    expect(assistants).toHaveLength(1)
    expect(assistants[0].text).toBe(' Six is the perfect often7 number.')
    // And the agent's complaint is still shown — tether#80's half of the same
    // frame, which this must not trade away to keep the turn.
    expect(useStore.getState().notices.map((n) => n.text)).toEqual([
      'The agent reported an error — busy: another prompt is running',
    ])
  })
})

// ─── tether#88 ────────────────────────────────────────────────────────────────
//
// addMessage's user branch used to null curTurnId and both turn clocks, so the
// browser ENDED the running turn the moment the user sent anything. Everything
// the daemon went on sending for that turn then found no open bubble and started
// a second one, and the first never got its answerMs — the damage tether#83
// describes, arriving through the door tether#83 could not close.
//
// These drive addMessage directly rather than through ChatPane, because it is
// the first thing both send paths do: sendMessage (panes/chat/index.tsx) and
// doInjectAndSend each run `addMessage(user) → setState({streaming, streamingMsgId})`
// and only then write to the wire. Mounting the pane would add a WebTransport
// connection and assert nothing more about this reducer.
//
// What that leaves OUT, since the reducer is not the whole story: only the last
// test here replays the setState half, so nothing below covers what the surviving
// curTurnId does to the pane — the thinking dots' gate, ThinkingBlock's `live`,
// and the autoscroll dep, which is the one that needed changing (tether#88; its
// decision is pinned in ChatPane.test.tsx as transcriptTextLength). Nor is
// injectAndSend fully modelled: it also gates on connState and can defer the
// addMessage by up to 5s through pendingInjectRef.
describe('addMessage no longer ends the turn the daemon is still streaming (tether#88)', () => {
  beforeEach(reset)
  afterEach(reset)

  // The core case. Before the fix the two halves land in two bubbles and the
  // first has no answerMs; nothing merges them afterwards.
  it('keeps the running turn in one bubble when a second prompt is sent', () => {
    const h = useStore.getState().handleEnvelope
    h(textEnv('the first half '))
    useStore.getState().addMessage({ id: 'u2', role: 'user', text: 'and another thing', ts: 2 })
    h(textEnv('and the second'))
    h(resultEnv())

    const assistants = useStore.getState().messages.filter((m) => m.role === 'assistant')
    expect(assistants).toHaveLength(1)
    expect(assistants[0].text).toBe('the first half and the second')
    expect(assistants[0].answerMs).toBeDefined()
  })

  // The clocks, asserted on the VALUE and not just on definedness — tether#83's
  // surviving mutant is the reason. The answer branch re-stamps answerStartTs
  // whenever it finds it null, so a fix that kept curTurnId and still dropped the
  // clocks produces a badge here too: one that starts at the first delta AFTER
  // the prompt (t0+3000, i.e. 2000ms) instead of at the answer's first token.
  it('leaves the turn its answer clock, so the badge still measures the whole answer', () => {
    const t0 = 1_800_000_000_000
    const now = vi.spyOn(Date, 'now')
    try {
      const h = useStore.getState().handleEnvelope
      now.mockReturnValue(t0)
      h(textEnv('half '))
      now.mockReturnValue(t0 + 1_000)
      useStore.getState().addMessage({ id: 'u2', role: 'user', text: 'hurry up', ts: t0 + 1_000 })
      now.mockReturnValue(t0 + 3_000)
      h(textEnv('an answer'))
      now.mockReturnValue(t0 + 5_000)
      h(resultEnv())

      const first = useStore.getState().messages.find((m) => m.role === 'assistant')
      expect(first?.answerMs).toBe(5_000)
    } finally {
      now.mockRestore()
    }
  })

  // The thinking clock has no self-repair at all: thinkingMs is stamped once, by
  // the first answer delta, and only if thinkingStartTs survived until then.
  // Named apart from tether#83's near-twin above on purpose: they differ only in
  // what interrupts the turn, and a `-t` run that matches both cannot show which
  // one a mutant killed.
  it('leaves the turn its thinking clock across a second prompt', () => {
    const h = useStore.getState().handleEnvelope
    h(thinkingEnv('pondering'))
    useStore.getState().addMessage({ id: 'u2', role: 'user', text: 'well?', ts: 2 })
    h(textEnv('the answer'))

    const first = useStore.getState().messages.find((m) => m.role === 'assistant')
    expect(first?.thinking).toBe('pondering')
    expect(first?.thinkingMs).toBeDefined()
  })

  // The other side of the fix, and the one an over-broad version breaks: the turn
  // must still END. cc does not reject a prompt sent mid-turn the way opencode
  // does — ccSession.SendPrompt has no busy gate and writes it straight to cc's
  // stdin — it QUEUES it and runs it after the first turn's result (measured
  // 2026-08-09 against a real cc: first result at 19209ms, the second turn's own
  // system/init at 19246ms, its result at 21112ms, no delta of the second turn
  // before the first result). The frames below are a minimal stand-in for that
  // SHAPE, not a capture of it: a real cc run also carries session_ready, usage,
  // and far larger text chunks. What is being pinned is that the queued turn owes
  // its own bubble and its own badge.
  it('still opens a fresh bubble for the queued turn once the first one ends', () => {
    const h = useStore.getState().handleEnvelope
    h(textEnv('answer A'))
    useStore.getState().addMessage({ id: 'u2', role: 'user', text: 'prompt B', ts: 2 })
    h(textEnv(' continued'))
    h(resultEnv())
    h(textEnv('answer B'))
    h(resultEnv())

    const roles = useStore.getState().messages.map((m) => m.role)
    expect(roles).toEqual(['assistant', 'user', 'assistant'])
    const assistants = useStore.getState().messages.filter((m) => m.role === 'assistant')
    expect(assistants.map((m) => m.text)).toEqual(['answer A continued', 'answer B'])
    expect(assistants[0].answerMs).toBeDefined()
    expect(assistants[1].answerMs).toBeDefined()
  })

  // NEGATIVE CONTROLS — these two and only these two pass with or without the
  // tether#88 change, so nobody later reads "7 passed" as seven independent
  // guards. They cover `stopped`, the one field the reset still touches, and they
  // are here because the change had to leave it alone: without them, deleting
  // `stopped: false` along with the rest would have been silent. (The five that do
  // discriminate are the four tests above and the live replay at the end.)
  //
  // tether#42, unchanged: `stopped` has to keep clearing, or every turn after a
  // manual Stop would have its deltas dropped. It cannot collide with an open turn
  // — stopTurn runs finalizeTurn first, so stopped === true implies
  // curTurnId === null, which the middle assertion pins. The pre-existing test of
  // the same rule ('a new user turn clears the stopped flag so the next turn
  // streams normally', tether#42's block) does not assert that implication, which
  // is what this change made worth pinning.
  it('still re-arms delta handling after a manual stop', () => {
    const h = useStore.getState().handleEnvelope
    h(textEnv('a'))
    useStore.getState().stopTurn()
    expect(useStore.getState().curTurnId).toBeNull()
    useStore.getState().addMessage({ id: 'u1', role: 'user', text: 'again', ts: 1 })
    expect(useStore.getState().stopped).toBe(false)
    h(textEnv('fresh answer'))

    const assistants = useStore.getState().messages.filter((m) => m.role === 'assistant')
    expect(assistants.map((m) => m.text)).toEqual(['a', 'fresh answer'])
    expect(useStore.getState().streaming).toBe(true)
  })

  // An assistant message must not clear `stopped` — only a user turn does. Pins
  // the role gate, which a fix that hoisted `stopped: false` out of the ternary
  // would silently delete while every other test here stayed green.
  it('does not let an assistant message re-arm a stopped turn', () => {
    const h = useStore.getState().handleEnvelope
    h(textEnv('partial'))
    useStore.getState().stopTurn()
    useStore.getState().addMessage({ id: 'a1', role: 'assistant', text: 'injected', ts: 1 })
    expect(useStore.getState().stopped).toBe(true)
    h(textEnv(' late buffered'))
    expect(useStore.getState().messages.some((m) => m.text.includes('late buffered'))).toBe(false)
  })

  // The same thing again, end to end, against frames nobody wrote to suit it.
  //
  // Captured 2026-08-09 from a tether daemon built from the Go tree at main
  // 34b5bfe (`tether version` says v0.1.0-dev; that build carries no ldflags)
  // talking to a real opencode over the real HTTP/3 + WebTransport stack, driven
  // by a Go client that sent the second prompt 1526ms after the first token of
  // the first answer. The client had processed 72 envelopes of the run when it
  // wrote that prompt; the busy rejection came back, and the SAME turn went on
  // sending text for another 3.6 seconds — 171 more envelopes, 169 of them
  // deltas, before its own result.
  //
  // The array below is a WINDOW on that run, not the run: it opens on the last
  // four of those 72 (the 68 before them, session_ready and the answer so far,
  // are dropped without a marker — there is nothing to mark, the array simply
  // starts mid-answer), and elides two stretches after the seam, which ARE
  // marked. So SECOND_PROMPT_AFTER below is 4, an index into this array, and 72
  // is the count in the run; both describe the same instant.
  //
  // The lines are parsed here rather than written as objects because that is the
  // hop being reproduced: wt.ts reads one uni stream per envelope and hands each
  // line to JSON.parse before handleEnvelope sees it. They are unedited, which is
  // why the concatenation reads like nonsense — this is the order the CLIENT saw,
  // and it is not the order the daemon sent. Everything here after session_ready
  // came out of serveChat's one sequential drain loop, one OpenUniStreamSync per
  // envelope (the busy rejection included: opencode's SendPrompt returns nil, so
  // it travels as an agent event through fanOut, not through the prompt-reader's
  // own sendEnvelope, which is the other, concurrent sender on this route). Send
  // order is still not delivery order — wt_chat.go says so itself where it sends
  // the tether#50 notice — and the same scrambling is visible in tether#83's
  // capture. Nothing below depends on the order being faithful: what the capture
  // is for is that a large part of one turn's answer arrives AFTER the user has
  // sent their next prompt.
  //
  // NOT captured here, and named so it is not read as if it were: the cc side.
  // The same client against the same daemon with `?provider=claude-code` shows a
  // different shape — no rejection, the first turn's result at 7064ms, then a
  // SECOND turn's delta at 10644ms and its own result at 10650ms. That is why the
  // queued-turn test above exists as well as this one, and why neither of them is
  // a reason to block the second prompt in the composer.
  const CAPTURED_LIVE = [
    '{"kind":"message","sessionId":"ses_018e34b7bffeMC27xL0dmB1RvH","payload":"."}',
    '{"kind":"message","sessionId":"ses_018e34b7bffeMC27xL0dmB1RvH","payload":" cool"}',
    '{"kind":"message","sessionId":"ses_018e34b7bffeMC27xL0dmB1RvH","payload":"18"}',
    '{"kind":"message","sessionId":"ses_018e34b7bffeMC27xL0dmB1RvH","payload":"19"}',
    // ── the user's second prompt was written here (SECOND_PROMPT_AFTER) ──
    '{"kind":"message","sessionId":"ses_018e34b7bffeMC27xL0dmB1RvH","payload":" steady"}',
    '{"kind":"message","sessionId":"ses_018e34b7bffeMC27xL0dmB1RvH","payload":"."}',
    '{"kind":"message","sessionId":"ses_018e34b7bffeMC27xL0dmB1RvH","payload":"\\n"}',
    '{"kind":"message","sessionId":"ses_018e34b7bffeMC27xL0dmB1RvH","payload":" shy"}',
    // 7 further deltas of the same answer elided.
    '{"kind":"message","sessionId":"ses_018e34b7bffeMC27xL0dmB1RvH","payload":" clean"}',
    '{"kind":"message","sessionId":"ses_018e34b7bffeMC27xL0dmB1RvH","payload":" small"}',
    '{"kind":"message","sessionId":"ses_018e34b7bffeMC27xL0dmB1RvH","payload":"\\n"}',
    '{"kind":"message","sessionId":"ses_018e34b7bffeMC27xL0dmB1RvH","payload":"\\n"}',
    '{"kind":"error","sessionId":"ses_018e34b7bffeMC27xL0dmB1RvH","payload":{"code":"agent_error","message":"busy: another prompt is running","terminal":false}}',
    '{"kind":"message","sessionId":"ses_018e34b7bffeMC27xL0dmB1RvH","payload":" sharp"}',
    '{"kind":"message","sessionId":"ses_018e34b7bffeMC27xL0dmB1RvH","payload":"."}',
    '{"kind":"message","sessionId":"ses_018e34b7bffeMC27xL0dmB1RvH","payload":"\\n"}',
    '{"kind":"message","sessionId":"ses_018e34b7bffeMC27xL0dmB1RvH","payload":"23"}',
    // 150 further deltas elided, then the turn's own result.
    '{"kind":"result","sessionId":"ses_018e34b7bffeMC27xL0dmB1RvH","payload":"stop"}',
  ]

  // Where the send goes when this WINDOW is replayed: after four of its lines,
  // which are envelopes 69-72 of the run. Replaying it at any other index would
  // still be a real sequence of daemon frames, but it would no longer be this run.
  const SECOND_PROMPT_AFTER = 4

  it('replays a live capture, second prompt and all, into one bubble', () => {
    const s = useStore.getState()
    CAPTURED_LIVE.forEach((line, i) => {
      if (i === SECOND_PROMPT_AFTER) {
        // Exactly what sendMessage and injectAndSend do, in their order
        // (panes/chat/index.tsx): the store is updated first, the bytes go on
        // the wire after. This is the half of the bug that lives on the send
        // side, so replaying the frames without it would not reproduce it.
        s.addMessage({ id: 'u2', role: 'user', text: 'Actually, what is 2+2?', ts: Date.now() })
        useStore.setState({ streaming: true, streamingMsgId: null })
      }
      s.handleEnvelope(JSON.parse(line) as Envelope)
    })

    const assistants = useStore.getState().messages.filter((m) => m.role === 'assistant')
    expect(assistants).toHaveLength(1)
    expect(assistants[0].text).toBe('. cool1819 steady.\n shy clean small\n\n sharp.\n23')
    // The badge the orphaned bubble never got.
    expect(assistants[0].answerMs).toBeDefined()
    // The user's prompt is still in the transcript, after the answer bubble that
    // was already open when they sent it — which is where it happened.
    expect(useStore.getState().messages.map((m) => m.role)).toEqual(['assistant', 'user'])
    // And tether#80's line is still shown: keeping the turn must not cost the
    // agent's own complaint about the prompt it dropped.
    expect(useStore.getState().notices.map((n) => n.text)).toEqual([
      'The agent reported an error — busy: another prompt is running',
    ])
  })
})

// tether#80 — the cascading half of adding a class to Notice: tether#50's banner
// collapse used to compare the LAST notice's text outright, so any unrelated line
// arriving in between un-gated it. tether#80's line can now be that unrelated
// line, and often, so the comparison is scoped to the last SESSION banner.
describe('the session-notice collapse survives an interposed line (tether#50, tether#80)', () => {
  beforeEach(() => { reset(); useStore.setState({ fatal: null }) })
  afterEach(() => { reset(); useStore.setState({ fatal: null }) })

  const noticeEnv = (text: string): Envelope => ({ kind: 'message', payload: { type: 'notice', text } })
  const banner = 'Started a new session — the previous conversation\'s context could not be restored.'

  it('still collapses an identical banner after an agent-error line has landed', () => {
    const h = useStore.getState().handleEnvelope
    h(noticeEnv(banner))
    h(errorEnv({ code: 'agent_error', message: 'busy: another prompt is running', terminal: false }))
    h(noticeEnv(banner))
    const texts = useStore.getState().notices.map((n) => n.text)
    expect(texts.filter((t) => t === banner)).toHaveLength(1)
    expect(useStore.getState().notices).toHaveLength(2)
  })

  // The other direction: a DIFFERENT banner is new information and must still land
  // (tether#52's rebind wording vs the context-lost wording).
  it('does not collapse a banner that says something different', () => {
    const h = useStore.getState().handleEnvelope
    h(noticeEnv(banner))
    h(noticeEnv('Started a new session in this workspace — the previous conversation belongs to a different workspace and stays there.'))
    expect(useStore.getState().notices).toHaveLength(2)
  })

  // Notice.kind's doc says every production site sets it. That claim needs a test
  // rather than a comment, because an unset class is INVISIBLE today: only the
  // agent-error and session rules read the field, so leaving 'prompt_undelivered'
  // off changes no behaviour at all — a mutation run confirmed that dropping it
  // killed nothing. It would surface only the next time a rule is scoped by kind,
  // which is precisely how tether#50's collapse came to be un-gated by an
  // unrelated line in the first place.
  it('labels every notice with the class its own rules are scoped on', () => {
    const h = useStore.getState().handleEnvelope
    h(noticeEnv(banner))
    h(errorEnv({ code: 'prompt_undelivered', message: 'gone', terminal: false }))
    h(errorEnv({ code: 'agent_error', message: 'busy', terminal: false }))
    expect(useStore.getState().notices.map((n) => n.kind)).toEqual(['session', 'prompt_undelivered', 'agent_error'])
  })
})

// The bounding rules as pure functions, so the cases above assert BEHAVIOUR and
// these assert the RULES — including the ones that are hard to provoke through
// handleEnvelope because they depend on a clock.
describe('appendAgentErrorNotice (tether#80)', () => {
  const mk = (text: string, ts: number, repeats?: number): Notice =>
    ({ id: `id-${text}-${ts}`, text, ts, kind: 'agent_error', ...(repeats ? { repeats } : {}) })

  it('appends a first arrival with a count of one', () => {
    const out = appendAgentErrorNotice([], { id: 'a', text: 'boom', ts: 100 })
    expect(out).toEqual([{ id: 'a', text: 'boom', ts: 100, kind: 'agent_error', repeats: 1 }])
  })

  it('collapses a repeat into the existing entry, keeping its id', () => {
    const out = appendAgentErrorNotice([mk('boom', 100)], { id: 'b', text: 'boom', ts: 200 })
    expect(out).toHaveLength(1)
    expect(out[0]?.id).toBe('id-boom-100')
    expect(out[0]?.repeats).toBe(2)
    expect(out[0]?.ts).toBe(200)
  })

  // ts moves FORWARD only. A refresh stamped from a clock behind the one that
  // stamped the original (mergeTranscript's doc: daemon vs browser clocks, skew
  // unbounded) must not drag the line back above a message it already reads
  // below — which is the whole reason its position is refreshed at all.
  it('never moves a collapsed line backwards in the transcript', () => {
    const out = appendAgentErrorNotice([mk('boom', 5000)], { id: 'b', text: 'boom', ts: 100 })
    expect(out[0]?.ts).toBe(5000)
    expect(out[0]?.repeats).toBe(2)
  })

  it('evicts the least-recently-seen agent line once over the cap', () => {
    let notices: Notice[] = []
    for (let i = 0; i < AGENT_ERROR_NOTICE_LIMIT; i++) {
      notices = appendAgentErrorNotice(notices, { id: `a${i}`, text: `e${i}`, ts: 100 + i })
    }
    // Refresh the OLDEST so it is no longer least-recently-seen.
    notices = appendAgentErrorNotice(notices, { id: 'x', text: 'e0', ts: 9000 })
    notices = appendAgentErrorNotice(notices, { id: 'new', text: 'newcomer', ts: 9001 })
    expect(notices).toHaveLength(AGENT_ERROR_NOTICE_LIMIT)
    expect(notices.map((n) => n.text)).toContain('e0')
    expect(notices.map((n) => n.text)).not.toContain('e1')
  })

  // Found by tether#80's review, and the worst possible defect for this wi: the
  // eviction scan originally ranked the just-appended line alongside the rest, so
  // when every existing line held a LATER ts, the newcomer was the
  // least-recently-seen entry and evicted itself. The newest agent error would
  // never be shown — the exact silence this change exists to remove.
  //
  // That is not a contrived arrangement: nextNoticeTs derives ts from the last
  // message in the transcript, which carries the daemon's clock after a refetch,
  // so lines stamped while a daemon-ahead history was loaded outrank everything
  // stamped after it was replaced. The store-level case for that path is below.
  it('never evicts the line that just arrived, even when every older line is stamped later', () => {
    let notices: Notice[] = []
    for (let i = 0; i < AGENT_ERROR_NOTICE_LIMIT; i++) {
      notices = appendAgentErrorNotice(notices, { id: `old${i}`, text: `old ${i}`, ts: 9000 + i })
    }
    notices = appendAgentErrorNotice(notices, { id: 'new', text: 'the newest failure', ts: 100 })
    expect(notices).toHaveLength(AGENT_ERROR_NOTICE_LIMIT)
    expect(notices.map((n) => n.text)).toContain('the newest failure')
    // …and the one it displaced is the genuinely oldest of the rest.
    expect(notices.map((n) => n.text)).not.toContain('old 0')
  })

  it('never evicts a notice of another class to make room', () => {
    const session: Notice = { id: 'sess', text: 'Started a new session', ts: 1 }
    let notices: Notice[] = [session]
    for (let i = 0; i < AGENT_ERROR_NOTICE_LIMIT + 2; i++) {
      notices = appendAgentErrorNotice(notices, { id: `a${i}`, text: `e${i}`, ts: 100 + i })
    }
    expect(notices).toContainEqual(session)
    expect(notices.filter((n) => n.kind === 'agent_error')).toHaveLength(AGENT_ERROR_NOTICE_LIMIT)
  })
})

describe('nextNoticeTs (tether#77 rule, shared since tether#80)', () => {
  // The clock is pinned rather than read twice, for the reason the ordering case
  // above documents: with a live clock this passes even for a bare Date.now() most
  // of the time, so it would only fail probabilistically and would not be a guard.
  it('is strictly after the last message even when they share a millisecond', () => {
    const frozen = 1_800_000_000_000
    const spy = vi.spyOn(Date, 'now').mockReturnValue(frozen)
    try {
      expect(nextNoticeTs([{ id: 'm', role: 'user', text: 'x', ts: frozen }])).toBe(frozen + 1)
    } finally {
      spy.mockRestore()
    }
  })

  it('is after a transcript carrying a clock from the future', () => {
    const ahead = Date.now() + 60_000
    expect(nextNoticeTs([{ id: 'm', role: 'user', text: 'x', ts: ahead }])).toBe(ahead + 1)
  })

  it('falls back to the wall clock for an empty transcript', () => {
    const before = Date.now()
    expect(nextNoticeTs([])).toBeGreaterThanOrEqual(before)
  })
})

describe('mergeTranscript renders a repeat count (tether#80)', () => {
  it('shows the count above one and nothing at one or absent', () => {
    const msgs: Message[] = []
    const notices: Notice[] = [
      { id: 'a', text: 'once', ts: 1, kind: 'agent_error', repeats: 1 },
      { id: 'b', text: 'twice', ts: 2, kind: 'agent_error', repeats: 2 },
      { id: 'c', text: 'plain', ts: 3 },
    ]
    expect(mergeTranscript(msgs, notices).map((m) => m.text)).toEqual(['once', 'twice (×2)', 'plain'])
  })
})

// tether#106 — loadHistory replaces CONTENT without replacing IDENTITY.
//
// `key={m.id}` is what React reconciles the transcript on, `historyEntryToMessage`
// mints a fresh crypto.randomUUID() for every entry on every load, and both
// "which blocks are expanded" Sets in ChatPane are keyed by message id. So before
// this, every reload remounted the entire transcript: expansions collapsed and the
// scroll container's scrollHeight collapsed mid-commit, clamping scrollTop to the
// top of the conversation. Survivable once per deliberate switch; not survivable
// now that a held session's transcript reloads whenever the other agent writes.
//
// jsdom reports every scroll metric as 0, so the scroll itself is not assertable
// here — the ids ARE the mechanism, and they are.
describe('loadHistory message identity (tether#106)', () => {
  afterEach(reset)

  const hist = (role: Message['role'], text: string, ts: number): Message =>
    ({ id: crypto.randomUUID(), role, text, ts })

  it('keeps the ids of the messages already on screen when one is appended', () => {
    useStore.getState().loadHistory([hist('user', 'ask', 1), hist('assistant', 'answer', 2)])
    const before = useStore.getState().messages.map(m => m.id)

    useStore.getState().loadHistory([
      hist('user', 'ask', 1), hist('assistant', 'answer', 2), hist('user', 'and again', 3),
    ])
    const after = useStore.getState().messages.map(m => m.id)

    expect(after.slice(0, 2)).toEqual(before)
    expect(after).toHaveLength(3)
    expect(before).not.toContain(after[2]) // the appended one is genuinely new
  })

  it('survives a WINDOW THAT SLID, which a positional match cannot', () => {
    // The case this feature actually lives in. CCStore serves the last 200 messages
    // inside a 1 MiB window, so past either bound every append drops the message at
    // index 0 and shifts the rest. A prefix match breaks at i=0 here and reissues
    // every id — i.e. it remounts the whole transcript every three seconds, on
    // exactly the long cc conversations this feature exists to follow.
    useStore.getState().loadHistory([hist('user', 'A', 1), hist('assistant', 'B', 2), hist('user', 'C', 3)])
    const before = useStore.getState().messages
    const idOfB = before[1].id, idOfC = before[2].id

    useStore.getState().loadHistory([hist('assistant', 'B', 2), hist('user', 'C', 3), hist('assistant', 'D', 4)])
    const after = useStore.getState().messages

    expect(after.map(m => m.text)).toEqual(['B', 'C', 'D'])
    expect(after[0].id).toBe(idOfB)
    expect(after[1].id).toBe(idOfC)
    expect(before.map(m => m.id)).not.toContain(after[2].id)
  })

  it('keeps the id of the message that is still GROWING', () => {
    // The other agent's current turn gains text on every write. If text were part of
    // the identity key it would get a new id on every reload — collapsing the
    // thinking block of the one turn anybody is watching, three seconds at a time.
    useStore.getState().loadHistory([hist('user', 'ask', 1), hist('assistant', 'partial', 2)])
    const before = useStore.getState().messages.map(m => m.id)

    useStore.getState().loadHistory([hist('user', 'ask', 1), hist('assistant', 'partial and then some', 2)])
    const after = useStore.getState().messages

    // Same identity, NEW content — this is a reload, not a cache.
    expect(after.map(m => m.id)).toEqual(before)
    expect(after.map(m => m.text)).toEqual(['ask', 'partial and then some'])
  })

  it('never hands one id to two messages', () => {
    // Two messages can share role+ts — a coarse clock, or the same turn re-read — and
    // duplicate React keys in one list are a worse failure than the remount this
    // avoids. Each old id is handed out at most once; the surplus keeps its own.
    useStore.getState().loadHistory([hist('user', 'same', 1), hist('user', 'same', 1)])
    const before = useStore.getState().messages.map(m => m.id)

    useStore.getState().loadHistory([hist('user', 'same', 1), hist('user', 'same', 1), hist('user', 'same', 1)])
    const after = useStore.getState().messages.map(m => m.id)

    expect(new Set(after).size).toBe(3)
    expect(after.slice(0, 2)).toEqual(before)
  })

  it('gives every message a fresh id when nothing matches', () => {
    // A session switch. The old ids belong to a different conversation.
    useStore.getState().loadHistory([hist('user', 'session A', 1), hist('assistant', 'A reply', 2)])
    const before = useStore.getState().messages.map(m => m.id)
    useStore.getState().loadHistory([hist('user', 'session B', 10), hist('assistant', 'B reply', 20)])
    for (const id of useStore.getState().messages.map(m => m.id)) {
      expect(before).not.toContain(id)
    }
  })

  it('still resets the turn state and the permission queue', () => {
    // The identity carry-over must not turn the replace into a merge. loadHistory is
    // also what drops the prior session's pending permission cards and turn cursor
    // (tether#40), and "everything matched" is the case most likely to skip it.
    useStore.getState().loadHistory([hist('user', 'ask', 1)])
    useStore.setState({
      streaming: true, streamingMsgId: 'x', curTurnId: 'x',
      pendingPermissions: [{ id: 'p1', toolName: 'Bash', input: {} }],
    })
    useStore.getState().loadHistory([hist('user', 'ask', 1)])

    const s = useStore.getState()
    expect(s.streaming).toBe(false)
    expect(s.streamingMsgId).toBeNull()
    expect(s.curTurnId).toBeNull()
    expect(s.pendingPermissions).toEqual([])
  })
})

// ────────────────────────────────────────────────────────────────────────────
// tether#107 — prepending an older page, and merging a newer one into a
// transcript that already holds older pages.
//
// The three properties asserted here are the three tether#106 shipped and this wi
// had to keep, restated for the two new reducers:
//
//   1. a message already on screen keeps its id (React reconciles on it, and BOTH
//      expansion Sets are keyed by it);
//   2. nothing that is on screen disappears;
//   3. the live turn is not touched.
//
// None of them is visible in a test that only counts messages, and (1) in particular
// fails silently: the transcript still renders, it just remounts, collapsing every
// expanded block and clamping the reader's scroll.
describe('prependHistory (tether#107)', () => {
  // The shared `reset()` predates these fields, so the page counter has to be cleared
  // here or one test's count leaks into the next one's assertion. Local rather than
  // folded into `reset()`, so nothing outside this wi's tests changes.
  afterEach(() => {
    reset()
    useStore.setState({ transcriptEarlier: null, transcriptOtherRecord: null, transcriptPagesBack: 0 })
  })

  const hist = (role: Message['role'], text: string, ts: number): Message =>
    ({ id: crypto.randomUUID(), role, text, ts })

  /** Seed the transcript with known, stable ids. */
  function seed(...msgs: Message[]): string[] {
    useStore.setState({ messages: msgs })
    return msgs.map(m => m.id)
  }

  it('puts the older page in front, oldest first', () => {
    seed({ id: 'on-screen-1', role: 'user', text: 'the fifth thing', ts: 500 })
    useStore.getState().prependHistory([
      hist('user', 'the third thing', 300),
      hist('assistant', 'the fourth thing', 400),
    ])
    expect(useStore.getState().messages.map(m => m.text))
      .toEqual(['the third thing', 'the fourth thing', 'the fifth thing'])
  })

  it('leaves the id of every message already on screen byte-identical', () => {
    // THE property. Exact ids, not a count: a reducer that rebuilt the array with
    // fresh uuids would keep the length and the order and still remount the whole
    // transcript, collapsing the reader's expanded blocks and their scroll position.
    const kept = seed(
      { id: 'keep-a', role: 'user', text: 'ask', ts: 500 },
      { id: 'keep-b', role: 'assistant', text: 'answer', ts: 600 },
    )
    useStore.getState().prependHistory([hist('user', 'older', 100)])

    const after = useStore.getState().messages
    expect(after.map(m => m.id).slice(1)).toEqual(kept)
    expect(after[0].id).not.toBe('keep-a')
    // …and no duplicate keys anywhere, which is the failure that would be WORSE
    // than the remount this avoids.
    expect(new Set(after.map(m => m.id)).size).toBe(after.length)
  })

  it('does not touch the live turn or the permission queue', () => {
    // loadHistory resets these, because it is the server-truth REPLACE and its array
    // may belong to another session. This adds older history to the session already
    // on screen and reports nothing about the live turn, so a reducer that reset here
    // would let "load earlier messages" cancel the reader's own in-flight turn.
    seed({ id: 'live', role: 'assistant', text: 'mid-turn', ts: 900 })
    useStore.setState({
      streaming: true, streamingMsgId: 'live', curTurnId: 'live', answerStartTs: 800,
      pendingPermissions: [{ id: 'p1', toolName: 'Bash', input: {} }],
    })
    useStore.getState().prependHistory([hist('user', 'older', 100)])

    const s = useStore.getState()
    expect(s.streaming).toBe(true)
    expect(s.streamingMsgId).toBe('live')
    expect(s.curTurnId).toBe('live')
    expect(s.answerStartTs).toBe(800)
    expect(s.pendingPermissions).toHaveLength(1)
  })

  it('counts the pages the reader has gone back', () => {
    // The refresh path branches on this, so it is behaviour rather than bookkeeping.
    expect(useStore.getState().transcriptPagesBack).toBe(0)
    seed({ id: 'on-screen', role: 'user', text: 'newest', ts: 500 })
    useStore.getState().prependHistory([hist('user', 'p1', 400)])
    expect(useStore.getState().transcriptPagesBack).toBe(1)
    useStore.getState().prependHistory([hist('user', 'p2', 300)])
    expect(useStore.getState().transcriptPagesBack).toBe(2)
    // …and loadHistory, being the wholesale replace, puts it back to zero: the array
    // it installs is one page, so no earlier page is on screen any more.
    useStore.getState().loadHistory([hist('user', 'fresh', 900)])
    expect(useStore.getState().transcriptPagesBack).toBe(0)
  })

  it('drops an entry the transcript already holds rather than duplicating a key', () => {
    seed({ id: 'dupe-target', role: 'user', text: 'same turn', ts: 500 })
    useStore.getState().prependHistory([
      hist('user', 'genuinely older', 100),
      hist('user', 'same turn', 500),
    ])
    const after = useStore.getState().messages
    expect(after.map(m => m.text)).toEqual(['genuinely older', 'same turn'])
    expect(after[1].id).toBe('dupe-target')
  })

  it('is a no-op for an empty page', () => {
    const kept = seed({ id: 'only', role: 'user', text: 'hello', ts: 1 })
    useStore.getState().prependHistory([])
    expect(useStore.getState().messages.map(m => m.id)).toEqual(kept)
    expect(useStore.getState().transcriptPagesBack).toBe(0)
  })
})

// tether#116 — one turn, one bubble, even when a page boundary lands inside it.
//
// The reported defect: scrolling back through a 148 MB session produced a new "tether"
// header per page (six in eighteen seconds, the daemon's log showing count=1 for each
// 1 MiB page) because the daemon merges consecutive assistant records per WINDOW and
// tether#107 made every page its own window. The seam is the client's to close; these
// pin both halves of that — what it joins, and the four shapes it must refuse instead.
describe('joining a turn split by a page boundary (tether#116)', () => {
  afterEach(() => {
    reset()
    useStore.setState({ transcriptEarlier: null, transcriptOtherRecord: null, transcriptPagesBack: 0 })
  })

  const ai = (text: string, ts: number, extra: Partial<Message> = {}): Message =>
    ({ id: crypto.randomUUID(), role: 'assistant', text, ts, ...extra })
  const user = (text: string, ts: number): Message =>
    ({ id: crypto.randomUUID(), role: 'user', text, ts })
  const tool = (id: string) => ({ id, name: 'Bash', input: {} })

  it('joins the seam into one bubble when both sides of it are assistant', () => {
    useStore.setState({ messages: [ai('second half', 600)] })
    useStore.getState().prependHistory([user('go', 100), ai('first half', 200)])

    const after = useStore.getState().messages
    expect(after.map(m => m.role)).toEqual(['user', 'assistant'])
    expect(after[1].text).toBe(`first half${TURN_JOIN}second half`)
  })

  it('separates the two fragments with a BLANK line, not a newline', () => {
    // A lone "\n" is a CommonMark soft break and renders as a SPACE, so it would run the
    // end of one fragment onto the start of the next — which is why this asserts the
    // string rather than "contains both texts". Same constant as the daemon's ccTurnJoin.
    expect(TURN_JOIN).toBe('\n\n')
    const joined = joinTurnAcrossPages(ai('查清楚:', 100), ai('先看进度:', 200))
    expect(joined?.text).toBe('查清楚:\n\n先看进度:')
  })

  it('keeps the ON-SCREEN id, so the reader\'s expansions and scroll survive', () => {
    // THE property, and the reason identity comes from the newer half: expandedBlocks and
    // expandedThinking are Sets keyed by message id, and `key={m.id}` is React's handle on
    // the row. A join that minted a fresh id would remount the bubble the reader is looking
    // at — the damage tether#106 removed from the reload path.
    useStore.setState({ messages: [{ id: 'on-screen', role: 'assistant', text: 'second', ts: 600 }] })
    useStore.getState().prependHistory([ai('first', 200)])

    const after = useStore.getState().messages
    expect(after).toHaveLength(1)
    expect(after[0].id).toBe('on-screen')
  })

  it('takes the OLDER half\'s ts and ord, because a bubble carries its first fragment\'s', () => {
    // Not cosmetic: mergeHistory indexes the transcript BY ord, so a merged bubble carrying
    // the newer fragment's position would let a later refresh match the wrong slot.
    useStore.setState({ messages: [ai('second', 600, { ord: 900 })] })
    useStore.getState().prependHistory([ai('first', 200, { ord: 300 })])

    const after = useStore.getState().messages
    expect(after).toHaveLength(1)
    expect(after[0].ts).toBe(200)
    expect(after[0].ord).toBe(300)
  })

  it('concatenates the tool calls, older page first', () => {
    useStore.setState({ messages: [ai('second', 600, { tools: [tool('c'), tool('d')] })] })
    useStore.getState().prependHistory([ai('first', 200, { tools: [tool('a'), tool('b')] })])

    expect(useStore.getState().messages[0].tools?.map(t => t.id)).toEqual(['a', 'b', 'c', 'd'])
  })

  it('leaves the seam alone when the on-screen side of it is a user message', () => {
    useStore.setState({ messages: [user('next question', 600)] })
    useStore.getState().prependHistory([ai('an answer', 200)])
    expect(useStore.getState().messages.map(m => m.text)).toEqual(['an answer', 'next question'])
  })

  it('leaves the seam alone when the older side of it is a user message', () => {
    useStore.setState({ messages: [ai('an answer', 600)] })
    useStore.getState().prependHistory([user('a question', 200)])
    expect(useStore.getState().messages.map(m => m.text)).toEqual(['a question', 'an answer'])
  })

  it('refuses when the older half carries thinking, rather than dropping it', () => {
    // Cannot occur today — MessagePage only sets HasEarlier for the cc source, and the cc
    // parser sets neither field — but "cannot happen" is a property of another file, and
    // the cost of being wrong is a silently missing thinking block.
    expect(joinTurnAcrossPages(ai('first', 200, { thinking: 'reasoning' }), ai('second', 600))).toBeNull()
  })

  it('refuses when the older half carries a fenced block, rather than dropping it', () => {
    const block = { blockId: 'b1', lang: 'md', body: 'card' } as unknown as Message['block']
    expect(joinTurnAcrossPages(ai('first', 200, { block }), ai('second', 600))).toBeNull()
  })

  it('refuses when only one half carries an ord', () => {
    // There is no correct ord to give the result, and inventing one puts a wrong key into
    // mergeHistory's index. Both directions, because only one of them is the obvious one.
    expect(joinTurnAcrossPages(ai('first', 200), ai('second', 600, { ord: 900 }))).toBeNull()
    expect(joinTurnAcrossPages(ai('first', 200, { ord: 300 }), ai('second', 600))).toBeNull()
  })

  it('joins when NEITHER half carries an ord, and leaves the key absent', () => {
    const joined = joinTurnAcrossPages(ai('first', 200), ai('second', 600))
    expect(joined).not.toBeNull()
    // `ord: undefined` is a present key; historyEntryToMessage is deliberate about the
    // difference and hasOrd is what reads it.
    expect('ord' in (joined as Message)).toBe(false)
    expect(hasOrd(joined as Message)).toBe(false)
  })

  it('shrinks the transcript by exactly one bubble and mints no duplicate id', () => {
    useStore.setState({ messages: [ai('second', 600), user('later', 700)] })
    useStore.getState().prependHistory([user('go', 100), ai('first', 200)])

    const after = useStore.getState().messages
    expect(after).toHaveLength(3) // 2 on screen + 2 prepended - 1 joined
    expect(new Set(after.map(m => m.id)).size).toBe(after.length)
    expect(after.map(m => m.text)).toEqual(['go', `first${TURN_JOIN}second`, 'later'])
  })

  it('still counts the page the reader went back', () => {
    useStore.setState({ messages: [ai('second', 600)] })
    useStore.getState().prependHistory([ai('first', 200)])
    expect(useStore.getState().transcriptPagesBack).toBe(1)
  })

  it('dedupes BEFORE joining, so a page whose tail is on screen is not joined to its own duplicate', () => {
    // Order matters: seaming the raw page would fold the on-screen bubble into a copy of
    // itself, doubling its text.
    useStore.setState({ messages: [{ id: 'held', role: 'assistant', text: 'same turn', ts: 600 }] })
    useStore.getState().prependHistory([user('go', 100), ai('same turn', 600)])

    const after = useStore.getState().messages
    expect(after.map(m => m.text)).toEqual(['go', 'same turn'])
    expect(after[1].id).toBe('held')
  })

  it('does not touch the live turn while joining', () => {
    useStore.setState({ messages: [{ id: 'live', role: 'assistant', text: 'mid-turn', ts: 900 }] })
    useStore.setState({ streaming: true, streamingMsgId: 'live', curTurnId: 'live', answerStartTs: 800 })
    useStore.getState().prependHistory([ai('earlier fragment', 200)])

    const s = useStore.getState()
    expect(s.streaming).toBe(true)
    expect(s.streamingMsgId).toBe('live')
    expect(s.curTurnId).toBe('live')
    expect(s.messages[0].id).toBe('live')
  })

  it('collapses SIX pages of one turn into one bubble — the reported symptom', () => {
    // The shape the owner saw: one "继续", then a turn long enough that six consecutive
    // 1 MiB pages each held a single fragment of it.
    useStore.setState({ messages: [ai('fragment 6', 600)] })
    for (const [text, ts] of [['fragment 5', 500], ['fragment 4', 400], ['fragment 3', 300],
                              ['fragment 2', 200], ['fragment 1', 100]] as const) {
      useStore.getState().prependHistory([ai(text, ts)])
    }

    const after = useStore.getState().messages
    expect(after).toHaveLength(1)
    expect(after[0].role).toBe('assistant')
    expect(after[0].ts).toBe(100)
    expect(after[0].text).toBe(
      ['fragment 1', 'fragment 2', 'fragment 3', 'fragment 4', 'fragment 5', 'fragment 6'].join(TURN_JOIN),
    )
    expect(useStore.getState().transcriptPagesBack).toBe(5)
  })

  it('simply prepends when the transcript is empty, with nothing to seam against', () => {
    useStore.setState({ messages: [] })
    useStore.getState().prependHistory([ai('only', 200)])
    expect(useStore.getState().messages.map(m => m.text)).toEqual(['only'])
  })
})

describe('mergeHistory (tether#107)', () => {
  afterEach(() => {
    reset()
    useStore.setState({ transcriptEarlier: null, transcriptOtherRecord: null, transcriptPagesBack: 0 })
  })

  // `ord` defaults to `ts` so that the tether#107 cases below read as they were
  // written. That default is a CONVENIENCE OF THIS FIXTURE and not a fact about the
  // wire — the daemon's ord is a file position and its ts is a clock, and tether#109's
  // own tests below pass the two separately on purpose, because a fixture in which they
  // move together cannot express the bug (that is exactly how the re-cut bubble got
  // through: its ts moved while the conversation did not).
  const hist = (role: Message['role'], text: string, ts: number, ord: number = ts): Message =>
    ({ id: crypto.randomUUID(), role, text, ts, ord })

  it('keeps the older pages, updates the overlap in place, and appends what is new', () => {
    // The shape the three-second probe produces once the reader has paged back: the
    // daemon's newest window covers the tail of what is on screen plus one new turn.
    useStore.setState({
      messages: [
        { id: 'older-1', role: 'user', text: 'page one, turn one', ts: 100, ord: 100 },
        { id: 'newer-1', role: 'user', text: 'ask', ts: 500, ord: 500 },
        { id: 'newer-2', role: 'assistant', text: 'partial ans', ts: 600, ord: 600 },
      ],
      transcriptPagesBack: 1,
    })

    const ok = useStore.getState().mergeHistory([
      hist('user', 'ask', 500),
      hist('assistant', 'partial answer, now complete', 600),
      hist('user', 'and one more', 700),
    ])
    expect(ok).toBe(true)

    const after = useStore.getState().messages
    // Nothing removed — the page the reader loaded is still there.
    expect(after.map(m => m.text)).toEqual([
      'page one, turn one',
      'ask',
      'partial answer, now complete',
      'and one more',
    ])
    // …and identity survived for everything that was already on screen, including the
    // GROWING turn, whose text changed. That is why messageKey excludes text.
    expect(after[0].id).toBe('older-1')
    expect(after[1].id).toBe('newer-1')
    expect(after[2].id).toBe('newer-2')
    expect(new Set(after.map(m => m.id)).size).toBe(4)
  })

  it('reports false when the new window does not overlap what is on screen', () => {
    // Over a megabyte written between two three-second probes: the daemon's window now
    // starts after the loaded array ended. A merge would splice two disjoint stretches
    // together with an invisible hole between them, so this refuses and the caller
    // falls back to the visible reset.
    useStore.setState({
      messages: [{ id: 'stale', role: 'user', text: 'long ago', ts: 100, ord: 100 }],
      transcriptPagesBack: 2,
    })
    const ok = useStore.getState().mergeHistory([hist('user', 'much later', 90000)])
    expect(ok).toBe(false)
    // And it changed NOTHING: refusing has to leave the decision entirely to the
    // caller, or the fallback would land on a half-merged array.
    expect(useStore.getState().messages.map(m => m.id)).toEqual(['stale'])
    expect(useStore.getState().transcriptPagesBack).toBe(2)
  })

  it('installs into an EMPTY transcript rather than calling that disjoint', () => {
    useStore.setState({ messages: [] })
    expect(useStore.getState().mergeHistory([hist('user', 'first ever', 1)])).toBe(true)
    expect(useStore.getState().messages.map(m => m.text)).toEqual(['first ever'])
  })

  it('drops a field the daemon has stopped sending instead of letting it linger', () => {
    useStore.setState({
      messages: [{ id: 'keep', role: 'assistant', text: 'ans', ts: 600, ord: 600, thinking: 'was thinking' }],
    })
    useStore.getState().mergeHistory([{ id: 'fresh', role: 'assistant', text: 'ans', ts: 600, ord: 600 }])
    const after = useStore.getState().messages
    expect(after).toHaveLength(1)
    expect(after[0].id).toBe('keep')
    expect(after[0].thinking).toBeUndefined()
  })

  it('is a no-op for an empty page, and reports success', () => {
    // An empty newest page is not a disjoint window, it is a transcript with nothing
    // new in it. Reporting false would send the caller to loadHistory([]), which
    // would blank a transcript the reader has pages of.
    useStore.setState({ messages: [{ id: 'only', role: 'user', text: 'hello', ts: 1 }] })
    expect(useStore.getState().mergeHistory([])).toBe(true)
    expect(useStore.getState().messages.map(m => m.id)).toEqual(['only'])
  })
})

// ────────────────────────────────────────────────────────────────────────────
// tether#109 — mergeHistory CHECKS the order tether#107 assumed.
//
// The counter-example that made this necessary was on the owner's screen: two
// consecutive bubbles whose lower one read 3h16m EARLIER than the upper one. It was
// reproduced by replaying the reported 125 MB cc transcript through the real window
// rule — 36 of 1,031 consecutive single-record appends do it, worst case 3h36m — and
// the mechanism is neither of the two the wi first proposed:
//
//   the window slid FORWARD by 1,600 bytes; the assistant turn straddling its leading
//   edge lost its first fragment; CCStore stamps a merged turn with its FIRST
//   fragment's time, so the SAME turn came back stamped 03:41:49 instead of 03:41:45
//   — a messageKey never seen before, appended at the end, under a bubble from 07:18.
//
// The window start did not move backwards once in 1,053 samples, so nothing here needs
// the widen-once retry or the message cap. Every fixture below therefore keeps `ord`
// and `ts` INDEPENDENT: a fixture in which they move together is one in which this bug
// cannot be written down.
//
// The ords are the shape the real ones have — byte positions a megabyte apart — and the
// timestamps are the real ones, in ms, so the numbers here are the measurement rather
// than a restatement of it.
describe('mergeHistory checks the order it used to assume (tether#109)', () => {
  afterEach(() => {
    reset()
    useStore.setState({ transcriptEarlier: null, transcriptOtherRecord: null, transcriptPagesBack: 0 })
  })

  // 2026-08-19, the reproduced case, in ms.
  const TS_EDGE_FULL = Date.parse('2026-08-19T03:41:45.555Z')
  const TS_EDGE_RECUT = Date.parse('2026-08-19T03:41:49.404Z')
  const TS_MIDDLE = Date.parse('2026-08-19T04:50:48.478Z')
  const TS_NEWEST = Date.parse('2026-08-19T07:18:29.315Z')

  /** The transcript a reader of that session had on screen, with one page loaded. */
  function seedReproduction(): void {
    useStore.setState({
      messages: [
        // The leading-edge bubble of the page that was newest when it was fetched: the
        // WHOLE turn, starting at the first fragment the window then contained.
        { id: 'edge', role: 'assistant', text: 'the whole turn, first fragment onwards', ts: TS_EDGE_FULL, ord: 122154092 },
        { id: 'middle', role: 'user', text: 'a question an hour later', ts: TS_MIDDLE, ord: 122378593 },
        { id: 'newest', role: 'assistant', text: 'the newest turn', ts: TS_NEWEST, ord: 123197925 },
      ],
      transcriptPagesBack: 1,
    })
  }

  it('does not append the re-cut leading-edge bubble at the end', () => {
    // THE regression. On the code this replaces the array ends
    // [.., 'the newest turn', 'the same turn, minus its first fragment'] — 3h36m out of
    // order, which is what the owner photographed.
    seedReproduction()

    const ok = useStore.getState().mergeHistory([
      // Same turn, one record later. Its ord is INSIDE the span already on screen, so
      // its bytes are already rendered inside the bubble above that swallowed them.
      { id: 'recut', role: 'assistant', text: 'minus its first fragment', ts: TS_EDGE_RECUT, ord: 122155976 },
      { id: 'm2', role: 'user', text: 'a question an hour later', ts: TS_MIDDLE, ord: 122378593 },
      { id: 'n2', role: 'assistant', text: 'the newest turn', ts: TS_NEWEST, ord: 123197925 },
    ])

    // TRUE, not false: the reader keeps the page they loaded. Refusing here would be
    // correct-but-useless — it fires on 3.5% of appends, i.e. every few seconds on an
    // active session, and each refusal throws away the pages tether#107 exists to keep.
    expect(ok).toBe(true)
    const after = useStore.getState().messages
    // Exact texts, in order. A length check would pass on an array that kept the
    // re-cut bubble and dropped something else.
    expect(after.map(m => m.text)).toEqual([
      'the whole turn, first fragment onwards',
      'a question an hour later',
      'the newest turn',
    ])
    // The ordering key is monotonic — the property the whole wi is about, stated on the
    // thing that is actually ordered rather than on the timestamps, which cc itself
    // writes out of order (6 reversals up to 4.5 minutes in the same tail).
    expect(after.map(m => m.ord)).toEqual([122154092, 122378593, 123197925])
    // …and identity survived, so React reconciles instead of remounting: the reader
    // keeps their expanded blocks and their scroll position.
    expect(after.map(m => m.id)).toEqual(['edge', 'middle', 'newest'])
  })

  it('keeps appending what is genuinely newer while it skips the re-cut bubble', () => {
    // The two decisions happen in ONE merge on the real path, and a test that only
    // showed the skip would pass on a reducer that had stopped appending anything.
    seedReproduction()

    const ok = useStore.getState().mergeHistory([
      { id: 'recut', role: 'assistant', text: 'minus its first fragment', ts: TS_EDGE_RECUT, ord: 122155976 },
      { id: 'm2', role: 'user', text: 'a question an hour later', ts: TS_MIDDLE, ord: 122378593 },
      { id: 'n2', role: 'assistant', text: 'the newest turn, now longer', ts: TS_NEWEST, ord: 123197925 },
      { id: 'brand-new', role: 'user', text: 'what the other agent just wrote', ts: TS_NEWEST + 60000, ord: 123205000 },
    ])

    expect(ok).toBe(true)
    const after = useStore.getState().messages
    expect(after.map(m => m.text)).toEqual([
      'the whole turn, first fragment onwards',
      'a question an hour later',
      'the newest turn, now longer', // the growing turn was updated in place…
      'what the other agent just wrote', // …and the new one is at the end
    ])
    expect(after.map(m => m.id)).toEqual(['edge', 'middle', 'newest', 'brand-new'])
    expect(new Set(after.map(m => m.id)).size).toBe(4)
  })

  it('refuses when the window really did move backwards, rather than folding it in', () => {
    // widen-once: ccReadTail retries a 1 MiB window that holds no conversation with a
    // 16 MiB one. Measured on the same transcript, it fires on 9 of 1,053 sampled sizes
    // and starts the page 15.6 MiB earlier — so this is the case the wi PREDICTED, and
    // it is real even though it is not the one in the screenshot.
    //
    // Refused rather than skipped: content below everything on screen is content the
    // reader has not got, and prepending it here would need a guarantee that the two
    // ranges meet, which nothing on this side has.
    seedReproduction()

    const ok = useStore.getState().mergeHistory([
      { id: 'way-older', role: 'user', text: '15.6 MiB earlier', ts: Date.parse('2026-08-19T00:51:49.998Z'), ord: 105904904 },
      { id: 'm2', role: 'user', text: 'a question an hour later', ts: TS_MIDDLE, ord: 122378593 },
      { id: 'n2', role: 'assistant', text: 'the newest turn', ts: TS_NEWEST, ord: 123197925 },
    ])

    expect(ok).toBe(false)
    // Nothing moved. A half-merged array would leave the caller's fallback landing on a
    // transcript this reducer had already damaged.
    expect(useStore.getState().messages.map(m => m.id)).toEqual(['edge', 'middle', 'newest'])
    expect(useStore.getState().transcriptPagesBack).toBe(1)
  })

  it('refuses when a message on screen carries no position to compare against', () => {
    // The premise is then not checkable, and checking it is this reducer's job. Reachable
    // for a bubble the BROWSER made (handleEnvelope) rather than one the daemon served.
    useStore.setState({
      messages: [
        { id: 'served', role: 'user', text: 'from the daemon', ts: 100, ord: 4096 },
        { id: 'local', role: 'assistant', text: 'a live bubble, never recorded', ts: 200 },
      ],
      transcriptPagesBack: 1,
    })
    expect(useStore.getState().mergeHistory([
      { id: 'x', role: 'user', text: 'from the daemon', ts: 100, ord: 4096 },
    ])).toBe(false)
    expect(useStore.getState().messages.map(m => m.id)).toEqual(['served', 'local'])
  })

  it('refuses a page from a daemon that does not report positions at all', () => {
    // The wire is hand-mirrored, so "the field is there" is a claim about a running
    // binary rather than about a type. An SPA newer than its daemon takes the visible
    // reset instead of guessing, which is the same answer every other unverifiable case
    // gets.
    useStore.setState({
      messages: [{ id: 'served', role: 'user', text: 'ask', ts: 100, ord: 4096 }],
      transcriptPagesBack: 1,
    })
    expect(useStore.getState().mergeHistory([
      { id: 'x', role: 'user', text: 'ask', ts: 100 },
    ])).toBe(false)
    expect(useStore.getState().messages.map(m => m.id)).toEqual(['served'])
  })

  it('refuses a position that arrives as JSON null rather than treating it as zero', () => {
    // `ord` crosses JSON, and null is the shape a hand-mirrored optional number takes
    // when the other side stops filling it in. Written with a cast because the TYPE
    // cannot express what the wire can send, which is the entire reason the check exists.
    //
    // The null is on the HELD side deliberately, and that placement is the only version
    // of this test that discriminates. Measured: a null on the INCOMING side is refused
    // even with the gate deleted, because `null > 1` and `null >= 1` are both false in
    // JavaScript and the message falls through to the last refusal — i.e. that arrangement
    // would pass on a reducer with no gate at all, and an earlier draft of this test did
    // exactly that. Held, it is different: an entry that fails `Number.isFinite` but
    // passes `!== undefined` joins the position index and drags `lowest` down to a
    // coerced zero, so the span the interior case is measured against is silently wrong.
    useStore.setState({
      messages: [
        { id: 'served', role: 'user', text: 'ask', ts: 100, ord: 4096 },
        { id: 'nulled', role: 'assistant', text: 'answer', ts: 200, ord: null as unknown as number },
      ],
      transcriptPagesBack: 1,
    })
    expect(useStore.getState().mergeHistory([
      { id: 'x', role: 'user', text: 'ask', ts: 100, ord: 4096 },
      { id: 'y', role: 'user', text: 'newer', ts: 300, ord: 9000 },
    ])).toBe(false)
    expect(useStore.getState().messages.map(m => m.id)).toEqual(['served', 'nulled'])
  })

  it('refuses a page whose APPENDED TAIL is not in position order', () => {
    // Cannot happen from this daemon — both readers emit in file order. It is checked
    // because "the page is sorted" is precisely the kind of assumption this wi exists to
    // stop making, and because the alternative is that a scrambled page is appended
    // scrambled.
    //
    // The name says APPENDED TAIL and the fixture puts the disorder there, because that is
    // all the check looks at and review found the reducer's comment claiming more: a page
    // arriving as [3000, 1000, 2000] against those three ords on screen merges happily,
    // and correctly — an in-place update and a skip are both order-independent. Asserted
    // below, so the narrow claim is the tested one.
    useStore.setState({
      messages: [{ id: 'a', role: 'user', text: 'on screen', ts: 100, ord: 1000 }],
      transcriptPagesBack: 1,
    })
    expect(useStore.getState().mergeHistory([
      { id: 'a2', role: 'user', text: 'on screen', ts: 100, ord: 1000 },
      { id: 'b', role: 'user', text: 'newer', ts: 300, ord: 3000 },
      { id: 'c', role: 'user', text: 'newer still, out of order', ts: 200, ord: 2000 },
    ])).toBe(false)
    expect(useStore.getState().messages.map(m => m.id)).toEqual(['a'])
  })

  it('merges a page whose MATCHED entries arrive shuffled, because those are order-free', () => {
    // The other side of the check above, and the reason it is stated narrowly. Nothing
    // produces this either, but a reducer that refused here would be refusing something
    // harmless — and the refusal costs the reader their pages.
    useStore.setState({
      messages: [
        { id: 'a', role: 'user', text: 'one', ts: 100, ord: 1000 },
        { id: 'b', role: 'assistant', text: 'two', ts: 200, ord: 2000 },
        { id: 'c', role: 'user', text: 'three', ts: 300, ord: 3000 },
      ],
      transcriptPagesBack: 1,
    })
    expect(useStore.getState().mergeHistory([
      { id: 'x', role: 'user', text: 'three, updated', ts: 300, ord: 3000 },
      { id: 'y', role: 'user', text: 'one, updated', ts: 100, ord: 1000 },
      { id: 'z', role: 'assistant', text: 'two, updated', ts: 200, ord: 2000 },
    ])).toBe(true)
    const after = useStore.getState().messages
    // Positions are the array's, contents are the page's, ids are the array's.
    expect(after.map(m => m.text)).toEqual(['one, updated', 'two, updated', 'three, updated'])
    expect(after.map(m => m.id)).toEqual(['a', 'b', 'c'])
  })

  it('refuses an interior position that is NOT an assistant bubble', () => {
    // The role check on the skip. Only a run of ASSISTANT records merges in CCStore, so
    // only an assistant bubble's position can move when the window slides. A user record
    // is its own bubble at its own offset in every window: if its position is inside a
    // CONTIGUOUS span on screen then it is on screen and would have matched. So
    // "interior, unmatched, not assistant" is proof that the span is not contiguous, and
    // the honest answer to that is the visible reset rather than a skipped bubble.
    //
    // Without the role check this message is skipped silently — a bubble the reader never
    // sees, in the middle of a conversation, with nothing to suggest it.
    useStore.setState({
      messages: [
        { id: 'a', role: 'assistant', text: 'oldest on screen', ts: 100, ord: 1000 },
        { id: 'b', role: 'user', text: 'newest on screen', ts: 300, ord: 3000 },
      ],
      transcriptPagesBack: 1,
    })
    expect(useStore.getState().mergeHistory([
      { id: 'x', role: 'assistant', text: 'oldest on screen', ts: 100, ord: 1000 },
      { id: 'y', role: 'user', text: 'a user turn nobody has', ts: 200, ord: 2000 },
      { id: 'z', role: 'user', text: 'newest on screen', ts: 300, ord: 3000 },
    ])).toBe(false)
    expect(useStore.getState().messages.map(m => m.id)).toEqual(['a', 'b'])

    // The CONTROL: the same shape with the same position, as an ASSISTANT bubble, is the
    // re-cut case and IS skipped. Without this the test above would also pass on a
    // reducer that had stopped skipping anything.
    expect(useStore.getState().mergeHistory([
      { id: 'x', role: 'assistant', text: 'oldest on screen', ts: 100, ord: 1000 },
      { id: 'y', role: 'assistant', text: 'a re-cut suffix', ts: 200, ord: 2000 },
      { id: 'z', role: 'user', text: 'newest on screen', ts: 300, ord: 3000 },
    ])).toBe(true)
    expect(useStore.getState().messages.map(m => m.id)).toEqual(['a', 'b'])
  })

  it('cannot merge one session\'s page into another session\'s transcript', () => {
    // Found by review, and it is arithmetic rather than bad luck: `ord` is 1-based and
    // both daemon stores number from the beginning of their own record, so `ord === 1` is
    // in EVERY page that reaches byte 0 — which for tether's own store is every page,
    // since LoadHistory reads the whole file. One matching position is all mergeHistory
    // needs to report success, after which every other position in the arriving page
    // lands inside the previous session's span and (as an assistant bubble) is skipped.
    // Session A's transcript would then be displayed under session B.
    //
    // The fix is upstream of this reducer: `transcriptPagesBack` describes the pages of
    // ONE session, so `setSessionId` retires it on a change and the refresh takes the
    // wholesale replace instead. Asserted on the reducer's INPUT rather than on
    // mergeHistory, because the merge itself cannot tell two sessions apart — nothing on
    // a message says which session it came from — so the only checkable statement is that
    // the flag which permits merging does not survive the switch.
    useStore.setState({
      sessionId: 'session-A',
      messages: [
        { id: 'a1', role: 'user', text: "A's first turn", ts: 100, ord: 1 },
        { id: 'a2', role: 'assistant', text: "A's answer", ts: 200, ord: 900 },
      ],
      transcriptPagesBack: 1,
    })
    useStore.getState().setSessionId('session-B')
    expect(useStore.getState().transcriptPagesBack).toBe(0)

    // Re-announcing the SAME sid is not a switch (handleEnvelope's session_ready calls
    // setSessionId with the sid it already has, to persist it), so the reader sitting in
    // one session keeps their pages.
    useStore.setState({ transcriptPagesBack: 2 })
    useStore.getState().setSessionId('session-B')
    expect(useStore.getState().transcriptPagesBack).toBe(2)

    // The two facts that are deliberately NOT reset: they come off the response that
    // installs the messages they describe, and the switch's own refresh records them a
    // moment later. Clearing them here would print "this is the beginning of the
    // conversation" over the outgoing session's messages for one frame.
    useStore.setState({ transcriptEarlier: 4096, transcriptOtherRecord: 'cc' })
    useStore.getState().setSessionId('session-C')
    expect(useStore.getState().transcriptEarlier).toBe(4096)
    expect(useStore.getState().transcriptOtherRecord).toBe('cc')
  })

  it('does not install an array the caller still holds', () => {
    // The empty-transcript branch is the only exit that could hand the store its
    // caller's array. A store sharing an array with a caller can change without
    // notifying any subscriber, which renders as a transcript that is right in the
    // devtools and stale on screen.
    useStore.setState({ messages: [], transcriptPagesBack: 1 })
    const page: Message[] = [{ id: 'a', role: 'user', text: 'installed', ts: 1, ord: 1 }]
    expect(useStore.getState().mergeHistory(page)).toBe(true)
    page.push({ id: 'b', role: 'user', text: 'never sent to the store', ts: 2, ord: 2 })
    expect(useStore.getState().messages.map(m => m.text)).toEqual(['installed'])
  })

  it('keeps two messages that share role+ts apart, because the position is unique', () => {
    // The secondary defect the wi measured: messageKey is role+ts, and the map behind it
    // kept only the FIRST index per key, so two messages sharing one key had both
    // updates land in one slot — the second overwriting the first's CONTENT while the
    // other bubble kept stale text.
    //
    // Measured honestly, this is a hazard rather than an observed event: across 1,096
    // pages of the reported transcript (15,076 served messages) the collision rate among
    // SERVED messages is 0. The wi's "1 in 97" was counted over raw cc RECORDS, and a
    // run of assistant records becomes one message — so that figure is over a population
    // messageKey is never applied to.
    useStore.setState({
      messages: [
        { id: 'first', role: 'assistant', text: 'stale A', ts: 700, ord: 5000 },
        { id: 'second', role: 'assistant', text: 'stale B', ts: 700, ord: 6000 },
      ],
      transcriptPagesBack: 1,
    })
    expect(useStore.getState().mergeHistory([
      { id: 'x', role: 'assistant', text: 'fresh A', ts: 700, ord: 5000 },
      { id: 'y', role: 'assistant', text: 'fresh B', ts: 700, ord: 6000 },
    ])).toBe(true)
    const after = useStore.getState().messages
    expect(after.map(m => m.text)).toEqual(['fresh A', 'fresh B'])
    expect(after.map(m => m.id)).toEqual(['first', 'second'])
  })

  it('installs a page into an empty transcript without asking for positions', () => {
    // One page is one contiguous range whatever is in it, so there is nothing to check —
    // and refusing would send the caller to loadHistory for the ordinary case of a
    // transcript that has not loaded yet.
    useStore.setState({ messages: [], transcriptPagesBack: 1 })
    expect(useStore.getState().mergeHistory([
      { id: 'a', role: 'user', text: 'no ord anywhere', ts: 1 },
    ])).toBe(true)
    expect(useStore.getState().messages.map(m => m.text)).toEqual(['no ord anywhere'])
  })
})

describe('historyEntryToMessage carries the position (tether#109)', () => {
  it('copies ord off the entry, and leaves it absent when the daemon sent none', () => {
    // Both halves of the two-place check this file's Message doc describes: the Go field
    // exists AND this function copies it. Neither alone is enough — a field on the wire
    // that this drops is a field mergeHistory can never see, and the symptom would be a
    // permanent silent fallback to loadHistory rather than an error.
    expect(historyEntryToMessage({ role: 'user', text: 'hi', ts: 5, ord: 4096 }).ord).toBe(4096)
    expect(historyEntryToMessage({ role: 'user', text: 'hi', ts: 5 }).ord).toBeUndefined()
    // A hand-mirrored wire type cannot rule out a null, and copying it would defeat the
    // Number.isFinite gate downstream by putting a non-number where a number is declared.
    const nulled = { role: 'user', text: 'hi', ts: 5, ord: null } as unknown as HistoryEntry
    expect(historyEntryToMessage(nulled).ord).toBeUndefined()
  })
})

describe('hasOrd (tether#109)', () => {
  it('accepts a position and rejects every shape the wire can send instead', () => {
    expect(hasOrd({ ord: 1 })).toBe(true)
    expect(hasOrd({ ord: 0 })).toBe(true)
    expect(hasOrd({})).toBe(false)
    expect(hasOrd({ ord: undefined })).toBe(false)
    expect(hasOrd({ ord: null as unknown as number })).toBe(false)
    expect(hasOrd({ ord: '4096' as unknown as number })).toBe(false)
    expect(hasOrd({ ord: NaN })).toBe(false)
    expect(hasOrd({ ord: Infinity })).toBe(false)
  })
})

describe('setTranscriptBounds (tether#107)', () => {
  afterEach(() => {
    reset()
    useStore.setState({ transcriptEarlier: null, transcriptOtherRecord: null, transcriptPagesBack: 0 })
  })

  it('records both facts', () => {
    useStore.getState().setTranscriptBounds({ earlier: 4096, otherRecord: 'cc' })
    expect(useStore.getState().transcriptEarlier).toBe(4096)
    expect(useStore.getState().transcriptOtherRecord).toBe('cc')
  })

  it('does not publish a new state object when nothing moved', () => {
    // ChatPane subscribes without a selector, so an unconditional set() re-renders it
    // — and invalidates the transcript memo — on every one of the three-second
    // probe's reloads. Same argument, and same construction, as clearNotices.
    useStore.getState().setTranscriptBounds({ earlier: 4096, otherRecord: null })
    let updates = 0
    const off = useStore.subscribe(() => { updates++ })
    useStore.getState().setTranscriptBounds({ earlier: 4096, otherRecord: null })
    off()
    expect(updates).toBe(0)
  })
})
