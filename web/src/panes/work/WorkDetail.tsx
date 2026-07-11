// WorkDetail — right-pane detail for the selected wi (tether#23). Migrated
// from the old middle-canvas DetailMode (tether#20/#21): the wi's goal /
// status / content (markdown) plus its scenario step DAG (dagre, TB), and
// the click-to-work action bar (formerly in WorkPane). The relationship
// knowledge-graph now lives in the middle canvas (WorkGraphView); selecting
// a node there routes here via store.selectedWiId.
import { lazy, Suspense, useEffect, useState } from 'react'
import { AihubError, fetchItem, fetchSteps } from '../../lib/aihub'
import type { WorkItemDetail, WorkSteps } from '../../lib/wire.gen'
import { useStore } from '../../lib/store'
import { Dag } from './Dag'
import type { DagEdge, DagNode } from './Dag'

const Markdown = lazy(() => import('../canvas/Markdown'))

function describeError(e: unknown): string {
  if (e instanceof AihubError) {
    if (e.status === 503) return 'aihub not configured'
    if (e.status === 403) return 'not authorized for this project'
    if (e.status === 404) return 'not found'
    return `error (HTTP ${e.status})`
  }
  return e instanceof Error ? e.message : String(e)
}

export default function WorkDetail({ id }: { id: string }) {
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
      .then((d) => { if (alive) setItem(d) })
      .catch((e) => { if (alive) setItemError(describeError(e)) })

    fetchSteps(id)
      .then((s) => { if (alive) setSteps(s) })
      .catch((e) => { if (alive) setStepsError(describeError(e)) })

    return () => { alive = false }
  }, [id])

  const dagNodes: DagNode[] = (steps?.nodes ?? []).map((n) => ({ id: n.id, label: n.id, status: n.status }))
  const dagEdges: DagEdge[] = (steps?.nodes ?? []).flatMap((n) =>
    (n.prev ?? []).map((p) => ({ from: p, to: n.id, kind: 'step' as const })),
  )

  // queued/unclaimed → not started (offer ▶开始做); else offer →进入 chat.
  const isUnstarted = !item?.status || item.status === 'queued' || item.status === 'pending'

  const startWi = () => {
    if (!item) return
    const slug = item.slug
    window.dispatchEvent(new CustomEvent('tether:select-tab', { detail: 'chat' }))
    window.dispatchEvent(new CustomEvent('tether:inject-prompt', { detail: `/pf-work ${slug}` }))
    localStorage.setItem('tether_wi_sid:' + slug, useStore.getState().sessionId ?? '')
  }

  const resumeWi = () => {
    if (!item) return
    const slug = item.slug
    const sid = localStorage.getItem('tether_wi_sid:' + slug)
    window.dispatchEvent(new CustomEvent('tether:select-tab', { detail: 'chat' }))
    if (sid) {
      window.dispatchEvent(new CustomEvent('tether:switch-session', { detail: sid }))
    } else {
      window.dispatchEvent(new CustomEvent('tether:inject-prompt', { detail: `/pf-work ${slug} --resume` }))
    }
  }

  return (
    <div className="canvas-view work-detail-view scroll-thin">
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
                {item.labels.map((l) => <span key={l} className="work-label">{l}</span>)}
              </div>
            )}
          </div>

          <div className="work-action-bar">
            {isUnstarted
              ? <button className="btn-primary-sm" onClick={startWi}>▶ 开始做</button>
              : <button className="btn-ghost-sm" onClick={resumeWi}>→ 进入 chat</button>}
          </div>

          {item.content && (
            <div className="canvas-section">
              <div className="section-label canvas-section-head">Content</div>
              <Suspense fallback={<pre className="canvas-pre mono">{item.content}</pre>}>
                <Markdown text={item.content} />
              </Suspense>
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
                <Dag nodes={dagNodes} edges={dagEdges} direction="TB" />
              </div>
            )}
          </div>
        </>
      )}
    </div>
  )
}
