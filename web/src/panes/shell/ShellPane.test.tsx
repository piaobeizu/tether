import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react'
import ShellPane from './index'

/**
 * tether#151 — the pane used to die in silence, and this file is the assertion
 * that it no longer does.
 *
 * What "the defect exists" looks like, stated as a number so these cases can be
 * checked against it: with the pre-#151 pane mounted, `queryByTestId('shell-status')`
 * is null BOTH before and after the shell stream ends. Every case below therefore
 * asserts the before-state as well as the after-state — a banner that was always on
 * screen would satisfy the second half on its own, and would be just as useless.
 *
 * These cases assert the literal sentences the user reads, not constants imported
 * from the pane. That is deliberate on two counts: the text is the deliverable (a
 * status variable nothing renders is an empty fix that passes every test), and a
 * test that imports the string it checks cannot be run against a build that does
 * not have it.
 */

/** One read result, or one settled `closed` — enough of a promise to drive by hand. */
const wtFixture = vi.hoisted(() => {
  interface Deferred<T> { promise: Promise<T>; resolve: (v: T) => void; reject: (e: unknown) => void }

  function defer<T>(): Deferred<T> {
    let resolve!: (v: T) => void
    let reject!: (e: unknown) => void
    const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej })
    // The pane attaches its handler an await or two later than the test settles
    // this, and node counts the gap as an unhandled rejection. A no-op handler
    // here does not stop the pane's own from running.
    promise.catch(() => {})
    return { promise, resolve, reject }
  }

  interface ReadResult { done: boolean; value?: Uint8Array }

  /**
   * A stand-in for one /wt/shell WebTransport whose stream and session END WHEN
   * THE TEST SAYS SO. The four verbs at the bottom are the four ways this pane
   * can lose a shell; nothing else in the fixture decides anything.
   */
  class FakeShell {
    /** The read the pane is parked on right now, if any. */
    pendingRead: Deferred<ReadResult> | null = null
    /** Bytes the terminal sent up the stream. */
    readonly writes: Uint8Array[] = []
    /** How many times anybody closed this transport (the effect cleanup does). */
    closes = 0

    private readonly closedGate = defer<void>()

    constructor(readonly url: string) {}

    get closed(): Promise<void> { return this.closedGate.promise }

    createBidirectionalStream = async () => ({
      writable: {
        getWriter: () => ({
          write: async (b: Uint8Array) => { this.writes.push(b) },
          close: async () => {},
        }),
      },
      readable: {
        getReader: () => ({
          read: () => {
            const d = defer<ReadResult>()
            this.pendingRead = d
            return d.promise
          },
        }),
      },
    })

    close(): void { this.closes++ }

    /** The daemon closed the stream: a clean FIN, which is what `exit` produces. */
    endStream(): void { this.pendingRead?.resolve({ done: true }) }
    /** The stream was reset under us mid-read. */
    breakStream(err: Error): void { this.pendingRead?.reject(err) }
    /** The session was closed gracefully, with the stream left as it was. */
    closeSession(): void { this.closedGate.resolve() }
    /** The session went away abruptly — what Chrome reports for most closes. */
    loseSession(err: Error): void { this.closedGate.reject(err) }
  }

  const instances: FakeShell[] = []
  /** Set to make the NEXT createWT reject — a connection that never comes up. */
  let failNext: Error | null = null

  const createWT = vi.fn(async (url: string) => {
    if (failNext) { const err = failNext; failNext = null; throw err }
    const s = new FakeShell(url)
    instances.push(s)
    return s
  })

  return {
    createWT,
    instances,
    failWith(err: Error) { failNext = err },
    reset() { instances.length = 0; failNext = null; createWT.mockClear() },
  }
})

vi.mock('../../lib/wt', () => ({ createWT: wtFixture.createWT }))
// The pane reports every size xterm settles on down the control lane (tether#68).
// None of these cases are about that, and a null client is the state the app is
// really in until /wt/control connects.
vi.mock('../../lib/control', () => ({ activeControlClient: () => null }))

/** jsdom has neither of these, and xterm's Terminal.open() and the pane both need one. */
class StubResizeObserver {
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
}

const ENDED_HEADLINE = 'Shell disconnected'
const ENDED_DETAIL_FRAGMENT = 'tether cannot tell which, so it will not start a new one for you.'
const FAILED_HEADLINE = 'Shell connection failed'
const RESTART_LABEL = 'Start a new shell'

