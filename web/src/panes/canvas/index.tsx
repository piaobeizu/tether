// Canvas — middle-pane view.
//
// Reads the shared selection slice from the store (lib/store.ts) and renders a
// workspace file's content when one is selected (from the left WorkspaceTree),
// otherwise an empty-state hint. The Work relationship map moved OUT of the
// middle into the right Work tab in tether#26 (owner found the middle map
// cluttered) — the middle is now the file / main-work area only.
//
// Markdown file rendering (tether#21) stays: `.md` files render via the
// lazy-loaded <Markdown> component (react-markdown + remark-gfm); non-markdown
// files keep the plain <pre> fallback.
import { lazy, Suspense, useEffect, useState } from 'react'
import { useStore } from '../../lib/store'
import { AihubError, fetchFile, fetchWorkspaces, type Workspace } from '../../lib/aihub'
import { Icon } from '../../lib/icons'

const Markdown = lazy(() => import('./Markdown'))

function describeError(e: unknown): string {
  if (e instanceof AihubError) {
    if (e.status === 503) return 'aihub not configured'
    if (e.status === 403) return 'not authorized for this project'
    if (e.status === 404) return 'not found'
    return `error (HTTP ${e.status})`
  }
  return e instanceof Error ? e.message : String(e)
}

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
// workspace's name/path, and three quick-action entries. The Work relationship
// map stays in the right tab (tether#26); this is a welcoming landing surface,
// not a data dashboard.
function CanvasHome() {
  const [ws, setWs] = useState<Workspace[]>([])

  useEffect(() => {
    let alive = true
    fetchWorkspaces()
      .then((d) => { if (alive) setWs(d) })
      .catch(() => { /* home still renders without the workspace line */ })
    return () => { alive = false }
  }, [])

  const primary = ws[0]

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
          <span className="canvas-home-glyph">▶</span> 对话
        </button>
        <button className="canvas-home-action" onClick={() => selectTab('work')}>
          <span className="canvas-home-glyph">◱</span> 选个 wi
        </button>
        <button
          className="canvas-home-action"
          onClick={() => window.dispatchEvent(new CustomEvent('tether:focus-files'))}
        >
          <span className="canvas-home-glyph mono">⌘P</span> 打开文件
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
