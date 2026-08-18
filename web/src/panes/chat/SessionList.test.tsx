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
import {
  SESSION_ACTIVITY_PATH,
  resetSessionActivityForTests,
  type SessionActivityMap,
} from '../../lib/sessionActivity'
import {
  EXTERNAL_SESSION_PROMISE,
  putWiBinding,
  resetArmedBinding,
  resetMigrationForTests,
  type SessionSummary,
} from '../../lib/wiSession'

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
function mockDaemon(
  rows: () => SessionSummary[] = () => DEFAULT_ROWS,
  activity: () => SessionActivityMap = () => ({}),
) {
  const fn = vi.fn(async (url: string, init?: RequestInit) => {
    if (url === '/api/v1/sessions') {
      return { ok: true, status: 200, json: async () => rows() }
    }
    // tether#103 — every SessionRow subscribes to the shared activity poller, so
    // rendering this list polls this route. Answered rather than left to the throw
    // below: the poller swallows its own failures, so an unanswered route would
    // mean the rows here never got a marker while this suite stayed green — which
    // is the very shape of silence this mock's "unknown URLs throw" rule exists to
    // prevent.
    if (url === SESSION_ACTIVITY_PATH) {
      return { ok: true, status: 200, json: async () => activity() }
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
  // tether#103 — the activity poller is module-level state, so without this one
  // file's subscribers and timer would still be alive inside the next.
  resetSessionActivityForTests()
  useStore.setState({
    sessionId: 'sid-previous',
    messages: [],
    notices: [{ id: 'n1', text: 'context lost', ts: 5 }],
  })
})

afterEach(() => {
  cleanup()
  resetSessionActivityForTests()
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

// tether#92 — the other wiring hop, and the one this whole slice is about: a
// conversation the coding agent recorded, which the daemon now enumerates, has to
// travel daemon JSON -> fetchSessions -> this list -> the shared row -> the DOM,
// and clicking it has to go through openSession like everything else.
//
// Driven through the real DOM and asserted on openSession's EFFECTS rather than
// on a spy, so a list that grew its own switch for read-only rows fails here.
describe('SessionList shows sessions tether never recorded (tether#92)', () => {
  const CC_SID = 'sid-cc-terminal-01'

  function mockMixedDaemon() {
    const fn = vi.fn(async (url: string) => {
      if (url === '/api/v1/sessions') {
        return {
          ok: true, status: 200, json: async () => [
            { sid: SID_WITH_HISTORY, title: 'B-only prompt', updatedAt: HOUR_AGO + 1000, source: 'tether' },
            { sid: CC_SID, title: 'typed in a terminal', updatedAt: HOUR_AGO, source: 'cc' },
          ] as SessionSummary[],
        }
      }
      if (url === `/api/v1/sessions/${CC_SID}/messages`) {
        // What the daemon now serves for one of these: the coding agent's own
        // transcript, converted. Before this slice the answer was 200 [], which
        // is what made a listed session open as an empty chat.
        return { ok: true, status: 200, json: async () => [{ role: 'user', text: 'typed in a terminal', ts: 20 }] }
      }
      if (url === `/api/v1/sessions/${SID_WITH_HISTORY}/messages`) {
        return { ok: true, status: 200, json: async () => [{ role: 'user', text: 'B-only prompt', ts: 10 }] }
      }
      throw new Error(`unexpected fetch: ${url}`)
    })
    vi.stubGlobal('fetch', fn)
    return fn
  }

  it('renders the row, marks it external, and leaves tether rows unmarked', async () => {
    mockMixedDaemon()
    const { container } = render(<SessionList />)

    await waitFor(() => screen.getByText('Sessions'))
    fireEvent.click(screen.getByText('Sessions'))
    await waitFor(() => screen.getByText('typed in a terminal'))

    const rows = [...container.querySelectorAll('.chat-sessions-list .tree-row')]
    expect(rows).toHaveLength(2)
    const marked = rows.filter(r => r.querySelector('.session-row-src'))
    expect(marked).toHaveLength(1)
    expect(marked[0].textContent).toContain('typed in a terminal')
  })

  it('opens one through openSession and loads its transcript', async () => {
    mockMixedDaemon()
    const reconnects = watchReconnects()
    render(<SessionList />)

    await clickSession('typed in a terminal')

    // openSession's effects, not a mock: the sid persisted, the channel rebound,
    // the transcript loaded.
    expect(reconnects()).toBe(1)
    await waitFor(() => expect(useStore.getState().sessionId).toBe(CC_SID))
    expect(localStorage.getItem('tether_last_sid')).toBe(CC_SID)
    await waitFor(() =>
      expect(useStore.getState().messages.map(m => m.text)).toEqual(['typed in a terminal']))
  })

  it('tells the user what it does and does not promise, once one is open', async () => {
    mockMixedDaemon()
    render(<SessionList />)

    await clickSession('typed in a terminal')

    const note = await waitFor(() => screen.getByRole('note'))
    expect(note.textContent).toBe(EXTERNAL_SESSION_PROMISE)
  })

  // THE regression this design exists for, and the one the first version failed.
  //
  // A fresh mount with the sid already set is what a page reload IS: ChatPane
  // restores tether_last_sid on mount, so the session comes back — and the first
  // version's warning did not, because it was posted from a click handler into
  // `notices`, which resets to [] on every load. The user returned to an external
  // conversation, with an enabled composer, and nothing on screen said so.
  //
  // No click here on purpose: if the promise still needs one, this fails.
  it('still says it after a reload, with no click at all', async () => {
    mockMixedDaemon()
    useStore.setState({ sessionId: CC_SID, notices: [] })

    render(<SessionList />)

    const note = await waitFor(() => screen.getByRole('note'))
    expect(note.textContent).toBe(EXTERNAL_SESSION_PROMISE)
    // And it is visible while the list is COLLAPSED, which is its default state
    // and therefore the state a reload lands in.
    expect(document.querySelector('.chat-sessions-list')).toBeNull()
  })

  it('says nothing while the open session is one tether recorded', async () => {
    mockMixedDaemon()
    useStore.setState({ sessionId: SID_WITH_HISTORY })

    render(<SessionList />)

    await waitFor(() => screen.getByText('Sessions'))
    expect(screen.queryByRole('note')).toBeNull()
  })

  it('withdraws it when the user moves back to a session tether recorded', async () => {
    mockMixedDaemon()
    useStore.setState({ sessionId: CC_SID })
    render(<SessionList />)
    await waitFor(() => screen.getByRole('note'))

    // What session_ready does when a resume forks, and what clicking another row
    // does. Either way the promise must stop applying.
    useStore.setState({ sessionId: SID_WITH_HISTORY })

    await waitFor(() => expect(screen.queryByRole('note')).toBeNull())
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

// tether#101 — the same wiring hop for `runningAs`, driven from RAW DAEMON JSON.
//
// This is the frontend end of a two-part guard, and the two parts check different
// links of one chain:
//
//   Go json tag "runningAs"  ==  the property name in the hand-written TS interface
//        ← internal/session/sessionlist_test.go's TestSessionSummaryIsMirroredInTypeScript
//   that property            ==  what the row actually reads and renders
//        ← this test, plus tsc
//
// The fixture below is a JSON literal rather than a typed object on purpose. A typed
// fixture would be renamed in lockstep by any editor doing a rename, and would go on
// passing while the daemon kept sending the old key; a raw literal cast at the fetch
// boundary — which is exactly what fetchSessions does with the daemon's body — does
// not. The repo has the receipts for missed hops of this shape, and nothing in either
// language fails when this one breaks.
describe('SessionList marks a session a background agent is using (tether#101)', () => {
  const HELD_SID = 'sid-held-by-a-job'

  function mockDaemonWithAHeldRow() {
    const rows = JSON.parse(`[
      { "sid": "${SID_WITH_HISTORY}", "title": "not held", "updatedAt": ${HOUR_AGO + 1000}, "source": "tether" },
      { "sid": "${HELD_SID}", "title": "a job has this open", "updatedAt": ${HOUR_AGO}, "source": "cc", "runningAs": "bg" }
    ]`) as unknown[]
    vi.stubGlobal('fetch', vi.fn(async (url: string) => {
      if (url === '/api/v1/sessions') return { ok: true, status: 200, json: async () => rows }
      return { ok: true, status: 200, json: async () => [] }
    }))
  }

  it('renders the marker on the held row only, from the daemon’s own key name', async () => {
    mockDaemonWithAHeldRow()
    const { container } = render(<SessionList />)

    await waitFor(() => screen.getByText('Sessions'))
    fireEvent.click(screen.getByText('Sessions'))
    await waitFor(() => screen.getByText('a job has this open'))

    const rows = [...container.querySelectorAll('.chat-sessions-list .tree-row')]
    expect(rows).toHaveLength(2)
    const marked = rows.filter(r => r.querySelector('.session-row-running'))
    expect(marked).toHaveLength(1)
    expect(marked[0].textContent).toContain('a job has this open')
    expect(marked[0].querySelector('.session-row-running')?.textContent).toBe('running')
  })

  it('still opens it — the marker is a hint and the daemon has the last word', async () => {
    // If the job has finished by now the session opens normally; if it has not, the
    // attach path answers with session_held_by_background_agent and the card
    // explains it. Either way the click must go through openSession, so the list
    // never becomes a place that silently refuses.
    mockDaemonWithAHeldRow()
    const reconnects = watchReconnects()
    render(<SessionList />)

    await clickSession('a job has this open')

    expect(reconnects()).toBe(1)
    await waitFor(() => expect(useStore.getState().sessionId).toBe(HELD_SID))
    expect(localStorage.getItem('tether_last_sid')).toBe(HELD_SID)
  })
})

// tether#103 — the activity marker reaches the CHAT list's rows too.
//
// The row is shared with the wi detail pane, and this repo's receipt for "shared
// component, therefore fine" is the first draft of that very row: two copies that
// had already diverged three ways by review. So both consumers assert it, and each
// asserts the half that is its own. Here that half is "the marker rides the same
// rows the list already renders, and only on the rows the daemon named" — a
// per-row filter, which is the mistake a `.some()`-shaped implementation makes
// invisible.
describe('SessionList marks which conversations have a turn in flight (tether#103)', () => {
  it('marks only the rows the daemon named, with the state it named', async () => {
    mockDaemon(
      () => DEFAULT_ROWS,
      () => ({ [SID_WITH_HISTORY]: 'working' }),
    )
    const { container } = render(<SessionList />)

    await waitFor(() => screen.getByText('Sessions'))
    fireEvent.click(screen.getByText('Sessions'))
    await waitFor(() => expect(container.querySelectorAll('.session-row-act')).toHaveLength(1))

    const rows = [...container.querySelectorAll('.chat-sessions-list .tree-row')]
    expect(rows).toHaveLength(2)
    // The marked row is the one the daemon named, and the OTHER row carries no
    // marker at all — absence in the map means nothing live holds that session, so
    // the row must say nothing rather than settle on a default.
    const marked = rows.filter(r => r.querySelector('.session-row-act'))
    expect(marked).toHaveLength(1)
    expect(marked[0].textContent).toContain('B-only prompt')
    expect(marked[0].querySelector('.session-row-act')?.className).toBe('session-row-act working')
  })
})
