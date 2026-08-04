// tether#61 — the Workspace session list must open a session the SAME way every
// other call site does. It used to inline its own version that never reconnected
// the WebTransport channel and that hid setSessionId (hence tether_last_sid)
// behind a non-empty history, so switching from here left the live stream — and
// the next prompt sent — on the session the user had just left.
//
// These drive the real DOM click rather than spying on openSession, so they fail
// if the list ever grows its own switch again, whatever it is implemented with.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import WorkspacePane from './index'
import { useStore } from '../../lib/store'

const SID_WITH_HISTORY = 'sid-has-history-1'
const SID_EMPTY = 'sid-no-history-2'

/** Route the pane's fetches by URL: workspaces, the session list, the file tree
 *  WorkspaceTree pulls for the auto-expanded workspace, and /messages. Anything
 *  else throws rather than quietly succeeding, so the mock's realism is
 *  self-enforcing when the pane grows a new call. Paths match mux.go. */
function mockDaemon() {
  vi.stubGlobal('fetch', vi.fn(async (url: string) => {
    if (url === '/api/v1/workspaces') {
      return { ok: true, status: 200, json: async () => [{ id: 'ws-1', name: 'tether', path: '/w/tether', addedAt: '' }] }
    }
    if (url === '/api/v1/sessions') {
      return { ok: true, status: 200, json: async () => [SID_WITH_HISTORY, SID_EMPTY] }
    }
    if (url === `/api/v1/sessions/${SID_WITH_HISTORY}/messages`) {
      return { ok: true, status: 200, json: async () => [{ role: 'user', text: 'B-only prompt', ts: 10 }] }
    }
    if (url === `/api/v1/sessions/${SID_EMPTY}/messages`) {
      return { ok: true, status: 200, json: async () => [] }
    }
    if (url.startsWith('/api/v1/workspaces/ws-1/files?dir=')) {
      return { ok: true, status: 200, json: async () => [] }
    }
    throw new Error(`unexpected fetch: ${url}`)
  }))
}

let offRetry: (() => void) | null = null
function watchReconnects(): () => number {
  let n = 0
  const onRetry = () => { n++ }
  window.addEventListener('tether:retry-connection', onRetry)
  offRetry = () => window.removeEventListener('tether:retry-connection', onRetry)
  return () => n
}

/** Expand the collapsed "Sessions" section and click one session row. */
async function clickSession(sid: string) {
  await waitFor(() => screen.getByText('Sessions'))
  fireEvent.click(screen.getByText('Sessions'))
  const row = await waitFor(() => screen.getByText(`${sid.slice(0, 16)}…`))
  fireEvent.click(row)
}

beforeEach(() => {
  localStorage.clear()
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

describe('Workspace session list (tether#61)', () => {
  it('reconnects the WT channel and persists the sid when switching', async () => {
    mockDaemon()
    const reconnects = watchReconnects()
    render(<WorkspacePane />)

    await clickSession(SID_WITH_HISTORY)

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
    render(<WorkspacePane />)

    await clickSession(SID_EMPTY)

    // The old inline version put setSessionId inside `if (msgs.length > 0)`, so
    // this click changed nothing — except that it had already cleared notices.
    expect(reconnects()).toBe(1)
    await waitFor(() => expect(useStore.getState().sessionId).toBe(SID_EMPTY))
    expect(localStorage.getItem('tether_last_sid')).toBe(SID_EMPTY)
  })
})
