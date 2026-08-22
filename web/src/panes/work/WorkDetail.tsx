// WorkDetail — right-pane detail for the selected wi (tether#23). Migrated
// from the old middle-canvas DetailMode (tether#20/#21): the wi's goal /
// status / content (markdown) plus its scenario step DAG (dagre, TB), and
// the click-to-work action bar (formerly in WorkPane). The relationship
// knowledge-graph now lives in the middle canvas (WorkGraphView); selecting
// a node there routes here via store.selectedWiId.
import { lazy, Suspense, useEffect, useState } from 'react'
import { describeError, fetchItem, fetchSteps } from '../../lib/aihub'
import type { WorkItemDetail, WorkSteps } from '../../lib/wire.gen'
import { openSession } from '../../lib/session'
import { SessionRow } from '../../lib/SessionRow'
import {
  WI_BOUND_EVENT,
  bindWorkItem,
  fetchSessions,
  sessionsForWorkItem,
  type SessionSummary,
} from '../../lib/wiSession'
import { Dag } from './Dag'
import type { DagEdge, DagNode } from './Dag'
import EventTimeline from './EventTimeline'

const Markdown = lazy(() => import('../canvas/Markdown'))

// Build scenario-step edges for the DAG. Prefer the real prev-derived edges;
// when a degraded steps response omits prev (e.g. global-routing#62
// port_feature returns only touched steps with no prev), synthesize a
// record-order chain so the graph renders as a connected line instead of
// unsorted, edgeless nodes. Guard: fall back ONLY when there are no
// prev-derived edges, so a real DAG is never overwritten by the chain.
export function buildStepEdges(nodes: { id: string; prev?: string[] }[]): DagEdge[] {
  const prevEdges = nodes.flatMap((n) =>
    (n.prev ?? []).map((p) => ({ from: p, to: n.id, kind: 'step' as const })),
  )
  if (prevEdges.length > 0) return prevEdges
  if (nodes.length >= 2) {
    return nodes.slice(1).map((n, i) => ({ from: nodes[i].id, to: n.id, kind: 'step' as const }))
  }
  return []
}

