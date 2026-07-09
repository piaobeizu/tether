import { useEffect, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { Icon } from '../../lib/icons'
import { AihubError, fetchEvents, fetchItem, fetchProjects, fetchQueue } from '../../lib/aihub'
import type {
  WorkEvent,
  WorkItemDetail,
  WorkPausedItem,
  WorkProject,
  WorkQueue,
  WorkReadyItem,
  WorkRunningItem,
  WorkStalledItem,
} from '../../lib/wire.gen'

const POLL_MS = 8000

interface Props {
  /** Whether the Work tab is the currently-selected right-pane tab. Polling
   *  (see effect below) runs only while active AND the document is visible. */
  active: boolean
}

function describeError(e: unknown): string {
  if (e instanceof AihubError) {
    if (e.status === 503) return 'aihub not configured'
    if (e.status === 403) return 'not authorized for this project'
    return `error (HTTP ${e.status})`
  }
  return e instanceof Error ? e.message : String(e)
}

function fmtTime(iso: string | undefined): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (isNaN(d.getTime())) return iso
  return d.toLocaleTimeString()
}

function fmtClock(d: Date): string {
  return d.toLocaleTimeString([], { hour12: false })
}

/** True when every LCRS section is empty (project with zero work items). */
function isQueueEmpty(q: WorkQueue): boolean {
  return q.items.length === 0 &&
    q.needsHumanSession.length === 0 &&
    q.unclassified.length === 0 &&
    q.running.length === 0 &&
    (q.staleRunning?.length ?? 0) === 0 &&
    q.stalled.length === 0 &&
    q.paused.length === 0
}

/** Compact one-line preview of an event payload for the timeline. This is a
 *  dedicated renderer — intentionally not the chat fenced-block renderer,
 *  which is scoped to D-19 structured chat output only. */
function payloadPreview(payload: unknown): string {
  if (payload === undefined || payload === null) return ''
  try {
    const s = typeof payload === 'string' ? payload : JSON.stringify(payload)
    return s.length > 140 ? s.slice(0, 140) + '…' : s
  } catch {
    return String(payload)
  }
}

