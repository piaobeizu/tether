import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, render, screen } from '@testing-library/react'
import ChatPane, { HELD_SESSION_PLACEHOLDER, HELD_SESSION_READABLE_NOTE } from './index'
import { useStore } from '../../lib/store'
import { ErrCodeSessionHeldByBackgroundAgent, ErrCodeSessionOwned } from '../../lib/wire.gen'

// tether#80 — the LAST hop, which nothing pinned until now.
//
// Everything else about a notice is asserted in store.test.ts: it is created,
// it is bounded, it is ordered, and mergeTranscript projects it into a
// `role: 'system'` Message. None of that says the pane still CALLS
// mergeTranscript. ChatPane.test.tsx is 560 lines that render sub-components
// directly and never mount ChatPane at all, so replacing
// `mergeTranscript(messages, notices)` with `messages` at index.tsx's transcript
// memo left the entire frontend suite green — every notice in the app silently
// stops being rendered and no test notices (found by tether#77's review, filed
// as this wi's N5; the general form is the team's
// "test the WIRING hop, not just the unit").
//
// That gap is worth more than usual here, because "the frame becomes a line a
// human can read" is the whole point of this wi. A fix that produced a perfect
// notice the pane never rendered would pass every other test in the repo.
//
// Mounting ChatPane is possible without a WebTransport stack for one specific
// reason: with no remembered sid and `workspacesLoaded` false, the first connect
// is DEFERRED (shouldDeferFirstConnect) behind a store subscription and a 2s
// fallback timer, neither of which fires here. That is the harness's own
// precondition, so it is asserted below rather than assumed — if a future change
// makes the pane connect on mount, these tests must fail loudly instead of
// quietly exercising a different path.

const originalFetch = globalThis.fetch

function stubFetch() {
  // Both mount fetches: GET /api/v1/providers, and GET /messages (never reached
  // here — sessionId stays null — but stubbed so a regression that DOES reach it
  // fails on an assertion rather than an unhandled rejection).
  return vi.fn(async (input: RequestInfo | URL) => {
    const url = String(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url)
    const body = url.includes('/providers') ? { providers: ['claude-code'] } : []
    return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } })
  })
}

