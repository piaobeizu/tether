import { describe, expect, it } from 'vitest'
import { buildStepEdges } from './WorkDetail'

describe('buildStepEdges', () => {
  it('uses prev-derived edges when present, ignoring the chain fallback', () => {
    const edges = buildStepEdges([{ id: 'a' }, { id: 'b', prev: ['a'] }])
    expect(edges).toEqual([{ from: 'a', to: 'b', kind: 'step' }])
  })

  it('synthesizes a record-order chain when no node has prev (degraded)', () => {
    const edges = buildStepEdges([{ id: 'a' }, { id: 'b' }, { id: 'c' }])
    expect(edges).toEqual([
      { from: 'a', to: 'b', kind: 'step' },
      { from: 'b', to: 'c', kind: 'step' },
    ])
  })

  it('returns no edges for fewer than two prev-less nodes', () => {
    expect(buildStepEdges([])).toEqual([])
    expect(buildStepEdges([{ id: 'only' }])).toEqual([])
  })

  it('does not synthesize a chain once any prev edge exists', () => {
    // 'b' has prev; 'c' does not. prevEdges is non-empty → short-circuit, so
    // 'c' is left unconnected rather than chained from 'b' (a real, if partial,
    // DAG is never overwritten by the degraded fallback).
    const edges = buildStepEdges([{ id: 'a' }, { id: 'b', prev: ['a'] }, { id: 'c' }])
    expect(edges).toEqual([{ from: 'a', to: 'b', kind: 'step' }])
  })
})