export default function WorkDetail({ id }: { id: string }) {
  const [item, setItem] = useState<WorkItemDetail | null>(null)
  const [itemError, setItemError] = useState<string | null>(null)
  const [steps, setSteps] = useState<WorkSteps | null>(null)
  const [stepsError, setStepsError] = useState<string | null>(null)
  // tether#91 — every session bound to this wi, newest first. One-to-many comes
  // free from storing session → wi: these are simply the rows of the session list
  // whose record names this slug, so no endpoint and no index were needed for it.
  const [sessions, setSessions] = useState<SessionSummary[]>([])
  // Whether the list above is an ANSWER or merely its initial value. The
  // distinction is load-bearing for resumeWi: an empty `sessions` that has not
  // been fetched yet means "unknown", and treating it as "no session" would make
  // a fast click on "Open in chat" silently do the not-bound thing. The old
  // localStorage read had no such window because it was synchronous.
  const [sessionsLoaded, setSessionsLoaded] = useState(false)

  useEffect(() => {
    let alive = true
    setItem(null)
    setItemError(null)
    setSteps(null)
    setStepsError(null)
    setSessions([])
    setSessionsLoaded(false)

    fetchItem(id)
      .then((d) => { if (alive) setItem(d) })
      .catch((e) => { if (alive) setItemError(describeError(e)) })

    fetchSteps(id)
      .then((s) => { if (alive) setSteps(s) })
      .catch((e) => { if (alive) setStepsError(describeError(e)) })

    return () => { alive = false }
  }, [id])

  // Keyed on the SLUG, not on `id`: the slug is what a binding records (it is
  // what a human reads and what /pf-work takes), and it only becomes known once
  // the item above has loaded. Refetched on tether:wi-bound so a Start click
  // shows up here without a reload.
  const slug = item?.slug
  useEffect(() => {
    if (!slug) return
    let alive = true
    const reload = () => {
      fetchSessions()
        .then((rows) => { if (alive) setSessions(sessionsForWorkItem(rows, slug)) })
        // A session list we could not fetch is not an error worth showing on a wi
        // page — the action bar falls back to injecting the resume prompt, which
        // is what it did for every wi before this existed.
        .catch(() => { if (alive) setSessions([]) })
        // Settled either way: a failed fetch is still an answer for resumeWi's
        // purposes, and leaving this false would make every click re-ask.
        .finally(() => { if (alive) setSessionsLoaded(true) })
    }
    reload()
    window.addEventListener(WI_BOUND_EVENT, reload)
    return () => { alive = false; window.removeEventListener(WI_BOUND_EVENT, reload) }
  }, [slug])

  const dagNodes: DagNode[] = (steps?.nodes ?? []).map((n) => ({ id: n.id, label: n.id, status: n.status }))
  const dagEdges: DagEdge[] = buildStepEdges(steps?.nodes ?? [])

  // queued/unclaimed → not started (offer "▶ Start"); else offer "→ Open in chat".
  const isUnstarted = !item?.status || item.status === 'queued' || item.status === 'pending'

  const startWi = () => {
    if (!item) return
    const slug = item.slug
    window.dispatchEvent(new CustomEvent('tether:select-tab', { detail: 'chat' }))
    window.dispatchEvent(new CustomEvent('tether:inject-prompt', { detail: `/pf-work ${slug}` }))
    // tether#91 — the binding is recorded on the DAEMON, keyed by session, and it
    // is not recorded at all when there is no session yet (bindWorkItem waits for
    // one). This line used to be
    // `localStorage.setItem('tether_wi_sid:' + slug, sessionId ?? '')`, which lost
    // the mapping on any other device and wrote a real-looking empty mapping when
    // there was no session — after which "Open in chat" silently fell back to
    // re-injecting the prompt forever.
    bindWorkItem(slug)
  }

  const resumeWi = async () => {
    if (!item) return
    const slug = item.slug
    window.dispatchEvent(new CustomEvent('tether:select-tab', { detail: 'chat' }))

    // Ask now if the list has not answered yet, rather than reading an empty
    // initial value as "nothing is bound". The binding used to be a synchronous
    // localStorage read, so a click a few milliseconds after the pane appeared
    // could not miss it; a fetch can, and the failure would look exactly like a
    // wi that was never started.
    let bound = sessions
    if (!sessionsLoaded) {
      try {
        bound = sessionsForWorkItem(await fetchSessions(), slug)
      } catch {
        bound = []
      }
    }

    // Newest first is the daemon's order, so bound[0] is the most recent session
    // that worked on this wi. With the mapping inverted a wi can have several;
    // opening the latest is the useful default and the rest are listed below for
    // anyone who wants an older one.
    if (bound.length > 0) {
      // tether#61 — one shared implementation of "open that session", called
      // directly. This used to go out as a `tether:switch-session` event that
      // ChatPane picked up and handled its own way; the workspace session list
      // handled it a third, broken way. Now there is nothing to keep in sync.
      openSession(bound[0].sid)
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
              ? <button className="btn-primary-sm" onClick={startWi}>▶ Start</button>
              : <button className="btn-ghost-sm" onClick={() => void resumeWi()}>→ Open in chat</button>}
          </div>

          {item.content && (
            <div className="canvas-section">
              <div className="section-label canvas-section-head">Content</div>
              <Suspense fallback={<pre className="canvas-pre mono">{item.content}</pre>}>
                <Markdown text={item.content} />
              </Suspense>
            </div>
          )}

          {/* tether#91 — every session that has worked on this wi, newest first.
              This is what inverting the mapping bought: the old forward mapping
              held ONE sid per wi in this browser's localStorage, so a second
              session on the same wi overwrote the first and neither survived a
              change of device. Rendered only when there is at least one, so a wi
              nobody has started looks exactly as it did before. */}
          {sessions.length > 0 && (
            <div className="canvas-section">
              <div className="section-label canvas-section-head">Sessions</div>
              {/* The same row the chat pane renders — including marking the one
                  you are already in, and switching to the chat tab on click,
                  neither of which the hand-written copy this replaces did.
                  omitWorkItem because THIS PAGE is the work item. */}
              {sessions.map((s) => (
                <SessionRow key={s.sid} session={s} omitWorkItem />
              ))}
            </div>
          )}

          <div className="canvas-section">
            <div className="section-label canvas-section-head">Scenario steps</div>
            {stepsError && <div className="work-error">{stepsError}</div>}
            {steps?.degraded && dagNodes.length > 0 && (
              <div className="canvas-hint">step order approximate (degraded)</div>
            )}
            {steps?.degraded && dagNodes.length === 0 && (
              <div className="canvas-hint">scenario steps unavailable (no scenario clone)</div>
            )}
            {!steps && !stepsError && <div className="work-empty">loading…</div>}
            {steps && !steps.degraded && dagNodes.length === 0 && !stepsError && <div className="work-empty">no steps</div>}
            {steps && dagNodes.length > 0 && (
              <div className="canvas-dag-wrap">
                <Dag nodes={dagNodes} edges={dagEdges} direction="LR" />
              </div>
            )}
          </div>

          <div className="canvas-section">
            <div className="section-label canvas-section-head">Activity</div>
            <EventTimeline id={id} />
          </div>
        </>
      )}
    </div>
  )
}