describe('ChatPane renders notices (tether#80, wi N5)', () => {
  beforeEach(() => {
    localStorage.clear()
    globalThis.fetch = stubFetch() as unknown as typeof fetch
    useStore.setState({
      messages: [], notices: [], pendingPermissions: [], fatal: null,
      streaming: false, streamingMsgId: null, curTurnId: null,
      sessionId: null, workspacesLoaded: false, activeWorkspace: null,
    })
    // The harness's precondition, asserted: no remembered sid + workspaces not
    // settled is exactly the state that defers the first connect.
    expect(localStorage.getItem('tether_last_sid')).toBeNull()
    expect(useStore.getState().workspacesLoaded).toBe(false)
    // And nothing in this file may reach a WebTransport constructor.
    expect((globalThis as { WebTransport?: unknown }).WebTransport).toBeUndefined()
  })

  afterEach(() => {
    cleanup()
    globalThis.fetch = originalFetch
    useStore.setState({ messages: [], notices: [], sessionId: null })
    localStorage.clear()
  })

  it('renders an agent-error notice as a system line in the transcript', () => {
    useStore.setState({
      notices: [{
        id: 'n1',
        text: 'The agent reported an error — busy: another prompt is running',
        ts: 1000,
        kind: 'agent_error',
        repeats: 1,
      }],
    })
    render(<ChatPane />)
    const line = screen.getByText('The agent reported an error — busy: another prompt is running')
    expect(line).toBeTruthy()
    expect(line.className).toBe('msg-system-text')
  })

  it('renders the collapsed repeat count the store folded arrivals into', () => {
    useStore.setState({
      notices: [{ id: 'n1', text: 'The agent reported an error — busy', ts: 1000, kind: 'agent_error', repeats: 12 }],
    })
    render(<ChatPane />)
    // The count lives in the mergeTranscript projection, so this also pins that
    // the pane renders the PROJECTION and not Notice.text directly.
    expect(screen.getByText('The agent reported an error — busy (×12)')).toBeTruthy()
  })

  it('interleaves the notice with the conversation instead of dropping either', () => {
    useStore.setState({
      messages: [
        { id: 'u1', role: 'user', text: 'first prompt', ts: 1000 },
        { id: 'u2', role: 'user', text: 'second prompt', ts: 3000 },
      ],
      notices: [{ id: 'n1', text: 'The agent reported an error — boom', ts: 2000, kind: 'agent_error' }],
    })
    const { container } = render(<ChatPane />)
    const texts = [...container.querySelectorAll('.msg-user-bubble, .msg-system-text')].map((e) => e.textContent)
    expect(texts).toEqual(['first prompt', 'The agent reported an error — boom', 'second prompt'])
  })

  // The other direction: an empty notice list must not invent a system line.
  //
  // NEGATIVE CONTROL — this one passes with or without the wiring hop, by
  // construction (`messages` and `mergeTranscript(messages, [])` are the same
  // array). It is here to say that the three above discriminate and this does
  // not, so nobody later reads "4 passed" as four independent guards.
  it('renders no system line when there are no notices', () => {
    useStore.setState({ messages: [{ id: 'u1', role: 'user', text: 'hello', ts: 1000 }], notices: [] })
    const { container } = render(<ChatPane />)
    expect(container.querySelectorAll('.msg-system')).toHaveLength(0)
    expect(container.querySelectorAll('.msg-user-bubble')).toHaveLength(1)
  })

  // tether#83 — the same wiring hop, one level down, for the pointer the fix
  // stopped clearing. ThinkingBlock's `live` prop used to be `m.id === curTurnId
  // && !m.text`, and its doc promises the block cannot get stuck on "thinking…"
  // because the turn ending on a result OR AN ERROR makes it false. A
  // non-terminal error no longer ends the turn, so that promise now rests on the
  // `streaming` conjunct at the call site — which lives in a JSX map that nothing
  // else in the suite renders, so nothing else can see it disappear.
  //
  // The reachable path is cc-only and narrow, which is the argument FOR pinning
  // it rather than against: cc is the only provider that emits thinking deltas
  // (claude_provider.go), its stdout dying mid-thinking is an ErrCodeAgent frame
  // from ccSession.abandon, and the stream-end result that would eventually
  // collapse the block is a best-effort broadcast the daemon drops on a slow
  // subscriber.
  it('collapses a live thinking block when a non-terminal error lands on the turn', () => {
    const h = useStore.getState().handleEnvelope
    h({ kind: 'message', payload: { type: 'thinking', text: 'weighing the options' } })
    // Precondition: the block is live, i.e. this test can tell the difference.
    expect(useStore.getState().streaming).toBe(true)
    expect(render(<ChatPane />).container.querySelectorAll('.msg-thinking').length).toBe(1)
    expect(screen.getByText('thinking…')).toBeTruthy()
    cleanup()

    h({ kind: 'error', payload: { code: 'agent_error', message: 'stdout died', terminal: false } } as never)
    render(<ChatPane />)
    // The turn is kept (tether#83) — the thinking text is still on its bubble…
    expect(useStore.getState().curTurnId).not.toBeNull()
    // …but the block no longer claims the agent is still thinking.
    expect(screen.queryByText('thinking…')).toBeNull()
  })
})

