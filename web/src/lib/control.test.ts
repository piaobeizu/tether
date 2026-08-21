import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { ControlClient, activeControlClient } from './control'
import { createWT } from './wt'
import { ClientFrameResize } from './wire.gen'
import type { ClientFrame } from './wire.gen'

// createWT is the one await connect() cannot get past on its own here, so it is
// mocked for the whole file. The default below reproduces what the real one does
// under jsdom — there is no WebTransport constructor, so it throws — which is
// the behaviour the activeControlClient cases already relied on. Only the
// teardown cases at the bottom replace it, and each replaces it with a promise
// it resolves by hand.
vi.mock('./wt', () => ({ createWT: vi.fn() }))

beforeEach(() => {
  // mockReset, not just a fresh implementation: the call history is asserted
  // below to prove connect() really is parked in the await, and a count that
  // carried over from earlier cases would make that assertion depend on file
  // order rather than on the client.
  vi.mocked(createWT).mockReset()
  vi.mocked(createWT).mockRejectedValue(new Error('no WebTransport in jsdom'))
})

/**
 * These cover the send side of the terminal-resize lane (tether#68): that a
 * resize actually leaves as a well-formed frame carrying the numbers it was
 * given. The browser half (ResizeObserver → xterm onResize → here) needs a
 * real layout engine and is covered by live-verify instead.
 */

/** Captures everything written to the control stream, decoded to frames. */
function captureWriter(client: ControlClient): ClientFrame[] {
  const sent: ClientFrame[] = []
  const writer = {
    write(bytes: Uint8Array) {
      for (const line of new TextDecoder().decode(bytes).split('\n')) {
        if (line.trim()) sent.push(JSON.parse(line) as ClientFrame)
      }
      return Promise.resolve()
    },
  }
  // The writer is private and only ever set by connect(), which needs a live
  // WebTransport; inject directly so the frame shape can be asserted without one.
  ;(client as unknown as { writer: unknown }).writer = writer
  return sent
}

describe('ControlClient.sendResize', () => {
  let client: ControlClient
  let sent: ClientFrame[]

  beforeEach(() => {
    client = new ControlClient()
    sent = captureWriter(client)
  })

  it('sends a resize frame carrying the session id and the given dimensions', async () => {
    await client.sendResize('sid-42', 143, 41)

    expect(sent).toHaveLength(1)
    expect(sent[0]).toEqual({
      kind: ClientFrameResize,
      sessionId: 'sid-42',
      cols: 143,
      rows: 41,
    })
  })

  // 143 !== 41 above, so a transposed cols/rows would fail that assertion; this
  // pins the orientation explicitly so the intent survives a future refactor.
  it('does not transpose cols and rows', async () => {
    await client.sendResize('s', 200, 50)
    expect(sent[0].cols).toBe(200)
    expect(sent[0].rows).toBe(50)
  })

  it('keeps the empty session id — a shell can exist before any chat session', async () => {
    await client.sendResize('', 80, 24)
    expect(sent).toHaveLength(1)
    expect(sent[0].sessionId).toBe('')
  })

  it.each([
    ['zero cols', 0, 24],
    ['zero rows', 80, 0],
    ['both zero', 0, 0],
    ['negative', -1, 24],
  ])('drops %s instead of blanking the remote TUI', async (_name, cols, rows) => {
    await client.sendResize('sid', cols, rows)
    expect(sent).toHaveLength(0)
  })

  it('is a no-op when the control lane is not connected', async () => {
    const disconnected = new ControlClient()
    await expect(disconnected.sendResize('sid', 80, 24)).resolves.toBeUndefined()
  })
})

describe('activeControlClient', () => {
  it('is null until a client starts', () => {
    expect(activeControlClient()).toBeNull()
  })

  it('publishes the started client and clears it on stop', async () => {
    const client = new ControlClient()
    // start() attempts a real WT connect, which fails under vitest and is
    // swallowed into a reconnect timer — publication happens before that, which
    // is the behaviour ShellPane depends on.
    await client.start()
    expect(activeControlClient()).toBe(client)

    client.stop()
    expect(activeControlClient()).toBeNull()
  })

  it('stop() on a superseded client does not clear the live one', async () => {
    const first = new ControlClient()
    await first.start()
    const second = new ControlClient()
    await second.start()

    first.stop()

    expect(activeControlClient()).toBe(second)
    second.stop()
  })
})

/**
 * A promise plus its resolver, so a test can park code inside an await and
 * decide by hand when it comes back out. The point is that the ordering below is
 * a property of the fixture rather than a race the test hopes to win.
 */
function gate<T>() {
  let open!: (value: T) => void
  const promise = new Promise<T>((resolve) => { open = resolve })
  return { promise, open }
}

/**
 * A stand-in for the WebTransport createWT hands back, recording the two things
 * these cases are about: whether anybody closed it, and what got written to its
 * stream. `closed` never settles — this fixture is about the caller's own
 * teardown, not about the transport dying on its own.
 */
function fakeTransport(opts: { holdStream?: boolean } = {}) {
  const sent: ClientFrame[] = []
  const closes: WebTransportCloseInfo[] = []
  let streamRequests = 0
  const stream = gate<void>()
  if (!opts.holdStream) stream.open()

  const wt = {
    closed: new Promise<WebTransportCloseInfo>(() => {}),
    async createBidirectionalStream() {
      streamRequests++
      await stream.promise
      return {
        writable: {
          getWriter: () => ({
            write(bytes: Uint8Array) {
              for (const line of new TextDecoder().decode(bytes).split('\n')) {
                if (line.trim()) sent.push(JSON.parse(line) as ClientFrame)
              }
              return Promise.resolve()
            },
            releaseLock() {},
          }),
        },
        // Never yields a pong; readPongs is not what is under test here.
        readable: { getReader: () => ({ read: () => new Promise<never>(() => {}) }) },
      }
    },
    close(info: WebTransportCloseInfo) { closes.push(info) },
  }

  return {
    wt: wt as unknown as WebTransport,
    sent,
    closes,
    streamRequests: () => streamRequests,
    openStream: () => stream.open(),
  }
}

