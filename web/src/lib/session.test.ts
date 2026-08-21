// tether#61 — openSession is the single "open a different session" operation.
// Before it existed, ChatPane and the WorkspacePane session list each had their
// own version and they had drifted: the workspace one never reconnected the
// WebTransport channel, and it hid setSessionId (which is what persists
// tether_last_sid) behind a non-empty history. Each test below pins one half of
// that divergence, or one of the hazards centralising it made cheap to close.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { loadEarlierTranscript, openSession, refreshTranscript, REFRESH_TRANSCRIPT_EVENT } from './session'
import { useStore, type Message } from './store'
import {
  TRANSCRIPT_UPDATED_AT_HEADER,
  resetTranscriptWatchForTests,
  transcriptWatchState,
} from './transcriptWatch'

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

/** Record every 'tether:refresh-transcript' — the channel openSession offers a click
 *  on the ALREADY-OPEN row on (tether#106). ChatPane decides whether it means
 *  anything; this module only offers it. */
function watchRefreshOffers(): { count: () => number } {
  let seen = 0
  const onRefresh = () => { seen++ }
  window.addEventListener(REFRESH_TRANSCRIPT_EVENT, onRefresh)
  listeners.push(() => window.removeEventListener(REFRESH_TRANSCRIPT_EVENT, onRefresh))
  return { count: () => seen }
}

beforeEach(() => {
  localStorage.clear()
  resetTranscriptWatchForTests()
  useStore.setState({ sessionId: null, messages: [], notices: [], pendingPermissions: [] })
})

afterEach(() => {
  for (const off of listeners) off()
  listeners = []
  resetTranscriptWatchForTests()
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
      sessionId: 'sid-a',
      messages: [msg('a1', 'A-only prompt', 1)],
      // Tagged with the session being LEFT, which is what makes this a test of the
      // rule and not of a fixture's omission. Since tether#132 the reducer drops a
      // request because its sid is not the arriving one; an UNTAGGED request — what
      // this fixture used to hold — is dropped for a weaker reason, that it matches
      // no session at all, and THAT outcome survives deleting the sid discriminator
      // entirely. store.test.ts pins the untagged case deliberately and on its own.
      pendingPermissions: [{ id: 'p1', toolName: 'Bash', input: {}, sessionId: 'sid-a' }],
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

    // The trailing `undefined` is authedFetch forwarding an absent init (tether#106
    // routed this through it so an expired cookie redirects instead of looping); the
    // url is what this test is about.
    expect(fetchMock).toHaveBeenCalledWith('/api/v1/sessions/a%2Fb%3Fc/messages', undefined)
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
    //
    // tether#106 narrowed what this click MEANS, and this test is the ratchet on
    // what it must still not DO. Every one of these four is a separate way for the
    // narrowing to have gone too far: a reconnect tears down the live channel
    // (tether#61), a fetch here would be an unconditional reload of the transcript
    // this pane may be streaming into (tether#42), and clearing the notices is the
    // tether#57 defect exactly.
    expect(wt.count()).toBe(0)
    expect(fetchMock).not.toHaveBeenCalled()
    expect(useStore.getState().messages.map(m => m.text)).toEqual(['mid-turn prompt'])
    expect(useStore.getState().notices).toHaveLength(1)
  })

  it('OFFERS the already-open click to ChatPane, and offering is all it does', async () => {
    // tether#106 — the click on the highlighted row is not nothing: for a session a
    // background agent holds there is no live stream to protect and the transcript
    // below is a still frame. openSession cannot tell those apart (whether a stream
    // exists is ChatPane's fact), so it raises the event and stops. Everything the
    // test above pins stays pinned; this one pins that the offer is made at all,
    // because deleting it is silent — no test fails, the click just goes back to
    // meaning nothing.
    const fetchMock = mockMessages([{ role: 'user', text: 'refetched', ts: 1 }])
    const offers = watchRefreshOffers()
    const wt = watchReconnects()
    useStore.setState({ sessionId: 'sid-a', messages: [msg('live', 'mid-turn prompt', 1)] })

    openSession('sid-a')
    await settle()

    expect(offers.count()).toBe(1)
    expect(wt.count()).toBe(0)
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('does NOT offer a refresh when the click is a real switch', async () => {
    // A switch already reloads the transcript on its own. An offer here would make
    // ChatPane reload it a third time (openSession + the [sessionId] effect + this).
    mockMessages([])
    const offers = watchRefreshOffers()
    useStore.setState({ sessionId: 'sid-a' })

    openSession('sid-b')
    await settle()

    expect(offers.count()).toBe(0)
  })
})

