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
import { chatURL } from '../../lib/chatUrl'

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
    workspacesLoaded: false, // tether#52 — the store is a module singleton; reset the gate each test
    activeWorkspace: null,   // same singleton, same reason: a test that publishes one must not leak it
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

// tether#52 — ChatPane's first-connect gate (store.ts's workspacesLoaded) must
// release once THIS fetch settles, on both the success and the error path —
// see index.tsx's load(). A gate that only releases on success would leave a
// sid-less first connect waiting forever whenever /api/v1/workspaces 500s or
// the network is down (the 2s fallback timer in ChatPane is a backstop, not a
// substitute — this test is what proves the primary release path works at all).
describe('WorkspacePane workspacesLoaded gate (tether#52)', () => {
  it('sets workspacesLoaded once the workspaces fetch resolves', async () => {
    mockDaemon()
    expect(useStore.getState().workspacesLoaded).toBe(false)
    render(<WorkspacePane />)
    await waitFor(() => expect(useStore.getState().workspacesLoaded).toBe(true))
  })

  it('still sets workspacesLoaded when the workspaces fetch fails', async () => {
    vi.stubGlobal('fetch', vi.fn(async (url: string) => {
      if (url === '/api/v1/workspaces') return { ok: false, status: 500, json: async () => ({}) }
      if (url === '/api/v1/sessions') return { ok: true, status: 200, json: async () => [] }
      throw new Error(`unexpected fetch: ${url}`)
    }))
    expect(useStore.getState().workspacesLoaded).toBe(false)
    render(<WorkspacePane />)
    await waitFor(() => expect(useStore.getState().workspacesLoaded).toBe(true))
  })

  // The composition test that the first version of this slice was missing, and
  // that would have caught it: it is not enough for the gate to OPEN, the
  // workspace has to be published in the same breath.
  //
  // ChatPane's gate subscription runs synchronously inside the store update that
  // opens the gate, and it connects immediately, reading `activeWorkspace` via
  // getState(). A brand-new session's cwd is pinned at spawn, so an
  // `activeWorkspace` that is still null at that instant means the session runs
  // in the daemon's default directory for the rest of its life. Publishing it
  // from the pane's [activeId, workspaces] effect — one React commit later —
  // looks identical in every other test and is exactly the bug.
  it('has already published the selection in the update that opens the gate', async () => {
    mockDaemon()
    const atRelease: Array<{ ws: string | null }> = []
    const unsub = useStore.subscribe(s => {
      if (s.workspacesLoaded && atRelease.length === 0) {
        atRelease.push({ ws: s.activeWorkspace?.id ?? null })
      }
    })
    try {
      render(<WorkspacePane />)
      await waitFor(() => expect(atRelease.length).toBe(1))
    } finally {
      unsub()
    }
    expect(atRelease[0]).toEqual({ ws: 'ws-1' })
  })

  // tether#65 — the mirror of the test above, and the safety property Part B of
  // that wi rests on.
  //
  // tether#65 makes a corrupt ~/.tether/workspaces.json leave the daemon's
  // workspace registry NIL, so `/api/v1/workspaces` stops being registered at all
  // (mux.go wires that route family only for a non-nil registry) and the request
  // falls to mux.go's unconditional `/api/v1/` stub — 501 "not implemented" —
  // rather than to the SPA shell. A nil registry also refuses any request that DOES
  // carry `ws`, with no_workspace_registry. Those two facts are only safe together
  // because of the invariant asserted here: a failed workspaces fetch must publish
  // NO selection, so `activeWorkspace` stays null, so chatURL omits `ws` entirely
  // and the session falls back to --workspace-root instead of being refused.
  //
  // If this ever regressed to publishing a stale or placeholder selection, a
  // corrupt registry file would stop being "the file browser is empty" and become
  // "every new chat is refused" — which is why it is pinned rather than left to
  // the reading of load()'s catch block. The assertion goes through chatURL, not
  // just the store, because the store value alone is not the claim.
  it('publishes no selection when the workspaces fetch fails, so chatURL omits ws', async () => {
    vi.stubGlobal('fetch', vi.fn(async (url: string) => {
      // 501 is what a nil-registry daemon actually answers here (mux.go's
      // `/api/v1/` stub), not a 500 and not the SPA shell — kept faithful so the
      // mock cannot drift into testing a response the daemon never sends.
      if (url === '/api/v1/workspaces') {
        return { ok: false, status: 501, json: async () => ({}) }
      }
      if (url === '/api/v1/sessions') return { ok: true, status: 200, json: async () => [] }
      throw new Error(`unexpected fetch: ${url}`)
    }))

    render(<WorkspacePane />)
    await waitFor(() => expect(useStore.getState().workspacesLoaded).toBe(true))
    expect(useStore.getState().activeWorkspace).toBeNull()

    // The consequence that actually matters, asserted end to end.
    const url = chatURL({
      host: 'h',
      provider: 'claude',
      sid: '',
      wsID: useStore.getState().activeWorkspace?.id ?? '',
    })
    expect(url).not.toContain('ws=')
  })
})