/**
 * The private ping-interval handle. Asserted directly for the reason
 * transcriptWatch.test.ts spells out for its own timer: counting what was sent
 * cannot see a leaked interval on its own, because sendPing() also needs the
 * writer that a correct teardown withholds — so an interval installed with no
 * writer would look exactly like no interval at all.
 */
function pingTimer(client: ControlClient): unknown {
  return (client as unknown as { timer: unknown }).timer
}

/**
 * Teardown that lands mid-connect (tether#128).
 *
 * stop() can only close what connect() has already published to `this.wt`, so a
 * stop that lands INSIDE one of connect()'s two awaits used to close null and
 * then stand by while connect() finished building a live WebTransport and a 5s
 * ping interval that nothing referenced anymore — `active` is already null by
 * then, so nothing could ever stop them again. handleLine's own `stopped` check
 * kept the latency number clean, which is why the leak was invisible.
 *
 * Both windows have a real trigger: React's StrictMode runs an effect, its
 * cleanup, then the effect again, so every dev load lands a stop in the first
 * connect's awaits; and ChatPane's reconnect path stops the outgoing
 * ControlClient without knowing whether it ever finished connecting.
 * transcriptWatch.ts's watchToken and wiSession.ts's module-level `migrated` are
 * the same defence, already written twice in this codebase.
 */
describe('ControlClient teardown during connect', () => {
  beforeEach(() => { vi.useFakeTimers() })
  afterEach(() => { vi.useRealTimers() })

  // The positive control for the two cases below, and the only case in this file
  // that gets all the way through connect(). Two things would otherwise be
  // unfalsifiable: that an empty `sent` means the teardown worked rather than
  // that the fixture never reached the ping loop at all; and that the teardown
  // only fires on the way OUT — releasing ownership of a published transport is
  // one line in connect(), and deleting it makes every successful connect close
  // its own transport immediately, which nothing else here would notice.
  it('leaves a connect nobody stopped running, and closes it on the later stop', async () => {
    const t = fakeTransport()
    vi.mocked(createWT).mockResolvedValue(t.wt)

    const client = new ControlClient()
    await client.start()
    await vi.advanceTimersByTimeAsync(0)

    expect(t.closes).toEqual([])
    expect(t.sent).toHaveLength(1)          // the immediate ping
    expect(pingTimer(client)).not.toBeNull()
    await vi.advanceTimersByTimeAsync(60_000)
    expect(t.sent.length).toBeGreaterThan(1)   // ...and the interval after it

    client.stop()
    expect(t.closes).toEqual([{ closeCode: 0, reason: 'client close' }])
  })

  it('closes the transport handed to it after the stop, and starts no ping loop', async () => {
    const connecting = gate<WebTransport>()
    vi.mocked(createWT).mockReturnValue(connecting.promise)
    const t = fakeTransport()

    const client = new ControlClient()
    const started = client.start()          // parks in `await createWT(url)`
    expect(vi.mocked(createWT)).toHaveBeenCalledTimes(1)   // the window is open

    client.stop()                            // this.wt is still null right here
    connecting.open(t.wt)                    // ...and only now does the await return
    await started
    await vi.advanceTimersByTimeAsync(0)

    expect(t.closes).toEqual([{ closeCode: 0, reason: 'client close' }])
    expect(pingTimer(client)).toBeNull()
    // Nothing was asked of the transport on the way out either. Dropping this
    // assertion makes the stopped re-read after the FIRST await redundant — the
    // one after the second await plus the teardown produce the same closes/sent
    // numbers — so without it that clause could be deleted with this file still
    // green, and every stop landing in the cert-hash fetch would spend a stream
    // open on a session it is about to close.
    expect(t.streamRequests()).toBe(0)
    // A full minute, so this does not encode PING_INTERVAL_MS.
    await vi.advanceTimersByTimeAsync(60_000)
    expect(t.sent).toEqual([])
  })

  it('closes the transport when the stop lands in the createBidirectionalStream await', async () => {
    // The narrower of the two windows, and the assertions divide differently.
    // Before the fix `this.wt` had already been published by the time stop()
    // ran, so stop's own close() fired — the closes assertion passes either way.
    // The interval did not survive either, but only by accident: the old code
    // reached `this.wt.closed` after stop() had nulled that field, and the
    // TypeError landed in connect's own catch, which cleared the interval it had
    // just installed. What got through was the writer and the ping written to a
    // transport already told to go away, so that is the discriminating
    // assertion here.
    const t = fakeTransport({ holdStream: true })
    vi.mocked(createWT).mockResolvedValue(t.wt)

    const client = new ControlClient()
    const started = client.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(t.streamRequests()).toBe(1)       // parked in the second await

    client.stop()
    t.openStream()
    await started
    await vi.advanceTimersByTimeAsync(0)

    expect(t.closes).toContainEqual({ closeCode: 0, reason: 'client close' })
    expect(pingTimer(client)).toBeNull()
    await vi.advanceTimersByTimeAsync(60_000)
    expect(t.sent).toEqual([])
  })
})
