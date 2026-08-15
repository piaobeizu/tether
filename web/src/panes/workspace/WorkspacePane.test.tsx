// tether#91 — this pane no longer owns a session list. It moved to the chat pane
// (panes/chat/SessionList.tsx), where the click-through tests moved with it; the
// only session-list assertion left here is the one that keeps it from coming back,
// because "the old one is still there too" is the failure mode that produced
// lib/session.ts in the first place.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import WorkspacePane, { resolveSelection } from './index'
import { useStore, WORKSPACE_ID_KEY } from '../../lib/store'
import { chatURL } from '../../lib/chatUrl'

/** Route the pane's fetches by URL: workspaces and the file tree WorkspaceTree
 *  pulls for the auto-expanded workspace. Anything else throws rather than
 *  quietly succeeding, so the mock's realism is self-enforcing when the pane
 *  grows a new call — and so a re-added session-list fetch is loud. Paths match
 *  mux.go. */
function mockDaemon() {
  vi.stubGlobal('fetch', vi.fn(async (url: string) => {
    if (url === '/api/v1/workspaces') {
      return { ok: true, status: 200, json: async () => [{ id: 'ws-1', name: 'tether', path: '/w/tether', addedAt: '' }] }
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
  offGate?.(); offGate = null
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

// tether#91 — the session list moved to chat. This is the guard that it moved
// rather than multiplied.
//
// Deleting the old one is part of that slice, not tidying that could follow it:
// two lists over one endpoint is precisely the shape lib/session.ts's doc
// describes, where three call sites each implemented "open a session" and two of
// them were quietly broken. The behavioural half of the list is asserted in
// panes/chat/SessionList.test.tsx; what is asserted HERE is absence.
describe('Workspace pane no longer owns a session list (tether#91)', () => {
  it('renders no Sessions section and never asks for the session list', async () => {
    mockDaemon()
    render(<WorkspacePane />)

    // Wait for the pane to have finished its own work, so "no request" is a
    // statement about a settled component and not about a race.
    await waitFor(() => expect(useStore.getState().workspacesLoaded).toBe(true))
    await waitFor(() => expect(screen.getByText('tether')).toBeTruthy())

    expect(screen.queryByText('Sessions')).toBeNull()
    // The URL, not just the absence of the heading: a list fetched and not
    // rendered is still a second owner of the endpoint, and the mock swallows the
    // error a stray fetch would raise (the pane's own catch does too).
    const urls = (globalThis.fetch as unknown as { mock: { calls: [string][] } }).mock.calls.map(c => c[0])
    expect(urls).not.toContain('/api/v1/sessions')
    expect(urls.some(u => u.startsWith('/api/v1/sessions'))).toBe(false)
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
  // from the pane's [selectedId, workspaces] effect — one React commit later —
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

// ─── tether#66 ───────────────────────────────────────────────────────────────
//
// Selecting a workspace had no effect on where a new session ran. Every piece
// worked; they just could not be combined:
//
//   - the selection lived in ONE component useState (this pane),
//   - `ws` travels only when there is no sid (chatUrl.ts — tether#52, so a
//     reconnect can never rebind a live session), so the workspace can only be
//     decided at the instant a session is CREATED,
//   - and the only way to create one is App's startNewSession, which is
//     `localStorage.removeItem('tether_last_sid')` + `location.reload()`.
//
// The reload that acts on the choice is the same reload that destroys it, so the
// pane came back up and selected data[0]. Re-ordering ~/.tether/workspaces.json
// was the workaround.
//
// The load-bearing test here is the RELOAD HOP one. "After the click the store
// holds the new workspace" was already true before this fix — asserting only
// that is the green-suite-but-dead-path failure mode this project keeps hitting.
const WS_ALPHA = { id: 'ws-alpha', name: 'alpha', path: '/w/alpha', addedAt: '' }
const WS_BETA = { id: 'ws-beta', name: 'beta', path: '/w/beta', addedAt: '' }

/** Two workspaces, in registry order — alpha is data[0], beta is the one whose
 *  selection used to be impossible to act on. */
function mockTwoWorkspaces() {
  vi.stubGlobal('fetch', vi.fn(async (url: string) => {
    if (url === '/api/v1/workspaces') {
      return { ok: true, status: 200, json: async () => [WS_ALPHA, WS_BETA] }
    }
    if (/^\/api\/v1\/workspaces\/ws-(alpha|beta)\/files\?dir=/.test(url)) {
      return { ok: true, status: 200, json: async () => [] }
    }
    throw new Error(`unexpected fetch: ${url}`)
  }))
}

/** What App.startNewSession leaves behind: the sid is gone, the page is torn down
 *  and rebuilt, and localStorage is the ONLY thing that crosses over. The store is
 *  a module singleton in this process, so reset it to its initial state — a real
 *  reload blanks EVERY field, and resetting only the two this test reads would let
 *  it pass on the strength of state a fresh page does not have. */
function reloadPage(unmount: () => void) {
  unmount()
  cleanup()
  localStorage.removeItem('tether_last_sid')
  useStore.setState(useStore.getInitialState())
}

/** Capture `activeWorkspace` at the FIRST notification in which the
 *  first-connect gate opens, and nothing later.
 *
 *  This is the only sample that means anything for tether#66, and asserting on
 *  `getState()` after the dust settles is not a substitute. zustand notifies
 *  synchronously from inside `set`, ChatPane's gate subscriber calls doConnect
 *  right there, and doConnect bakes `activeWorkspace` into the connect URL — so
 *  the value that matters is the one carried by the update that flips
 *  `workspacesLoaded`, not the one the publishing effect writes a commit later.
 *  RTL's `waitFor`/`render` flush effects inside `act`, which makes those two
 *  indistinguishable from the outside: hand `settleWorkspaces` the pre-tether#66
 *  `data[0]` while `resolveSelection` still drives the local state, and every
 *  post-hoc assertion in this file stays green while the real ChatPane connects
 *  with the wrong `ws`. That is tether#52's bug, rebuilt. Hence this subscriber. */
function watchGateOpen(): () => string | null | undefined {
  let atOpen: string | null | undefined
  const unsub = useStore.subscribe(s => {
    if (s.workspacesLoaded && atOpen === undefined) atOpen = s.activeWorkspace?.id ?? null
  })
  offGate = unsub
  return () => atOpen
}
let offGate: (() => void) | null = null

describe('resolveSelection (tether#66)', () => {
  const reg = [WS_ALPHA, WS_BETA]

  it('prefers the remembered workspace over data[0]', () => {
    expect(resolveSelection({ registry: reg, currentId: null, persistedId: 'ws-beta' }))
      .toBe(WS_BETA)
  })

  it('falls back to data[0] when nothing is remembered', () => {
    expect(resolveSelection({ registry: reg, currentId: null, persistedId: null }))
      .toBe(WS_ALPHA)
  })

  // A workspace the user removed — or a registry file swapped underneath the
  // browser — must not leave the pane with a selection the daemon would refuse.
  it('ignores a remembered id the registry no longer contains', () => {
    expect(resolveSelection({ registry: reg, currentId: null, persistedId: 'ws-gone' }))
      .toBe(WS_ALPHA)
  })

  // load() re-runs on every add and delete; it must not yank a live selection
  // back to the remembered one.
  it('keeps the current selection ahead of both the remembered id and data[0]', () => {
    expect(resolveSelection({ registry: reg, currentId: 'ws-beta', persistedId: 'ws-alpha' }))
      .toBe(WS_BETA)
  })

  // The regression the wi calls for: nothing registered → no `ws` on the wire →
  // the daemon spawns in --workspace-root. Null must stay reachable for that.
  it('returns null for an empty registry', () => {
    expect(resolveSelection({ registry: [], currentId: 'ws-beta', persistedId: 'ws-beta' }))
      .toBeNull()
  })
})

describe('WorkspacePane selection survives a reload (tether#66)', () => {
  it('opens the next session in the SELECTED workspace, not data[0]', async () => {
    mockTwoWorkspaces()

    // ── Page 1: cold profile, so the pane defaults to data[0].
    const first = render(<WorkspacePane />)
    await waitFor(() => expect(useStore.getState().activeWorkspace?.id).toBe('ws-alpha'))

    // The user clicks the SECOND workspace.
    fireEvent.click(await screen.findByText('beta'))
    await waitFor(() => expect(useStore.getState().activeWorkspace?.id).toBe('ws-beta'))
    // Everything above this line was already true before tether#66.
    expect(localStorage.getItem(WORKSPACE_ID_KEY)).toBe('ws-beta')

    // ── The hop. This is the whole bug.
    reloadPage(first.unmount)

    // ── Page 2, i.e. the page on which the new session is actually created.
    const atGateOpen = watchGateOpen()
    render(<WorkspacePane />)
    await waitFor(() => expect(useStore.getState().workspacesLoaded).toBe(true))

    // The assertion that carries the fix: the remembered workspace is already
    // published in the update that releases the gate, because that is the instant
    // ChatPane connects and pins the new session's cwd for its whole life.
    expect(atGateOpen()).toBe('ws-beta') // was 'ws-alpha'
    expect(useStore.getState().activeWorkspace?.id).toBe('ws-beta')

    // The consequence, asserted through the thing that carries it. ChatPane's
    // sid-less doConnect builds exactly this URL from exactly these two reads.
    expect(chatURL({
      host: 'h',
      provider: 'claude-code',
      sid: localStorage.getItem('tether_last_sid') ?? '',
      wsID: atGateOpen() ?? '',
    })).toBe('https://h/wt/chat?provider=claude-code&ws=ws-beta')

    // …and the row the user sees marked is that same one. `.tree-row.active` is
    // now the only on-screen statement of a durable preference, so it is part of
    // the fix rather than decoration.
    const betaRow = screen.getByLabelText('Remove workspace beta').parentElement
    expect(betaRow?.className).toContain('active')
    expect(screen.getByLabelText('Remove workspace alpha').parentElement?.className)
      .not.toContain('active')
  })

  // The reason rememberWorkspace records but never erases (store.ts). This pane
  // publishes `null` on mount — before GET /api/v1/workspaces resolves — so an
  // erase-on-null would blank the remembered id on every page load. That version
  // of the fix passes every other test in this file, because they all start from
  // a cleared localStorage.
  it('does not let the mount-time null publish erase the remembered id', async () => {
    localStorage.setItem(WORKSPACE_ID_KEY, 'ws-beta')
    mockTwoWorkspaces()

    render(<WorkspacePane />)
    // Sampled at the instant the pane has mounted (and published null) but the
    // fetch has not settled: the remembered id must still be there to be read.
    expect(localStorage.getItem(WORKSPACE_ID_KEY)).toBe('ws-beta')

    await waitFor(() => expect(useStore.getState().activeWorkspace?.id).toBe('ws-beta'))
  })

  // The deselect decision. The row click used to toggle: clicking the open row
  // set the selection back to null. That was invisible while the selection died
  // at every reload; now that it survives one, collapsing a file tree would
  // durably send new chats to --workspace-root with nothing on screen saying so.
  // So a row click SELECTS and toggles only the TREE. Deliberate deselection is
  // gone — "run in --workspace-root" was never a thing the UI could name (it is
  // a daemon launch flag), and it stays reachable where it is meaningful: an
  // empty registry (see resolveSelection's null case above).
  it('collapsing the selected row keeps the selection', async () => {
    mockTwoWorkspaces()
    render(<WorkspacePane />)
    await waitFor(() => expect(useStore.getState().activeWorkspace?.id).toBe('ws-alpha'))

    // alpha auto-expanded on load, so its path detail is showing.
    expect(screen.getByText('/w/alpha')).toBeTruthy()

    fireEvent.click(screen.getByText('alpha'))

    // Tree collapsed…
    await waitFor(() => expect(screen.queryByText('/w/alpha')).toBeNull())
    // …selection intact, in the store AND on disk.
    expect(useStore.getState().activeWorkspace?.id).toBe('ws-alpha')
    expect(localStorage.getItem(WORKSPACE_ID_KEY)).toBe('ws-alpha')
  })

  // Selecting is a preference for the NEXT session, never a rebind of the live
  // one (tether#52's invariant). Nothing about a row click may reach the
  // connection, and nothing may make a reconnect start carrying `ws`.
  //
  // The two guards below already held before tether#66 — a workspace row click
  // has never dispatched a reconnect. They are here as regression protection for
  // the thing this slice makes tempting ("the selection changed, go use it"), not
  // as evidence of the fix.
  it('does not touch the live session when the selection changes', async () => {
    mockTwoWorkspaces()
    const reconnects = watchReconnects()
    localStorage.setItem('tether_last_sid', 'sid-live')
    useStore.setState({ sessionId: 'sid-live' })
    render(<WorkspacePane />)
    await waitFor(() => expect(useStore.getState().activeWorkspace?.id).toBe('ws-alpha'))

    fireEvent.click(await screen.findByText('beta'))
    await waitFor(() => expect(useStore.getState().activeWorkspace?.id).toBe('ws-beta'))

    expect(reconnects()).toBe(0)
    expect(useStore.getState().sessionId).toBe('sid-live')
    // The URL a reconnect would build NOW — every value read from live state, so
    // this fails if a selection change ever starts leaking into a resumed
    // session. The sid wins and `ws` is absent even though the selection moved.
    expect(chatURL({
      host: 'h',
      provider: 'claude-code',
      sid: localStorage.getItem('tether_last_sid') ?? '',
      wsID: useStore.getState().activeWorkspace?.id ?? '',
    })).toBe('https://h/wt/chat?provider=claude-code&sid=sid-live')
  })

  // remove() is the one place where the selection legitimately moves without the
  // user choosing the replacement, and it is also the only caller that relies on
  // resolveSelection dropping an id that has left the registry. Nothing pinned
  // it before. The collapse in the middle doubles as the guard on
  // `sel.id !== selectedId`: a load() caused by something else must not re-open a
  // tree the user closed.
  it('deleting the selected workspace falls back, and re-persists', async () => {
    const remaining = [WS_ALPHA]
    let deleted = false
    vi.stubGlobal('fetch', vi.fn(async (url: string, init?: { method?: string }) => {
      if (url === '/api/v1/workspaces') {
        return { ok: true, status: 200, json: async () => (deleted ? remaining : [WS_ALPHA, WS_BETA]) }
      }
      if (url === '/api/v1/workspaces/ws-beta' && init?.method === 'DELETE') {
        deleted = true
        return { ok: true, status: 204, json: async () => ({}) }
      }
      if (/^\/api\/v1\/workspaces\/ws-(alpha|beta)\/files\?dir=/.test(url)) {
        return { ok: true, status: 200, json: async () => [] }
      }
      throw new Error(`unexpected fetch: ${url}`)
    }))

    render(<WorkspacePane />)
    await waitFor(() => expect(useStore.getState().activeWorkspace?.id).toBe('ws-alpha'))

    // Select beta, then collapse its tree — selection on beta, nothing expanded.
    fireEvent.click(await screen.findByText('beta'))
    await waitFor(() => expect(screen.getByText('/w/beta')).toBeTruthy())
    fireEvent.click(screen.getByText('beta'))
    await waitFor(() => expect(screen.queryByText('/w/beta')).toBeNull())
    expect(localStorage.getItem(WORKSPACE_ID_KEY)).toBe('ws-beta')

    fireEvent.click(screen.getByLabelText('Remove workspace beta'))

    // Falls back to the only survivor — and the remembered id follows, so the
    // next reload cannot resurrect a workspace the daemon would reject.
    await waitFor(() => expect(useStore.getState().activeWorkspace?.id).toBe('ws-alpha'))
    expect(localStorage.getItem(WORKSPACE_ID_KEY)).toBe('ws-alpha')
    expect(screen.queryByLabelText('Remove workspace beta')).toBeNull()
  })

  // The fail-safe half of remove(): if the reload after the DELETE does not
  // arrive, the pane must not keep publishing the workspace that no longer
  // exists — the daemon would refuse the next new session with
  // `unknown_workspace`. Dropping to null instead falls back to
  // --workspace-root, which is a working chat.
  it('does not keep a deleted workspace selected when the reload fails', async () => {
    let deleted = false
    vi.stubGlobal('fetch', vi.fn(async (url: string, init?: { method?: string }) => {
      if (url === '/api/v1/workspaces') {
        if (deleted) return { ok: false, status: 500, json: async () => ({}) }
        return { ok: true, status: 200, json: async () => [WS_ALPHA, WS_BETA] }
      }
      if (url === '/api/v1/workspaces/ws-beta' && init?.method === 'DELETE') {
        deleted = true
        return { ok: true, status: 204, json: async () => ({}) }
      }
      if (/^\/api\/v1\/workspaces\/ws-(alpha|beta)\/files\?dir=/.test(url)) {
        return { ok: true, status: 200, json: async () => [] }
      }
      throw new Error(`unexpected fetch: ${url}`)
    }))

    render(<WorkspacePane />)
    await waitFor(() => expect(useStore.getState().activeWorkspace?.id).toBe('ws-alpha'))
    fireEvent.click(await screen.findByText('beta'))
    await waitFor(() => expect(useStore.getState().activeWorkspace?.id).toBe('ws-beta'))

    fireEvent.click(screen.getByLabelText('Remove workspace beta'))

    await waitFor(() => expect(useStore.getState().activeWorkspace).toBeNull())
    expect(chatURL({
      host: 'h', provider: 'claude-code', sid: '',
      wsID: useStore.getState().activeWorkspace?.id ?? '',
    })).not.toContain('ws=')
  })

  // The `sel.id !== selectedId` guard in load(). load() re-runs on every add and
  // delete; when it resolves to the selection you already had, it must leave the
  // disclosure state alone. Without the guard, deleting some OTHER workspace
  // re-opens the tree you had just closed.
  it('a load that does not move the selection leaves the tree collapsed', async () => {
    let deleted = false
    vi.stubGlobal('fetch', vi.fn(async (url: string, init?: { method?: string }) => {
      if (url === '/api/v1/workspaces') {
        return { ok: true, status: 200, json: async () => (deleted ? [WS_BETA] : [WS_ALPHA, WS_BETA]) }
      }
      if (url === '/api/v1/workspaces/ws-alpha' && init?.method === 'DELETE') {
        deleted = true
        return { ok: true, status: 204, json: async () => ({}) }
      }
      if (/^\/api\/v1\/workspaces\/ws-(alpha|beta)\/files\?dir=/.test(url)) {
        return { ok: true, status: 200, json: async () => [] }
      }
      throw new Error(`unexpected fetch: ${url}`)
    }))

    render(<WorkspacePane />)
    await waitFor(() => expect(useStore.getState().activeWorkspace?.id).toBe('ws-alpha'))
    fireEvent.click(await screen.findByText('beta'))          // select beta (expands)
    await waitFor(() => expect(screen.getByText('/w/beta')).toBeTruthy())
    fireEvent.click(screen.getByText('beta'))                 // collapse, keep selected
    await waitFor(() => expect(screen.queryByText('/w/beta')).toBeNull())

    fireEvent.click(screen.getByLabelText('Remove workspace alpha'))

    await waitFor(() => expect(screen.queryByLabelText('Remove workspace alpha')).toBeNull())
    expect(useStore.getState().activeWorkspace?.id).toBe('ws-beta') // selection untouched
    expect(screen.queryByText('/w/beta')).toBeNull()               // and still collapsed
  })
})
