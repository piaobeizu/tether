// tether#103 — the shared activity poller.
//
// The wi's hard requirement is that the marker CHANGES when the state does, and
// its named worst case is a mutation nobody would notice: delete the refresh and
// every render test still passes, because a frozen marker renders exactly like a
// live one. So the tests here are about the MECHANISM — that a timer exists, that
// there is one of it, that it stops when nobody is looking — rather than about the
// shape of one response.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  SESSION_ACTIVITY_HELD,
  SESSION_ACTIVITY_IDLE,
  SESSION_ACTIVITY_PATH,
  SESSION_ACTIVITY_POLL_MS,
  SESSION_ACTIVITY_WORKING,
  fetchSessionActivity,
  resetSessionActivityForTests,
  sessionActivityPollerState,
  subscribeSessionActivity,
  type SessionActivityMap,
} from './sessionActivity'

/**
 * daemon stubs fetch with a body the test can change between polls, and counts the
 * requests that were actually issued.
 *
 * The body is RAW JSON parsed at the fetch boundary rather than a typed object.
 * That is the lesson tether#101 measured: a typed fixture is immune to the
 * property NAMES, so a rename on the frontend side alone kept 24 tests green and
 * `tsc -b` at exit 0 while the feature was dead. A fixture the reader has to parse
 * is what pins the words on the wire.
 */
function daemon(body: () => string) {
  let calls = 0
  const fn = vi.fn(async (url: string) => {
    if (url !== SESSION_ACTIVITY_PATH) throw new Error(`unexpected fetch: ${url}`)
    calls++
    const text = body()
    return { ok: true, status: 200, json: async () => JSON.parse(text) as unknown }
  })
  vi.stubGlobal('fetch', fn)
  return { fn, calls: () => calls }
}

/** Put the tab in the background (or back), the way the browser reports it. */
function setVisibility(state: 'visible' | 'hidden') {
  Object.defineProperty(document, 'visibilityState', { value: state, configurable: true })
  document.dispatchEvent(new Event('visibilitychange'))
}

beforeEach(() => {
  resetSessionActivityForTests()
  Object.defineProperty(document, 'visibilityState', { value: 'visible', configurable: true })
  vi.useFakeTimers()
})

afterEach(() => {
  resetSessionActivityForTests()
  vi.useRealTimers()
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('fetchSessionActivity', () => {
  it('reads the map the daemon sends, from raw JSON', async () => {
    daemon(() => `{"sid-a":"working","sid-b":"idle","sid-c":"held"}`)
    await expect(fetchSessionActivity()).resolves.toEqual({
      'sid-a': SESSION_ACTIVITY_WORKING,
      'sid-b': SESSION_ACTIVITY_IDLE,
      'sid-c': SESSION_ACTIVITY_HELD,
    })
  })

  it('pins the three state words and the path as the daemon spells them', () => {
    // Literals, not "some non-empty string". These four strings ARE the contract,
    // and the Go side has a ratchet that reads this file for exactly them; pinning
    // them here as well is what makes a one-sided rename fail on BOTH sides
    // instead of only in the guard that a hurried author might delete.
    expect(SESSION_ACTIVITY_WORKING).toBe('working')
    expect(SESSION_ACTIVITY_IDLE).toBe('idle')
    expect(SESSION_ACTIVITY_HELD).toBe('held')
    expect(SESSION_ACTIVITY_PATH).toBe('/api/v1/session-activity')
  })

  it('drops a state this build does not know, rather than passing it through', async () => {
    daemon(() => `{"sid-a":"working","sid-b":"teleporting"}`)
    await expect(fetchSessionActivity()).resolves.toEqual({ 'sid-a': SESSION_ACTIVITY_WORKING })
  })

  it('survives the shapes that are not a map', async () => {
    for (const body of ['null', '[]', '"nope"', '{"sid-a":7}']) {
      daemon(() => body)
      await expect(fetchSessionActivity()).resolves.toEqual({})
    }
  })

  it('throws on a daemon error so the caller can keep the previous answer', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => ({ ok: false, status: 503, json: async () => ({}) })))
    await expect(fetchSessionActivity()).rejects.toThrow(/503/)
  })
})

