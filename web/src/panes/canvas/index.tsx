// Canvas — middle-pane artifact viewer (tether#20 Task 10/11).
//
// Reads the shared selection slice from the store (lib/store.ts `select`)
// and renders one of three modes:
//   - detail mode: a work item's goal/status/content + its scenario step DAG
//   - file mode: a workspace file's content (from WorkspaceTree)
//   - empty: neither selected — the original "no artifacts yet" placeholder
//
// No markdown renderer exists yet anywhere in web/src (no marked/remark/etc.
// dependency, no <Markdown> component) — content is rendered as preformatted
// text per the task's fallback guidance.
import { useEffect, useState } from 'react'
import { Icon } from '../../lib/icons'
import { useStore } from '../../lib/store'
import { AihubError, fetchFile, fetchItem, fetchSteps } from '../../lib/aihub'
import type { WorkItemDetail, WorkSteps } from '../../lib/wire.gen'
import { Dag } from '../work/Dag'
import type { DagEdge, DagNode } from '../work/Dag'

function describeError(e: unknown): string {
  if (e instanceof AihubError) {
    if (e.status === 503) return 'aihub not configured'
    if (e.status === 403) return 'not authorized for this project'
    if (e.status === 404) return 'not found'
    return `error (HTTP ${e.status})`
  }
  return e instanceof Error ? e.message : String(e)
}

/** Preformatted-text fallback used for both markdown and plain file content —
 *  see module doc: no markdown renderer exists in this repo yet. */
function Prose({ text }: { text: string }) {
  return <pre className="canvas-pre mono">{text}</pre>
}

export default function Canvas() {
  const selectedWiId = useStore(s => s.selectedWiId)
  const selectedFile = useStore(s => s.selectedFile)

  if (selectedWiId) return <DetailMode id={selectedWiId} />
  if (selectedFile) return <FileMode wsId={selectedFile.wsId} path={selectedFile.path} />

  return (
    <div className="dt-mid-empty">
      <Icon name="folder-open" size={26} />
      <div className="serif dt-mid-empty-title">no artifacts yet</div>
      <div className="dt-mid-empty-sub">
        Files, diffs, and skill output from this workspace show up here as tether works.
      </div>
    </div>
  )
}

function DetailMode({ id }: { id: string }) {
  const [item, setItem] = useState<WorkItemDetail | null>(null)
  const [itemError, setItemError] = useState<string | null>(null)
  const [steps, setSteps] = useState<WorkSteps | null>(null)
  const [stepsError, setStepsError] = useState<string | null>(null)

  useEffect(() => {
    let alive = true
    setItem(null)
    setItemError(null)
    setSteps(null)
    setStepsError(null)

    fetchItem(id)
      .then(d => { if (alive) setItem(d) })
      .catch(e => { if (alive) setItemError(describeError(e)) })

    fetchSteps(id)
      .then(s => { if (alive) setSteps(s) })
      .catch(e => { if (alive) setStepsError(describeError(e)) })

    return () => { alive = false }
  }, [id])

  const dagNodes: DagNode[] = (steps?.nodes ?? []).map(n => ({ id: n.id, label: n.id, status: n.status }))
  const dagEdges: DagEdge[] = (steps?.nodes ?? []).flatMap(n =>
    (n.prev ?? []).map(p => ({ from: p, to: n.id, kind: 'step' as const }))
  )

  return (
    <div className="canvas-view">
      {itemError && <div className="work-error">{itemError}</div>}
      {!itemError && !item && <div className="work-empty">loading…</div>}
      {item && (
        <>
          <div className="canvas-head">
            <span className="mono canvas-slug">{item.slug}</span>
            <div className="canvas-goal">{item.goal}</div>
            <div className="canvas-meta">
              <span className="work-badge">{item.status}</span>
              <span className="work-badge">{item.priority}</span>
              {item.wiType && <span className="work-badge">{item.wiType}</span>}
            </div>
            {item.labels.length > 0 && (
              <div className="work-labels">
                {item.labels.map(l => <span key={l} className="work-label">{l}</span>)}
              </div>
            )}
          </div>

          {item.content && (
            <div className="canvas-section">
              <div className="section-label canvas-section-head">Content</div>
              <Prose text={item.content} />
            </div>
          )}

          <div className="canvas-section">
            <div className="section-label canvas-section-head">Scenario steps</div>
            {stepsError && <div className="work-error">{stepsError}</div>}
            {steps?.degraded && (
              <div className="canvas-hint">scenario steps unavailable (no scenario clone)</div>
            )}
            {!steps && !stepsError && <div className="work-empty">loading…</div>}
            {steps && dagNodes.length === 0 && !stepsError && <div className="work-empty">no steps</div>}
            {steps && dagNodes.length > 0 && (
              <div className="canvas-dag-wrap">
                <Dag nodes={dagNodes} edges={dagEdges} direction="LR" />
              </div>
            )}
          </div>
        </>
      )}
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
      .then(d => { if (alive) setData(d) })
      .catch(e => { if (alive) setError(describeError(e)) })
    return () => { alive = false }
  }, [wsId, path])

  const isMd = path.toLowerCase().endsWith('.md')
  const filename = path.split('/').pop() || path

  return (
    <div className="canvas-view">
      <div className="canvas-head">
        <span className="mono canvas-slug">{filename}</span>
        <div className="canvas-file-path mono">{path}</div>
      </div>
      {error && <div className="work-error">{error}</div>}
      {!error && !data && <div className="work-empty">loading…</div>}
      {data && (
        <>
          {data.truncated && <div className="canvas-hint">truncated — showing partial content</div>}
          {isMd ? <Prose text={data.content} /> : <pre className="canvas-pre mono">{data.content}</pre>}
        </>
      )}
    </div>
  )
}
