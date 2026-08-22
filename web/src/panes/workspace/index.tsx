import { useEffect, useRef, useState } from 'react'
import { httpErrorMessage } from '../../lib/httpError'
import { Icon } from '../../lib/icons'
import { rememberedWorkspaceId, useStore } from '../../lib/store'
import WorkspaceTree from './WorkspaceTree'

interface Workspace {
  id: string
  name: string
  path: string
  addedAt: string
  activeSid?: string
}

/**
 * resolveSelection decides which workspace is selected once a
 * GET /api/v1/workspaces response is in hand. Extracted as a pure function
 * (mirrors chatURL / shouldDeferFirstConnect) because it is the whole of
 * tether#66: which of these three candidates wins.
 *
 * Order, and why each rung exists:
 *
 *  1. `currentId`, if the registry still contains it. load() re-runs after every
 *     add and delete, and it must never yank the selection out from under a user
 *     who is working in it.
 *  2. `persistedId`, if the registry still contains it. THIS RUNG IS THE FIX.
 *     Selecting a workspace only ever decides where the NEXT session runs
 *     (a session's cwd is pinned at spawn and cc's --resume is cwd-scoped, so it
 *     can never be moved — see chatUrl.ts), and the only way to get a next
 *     session is App's startNewSession, which drops the sid and calls
 *     location.reload(). Before tether#66 the selection lived in component state
 *     only, so the reload that acted on the choice was also the thing that
 *     destroyed it: every new chat landed in registry[0] no matter what the user
 *     had clicked, and re-ordering ~/.tether/workspaces.json was the workaround.
 *     An id the registry no longer contains falls through: a removed workspace is
 *     not a selection, and a bad or hand-edited value can never reach the wire
 *     because it has to be found in the fetched registry to be published at all.
 *  3. `registry[0]`, so a profile that has never chosen still gets a workspace
 *     rather than the daemon's --workspace-root. Note this rung REPLACES the
 *     remembered id rather than leaving it alone — whatever it returns is
 *     published and therefore persisted by the caller. That is what you want for
 *     a workspace the user deleted; it also means a response that is missing the
 *     remembered workspace for any other reason (a registry file caught
 *     mid-rewrite, a different daemon on the same origin) costs the user their
 *     preference. Acceptable: the alternative is holding a selection the daemon
 *     would refuse, and load() only ever sees a 200 it could parse.
 *
 * Null only when the registry is EMPTY. That is deliberate: null makes chatURL
 * omit `ws` entirely, and the daemon then falls back to --workspace-root — the
 * right answer when there is genuinely nothing registered, and (since the row
 * click no longer clears the selection, see onRowClick) the only way to get it.
 *
 * Scope of the persistence: it is one key for the whole origin, last writer
 * wins. Two tabs do converge (a second tab has no `currentId`, so it adopts the
 * remembered id), but a click in one tab moves the preference under the other,
 * whose sidebar keeps highlighting the old row until it reloads. Fine for a
 * single-user daemon; it is a preference, not session state.
 */
export function resolveSelection<T extends { id: string }>(o: {
  registry: T[]
  currentId: string | null
  persistedId: string | null
}): T | null {
  const live = o.currentId ? o.registry.find(w => w.id === o.currentId) : undefined
  if (live) return live
  const saved = o.persistedId ? o.registry.find(w => w.id === o.persistedId) : undefined
  if (saved) return saved
  return o.registry[0] ?? null
}