// ────────────────────────────────────────────────────────────────────────────
// tether#104 — the READ-ONLY reading state, and the thing it rests on.
//
// A session held by a live background agent is refused terminally
// (ErrCodeSessionHeldByBackgroundAgent), so connState goes 'failed' and the card
// renders. The transcript underneath it was ALREADY served — openSession fetches
// GET /api/v1/sessions/<sid>/messages over plain HTTP, which needs no agent
// process and no WebTransport — and `{transcript.map(...)}` is not gated on the
// connection at all, so the card OVERLAYS the conversation rather than replacing
// it. The owner confirmed that on the live deployment (8994377) before this wi
// was scoped.
//
// Nothing pinned it. "Failed connection ⇒ show the error INSTEAD of the content"
// is the obvious-looking simplification, and a reader who applies it breaks the
// only feature this wi ships while every other test stays green — the transcript
// tests above all run with fatal: null. That is what the first test here exists
// to stop.
//
// # Reaching connState === 'failed' honestly
//
// Not by poking component state — there is no seam for it, and one bolted on
// here would let the assertions pass over a path the app never takes. The pane
// is driven through its real machinery instead:
//
//   workspacesLoaded: true  ⇒ shouldDeferFirstConnect is false ⇒ mount connects.
//   TetherWT.connect() awaits two fetches (stubbed) and then constructs
//   WebTransport, which jsdom does not have ⇒ the promise rejects.
//   index.tsx's .catch reads store.fatal at that moment: non-null ⇒
//   shouldReconnectAfterClose false ⇒ setConnState('failed').
//
// `fatal` is therefore set AFTER render() and not before: doConnect calls
// clearFatal() synchronously on its way out (a fresh attempt deserves a fresh
// chance, tether#63), so a value set beforehand would be wiped before the catch
// could read it. That ordering is the harness's one piece of cleverness, so the
// first test below asserts the state it produces (`.failed-card` on screen)
// rather than trusting it — a harness that silently stopped reaching 'failed'
// would otherwise turn every assertion here into a vacuous truth.
describe('a session held by a background agent reads as a state, not a failure (tether#104)', () => {
  const held = {
    code: ErrCodeSessionHeldByBackgroundAgent,
    message: 'session e4d1f668 is being used by a live background agent (kind bg, job e4d1f668); resuming it would take it away from that job',
  }
  // The control code for the no-spillover assertions. Terminal like the one under
  // test, and permanent where that one is temporary — so if the new presentation
  // leaked out of its code, this is where it would show.
  const owned = { code: ErrCodeSessionOwned, message: 'session is owned by client abc' }

  beforeEach(() => {
    localStorage.clear()
    globalThis.fetch = stubFetch() as unknown as typeof fetch
    useStore.setState({
      messages: [], notices: [], pendingPermissions: [], fatal: null,
      streaming: false, streamingMsgId: null, curTurnId: null,
      sessionId: null, workspacesLoaded: true, activeWorkspace: null,
    })
    expect((globalThis as { WebTransport?: unknown }).WebTransport).toBeUndefined()
  })

  afterEach(() => {
    cleanup()
    globalThis.fetch = originalFetch
    useStore.setState({ messages: [], notices: [], sessionId: null, fatal: null, workspacesLoaded: false })
    localStorage.clear()
  })

  // Mounts the pane, lets the connect fail, and lands the given refusal on it.
  async function renderRefused(fatal: { code: string; message: string }) {
    const r = render(<ChatPane />)
    useStore.setState({ fatal })
    await act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)) })
    return r
  }

  it('still renders the conversation underneath the card', async () => {
    useStore.setState({
      messages: [
        { id: 'u1', role: 'user', text: 'what did we decide about the cache?', ts: 1000 },
        { id: 'a1', role: 'assistant', text: 'we settled on a bounded tail', ts: 2000 },
      ],
    })
    const { container } = await renderRefused(held)

    // The harness's own precondition: the refusal really did land and the card
    // really is on screen. Without this the assertion below passes just as well
    // on a pane that never reached 'failed'.
    expect(container.querySelectorAll('.failed-card')).toHaveLength(1)

    // The point: BOTH halves are present at once.
    expect(screen.getByText('what did we decide about the cache?')).toBeTruthy()
    expect([...container.querySelectorAll('.msg-user-bubble, .msg-ai-body')].length).toBeGreaterThan(0)
    expect(screen.getByText('we settled on a bounded tail')).toBeTruthy()
  })

  it('keeps the composer disabled — readable is not writable', async () => {
    useStore.setState({ messages: [{ id: 'u1', role: 'user', text: 'hello', ts: 1000 }] })
    const { container } = await renderRefused(held)
    expect(container.querySelectorAll('.failed-card')).toHaveLength(1)

    const box = container.querySelector('.composer-input') as HTMLTextAreaElement
    expect(box.disabled).toBe(true)
    const send = container.querySelector('.send-btn') as HTMLButtonElement
    expect(send.disabled).toBe(true)
  })

  it('tells the composer WHY it is read-only instead of saying "not connected"', async () => {
    useStore.setState({ messages: [{ id: 'u1', role: 'user', text: 'hello', ts: 1000 }] })
    const { container } = await renderRefused(held)
    expect(container.querySelectorAll('.failed-card')).toHaveLength(1)

    // The exact string, not a property of it. A `toMatch(/read-only/)` here would
    // survive the placeholder degrading back to something generic that happened
    // to contain the phrase (tether#102 measured a property assertion keeping a
    // real mutant alive in this very suite).
    const box = container.querySelector('.composer-input') as HTMLTextAreaElement
    expect(box.placeholder).toBe(HELD_SESSION_PLACEHOLDER)
    expect(box.placeholder).toBe('read-only — a background agent is using this conversation')
  })

  it('points at the conversation below, and says only what tether has of it', async () => {
    useStore.setState({ messages: [{ id: 'u1', role: 'user', text: 'hello', ts: 1000 }] })
    const { container } = await renderRefused(held)

    const note = container.querySelector('.failed-card .state-card-read')
    expect(note?.textContent).toBe(HELD_SESSION_READABLE_NOTE)
    // No completeness claim. SessionList's EXTERNAL_SESSION_PROMISE owns the
    // extent question and only appears when the extent is actually bounded (a
    // cc-sourced transcript); this line has to be true under BOTH sources, so it
    // states an upper bound and nothing else.
    expect(HELD_SESSION_READABLE_NOTE).toContain('what tether has of this conversation')
    expect(HELD_SESSION_READABLE_NOTE).not.toMatch(/\b(whole|entire|full|complete|all of)\b/i)
  })

  // The line above is a claim about the screen. When there is nothing below the
  // card, making it would be a new false statement — and "no transcript" is a
  // NORMAL answer here (SessionIndex.Messages returns SourceNone with a nil
  // slice for a session neither store has).
  it('does not point at a conversation that is not there', async () => {
    useStore.setState({ messages: [], notices: [] })
    const { container } = await renderRefused(held)
    expect(container.querySelectorAll('.failed-card')).toHaveLength(1)
    expect(container.querySelector('.state-card-read')).toBeNull()
  })

  it('drops the danger dressing for this code only', async () => {
    useStore.setState({ messages: [{ id: 'u1', role: 'user', text: 'hello', ts: 1000 }] })
    const { container } = await renderRefused(held)

    const card = container.querySelector('.failed-card') as HTMLElement
    expect(card.className).toBe('failed-card state-card')
    // The headline is the pane's own inline colour, not var(--danger).
    const headline = card.querySelector('.failed-card-headline') as HTMLElement
    expect(headline.style.color).toBe('var(--ink-primary)')
    // "Retry" describes retrying something that broke. Nothing broke.
    expect(card.querySelector('.btn-ghost-sm')?.textContent).toBe('Check again')
  })

  // ── No spillover ──────────────────────────────────────────────────────────
  //
  // The whole change is keyed on one code. These pin the OTHER terminal codes'
  // presentation unchanged, so a future edit that reaches for the card as a
  // whole — restyling it, moving the note out of its conditional, dropping the
  // code check — fails here rather than quietly recolouring every refusal.
  it('leaves another terminal code rendering exactly as before', async () => {
    useStore.setState({ messages: [{ id: 'u1', role: 'user', text: 'hello', ts: 1000 }] })
    const { container } = await renderRefused(owned)

    const card = container.querySelector('.failed-card') as HTMLElement
    // No modifier class, so index.css's .failed-card.state-card override cannot
    // apply and the danger tint/border stay.
    expect(card.className).toBe('failed-card')
    const headline = card.querySelector('.failed-card-headline') as HTMLElement
    expect(headline.style.color).toBe('var(--danger)')
    expect(headline.textContent).toBe('This session is open on another device.')
    expect(card.querySelector('.btn-ghost-sm')?.textContent).toBe('Retry')
    // The read-only note is this code's alone.
    expect(card.querySelector('.state-card-read')).toBeNull()
    // …and so is the placeholder. The generic branch is still what every other
    // non-connected state gets.
    const box = container.querySelector('.composer-input') as HTMLTextAreaElement
    expect(box.placeholder).toBe('not connected')
    // The transcript is unconditional for every code, not just the new one.
    expect(screen.getByText('hello')).toBeTruthy()
  })
})

