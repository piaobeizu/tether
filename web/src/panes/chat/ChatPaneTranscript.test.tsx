import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, render, screen } from '@testing-library/react'
import ChatPane, {
  ARRIVAL_TRACE_MS,
  HELD_ACTIVITY_GONE, HELD_ACTIVITY_IDLE, HELD_ACTIVITY_UNKNOWN, HELD_ACTIVITY_WORKING,
  HELD_SESSION_PLACEHOLDER, HELD_SESSION_READABLE_NOTE,
  TRANSCRIPT_DOTS_EARLIER_LABEL, TRANSCRIPT_DOTS_NEWER_LABEL,
  TRANSCRIPT_EDGE_MIN_INTERVAL_MS,
  TRANSCRIPT_OVERSCROLL_TOUCH_PX,
  TRANSCRIPT_START_COMPLETE, TRANSCRIPT_START_TETHER_RECORD_ONLY,
} from './index'
import { useStore } from '../../lib/store'
import { ErrCodeSessionHeldByBackgroundAgent, ErrCodeSessionOwned } from '../../lib/wire.gen'
import { resetTranscriptWatchForTests } from '../../lib/transcriptWatch'
import {
  SESSION_ACTIVITY_PATH,
  resetSessionActivityForTests,
  sessionActivityPollerState,
  subscribeSessionActivity,
} from '../../lib/sessionActivity'

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

/** How tall one bubble is in the geometry model below. */
const ROW_PX = 100

/** tether#112 — identifiers for the two fingers the touch fixture can put on the glass: the
 *  one making the gesture, and a thumb already resting there. Distinct and arbitrary; what
 *  matters is only that the pane must follow the RIGHT one. */
const TOUCH_ID = 7
const RESTING_ID = 3

/**
 * Give an element a scroll geometry jsdom does not have.
 *
 * `scrollHeight` COUNTS THE BUBBLES IN THE DOM rather than being a number the test
 * sets, so a prepend makes the container taller with no help from the test — which is
 * the whole point: a model the test drives by hand would let a broken correction and a
 * correct one produce the same numbers.
 *
 * There is deliberately NO clamping to `scrollHeight - clientHeight`. A real browser
 * clamps, and clamping here would hide the difference between "restored the anchor"
 * and "jumped to the bottom" in exactly the cases where they land close together.
 *
 * At module scope since tether#108, which needs the same model to check that the arrival
 * TRACE does not move a reader who has scrolled up. Written for tether#107 and unchanged.
 */
function fakeScrollBox(el: HTMLElement, clientHeight: number) {
  let top = 0
  Object.defineProperty(el, 'clientHeight', { configurable: true, get: () => clientHeight })
  Object.defineProperty(el, 'scrollHeight', {
    configurable: true,
    get: () => el.querySelectorAll('.msg-user, .msg-ai, .msg-system').length * ROW_PX,
  })
  Object.defineProperty(el, 'scrollTop', {
    configurable: true,
    get: () => top,
    set: (v: number) => { top = v },
  })
  return {
    top: () => top,
    scrollTo: (v: number) => { top = v },
    height: () => el.scrollHeight,
  }
}

/**
 * fakeScrollBox, plus the half of a browser jsdom leaves out: **scrolling fires `scroll`
 * events, including when the scroll came from code** (tether#110).
 *
 * Per CSSOM View, assigning `scrollTop` performs a scroll, and the scroll steps that run
 * afterwards fire the event. That is not a detail — it is the mechanism of the loop this
 * wi has to bound. `scrollAfterPrepend`'s correction is a `scrollTop` write, so a
 * prepend re-enters the scroll handler on its own, and the autoscroll effect's
 * `scrollTop = scrollHeight` does the same at the other end. A fixture that only fired
 * events the test dispatched by hand could not express the difference between a latch
 * that works and no latch at all: both would look bounded, because nothing would ever
 * re-enter.
 *
 * Fired on a MACROTASK rather than synchronously, because the real one is not
 * synchronous either: a synchronous dispatch inside the layout effect's own write would
 * re-enter React during commit, which no browser does. `settle()` flushes five rounds of
 * `setTimeout(0)`, so a scroll that begets a scroll is followed to a fixed point.
 *
 * `scrollTo` goes through the property rather than the closure, so a reader's scroll
 * fires an event too — which is what the handler is attached for.
 */
