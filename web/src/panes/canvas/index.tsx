// Canvas — middle-pane view (tether#23).
//
// Reads the shared selection slice from the store (lib/store.ts) and renders:
//   - file mode: a workspace file's content (from the left WorkspaceTree)
//   - otherwise: the wi relationship knowledge-graph (WorkGraphView) for the
//     current Work project — the always-present "project map".
//
// The old wi-detail mode (goal/status/content + step DAG) moved to the right
// pane (WorkDetail) in the tether#23 panel inversion. Markdown file rendering
// (tether#21) stays: `.md` files render via the lazy-loaded <Markdown>
// component (react-markdown + remark-gfm); non-markdown files keep the plain
// <pre> fallback.
import { lazy, Suspense, useEffect, useState } from 'react'
import { useStore } from '../../lib/store'
import { AihubError, fetchFile } from '../../lib/aihub'
import WorkGraphView from '../work/WorkGraphView'

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
  return <WorkGraphView />
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