describe('the shared poller', () => {
  it('refreshes on its own clock — the state CHANGES without a remount', async () => {
    // The heart of it, and the mutation this whole file exists for: delete the
    // interval and the first assertion still passes while this one fails.
    let body = `{"sid-a":"idle"}`
    const d = daemon(() => body)
    const seen: SessionActivityMap[] = []
    subscribeSessionActivity(m => { seen.push(m) })

    await vi.advanceTimersByTimeAsync(0)
    expect(seen.at(-1)).toEqual({ 'sid-a': SESSION_ACTIVITY_IDLE })

    body = `{"sid-a":"working"}`
    await vi.advanceTimersByTimeAsync(SESSION_ACTIVITY_POLL_MS)
    expect(seen.at(-1)).toEqual({ 'sid-a': SESSION_ACTIVITY_WORKING })

    body = `{}`
    await vi.advanceTimersByTimeAsync(SESSION_ACTIVITY_POLL_MS)
    // Back to nothing: the sid is ABSENT, not present-and-idle. A poller that
    // merged answers instead of replacing them would leave "working" on screen
    // forever once it had seen it.
    expect(seen.at(-1)).toEqual({})
    expect(d.calls()).toBe(3)
  })

  it('polls ONCE per tick however many consumers are subscribed', async () => {
    const d = daemon(() => `{"sid-a":"working"}`)
    const offs: (() => void)[] = []
    // Each subscription is allowed to SETTLE before the next one arrives, which is
    // the whole point of this shape. Subscribing three times in one synchronous
    // burst does not test the sharing: the in-flight guard collapses three
    // simultaneous polls into one all by itself, so a mutant that polls per
    // subscriber survives it. Measured — that mutant did survive the burst version
    // of this test, and this is what kills it.
    for (let i = 0; i < 3; i++) {
      offs.push(subscribeSessionActivity(() => {}))
      await vi.advanceTimersByTimeAsync(0)
    }
    // One immediate poll for the whole page, not one per subscriber. A row is
    // rendered by two panes and a list has many rows, so a fetch per consumer
    // multiplies the mount cost by the row count.
    expect(d.calls()).toBe(1)
    expect(sessionActivityPollerState()).toEqual({ running: true, subscribers: 3 })

    await vi.advanceTimersByTimeAsync(SESSION_ACTIVITY_POLL_MS)
    expect(d.calls()).toBe(2)

    for (const off of offs) off()
    // Asserted on the TIMER, not only on the request count: "still ticking with
    // nobody listening" is the leak a reference count exists to prevent, and it is
    // invisible to a count taken right after the last unsubscribe.
    expect(sessionActivityPollerState()).toEqual({ running: false, subscribers: 0 })
    await vi.advanceTimersByTimeAsync(SESSION_ACTIVITY_POLL_MS * 3)
    expect(d.calls()).toBe(2)
  })

  it('does not accumulate an interval per subscriber', async () => {
    // The other half of "one poller", and it is a LEAK rather than a cost: a module
    // that installed a fresh interval on every subscribe would keep only the last
    // handle, so unsubscribing everyone would clear one timer and leave the rest
    // firing forever — with `running: false` reported the whole time.
    const d = daemon(() => `{}`)
    const offs = [
      subscribeSessionActivity(() => {}),
      subscribeSessionActivity(() => {}),
      subscribeSessionActivity(() => {}),
    ]
    await vi.advanceTimersByTimeAsync(0)
    for (const off of offs) off()
    expect(sessionActivityPollerState().running).toBe(false)
    const settled = d.calls()
    await vi.advanceTimersByTimeAsync(SESSION_ACTIVITY_POLL_MS * 5)
    expect(d.calls()).toBe(settled)
  })

  it('pauses while the tab is hidden and refetches the moment it is shown', async () => {
    const d = daemon(() => `{"sid-a":"working"}`)
    subscribeSessionActivity(() => {})
    await vi.advanceTimersByTimeAsync(0)
    expect(d.calls()).toBe(1)

    setVisibility('hidden')
    expect(sessionActivityPollerState().running).toBe(false)
    await vi.advanceTimersByTimeAsync(SESSION_ACTIVITY_POLL_MS * 5)
    expect(d.calls()).toBe(1)

    setVisibility('visible')
    // Immediately, not on the next tick. Waiting out the interval would show a
    // marker up to a whole poll stale on the frame the user is looking at — which
    // is the defect the pause introduces if only half of it is implemented, and it
    // looks exactly like the frozen marker the pause was traded for.
    await vi.advanceTimersByTimeAsync(0)
    expect(d.calls()).toBe(2)
    expect(sessionActivityPollerState().running).toBe(true)
  })

  it('polls at 3s — pinned as a value, not as a range', async () => {
    // A literal, because a range would be satisfied by the mutant: tether#102
    // measured a real defect surviving `toBeLessThan`-shaped assertions. 3s is the
    // decided interval, so 3s is what is asserted.
    expect(SESSION_ACTIVITY_POLL_MS).toBe(3000)
    const d = daemon(() => `{}`)
    subscribeSessionActivity(() => {})
    await vi.advanceTimersByTimeAsync(0)
    expect(d.calls()).toBe(1)
    await vi.advanceTimersByTimeAsync(2999)
    expect(d.calls()).toBe(1)
    await vi.advanceTimersByTimeAsync(1)
    expect(d.calls()).toBe(2)
  })

  it('keeps the previous answer when a poll fails, and recovers on the next tick', async () => {
    let fail = false
    const fn = vi.fn(async () => {
      if (fail) throw new Error('offline')
      return { ok: true, status: 200, json: async () => JSON.parse(`{"sid-a":"working"}`) as unknown }
    })
    vi.stubGlobal('fetch', fn)
    const seen: SessionActivityMap[] = []
    subscribeSessionActivity(m => { seen.push(m) })
    await vi.advanceTimersByTimeAsync(0)
    expect(seen.at(-1)).toEqual({ 'sid-a': SESSION_ACTIVITY_WORKING })

    fail = true
    await vi.advanceTimersByTimeAsync(SESSION_ACTIVITY_POLL_MS)
    // No new publish, so the last good answer is still what a consumer holds.
    // Blanking the row on a transient failure would flicker every marker off on a
    // single dropped request.
    expect(seen.at(-1)).toEqual({ 'sid-a': SESSION_ACTIVITY_WORKING })

    fail = false
    await vi.advanceTimersByTimeAsync(SESSION_ACTIVITY_POLL_MS)
    expect(seen.at(-1)).toEqual({ 'sid-a': SESSION_ACTIVITY_WORKING })
    expect(sessionActivityPollerState().running).toBe(true)
  })

  it('recovers from a request that never settles', async () => {
    // The overlap guard is a way for the poller to freeze ITSELF: `inFlight` is only
    // released in the finally of a settled fetch, so one request that never answers
    // would make every later tick a no-op while `running` kept reporting true — the
    // frozen marker this whole module exists to prevent, reached through its own
    // safety valve. The deadline is what releases it.
    let hangs = true
    let started = 0
    vi.stubGlobal('fetch', vi.fn(async (_url: string, init?: RequestInit) => {
      started++
      if (hangs) {
        // Settle only when the caller's own signal aborts. No signal ⇒ never.
        await new Promise((_res, rej) => {
          init?.signal?.addEventListener('abort', () => rej(new Error('TimeoutError')))
        })
      }
      return { ok: true, status: 200, json: async () => JSON.parse(`{"sid-a":"working"}`) as unknown }
    }))
    const seen: SessionActivityMap[] = []
    subscribeSessionActivity(m => { seen.push(m) })
    await vi.advanceTimersByTimeAsync(0)
    expect(started).toBe(1)
    expect(seen).toHaveLength(0)

    // The deadline fires, the guard is released, and the NEXT tick gets through.
    hangs = false
    await vi.advanceTimersByTimeAsync(SESSION_ACTIVITY_POLL_MS * 3)
    expect(started).toBeGreaterThan(1)
    expect(seen.at(-1)).toEqual({ 'sid-a': SESSION_ACTIVITY_WORKING })
  })

  it('does not overlap requests when the daemon is slower than the interval', async () => {
    // A holder object rather than a `let`: TypeScript narrows a `let` assigned only
    // inside a closure to `never` at the call below, and the point of this test is
    // the call.
    const gate: { release: (() => void) | null } = { release: null }
    let started = 0
    vi.stubGlobal('fetch', vi.fn(async () => {
      started++
      await new Promise<void>(res => { gate.release = res })
      return { ok: true, status: 200, json: async () => ({}) }
    }))
    // Deliberately ignores the abort signal: this case is about the guard holding
    // while a request is genuinely still in flight, which is the opposite of the
    // one above.
    subscribeSessionActivity(() => {})
    await vi.advanceTimersByTimeAsync(0)
    expect(started).toBe(1)

    // Three ticks go by while the first request is still open.
    await vi.advanceTimersByTimeAsync(SESSION_ACTIVITY_POLL_MS * 3)
    expect(started).toBe(1)

    gate.release?.()
    await vi.advanceTimersByTimeAsync(SESSION_ACTIVITY_POLL_MS)
    expect(started).toBe(2)
  })
})