function liveScrollBox(el: HTMLElement, clientHeight: number) {
  const box = fakeScrollBox(el, clientHeight)
  const desc = Object.getOwnPropertyDescriptor(el, 'scrollTop')
  const get = desc?.get as () => number
  const set = desc?.set as (v: number) => void
  let events = 0
  let wheels = 0
  let touchMoves = 0
  el.addEventListener('scroll', () => { events++ })
  // tether#112 — counted for the same reason the scroll events are: so a test can assert the
  // FIXTURE really delivered the gesture. Without these, "pulling at the end loads" would be
  // satisfied by a dispatch that never reached a listener, which is precisely the vacuous
  // shape jsdom's silent `scrollTop` writes taught this file to distrust.
  el.addEventListener('wheel', () => { wheels++ })
  el.addEventListener('touchmove', () => { touchMoves++ })
  Object.defineProperty(el, 'scrollTop', {
    configurable: true,
    get: () => get.call(el),
    set: (v: number) => {
      set.call(el, v)
      setTimeout(() => el.dispatchEvent(new Event('scroll')), 0)
    },
  })
  const point = (identifier: number, clientY: number) => ({ identifier, clientY } as unknown as Touch)
  const lists = (y: number, resting?: number) => ({
    touches: resting === undefined
      ? [point(TOUCH_ID, y)]
      // The thumb FIRST, which is where the browser puts the older touch — and is what makes
      // `touches[0]` the wrong thing for a handler to read.
      : [point(RESTING_ID, resting), point(TOUCH_ID, y)],
    changedTouches: [point(TOUCH_ID, y)],
    bubbles: true,
  })
  const touchStart = (y: number, resting?: number) =>
    el.dispatchEvent(new TouchEvent('touchstart', lists(y, resting)))
  const touchMoveTo = (y: number, resting?: number) =>
    el.dispatchEvent(new TouchEvent('touchmove', lists(y, resting)))
  const touchEnd = () =>
    el.dispatchEvent(new TouchEvent('touchend', { touches: [], changedTouches: [point(TOUCH_ID, 0)], bubbles: true }))
  /** The browser claiming the gesture for itself — pull-to-refresh, a back swipe. */
  const touchCancel = () =>
    el.dispatchEvent(new TouchEvent('touchcancel', { touches: [], changedTouches: [point(TOUCH_ID, 0)], bubbles: true }))
  return {
    top: box.top,
    height: box.height,
    /**
     * The reader scrolls: move, then deliver the event SYNCHRONOUSLY.
     *
     * Deliberately not the same timing as the wrapped setter above. The test is standing
     * in for the browser's scroll machinery here, so a sequence of positions the handler
     * must see one at a time has to be delivered one at a time — four queued events would
     * all read the final position and the sequence would be untestable. The PANE's own
     * writes keep the asynchronous timing, because that is the timing the loop needs.
     */
    scrollTo: (v: number) => { box.scrollTo(v); el.dispatchEvent(new Event('scroll')) },
    /** A scroll event with no movement — momentum settling, a rubber-band release. */
    fire: () => el.dispatchEvent(new Event('scroll')),
    /** How many scroll events this element has seen, from ANY source — the test's, the
     *  prepend anchor's, and the autoscroll's. Exposed so a test can assert the fixture
     *  really is re-entrant rather than assuming it. */
    events: () => events,

    /**
     * tether#112 — the gesture at an end, which is the case `scroll` cannot express.
     *
     * `scrollTop` is deliberately NOT touched by either of these. That is not a shortcut: it
     * is the scenario. At an end the browser has already clamped the position, so pulling
     * further moves nothing, fires no `scroll`, and the only trace the gesture leaves is the
     * `wheel` / `touchmove` event itself. A fixture that moved the box would be testing the
     * path that already worked.
     *
     * `deltaY > 0` is toward the bottom, matching the platform's own sign. `deltaMode`
     * defaults to 0 (measured, not assumed — jsdom 29.1.1's WheelEvent), which is irrelevant
     * to the pane: it reads only the sign.
     */
    wheel: (deltaY: number) => el.dispatchEvent(new WheelEvent('wheel', { deltaY, bubbles: true })),

    /**
     * A touch gesture, in the three parts a real one has — because whether a defect exists at
     * all depends on where the gesture BOUNDARIES are. A fixture that restarts the gesture
     * before every move (which is what this used to do) cannot express "the finger is still
     * down", and that is precisely where the burst lived that review found.
     *
     * The touch points are plain objects rather than `Touch` instances because this jsdom has
     * no `Touch` interface at all (`typeof Touch === 'undefined'`, measured) while its
     * `TouchEvent` constructor accepts and re-exposes whatever it is handed — `identifier`,
     * `clientY`, `touches` and `changedTouches` all round-trip (measured). Using the real
     * constructor rather than a hand-decorated `Event` keeps `instanceof`/`type` honest at the
     * one hop that matters.
     *
     * `resting` puts a SECOND finger on the glass that never moves — a thumb holding the
     * phone — and puts it FIRST in `touches`, which is where the browser puts it and which is
     * what makes `touches[0]` the wrong thing to read.
     */
    touchStart,
    touchMoveTo,
    touchEnd,
    touchCancel,
    /**
     * One COMPLETE gesture: finger down, a single pull of `dy`, finger up. Convenience for
     * the cases that are genuinely about one gesture. Anything about what happens WHILE the
     * finger stays down must use the three parts, or the assertion is about a fixture that
     * silently starts a new gesture per move.
     */
    touchPull: (dy: number, from = 400) => {
      touchStart(from)
      touchMoveTo(from - dy)
      touchEnd()
    },
    /** How many wheel / touchmove events this element has actually delivered to a listener. */
    wheels: () => wheels,
    touchMoves: () => touchMoves,
  }
}

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
   * What the daemon has of this transcript. Served by the stub below, so a reload the
   * pane performs lands the SAME conversation the test seeded rather than emptying it —
   * which matters here because the note under test is gated on there being a transcript
   * on screen at all.
   */
  let daemonTranscript = '[]'

  /** The version the daemon reports for it. Moving this is how a test says "the other
   *  agent just wrote". */
  let daemonVersion = 1000

  /** What GET /api/v1/session-activity answers (tether#108). sid -> state. */
  let daemonActivity: Record<string, string> = {}
  const activityPolls = () => seen.filter(r => r.url === SESSION_ACTIVITY_PATH).length
  /**
   * The sentence the card is SHOWING, or null when it is showing none.
   *
   * Reads the one slot carrying `on` rather than the container's textContent, because the
   * container deliberately holds all four sentences at once — three of them
   * `visibility: hidden` — so that its height cannot change when the answer does. See
   * HELD_ACTIVITY_LINES.
   */
  const activityLine = () => document.querySelector('.state-card-activity-line.on')?.textContent ?? null
  /** How many sentences are in the DOM, and how many are showing. */
  const activitySlots = () => ({
    all: document.querySelectorAll('.state-card-activity-line').length,
    on: document.querySelectorAll('.state-card-activity-line.on').length,
  })

  /**
   * A daemon that answers everything the pane asks on its way to a live connection,
   * and reports a transcript version the test can move.
   */
  function stubDaemon() {
    return vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url)
      seen.push({ method: init?.method ?? 'GET', url })
      if (url.includes('/providers')) return new Response(JSON.stringify({ providers: ['claude-code'] }), { status: 200 })
      if (url.includes('/wt-ticket')) return new Response(JSON.stringify({ ticket: 'tkt' }), { status: 200 })
      if (url.includes('/cert-hash')) return new Response('', { status: 404 })
      // tether#108 — answered explicitly rather than by the `[]` fall-through below.
      // `fetchSessionActivity` turns an array into `{}`, so the fall-through would make
      // every test in this block silently claim the daemon reported "nothing is holding
      // this sid" — an answer, and the one that reads as "the hold has ended".
      if (url === SESSION_ACTIVITY_PATH) {
        return new Response(JSON.stringify(daemonActivity), { status: 200, headers: { 'Content-Type': 'application/json' } })
      }
      if (url === MESSAGES_URL) {
        const headers = new Headers({ 'Content-Type': 'application/json' })
        headers.set('X-Tether-Transcript-Updated-At', String(daemonVersion))
        return new Response(init?.method === 'HEAD' ? null : daemonTranscript, { status: 200, headers })
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

  /**
   * Force one probe without waiting out TRANSCRIPT_POLL_MS.
   *
   * The watcher checks immediately when the tab comes back, so this exercises the real
   * poll path (same `probe`, same guards) rather than reaching into the module. Fake
   * timers are the alternative and they fight the connect machinery this harness needs.
   */
  const forceProbe = async () => {
    for (const state of ['hidden', 'visible'] as const) {
      Object.defineProperty(document, 'visibilityState', { value: state, configurable: true })
      await act(async () => { document.dispatchEvent(new Event('visibilitychange')) })
    }
    await settle()
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
    daemonTranscript = '[]'
    daemonVersion = 1000
    daemonActivity = {}
    // transcriptWatch is a MODULE, so its loaded-version memory outlives a component
    // tree: without this, the exact request counts below depend on whichever test ran
    // before (its own doc says as much, and this is the only file that drives the real
    // watcher through the real pane).
    resetTranscriptWatchForTests()
    // tether#108 — same argument, second module: the activity poller keeps its last
    // answer, its `answered` flag and its interval at module scope, so without this a
    // previous test's successful poll decides what this one's first render says, and its
    // interval keeps firing inside this one.
    resetSessionActivityForTests()
    globalThis.fetch = stubDaemon() as unknown as typeof fetch
    localStorage.setItem('tether_last_sid', SID)
    useStore.setState({
      messages: [], notices: [], pendingPermissions: [], fatal: null,
      streaming: false, streamingMsgId: null, curTurnId: null,
      sessionId: SID, workspacesLoaded: true, activeWorkspace: null,
    })
  })

  afterEach(() => {
    cleanup()
    resetTranscriptWatchForTests()
    resetSessionActivityForTests()
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

  it('probes the held session, and re-reads NOTHING while nothing has changed', async () => {
    daemonTranscript = JSON.stringify([{ role: 'user', text: 'what the agent said first', ts: 1000 }])
    const { container } = await renderRefusedWithSid(held)
    // The harness's precondition: the refusal really landed. Without this the
    // assertions below would pass just as well on a pane that never reached 'failed'.
    expect(container.querySelectorAll('.failed-card')).toHaveLength(1)

    // Exact counts, not "at least one" — tether#102 measured a property assertion in
    // this very suite keeping a real mutant alive. ONE probe (the watcher is wired, and
    // wired once) and ONE load (the pane's own [sessionId] effect at mount). The second
    // number is the one that costs something: the mount load records the version it
    // received, so the probe that follows has a real baseline and does not spend a full
    // transcript GET learning what that request already knew.
    expect(transcriptProbes()).toBe(1)
    expect(transcriptGets()).toBe(1)
  })

  it('reloads and re-renders when the other agent writes', async () => {
    daemonTranscript = JSON.stringify([{ role: 'user', text: 'what the agent said first', ts: 1000 }])
    await renderRefusedWithSid(held)
    expect(screen.getByText('what the agent said first')).toBeTruthy()
    expect(screen.queryByText('and then this')).toBeNull()

    // The other agent appends. Nothing about the pane changes; only the daemon does.
    daemonTranscript = JSON.stringify([
      { role: 'user', text: 'what the agent said first', ts: 1000 },
      { role: 'assistant', text: 'and then this', ts: 2000 },
    ])
    daemonVersion = 2000

    await forceProbe()

    // The whole feature, end to end and on screen: probe saw a new version, the pane
    // re-read the transcript, and the new turn is rendered without anyone touching
    // anything. Deleting the effect that wires the watcher leaves every unit test in
    // transcriptWatch.test.ts green and fails exactly here.
    expect(transcriptProbes()).toBe(2)
    expect(transcriptGets()).toBe(2)
    expect(screen.getByText('and then this')).toBeTruthy()
    expect(screen.getByText('what the agent said first')).toBeTruthy()
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
    // Seeded on the DAEMON, not just in the store: the pane reloads the transcript on
    // this path, so a store-only fixture would be replaced by the empty body the stub
    // would otherwise serve — and the note is gated on there being a transcript.
    daemonTranscript = JSON.stringify([{ role: 'user', text: 'hello', ts: 1000 }])
    const { container } = await renderRefusedWithSid(held)
    expect(container.querySelectorAll('.msg-user-bubble')).toHaveLength(1)

    const note = container.querySelector('.failed-card .state-card-read')
    expect(note?.textContent).toBe(HELD_SESSION_READABLE_NOTE)
    expect(HELD_SESSION_READABLE_NOTE).not.toContain('as it stood when this pane fetched it')
    expect(HELD_SESSION_READABLE_NOTE).toContain('what tether has of this conversation')
  })

  // ──────────────────────────────────────────────────────────────────────────
  // tether#108 — the state line, wired.
  //
  // This is the hop nothing else can see. `heldActivityLine` is unit-tested in
  // ChatPane.test.tsx and the poller is unit-tested in sessionActivity.test.ts, and both
  // can be perfect while this pane subscribes to neither — which is precisely the state
  // the repo was in at 2ef2f76: the daemon answered this question every three seconds and
  // `grep sessionActivity web/src/panes/chat/index.tsx` returned nothing.

  it('says a turn is in flight when the daemon says the holder is busy', async () => {
    daemonActivity = { [SID]: 'working' }
    daemonTranscript = JSON.stringify([{ role: 'user', text: 'hello', ts: 1000 }])
    const { container } = await renderRefusedWithSid(held)
    expect(container.querySelectorAll('.failed-card')).toHaveLength(1)

    // The whole feature, on screen: deleting the one line that renders
    // <HeldSessionActivity> leaves every unit test green and fails exactly here.
    expect(activityLine()).toBe(HELD_ACTIVITY_WORKING)
    // …and it asked. Exact count: ONE poll, because the poller is shared and the pane is
    // its only subscriber here (the session list is collapsed, so no row is mounted).
    expect(activityPolls()).toBe(1)
  })

  it('says no turn is in flight when the daemon says the holder is idle', async () => {
    daemonActivity = { [SID]: 'idle' }
    await renderRefusedWithSid(held)
    expect(activityLine()).toBe(HELD_ACTIVITY_IDLE)
  })

  it('refuses to guess when the daemon could not read a status', async () => {
    // `held` on the wire, which is what the daemon sends for a live cc process whose
    // record carries no status. Rendering "no turn in flight" here would be the mislabel
    // this whole slice exists to avoid.
    daemonActivity = { [SID]: 'held' }
    await renderRefusedWithSid(held)
    expect(activityLine()).toBe(HELD_ACTIVITY_UNKNOWN)
    expect(activityLine()).not.toBe(HELD_ACTIVITY_IDLE)
  })

  it('reads an EMPTY answer as the hold having ended', async () => {
    // The fourth answer, and the one this state reaches most often: the process that made
    // the daemon refuse the resume has exited. `fatal` is sticky, so the card is still up
    // — and this is the line that tells the reader "Check again" is now worth pressing.
    daemonActivity = {}
    await renderRefusedWithSid(held)
    expect(activityLine()).toBe(HELD_ACTIVITY_GONE)
  })

  it('re-renders the new sentence when the daemon\'s answer changes', async () => {
    // The mutation this test exists for: a line painted once at mount is right for three
    // seconds and then lies, and a stale sentence is indistinguishable from a live one.
    //
    // What it pins is the SUBSCRIPTION — that a fresh answer reaches this pane and repaints
    // it — and not the interval. `forceProbe` drives a poll by hiding and showing the tab,
    // which sessionActivity's own visibilitychange handler answers with an immediate refetch
    // (its "coming back to the tab must not show a marker a whole poll stale" half), so a
    // module whose 3s interval never started would still pass here. The interval itself is
    // pinned in sessionActivity.test.ts, with fake timers this harness cannot use — they
    // fight the connect machinery it needs.
    daemonActivity = { [SID]: 'working' }
    await renderRefusedWithSid(held)
    expect(activityLine()).toBe(HELD_ACTIVITY_WORKING)
    const before = activityPolls()

    // The agent finishes its turn. Nothing about the pane changes; only the daemon does.
    daemonActivity = { [SID]: 'idle' }
    await forceProbe()

    expect(activityPolls()).toBe(before + 1)
    expect(activityLine()).toBe(HELD_ACTIVITY_IDLE)
  })

  it('says NOTHING, while holding its height, until the daemon has answered', async () => {
    // Two properties, and the second is why this card is built the way it is.
    //
    // (a) No sentence is shown before an answer arrives. Absence alone would produce
    //     HELD_ACTIVITY_GONE — a claim about another process, made by default, for one
    //     round trip after every open.
    // (b) The row's HEIGHT is already whatever it will ever be, because all four
    //     sentences are in the DOM from the first render with three of them hidden. jsdom
    //     computes no layout, so the height itself is not assertable here — what IS
    //     assertable is the structure that produces it, and that is what a mutation to
    //     "render only the current line" would break.
    //
    // A request that never settles is what makes the pre-answer frame reachable at all:
    // with a responding daemon, `settle()` drains the poll before any assertion can run.
    globalThis.fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url)
      seen.push({ method: init?.method ?? 'GET', url })
      if (url === SESSION_ACTIVITY_PATH) return new Promise<Response>(() => {}) // never settles
      if (url.includes('/providers')) return new Response(JSON.stringify({ providers: ['claude-code'] }), { status: 200 })
      if (url === MESSAGES_URL) {
        const headers = new Headers({ 'Content-Type': 'application/json' })
        headers.set('X-Tether-Transcript-Updated-At', String(daemonVersion))
        return new Response(init?.method === 'HEAD' ? null : daemonTranscript, { status: 200, headers })
      }
      return new Response('[]', { status: 200, headers: { 'Content-Type': 'application/json' } })
    }) as unknown as typeof fetch

    const { container } = await renderRefusedWithSid(held)
    // The precondition: it really did ask, and really did not get an answer.
    expect(activityPolls()).toBe(1)
    expect(container.querySelector('.state-card-activity')).toBeTruthy()
    expect(activityLine()).toBeNull()
    expect(activitySlots()).toEqual({ all: 4, on: 0 })
  })

  it('keeps all four sentences in the DOM once one of them is showing', async () => {
    // The other half of (b) above: the row that reserved its height before the answer must
    // still be reserving it after, or the answer arriving would shrink it. Exactly one is
    // showing — two would mean the CSS is deciding, which it is not allowed to.
    daemonActivity = { [SID]: 'working' }
    await renderRefusedWithSid(held)
    expect(activityLine()).toBe(HELD_ACTIVITY_WORKING)
    expect(activitySlots()).toEqual({ all: 4, on: 1 })
  })

  it('does not answer from a reading taken before the line appeared', async () => {
    // The stale-answer hazard, and it is reachable rather than theoretical: the poller's
    // last answer and its subscriber set are module state that outlives every unsubscribe.
    // A reader who expands the session list (its rows poll), collapses it (the timer stops,
    // the answer is kept) and then opens a held session would — with a module-level "has
    // ever answered" flag — get the first frame of a sentence beginning "Right now" out of
    // a reading taken an arbitrary time earlier. Since that older answer will not contain
    // this sid, the sentence would be "nothing live is holding this conversation".
    //
    // Seeded here the way an expanded list would seed it: subscribe, let a poll land,
    // unsubscribe. Then the pane's own activity request never settles, so if it were
    // willing to use the stale map it would have to.
    daemonActivity = { 'some-other-session': 'working' }
    const off = subscribeSessionActivity(() => {})
    await settle()
    off()
    // The precondition: the module really is holding an answer that lacks our sid.
    expect(activityPolls()).toBe(1)
    expect(sessionActivityPollerState()).toEqual({ running: false, subscribers: 0 })

    globalThis.fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url)
      seen.push({ method: init?.method ?? 'GET', url })
      if (url === SESSION_ACTIVITY_PATH) return new Promise<Response>(() => {}) // never settles
      if (url.includes('/providers')) return new Response(JSON.stringify({ providers: ['claude-code'] }), { status: 200 })
      if (url === MESSAGES_URL) {
        const headers = new Headers({ 'Content-Type': 'application/json' })
        headers.set('X-Tether-Transcript-Updated-At', String(daemonVersion))
        return new Response(init?.method === 'HEAD' ? null : daemonTranscript, { status: 200, headers })
      }
      return new Response('[]', { status: 200, headers: { 'Content-Type': 'application/json' } })
    }) as unknown as typeof fetch

    await renderRefusedWithSid(held)
    expect(activityLine()).toBeNull()
    expect(activityLine()).not.toBe(HELD_ACTIVITY_GONE)
  })

  it('does not treat a FAILED poll as an answer', async () => {
    // A failure is not an answer, and the direction matters: read as one, absence would
    // become "nothing live is holding this conversation" — i.e. the pane would tell the
    // reader the hold had ended because it could not reach the daemon.
    globalThis.fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url)
      seen.push({ method: init?.method ?? 'GET', url })
      if (url === SESSION_ACTIVITY_PATH) throw new Error('offline')
      if (url.includes('/providers')) return new Response(JSON.stringify({ providers: ['claude-code'] }), { status: 200 })
      if (url === MESSAGES_URL) {
        const headers = new Headers({ 'Content-Type': 'application/json' })
        headers.set('X-Tether-Transcript-Updated-At', String(daemonVersion))
        return new Response(init?.method === 'HEAD' ? null : daemonTranscript, { status: 200, headers })
      }
      return new Response('[]', { status: 200, headers: { 'Content-Type': 'application/json' } })
    }) as unknown as typeof fetch

    await renderRefusedWithSid(held)
    expect(activityPolls()).toBe(1)
    expect(activityLine()).toBeNull()
  })

  it('puts the state line ABOVE the line that points at the transcript', async () => {
    // Reading order is a decision this card makes explicitly: "do I wait or do I leave" is
    // the more urgent of the two questions, and the read-only note answers the other one
    // ("there is something below to read"). Nothing else would notice a swap.
    daemonActivity = { [SID]: 'idle' }
    daemonTranscript = JSON.stringify([{ role: 'user', text: 'hello', ts: 1000 }])
    const { container } = await renderRefusedWithSid(held)
    const card = container.querySelector('.failed-card') as HTMLElement
    const kids = [...card.children].map(e => e.className)
    const activityAt = kids.findIndex(c => c.includes('state-card-activity'))
    const noteAt = kids.findIndex(c => c.includes('state-card-read'))
    // Preconditions: both really are on screen, so this is about order and not presence.
    expect(activityAt).toBeGreaterThan(-1)
    expect(noteAt).toBeGreaterThan(-1)
    expect(activityAt).toBeLessThan(noteAt)
  })

  it('asks NOTHING about activity when the session is CONNECTED', async () => {
    // The cost claim, asserted as a count rather than argued. The wi assumed this poller
    // was already running and the subscription therefore free; it is not — the chat
    // session list is COLLAPSED by default (SessionList's `open` starts false, rows live
    // inside `{open && …}`), so with no held session on screen nothing subscribes and no
    // interval exists. An unconditional hook in this pane would have made it a permanent
    // three-second request for every user.
    ;(globalThis as { WebTransport?: unknown }).WebTransport = FakeWebTransport
    const { container } = render(<ChatPane />)
    await settle()
    // Precondition: really connected. Without it a pane stuck in 'connecting' satisfies
    // the assertion below while testing nothing.
    const box = container.querySelector('.composer-input') as HTMLTextAreaElement
    expect(box.disabled).toBe(false)

    expect(activityPolls()).toBe(0)
    expect(document.querySelector('.state-card-activity')).toBeNull()
  })

  it('asks NOTHING about activity for another terminal code', async () => {
    // The gate is one code's, the same one tether#106 used. A line saying "no turn is in
    // flight in that agent" under `session_owned_by_other` would name an agent that has
    // nothing to do with why the connection was refused.
    const { container } = await renderRefusedWithSid(owned)
    expect(container.querySelectorAll('.failed-card')).toHaveLength(1)
    expect(activityPolls()).toBe(0)
    expect(document.querySelector('.state-card-activity')).toBeNull()
  })

  it('stops polling for activity when the pane unmounts', async () => {
    // The subscription is a mount, so an unmount has to be an unsubscribe — otherwise the
    // interval outlives the card and keeps asking about a session nobody is looking at.
    daemonActivity = { [SID]: 'working' }
    const r = await renderRefusedWithSid(held)
    expect(activityPolls()).toBe(1)

    r.unmount()
    // On the poller's own bookkeeping, not only on a count taken right after the unmount:
    // "still ticking with nobody listening" is invisible to the latter.
    expect(sessionActivityPollerState()).toEqual({ running: false, subscribers: 0 })
  })

  // ──────────────────────────────────────────────────────────────────────────
  // tether#108 — the arrival trace.

  it('marks the message that just arrived, and only that one', async () => {
    daemonTranscript = JSON.stringify([{ role: 'user', text: 'first', ts: 1000 }])
    const { container } = await renderRefusedWithSid(held)
    expect(container.querySelectorAll('.msg-user')).toHaveLength(1)
    // Nothing is traced on the way in: the transcript appearing is not an arrival.
    expect(container.querySelectorAll('.msg-arrived')).toHaveLength(0)

    daemonTranscript = JSON.stringify([
      { role: 'user', text: 'first', ts: 1000 },
      { role: 'assistant', text: 'and then this', ts: 2000 },
    ])
    daemonVersion = 2000
    await forceProbe()

    // Exactly one bubble wears the trace, and it is the new one. Count AND identity: a
    // rule that traced everything new would also produce "at least one".
    const traced = [...container.querySelectorAll('.msg-arrived')]
    expect(traced).toHaveLength(1)
    expect(traced[0].className).toBe('msg-ai msg-arrived')
    expect(traced[0].textContent).toContain('and then this')
    // The one that was already there is untouched — including its class, which is what
    // the CSS keys on.
    expect((container.querySelector('.msg-user') as HTMLElement).className).toBe('msg-user')
  })

  it('takes the trace back off, so it is an event and not a state', async () => {
    // The distinction the wi turns on: a spinner says "you are waiting" and stays, a
    // trace says "one just landed" and stops. A class that never came off would leave
    // every message ever received highlighted.
    daemonTranscript = JSON.stringify([{ role: 'user', text: 'first', ts: 1000 }])
    const { container } = await renderRefusedWithSid(held)
    daemonTranscript = JSON.stringify([
      { role: 'user', text: 'first', ts: 1000 },
      { role: 'assistant', text: 'and then this', ts: 2000 },
    ])
    daemonVersion = 2000
    await forceProbe()
    expect(container.querySelectorAll('.msg-arrived')).toHaveLength(1)

    await act(async () => { await new Promise(resolve => setTimeout(resolve, ARRIVAL_TRACE_MS + 20)) })
    expect(container.querySelectorAll('.msg-arrived')).toHaveLength(0)
  })

  it('does not trace the whole transcript when a disjoint refresh replaces it', async () => {
    // tether#107's fallback: over a megabyte written between two probes means the windows
    // do not overlap, mergeHistory reports it, and loadHistory replaces the array — so
    // every id is new and at the end. That is a replacement, not twenty arrivals, and it
    // already announces itself by visibly jumping.
    daemonTranscript = JSON.stringify([
      { role: 'user', text: 'old-a', ts: 1000 },
      { role: 'user', text: 'old-b', ts: 1001 },
    ])
    const { container } = await renderRefusedWithSid(held)
    expect(container.querySelectorAll('.msg-user')).toHaveLength(2)

    daemonTranscript = JSON.stringify([
      { role: 'user', text: 'new-a', ts: 5000 },
      { role: 'user', text: 'new-b', ts: 5001 },
    ])
    daemonVersion = 2000
    await forceProbe()

    // The precondition: it really did replace rather than append.
    expect([...container.querySelectorAll('.msg-user-bubble')].map(e => e.textContent))
      .toEqual(['new-a', 'new-b'])
    expect(container.querySelectorAll('.msg-arrived')).toHaveLength(0)
  })

  it('does not move a reader who has scrolled up when a traced message arrives', async () => {
    // tether#106's property, re-pinned on the commit tether#108 adds. The trace fires on
    // the same commit the autoscroll effect reads its geometry from, and it sets React
    // state from an effect — so this is the assertion that says the extra render cannot
    // become a scroll.
    daemonTranscript = JSON.stringify(
      Array.from({ length: 10 }, (_, i) => ({ role: 'user', text: `old-${i}`, ts: 1000 + i })),
    )
    const { container } = await renderRefusedWithSid(held)
    const box = fakeScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)
    expect(box.height()).toBe(1000)
    box.scrollTo(0) // reading the top of the conversation

    daemonTranscript = JSON.stringify([
      ...Array.from({ length: 10 }, (_, i) => ({ role: 'user', text: `old-${i}`, ts: 1000 + i })),
      { role: 'assistant', text: 'arrived', ts: 9000 },
    ])
    daemonVersion = 2000
    await forceProbe()

    // The precondition: the trace really is on screen, so this test is about the commit
    // that carries it rather than about a quiet one.
    expect(container.querySelectorAll('.msg-arrived')).toHaveLength(1)
    expect(box.top()).toBe(0)

    // The CONTROL, so this assertion is known to discriminate: a reader at the bottom
    // still follows the conversation, trace or no trace.
    box.scrollTo(box.height() - 300)
    daemonTranscript = JSON.stringify([
      ...Array.from({ length: 10 }, (_, i) => ({ role: 'user', text: `old-${i}`, ts: 1000 + i })),
      { role: 'assistant', text: 'arrived', ts: 9000 },
      { role: 'assistant', text: 'and again', ts: 9001 },
    ])
    daemonVersion = 3000
    await forceProbe()
    expect(box.top()).toBe(box.height())
  })

  it('does NOT trace an arrival in a connected session', async () => {
    // The trace's gate, and the one this suite could not see until a mutation battery
    // pointed it out: dropping `readingHeldSession` from the trace effect survived
    // everything else here.
    //
    // It matters because the connected pane is where the arrivals are the READER'S OWN.
    // A live session announces motion already — the thinking dots, the streaming answer
    // body, and the send the user just made — so a highlight there flashes the user's own
    // message back at them and says a stranger's turn just landed.
    ;(globalThis as { WebTransport?: unknown }).WebTransport = FakeWebTransport
    daemonTranscript = JSON.stringify([{ role: 'user', text: 'earlier prompt', ts: 1000 }])
    const { container } = render(<ChatPane />)
    await settle()
    // Preconditions: really connected, and a non-empty transcript on screen — so the next
    // message is an APPEND rather than the repopulation the other rule drops anyway.
    const box = container.querySelector('.composer-input') as HTMLTextAreaElement
    expect(box.disabled).toBe(false)
    expect(container.querySelectorAll('.msg-user')).toHaveLength(1)

    await act(async () => {
      useStore.getState().addMessage({ id: 'live-1', role: 'assistant', text: 'a live reply', ts: 9000 })
    })

    // It arrived and rendered…
    expect(container.querySelectorAll('.msg-ai')).toHaveLength(1)
    expect(screen.getByText('a live reply')).toBeTruthy()
    // …and wears nothing.
    expect(container.querySelectorAll('.msg-arrived')).toHaveLength(0)
  })

  it('keeps the ids of everything already on screen when a traced message arrives', async () => {
    // The identity property tether#106 shipped, re-pinned with the trace on screen:
    // `key={m.id}` is React's reconciliation key and both expansion Sets are keyed by
    // message id, so a change that re-minted ids to mark arrivals would collapse the
    // reader's expansions every three seconds — which is exactly what tether#106 removed.
    daemonTranscript = JSON.stringify([{ role: 'user', text: 'first', ts: 1000 }])
    await renderRefusedWithSid(held)
    const idsBefore = useStore.getState().messages.map(m => m.id)
    expect(idsBefore).toHaveLength(1)

    daemonTranscript = JSON.stringify([
      { role: 'user', text: 'first', ts: 1000 },
      { role: 'assistant', text: 'and then this', ts: 2000 },
    ])
    daemonVersion = 2000
    await forceProbe()

    const after = useStore.getState().messages
    expect(after.map(m => m.id).slice(0, 1)).toEqual(idsBefore)
    expect(new Set(after.map(m => m.id)).size).toBe(after.length)
  })

  it('still renders the top-of-transcript marker with the activity line on screen', async () => {
    // The two features tether#108 puts in the same card as tether#107's marker, together,
    // because "the marker is unconditional under messages.length > 0" is a property this
    // wi must not quietly re-condition.
    daemonActivity = { [SID]: 'working' }
    daemonTranscript = JSON.stringify([{ role: 'user', text: 'hello', ts: 1000 }])
    const { container } = await renderRefusedWithSid(held)

    expect(activityLine()).toBe(HELD_ACTIVITY_WORKING)
    expect(container.querySelector('.transcript-top .transcript-top-note')?.textContent)
      .toBe(TRANSCRIPT_START_COMPLETE)
  })

  it('renders the activity line even with NO transcript, unlike the read-only note', async () => {
    // Deliberately not gated on transcript.length. The note below it makes a claim about
    // the screen and needs something on it; this makes a claim about the other process,
    // and an empty transcript is precisely where a reader most needs to know whether
    // waiting will produce anything.
    daemonActivity = { [SID]: 'working' }
    daemonTranscript = '[]'
    const { container } = await renderRefusedWithSid(held)

    expect(container.querySelectorAll('.msg-user, .msg-ai')).toHaveLength(0)
    expect(container.querySelector('.state-card-read')).toBeNull()
    expect(activityLine()).toBe(HELD_ACTIVITY_WORKING)
  })
})

