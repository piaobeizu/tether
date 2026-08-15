// tether#91 — the session list, after moving out of the workspace pane.
//
// The first describe below is the tether#61 coverage, moved with the component it
// guards: the list must open a session the SAME way every other call site does.
// It used to inline its own version that never reconnected the WebTransport
// channel and that hid setSessionId (hence tether_last_sid) behind a non-empty
// history, so switching from here left the live stream — and the next prompt
// sent — on the session the user had just left.
//
// These drive the real DOM click rather than spying on openSession, so they fail
// if the list ever grows its own switch again, whatever it is implemented with.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import SessionList from './SessionList'
import { useStore } from '../../lib/store'
import { putWiBinding, resetArmedBinding, resetMigrationForTests, type SessionSummary } from '../../lib/wiSession'

const SID_WITH_HISTORY = 'sid-has-history-1'
const SID_EMPTY = 'sid-no-history-2'

const HOUR_AGO = Date.now() - 3_600_000

const DEFAULT_ROWS: SessionSummary[] = [
  { sid: SID_WITH_HISTORY, title: 'B-only prompt', updatedAt: HOUR_AGO + 1000 },
  { sid: SID_EMPTY, title: 'nothing said', updatedAt: HOUR_AGO },
]

/** Route the component's fetches by URL. Anything else throws rather than
 *  quietly succeeding, so the mock's realism is self-enforcing. Paths match
 *  mux.go. `rows` is a getter so a test can change the answer mid-run. */