export default function WorkspacePane() {
  const [workspaces, setWorkspaces] = useState<Workspace[]>([])
  const [error, setError] = useState<string | null>(null)
  // tether#66 — SELECTION and DISCLOSURE are two different things, and one
  // useState used to be both. `selectedId` is a durable preference ("new chats
  // run here", persisted); `expandedId` is which row's file tree is open right
  // now. They were the same variable, which meant the second click on the open
  // row — an ordinary collapse — also cleared the selection. Harmless while the
  // selection died at every reload anyway; a footgun the moment it survives one,
  // since collapsing a tree would durably move new sessions to
  // --workspace-root with nothing on screen saying so.
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const [expandedId, setExpandedId] = useState<string | null>(null)

  // tether#47 — publish the selected workspace (id + abspath) to the store so
  // chat's @-mention picker knows which workspace's files to offer, and so a
  // sid-less connect can pin its cwd to it (tether#52 chatUrl.ts). Covers every
  // setSelectedId path (initial resolve, row click, delete). Null when none.
  useEffect(() => {
    const ws = workspaces.find(w => w.id === selectedId)
    useStore.getState().setActiveWorkspace(ws ? { id: ws.id, path: ws.path } : null)
  }, [selectedId, workspaces])
  const [filter, setFilter] = useState('')
  const filterRef = useRef<HTMLInputElement>(null)
  const [adding, setAdding] = useState(false)
  const [newPath, setNewPath] = useState('')
  const [newName, setNewName] = useState('')

  // tether#161 — the error row below renders whatever these two throws carry, and
  // they used to carry `HTTP ${res.status}`. On the ADD path that is the difference
  // between "HTTP 400" and "workspace: a workspace path must be absolute" — a
  // sentence tether#147 wrote for exactly this moment, derived from the sentinel
  // the daemon hit and deliberately free of any daemon-side path.
  const load = async () => {
    try {
      const res = await fetch('/api/v1/workspaces')
      if (!res.ok) throw new Error(await httpErrorMessage(res))
      const data = await res.json() as Workspace[]
      setWorkspaces(data)
      setError(null)
      // tether#66 — the remembered id outranks data[0]; see resolveSelection.
      // `selectedId` is this render's value: a click that lands while this fetch
      // is in flight is not seen here, so the resolve republishes the selection
      // the user just left. It corrects itself on the next commit because
      // setWorkspaces installs a fresh array and the publishing effect below
      // re-runs on it — unreachable today (there are no rows to click before the
      // first load resolves) but it is why that effect must not be memoized away.
      const sel = resolveSelection({
        registry: data,
        currentId: selectedId,
        persistedId: rememberedWorkspaceId(),
      })
      if (sel && sel.id !== selectedId) {
        setSelectedId(sel.id)
        // Open the newly-selected row's tree. Only reached when the selection
        // actually moved (first load, or a delete that took the selected one),
        // so a load() triggered by add/delete cannot re-open a tree the user
        // has since collapsed.
        setExpandedId(sel.id)
      }
      // tether#52 — release ChatPane's first-connect gate, and publish the
      // selection IN THE SAME store update (store.ts settleWorkspaces).
      //
      // The selection is computed here rather than left to the
      // [selectedId, workspaces] effect below, and that is the fix for a real
      // bug: the effect runs one React commit LATER, while zustand notifies
      // ChatPane's gate listener synchronously, so releasing the gate from here
      // and publishing from there meant every fresh session connected before the
      // workspace was known — with no `ws`, into --workspace-root, permanently.
      // The effect still owns every LATER change (row click, delete) and
      // re-publishes the same value idempotently.
      useStore.getState().settleWorkspaces(sel ? { id: sel.id, path: sel.path } : null)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
      // Release the gate but publish NO selection: a failed fetch must not leave
      // chat waiting forever for a list that will never arrive (ChatPane's 2s
      // fallback timer is the other half of that guarantee), and must not wipe a
      // selection an earlier successful load already published — load() also runs
      // after add/remove.
      useStore.getState().setWorkspacesLoaded(true)
    }
  }

  useEffect(() => { void load() }, [])

  // ⌘P / Ctrl+P focuses the workspace filter.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'p') {
        e.preventDefault()
        filterRef.current?.focus()
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [])

  // The middle-pane home's "Open file" quick-action focuses the filter (tether#33).
  useEffect(() => {
    const onFocusFiles = () => filterRef.current?.focus()
    window.addEventListener('tether:focus-files', onFocusFiles)
    return () => window.removeEventListener('tether:focus-files', onFocusFiles)
  }, [])

  const addWorkspace = async () => {
    const path = newPath.trim()
    if (!path) return
    const name = newName.trim() || (path.split('/').pop() ?? path)
    try {
      const res = await fetch('/api/v1/workspaces', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name, path }),
      })
      if (!res.ok) throw new Error(await httpErrorMessage(res))
      setNewPath('')
      setNewName('')
      setAdding(false)
      await load()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }

  const remove = async (id: string, e: React.MouseEvent) => {
    e.stopPropagation()
    await fetch(`/api/v1/workspaces/${id}`, { method: 'DELETE' })
    // These do NOT steer the load() below — it closes over this render's
    // `selectedId` and still sees the old id. Picking the replacement is
    // resolveSelection's job and needs no help: the deleted id is gone from the
    // registry, so rungs 1 and 2 both miss and data[0] wins. Deleting the
    // selected workspace is the one case where "the selection moved and the user
    // did not choose the new one" is correct.
    //
    // What they DO buy is the fail-safe when that reload does not arrive (the
    // daemon 500s, or goes away between the DELETE and the GET). Without them the
    // pane would keep a deleted workspace as its published selection, and the
    // next new session would hand the daemon an id it no longer knows —
    // `unknown_workspace`, i.e. a refused chat. Clearing them means the worst
    // case is a null selection and a fall back to --workspace-root.
    if (selectedId === id) setSelectedId(null)
    if (expandedId === id) setExpandedId(null)
    await load()
  }

  // A row click SELECTS (durably — store + localStorage via the effect above)
  // and toggles that row's file tree. It deliberately never DESELECTS: see the
  // selectedId/expandedId split above and resolveSelection's closing paragraph.
  // Selecting cannot disturb a live session — a session's workspace is fixed at
  // spawn, and chatUrl.ts only ever sends `ws` when there is no sid — so this is
  // a preference for the next new session, not a rebind of the current one.
  const onRowClick = (id: string) => {
    setExpandedId(cur => (cur === id ? null : id))
    setSelectedId(id)
  }

  const filtered = workspaces.filter(ws =>
    !filter ||
    ws.name.toLowerCase().includes(filter.toLowerCase()) ||
    ws.path.toLowerCase().includes(filter.toLowerCase())
  )

  const liveCount = workspaces.filter(ws => ws.activeSid).length

  return (
    <>
      <div className="dt-left-head">
        <span className="section-label">Workspaces</span>
        <button className="icon-btn-sm" onClick={() => setAdding(a => !a)} title="Add workspace" aria-label="Add workspace">
          <Icon name="plus" size={12} />
        </button>
      </div>

      {adding && (
        <div className="ws-add-form">
          <input
            className="skill-input"
            placeholder="workspace path"
            autoFocus
            value={newPath}
            onChange={e => setNewPath(e.target.value)}
            onKeyDown={e => { if (e.key === 'Enter') void addWorkspace(); if (e.key === 'Escape') setAdding(false) }}
          />
          <input
            className="skill-input"
            placeholder="name (optional)"
            value={newName}
            onChange={e => setNewName(e.target.value)}
            onKeyDown={e => { if (e.key === 'Enter') void addWorkspace(); if (e.key === 'Escape') setAdding(false) }}
          />
          <div style={{ display: 'flex', gap: 6 }}>
            <button className="btn-primary-sm" disabled={!newPath.trim()} onClick={() => void addWorkspace()}>Add</button>
            <button className="btn-ghost-sm" onClick={() => { setAdding(false); setNewPath(''); setNewName('') }}>Cancel</button>
          </div>
        </div>
      )}

      <div className="dt-search">
        <Icon name="search" size={12} style={{ color: 'var(--ink-quat)', flexShrink: 0 }} />
        <input
          ref={filterRef}
          value={filter}
          onChange={e => setFilter(e.target.value)}
          placeholder="filter…"
        />
        <span className="kbd">⌘P</span>
      </div>

      <div className="dt-tree scroll-thin">
        {error && (
          <div style={{ padding: '8px 12px', color: 'var(--danger)', fontSize: 11 }}>{error}</div>
        )}
        {filtered.length === 0 && !error && (
          <div style={{ padding: '8px 12px', color: 'var(--ink-quat)', fontSize: 12, fontFamily: 'var(--font-mono)' }}>
            {filter ? 'no matches' : 'no workspaces'}
          </div>
        )}
        {filtered.map(ws => (
          <div key={ws.id}>
            <div
              className={`tree-row${selectedId === ws.id ? ' active' : ''}`}
              style={{ paddingLeft: 8 }}
              onClick={() => onRowClick(ws.id)}
            >
              <Icon
                name={expandedId === ws.id ? 'chev-down' : 'chevron'}
                size={11}
                style={{ color: 'var(--ink-quat)', flexShrink: 0 }}
              />
              <span className={`ws-dot${ws.activeSid ? ' live' : ''}`} />
              <span className="tree-label" style={{ fontWeight: 600, flex: 1 }}>{ws.name}</span>
              <button
                onClick={e => remove(ws.id, e)}
                className="ws-remove-btn"
                title="Remove workspace"
                aria-label={`Remove workspace ${ws.name}`}
              >×</button>
            </div>
            {expandedId === ws.id && (
              <div style={{ paddingLeft: 32, paddingRight: 10, paddingTop: 2, paddingBottom: 6 }}>
                <div style={{ fontFamily: 'var(--font-mono)', fontSize: 10.5, color: 'var(--ink-quat)', whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>
                  {ws.path}
                </div>
                {ws.activeSid && (
                  <div style={{ fontFamily: 'var(--font-mono)', fontSize: 10.5, color: 'var(--success)', marginTop: 2 }}>
                    {ws.activeSid.slice(0, 12)}…
                  </div>
                )}
              </div>
            )}
            {expandedId === ws.id && <WorkspaceTree workspaceId={ws.id} />}
          </div>
        ))}
      </div>

      {/* tether#91 — the session list used to be here, under the file tree. It
          moved to the chat pane (panes/chat/SessionList.tsx), which is where a
          session is a first-class thing; this column is about files. It is not
          duplicated here: two lists over one endpoint is how the two "open a
          session" implementations lib/session.ts had to consolidate came about. */}

      <div className="dt-left-foot">
        {workspaces.length} workspace{workspaces.length !== 1 ? 's' : ''}
        {liveCount > 0 && ` · ${liveCount} live`}
      </div>
    </>
  )
}