// ────────────────────────────────────────────────────────────────────────────
// tether#107 — reading BACKWARDS, in the pane, with a scroll container that moves.
//
// Everything below the store and the loader is unit-tested elsewhere (store.test.ts,
// session.test.ts). What NONE of that can see is the three properties tether#106
// shipped and this wi had to keep, because all three are about a mounted pane with a
// real scroll box and two pages of transcript in it:
//
//   1. `nearBottom` still protects a reader who has scrolled up, and the prepend must
//      not yank them down either;
//   2. prepending must not disturb the ids of messages already on screen (React
//      reconciles on `key={m.id}` and both expansion Sets are keyed by it);
//   3. the three-second refresh must not silently discard pages the reader loaded.
//
// jsdom has no layout: `scrollHeight` and `clientHeight` are 0 and `scrollTop` never
// moves, so every scroll assertion in this repo before now was impossible. fakeScrollBox
// installs a geometry MODEL on the element — height derived from the bubbles actually in
// the DOM, so it grows and shrinks with the transcript by itself — which is what makes
// "the reader was moved by exactly the height that was inserted above them" an assertion
// rather than a hope.
describe('paging a transcript backwards (tether#107)', () => {
  const SID = 'sid-paged-0001'
  const MESSAGES_URL = `/api/v1/sessions/${SID}/messages`

  /** Per-URL daemon. The transcript route answers with real Headers so the boundary
   *  facts travel exactly as they do in production. */
  type Page = { entries: unknown[]; earlier?: number; otherRecord?: string }
  let pages: Record<string, Page> = {}
  let requested: string[] = []

  function stubDaemon() {
    return vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url)
      requested.push(`${init?.method ?? 'GET'} ${url}`)
      if (url.includes('/providers')) return new Response(JSON.stringify({ providers: ['claude-code'] }), { status: 200 })
      // tether#108 — see the other harness: an array answer would be read as `{}`, i.e.
      // as the daemon positively saying nothing holds this sid.
      if (url === SESSION_ACTIVITY_PATH) return new Response('{}', { status: 200, headers: { 'Content-Type': 'application/json' } })
      const page = pages[url]
      if (page) {
        const headers = new Headers({ 'Content-Type': 'application/json' })
        headers.set('X-Tether-Transcript-Updated-At', String(daemonVersion))
        if (page.earlier !== undefined) headers.set('X-Tether-Transcript-Earlier', String(page.earlier))
        if (page.otherRecord !== undefined) headers.set('X-Tether-Transcript-Other-Record', page.otherRecord)
        return new Response(init?.method === 'HEAD' ? null : JSON.stringify(page.entries), { status: 200, headers })
      }
      return new Response('[]', { status: 200, headers: { 'Content-Type': 'application/json' } })
    })
  }
  let daemonVersion = 1000

  // `ord` defaults to `ts` so every tether#106/#107 case below reads as it was written.
  // A convenience of this fixture, not a fact about the wire: the daemon's ord is a byte
  // position and its ts is a clock. tether#109's cases pass the two separately, because
  // its whole mechanism is a ts that moves while the conversation does not.
  const entry = (role: string, text: string, ts: number, ord: number = ts) => ({ role, text, ts, ord })

  const settle = async () => {
    for (let i = 0; i < 5; i++) {
      await act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)) })
    }
  }

  beforeEach(() => {
    localStorage.clear()
    pages = {}
    requested = []
    daemonVersion = 1000
    resetTranscriptWatchForTests()
    resetSessionActivityForTests()
    globalThis.fetch = stubDaemon() as unknown as typeof fetch
    // No remembered sid and workspaces unsettled ⇒ the first connect is DEFERRED
    // behind a store subscription and a 2s timer, neither of which fires here. That is
    // the harness's precondition and it is asserted below, because a pane that started
    // connecting would be exercising a different path than these assertions describe.
    useStore.setState({
      messages: [], notices: [], pendingPermissions: [], fatal: null,
      streaming: false, streamingMsgId: null, curTurnId: null,
      sessionId: SID, workspacesLoaded: false, activeWorkspace: null,
      transcriptEarlier: null, transcriptOtherRecord: null, transcriptPagesBack: 0,
    })
    expect(localStorage.getItem('tether_last_sid')).toBeNull()
    expect((globalThis as { WebTransport?: unknown }).WebTransport).toBeUndefined()
  })

  afterEach(() => {
    cleanup()
    resetTranscriptWatchForTests()
    resetSessionActivityForTests()
    globalThis.fetch = originalFetch
    useStore.setState({
      messages: [], notices: [], sessionId: null, fatal: null, streaming: false, workspacesLoaded: false,
      transcriptEarlier: null, transcriptOtherRecord: null, transcriptPagesBack: 0,
    })
    localStorage.clear()
  })

  /** n uniquely-numbered user turns, oldest first, stamped from `firstTs`. */
  const turns = (label: string, n: number, firstTs: number) =>
    Array.from({ length: n }, (_, i) => entry('user', `${label}-${i}`, firstTs + i))

  // ── 1. the top-of-transcript marker, all three states ─────────────────────
  it('offers to load earlier messages when the daemon says there are some', async () => {
    pages[MESSAGES_URL] = { entries: turns('recent', 3, 500), earlier: 1048576 }
    render(<ChatPane />)
    await settle()

    const btn = document.querySelector('.transcript-top .transcript-more') as HTMLButtonElement
    expect(btn).toBeTruthy()
    expect(btn.textContent).toBe('load earlier messages')
    expect(btn.disabled).toBe(false)
    // …and NOT the "you have reached the beginning" line, which is the false claim this
    // whole wi exists to stop.
    expect(document.querySelector('.transcript-top-note')).toBeNull()
  })

  it('says this is the beginning when the daemon sends no cursor', async () => {
    pages[MESSAGES_URL] = { entries: turns('all', 2, 500) }
    render(<ChatPane />)
    await settle()

    const note = document.querySelector('.transcript-top .transcript-top-note')
    expect(note?.textContent).toBe(TRANSCRIPT_START_COMPLETE)
    expect(note?.textContent).toBe('the beginning of this conversation')
    expect(document.querySelector('.transcript-more')).toBeNull()
  })

  it('says whose record it is the beginning of when another store has one too', async () => {
    // The population this covers is EMPTY on the reference machine (tether#107's
    // measurement), and the mechanism is real: tether's own history wins whenever it
    // exists, and a terminal-launched background job writes cc's transcript instead. So
    // reaching the top of tether's short record is not reaching the beginning of the
    // conversation, and saying so would be false.
    pages[MESSAGES_URL] = { entries: turns('tether', 2, 500), otherRecord: 'cc' }
    render(<ChatPane />)
    await settle()

    const note = document.querySelector('.transcript-top .transcript-top-note')
    expect(note?.textContent).toBe(TRANSCRIPT_START_TETHER_RECORD_ONLY)
    // The claim it must NOT make.
    expect(note?.textContent).not.toBe(TRANSCRIPT_START_COMPLETE)
    expect(note?.textContent).toContain('Claude Code')
  })

  it('renders no marker at all above an empty transcript', async () => {
    // "The beginning of this conversation" above nothing is a new false claim, and an
    // empty transcript is a NORMAL answer here: SessionIndex.MessagePage returns
    // SourceNone with no messages for a sid neither store has, which is what
    // openSession fetches for a session created moments ago.
    pages[MESSAGES_URL] = { entries: [] }
    render(<ChatPane />)
    await settle()
    expect(document.querySelector('.transcript-top')).toBeNull()
  })

  it('renders no marker when the only thing on screen is a locally-originated notice', async () => {
    // The gate is on `messages`, not on `transcript`, and this is what makes the
    // difference observable: mergeTranscript projects a notice into the rendered
    // transcript, so a pane gated on `transcript.length` would announce "the beginning
    // of this conversation" above a daemon banner and no conversation at all. Found by
    // a mutation battery — the empty case above passes either way, because with no
    // notices the two lengths are equal.
    pages[MESSAGES_URL] = { entries: [] }
    useStore.setState({
      notices: [{ id: 'n1', text: 'the previous context could not be restored', ts: 1000, kind: 'session' }],
    })
    const { container } = render(<ChatPane />)
    await settle()

    // The precondition: the notice really is rendered, so the two gates really do
    // disagree here.
    expect(container.querySelectorAll('.msg-system-text')).toHaveLength(1)
    expect(useStore.getState().messages).toHaveLength(0)
    expect(document.querySelector('.transcript-top')).toBeNull()
  })

  // ── 2. the click, end to end ──────────────────────────────────────────────
  it('renders the earlier page above the one on screen, and moves the cursor back', async () => {
    pages[MESSAGES_URL] = { entries: turns('recent', 3, 500), earlier: 4096 }
    pages[`${MESSAGES_URL}?before=4096`] = { entries: turns('older', 2, 100), earlier: 2048 }
    const { container } = render(<ChatPane />)
    await settle()
    expect(screen.queryByText('older-0')).toBeNull()

    await act(async () => { (container.querySelector('.transcript-more') as HTMLButtonElement).click() })
    await settle()

    // On screen, in order: the older page above the recent one.
    const texts = [...container.querySelectorAll('.msg-user-bubble')].map(e => e.textContent)
    expect(texts).toEqual(['older-0', 'older-1', 'recent-0', 'recent-1', 'recent-2'])
    // The request actually carried the cursor.
    expect(requested).toContain(`GET ${MESSAGES_URL}?before=4096`)
    // …and the button is still there, now pointing one page further back. Exact value:
    // a cursor that stayed at 4096 would re-serve this page forever while looking like
    // it worked.
    expect(useStore.getState().transcriptEarlier).toBe(2048)
    expect((container.querySelector('.transcript-more') as HTMLButtonElement).textContent).toBe('load earlier messages')
  })

  it('keeps the ids of the messages already on screen when a page is prepended', async () => {
    // THE identity property. `key={m.id}` is what React reconciles on and both
    // expansion Sets are keyed by message id, so re-minting an id collapses every
    // expanded block and clamps the reader's scroll — the damage tether#106 removed
    // from the reload path.
    pages[MESSAGES_URL] = { entries: turns('recent', 3, 500), earlier: 4096 }
    pages[`${MESSAGES_URL}?before=4096`] = { entries: turns('older', 2, 100) }
    const { container } = render(<ChatPane />)
    await settle()
    const idsBefore = useStore.getState().messages.map(m => m.id)
    expect(idsBefore).toHaveLength(3)

    await act(async () => { (container.querySelector('.transcript-more') as HTMLButtonElement).click() })
    await settle()

    const after = useStore.getState().messages
    expect(after.map(m => m.id).slice(2)).toEqual(idsBefore)
    expect(new Set(after.map(m => m.id)).size).toBe(after.length)
  })

  // ── 3. scroll: the reader stays where they are ─────────────────────────────
  it('does not yank a reader who has scrolled up down to a new message', async () => {
    // tether#106's property, re-pinned here because tether#107 adds a second effect
    // that writes scrollTop on the same commits. Nothing in the repo scrolled a chat
    // container before this file, so `nearBottom` has never actually been exercised.
    pages[MESSAGES_URL] = { entries: turns('recent', 10, 500) }
    const { container } = render(<ChatPane />)
    await settle()
    const box = fakeScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)
    expect(box.height()).toBe(1000)

    box.scrollTo(0) // reading the top of the conversation
    await act(async () => { useStore.getState().addMessage({ id: 'new-1', role: 'user', text: 'arrived', ts: 9000 }) })
    expect(box.top()).toBe(0)

    // The CONTROL, so this assertion is known to discriminate: a reader who IS at the
    // bottom still follows the conversation.
    box.scrollTo(box.height() - 300)
    await act(async () => { useStore.getState().addMessage({ id: 'new-2', role: 'user', text: 'and again', ts: 9001 }) })
    expect(box.top()).toBe(box.height())
  })

  it('moves the reader down by exactly the height inserted above them', async () => {
    pages[MESSAGES_URL] = { entries: turns('recent', 10, 500), earlier: 4096 }
    pages[`${MESSAGES_URL}?before=4096`] = { entries: turns('older', 5, 100) }
    const { container } = render(<ChatPane />)
    await settle()
    const box = fakeScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)
    expect(box.height()).toBe(1000)
    box.scrollTo(200)

    await act(async () => { (container.querySelector('.transcript-more') as HTMLButtonElement).click() })
    await settle()

    // 5 bubbles × 100px went in above the reader, so they move from 200 to 700 and are
    // looking at the same message. Exact, not "greater than": every wrong answer here
    // is also greater than 200.
    expect(box.height()).toBe(1500)
    expect(box.top()).toBe(700)
    // Specifically NOT the bottom, which is what the autoscroll effect would have done.
    expect(box.top()).not.toBe(1500)
  })

  it('does not autoscroll on the commit that lands an earlier page, even when nearBottom is true', async () => {
    // The case the one-shot skip flag exists for, and the only one where the two
    // effects disagree: a reader near the BOTTOM of a short transcript can still reach
    // the button at the top of the container. Without the flag, the autoscroll effect
    // fires on the same commit, computes nearBottom from the corrected geometry
    // (900 - 600 - 300 = 0 < 120) and snaps to 900 — the bottom of the page they just
    // asked to see the top of.
    pages[MESSAGES_URL] = { entries: turns('recent', 4, 500), earlier: 4096 }
    pages[`${MESSAGES_URL}?before=4096`] = { entries: turns('older', 5, 100) }
    const { container } = render(<ChatPane />)
    await settle()
    const box = fakeScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)
    expect(box.height()).toBe(400)
    box.scrollTo(100) // at the bottom: 400 - 100 - 300 = 0, i.e. nearBottom
    // The harness's precondition: nearBottom really is true here, so this test is about
    // the flag and not about a geometry that happens to be safe anyway.
    expect(box.height() - box.top() - 300).toBeLessThan(120)

    await act(async () => { (container.querySelector('.transcript-more') as HTMLButtonElement).click() })
    await settle()

    expect(box.height()).toBe(900)
    expect(box.top()).toBe(600)
    expect(box.top()).not.toBe(900)

    // …and the flag is ONE-SHOT: the very next arriving message autoscrolls as it
    // always has. A flag that stayed set would silently disable following the
    // conversation for the rest of the session.
    box.scrollTo(600) // still at the bottom of 900
    await act(async () => { useStore.getState().addMessage({ id: 'new-1', role: 'user', text: 'arrived', ts: 9000 }) })
    expect(box.top()).toBe(1000)
  })

  // ── 4. the refresh, with two pages loaded ─────────────────────────────────
  it('keeps the pages the reader loaded when the three-second probe fires', async () => {
    // The interaction the wi names, and the one no other test in this repo can reach:
    // tether#106's watcher reloads through loadHistory, which REPLACES the array. Left
    // alone it would throw away every earlier page, every three seconds, while the
    // reader is reading them.
    //
    // The watcher only runs in the held-session state, so this one drives the pane to
    // it: workspaces settled ⇒ mount connects, jsdom has no WebTransport ⇒ the connect
    // rejects, and the refusal landed on the store makes connState 'failed'.
    useStore.setState({ workspacesLoaded: true })
    localStorage.setItem('tether_last_sid', SID)
    pages[MESSAGES_URL] = { entries: turns('recent', 3, 500), earlier: 4096 }
    pages[`${MESSAGES_URL}?before=4096`] = { entries: turns('older', 2, 100), earlier: 2048 }

    const { container } = render(<ChatPane />)
    useStore.setState({ fatal: { code: ErrCodeSessionHeldByBackgroundAgent, message: 'a background agent is using this conversation' } })
    await settle()
    // Preconditions, asserted: the refusal landed AND the watcher is running. Without
    // both, everything below passes on a pane that never probes anything.
    expect(container.querySelectorAll('.failed-card')).toHaveLength(1)
    expect(requested.filter(r => r === `HEAD ${MESSAGES_URL}`).length).toBeGreaterThan(0)

    await act(async () => { (container.querySelector('.transcript-more') as HTMLButtonElement).click() })
    await settle()
    expect([...container.querySelectorAll('.msg-user-bubble')].map(e => e.textContent))
      .toEqual(['older-0', 'older-1', 'recent-0', 'recent-1', 'recent-2'])
    const idsBefore = useStore.getState().messages.map(m => m.id)

    // The other agent writes: same window, one more turn.
    pages[MESSAGES_URL] = { entries: [...turns('recent', 3, 500), entry('assistant', 'brand new', 900)], earlier: 5120 }
    daemonVersion = 2000
    for (const state of ['hidden', 'visible'] as const) {
      Object.defineProperty(document, 'visibilityState', { value: state, configurable: true })
      await act(async () => { document.dispatchEvent(new Event('visibilitychange')) })
    }
    await settle()

    // The reader's page survived and the new turn arrived.
    expect([...container.querySelectorAll('.msg-user-bubble')].map(e => e.textContent))
      .toEqual(['older-0', 'older-1', 'recent-0', 'recent-1', 'recent-2'])
    expect(screen.getByText('brand new')).toBeTruthy()
    // Identity survived for everything that was already on screen.
    expect(useStore.getState().messages.map(m => m.id).slice(0, 5)).toEqual(idsBefore)
    // …and the cursor still describes the OLDEST page on screen. Taking the refresh's
    // 5120 would send the next click forward, to re-serve pages already rendered.
    expect(useStore.getState().transcriptEarlier).toBe(2048)
  })

  it('does not put tether#108\'s arrival trace on a page the reader asked for', async () => {
    // The cross-feature case, and the only place both wis are on screen at once: the trace
    // exists only in the held state, which is also the only state that pages backwards
    // while something writes. An older page is not an arrival — the reader fetched it —
    // and flashing 25 bubbles they deliberately asked for would say they had just landed.
    //
    // trailingArrivals' walk-from-the-end shape is what makes this hold, and the unit test
    // in ChatPane.test.tsx pins the rule; this pins that the pane uses it on the real path.
    useStore.setState({ workspacesLoaded: true })
    localStorage.setItem('tether_last_sid', SID)
    pages[MESSAGES_URL] = { entries: turns('recent', 3, 500), earlier: 4096 }
    pages[`${MESSAGES_URL}?before=4096`] = { entries: turns('older', 2, 100), earlier: 2048 }

    const { container } = render(<ChatPane />)
    useStore.setState({ fatal: { code: ErrCodeSessionHeldByBackgroundAgent, message: 'a background agent is using this conversation' } })
    await settle()
    // Preconditions: the refusal landed (so the trace is armed at all) and nothing is
    // traced yet. Without the first one this test would pass on a pane where the feature
    // is switched off entirely.
    expect(container.querySelectorAll('.failed-card')).toHaveLength(1)
    expect(container.querySelectorAll('.msg-arrived')).toHaveLength(0)

    await act(async () => { (container.querySelector('.transcript-more') as HTMLButtonElement).click() })
    await settle()

    // The page really did land…
    expect([...container.querySelectorAll('.msg-user-bubble')].map(e => e.textContent))
      .toEqual(['older-0', 'older-1', 'recent-0', 'recent-1', 'recent-2'])
    // …and none of it is wearing a trace.
    expect(container.querySelectorAll('.msg-arrived')).toHaveLength(0)
  })

  // ── 6. tether#109, on the real path, with the reader's geometry ────────────
  //
  // The reported defect end to end: a probe whose newest window opens one record further
  // into the assistant turn at its leading edge. cc stamps a merged turn with its FIRST
  // fragment's time, so that turn arrives with a later ts and a key nothing on screen
  // has; tether#107 appended it, and the owner photographed a bubble 3h16m older sitting
  // under the newest one.
  //
  // Written at the PANE rather than only at the reducer because the two things this must
  // not cost are pane-level: the reader's place in the scroll box, and tether#108's
  // arrival trace. Both are invisible to a store test.
  const recut = {
    edgeFull: Date.parse('2026-08-19T03:41:45.555Z'),
    edgeRecut: Date.parse('2026-08-19T03:41:49.404Z'),
    newest: Date.parse('2026-08-19T07:18:29.315Z'),
  }

  /** Drive the pane into the held state with one earlier page loaded.
   *
   * SIX bubbles, and the count is load-bearing rather than arbitrary: `fakeScrollBox`
   * models `scrollHeight` as rows × 100 and the tests below set `clientHeight` to 300, so
   * a reader at `scrollTop = 0` is "not near the bottom" only once
   * `scrollHeight - clientHeight > 120`, i.e. from 5 rows up. A 3-bubble fixture makes
   * `scrollHeight - scrollTop - clientHeight` exactly 0 — which IS `nearBottom` — so the
   * scroll assertion would have been vacuous while its comment claimed the opposite.
   * Found by review; the same shape the pre-existing 10-row scroll test already uses. */
  async function heldWithAPageLoaded() {
    useStore.setState({ workspacesLoaded: true })
    localStorage.setItem('tether_last_sid', SID)
    pages[MESSAGES_URL] = {
      entries: [
        entry('assistant', 'the whole turn, first fragment onwards', recut.edgeFull, 122154092),
        entry('user', 'filler-a', recut.edgeFull + 1000, 122500000),
        entry('user', 'filler-b', recut.edgeFull + 2000, 122600000),
        entry('user', 'filler-c', recut.edgeFull + 3000, 122700000),
        entry('user', 'the newest turn', recut.newest, 123197925),
      ],
      earlier: 122154091,
    }
    pages[`${MESSAGES_URL}?before=122154091`] = {
      entries: [entry('user', 'older-0', recut.edgeFull - 3600000, 121000000)],
      earlier: 120000000,
    }
    const { container } = render(<ChatPane />)
    useStore.setState({ fatal: { code: ErrCodeSessionHeldByBackgroundAgent, message: 'a background agent is using this conversation' } })
    await settle()
    expect(container.querySelectorAll('.failed-card')).toHaveLength(1)
    await act(async () => { (container.querySelector('.transcript-more') as HTMLButtonElement).click() })
    await settle()
    return container
  }

  /** One turn of the three-second probe. */
  async function probe() {
    daemonVersion += 1000
    for (const state of ['hidden', 'visible'] as const) {
      Object.defineProperty(document, 'visibilityState', { value: state, configurable: true })
      await act(async () => { document.dispatchEvent(new Event('visibilitychange')) })
    }
    await settle()
  }

  it('does not move a reader who has scrolled up when the window re-cuts its leading bubble', async () => {
    const container = await heldWithAPageLoaded()
    const box = fakeScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)
    expect(box.height()).toBe(600) // six bubbles
    box.scrollTo(0) // reading the page they just asked for
    // The precondition that makes the scroll assertion discriminating: the reader is NOT
    // near the bottom — 600 - 0 - 300 = 300, well over the pane's 120px threshold — so an
    // autoscroll would be visible as a jump to 600.
    expect(box.height() - box.top() - 300).toBe(300)
    expect(box.height() - box.top() - 300).toBeGreaterThan(120)

    pages[MESSAGES_URL] = {
      entries: [
        entry('assistant', 'minus its first fragment', recut.edgeRecut, 122155976),
        entry('user', 'filler-a', recut.edgeFull + 1000, 122500000),
        entry('user', 'filler-b', recut.edgeFull + 2000, 122600000),
        entry('user', 'filler-c', recut.edgeFull + 3000, 122700000),
        entry('user', 'the newest turn', recut.newest, 123197925),
      ],
      earlier: 122155975,
    }
    await probe()

    // The transcript is unchanged and IN ORDER: the re-cut bubble did not land at the end
    // under a bubble three hours newer.
    expect([...container.querySelectorAll('.msg-user-bubble')].map(e => e.textContent))
      .toEqual(['older-0', 'filler-a', 'filler-b', 'filler-c', 'the newest turn'])
    expect(useStore.getState().messages.map(m => m.text)).toEqual([
      'older-0', 'the whole turn, first fragment onwards', 'filler-a', 'filler-b', 'filler-c', 'the newest turn',
    ])
    // The reader is where they were, and the cursor still describes the oldest page.
    expect(box.top()).toBe(0)
    expect(useStore.getState().transcriptEarlier).toBe(120000000)
    expect(useStore.getState().transcriptPagesBack).toBe(1)
  })

  it('marks nothing as arrived when the probe only re-cut a bubble the reader already has', async () => {
    // tether#108's trace says "one just landed", which is an event. A window that slid
    // over a turn is not an arrival — nothing was written that the reader cannot already
    // see — so a trace here would be the feature lying about the only thing it says.
    const container = await heldWithAPageLoaded()
    expect(container.querySelectorAll('.msg-arrived')).toHaveLength(0)

    const recutPage = [
      entry('assistant', 'minus its first fragment', recut.edgeRecut, 122155976),
      entry('user', 'filler-a', recut.edgeFull + 1000, 122500000),
      entry('user', 'filler-b', recut.edgeFull + 2000, 122600000),
      entry('user', 'filler-c', recut.edgeFull + 3000, 122700000),
      entry('user', 'the newest turn', recut.newest, 123197925),
    ]
    pages[MESSAGES_URL] = { entries: recutPage, earlier: 122155975 }
    await probe()
    expect(container.querySelectorAll('.msg-arrived')).toHaveLength(0)

    // The CONTROL, so the assertion above is known to discriminate: a turn that really
    // did arrive, in the same state, over the same path, IS traced.
    pages[MESSAGES_URL] = {
      entries: [...recutPage, entry('user', 'what the other agent just wrote', recut.newest + 60000, 123205000)],
      earlier: 122155975,
    }
    await probe()
    expect([...container.querySelectorAll('.msg-arrived')].map(e => e.textContent?.includes('what the other agent just wrote')))
      .toEqual([true])
  })

  it('still offers the top-of-transcript states while a re-cut page is merged in', async () => {
    // tether#107's three tops render unconditionally on messages.length > 0. A merge that
    // skipped a bubble must not be able to empty the transcript or drop the cursor, which
    // is what would take the button off the screen and leave the reader at a ceiling with
    // nothing on it again.
    const container = await heldWithAPageLoaded()
    expect(container.querySelector('.transcript-more')).toBeTruthy()

    pages[MESSAGES_URL] = {
      entries: [
        entry('assistant', 'minus its first fragment', recut.edgeRecut, 122155976),
        entry('user', 'filler-a', recut.edgeFull + 1000, 122500000),
        entry('user', 'filler-b', recut.edgeFull + 2000, 122600000),
        entry('user', 'filler-c', recut.edgeFull + 3000, 122700000),
        entry('user', 'the newest turn', recut.newest, 123197925),
      ],
      earlier: 122155975,
    }
    await probe()

    expect(container.querySelector('.transcript-more')).toBeTruthy()
    // …and the button is there because the MERGE kept the reader's page, not because a
    // fallback installed a fresh one that happens to carry a cursor too. Review found the
    // earlier version of this test could not tell those apart: `loadHistory` + the new
    // page's own `earlier` renders an identical button. `pagesBack` and the prepended
    // bubble are what distinguish them — the fallback zeroes the first and drops the
    // second.
    expect(useStore.getState().transcriptPagesBack).toBe(1)
    expect(container.querySelector('.transcript-more')?.textContent).toBe('load earlier messages')
    expect(useStore.getState().messages.map(m => m.text)).toContain('older-0')
    expect(useStore.getState().transcriptEarlier).toBe(120000000)
  })
})