function mockDaemon(rows: () => SessionSummary[] = () => DEFAULT_ROWS) {
  const fn = vi.fn(async (url: string, init?: RequestInit) => {
    if (url === '/api/v1/sessions') {
      return { ok: true, status: 200, json: async () => rows() }
    }
    if (url === `/api/v1/sessions/${SID_WITH_HISTORY}/messages`) {
      return { ok: true, status: 200, json: async () => [{ role: 'user', text: 'B-only prompt', ts: 10 }] }
    }
    if (url === `/api/v1/sessions/${SID_EMPTY}/messages`) {
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

let offRetry: (() => void) | null = null
function watchReconnects(): () => number {
  let n = 0
  const onRetry = () => { n++ }
  window.addEventListener('tether:retry-connection', onRetry)
  offRetry = () => window.removeEventListener('tether:retry-connection', onRetry)
  return () => n
}

/** Expand the collapsed "Sessions" section and click one row by its label. */
async function clickSession(label: string) {
  await waitFor(() => screen.getByText('Sessions'))
  fireEvent.click(screen.getByText('Sessions'))
  const row = await waitFor(() => screen.getByText(label))
  fireEvent.click(row)
}

beforeEach(() => {
  localStorage.clear()
  resetArmedBinding()
  resetMigrationForTests()
  useStore.setState({
    sessionId: 'sid-previous',
    messages: [],
    notices: [{ id: 'n1', text: 'context lost', ts: 5 }],
  })
})

afterEach(() => {
  cleanup()
  offRetry?.(); offRetry = null
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('SessionList opens a session through the one shared operation (tether#61)', () => {
  it('reconnects the WT channel and persists the sid when switching', async () => {
    mockDaemon()
    const reconnects = watchReconnects()
    render(<SessionList />)

    await clickSession('B-only prompt')

    expect(reconnects()).toBe(1)
    await waitFor(() => expect(useStore.getState().sessionId).toBe(SID_WITH_HISTORY))
    expect(localStorage.getItem('tether_last_sid')).toBe(SID_WITH_HISTORY)
    expect(useStore.getState().notices).toHaveLength(0) // tether#57 — retired on a switch
    await waitFor(() =>
      expect(useStore.getState().messages.map(m => m.text)).toEqual(['B-only prompt']))
  })

  it('still switches when the target session has no history', async () => {
    mockDaemon()
    const reconnects = watchReconnects()
    render(<SessionList />)

    await clickSession('nothing said')

    // The old inline version put setSessionId inside `if (msgs.length > 0)`, so
    // this click changed nothing — except that it had already cleared notices.
    expect(reconnects()).toBe(1)
    await waitFor(() => expect(useStore.getState().sessionId).toBe(SID_EMPTY))
    expect(localStorage.getItem('tether_last_sid')).toBe(SID_EMPTY)
  })
})

describe('SessionList rows read as something', () => {
  it('shows the work item when there is one, the prompt when there is not, the sid otherwise', async () => {
    mockDaemon(() => [
      { sid: 'aaaa1111-bound', workItem: 'tether#91', title: 'add a session list', updatedAt: HOUR_AGO + 2000 },
      { sid: 'bbbb2222-titled', title: 'fix the QUIC handshake', updatedAt: HOUR_AGO + 1000 },
      { sid: 'cccc3333-bare-and-long-enough', updatedAt: HOUR_AGO },
    ])
    render(<SessionList />)
    await clickSession('tether#91')

    // The bound row shows the work item, NOT its title — that precedence is the
    // whole reason the mapping was inverted.
    expect(screen.queryByText('add a session list')).toBeNull()
    expect(screen.getByText('fix the QUIC handshake')).toBeTruthy()
    expect(screen.getByText('cccc3333-bare-an…')).toBeTruthy()
  })

  it('renders the daemon order verbatim — no client-side sort or reverse', async () => {
    // Deliberately NOT sorted by sid, and not the reverse of it either: the bug
    // this list replaces was `[...sessions].reverse()` over a response in
    // UUID-filename order. A component that re-sorts would move these.
    mockDaemon(() => [
      { sid: 'mmmm2222', title: 'newest', updatedAt: 300 },
      { sid: 'aaaa1111', title: 'middle', updatedAt: 200 },
      { sid: 'zzzz3333', title: 'oldest', updatedAt: 100 },
    ])
    const { container } = render(<SessionList />)
    await waitFor(() => screen.getByText('Sessions'))
    fireEvent.click(screen.getByText('Sessions'))
    await waitFor(() => screen.getByText('newest'))

    const labels = [...container.querySelectorAll('.chat-sessions-list .tree-label')]
      .map(n => n.textContent)
    expect(labels).toEqual(['newest', 'middle', 'oldest'])
  })

  it('marks the current session and does not reopen it on click', async () => {
    mockDaemon()
    const reconnects = watchReconnects()
    useStore.setState({ sessionId: SID_WITH_HISTORY })
    const { container } = render(<SessionList />)

    await clickSession('B-only prompt')

    // openSession returns early for the session already open — reopening it would
    // tear down a live WebTransport mid-turn.
    expect(reconnects()).toBe(0)
    const active = container.querySelectorAll('.tree-row.active')
    expect(active).toHaveLength(1)
    expect(active[0].textContent).toContain('B-only prompt')
  })

  it('renders nothing at all when there are no sessions', async () => {
    const fetchMock = mockDaemon(() => [])
    const { container } = render(<SessionList />)
    await waitFor(() => expect(fetchMock).toHaveBeenCalled())
    expect(container.querySelector('.chat-sessions')).toBeNull()
  })
})

// The wiring hop. A correct store plus a correct component with nothing joining
// them is this repo's most reliable failure mode, so this asserts the whole path:
// record a binding for the live session -> the daemon's next answer carries it ->
// the row's label changes, with no reload and no remount.
describe('SessionList reflects a binding made from a wi (tether#91)', () => {
  it('relabels the current session once its work item is recorded', async () => {
    let bound = false
    mockDaemon(() => [
      bound
        ? { sid: SID_WITH_HISTORY, workItem: 'tether#91', title: 'B-only prompt', updatedAt: HOUR_AGO + 1000 }
        : { sid: SID_WITH_HISTORY, title: 'B-only prompt', updatedAt: HOUR_AGO + 1000 },
    ])
    useStore.setState({ sessionId: SID_WITH_HISTORY })
    render(<SessionList />)

    await waitFor(() => screen.getByText('Sessions'))
    fireEvent.click(screen.getByText('Sessions'))
    await waitFor(() => screen.getByText('B-only prompt'))

    // What WorkDetail's Start button does, minus the wi pane.
    bound = true
    await putWiBinding(SID_WITH_HISTORY, 'tether#91')

    await waitFor(() => expect(screen.getByText('tether#91')).toBeTruthy())
    expect(screen.queryByText('B-only prompt')).toBeNull()
  })
})

describe('SessionList migrates the legacy browser mapping (tether#91)', () => {
  it('moves a legacy key on mount and deletes it', async () => {
    const fetchMock = mockDaemon()
    localStorage.setItem('tether_wi_sid:tether#90', SID_EMPTY)

    render(<SessionList />)

    await waitFor(() => expect(localStorage.getItem('tether_wi_sid:tether#90')).toBeNull())
    const puts = fetchMock.mock.calls.filter(([, init]) => (init as RequestInit | undefined)?.method === 'PUT')
    expect(puts).toHaveLength(1)
    expect(puts[0][0]).toBe(`/api/v1/sessions/${SID_EMPTY}/wi`)
  })

  it('issues no PUT at all when the browser holds no legacy keys', async () => {
    const fetchMock = mockDaemon()
    render(<SessionList />)

    await waitFor(() => screen.getByText('Sessions'))
    const puts = fetchMock.mock.calls.filter(([, init]) => (init as RequestInit | undefined)?.method === 'PUT')
    expect(puts).toHaveLength(0)
  })
})

// The daemon answers this route with a directory scan plus a stat and a bounded
// read PER SESSION (session.SessionIndex.List). Asking for it again on every
// click through a 90-session history is real work for no new information, so the
// component asks only when it sees a session it has never listed.
describe('SessionList only refetches when it might have missed a session', () => {
  const listCalls = (fn: { mock: { calls: [string, RequestInit?][] } }) =>
    fn.mock.calls.filter(([url]) => url === '/api/v1/sessions')

  it('does not refetch when switching to a session it already lists', async () => {
    const fetchMock = mockDaemon()
    useStore.setState({ sessionId: SID_WITH_HISTORY })
    render(<SessionList />)
    await waitFor(() => screen.getByText('Sessions'))
    await waitFor(() => expect(listCalls(fetchMock)).toHaveLength(1))

    // A row the list already knows about: the highlight moves, nothing is asked.
    useStore.setState({ sessionId: SID_EMPTY })
    await waitFor(() => screen.getByText('Sessions'))
    expect(listCalls(fetchMock)).toHaveLength(1)
  })

  it('does refetch when a session it has never listed appears', async () => {
    const fetchMock = mockDaemon()
    render(<SessionList />)
    await waitFor(() => screen.getByText('Sessions'))
    await waitFor(() => expect(listCalls(fetchMock)).toHaveLength(1))

    // What a brand-new session looks like from here.
    useStore.setState({ sessionId: 'sid-brand-new-9999' })
    await waitFor(() => expect(listCalls(fetchMock)).toHaveLength(2))
  })
})

describe('SessionList when the daemon does not answer', () => {
  it('keeps the rows it already had rather than blanking the list', async () => {
    let fail = false
    vi.stubGlobal('fetch', vi.fn(async (url: string) => {
      if (url === '/api/v1/sessions') {
        if (fail) return { ok: false, status: 500, json: async () => [] }
        return { ok: true, status: 200, json: async () => DEFAULT_ROWS }
      }
      return { ok: true, status: 200, json: async () => [] }
    }))

    render(<SessionList />)
    await waitFor(() => screen.getByText('Sessions'))
    fireEvent.click(screen.getByText('Sessions'))
    await waitFor(() => screen.getByText('B-only prompt'))

    // A failed refetch must not turn a readable list into an empty one — the
    // rows are still clickable and the chat below them still works.
    fail = true
    useStore.setState({ sessionId: 'sid-brand-new-9999' })
    await new Promise(r => setTimeout(r, 20))

    expect(screen.getByText('B-only prompt')).toBeTruthy()
    expect(screen.getByText('nothing said')).toBeTruthy()
  })
})
