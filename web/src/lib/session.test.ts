// tether#61 — openSession is the single "open a different session" operation.
// Before it existed, ChatPane and the WorkspacePane session list each had their
// own version and they had drifted: the workspace one never reconnected the
// WebTransport channel, and it hid setSessionId (which is what persists
// tether_last_sid) behind a non-empty history. Each test below pins one half of
// that divergence, or one of the hazards centralising it made cheap to close.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { openSession } from './session'
import { useStore, type Message } from './store'

/** Drain openSession's fetch chain. Every hop in it is a microtask and this is
 *  a macrotask, so all of them have run by the time this resolves. */
const settle = () => new Promise<void>(r => setTimeout(r, 0))

/** Stub /messages with a fixed reply. `ok: false` models a daemon-side failure. */
function mockMessages(entries: unknown[], ok = true) {
  const fetchMock = vi.fn(async () => ({ ok, status: ok ? 200 : 500, json: async () => entries }))
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

/** Record every 'tether:retry-connection' — the app's "rebind the WT" channel,
 *  which ChatPane (owner of the connection) listens on. */
function watchReconnects(): { count: () => number; sidAtDispatch: () => (string | null)[] } {
  const seen: (string | null)[] = []
  const onRetry = () => seen.push(localStorage.getItem('tether_last_sid'))
  window.addEventListener('tether:retry-connection', onRetry)
  listeners.push(() => window.removeEventListener('tether:retry-connection', onRetry))
  return { count: () => seen.length, sidAtDispatch: () => seen }
}
let listeners: (() => void)[] = []

const msg = (id: string, text: string, ts: number): Message =>
  ({ id, role: 'user', text, ts })

beforeEach(() => {
  localStorage.clear()
  useStore.setState({ sessionId: null, messages: [], notices: [], pendingPermissions: [] })
})

afterEach(() => {
  for (const off of listeners) off()
  listeners = []
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('openSession (tether#61)', () => {
  it('reconnects the WebTransport channel — the bug the workspace list had', async () => {
    mockMessages([{ role: 'user', text: 'hi', ts: 1 }])
    const wt = watchReconnects()

    openSession('sid-b')

    // Not "eventually": the channel must be rebound even if /messages is slow
    // or never answers, because until it is, the live stream AND the next
    // prompt the user sends still belong to the session they just left.
    expect(wt.count()).toBe(1)
    await settle()
    expect(wt.count()).toBe(1) // exactly one, not one per promise hop
  })

  it('persists the sid BEFORE asking for the reconnect (doConnect reads it)', () => {
    mockMessages([])
    const wt = watchReconnects()

    openSession('sid-b')

    // ChatPane's doConnect builds its `?sid=` from tether_last_sid. If the
    // reconnect were requested first, the fresh connection would resume the OLD
    // session and the switch would silently undo itself.
    expect(wt.sidAtDispatch()).toEqual(['sid-b'])
  })

  it('switches and persists even when the session has NO history', async () => {
    mockMessages([])

    openSession('sid-empty')
    await settle()

    // The workspace list used to call setSessionId inside `if (msgs.length > 0)`,
    // so this case changed nothing at all — while having already dropped the
    // notices.
    expect(useStore.getState().sessionId).toBe('sid-empty')
    expect(localStorage.getItem('tether_last_sid')).toBe('sid-empty')
  })

  it('switches and persists even when /messages fails', async () => {
    mockMessages([], false)

    openSession('sid-b')
    await settle()

    expect(useStore.getState().sessionId).toBe('sid-b')
    expect(localStorage.getItem('tether_last_sid')).toBe('sid-b')
  })

  it('loads the target session history into the transcript', async () => {
    mockMessages([
      { role: 'user', text: 'B-only prompt', ts: 10 },
      { role: 'assistant', text: 'B-only answer', ts: 20 },
    ])
    useStore.setState({ messages: [msg('a1', 'A-only prompt', 1)] })

    openSession('sid-b')
    await settle()

    expect(useStore.getState().messages.map(m => m.text))
      .toEqual(['B-only prompt', 'B-only answer'])
  })

  it('CLEARS the transcript when the target session is genuinely empty', async () => {
    mockMessages([]) // 200 with no messages — a real, empty session
    useStore.setState({
      messages: [msg('a1', 'A-only prompt', 1)],
      pendingPermissions: [{ id: 'p1', toolName: 'Bash', input: {} }],
    })

    openSession('sid-empty')
    await settle()

    // Leaving A's messages up under B's sid is not merely stale text: loadHistory
    // is also what drops A's pending permission cards and turn cursor, so
    // skipping it leaves interactive residue from a session you are no longer in.
    expect(useStore.getState().messages).toEqual([])
    expect(useStore.getState().pendingPermissions).toEqual([])
  })

  it('does NOT blank an existing transcript when the fetch fails', async () => {
    mockMessages([], false) // e.g. a 500 from the daemon
    useStore.setState({ messages: [msg('a1', 'A-only prompt', 1)] })

    openSession('sid-b')
    await settle()

    // "Empty session" and "could not ask" must stay distinguishable: one bad
    // request should not erase history the user can still read.
    expect(useStore.getState().messages.map(m => m.text)).toEqual(['A-only prompt'])
  })

  it('ignores a response that arrives after the sid has moved on', async () => {
    // Two switches in flight; the FIRST one answers last.
    let release!: () => void
    const slow = new Promise<void>(r => { release = r })
    vi.stubGlobal('fetch', vi.fn(async (url: string) => {
      if (url.includes('sid-slow')) {
        await slow
        return { ok: true, status: 200, json: async () => [{ role: 'user', text: 'SLOW', ts: 1 }] }
      }
      return { ok: true, status: 200, json: async () => [{ role: 'user', text: 'FAST', ts: 2 }] }
    }))

    openSession('sid-slow')
    openSession('sid-fast')
    await settle()
    expect(useStore.getState().messages.map(m => m.text)).toEqual(['FAST'])

    release()
    await settle()

    // The stale answer must not overwrite the session the user actually landed on.
    expect(useStore.getState().sessionId).toBe('sid-fast')
    expect(useStore.getState().messages.map(m => m.text)).toEqual(['FAST'])
  })

  it('retires the notices of the session being left, SYNCHRONOUSLY (tether#57)', () => {
    mockMessages([{ role: 'user', text: 'hi', ts: 1 }])
    useStore.setState({ notices: [{ id: 'n1', text: 'context lost', ts: 5 }] })

    openSession('sid-b')

    // Asserted before the fetch settles on purpose. Deferring the clear into the
    // .then would wipe a notice that arrived DURING the switch (it belongs to
    // the session being opened) and would leave it standing whenever the request
    // fails — the shape of bug tether#57 exists to prevent.
    expect(useStore.getState().notices).toHaveLength(0)
  })

  it('retires the notices even when the target session has no history', async () => {
    mockMessages([])
    useStore.setState({ notices: [{ id: 'n1', text: 'context lost', ts: 5 }] })

    openSession('sid-empty')
    await settle()

    // The old workspace-list version cleared notices up front but then did
    // nothing else for an empty target — so this is the case where clearing had
    // no switch to belong to. It must now be a real switch.
    expect(useStore.getState().notices).toHaveLength(0)
    expect(useStore.getState().sessionId).toBe('sid-empty')
  })

  it('url-encodes the sid into the request path', async () => {
    const fetchMock = mockMessages([])

    // The daemon rejects sids outside [A-Za-z0-9_-] (server/session_api.go
    // validSID), so this is defence in depth — but a sid reaching us from
    // localStorage or a wi record is not something this module verified.
    openSession('a/b?c')
    await settle()

    expect(fetchMock).toHaveBeenCalledWith('/api/v1/sessions/a%2Fb%3Fc/messages')
  })

  it('is a no-op for an empty sid — no clear, no switch, no reconnect', async () => {
    const fetchMock = mockMessages([])
    const wt = watchReconnects()
    useStore.setState({ sessionId: 'sid-a', notices: [{ id: 'n1', text: 'context lost', ts: 5 }] })

    openSession('')
    await settle()

    expect(useStore.getState().sessionId).toBe('sid-a')
    expect(useStore.getState().notices).toHaveLength(1)
    expect(fetchMock).not.toHaveBeenCalled()
    expect(wt.count()).toBe(0)
  })

  it('is a no-op when that session is already open — no reconnect, no reload', async () => {
    const fetchMock = mockMessages([{ role: 'user', text: 'refetched', ts: 1 }])
    const wt = watchReconnects()
    useStore.setState({
      sessionId: 'sid-a',
      messages: [msg('live', 'mid-turn prompt', 1)],
      notices: [{ id: 'n1', text: 'context lost', ts: 5 }],
    })

    openSession('sid-a')
    await settle()

    // The session list highlights the current session, so this click is easy to
    // make. Honouring it would close a live WebTransport mid-turn and reload the
    // transcript over an in-flight turn's bubble — for a session already open.
    expect(wt.count()).toBe(0)
    expect(fetchMock).not.toHaveBeenCalled()
    expect(useStore.getState().messages.map(m => m.text)).toEqual(['mid-turn prompt'])
    expect(useStore.getState().notices).toHaveLength(1)
  })
})