// ── tether#110 — both ends of the transcript load by being scrolled to ────────────
//
// Three things this file could not say before, and each one is a loop or a lie:
//
//   1. arriving at an end must start a request, and arriving at it AGAIN without having
//      left must not — because the correction that follows a prepend is itself a scroll,
//      and the autoscroll that follows an append is itself a scroll;
//   2. the bottom must go through the SAME refresh path tether#109 taught to check its
//      ordering, not a second one beside it;
//   3. the dots must exist exactly while a request does, and never at a ceiling.
//
// `liveScrollBox` is what makes (1) expressible at all: jsdom fires no scroll event for a
// `scrollTop` write, so with the plain `fakeScrollBox` a pane with NO latch and a pane
// with a correct one produce identical numbers.
describe('loading a transcript by scrolling to its ends (tether#110)', () => {
  const SID = 'sid-edges-0001'
  const MESSAGES_URL = `/api/v1/sessions/${SID}/messages`

  type Page = { entries: unknown[]; earlier?: number; otherRecord?: string }
  let pages: Record<string, Page> = {}
  let requested: string[] = []
  let daemonVersion = 1000
  /** URLs whose response is held open until the test releases it, so the in-flight state
   *  is observable rather than inferred. Checked at REQUEST time, so one can be installed
   *  after the mount fetches have already gone through. */
  let gates: Record<string, Promise<void>> = {}

  function stubDaemon() {
    return vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url)
      requested.push(`${init?.method ?? 'GET'} ${url}`)
      if (url.includes('/providers')) return new Response(JSON.stringify({ providers: ['claude-code'] }), { status: 200 })
      if (url === SESSION_ACTIVITY_PATH) return new Response('{}', { status: 200, headers: { 'Content-Type': 'application/json' } })
      const gate = gates[`${init?.method ?? 'GET'} ${url}`]
      if (gate) await gate
      const page = pages[url]
      if (page) {
        const headers = new Headers({ 'Content-Type': 'application/json' })
        headers.set('X-Tether-Transcript-Updated-At', String(daemonVersion))
        if (page.earlier !== undefined) headers.set('X-Tether-Transcript-Earlier', String(page.earlier))
        if (page.otherRecord !== undefined) headers.set('X-Tether-Transcript-Other-Record', page.otherRecord)
        return new Response(init?.method === 'HEAD' ? null : JSON.stringify(page.entries), { status: 200, headers })
      }
      return new Response('[]', { status: 200, headers: { 'Content-Type': 'application/json' } })
    })
  }

  const entry = (role: string, text: string, ts: number, ord: number = ts) => ({ role, text, ts, ord })
  const turns = (label: string, n: number, firstTs: number) =>
    Array.from({ length: n }, (_, i) => entry('user', `${label}-${i}`, firstTs + i))

  const settle = async () => {
    for (let i = 0; i < 5; i++) {
      await act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)) })
    }
  }

  /**
   * Step the wall clock past the interval floor.
   *
   * A `Date.now` spy and NOT `vi.useFakeTimers`, and not a real sleep either. The floor is
   * the only thing in this feature that reads a clock; everything else — `settle`, and the
   * scroll events `liveScrollBox` queues for the pane's own `scrollTop` writes — rides
   * real `setTimeout`, so faking timers would replace the machinery under test. A real
   * sleep works but costs this file about thirteen seconds of wall time, and paying it in
   * a parallel worker is not free for the rest of the suite: it changes when every other
   * file's promise chains interleave. Advancing a counter is exact, instant, and cannot
   * make another test flaky.
   *
   * Monotonic forward only, which is what makes it safe here: the other `Date.now`
   * readers this pane reaches (`sessionStart`, `fmtElapsed`) only ever compute an elapsed
   * time from it.
   */
  const pastFloor = () => { clock += TRANSCRIPT_EDGE_MIN_INTERVAL_MS + 20 }

  const countReq = (r: string) => requested.filter(x => x === r).length
  const countEarlierPages = () => requested.filter(x => x.startsWith('GET ') && x.includes('before=')).length
  const countNewestGets = () => countReq(`GET ${MESSAGES_URL}`)

  const dots = (root: ParentNode) => root.querySelectorAll('.transcript-top-slots .transcript-dots.on')
  const fallbackButton = (root: ParentNode) => root.querySelectorAll('.transcript-top-slots .transcript-more.on')

  let clock = 0
  let clockSpy: ReturnType<typeof vi.spyOn> | null = null

  beforeEach(() => {
    localStorage.clear()
    pages = {}
    requested = []
    gates = {}
    daemonVersion = 1000
    clock = 1_760_000_000_000
    clockSpy = vi.spyOn(Date, 'now').mockImplementation(() => clock)
    resetTranscriptWatchForTests()
    resetSessionActivityForTests()
    globalThis.fetch = stubDaemon() as unknown as typeof fetch
    useStore.setState({
      messages: [], notices: [], pendingPermissions: [], fatal: null,
      streaming: false, streamingMsgId: null, curTurnId: null,
      sessionId: SID, workspacesLoaded: false, activeWorkspace: null,
      transcriptEarlier: null, transcriptOtherRecord: null, transcriptPagesBack: 0,
    })
  })

  afterEach(() => {
    cleanup()
    clockSpy?.mockRestore()
    clockSpy = null
    resetTranscriptWatchForTests()
    resetSessionActivityForTests()
    globalThis.fetch = originalFetch
    useStore.setState({
      messages: [], notices: [], sessionId: null, fatal: null, streaming: false, workspacesLoaded: false,
      transcriptEarlier: null, transcriptOtherRecord: null, transcriptPagesBack: 0,
    })
    localStorage.clear()
  })

  /** The deferred-connect pane: no remembered sid, workspaces unsettled ⇒ no WebTransport
   *  attempt, no watcher, no probes. Everything requested is therefore attributable. */
  const renderIdle = async () => {
    const { container } = render(<ChatPane />)
    await settle()
    return container
  }

  /**
   * The held-session pane, which is the only state with a three-second poll to pre-empt.
   * Same construction as tether#106's cases: workspaces settled ⇒ the mount connects,
   * jsdom has no WebTransport ⇒ the connect rejects, and the refusal on the store makes
   * connState 'failed'.
   */
  const renderHeld = async () => {
    useStore.setState({ workspacesLoaded: true })
    localStorage.setItem('tether_last_sid', SID)
    const { container } = render(<ChatPane />)
    useStore.setState({ fatal: { code: ErrCodeSessionHeldByBackgroundAgent, message: 'a background agent is using this conversation' } })
    await settle()
    // Preconditions, asserted rather than assumed: without both, everything below is
    // exercising a pane in a different state than the assertions describe.
    expect(container.querySelectorAll('.failed-card')).toHaveLength(1)
    expect(requested.filter(r => r === `HEAD ${MESSAGES_URL}`).length).toBeGreaterThan(0)
    return container
  }

  // ── 1. the top: scrolling there loads ─────────────────────────────────────
  it('loads the earlier page when the reader scrolls to the top, with no click at all', async () => {
    pages[MESSAGES_URL] = { entries: turns('recent', 10, 500), earlier: 4096 }
    pages[`${MESSAGES_URL}?before=4096`] = { entries: turns('older', 5, 100), earlier: 2048 }
    const container = await renderIdle()
    const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)
    expect(box.height()).toBe(1000)
    expect(countEarlierPages()).toBe(0)

    // Reading somewhere in the middle. This ARMS the top end and asks for nothing —
    // pinned, because a trigger that fired on any scroll at all would pass every other
    // assertion in this test.
    await act(async () => { box.scrollTo(400) })
    await settle()
    expect(countEarlierPages()).toBe(0)

    await act(async () => { box.scrollTo(0) })
    await settle()

    expect(countEarlierPages()).toBe(1)
    expect(countReq(`GET ${MESSAGES_URL}?before=4096`)).toBe(1)
    expect([...container.querySelectorAll('.msg-user-bubble')].map(e => e.textContent).slice(0, 5))
      .toEqual(['older-0', 'older-1', 'older-2', 'older-3', 'older-4'])
    // The reader stayed on the message they were looking at: 5 bubbles × 100px went in
    // above scrollTop 0. Exact, because every wrong answer here is also greater than 0.
    expect(box.height()).toBe(1500)
    expect(box.top()).toBe(500)
    // …and the cursor moved back, so a second visit goes one page further rather than
    // re-serving this one.
    expect(useStore.getState().transcriptEarlier).toBe(2048)
  })

  it('does not chain-load: the correction that follows a prepend is a scroll, and it stops there', async () => {
    // The loop the wi names, in its first form. `scrollAfterPrepend` writes `scrollTop`;
    // a `scrollTop` write IS a scroll, so the browser re-enters this handler with no help
    // from the reader. The latch is what makes the re-entry land on "away from the end"
    // instead of on another request.
    pages[MESSAGES_URL] = { entries: turns('recent', 10, 500), earlier: 4096 }
    pages[`${MESSAGES_URL}?before=4096`] = { entries: turns('older', 5, 100), earlier: 2048 }
    pages[`${MESSAGES_URL}?before=2048`] = { entries: turns('oldest', 5, 10), earlier: 1024 }
    const container = await renderIdle()
    const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)
    const eventsBefore = box.events()

    await act(async () => { box.scrollTo(400) })
    await settle()
    await act(async () => { box.scrollTo(0) })
    await settle()

    // The FIXTURE's own precondition: more scroll events happened than the two this test
    // dispatched, i.e. the pane's correction really did re-enter the handler. Without
    // this, "exactly one page" would be true of a fixture that simply never re-entered,
    // and the latch would be untested.
    expect(box.events()).toBeGreaterThan(eventsBefore + 2)
    expect(countEarlierPages()).toBe(1)
    expect(useStore.getState().transcriptPagesBack).toBe(1)
    // Specifically: the SECOND page was never asked for.
    expect(countReq(`GET ${MESSAGES_URL}?before=2048`)).toBe(0)
  })

  it('asks once for a reader parked at the top, however many scroll events arrive', async () => {
    // The loop in its worst form: an earlier page that adds NO height (every record on it
    // is already on screen, so prependHistory drops the lot) while the daemon keeps
    // handing back a fresh cursor. The correction therefore leaves the reader at the top,
    // and every further scroll event — momentum, a rubber-band release, a finger resting
    // on the glass — is another arrival at an end that still has something to fetch.
    //
    // The events are spaced past the interval floor ON PURPOSE. Bunched together the floor
    // alone would hold them, and this assertion would be measuring the floor rather than
    // the latch it is written for.
    const sameFive = turns('recent', 5, 500)
    pages[MESSAGES_URL] = { entries: sameFive, earlier: 4096 }
    pages[`${MESSAGES_URL}?before=4096`] = { entries: sameFive, earlier: 2048 }
    pages[`${MESSAGES_URL}?before=2048`] = { entries: sameFive, earlier: 1024 }
    pages[`${MESSAGES_URL}?before=1024`] = { entries: sameFive, earlier: 512 }
    const container = await renderIdle()
    const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)

    await act(async () => { box.scrollTo(400) })
    await settle()
    await act(async () => { box.scrollTo(0) })
    await settle()
    expect(countEarlierPages()).toBe(1)

    for (let i = 0; i < 2; i++) {
      pastFloor()
      await act(async () => { box.fire() })
      await settle()
    }

    // The preconditions that make this the PARKED case: the reader never moved, and the
    // pane still believes there is something to fetch. Either one failing would make the
    // count below true for a reason that has nothing to do with the latch.
    expect(box.top()).toBe(0)
    expect(box.height()).toBe(500)
    expect(useStore.getState().transcriptEarlier).toBe(2048)
    expect(countEarlierPages()).toBe(1)

    // The CONTROL, so the assertion above is known to discriminate: leaving the end and
    // coming back is a new arrival, and it loads.
    pastFloor()
    await act(async () => { box.scrollTo(200) })
    await settle()
    await act(async () => { box.scrollTo(0) })
    await settle()
    expect(countEarlierPages()).toBe(2)
    expect(countReq(`GET ${MESSAGES_URL}?before=2048`)).toBe(1)
  })

  it('holds the floor when a reader oscillates across the threshold faster than it', async () => {
    // What the latch alone cannot stop, and therefore the only thing this test is about:
    // crossing out of the zone and back in re-arms honestly, so two full round trips
    // inside the floor would be two requests without it. On touch this is one shaky flick.
    const sameFive = turns('recent', 5, 500)
    pages[MESSAGES_URL] = { entries: sameFive, earlier: 4096 }
    pages[`${MESSAGES_URL}?before=4096`] = { entries: sameFive, earlier: 2048 }
    pages[`${MESSAGES_URL}?before=2048`] = { entries: sameFive, earlier: 1024 }
    const container = await renderIdle()
    const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)

    // TWO round trips, and the first one is allowed to SETTLE before the second starts.
    // That separation is the whole test. Written without it — all four positions inside one
    // act — this passed with the floor deleted, because the first request was still in
    // flight and the shared in-flight ref refused the second: the assertion was measuring
    // that guard, not this one. The mutation battery is what said so.
    //
    // `settle()` costs no simulated time (the clock is a frozen spy, stepped only by
    // pastFloor), so the second round trip arms honestly, finds nothing in flight, and has
    // `sinceLastMs === 0`. The floor is then the only thing left that can refuse it.
    await act(async () => {
      box.scrollTo(200)
      box.scrollTo(0)
    })
    await settle()
    expect(countEarlierPages()).toBe(1)
    expect(useStore.getState().transcriptEarlier).toBe(2048)

    await act(async () => {
      box.scrollTo(200)
      box.scrollTo(0)
    })
    await settle()

    expect(countEarlierPages()).toBe(1)
    expect(countReq(`GET ${MESSAGES_URL}?before=2048`)).toBe(0)

    // The CONTROL: the same round trip, one floor later, loads. Without it "1" would also
    // be what a pane that had stopped triggering entirely reports.
    pastFloor()
    await act(async () => {
      box.scrollTo(200)
      box.scrollTo(0)
    })
    await settle()
    expect(countEarlierPages()).toBe(2)
    expect(countReq(`GET ${MESSAGES_URL}?before=2048`)).toBe(1)
  })

  // ── 2. the dots: exactly while a request exists, and never at a ceiling ────
  it('shows the dots only while an earlier page is genuinely in flight', async () => {
    pages[MESSAGES_URL] = { entries: turns('recent', 10, 500), earlier: 4096 }
    pages[`${MESSAGES_URL}?before=4096`] = { entries: turns('older', 5, 100), earlier: 2048 }
    const container = await renderIdle()
    const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)

    // Idle: the fallback is showing, the dots are not.
    expect(dots(container)).toHaveLength(0)
    expect(fallbackButton(container)).toHaveLength(1)

    let release = () => {}
    gates[`GET ${MESSAGES_URL}?before=4096`] = new Promise<void>(r => { release = r })
    await act(async () => { box.scrollTo(400) })
    await settle()
    await act(async () => { box.scrollTo(0) })
    await settle()

    // In flight: the dots are on, the button is off — and it is still in the DOM, which
    // is the constant-height construction rather than an accident.
    expect(countEarlierPages()).toBe(1)
    expect(dots(container)).toHaveLength(1)
    expect(dots(container)[0].getAttribute('aria-label')).toBe(TRANSCRIPT_DOTS_EARLIER_LABEL)
    expect(dots(container)[0].querySelectorAll('.transcript-dot')).toHaveLength(3)
    expect(fallbackButton(container)).toHaveLength(0)
    expect(container.querySelectorAll('.transcript-top-slots .transcript-more')).toHaveLength(1)

    await act(async () => { release(); await Promise.resolve() })
    await settle()

    // Settled: back to the fallback, no dots.
    expect(dots(container)).toHaveLength(0)
    expect(fallbackButton(container)).toHaveLength(1)
  })

  it('puts NO dots at the ceiling, where waiting can never produce anything', async () => {
    // The judgement this lane keeps re-making: a spinner where nothing more can arrive is
    // a lie. At the true top the pane says which kind of top it is — tether#107's three
    // sentences — and scrolling into it must not turn that into a wait.
    pages[MESSAGES_URL] = { entries: turns('recent', 10, 500), earlier: 4096 }
    pages[`${MESSAGES_URL}?before=4096`] = { entries: turns('older', 5, 100) } // no cursor: the beginning
    const container = await renderIdle()
    const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)

    await act(async () => { box.scrollTo(400) })
    await settle()
    await act(async () => { box.scrollTo(0) })
    await settle()
    expect(countEarlierPages()).toBe(1)

    // At the ceiling now. Scroll into it as hard as the reader likes.
    pastFloor()
    for (let i = 0; i < 3; i++) {
      await act(async () => { box.scrollTo(0) })
      await settle()
    }

    expect(container.querySelectorAll('.transcript-dots')).toHaveLength(0)
    expect(container.querySelectorAll('.transcript-top-slots')).toHaveLength(0)
    expect(container.querySelector('.transcript-top .transcript-top-note')?.textContent).toBe(TRANSCRIPT_START_COMPLETE)
    // …and nothing was requested for it. A request that could not advance anything is the
    // same lie one layer down.
    expect(countEarlierPages()).toBe(1)
  })

  it('keeps the top marker to ONE cell with exactly one visible child, in both states', async () => {
    // The constant-height property, expressed as the only thing a test in this repo can
    // reach. jsdom computes no layout, so `visibility: hidden` and the grid cell itself
    // are invisible from here; what IS checkable is the STRUCTURE that produces them —
    // both children always present, exactly one carrying `.on`. tether#108 paid for the
    // alternative (a `min-height` in px, green everywhere, wrong at every other width).
    pages[MESSAGES_URL] = { entries: turns('recent', 10, 500), earlier: 4096 }
    pages[`${MESSAGES_URL}?before=4096`] = { entries: turns('older', 5, 100), earlier: 2048 }
    const container = await renderIdle()
    const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)

    const slots = () => container.querySelector('.transcript-top-slots') as HTMLElement
    expect(slots().children).toHaveLength(2)
    expect(slots().querySelectorAll(':scope > .on')).toHaveLength(1)

    let release = () => {}
    gates[`GET ${MESSAGES_URL}?before=4096`] = new Promise<void>(r => { release = r })
    await act(async () => { box.scrollTo(400) })
    await settle()
    await act(async () => { box.scrollTo(0) })
    await settle()

    expect(slots().children).toHaveLength(2)
    expect(slots().querySelectorAll(':scope > .on')).toHaveLength(1)
    // …and it is the OTHER one this time. A cell whose `.on` never moved would satisfy
    // the count above forever.
    expect(slots().querySelector(':scope > .on')?.classList.contains('transcript-dots')).toBe(true)

    await act(async () => { release(); await Promise.resolve() })
    await settle()
    expect(slots().querySelector(':scope > .on')?.classList.contains('transcript-more')).toBe(true)
  })

  // ── 3. the bottom ─────────────────────────────────────────────────────────
  it('re-reads the newest page on arriving at the bottom, without waiting for the poll', async () => {
    pages[MESSAGES_URL] = { entries: turns('recent', 10, 500) }
    const container = await renderHeld()
    const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)
    expect(box.height()).toBe(1000)

    // The other agent writes a turn — and the daemon's VERSION does not move, so the
    // three-second probe has no reason to reload. This is what makes the assertion below
    // about this wi's trigger rather than about tether#106's poll: give the poll its
    // chance first (a visibility return probes immediately) and watch it decline.
    pages[MESSAGES_URL] = { entries: [...turns('recent', 10, 500), entry('assistant', 'brand new', 9000)] }
    for (const state of ['hidden', 'visible'] as const) {
      Object.defineProperty(document, 'visibilityState', { value: state, configurable: true })
      await act(async () => { document.dispatchEvent(new Event('visibilitychange')) })
    }
    await settle()
    expect(screen.queryByText('brand new')).toBeNull()

    const getsBefore = countNewestGets()
    await act(async () => { box.scrollTo(300) })   // away from the bottom: arms
    await settle()
    expect(countNewestGets()).toBe(getsBefore)
    await act(async () => { box.scrollTo(700) })   // 1000 - 700 - 300 = 0: arrived
    await settle()

    expect(countNewestGets()).toBe(getsBefore + 1)
    expect(screen.getByText('brand new')).toBeTruthy()
  })

  it('asks once for a reader parked at the bottom, autoscroll included', async () => {
    // The second loop, and it is a different mechanism from the first: `nearBottom` is
    // ALSO the stick-to-bottom condition, so an append writes `scrollTop = scrollHeight`,
    // which is a scroll, which arrives back here at distance ≤ 0. Bounded by the same
    // latch: the reader never went further than the threshold, so nothing re-armed.
    pages[MESSAGES_URL] = { entries: turns('recent', 10, 500) }
    const container = await renderHeld()
    const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)

    pages[MESSAGES_URL] = { entries: [...turns('recent', 10, 500), entry('assistant', 'brand new', 9000)] }
    const getsBefore = countNewestGets()
    await act(async () => { box.scrollTo(300) })
    await settle()
    await act(async () => { box.scrollTo(700) })
    await settle()
    expect(countNewestGets()).toBe(getsBefore + 1)

    // The append landed and the pane followed it to the bottom — the precondition that
    // makes this the loop case rather than a reader who happens to be standing still.
    expect(box.height()).toBe(1100)
    expect(box.top()).toBe(1100)

    for (let i = 0; i < 2; i++) {
      pastFloor()
      await act(async () => { box.fire() })
      await settle()
    }
    expect(countNewestGets()).toBe(getsBefore + 1)

    // The CONTROL: leaving the bottom and returning is a new arrival, and it asks again.
    pastFloor()
    await act(async () => { box.scrollTo(300) })
    await settle()
    await act(async () => { box.scrollTo(800) })
    await settle()
    expect(countNewestGets()).toBe(getsBefore + 2)
  })

  it('never re-reads on scroll in a session that has a live stream', async () => {
    // The gate, and it is not caution. `refreshTranscript` has no `!streaming` guard on
    // purpose — its doc says every caller is a state in which a turn cannot be in flight —
    // and scrolling to the bottom of a connected session is the most ordinary thing there
    // is to do while a turn streams. Wiring this end everywhere would hand back tether#42.
    // There is also nothing to pre-empt: the three-second poll only exists in the held
    // state.
    pages[MESSAGES_URL] = { entries: turns('recent', 10, 500) }
    const container = await renderIdle()
    const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)
    const getsBefore = countNewestGets()

    for (const top of [300, 700, 300, 700]) {
      pastFloor()
      await act(async () => { box.scrollTo(top) })
      await settle()
    }

    expect(countNewestGets()).toBe(getsBefore)
    expect(container.querySelectorAll('.transcript-bottom')).toHaveLength(0)
  })

  it('re-reads through the merge, so the pages the reader loaded survive it', async () => {
    // What "reuse the existing refresh path" has to MEAN, rather than what it looks like.
    // `refreshTranscript` is the only caller that consults `transcriptPagesBack` and
    // therefore the only one that merges instead of replacing; a second fetch beside it
    // would look identical on the wire and throw away every earlier page — and would skip
    // tether#109's `ord` check on the way.
    pages[MESSAGES_URL] = { entries: turns('recent', 5, 500), earlier: 4096 }
    pages[`${MESSAGES_URL}?before=4096`] = { entries: turns('older', 3, 100), earlier: 2048 }
    const container = await renderHeld()
    const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)

    await act(async () => { box.scrollTo(300) })
    await settle()
    await act(async () => { box.scrollTo(0) })
    await settle()
    expect(useStore.getState().transcriptPagesBack).toBe(1)
    expect([...container.querySelectorAll('.msg-user-bubble')].map(e => e.textContent))
      .toEqual(['older-0', 'older-1', 'older-2', 'recent-0', 'recent-1', 'recent-2', 'recent-3', 'recent-4'])

    pages[MESSAGES_URL] = { entries: [...turns('recent', 5, 500), entry('assistant', 'brand new', 9000)], earlier: 5120 }
    pastFloor()
    const el = container.querySelector('.dt-chat') as HTMLElement
    await act(async () => { box.scrollTo(el.scrollHeight - 300) })
    await settle()

    expect(screen.getByText('brand new')).toBeTruthy()
    // The reader's page is still there, above the newest one…
    expect([...container.querySelectorAll('.msg-user-bubble')].map(e => e.textContent))
      .toEqual(['older-0', 'older-1', 'older-2', 'recent-0', 'recent-1', 'recent-2', 'recent-3', 'recent-4'])
    expect(useStore.getState().transcriptPagesBack).toBe(1)
    // …and the cursor still describes the OLDEST page on screen rather than the refresh's
    // own 5120, which would send the next load forward over pages already rendered.
    expect(useStore.getState().transcriptEarlier).toBe(2048)
  })

  it('shows the bottom dots only while the re-read is in flight', async () => {
    pages[MESSAGES_URL] = { entries: turns('recent', 10, 500) }
    const container = await renderHeld()
    const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)
    expect(container.querySelectorAll('.transcript-bottom')).toHaveLength(0)

    let release = () => {}
    gates[`GET ${MESSAGES_URL}`] = new Promise<void>(r => { release = r })
    await act(async () => { box.scrollTo(300) })
    await settle()
    await act(async () => { box.scrollTo(700) })
    await settle()

    const bottom = container.querySelectorAll('.transcript-bottom .transcript-dots')
    expect(bottom).toHaveLength(1)
    expect(bottom[0].getAttribute('aria-label')).toBe(TRANSCRIPT_DOTS_NEWER_LABEL)
    expect(bottom[0].querySelectorAll('.transcript-dot')).toHaveLength(3)
    // It is the LAST thing in the scroll container — where the message will appear, which
    // is the whole argument for its position.
    const rows = container.querySelectorAll('.dt-chat > *')
    expect(rows[rows.length - 1].classList.contains('transcript-bottom')).toBe(true)

    await act(async () => { release(); await Promise.resolve() })
    await settle()
    expect(container.querySelectorAll('.transcript-bottom')).toHaveLength(0)
  })

  it('holds one request at a time across BOTH ends', async () => {
    // One shared in-flight flag rather than one per end, and the reason is the anchor:
    // the bottom indicator changes the scroll height, and `scrollAfterPrepend` compares a
    // height captured before the top's request with one measured after it. If the two ends
    // could overlap, that comparison would silently carry the other end's indicator.
    pages[MESSAGES_URL] = { entries: turns('recent', 10, 500), earlier: 4096 }
    pages[`${MESSAGES_URL}?before=4096`] = { entries: turns('older', 5, 100), earlier: 2048 }
    const container = await renderHeld()
    const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)

    let release = () => {}
    gates[`GET ${MESSAGES_URL}?before=4096`] = new Promise<void>(r => { release = r })
    await act(async () => { box.scrollTo(400) })
    await settle()
    await act(async () => { box.scrollTo(0) })
    await settle()
    expect(countEarlierPages()).toBe(1)
    const getsBefore = countNewestGets()

    // Standing at the top of a 1000px box with a 300px viewport, the bottom end is 700px
    // away — so it armed on the way — and arriving there is a genuine arrival that only
    // the shared flag refuses.
    pastFloor()
    await act(async () => { box.scrollTo(700) })
    await settle()
    expect(countNewestGets()).toBe(getsBefore)
    expect(container.querySelectorAll('.transcript-bottom')).toHaveLength(0)

    await act(async () => { release(); await Promise.resolve() })
    await settle()

    // …and once it settles, the other end works again: leaving and returning asks.
    pastFloor()
    await act(async () => { box.scrollTo(400) })
    await settle()
    const el = container.querySelector('.dt-chat') as HTMLElement
    await act(async () => { box.scrollTo(el.scrollHeight - 300) })
    await settle()
    expect(countNewestGets()).toBe(getsBefore + 1)
  })

  // ── 4. what must not change ───────────────────────────────────────────────
  it('does not mark an automatically prepended page as having just arrived', async () => {
    // tether#108's trace means "one just landed", and a page the reader scrolled to is not
    // one. `trailingArrivals` excludes it by SHAPE — older records go in at the front, and
    // the walk from the end stops at the first id already on screen — so this pins that
    // the new trigger did not find a way around that shape.
    pages[MESSAGES_URL] = { entries: turns('recent', 10, 500), earlier: 4096 }
    pages[`${MESSAGES_URL}?before=4096`] = { entries: turns('older', 5, 100), earlier: 2048 }
    const container = await renderHeld()
    const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)
    expect(container.querySelectorAll('.msg-arrived')).toHaveLength(0)

    await act(async () => { box.scrollTo(400) })
    await settle()
    await act(async () => { box.scrollTo(0) })
    await settle()

    // The page really landed…
    expect(useStore.getState().transcriptPagesBack).toBe(1)
    expect(screen.getByText('older-0')).toBeTruthy()
    // …and none of it is wearing a trace.
    expect(container.querySelectorAll('.msg-arrived')).toHaveLength(0)

    // The CONTROL, over the OTHER new trigger, so the assertion above is known to
    // discriminate: a turn that genuinely arrived — pulled in by the bottom re-read — is
    // traced, and only that one.
    pages[MESSAGES_URL] = { entries: [...turns('recent', 10, 500), entry('assistant', 'brand new', 9000)], earlier: 4096 }
    pastFloor()
    const el = container.querySelector('.dt-chat') as HTMLElement
    await act(async () => { box.scrollTo(el.scrollHeight - 300) })
    await settle()
    expect([...container.querySelectorAll('.msg-arrived')].map(e => e.textContent?.includes('brand new')))
      .toEqual([true])
  })

  it('keeps the ids of everything on screen when a page arrives by scrolling', async () => {
    // `key={m.id}` is React's reconciliation key and both expansion Sets are keyed by id,
    // so re-minting one collapses the reader's expansions and clamps the scroll — on the
    // path whose entire purpose is to leave them where they were.
    pages[MESSAGES_URL] = { entries: turns('recent', 10, 500), earlier: 4096 }
    pages[`${MESSAGES_URL}?before=4096`] = { entries: turns('older', 5, 100), earlier: 2048 }
    const container = await renderIdle()
    const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)
    const idsBefore = useStore.getState().messages.map(m => m.id)
    expect(idsBefore).toHaveLength(10)

    await act(async () => { box.scrollTo(400) })
    await settle()
    await act(async () => { box.scrollTo(0) })
    await settle()

    const after = useStore.getState().messages
    expect(after.map(m => m.id).slice(5)).toEqual(idsBefore)
    expect(new Set(after.map(m => m.id)).size).toBe(after.length)
  })

  it('retires both latches on a session switch, so the switch cannot re-read its own fetch', async () => {
    // What the reset is FOR, which is not what it first looks like. A latch describes where
    // the reader has been in ONE conversation. Carried across a switch, an ARMED bottom
    // turns the reader's next arrival at the bottom of the new session into a re-read of
    // the page the switch itself has just fetched — up to a megabyte on the wire and an
    // unbounded read on the daemon, for a transcript that is already on screen.
    //
    // The construction: arm in session A without firing, switch, then arrive at the bottom
    // of session B in ONE scroll event (a single event delivers one position, so it cannot
    // arm and fire in the same breath). With the reset the arrival finds a cold latch; the
    // battery's M10 mutant — deleting the reset — makes it find A's.
    const SID2 = 'sid-edges-0002'
    const MESSAGES_URL2 = `/api/v1/sessions/${SID2}/messages`
    pages[MESSAGES_URL] = { entries: turns('recent', 10, 500) }
    pages[MESSAGES_URL2] = { entries: turns('second', 10, 500) }
    const container = await renderHeld()
    const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)

    // Arm, and ask for nothing — the precondition that makes this about the carry-over.
    const getsA = countNewestGets()
    await act(async () => { box.scrollTo(300) })
    await settle()
    expect(countNewestGets()).toBe(getsA)

    await act(async () => { useStore.setState({ sessionId: SID2 }) })
    await settle()
    expect(screen.getByText('second-0')).toBeTruthy()
    const getsB = countReq(`GET ${MESSAGES_URL2}`)
    expect(getsB).toBeGreaterThan(0)

    // One event, landing at the bottom, with the floor long since elapsed so that it is
    // not the thing refusing.
    pastFloor()
    const el = container.querySelector('.dt-chat') as HTMLElement
    await act(async () => { box.scrollTo(el.scrollHeight - 300) })
    await settle()

    expect(countReq(`GET ${MESSAGES_URL2}`)).toBe(getsB)
    expect(container.querySelectorAll('.transcript-bottom')).toHaveLength(0)

    // The CONTROL: once the reader has moved in THIS session, arriving at the bottom does
    // ask. Without it, "no extra request" would also be what a pane with the bottom
    // trigger switched off entirely reports.
    pastFloor()
    await act(async () => { box.scrollTo(300) })
    await settle()
    pastFloor()
    await act(async () => { box.scrollTo(el.scrollHeight - 300) })
    await settle()
    expect(countReq(`GET ${MESSAGES_URL2}`)).toBe(getsB + 1)
  })

  it('still lets the fallback button load a page, for a transcript that cannot scroll', async () => {
    // Why the button did not simply go away. A newest page that does not overfill the
    // viewport produces NO scroll events at all — there is nothing to scroll — so the
    // automatic trigger can never fire and every earlier page would be unreachable. It is
    // also the only keyboard-reachable path: `.dt-chat` is a plain div and takes no focus.
    pages[MESSAGES_URL] = { entries: turns('recent', 2, 500), earlier: 4096 }
    pages[`${MESSAGES_URL}?before=4096`] = { entries: turns('older', 2, 100), earlier: 2048 }
    const container = await renderIdle()
    // The precondition: this container genuinely cannot scroll (200px of content in a
    // 300px viewport), so no amount of scrolling could have loaded anything.
    const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)
    expect(box.height()).toBe(200)
    await act(async () => { box.fire() })
    await settle()
    expect(countEarlierPages()).toBe(0)

    await act(async () => { (container.querySelector('.transcript-more') as HTMLButtonElement).click() })
    await settle()

    expect(countEarlierPages()).toBe(1)
    expect([...container.querySelectorAll('.msg-user-bubble')].map(e => e.textContent))
      .toEqual(['older-0', 'older-1', 'recent-0', 'recent-1'])
  })

  // ── 5. tether#112 — pulling further at an end that cannot move ─────────────
  //
  // What the owner reported minutes after tether#110 shipped: "拉到底之后继续往下拉就拉不动
  // 了，必须先往上拉一点、再往下拉，才会显示三个点加载" — at the bottom, pulling further does
  // nothing; you have to scroll up a little and back down.
  //
  // Two stacked causes, and the FIRST is the one that makes every guard irrelevant: at an end
  // `scrollTop` is clamped, so pulling further changes no position and the browser fires no
  // `scroll` event whatsoever. The handler is not called, so its latch, its floor and its
  // in-flight ref are never even consulted. (The second cause is the latch, which would have
  // refused anyway — the bottom is where the pane parks the reader, so it is never armed
  // there.) Every design that keys off `scroll` alone is dead in that state regardless of how
  // its guards are tuned, which is why this is a new listener rather than a tuning.
  //
  // These cases therefore assert the MECHANISM as well as the outcome: that the gesture
  // produced no scroll event (`box.events()` flat) and moved nothing (`box.top()` flat), while
  // the request happened anyway.
  describe('pulling further at an end (tether#112)', () => {
    it('re-reads the newest page for a reader PARKED at the bottom who pulls further down', async () => {
      pages[MESSAGES_URL] = { entries: turns('recent', 10, 500) }
      const container = await renderHeld()
      const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)

      // Arrive at the bottom the way a reader does, and let that arrival have its one load.
      // This is what leaves the pane in the reported state: the latch spent, and the reader
      // standing where the autoscroll parks them.
      await act(async () => { box.scrollTo(300) })
      await settle()
      await act(async () => { box.scrollTo(700) })
      await settle()
      const getsBefore = countNewestGets()
      // Exact, not `> 0`: the mount's own load plus the arrival's re-read, deterministically.
      expect(getsBefore).toBe(2)
      // …and standing exactly AT the end, which is the position the whole wi is about:
      // 1000 - 700 - 300 = 0. Measured, not assumed — that arrival's re-read handed back a
      // page identical to the one on screen, so nothing was appended and the `nearBottom`
      // autoscroll had no commit to follow. (When it does append, it moves the reader to
      // scrollHeight and fires one scroll event; the touch case below pins that.)
      expect(box.top()).toBe(700)
      const el0 = container.querySelector('.dt-chat') as HTMLElement
      expect(el0.scrollHeight - box.top() - 300).toBe(0)

      pastFloor()
      pages[MESSAGES_URL] = { entries: [...turns('recent', 10, 500), entry('assistant', 'brand new', 9000)] }

      // The PRECONDITION, and tether#110's property that must not weaken: a further scroll
      // event at this position asks for nothing. The latch is doing that, and it stays.
      await act(async () => { box.fire() })
      await settle()
      expect(countNewestGets()).toBe(getsBefore)
      expect(screen.queryByText('brand new')).toBeNull()

      let release = () => {}
      gates[`GET ${MESSAGES_URL}`] = new Promise<void>(r => { release = r })
      const wheelsBefore = box.wheels()
      const eventsBefore = box.events()
      await act(async () => { box.wheel(120) })

      // The fixture really delivered the gesture — without this the assertion below could be
      // satisfied by a dispatch that reached nothing…
      expect(box.wheels()).toBe(wheelsBefore + 1)
      // …the position did not move and NO scroll event was produced, which is the defect's
      // mechanism stated as an assertion rather than as prose…
      expect(box.top()).toBe(700)
      expect(box.events()).toBe(eventsBefore)
      // …and the request went out anyway, with the dots to say so.
      expect(countNewestGets()).toBe(getsBefore + 1)
      expect(container.querySelectorAll('.transcript-bottom .transcript-dots')).toHaveLength(1)

      await act(async () => { release(); await Promise.resolve() })
      await settle()
      expect(screen.getByText('brand new')).toBeTruthy()
      expect(countNewestGets()).toBe(getsBefore + 1)
      expect(container.querySelectorAll('.transcript-bottom')).toHaveLength(0)
    })

    it('does the same for a touch pull, where there is no wheel to listen to', async () => {
      // The half of the fix that reaches the reader who reported it. A phone has no wheel
      // event at all, so a wheel-only fix would be green here and dead in the owner's hand.
      pages[MESSAGES_URL] = { entries: turns('recent', 10, 500) }
      const container = await renderHeld()
      const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)

      await act(async () => { box.scrollTo(300) })
      await settle()
      await act(async () => { box.scrollTo(700) })
      await settle()
      const getsBefore = countNewestGets()

      pastFloor()
      pages[MESSAGES_URL] = { entries: [...turns('recent', 10, 500), entry('assistant', 'brand new', 9000)] }

      const movesBefore = box.touchMoves()
      const eventsBefore = box.events()
      await act(async () => { box.touchPull(60) })
      await settle()

      expect(box.touchMoves()).toBe(movesBefore + 1)
      expect(box.events()).toBe(eventsBefore + 1) // the autoscroll after the append, nothing else
      expect(countNewestGets()).toBe(getsBefore + 1)
      expect(screen.getByText('brand new')).toBeTruthy()
    })

    it('ignores a resting finger, and acts on a pull that clears the threshold', async () => {
      // `touchmove` fires for a finger sitting on the glass, and a resting finger jitters. The
      // pair is what makes this discriminate: without the second half, "no request" would also
      // be what a pane with no touch listener at all reports.
      pages[MESSAGES_URL] = { entries: turns('recent', 10, 500) }
      const container = await renderHeld()
      const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)

      await act(async () => { box.scrollTo(300) })
      await settle()
      await act(async () => { box.scrollTo(700) })
      await settle()
      const getsBefore = countNewestGets()

      pastFloor()
      await act(async () => { box.touchPull(TRANSCRIPT_OVERSCROLL_TOUCH_PX) })
      await settle()
      expect(box.touchMoves()).toBe(1)
      expect(countNewestGets()).toBe(getsBefore)

      await act(async () => { box.touchPull(TRANSCRIPT_OVERSCROLL_TOUCH_PX + 1) })
      await settle()
      expect(countNewestGets()).toBe(getsBefore + 1)
    })

    it('asks ONCE per touch gesture, however long the finger stays on the glass', async () => {
      // The defect deep review found in the first cut of this fix, and it is the same shape as
      // the loop tether#110's latch exists to stop — reintroduced on the path that drops the
      // latch. The touch floor measures travel since `touchstart` and the anchor never moves,
      // so once a gesture has travelled 8px EVERY later move in it clears the floor, jitter
      // included. With only the 500ms interval floor left, a thumb resting at the bottom after
      // completing the very gesture this wi adds asked for the whole newest page twice a
      // second, indefinitely — a megabyte on the wire and an unbounded read on the daemon each
      // time.
      //
      // Expressible only because the fixture now has gesture BOUNDARIES: the old `touchPull`
      // dispatched a fresh `touchstart` before every move, so every case was a new gesture and
      // this state could not be reached.
      pages[MESSAGES_URL] = { entries: turns('recent', 10, 500) }
      const container = await renderHeld()
      const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)

      await act(async () => { box.scrollTo(300) })
      await settle()
      await act(async () => { box.scrollTo(700) })
      await settle()
      const gets = countNewestGets()

      pastFloor()
      // One gesture: down at 700, a real pull up to 100 — that asks, correctly…
      await act(async () => { box.touchStart(700) })
      await act(async () => { box.touchMoveTo(100) })
      await settle()
      expect(countNewestGets()).toBe(gets + 1)

      // …and then the finger just sits there, jittering, for three floors' worth of time.
      for (const y of [100.3, 99.8, 100.1]) {
        pastFloor()
        await act(async () => { box.touchMoveTo(y) })
        await settle()
      }
      expect(box.touchMoves()).toBe(4)
      expect(countNewestGets()).toBe(gets + 1)

      // The CONTROL: lifting the finger and pulling again is a NEW gesture, and it asks.
      await act(async () => { box.touchEnd() })
      pastFloor()
      await act(async () => { box.touchPull(60) })
      await settle()
      expect(countNewestGets()).toBe(gets + 2)
    })

    it('accumulates a SLOW pull, whose every single move is below the floor', async () => {
      // The property the anchor exists for, and the other half of the tension the case above
      // creates: measuring from the previous move instead of from the gesture's start would
      // also stop that burst, and would silently throw this away. A deliberate slow drag
      // arrives as many small moves, none of which clears 8px on its own.
      pages[MESSAGES_URL] = { entries: turns('recent', 10, 500) }
      const container = await renderHeld()
      const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)

      await act(async () => { box.scrollTo(300) })
      await settle()
      await act(async () => { box.scrollTo(700) })
      await settle()
      const gets = countNewestGets()

      pastFloor()
      await act(async () => { box.touchStart(700) })
      // Six moves of 3px each: no single move clears the 8px floor, the cumulative pull does.
      for (const y of [697, 694, 691, 688, 685, 682]) {
        await act(async () => { box.touchMoveTo(y) })
      }
      await settle()
      expect(box.touchMoves()).toBe(6)
      expect(countNewestGets()).toBe(gets + 1)
    })

    it('follows the finger that is MOVING, not whichever one is listed first', async () => {
      // A thumb holding the phone is a second touch that never moves, and the browser lists
      // the older touch first — so `touches[0]` is the thumb and reading it makes this whole
      // fix inert for a two-handed reader. `changedTouches` matched on `identifier` is the
      // finger the event is actually about. Measured in jsdom: with a resting point at 500 and
      // a moving one at 120, `touches[0].clientY` is 500 while `changedTouches[0].clientY` is
      // 120.
      //
      // The GEOMETRY is what makes this discriminate, and the first version of this case got
      // it wrong: with the thumb ABOVE the gesture's start, `start − thumb` is positive and a
      // handler reading the thumb loads anyway, for a reason unrelated to the pull. The thumb
      // is therefore BELOW the start (clientY 500 vs 300), so the wrong reading is −200 while
      // the real pull is +200. A mutant reading `touches[0]` was still alive until this.
      pages[MESSAGES_URL] = { entries: turns('recent', 10, 500) }
      const container = await renderHeld()
      const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)

      await act(async () => { box.scrollTo(300) })
      await settle()
      await act(async () => { box.scrollTo(700) })
      await settle()
      const gets = countNewestGets()

      pastFloor()
      await act(async () => { box.touchStart(300, 500) })
      await act(async () => { box.touchMoveTo(100, 500) })
      await settle()
      expect(box.touchMoves()).toBe(1)
      expect(countNewestGets()).toBe(gets + 1)
    })

    it('drops a gesture the browser took away, and keeps one it did not', async () => {
      // `touchcancel` is how the browser says "this touch is mine now" — Chrome's
      // pull-to-refresh and back-swipe both do it — and after that, whatever else arrives in
      // the sequence is not the reader pulling on this pane. Paired with the control so the
      // assertion is about the cancel rather than about touch being broken.
      pages[MESSAGES_URL] = { entries: turns('recent', 10, 500) }
      const container = await renderHeld()
      const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)

      await act(async () => { box.scrollTo(300) })
      await settle()
      await act(async () => { box.scrollTo(700) })
      await settle()
      const gets = countNewestGets()

      pastFloor()
      await act(async () => { box.touchStart(700) })
      await act(async () => { box.touchCancel() })
      await act(async () => { box.touchMoveTo(100) })
      await settle()
      expect(box.touchMoves()).toBe(1)
      expect(countNewestGets()).toBe(gets)

      // The CONTROL: the identical pull without the cancel asks.
      await act(async () => { box.touchStart(700) })
      await act(async () => { box.touchMoveTo(100) })
      await settle()
      expect(countNewestGets()).toBe(gets + 1)
    })

    it('spends the TOP latch on a gesture load too, not only the bottom one', async () => {
      // The symmetric half of the latch case below. Review found both `…ArmedRef = false`
      // lines surviving as mutants; a bottom-only test leaves the top one alive.
      const sameFive = turns('recent', 5, 500)
      pages[MESSAGES_URL] = { entries: sameFive, earlier: 4096 }
      pages[`${MESSAGES_URL}?before=4096`] = { entries: sameFive, earlier: 2048 }
      pages[`${MESSAGES_URL}?before=2048`] = { entries: sameFive, earlier: 1024 }
      pages[`${MESSAGES_URL}?before=1024`] = { entries: sameFive, earlier: 512 }
      const container = await renderIdle()
      const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)

      await act(async () => { box.scrollTo(200) })
      await settle()
      await act(async () => { box.scrollTo(0) })
      await settle()
      expect(countEarlierPages()).toBe(1)

      // Re-arm and return INSIDE the floor, so the arrival is refused and the latch stays set.
      await act(async () => { box.scrollTo(200) })
      await settle()
      await act(async () => { box.scrollTo(0) })
      await settle()
      expect(countEarlierPages()).toBe(1)

      pastFloor()
      await act(async () => { box.wheel(-120) })
      await settle()
      expect(countEarlierPages()).toBe(2)

      // The latch the gesture consumed.
      pastFloor()
      await act(async () => { box.fire() })
      await settle()
      expect(countEarlierPages()).toBe(2)
      expect(countReq(`GET ${MESSAGES_URL}?before=1024`)).toBe(0)
    })

    it('spends the latch on a gesture load, so a later scroll event cannot ask again', async () => {
      // The two `…ArmedRef.current = false` lines, which review found to be surviving mutants:
      // the comment claims they matter and nothing tested it. Reachable because a gesture can
      // fire while the latch is SET — arrive at the bottom, be refused by the floor (so the
      // latch stays armed), then pull. If the gesture did not spend it, the next bare scroll
      // event at the same clamped position would load a second time.
      pages[MESSAGES_URL] = { entries: turns('recent', 10, 500) }
      const container = await renderHeld()
      const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)

      await act(async () => { box.scrollTo(300) })
      await settle()
      await act(async () => { box.scrollTo(700) })
      await settle()
      const gets = countNewestGets()

      // Re-arm and come back INSIDE the floor: the arrival is refused, so the latch stays set.
      await act(async () => { box.scrollTo(300) })
      await settle()
      await act(async () => { box.scrollTo(700) })
      await settle()
      expect(countNewestGets()).toBe(gets)

      pastFloor()
      await act(async () => { box.wheel(120) })
      await settle()
      expect(countNewestGets()).toBe(gets + 1)

      // The latch the gesture consumed: a plain scroll event at the same position, a floor
      // later, must find nothing left to spend.
      pastFloor()
      await act(async () => { box.fire() })
      await settle()
      expect(countNewestGets()).toBe(gets + 1)
    })

    it('loads the earlier page for a reader parked at the TOP who pulls further up', async () => {
      // The same defect at the other end, and the wi named it as such: the top only escaped
      // notice because you normally ARRIVE there by scrolling, which does move the box. Once
      // parked, pulling further up was equally dead.
      //
      // The construction that leaves a reader parked at the top: an earlier page every record
      // of which is already on screen, so `prependHistory` drops the lot, the anchor
      // correction is zero, and `scrollTop` stays at 0 while the daemon still offers a cursor.
      const sameFive = turns('recent', 5, 500)
      pages[MESSAGES_URL] = { entries: sameFive, earlier: 4096 }
      pages[`${MESSAGES_URL}?before=4096`] = { entries: sameFive, earlier: 2048 }
      pages[`${MESSAGES_URL}?before=2048`] = { entries: turns('older', 3, 100), earlier: 1024 }
      const container = await renderIdle()
      const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)

      await act(async () => { box.scrollTo(200) })
      await settle()
      await act(async () => { box.scrollTo(0) })
      await settle()
      expect(countEarlierPages()).toBe(1)
      // Parked at the top, latch spent, and the pane still believes there is more.
      expect(box.top()).toBe(0)
      expect(box.height()).toBe(500)
      expect(useStore.getState().transcriptEarlier).toBe(2048)

      pastFloor()
      await act(async () => { box.fire() })
      await settle()
      expect(countEarlierPages()).toBe(1)

      const wheelsBefore = box.wheels()
      await act(async () => { box.wheel(-120) })
      await settle()

      expect(box.wheels()).toBe(wheelsBefore + 1)
      expect(countEarlierPages()).toBe(2)
      expect(countReq(`GET ${MESSAGES_URL}?before=2048`)).toBe(1)
      expect([...container.querySelectorAll('.msg-user-bubble')].map(e => e.textContent))
        .toEqual(['older-0', 'older-1', 'older-2', 'recent-0', 'recent-1', 'recent-2', 'recent-3', 'recent-4'])
    })

    it('ignores a gesture pushing AWAY from the end the reader is standing at', async () => {
      // The sign is what separates the two ends, and it has to be read: a transcript short
      // enough not to overfill the viewport reads as being at BOTH ends at once, and this pane
      // is at the bottom of a held session with an earlier page available, so a direction-blind
      // handler would fire the wrong end — or both. Pushing away from an end is also the
      // ordinary case that MOVES the box, so it belongs to the scroll path entirely.
      pages[MESSAGES_URL] = { entries: turns('recent', 10, 500), earlier: 4096 }
      pages[`${MESSAGES_URL}?before=4096`] = { entries: turns('older', 5, 100), earlier: 2048 }
      const container = await renderHeld()
      const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)

      await act(async () => { box.scrollTo(300) })
      await settle()
      await act(async () => { box.scrollTo(700) })
      await settle()
      const getsBefore = countNewestGets()
      const pagesBefore = countEarlierPages()

      // At the bottom, pulling UP: the browser will scroll, so there is nothing for this path
      // to do — and in particular it must not read "at the bottom" and refetch.
      pastFloor()
      await act(async () => { box.wheel(-120) })
      await settle()
      expect(box.wheels()).toBe(1)
      expect(countNewestGets()).toBe(getsBefore)
      // …and it did not fire the TOP either: the reader is 700px from it.
      expect(countEarlierPages()).toBe(pagesBefore)

      // The CONTROL, so the two counts above are known to discriminate.
      pastFloor()
      await act(async () => { box.wheel(120) })
      await settle()
      expect(countNewestGets()).toBe(getsBefore + 1)
    })

    it('stays out of the region the latch governs: near an end is not AT it', async () => {
      // The bound that makes this a second entry rather than a bypass, expressed behaviourally
      // rather than only as a constant. 30px from the bottom is inside TRANSCRIPT_EDGE_PX but
      // the box can still MOVE, so a real wheel there scrolls, fires a `scroll`, and the latch
      // is the right authority over whether that arrival loads. Widening the slack towards the
      // arrival threshold would let this path fire where the latch is meaningful — which is
      // exactly the loop tether#110 bounded.
      pages[MESSAGES_URL] = { entries: turns('recent', 10, 500) }
      const container = await renderHeld()
      const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)

      await act(async () => { box.scrollTo(300) })
      await settle()
      await act(async () => { box.scrollTo(670) })   // 1000 - 670 - 300 = 30: near, not at
      await settle()
      const gets = countNewestGets()

      pastFloor()
      await act(async () => { box.wheel(120) })
      await settle()
      expect(box.wheels()).toBe(1)
      expect(countNewestGets()).toBe(gets)

      // The CONTROL: the same gesture 30px further on, where the box really is clamped.
      await act(async () => { box.scrollTo(700) })
      await settle()
      const gets2 = countNewestGets()
      pastFloor()
      await act(async () => { box.wheel(120) })
      await settle()
      expect(countNewestGets()).toBe(gets2 + 1)
    })

    it('answers a wheel of any magnitude, because its unit is not pixels', async () => {
      // A fine trackpad scroll reports `deltaY` around 1, and `deltaMode` can make the number
      // a line or page count rather than a pixel count at all. Applying the TOUCH floor to a
      // wheel would therefore throw away the gentlest half of desktop scrolling while passing
      // every other test in this file, all of which spin a fat 120.
      pages[MESSAGES_URL] = { entries: turns('recent', 10, 500) }
      const container = await renderHeld()
      const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)

      await act(async () => { box.scrollTo(300) })
      await settle()
      await act(async () => { box.scrollTo(700) })
      await settle()
      const gets = countNewestGets()

      pastFloor()
      await act(async () => { box.wheel(1) })
      await settle()
      expect(countNewestGets()).toBe(gets + 1)
    })

    it('shares the interval floor with the scroll path, so a gesture cannot double an arrival', async () => {
      // Why the gesture stamps the SAME `…FiredAt` ref rather than getting its own budget. A
      // touch drag delivers `touchmove` and `scroll` interleaved, so an arrival and a pull land
      // in the same moment routinely; two budgets would make that two megabyte reads.
      //
      // The arrival is allowed to SETTLE first, which is the separation tether#110's own
      // battery had to learn: with the request still in flight the shared in-flight ref would
      // be the thing refusing and this would be measuring that guard instead. `settle()` costs
      // no simulated time (the clock is a frozen spy), so after it `inFlight` is false and
      // `sinceLastMs` is 0 — the floor is the only thing left that can refuse.
      pages[MESSAGES_URL] = { entries: turns('recent', 10, 500) }
      const container = await renderHeld()
      const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)

      await act(async () => { box.scrollTo(300) })
      await settle()
      await act(async () => { box.scrollTo(700) })
      await settle()
      const gets = countNewestGets()

      await act(async () => { box.wheel(120) })
      await settle()
      expect(box.wheels()).toBe(1)
      expect(countNewestGets()).toBe(gets)

      // The CONTROL: one floor later the same gesture is answered.
      pastFloor()
      await act(async () => { box.wheel(120) })
      await settle()
      expect(countNewestGets()).toBe(gets + 1)
    })

    it('stamps the floor ITSELF, so a second pull is not free after a first', async () => {
      // The gap the mutation battery found in the case above, and it is tether#110's own
      // lesson recurring: that test separates an ARRIVAL from a pull, so the stamp being
      // measured is the arrival's. Deleting the gesture path's own stamp left it green.
      // Two GESTURES in a row is the only shape that can see it — and the first is allowed
      // to settle, so the shared in-flight ref is not the thing refusing the second either.
      pages[MESSAGES_URL] = { entries: turns('recent', 10, 500) }
      const container = await renderHeld()
      const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)

      await act(async () => { box.scrollTo(300) })
      await settle()
      await act(async () => { box.scrollTo(700) })
      await settle()
      const gets = countNewestGets()

      pastFloor()
      await act(async () => { box.wheel(120) })
      await settle()
      expect(countNewestGets()).toBe(gets + 1)

      // No pastFloor: the clock has not moved since the pull above, so the ONLY thing that
      // can refuse this one is the stamp that pull left behind.
      await act(async () => { box.wheel(120) })
      await settle()
      expect(box.wheels()).toBe(2)
      expect(countNewestGets()).toBe(gets + 1)

      // The CONTROL: one floor later it is answered, so "1" is not simply a pane that has
      // stopped responding to gestures.
      pastFloor()
      await act(async () => { box.wheel(120) })
      await settle()
      expect(countNewestGets()).toBe(gets + 2)
    })

    it('does not spend the floor on a gesture the in-flight ref refused', async () => {
      // What the DECISION-SITE in-flight check buys over `refreshNewest`'s own duplicate one,
      // which tether#110 documented as an accepted mutation survivor and which — measured
      // here — is what silently absorbs the loss if this one is deleted. The request count is
      // identical either way; what differs is whether a refused gesture burns the 500ms
      // budget belonging to the gesture that comes after it.
      //
      // The construction: hold a request open at the OTHER end (the fallback button starts
      // one from any scroll position, which is what makes this reachable while the reader
      // stands at the bottom), pull at the bottom, then release and pull again with the clock
      // untouched. Correctly, the refused pull left no trace and the second one is answered.
      pages[MESSAGES_URL] = { entries: turns('recent', 10, 500), earlier: 4096 }
      pages[`${MESSAGES_URL}?before=4096`] = { entries: turns('older', 5, 100), earlier: 2048 }
      const container = await renderHeld()
      const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)

      await act(async () => { box.scrollTo(300) })
      await settle()
      await act(async () => { box.scrollTo(700) })
      await settle()
      const gets = countNewestGets()
      pastFloor()

      let release = () => {}
      gates[`GET ${MESSAGES_URL}?before=4096`] = new Promise<void>(r => { release = r })
      await act(async () => { (container.querySelector('.transcript-more') as HTMLButtonElement).click() })
      await settle()
      expect(countEarlierPages()).toBe(1)

      await act(async () => { box.wheel(120) })
      await settle()
      expect(box.wheels()).toBe(1)
      expect(countNewestGets()).toBe(gets)

      await act(async () => { release(); await Promise.resolve() })
      await settle()
      // The anchor correction kept the reader on the same message, which is still the end:
      // 1500 - 1200 - 300 = 0. Asserted because the pull below depends on it.
      expect(box.top()).toBe(1200)
      expect(box.height()).toBe(1500)

      // Clock untouched since the refused pull. If that pull had stamped the floor, this one
      // would be inside it.
      await act(async () => { box.wheel(120) })
      await settle()
      expect(countNewestGets()).toBe(gets + 1)
    })

    it('holds one request at a time however long the wheel spins', async () => {
      // The bound that replaces the latch on this path, and the one that matters: a continuous
      // trackpad flick is ~60 wheel events a second. The floor is stepped PAST between every
      // one of them, so the floor cannot be what is refusing — this isolates the shared
      // in-flight ref.
      pages[MESSAGES_URL] = { entries: turns('recent', 10, 500) }
      const container = await renderHeld()
      const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)

      await act(async () => { box.scrollTo(300) })
      await settle()
      await act(async () => { box.scrollTo(700) })
      await settle()
      const gets = countNewestGets()

      let release = () => {}
      gates[`GET ${MESSAGES_URL}`] = new Promise<void>(r => { release = r })
      pastFloor()
      await act(async () => { box.wheel(120) })
      await settle()
      expect(countNewestGets()).toBe(gets + 1)

      for (let i = 0; i < 6; i++) {
        pastFloor()
        await act(async () => { box.wheel(120) })
        await settle()
      }
      expect(box.wheels()).toBe(7)
      expect(countNewestGets()).toBe(gets + 1)

      // The CONTROL: once it settles, the next spin is answered.
      await act(async () => { release(); await Promise.resolve() })
      await settle()
      pastFloor()
      await act(async () => { box.wheel(120) })
      await settle()
      expect(countNewestGets()).toBe(gets + 2)
    })

    it('pulls at NO ceiling: not at a complete top, and not in a session with a live stream', async () => {
      // The judgement this lane keeps re-making, now reachable from a second direction. The
      // gesture skips the latch, so `available` is the ONLY thing standing between a pull and
      // three dots over a place nothing can arrive.
      pages[MESSAGES_URL] = { entries: turns('recent', 10, 500) }   // no cursor: complete
      const container = await renderIdle()
      const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)
      expect(useStore.getState().transcriptEarlier).toBeNull()
      const getsBefore = countNewestGets()

      // At the top of a complete transcript, pulling up.
      pastFloor()
      await act(async () => { box.wheel(-120) })
      await settle()
      expect(box.wheels()).toBe(1)
      expect(countEarlierPages()).toBe(0)
      expect(container.querySelectorAll('.transcript-dots')).toHaveLength(0)
      expect(container.querySelector('.transcript-top .transcript-top-note')?.textContent)
        .toBe(TRANSCRIPT_START_COMPLETE)

      // At the bottom of a session that is NOT being read without a stream: the poll this
      // pre-empts does not exist there, and `refreshTranscript` has no `!streaming` guard.
      pastFloor()
      await act(async () => { box.scrollTo(700) })
      await settle()
      await act(async () => { box.wheel(120) })
      await settle()
      expect(box.wheels()).toBe(2)
      expect(countNewestGets()).toBe(getsBefore)
      expect(container.querySelectorAll('.transcript-bottom')).toHaveLength(0)
    })

    it('works on a transcript too short to scroll at all, which no scroll event can reach', async () => {
      // A property this path adds rather than restores. 200px of content in a 300px viewport
      // produces no scroll event ever — which is why the fallback button had to stay for the
      // top end — and a gesture is delivered regardless of whether anything can move.
      pages[MESSAGES_URL] = { entries: turns('recent', 2, 500) }
      const container = await renderHeld()
      const box = liveScrollBox(container.querySelector('.dt-chat') as HTMLElement, 300)
      expect(box.height()).toBe(200)
      // The precondition: this box cannot scroll, so `scroll` is not merely quiet here, it is
      // impossible.
      await act(async () => { box.fire() })
      await settle()
      const getsBefore = countNewestGets()

      pastFloor()
      pages[MESSAGES_URL] = { entries: [...turns('recent', 2, 500), entry('assistant', 'brand new', 9000)] }
      await act(async () => { box.wheel(120) })
      await settle()

      expect(countNewestGets()).toBe(getsBefore + 1)
      expect(screen.getByText('brand new')).toBeTruthy()
    })
  })
})