describe('refreshTranscript (tether#106)', () => {
  /** A /messages reply that carries the version header, the way the daemon sends it. */
  function mockVersionedMessages(entries: unknown[], version: number | null) {
    const headers = new Headers()
    if (version !== null) headers.set(TRANSCRIPT_UPDATED_AT_HEADER, String(version))
    const fetchMock = vi.fn(async () => ({ ok: true, status: 200, headers, json: async () => entries }))
    vi.stubGlobal('fetch', fetchMock)
    return fetchMock
  }

  it('reloads the transcript and records which version it loaded', async () => {
    // The recording is the half that is easy to drop and impossible to see: without
    // it the first probe has no baseline, so everything written between the load and
    // that probe stays invisible until the write AFTER it — on a conversation whose
    // next write may be minutes away, a transcript that stops at a message the reader
    // can see is not the last one.
    mockVersionedMessages([{ role: 'user', text: 'newly appended', ts: 9 }], 1755500000000)
    useStore.setState({ sessionId: 'sid-a', messages: [msg('old', 'stale', 1)] })

    await refreshTranscript('sid-a')

    expect(useStore.getState().messages.map(m => m.text)).toEqual(['newly appended'])
    expect(transcriptWatchState().version).toBe(1755500000000)
  })

  it('drops a reply that arrives after the sid has moved on', async () => {
    mockVersionedMessages([{ role: 'user', text: 'from sid-a', ts: 1 }], 100)
    useStore.setState({ sessionId: 'sid-a', messages: [] })

    const inFlight = refreshTranscript('sid-a')
    useStore.setState({ sessionId: 'sid-b', messages: [msg('b1', 'B transcript', 2)] })
    await inFlight

    expect(useStore.getState().messages.map(m => m.text)).toEqual(['B transcript'])
    // …and the version must not be recorded either: it describes sid-a's file, and
    // recording it here would make the watch believe sid-b is up to date.
    expect(transcriptWatchState().version).toBe(0)
  })

  it('does not blank the transcript when the request fails', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => ({ ok: false, status: 500, headers: new Headers(), json: async () => [] })))
    useStore.setState({ sessionId: 'sid-a', messages: [msg('a1', 'still readable', 1)] })

    await refreshTranscript('sid-a')

    expect(useStore.getState().messages.map(m => m.text)).toEqual(['still readable'])
  })

  it('is a no-op for an empty sid', async () => {
    const fetchMock = mockVersionedMessages([], 100)
    await refreshTranscript('')
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('KEEPS a permission request raised in the session it is reloading (tether#132)', async () => {
    // The counterpart of openSession's "genuinely empty" test above, and the half no
    // switch can pin. Every caller of THIS function is a refetch of the session
    // already on screen — the click on the already-open row, the held-session
    // watcher's three-second reload, "Check again" — and none of them is a switch,
    // yet loadHistory used to discard the whole permission queue. So a live card
    // could be dismissed by a misclick with nothing anywhere able to raise it again:
    // `pendingPermissions` is filled from one broadcast envelope and nothing else,
    // and before tether#132 there was no backfill to re-send it.
    //
    // Asserted HERE rather than only on the reducer (store.test.ts covers that)
    // because this is the hop that supplies the reducer's discriminator: the sid
    // re-check above is what makes `s.sessionId` the session being installed, so a
    // load that landed without it would drop the card while every store-level test
    // stayed green.
    mockVersionedMessages([{ role: 'user', text: 'newly appended', ts: 9 }], 100)
    useStore.setState({
      sessionId: 'sid-a',
      messages: [],
      pendingPermissions: [{ id: 'p1', toolName: 'Bash', input: {}, sessionId: 'sid-a' }],
    })

    await refreshTranscript('sid-a')

    // The transcript IS replaced — this is still the server-truth load, and asserting
    // it here keeps the test from passing by way of loadHistory not having run.
    expect(useStore.getState().messages.map(m => m.text)).toEqual(['newly appended'])
    expect(useStore.getState().pendingPermissions.map(p => p.id)).toEqual(['p1'])
  })

  it('joins a load already in flight for the same session instead of racing it', async () => {
    // Three callers now share one sid — the watcher's reload, the click on the open row
    // and "Check again" — where before tether#106 the only caller was a switch and two
    // in-flight loads were for different sids by construction. Two overlapping loads can
    // settle in either order, so the older one could land last and take
    // noteTranscriptVersion with it, leaving the recorded version describing neither
    // what is on screen nor what the daemon has.
    let release!: () => void
    const gate = new Promise<void>(r => { release = r })
    const headers = new Headers()
    headers.set(TRANSCRIPT_UPDATED_AT_HEADER, '500')
    const fetchMock = vi.fn(async () => {
      await gate
      return { ok: true, status: 200, headers, json: async () => [{ role: 'user', text: 'once', ts: 1 }] }
    })
    vi.stubGlobal('fetch', fetchMock)
    useStore.setState({ sessionId: 'sid-a', messages: [] })

    const a = refreshTranscript('sid-a')
    const b = refreshTranscript('sid-a')
    expect(fetchMock).toHaveBeenCalledTimes(1)

    release()
    await Promise.all([a, b])
    expect(useStore.getState().messages.map(m => m.text)).toEqual(['once'])
    expect(transcriptWatchState().version).toBe(500)

    // And the slot is released, so the NEXT click really does re-read.
    await refreshTranscript('sid-a')
    expect(fetchMock).toHaveBeenCalledTimes(2)
  })
})