describe('ShellPane — the pane says what happened when the shell goes away (tether#151)', () => {
  beforeEach(() => {
    wtFixture.reset()
    ;(globalThis as unknown as { ResizeObserver: unknown }).ResizeObserver = StubResizeObserver
    ;(window as unknown as { matchMedia: unknown }).matchMedia = () => ({
      matches: false, media: '', onchange: null,
      addEventListener: () => {}, removeEventListener: () => {},
      addListener: () => {}, removeListener: () => {}, dispatchEvent: () => false,
    })
  })

  afterEach(() => { cleanup() })

  /**
   * Lets the microtask queue AND the timer queue drain, twice.
   *
   * Two macrotask yields rather than a counted number of microtask ticks: the
   * pane's end paths are `reader.read().then(...)` and `wt.closed.then(...)`
   * chained behind two awaits inside connect(), and a fixed tick count that
   * happens to be too small passes whether or not the pane reports anything —
   * which is the one outcome a test of a silent failure must not have.
   */
  const settle = () => act(async () => {
    await new Promise((r) => setTimeout(r, 0))
    await new Promise((r) => setTimeout(r, 0))
  })

  /**
   * Mounts the pane and leaves it parked on a live read, with every precondition
   * the cases rest on checked here — a harness that stopped reaching this state
   * would fail loudly instead of turning the assertions below into vacuous truths.
   */
  async function mountLive() {
    const { container } = render(<ShellPane />)
    await settle()
    expect(wtFixture.createWT).toHaveBeenCalledTimes(1)
    const shell = wtFixture.instances[0]
    expect(shell.url).toContain('/wt/shell')
    // The pump really is waiting on the stream — without this the "end it now"
    // calls below would resolve nothing and prove nothing.
    expect(shell.pendingRead).not.toBeNull()
    // THE DEFECT'S VALUE: nothing on screen yet, which is also all the old pane
    // ever showed, before the stream ended and after.
    expect(screen.queryByTestId('shell-status')).toBeNull()
    return { shell, container }
  }

  it('says the shell is gone when the stream ends cleanly, and does not reconnect', async () => {
    const { shell } = await mountLive()

    shell.endStream()
    await settle()

    const banner = screen.getByTestId('shell-status')
    expect(banner.textContent).toContain(ENDED_HEADLINE)
    expect(banner.textContent).toContain(ENDED_DETAIL_FRAGMENT)
    // Not a differential on its own — the old pane opened one transport too. It is
    // here so that a later "just reconnect it" change has to come past this line.
    expect(wtFixture.createWT).toHaveBeenCalledTimes(1)
  })

  it('says the same thing when the stream is reset instead of closed', async () => {
    const { shell } = await mountLive()

    shell.breakStream(new Error('WebTransportError: Connection lost'))
    await settle()

    expect(screen.getByTestId('shell-status').textContent).toContain(ENDED_HEADLINE)
    expect(wtFixture.createWT).toHaveBeenCalledTimes(1)
  })

  it('says it when the SESSION dies and the stream never settles at all', async () => {
    const { shell } = await mountLive()

    // Chrome rejects `closed` even for a graceful daemon-side close (lib/wt.ts,
    // tether#63), so the rejecting path is the ordinary one and is what is
    // exercised here.
    shell.loseSession(new Error('WebTransportError: Connection lost'))
    await settle()

    expect(screen.getByTestId('shell-status').textContent).toContain(ENDED_HEADLINE)
  })

  it('says it when the session closes gracefully too', async () => {
    const { shell } = await mountLive()

    shell.closeSession()
    await settle()

    expect(screen.getByTestId('shell-status').textContent).toContain(ENDED_HEADLINE)
  })

  it('puts the reason on screen when the connection never comes up', async () => {
    wtFixture.failWith(new Error('cert hash mismatch'))

    render(<ShellPane />)
    await settle()

    const banner = screen.getByTestId('shell-status')
    expect(banner.textContent).toContain(FAILED_HEADLINE)
    expect(banner.textContent).toContain('cert hash mismatch')
  })

  it('starts a second shell only when the user clicks for one', async () => {
    const { shell, container } = await mountLive()
    expect(container.querySelectorAll('.xterm')).toHaveLength(1)
    shell.endStream()
    await settle()
    expect(screen.getByTestId('shell-status')).not.toBeNull()
    // Nothing has restarted on its own in the meantime.
    expect(wtFixture.createWT).toHaveBeenCalledTimes(1)

    await act(async () => { fireEvent.click(screen.getByText(RESTART_LABEL)) })
    await settle()

    expect(wtFixture.createWT).toHaveBeenCalledTimes(2)
    // The banner is the pane's claim about right now, so a live shell must clear it.
    expect(screen.queryByTestId('shell-status')).toBeNull()
    expect(wtFixture.instances[1].pendingRead).not.toBeNull()
    // The restart rebuilds the Terminal in the SAME container div, so the old one
    // has to have taken its DOM with it. Two stacked terminals is what a restart
    // that only re-ran the effect would leave behind, and it is invisible to every
    // other assertion here.
    expect(container.querySelectorAll('.xterm')).toHaveLength(1)
    expect(shell.closes).toBe(1)
  })
})