export default function WorkPane({ active }: Props) {
  const [projects, setProjects] = useState<WorkProject[]>([])
  const [projectsError, setProjectsError] = useState<string | null>(null)
  const [project, setProject] = useState<string>('')

  const [queue, setQueue] = useState<WorkQueue | null>(null)
  const [queueError, setQueueError] = useState<string | null>(null)
  const [queueLoading, setQueueLoading] = useState(false)
  const [lastRefreshed, setLastRefreshed] = useState<Date | null>(null)

  const [view, setView] = useState<'queue' | 'detail'>('queue')
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const [item, setItem] = useState<WorkItemDetail | null>(null)
  const [itemError, setItemError] = useState<string | null>(null)

  const [events, setEvents] = useState<WorkEvent[]>([])
  const [eventsCursor, setEventsCursor] = useState<string | undefined>(undefined)
  const [eventsError, setEventsError] = useState<string | null>(null)
  const [eventsLoading, setEventsLoading] = useState(false)

  // Monotonic token for queue fetches: any newer load/refresh/project-switch
  // bumps it, so a slower in-flight fetch that resolves later is ignored
  // instead of clobbering the current project's queue (stale-response guard).
  const queueEpoch = useRef(0)

  // Live mirror of selectedId so async continuations (loadMoreEvents) can test
  // the CURRENT selection after their await — a captured closure would still
  // hold the item that was open when the click fired.
  const selectedIdRef = useRef<string | null>(selectedId)
  useEffect(() => { selectedIdRef.current = selectedId }, [selectedId])

  // ── Projects: load once on mount ────────────────────────────────────
  useEffect(() => {
    let alive = true
    fetchProjects()
      .then(ps => {
        if (!alive) return
        setProjects(ps)
        setProject(prev => prev || ps[0]?.name || '')
        setProjectsError(null)
      })
      .catch(e => { if (alive) setProjectsError(describeError(e)) })
    return () => { alive = false }
  }, [])

  // Manual refresh (button) — also used by the initial per-project load.
  const loadQueue = async (p: string) => {
    if (!p) return
    const epoch = ++queueEpoch.current
    setQueueLoading(true)
    try {
      const q = await fetchQueue(p)
      if (epoch !== queueEpoch.current) return // superseded by a newer load
      setQueue(q)
      setQueueError(null)
      setLastRefreshed(new Date())
    } catch (e) {
      if (epoch !== queueEpoch.current) return
      setQueueError(describeError(e))
    } finally {
      if (epoch === queueEpoch.current) setQueueLoading(false)
    }
  }

  // ── Queue: (re)load whenever the selected project changes; drop any
  // open detail view since it may belong to the previous project. ───────
  useEffect(() => {
    setQueue(null)
    setQueueError(null)
    setView('queue')
    setSelectedId(null)
    if (!project) return
    void loadQueue(project)
  }, [project])

  // ── Polling: only while this tab is active AND the document is visible.
  // A single interval at a time — cleared on tab-switch-away, unmount, and
  // whenever the document goes hidden; restarted on return to visible. ──
  useEffect(() => {
    if (!active || !project) return
    let alive = true
    let timer: ReturnType<typeof setInterval> | null = null

    const tick = () => {
      if (!alive || document.visibilityState !== 'visible') return
      fetchQueue(project)
        .then(q => { if (alive) { setQueue(q); setQueueError(null); setLastRefreshed(new Date()) } })
        .catch(e => { if (alive) setQueueError(describeError(e)) })
    }

    const startTimer = () => {
      if (timer !== null) return
      timer = setInterval(tick, POLL_MS)
    }
    const stopTimer = () => {
      if (timer !== null) { clearInterval(timer); timer = null }
    }

    if (document.visibilityState === 'visible') startTimer()

    const onVisibility = () => {
      if (document.visibilityState === 'visible') {
        tick()
        startTimer()
      } else {
        stopTimer()
      }
    }
    document.addEventListener('visibilitychange', onVisibility)

    return () => {
      alive = false
      stopTimer()
      document.removeEventListener('visibilitychange', onVisibility)
    }
  }, [active, project])

  // ── Item detail + timeline ───────────────────────────────────────────
  const openItem = (id: string) => {
    setSelectedId(id)
    setView('detail')
    setItem(null)
    setItemError(null)
    setEvents([])
    setEventsCursor(undefined)
    setEventsError(null)
  }

  useEffect(() => {
    if (!selectedId) return
    let alive = true

    fetchItem(selectedId)
      .then(d => { if (alive) setItem(d) })
      .catch(e => { if (alive) setItemError(describeError(e)) })

    setEventsLoading(true)
    fetchEvents(selectedId)
      .then(ev => { if (alive) { setEvents(ev.events); setEventsCursor(ev.nextCursor) } })
      .catch(e => { if (alive) setEventsError(describeError(e)) })
      .finally(() => { if (alive) setEventsLoading(false) })

    return () => { alive = false }
  }, [selectedId])

  const loadMoreEvents = async () => {
    if (!selectedId || !eventsCursor) return
    const reqId = selectedId
    setEventsLoading(true)
    try {
      const ev = await fetchEvents(reqId, eventsCursor)
      if (reqId !== selectedIdRef.current) return // switched items mid-flight; drop
      setEvents(prev => [...prev, ...ev.events])
      setEventsCursor(ev.nextCursor)
      setEventsError(null)
    } catch (e) {
      if (reqId !== selectedIdRef.current) return
      setEventsError(describeError(e))
    } finally {
      if (reqId === selectedIdRef.current) setEventsLoading(false)
    }
  }

  const backToQueue = () => {
    setView('queue')
    setSelectedId(null)
  }

  // ── Detail view ───────────────────────────────────────────────────────
  if (view === 'detail') {
    return (
      <div className="work-pane">
        <div className="work-detail-head">
          <button className="icon-btn-sm" onClick={backToQueue} title="Back to queue" aria-label="Back to queue">
            <Icon name="back" size={14} />
          </button>
          <span className="mono work-detail-id">{selectedId}</span>
        </div>
        <div className="work-detail-body scroll-thin">
          {itemError && <div className="work-error">{itemError}</div>}
          {!itemError && !item && <div className="work-empty">loading…</div>}
          {item && (
            <>
              <div className="work-detail-goal">{item.goal}</div>
              <div className="work-detail-meta">
                <span className="work-badge">{item.status}</span>
                <span className="work-badge">{item.priority}</span>
                {item.wiType && <span className="work-badge">{item.wiType}</span>}
              </div>
              <div className="work-detail-row">
                <span className="work-detail-k">current step</span>
                <span className="work-detail-v mono">
                  {item.currentStep || '—'}{' '}
                  <span style={{ color: 'var(--ink-tertiary)' }}>({item.currentStepStatus})</span>
                </span>
              </div>
              {item.labels.length > 0 && (
                <div className="work-labels">
                  {item.labels.map(l => <span key={l} className="work-label">{l}</span>)}
                </div>
              )}
              {item.content && (
                <div className="work-detail-content mono">{item.content}</div>
              )}

              <div className="section-label work-timeline-head">Timeline</div>
              {eventsError && <div className="work-error">{eventsError}</div>}
              <div className="work-timeline">
                {events.map((ev, i) => (
                  <div key={`${ev.ts}-${i}`} className="work-event-row">
                    <span className="mono work-event-ts">{fmtTime(ev.ts)}</span>
                    <span className="work-event-type">{ev.type}</span>
                    {ev.payload !== undefined && (
                      <span className="mono work-event-payload">{payloadPreview(ev.payload)}</span>
                    )}
                  </div>
                ))}
                {events.length === 0 && !eventsLoading && !eventsError && (
                  <div className="work-empty">no events</div>
                )}
              </div>
              {eventsCursor && (
                <button className="btn-ghost-sm" disabled={eventsLoading} onClick={() => void loadMoreEvents()}>
                  {eventsLoading ? 'loading…' : 'load more'}
                </button>
              )}
            </>
          )}
        </div>
      </div>
    )
  }

  // ── Queue view ────────────────────────────────────────────────────────
  return (
    <div className="work-pane">
      <div className="work-head">
        <select
          className="work-project-select"
          value={project}
          onChange={e => setProject(e.target.value)}
          disabled={projects.length === 0}
        >
          {projects.length === 0 && <option value="">no projects</option>}
          {projects.map(p => <option key={p.name} value={p.name}>{p.name}</option>)}
        </select>
        <button className="icon-btn-sm" title="Refresh" aria-label="Refresh queue" onClick={() => void loadQueue(project)}>
          <Icon name="bolt" size={13} />
        </button>
      </div>

      <div className="work-body scroll-thin">
        {projectsError && <div className="work-error">{projectsError}</div>}
        {/* Non-destructive: a transient poll failure surfaces here but never
            blanks the cached queue rendered below. */}
        {queueError && <div className="work-error">{queueError}</div>}
        {queue && (
          isQueueEmpty(queue) ? (
            <div className="work-empty">no work items</div>
          ) : (
            <>
              <ReadySection title="Ready" items={queue.items} onOpen={openItem} />
              <ReadySection title="Needs human" items={queue.needsHumanSession} onOpen={openItem} />
              <ReadySection title="Unclassified" items={queue.unclassified} onOpen={openItem} />
              <RunningSection title="Running" items={queue.running} stale={queue.staleRunning} onOpen={openItem} />
              <StalledSection items={queue.stalled} onOpen={openItem} />
              <PausedSection items={queue.paused} onOpen={openItem} />
            </>
          )
        )}
        {!queue && !queueError && queueLoading && <div className="work-empty">loading…</div>}
        {!queue && !queueError && !queueLoading && <div className="work-empty">select a project</div>}
      </div>

      <div className="work-foot mono">
        {lastRefreshed ? `last refreshed ${fmtClock(lastRefreshed)}` : queueLoading ? 'loading…' : ''}
      </div>
    </div>
  )
}

