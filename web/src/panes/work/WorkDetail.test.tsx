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
import { resetArmedBinding, type SessionSummary } from '../../lib/wiSession'

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
function mockWorkDaemon(status: string, sessions: () => SessionSummary[] = () => []) {
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
  useStore.setState({ sessionId: null, messages: [], notices: [] })
  watchPrompts()
})

afterEach(() => {
  cleanup()
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

  it('falls back to the resume prompt when no session is bound', async () => {
    mockWorkDaemon('running', () => [{ sid: 'sid-other', workItem: 'tether#90', updatedAt: 300 }])
    render(<WorkDetail id={WI_ID} />)
    // Wait for the session list to have settled, or "no binding" would just be
    // "the fetch had not answered yet".
    await screen.findByText('→ Open in chat')
    await waitFor(() => expect(screen.queryByText('Sessions')).toBeNull())

    fireEvent.click(screen.getByText('→ Open in chat'))

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