// ─── tether#91: the wi ↔ session binding, no longer in localStorage ──────────
//
// The action bar used to write `tether_wi_sid:<slug>` here and read it back on
// "Open in chat". These pin the replacement END TO END through the rendered
// component — the DOM click, the request it makes, and the effect it has — rather
// than spying on bindWorkItem, so a re-inlined localStorage write fails them
// whatever it is implemented with.
import { afterEach, beforeEach, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import WorkDetail from './WorkDetail'
import { useStore } from '../../lib/store'
import {
  SESSION_ACTIVITY_PATH,
  resetSessionActivityForTests,
  type SessionActivityMap,
} from '../../lib/sessionActivity'
import { resetArmedBinding, WI_BOUND_EVENT, type SessionSummary } from '../../lib/wiSession'

const WI_ID = 'wi_JAsOyS4F'
const SLUG = 'tether#91'

function item(status: string) {
  return {
    id: WI_ID, slug: SLUG, goal: 'a goal', status, priority: 'high',
    wiType: 'feature', labels: [], currentStepStatus: 'idle',
  }
}

type Call = [string, RequestInit?]

/** Route WorkDetail's fetches. `sessions` is a getter so a test can change the
 *  daemon's answer between renders. Unknown URLs throw. */
function mockWorkDaemon(
  status: string,
  sessions: () => SessionSummary[] = () => [],
  activity: () => SessionActivityMap = () => ({}),
) {
  const fn = vi.fn(async (url: string, init?: RequestInit) => {
    if (url === `/api/v1/work/items/${encodeURIComponent(WI_ID)}`) {
      return { ok: true, status: 200, json: async () => item(status) }
    }
    if (url === `/api/v1/work/items/${encodeURIComponent(WI_ID)}/steps`) {
      return { ok: true, status: 200, json: async () => ({ nodes: [], degraded: false }) }
    }
    if (url.startsWith(`/api/v1/work/items/${encodeURIComponent(WI_ID)}/events`)) {
      return { ok: true, status: 200, json: async () => ({ events: [] }) }
    }
    if (url === '/api/v1/sessions') {
      return { ok: true, status: 200, json: async () => sessions() }
    }
    // tether#103 — SessionRow now subscribes to the shared activity poller, so
    // rendering this pane polls this route. Answered rather than left to the
    // catch-all below, because this mock's realism is what makes it useful: a
    // rejected poll is swallowed by the poller's own error handling, so leaving it
    // out would mean the rows here silently never get a marker while the suite
    // stayed green.
    if (url === SESSION_ACTIVITY_PATH) {
      return { ok: true, status: 200, json: async () => activity() }
    }
    if (url.endsWith('/messages')) {
      return { ok: true, status: 200, json: async () => [] }
    }
    if (init?.method === 'PUT' && /^\/api\/v1\/sessions\/[^/]+\/wi$/.test(url)) {
      return { ok: true, status: 204, json: async () => ({}) }
    }
    throw new Error(`unexpected fetch: ${url}`)
  })
  vi.stubGlobal('fetch', fn)
  return fn
}

const putCalls = (fn: { mock: { calls: Call[] } }) =>
  fn.mock.calls.filter(([, init]) => init?.method === 'PUT')

/** How many times the session list has been ASKED FOR (tether#111).
 *
 * This is the timing-free half of the fix below. resumeWi re-asks only when
 * `sessionsLoaded` is false, and it does so SYNCHRONOUSLY inside the click handler,
 * before its first await — so "the click added no call here" is a direct observation
 * that the settled branch was taken, and it holds however slow the environment is.
 * The count, not a boolean: the pane asks once on mount and again on WI_BOUND_EVENT,
 * so the assertion has to be against the number already made. */
const sessionsCalls = (fn: { mock: { calls: Call[] } }) =>
  fn.mock.calls.filter(([url]) => url === '/api/v1/sessions')

let injected: string[] = []
let offInject: (() => void) | null = null
function watchPrompts() {
  injected = []
  const on = (e: Event) => injected.push(String((e as CustomEvent).detail))
  window.addEventListener('tether:inject-prompt', on)
  offInject = () => window.removeEventListener('tether:inject-prompt', on)
}

beforeEach(() => {
  localStorage.clear()
  resetArmedBinding()
  // tether#103 — the activity poller is module-level, so without this one file's
  // subscribers and timer would still be alive (and still counted) inside the next.
  resetSessionActivityForTests()
  useStore.setState({ sessionId: null, messages: [], notices: [] })
  watchPrompts()
})

afterEach(() => {
  cleanup()
  resetSessionActivityForTests()
  offInject?.(); offInject = null
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('WorkDetail records the binding on the daemon (tether#91)', () => {
  it('Start binds the live session over HTTP and writes no localStorage key', async () => {
    const fetchMock = mockWorkDaemon('queued')
    useStore.setState({ sessionId: 'sid-live' })
    render(<WorkDetail id={WI_ID} />)

    fireEvent.click(await screen.findByText('▶ Start'))

    await waitFor(() => expect(putCalls(fetchMock)).toHaveLength(1))
    expect(putCalls(fetchMock)[0][0]).toBe('/api/v1/sessions/sid-live/wi')
    expect(JSON.parse(String(putCalls(fetchMock)[0][1]?.body))).toEqual({ workItem: SLUG })

    // The key this replaces. Its absence is the point: it survived neither a
    // change of browser nor a cleared cache, and the daemon never saw it.
    expect(localStorage.getItem('tether_wi_sid:' + SLUG)).toBeNull()
    expect(Object.keys(localStorage).filter(k => k.startsWith('tether_wi_sid:'))).toEqual([])
    // Start still does its other two jobs.
    expect(injected).toEqual([`/pf-work ${SLUG}`])
  })

  it('Start with no session yet records nothing, then binds the session that appears', async () => {
    const fetchMock = mockWorkDaemon('queued')
    render(<WorkDetail id={WI_ID} />)

    fireEvent.click(await screen.findByText('▶ Start'))
    await waitFor(() => expect(injected).toHaveLength(1))

    // The old code wrote `sessionId ?? ''` right here, recording a mapping to
    // nothing that then made "Open in chat" fall through forever.
    expect(putCalls(fetchMock)).toHaveLength(0)

    useStore.setState({ sessionId: 'sid-fresh' })
    await waitFor(() => expect(putCalls(fetchMock)).toHaveLength(1))
    expect(putCalls(fetchMock)[0][0]).toBe('/api/v1/sessions/sid-fresh/wi')
  })
})

describe('WorkDetail resumes from the daemon-side binding (tether#91)', () => {
  it('Open in chat opens the NEWEST session bound to this wi', async () => {
    let reconnects = 0
    const onRetry = () => { reconnects++ }
    window.addEventListener('tether:retry-connection', onRetry)
    try {
      mockWorkDaemon('running', () => [
        { sid: 'sid-newest', workItem: SLUG, updatedAt: 300 },
        { sid: 'sid-older', workItem: SLUG, updatedAt: 100 },
      ])
      render(<WorkDetail id={WI_ID} />)

      fireEvent.click(await screen.findByText('→ Open in chat'))

      // Through openSession — the one shared implementation — so the WT channel
      // is rebound and the sid is persisted, not merely set in memory.
      await waitFor(() => expect(useStore.getState().sessionId).toBe('sid-newest'))
      expect(localStorage.getItem('tether_last_sid')).toBe('sid-newest')
      expect(reconnects).toBe(1)
      expect(injected).toEqual([])
    } finally {
      window.removeEventListener('tether:retry-connection', onRetry)
    }
  })

  // tether#111 — the test that used to stand here was a constructive flake, and the two
  // below are what it was trying to be.
  //
  // It gated on `await waitFor(() => expect(queryByText('Sessions')).toBeNull())`, with a
  // comment saying it was waiting for the list to settle "or 'no binding' would just be
  // 'the fetch had not answered yet'". That gate cannot tell those two apart: `Sessions`
  // renders only when `sessions.length > 0` (WorkDetail.tsx:209) and `sessions` starts as
  // `[]` and is reset to `[]` at the top of the effect (:60, :74), so the absence it waited
  // for is equally true BEFORE the answer arrives. It therefore passed on the first attempt
  // and proved nothing — and since the sessions effect is keyed on the SLUG, which only
  // exists once the item fetch has resolved, it usually passed while `sessionsLoaded` was
  // still false. The click then took resumeWi's `await fetchSessions()` branch (:144-146),
  // which dispatches one microtask after the click, and the SYNCHRONOUS assertion below it
  // read `[]`. Seen once on tether#110's branch, where ~13s of real sleeps changed how the
  // suite's promise chains interleaved; that wi later replaced the sleeps with a Date.now
  // spy, which is why the flake stopped reproducing without ever being fixed.
  //
  // The general judgement, worth more than this fix: an `expect(queryByText(X)).toBeNull()`
  // gate is almost never a proof of "it has answered", because it holds just as well before.
  // Read a gate's assertion aloud in both the before and the after state; if it is true in
  // both, it is not a gate.
  it('asks the daemon when the list has not answered yet, and still injects', async () => {
    // resumeWi's awaiting branch (WorkDetail.tsx:144-146), exercised deliberately. Its own
    // comment says why it exists: the binding used to be a synchronous localStorage read, so
    // a click a few milliseconds after the pane appeared could not miss it, and a fetch can.
    //
    // The window is HELD OPEN rather than raced for. Written first as "click as early as
    // possible and hope", which measured 1 session call instead of the 2 it asserted: on this
    // machine the list settles before `findByText` even resolves, so the click landed on the
    // settled branch and the test proved nothing about the other one. That is the same coin
    // flip the original flake was — it needed the whole suite's scheduling pressure to land
    // inside this window — so the fix is to make the window explicit and put the click in it
    // by construction, not by luck.
    const inner = mockWorkDaemon('running', () => [
      { sid: 'sid-other', workItem: 'tether#90', updatedAt: 300 },
    ])
    let release: () => void = () => {}
    const held = new Promise<void>((r) => { release = r })
    const outer = vi.fn(async (url: string, init?: RequestInit) => {
      if (url === '/api/v1/sessions') await held
      return inner(url, init)
    })
    vi.stubGlobal('fetch', outer)

    render(<WorkDetail id={WI_ID} />)
    fireEvent.click(await screen.findByText('→ Open in chat'))

    // SYNCHRONOUS, and the point of the test: the click is parked on its own re-ask, so
    // nothing has been injected yet. This is precisely the state the old test asserted its
    // way through — it read this empty array and called it a failure.
    expect(injected).toEqual([])

    release()
    await waitFor(() => expect(injected).toEqual([`/pf-work ${SLUG} --resume`]))
    expect(useStore.getState().sessionId).toBeNull()
    // Two asks: the pane's own on mount, and resumeWi's. This is what distinguishes this
    // test from the next one — without it, a resumeWi that had stopped awaiting would pass
    // both, since the two would then differ only in how they wait.
    expect(sessionsCalls(outer).length).toBeGreaterThan(1)
  })

  it('injects the resume prompt when the SETTLED list holds no binding', async () => {
    let rows: SessionSummary[] = [{ sid: 'sid-mine', workItem: SLUG, updatedAt: 300 }]
    const fetchMock = mockWorkDaemon('running', () => rows)
    render(<WorkDetail id={WI_ID} />)

    // Phase 1 — a binding that DOES match, so the answer's arrival is observable.
    // `Sessions` APPEARING is only reachable after the fetch settled, which is exactly what
    // its absence could not establish.
    await screen.findByText('Sessions')

    // Phase 2 — the daemon now reports nothing bound to this wi, and the refetch is driven
    // by the event a Start click emits (untested until now). `Sessions` DISAPPEARING is
    // reachable only from a settled response that produced an empty list, so past this line
    // the component is in the state this test is about: answered, and nothing bound.
    rows = [{ sid: 'sid-other', workItem: 'tether#90', updatedAt: 300 }]
    fireEvent(window, new Event(WI_BOUND_EVENT))
    await waitFor(() => expect(screen.queryByText('Sessions')).toBeNull())

    const asked = sessionsCalls(fetchMock).length
    fireEvent.click(screen.getByText('→ Open in chat'))

    // Timing-free proof that the settled branch was taken: resumeWi re-asks synchronously,
    // before its first await, so an unchanged count means it did not. Asserted BEFORE the
    // injection, because it is what makes the synchronous assertion sound rather than lucky.
    expect(sessionsCalls(fetchMock)).toHaveLength(asked)
    expect(injected).toEqual([`/pf-work ${SLUG} --resume`])
    expect(useStore.getState().sessionId).toBeNull()
  })
})

describe('WorkDetail lists every session of the wi (tether#91)', () => {
  // One-to-many is what inverting the mapping bought. The old forward key held
  // ONE sid per wi and the next Start overwrote it.
  it('shows all the wi bindings, newest first, and opens the one clicked', async () => {
    mockWorkDaemon('running', () => [
      { sid: 'sid-newest', workItem: SLUG, title: 'second attempt', updatedAt: 300 },
      { sid: 'sid-unrelated', workItem: 'tether#90', title: 'other work', updatedAt: 200 },
      { sid: 'sid-older', workItem: SLUG, title: 'first attempt', updatedAt: 100 },
    ])
    const { container } = render(<WorkDetail id={WI_ID} />)

    await waitFor(() => screen.getByText('Sessions'))
    const labels = [...container.querySelectorAll('.tree-row .tree-label')].map(n => n.textContent)
    expect(labels).toEqual(['second attempt', 'first attempt'])
    expect(screen.queryByText('other work')).toBeNull()

    fireEvent.click(screen.getByText('first attempt'))
    await waitFor(() => expect(useStore.getState().sessionId).toBe('sid-older'))
  })

  it('renders no Sessions section for a wi nobody has worked on', async () => {
    mockWorkDaemon('running', () => [])
    render(<WorkDetail id={WI_ID} />)
    await screen.findByText('→ Open in chat')
    expect(screen.queryByText('Sessions')).toBeNull()
  })

  // tether#103 — the marker reaches the rows THIS pane renders, not only the chat
  // list's. It is the same SessionRow, but "the same component" is exactly the
  // assumption this repo has been burned by: the first draft of that row was two
  // copies that had diverged three ways before review. One poller, and it is
  // subscribed from inside the row, so the claim to check here is that this pane
  // needed no code of its own to get the feature and did not get two timers for it.
  it('marks a bound session that is working, with ONE shared poller', async () => {
    const fetchMock = mockWorkDaemon(
      'running',
      () => [
        { sid: 'sid-newest', workItem: SLUG, title: 'second attempt', updatedAt: 300 },
        { sid: 'sid-older', workItem: SLUG, title: 'first attempt', updatedAt: 100 },
      ],
      () => ({ 'sid-newest': 'working', 'sid-older': 'held' }),
    )
    const { container } = render(<WorkDetail id={WI_ID} />)

    await waitFor(() => screen.getByText('Sessions'))
    const stated = () => container.querySelectorAll('.session-row-act.working, .session-row-act.idle, .session-row-act.held')
    await waitFor(() => expect(stated()).toHaveLength(2))
    const classes = [...stated()].map(n => n.className)
    expect(classes).toEqual(['session-row-act working', 'session-row-act held'])

    // Two rows, ONE request to the activity route.
    const activityCalls = fetchMock.mock.calls.filter(([url]) => url === SESSION_ACTIVITY_PATH)
    expect(activityCalls).toHaveLength(1)
  })
})

// The window the old code did not have. The binding used to be a synchronous
// localStorage read, so "Open in chat" could not be clicked before the answer
// existed. It is a fetch now, and an empty list that has not answered yet must
// not be read as "this wi has no session" — that failure mode is invisible,
// because falling back to the resume prompt is exactly what an unstarted wi does.
//
// The list fetch is held open here so the click lands strictly inside the window;
// without the fix this test fails deterministically rather than by timing luck.
describe('WorkDetail resume before the session list has answered (tether#91)', () => {
  it('waits for the binding instead of falling through to the resume prompt', async () => {
    let releaseList: (() => void) | null = null
    const listHeld = new Promise<void>((r) => { releaseList = r })

    vi.stubGlobal('fetch', vi.fn(async (url: string) => {
      if (url === `/api/v1/work/items/${encodeURIComponent(WI_ID)}`) {
        return { ok: true, status: 200, json: async () => item('running') }
      }
      if (url === `/api/v1/work/items/${encodeURIComponent(WI_ID)}/steps`) {
        return { ok: true, status: 200, json: async () => ({ nodes: [], degraded: false }) }
      }
      if (url.startsWith(`/api/v1/work/items/${encodeURIComponent(WI_ID)}/events`)) {
        return { ok: true, status: 200, json: async () => ({ events: [] }) }
      }
      if (url === '/api/v1/sessions') {
        await listHeld
        return { ok: true, status: 200, json: async () => [{ sid: 'sid-bound', workItem: SLUG, updatedAt: 300 }] }
      }
      // tether#103 — see mockDaemon above.
      if (url === SESSION_ACTIVITY_PATH) return { ok: true, status: 200, json: async () => ({}) }
      if (url.endsWith('/messages')) return { ok: true, status: 200, json: async () => [] }
      throw new Error(`unexpected fetch: ${url}`)
    }))

    render(<WorkDetail id={WI_ID} />)
    const btn = await screen.findByText('→ Open in chat')

    // Click while the list is still in flight.
    fireEvent.click(btn)
    expect(injected).toEqual([])

    releaseList!()

    await waitFor(() => expect(useStore.getState().sessionId).toBe('sid-bound'))
    // And it did NOT also inject the prompt on the way.
    expect(injected).toEqual([])
  })
})

// The wi detail lives in the MIDDLE column; chat is a tab in the right one. A row
// click that switches the live session without bringing chat forward tears down
// the WebTransport and repoints the next prompt while the user is looking at
// Skills or Shell, with nothing visible happening. The first draft of this pane
// had exactly that: it called openSession directly and, unlike the action-bar
// button 79 lines above it, dispatched no tether:select-tab.
describe('WorkDetail session rows bring chat forward (tether#91)', () => {
  it('selects the chat tab and marks the session you are already in', async () => {
    const tabs: string[] = []
    const onTab = (e: Event) => tabs.push(String((e as CustomEvent).detail))
    window.addEventListener('tether:select-tab', onTab)
    try {
      mockWorkDaemon('running', () => [
        { sid: 'sid-newest', workItem: SLUG, title: 'second attempt', updatedAt: 300 },
        { sid: 'sid-older', workItem: SLUG, title: 'first attempt', updatedAt: 100 },
      ])
      useStore.setState({ sessionId: 'sid-newest' })
      const { container } = render(<WorkDetail id={WI_ID} />)
      await waitFor(() => screen.getByText('Sessions'))

      // Rendered by the shared row, so the current session is marked here too.
      const active = container.querySelectorAll('.tree-row.active')
      expect(active).toHaveLength(1)
      expect(active[0].textContent).toContain('second attempt')

      fireEvent.click(screen.getByText('first attempt'))

      expect(tabs).toEqual(['chat'])
      await waitFor(() => expect(useStore.getState().sessionId).toBe('sid-older'))
    } finally {
      window.removeEventListener('tether:select-tab', onTab)
    }
  })
})
