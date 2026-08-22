// Canvas — one of the middle column's main views, picked from the left activity
// bar (tether#90). The other is Work.
//
// Reads the shared selection slice from the store (lib/store.ts) and renders a
// workspace file's content when one is selected (from the left WorkspaceTree),
// otherwise an empty-state hint. Canvas is the ONLY reader of
// store.selectedFile, which is why App switches the middle column back to this
// view whenever a file is picked — otherwise a tree click while Work was up
// would set state nothing renders.
//
// The Work relationship map moved OUT of the middle into a right-pane tab in
// tether#26 (the middle map read as cluttered) and came back to the middle as
// its own activity-bar view in tether#90 — beside this one, not sharing it.
//
// Markdown file rendering (tether#21) stays: `.md` files render via the
// lazy-loaded <Markdown> component (react-markdown + remark-gfm); non-markdown
// files keep the plain <pre> fallback.
import { lazy, Suspense, useEffect, useState } from 'react'
import { useStore } from '../../lib/store'
import { describeError, fetchFile, fetchWorkspaces, type Workspace } from '../../lib/aihub'
import { Icon } from '../../lib/icons'

const Markdown = lazy(() => import('./Markdown'))

export default function Canvas() {
  const selectedFile = useStore((s) => s.selectedFile)

  if (selectedFile) return <FileMode wsId={selectedFile.wsId} path={selectedFile.path} />
  return <CanvasHome />
}

function selectTab(detail: 'chat' | 'work') {
  window.dispatchEvent(new CustomEvent('tether:select-tab', { detail }))
}

// CanvasHome — the middle-pane empty state (tether#33). Replaces the old lone
// faint hint with a centered, branded home: the tether mark, the current
// workspace's name/path, and three quick-action entries. This is a welcoming
// landing surface, not a data dashboard — "Pick a wi" hands off to the Work
// view rather than showing the map here. That hand-off is the `selectTab('work')`
// below: App routes the name to the middle column now, not to a right tab
// (tether#90), so this file did not have to change with it.
function CanvasHome() {
  const [ws, setWs] = useState<Workspace[]>([])
  // tether#129 — WHICH workspace this line is about. store.activeWorkspace is the
  // one the rest of the app is pointed at: WorkspacePane sets it, the left file
  // tree lists it, and chatUrl.ts pins a new session's cwd to it. The listing
  // fetched below is only where the NAME comes from — activeWorkspace carries an
  // id and a path and no name.
  const active = useStore((s) => s.activeWorkspace)

  useEffect(() => {
    let alive = true
    fetchWorkspaces()
      .then((d) => { if (alive) setWs(d) })
      .catch(() => { /* home still renders without the workspace line */ })
    return () => { alive = false }
  }, [])

  // This used to be `ws[0]` — the daemon's listing order, which is registration
  // order and has nothing to do with what the user picked. With two workspaces
  // registered and the second one active, the home printed the wrong name over
  // the wrong path and put `· +N more` beside them, which reads as though the one
  // named were the current one and the others merely also present.
  //
  // The `ws[0]` FALLBACK is deliberate and load-bearing, not a leftover: nothing
  // is active until WorkspacePane's fetch settles (store.workspacesLoaded is the
  // gate ChatPane waits on), so it is what the home shows for the first frames of
  // every cold load. It also covers an active id the listing does not contain —
  // activeWorkspace is persisted across runs, so it can name a workspace that has
  // since been removed. Falling back to a real entry beats printing a blank label
  // beside a path from a workspace that no longer exists.
  //
  // Written with the null check outside the predicate rather than as
  // `ws.find(w => w.id === active?.id)`. The short form leans on no entry having
  // an undefined id to make the no-selection case fall through, and this listing
  // is unvalidated JSON off the wire (lib/aihub's getJSON just casts) — an entry
  // missing its id would match `undefined` and be picked as the active one.
  const primary = (active !== null ? ws.find((w) => w.id === active.id) : undefined) ?? ws[0]

  return (
    <div className="canvas-home">
      <div className="canvas-home-brand">
        <Icon name="tether" size={30} />
        <span className="canvas-home-word">tether</span>
      </div>
      {primary && (
        <div className="canvas-home-ws">
          <span className="canvas-home-ws-label">workspace</span> {primary.name}
          {ws.length > 1 && ` · +${ws.length - 1} more`}
          <div className="canvas-home-path mono">{primary.path}</div>
        </div>
      )}
      <div className="canvas-home-actions">
        <button className="canvas-home-action" onClick={() => selectTab('chat')}>
          <span className="canvas-home-glyph">▶</span> Chat
        </button>
        <button className="canvas-home-action" onClick={() => selectTab('work')}>
          <span className="canvas-home-glyph">◱</span> Pick a wi
        </button>
        <button
          className="canvas-home-action"
          onClick={() => window.dispatchEvent(new CustomEvent('tether:focus-files'))}
        >
          <span className="canvas-home-glyph mono">⌘P</span> Open file
        </button>
      </div>
    </div>
  )
}

function FileMode({ wsId, path }: { wsId: string; path: string }) {
  const [data, setData] = useState<{ path: string; content: string; truncated: boolean } | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let alive = true
    setData(null)
    setError(null)
    fetchFile(wsId, path)
      .then((d) => { if (alive) setData(d) })
      .catch((e) => { if (alive) setError(describeError(e)) })
    return () => { alive = false }
  }, [wsId, path])

  const isMd = path.toLowerCase().endsWith('.md')

  return (
    <div className="canvas-view">
      <div className="canvas-head">
        {/* Single line: the full relative path IS the title — no separate
            basename (it just duplicated the path's tail for nested files). */}
        <span className="mono canvas-slug">{path}</span>
      </div>
      {error && <div className="work-error">{error}</div>}
      {!error && !data && <div className="work-empty">loading…</div>}
      {data && (
        <>
          {data.truncated && <div className="canvas-hint">truncated — showing partial content</div>}
          {isMd ? (
            <Suspense fallback={<pre className="canvas-pre mono">{data.content}</pre>}>
              <Markdown text={data.content} />
            </Suspense>
          ) : (
            <pre className="canvas-pre mono">{data.content}</pre>
          )}
        </>
      )}
    </div>
  )
}