// ────────────────────────────────────────────────────────────────────────────
// tether#107 — reading BACKWARDS, and what the three-second refresh is allowed to
// do to pages the reader loaded on purpose.
//
// The second half is the one nothing else can catch. tether#106's probe reloads a
// held session's transcript whenever the other agent writes, through `loadHistory`,
// which REPLACES the array. Left alone, that discards every earlier page — every
// three seconds, while the reader is reading them — and the suite would stay green,
// because no existing test ever has two pages loaded.
describe('paging backwards (tether#107)', () => {
  /** A /messages stub that answers with real Headers, so the boundary facts travel. */
  function mockPages(pages: Record<string, { entries: unknown[]; earlier?: number; otherRecord?: string }>) {
    const calls: string[] = []
    const fetchMock = vi.fn(async (url: string) => {
      calls.push(url)
      const page = pages[url]
      if (!page) throw new Error(`no stub for ${url}`)
      const headers = new Headers()
      if (page.earlier !== undefined) headers.set('X-Tether-Transcript-Earlier', String(page.earlier))
      if (page.otherRecord !== undefined) headers.set('X-Tether-Transcript-Other-Record', page.otherRecord)
      return { ok: true, status: 200, headers, json: async () => page.entries }
    })
    vi.stubGlobal('fetch', fetchMock)
    return { calls }
  }

  // `ord` defaults to `ts` so the tether#107 cases below read as they were written. That
  // default is a convenience of this fixture and NOT a fact about the wire — the daemon's
  // ord is a byte position in a file and its ts is a clock. tether#109's cases pass the
  // two separately, because a fixture where they move together cannot express the bug:
  // the whole mechanism is a ts that moves while the conversation does not.
  const entry = (role: string, text: string, ts: number, ord: number = ts) => ({ role, text, ts, ord })
  const URL_NEWEST = '/api/v1/sessions/sid-paged-0001/messages'

  afterEach(() => {
    useStore.setState({
      messages: [], sessionId: null,
      transcriptEarlier: null, transcriptOtherRecord: null, transcriptPagesBack: 0,
    })
  })

  it('records the cursor and the other-record store off the response that loaded the page', async () => {
    mockPages({ [URL_NEWEST]: { entries: [entry('user', 'newest', 900)], earlier: 4096, otherRecord: 'cc' } })
    useStore.setState({ sessionId: 'sid-paged-0001' })

    await refreshTranscript('sid-paged-0001')

    expect(useStore.getState().transcriptEarlier).toBe(4096)
    expect(useStore.getState().transcriptOtherRecord).toBe('cc')
  })

  it('fetches the page BEFORE the oldest one and prepends it', async () => {
    const { calls } = mockPages({
      [URL_NEWEST]: { entries: [entry('user', 'newest', 900)], earlier: 4096 },
      [`${URL_NEWEST}?before=4096`]: { entries: [entry('user', 'older', 100)], earlier: 2048 },
    })
    useStore.setState({ sessionId: 'sid-paged-0001' })
    await refreshTranscript('sid-paged-0001')

    await loadEarlierTranscript('sid-paged-0001')

    expect(calls).toEqual([URL_NEWEST, `${URL_NEWEST}?before=4096`])
    expect(useStore.getState().messages.map(m => m.text)).toEqual(['older', 'newest'])
    // The cursor MOVED BACK, so the next click goes one page further rather than
    // re-fetching the page just loaded. Exact value, not "changed": a cursor that
    // stayed at 4096 would loop on one page forever while looking like it worked.
    expect(useStore.getState().transcriptEarlier).toBe(2048)
    expect(useStore.getState().transcriptPagesBack).toBe(1)
  })

  it('stops offering earlier pages when the daemon omits the cursor', async () => {
    mockPages({
      [URL_NEWEST]: { entries: [entry('user', 'newest', 900)], earlier: 4096 },
      [`${URL_NEWEST}?before=4096`]: { entries: [entry('user', 'the very first', 100)] },
    })
    useStore.setState({ sessionId: 'sid-paged-0001' })
    await refreshTranscript('sid-paged-0001')
    await loadEarlierTranscript('sid-paged-0001')

    // null, which is what the pane renders as "the beginning of this conversation".
    expect(useStore.getState().transcriptEarlier).toBeNull()
    // …and a further call is a no-op rather than a request with no cursor in it.
    const before = useStore.getState().messages.length
    await loadEarlierTranscript('sid-paged-0001')
    expect(useStore.getState().messages).toHaveLength(before)
  })

  it('spends one cursor once however fast the reader clicks', async () => {
    // The button disables while a load is in flight, but the disable is a render away
    // and the cursor only advances when a response lands. Two synchronous clicks would
    // otherwise both spend 4096 — a second megabyte fetched to be discarded.
    mockPages({
      [URL_NEWEST]: { entries: [entry('user', 'newest', 900)], earlier: 4096 },
      [`${URL_NEWEST}?before=4096`]: { entries: [entry('user', 'older', 100)], earlier: 2048 },
    })
    useStore.setState({ sessionId: 'sid-paged-0001' })
    await refreshTranscript('sid-paged-0001')

    const { calls } = mockPages({
      [`${URL_NEWEST}?before=4096`]: { entries: [entry('user', 'older', 100)], earlier: 2048 },
    })
    await Promise.all([
      loadEarlierTranscript('sid-paged-0001'),
      loadEarlierTranscript('sid-paged-0001'),
    ])
    expect(calls).toEqual([`${URL_NEWEST}?before=4096`])
    expect(useStore.getState().transcriptPagesBack).toBe(1)
  })

  it('does not prepend a page for a session the reader has already left', async () => {
    mockPages({
      [URL_NEWEST]: { entries: [entry('user', 'newest', 900)], earlier: 4096 },
      [`${URL_NEWEST}?before=4096`]: { entries: [entry('user', 'older', 100)] },
    })
    useStore.setState({ sessionId: 'sid-paged-0001' })
    await refreshTranscript('sid-paged-0001')

    const p = loadEarlierTranscript('sid-paged-0001')
    useStore.setState({ sessionId: 'sid-somewhere-else', messages: [] })
    await p

    expect(useStore.getState().messages).toHaveLength(0)
  })

  // ── The refresh, with pages loaded. THE property this wi had to keep. ──────
  it('keeps the loaded pages when the transcript is refreshed, and appends what is new', async () => {
    mockPages({
      [URL_NEWEST]: { entries: [entry('user', 'ask', 500), entry('assistant', 'partial', 600)], earlier: 4096 },
      [`${URL_NEWEST}?before=4096`]: { entries: [entry('user', 'page one', 100)], earlier: 2048 },
    })
    useStore.setState({ sessionId: 'sid-paged-0001' })
    await refreshTranscript('sid-paged-0001')
    await loadEarlierTranscript('sid-paged-0001')
    expect(useStore.getState().messages.map(m => m.text)).toEqual(['page one', 'ask', 'partial'])
    const idsBefore = useStore.getState().messages.map(m => m.id)

    // The other agent writes: the newest window now covers the same tail plus one new
    // turn, and the growing bubble has more text in it.
    mockPages({
      [URL_NEWEST]: {
        entries: [entry('user', 'ask', 500), entry('assistant', 'partial answer, complete', 600), entry('user', 'and more', 700)],
        earlier: 5120,
      },
    })
    await refreshTranscript('sid-paged-0001')

    const after = useStore.getState().messages
    // 1. the page the reader loaded is still there;
    // 2. the growing turn's text was updated;
    // 3. the new turn is at the end.
    expect(after.map(m => m.text)).toEqual(['page one', 'ask', 'partial answer, complete', 'and more'])
    // 4. and the ids of everything that was already on screen are byte-identical, so
    //    React reconciles rather than remounts and the reader keeps their expansions
    //    and their scroll position.
    expect(after.slice(0, 3).map(m => m.id)).toEqual(idsBefore)
    // 5. the cursor still describes the OLDEST page on screen, not the newest window.
    //    Taking the refresh's 5120 would send the next "load earlier" forward, to
    //    re-serve pages the reader is already looking at.
    expect(useStore.getState().transcriptEarlier).toBe(2048)
    expect(useStore.getState().transcriptPagesBack).toBe(1)
  })

  it('falls back to a visible replace when the refreshed window does not overlap', async () => {
    mockPages({
      [URL_NEWEST]: { entries: [entry('user', 'ask', 500)], earlier: 4096 },
      [`${URL_NEWEST}?before=4096`]: { entries: [entry('user', 'page one', 100)], earlier: 2048 },
    })
    useStore.setState({ sessionId: 'sid-paged-0001' })
    await refreshTranscript('sid-paged-0001')
    await loadEarlierTranscript('sid-paged-0001')
    expect(useStore.getState().messages).toHaveLength(2)

    // Over a megabyte written between two probes: the window has slid past everything
    // on screen. A merge here would splice two disjoint stretches together with an
    // invisible hole; the replace is a jump the reader can SEE.
    mockPages({ [URL_NEWEST]: { entries: [entry('user', 'much later', 90000)], earlier: 9999 } })
    await refreshTranscript('sid-paged-0001')

    expect(useStore.getState().messages.map(m => m.text)).toEqual(['much later'])
    expect(useStore.getState().transcriptPagesBack).toBe(0)
    // …and the cursor is now the NEW window's, because the array is now one page.
    expect(useStore.getState().transcriptEarlier).toBe(9999)
  })

  it('still replaces wholesale when no earlier page has been loaded', async () => {
    // The unchanged path, and most sessions are on it. A refresh with pagesBack 0 must
    // behave byte-for-byte as it did before tether#107: the server-truth replace.
    mockPages({ [URL_NEWEST]: { entries: [entry('user', 'first fetch', 100)], earlier: 4096 } })
    useStore.setState({ sessionId: 'sid-paged-0001' })
    await refreshTranscript('sid-paged-0001')

    mockPages({ [URL_NEWEST]: { entries: [entry('user', 'a different conversation', 700)] } })
    await refreshTranscript('sid-paged-0001')

    expect(useStore.getState().messages.map(m => m.text)).toEqual(['a different conversation'])
    expect(useStore.getState().transcriptEarlier).toBeNull()
  })

  // ── tether#109: the refresh path over a window that re-cut its leading bubble ──
  //
  // The reported defect, driven through the real loader rather than through the reducer:
  // two pages on screen, then a probe whose newest window opens one record further into
  // the assistant turn at its leading edge. cc stamps a merged turn with its FIRST
  // fragment's time, so that turn arrives with a LATER ts and a key nothing on screen has
  // — and tether#107 appended it, under a bubble three and a half hours newer.
  //
  // The ords here are byte positions a megabyte apart and the timestamps are the measured
  // ones, so the fixture has the shape the real data has: ts and ord move independently.
  it('keeps the reader\'s pages when the newest window re-cuts its leading bubble', async () => {
    const TS_EDGE_FULL = Date.parse('2026-08-19T03:41:45.555Z')
    const TS_EDGE_RECUT = Date.parse('2026-08-19T03:41:49.404Z')
    const TS_NEWEST = Date.parse('2026-08-19T07:18:29.315Z')

    mockPages({
      [URL_NEWEST]: {
        entries: [
          entry('assistant', 'the whole turn, first fragment onwards', TS_EDGE_FULL, 122154092),
          entry('assistant', 'the newest turn', TS_NEWEST, 123197925),
        ],
        earlier: 122154091,
      },
      [`${URL_NEWEST}?before=122154091`]: {
        entries: [entry('user', 'a page the reader asked for', TS_EDGE_FULL - 3600000, 121000000)],
        earlier: 120000000,
      },
    })
    useStore.setState({ sessionId: 'sid-paged-0001' })
    await refreshTranscript('sid-paged-0001')
    await loadEarlierTranscript('sid-paged-0001')
    const idsBefore = useStore.getState().messages.map(m => m.id)
    expect(idsBefore).toHaveLength(3)

    // The file grew by one record, so the window slid forward and dropped the leading
    // fragment of the turn at its edge.
    mockPages({
      [URL_NEWEST]: {
        entries: [
          entry('assistant', 'minus its first fragment', TS_EDGE_RECUT, 122155976),
          entry('assistant', 'the newest turn', TS_NEWEST, 123197925),
          entry('user', 'what the other agent just wrote', TS_NEWEST + 60000, 123205000),
        ],
        earlier: 122155975,
      },
    })
    await refreshTranscript('sid-paged-0001')

    const after = useStore.getState().messages
    // The re-cut bubble is NOT at the end. Its words are still on screen — they are a
    // suffix of the bubble above, which the daemon-side test pins — and the new turn is
    // where a new turn goes.
    expect(after.map(m => m.text)).toEqual([
      'a page the reader asked for',
      'the whole turn, first fragment onwards',
      'the newest turn',
      'what the other agent just wrote',
    ])
    // Identity survived, so React reconciles rather than remounting: the reader keeps
    // their expansions and their scroll position.
    expect(after.slice(0, 3).map(m => m.id)).toEqual(idsBefore)
    // And the merge SUCCEEDED — the reader's page is still counted and the cursor still
    // describes the oldest page on screen rather than the newest window's.
    expect(useStore.getState().transcriptPagesBack).toBe(1)
    expect(useStore.getState().transcriptEarlier).toBe(120000000)
  })

  it('falls back to a visible replace when the refreshed window starts before everything on screen', async () => {
    // widen-once: ccReadTail retries a 1 MiB window holding no conversation with a 16 MiB
    // one. Measured on the reported transcript it fires on 9 of 1,053 sampled sizes and
    // starts the page 15.6 MiB earlier — so this is real, even though it is not the
    // mechanism in the screenshot. Content below everything on screen cannot be folded
    // in: there is no guarantee the two ranges meet.
    mockPages({
      [URL_NEWEST]: {
        entries: [entry('user', 'ask', 500, 122000000), entry('assistant', 'answer', 600, 122500000)],
        earlier: 121999999,
      },
      [`${URL_NEWEST}?before=121999999`]: {
        entries: [entry('user', 'a page the reader asked for', 100, 121000000)],
        earlier: 120000000,
      },
    })
    useStore.setState({ sessionId: 'sid-paged-0001' })
    await refreshTranscript('sid-paged-0001')
    await loadEarlierTranscript('sid-paged-0001')
    expect(useStore.getState().messages).toHaveLength(3)

    mockPages({
      [URL_NEWEST]: {
        entries: [
          entry('user', '15.6 MiB earlier', 50, 105904904),
          entry('user', 'ask', 500, 122000000),
          entry('assistant', 'answer', 600, 122500000),
        ],
        earlier: 105904903,
      },
    })
    await refreshTranscript('sid-paged-0001')

    // The visible reset: the array is the newest page, in the daemon's order, and the
    // page counter and the cursor both describe that one page.
    expect(useStore.getState().messages.map(m => m.text)).toEqual(['15.6 MiB earlier', 'ask', 'answer'])
    expect(useStore.getState().transcriptPagesBack).toBe(0)
    expect(useStore.getState().transcriptEarlier).toBe(105904903)
  })

  it('replaces rather than merging when the reader switches session while paged back', async () => {
    // The cross-session hole, end to end. Found by review, and it is arithmetic rather
    // than bad luck: `ord` is 1-based and both daemon stores number from the start of
    // their own record, so `ord === 1` appears in every page that reaches byte 0 — every
    // page at all, for tether's own store. One matching position is all a merge needs to
    // report success, and the rest of the arriving page then lands inside the previous
    // session's span. Session A's transcript would render under session B, with A's byte
    // cursor still armed against B's file.
    //
    // Driven through `openSession` because that is the real path: it calls setSessionId
    // (which now retires the page count) and then refreshTranscript, and the ORDER of
    // those two is what makes the refresh take the replace.
    const URL_B = '/api/v1/sessions/sid-paged-0002/messages'
    mockPages({
      [URL_NEWEST]: { entries: [entry('user', "A's own turn", 500, 900)], earlier: 4096 },
      [`${URL_NEWEST}?before=4096`]: { entries: [entry('user', "A's first turn ever", 100, 1)], earlier: undefined },
      [URL_B]: { entries: [entry('user', "B's first turn ever", 700, 1)] },
    })
    useStore.setState({ sessionId: 'sid-paged-0001' })
    await refreshTranscript('sid-paged-0001')
    await loadEarlierTranscript('sid-paged-0001')
    // The precondition, asserted: A really is paged back, and it really does hold ord 1 —
    // the position B's page will also carry. Without both, this test proves nothing.
    expect(useStore.getState().transcriptPagesBack).toBe(1)
    expect(useStore.getState().messages.map(m => m.ord)).toEqual([1, 900])

    openSession('sid-paged-0002')
    await new Promise(resolve => setTimeout(resolve, 0))
    await refreshTranscript('sid-paged-0002')

    // B's transcript, and only B's. A merge would have left "A's own turn" on screen
    // (interior, assistant… or refused; either way the array would not be exactly B's).
    expect(useStore.getState().messages.map(m => m.text)).toEqual(["B's first turn ever"])
    expect(useStore.getState().transcriptPagesBack).toBe(0)
    expect(useStore.getState().transcriptEarlier).toBeNull()
  })
})