// ── Section renderers ────────────────────────────────────────────────────

function Section({ title, count, children }: { title: string; count: number; children: ReactNode }) {
  if (count === 0) return null
  return (
    <div className="work-section">
      <div className="section-label work-section-head">{title} <span className="work-section-count">{count}</span></div>
      {children}
    </div>
  )
}

function ReadySection({ title, items, onOpen }: { title: string; items: WorkReadyItem[]; onOpen: (id: string) => void }) {
  return (
    <Section title={title} count={items.length}>
      {items.map(it => (
        <div key={it.id} className="work-row" onClick={() => onOpen(it.id)}>
          <div className="work-row-top">
            {it.wiType && <span className="work-badge">{it.wiType}</span>}
            <span className="work-badge">{it.priority}</span>
          </div>
          <div className="work-row-goal">{it.goal}</div>
        </div>
      ))}
    </Section>
  )
}

function RunningSection(
  { title, items, stale, onOpen }:
  { title: string; items: WorkRunningItem[]; stale?: WorkRunningItem[]; onOpen: (id: string) => void }
) {
  const total = items.length + (stale?.length ?? 0)
  return (
    <Section title={title} count={total}>
      {items.map(it => (
        <div key={it.id} className="work-row" onClick={() => onOpen(it.id)}>
          <div className="work-row-goal">{it.goal}</div>
          <div className="work-row-sub">{it.ownerDisplay} · {fmtTime(it.lastActiveAt)}</div>
        </div>
      ))}
      {stale?.map(it => (
        <div key={it.id} className="work-row" onClick={() => onOpen(it.id)}>
          <div className="work-row-top">
            <span className="work-badge work-badge-warn">stale</span>
          </div>
          <div className="work-row-goal">{it.goal}</div>
          <div className="work-row-sub">{it.ownerDisplay} · {fmtTime(it.lastActiveAt)}</div>
        </div>
      ))}
    </Section>
  )
}

function StalledSection({ items, onOpen }: { items: WorkStalledItem[]; onOpen: (id: string) => void }) {
  return (
    <Section title="Stalled" count={items.length}>
      {items.map(it => (
        <div key={it.id} className="work-row" onClick={() => onOpen(it.id)}>
          <div className="work-row-goal">{it.stallReason}</div>
          <div className="work-row-sub">{it.lastActorDisplay} · since {fmtTime(it.stalledSince)}</div>
        </div>
      ))}
    </Section>
  )
}

function PausedSection({ items, onOpen }: { items: WorkPausedItem[]; onOpen: (id: string) => void }) {
  return (
    <Section title="Paused" count={items.length}>
      {items.map(it => (
        <div key={it.id} className="work-row" onClick={() => onOpen(it.id)}>
          <div className="work-row-goal">{it.pauseReason || '(no reason given)'}</div>
          <div className="work-row-sub">{it.lastActorDisplay} · since {fmtTime(it.pausedSince)}</div>
        </div>
      ))}
    </Section>
  )
}