// ────────────────────────────────────────────────────────────────────────────
// tether#106 — the transcript follows the other agent, and the click on the
// highlighted row still does nothing when there is something to protect.
//
// Both halves are wiring, and wiring is this repo's most reliable blind spot: the
// watcher is unit-tested in transcriptWatch.test.ts and the click is unit-tested in
// session.test.ts, and BOTH can be perfect while the pane subscribes to neither.
// Nothing else in the suite would notice — a transcript frozen at the moment it was
// fetched renders exactly like one that is up to date.
//
// The gate under test is `readingHeldSession`, i.e. the conjunction
// `connState === 'failed' && fatal.code === session_held_by_background_agent`. So the
// cases below are chosen to break it in each direction independently: connected (the
// first conjunct false, and the state the tether#61 guard exists for), another
// terminal code (the second false), and held (both true).
describe('a held transcript keeps up, and a live one is still left alone (tether#106)', () => {
  const held = {
    code: ErrCodeSessionHeldByBackgroundAgent,
    message: 'session e4d1f668 is being used by a live background agent (kind bg, job e4d1f668)',
  }
  const owned = { code: ErrCodeSessionOwned, message: 'session is owned by client abc' }
  const SID = 'sid-held-0001'
  const MESSAGES_URL = `/api/v1/sessions/${SID}/messages`

  /** Every request the pane issued, so a test can count the ones it cares about. */
  let seen: { method: string; url: string }[] = []
  const transcriptGets = () => seen.filter(r => r.url === MESSAGES_URL && r.method === 'GET').length
  const transcriptProbes = () => seen.filter(r => r.url === MESSAGES_URL && r.method === 'HEAD').length

  /**
   * A daemon that answers everything the pane asks on its way to a live connection,
   * and reports a transcript version the test can move.
   */
  function stubDaemon(version: () => number) {
    return vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url)
      seen.push({ method: init?.method ?? 'GET', url })
      if (url.includes('/providers')) return new Response(JSON.stringify({ providers: ['claude-code'] }), { status: 200 })
      if (url.includes('/wt-ticket')) return new Response(JSON.stringify({ ticket: 'tkt' }), { status: 200 })
      if (url.includes('/cert-hash')) return new Response('', { status: 404 })
      if (url === MESSAGES_URL) {
        const headers = new Headers({ 'Content-Type': 'application/json' })
        headers.set('X-Tether-Transcript-Updated-At', String(version()))
        return new Response(init?.method === 'HEAD' ? null : '[]', { status: 200, headers })
      }
      return new Response('[]', { status: 200, headers: { 'Content-Type': 'application/json' } })
    })
  }

  /**
   * The minimum WebTransport that lets doConnect reach connState 'connected'.
   *
   * jsdom has none, which is why every other test in this file exercises the FAILED
   * path — and why "clicking the current row while connected does nothing" could not
   * be asserted before. `closed` never settles, so onClose never fires and the pane
   * stays connected for the length of the test; the incoming stream never yields, so
   * no envelope arrives to move the store underneath the assertions.
   */
  class FakeWebTransport {
    ready = Promise.resolve()
    closed = new Promise<never>(() => {})
    incomingUnidirectionalStreams = new ReadableStream({ start() { /* silent */ } })
    createBidirectionalStream() {
      return Promise.resolve({ writable: new WritableStream(), readable: new ReadableStream({ start() { } }) })
    }
    close() { /* no-op */ }
  }

  /** Let every promise chain the connect path strings together settle. */
  const settle = async () => {
    for (let i = 0; i < 5; i++) {
      await act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)) })
    }
  }

  beforeEach(() => {
    localStorage.clear()
    seen = []
    globalThis.fetch = stubDaemon(() => 1000) as unknown as typeof fetch
    localStorage.setItem('tether_last_sid', SID)
    useStore.setState({
      messages: [], notices: [], pendingPermissions: [], fatal: null,
      streaming: false, streamingMsgId: null, curTurnId: null,
      sessionId: SID, workspacesLoaded: true, activeWorkspace: null,
    })
  })

  afterEach(() => {
    cleanup()
    globalThis.fetch = originalFetch
    delete (globalThis as { WebTransport?: unknown }).WebTransport
    useStore.setState({ messages: [], notices: [], sessionId: null, fatal: null, streaming: false, workspacesLoaded: false })
    localStorage.clear()
  })

  async function renderRefusedWithSid(fatal: { code: string; message: string }) {
    const r = render(<ChatPane />)
    useStore.setState({ fatal })
    await settle()
    return r
  }

  it('probes the transcript and reloads it while a background agent holds the session', async () => {
    const { container } = await renderRefusedWithSid(held)
    // The harness's precondition: the refusal really landed. Without this the
    // assertions below would pass just as well on a pane that never reached 'failed'.
    expect(container.querySelectorAll('.failed-card')).toHaveLength(1)

    // Exact counts, not "at least one". A watcher wired twice, or one that reloads on
    // every tick instead of on a change, satisfies "at least one" and is a defect —
    // tether#102 measured a property assertion in this very suite keeping a real
    // mutant alive. Two GETs: the pane's own [sessionId] load at mount, then the
    // reload the probe decided on because the version it saw is not one this pane had
    // recorded. Deleting the effect that wires the watcher leaves every unit test in
    // transcriptWatch.test.ts green and fails exactly here.
    expect(transcriptProbes()).toBe(1)
    expect(transcriptGets()).toBe(2)
    const probeAt = seen.findIndex(r => r.url === MESSAGES_URL && r.method === 'HEAD')
    const reloadAfterProbe = seen.slice(probeAt).some(r => r.url === MESSAGES_URL && r.method === 'GET')
    expect(reloadAfterProbe).toBe(true)
  })

  it('reloads on a click on the row that is already open', async () => {
    await renderRefusedWithSid(held)
    const before = transcriptGets()

    // What SessionRow's click reaches this pane as (lib/session.ts openSession).
    await act(async () => { window.dispatchEvent(new CustomEvent('tether:refresh-transcript')) })
    await settle()

    expect(transcriptGets()).toBe(before + 1)
  })

  it('asks both questions when "Check again" is pressed', async () => {
    // tether#104 named the button for the connection question. The reader pressing it
    // wants the other one too, and the connection attempt alone answers only the first.
    const { container } = await renderRefusedWithSid(held)
    const before = transcriptGets()

    const button = container.querySelector('.failed-card .btn-ghost-sm') as HTMLButtonElement
    expect(button.textContent).toBe('Check again')
    await act(async () => { button.click() })
    await settle()

    expect(transcriptGets()).toBe(before + 1)
  })

  it('does NOT follow or reload for another terminal code', async () => {
    // The gate is one code's. A watcher that ran for every refusal would poll — and
    // reload — sessions whose transcripts nothing is writing.
    await renderRefusedWithSid(owned)
    const before = transcriptGets()
    expect(transcriptProbes()).toBe(0)

    await act(async () => { window.dispatchEvent(new CustomEvent('tether:refresh-transcript')) })
    await settle()
    expect(transcriptGets()).toBe(before)
  })

  it('does NOTHING when the row that is already open is CONNECTED', async () => {
    // THE regression guard for this change (tether#61's rule, narrowed rather than
    // removed). The session list highlights the current row, so this click is easy to
    // make by accident, and reloading here would replace `messages` wholesale on top
    // of a turn the daemon is still streaming — dropping the optimistic bubble
    // tether#42 exists to keep, and doing it from the one path tether#57 showed can
    // silently eat state.
    ;(globalThis as { WebTransport?: unknown }).WebTransport = FakeWebTransport
    render(<ChatPane />)
    await settle()

    // The harness's precondition, asserted rather than assumed: the pane really is
    // connected. `disabled={connState !== 'connected'}` is the only observable that
    // says so, and without this check a pane stuck in 'connecting' would satisfy
    // every assertion below while testing nothing.
    const box = document.querySelector('.composer-input') as HTMLTextAreaElement
    expect(box.disabled).toBe(false)

    useStore.setState({
      streaming: true,
      messages: [{ id: 'live-bubble', role: 'assistant', text: 'mid-turn', ts: 1 }],
    })
    const before = transcriptGets()

    await act(async () => { window.dispatchEvent(new CustomEvent('tether:refresh-transcript')) })
    await settle()

    // Exactly the one load the pane's own [sessionId] effect made at mount, and not
    // one byte more.
    expect(before).toBe(1)
    expect(transcriptGets()).toBe(1)
    // No probe either: a session with a live stream has nothing to poll for.
    expect(transcriptProbes()).toBe(0)
    // And the in-flight turn is untouched — the actual harm, stated as itself.
    expect(useStore.getState().messages.map(m => m.id)).toEqual(['live-bubble'])
    expect(useStore.getState().streaming).toBe(true)
  })

  it('no longer tells the reader the transcript is a still frame', async () => {
    // The copy is part of the behaviour here. tether#104's line was true when the
    // transcript was one GET at open time; leaving it after this change would be the
    // worse of the two defects, because a reader who believes they are looking at a
    // frozen snapshot goes and reloads the page — the exact effort this removes.
    useStore.setState({ messages: [{ id: 'u1', role: 'user', text: 'hello', ts: 1000 }] })
    const { container } = await renderRefusedWithSid(held)

    const note = container.querySelector('.failed-card .state-card-read')
    expect(note?.textContent).toBe(HELD_SESSION_READABLE_NOTE)
    expect(HELD_SESSION_READABLE_NOTE).not.toContain('as it stood when this pane fetched it')
    expect(HELD_SESSION_READABLE_NOTE).toContain('what tether has of this conversation')
  })
})
