import { useEffect, useRef, useState } from 'react'
import { Icon } from '../../lib/icons'
import { useStore } from '../../lib/store'
import { AihubError, fetchDeps, fetchGraph, fetchProjects } from '../../lib/aihub'
import type { WorkGraph, WorkGraphNode, WorkProject } from '../../lib/wire.gen'
import { Dag } from './Dag'
import type { DagEdge, DagNode } from './Dag'

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

function fmtClock(d: Date): string {
  return d.toLocaleTimeString([], { hour12: false })
}

export default function WorkPane({ active }: Props) {
  const [projects, setProjects] = useState<WorkProject[]>([])
  const [projectsError, setProjectsError] = useState<string | null>(null)
  const [project, setProject] = useState<string>('')

  const [graph, setGraph] = useState<WorkGraph | null>(null)
  const [graphError, setGraphError] = useState<string | null>(null)
  const [graphLoading, setGraphLoading] = useState(false)
  const [lastRefreshed, setLastRefreshed] = useState<Date | null>(null)

  // Block-edge overlay lazily fetched for the currently-selected node (fetchDeps).
  // Cleared whenever the graph reloads, since it belongs to the prior fetch.
  const [blockEdges, setBlockEdges] = useState<DagEdge[]>([])
  const depsEpoch = useRef(0)

  const selectedWiId = useStore(s => s.selectedWiId)
  const select = useStore(s => s.select)

  // Monotonic token for graph fetches: any newer load/refresh/project-switch
  // bumps it, so a slower in-flight fetch that resolves later is ignored
  // instead of clobbering the current project's graph (stale-response guard).
  const graphEpoch = useRef(0)

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
  const loadGraph = async (p: string) => {
    if (!p) return
    const epoch = ++graphEpoch.current
    setGraphLoading(true)
    try {
      const g = await fetchGraph(p)
      if (epoch !== graphEpoch.current) return // superseded by a newer load
      setGraph(g)
      setGraphError(null)
      setBlockEdges([])
      setLastRefreshed(new Date())
    } catch (e) {
      if (epoch !== graphEpoch.current) return
      setGraphError(describeError(e))
    } finally {
      if (epoch === graphEpoch.current) setGraphLoading(false)
    }
  }

  // ── Graph: (re)load whenever the selected project changes ────────────
  useEffect(() => {
    setGraph(null)
    setGraphError(null)
    setBlockEdges([])
    select(null) // don't keep showing the previous project's wi/file in Canvas
    if (!project) return
    void loadGraph(project)
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
      const epoch = graphEpoch.current
      fetchGraph(project)
        .then(g => {
          if (!alive || epoch !== graphEpoch.current) return // superseded by a newer load
          setGraph(g)
          setGraphError(null)
          setLastRefreshed(new Date())
        })
        .catch(e => { if (alive && epoch === graphEpoch.current) setGraphError(describeError(e)) })
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

  // ── Node selection: route to the shared canvas selection + lazily overlay
  // this node's block edges (fetchDeps), mapped onto the currently-loaded
  // node set (edges to ids outside it are dropped rather than guessed). ──
  const onSelectNode = (id: string) => {
    select({ wiId: id })
    const epoch = ++depsEpoch.current
    const nodeIds = new Set((graph?.nodes ?? []).map(n => n.id))
    fetchDeps(id)
      .then(deps => {
        if (epoch !== depsEpoch.current) return
        const edges: DagEdge[] = []
        for (const b of deps.blockedBy) {
          if (nodeIds.has(b.id)) edges.push({ from: b.id, to: id, kind: 'block' })
        }
        for (const b of deps.blocking) {
          if (nodeIds.has(b.id)) edges.push({ from: id, to: b.id, kind: 'block' })
        }
        setBlockEdges(edges)
      })
      .catch(() => { /* deps overlay is best-effort; leave prior state as-is */ })
  }

  const dagNodes: DagNode[] = (graph?.nodes ?? []).map(n => ({
    id: n.id,
    label: n.slug,
    status: n.status,
    sub: n.wiType,
  }))
  const parentEdges: DagEdge[] = (graph?.nodes ?? [])
    .filter((n): n is WorkGraphNode & { parent: string } => !!n.parent)
    .map(n => ({ from: n.parent, to: n.id, kind: 'parent' as const }))
  const dagEdges = [...parentEdges, ...blockEdges]

  const isEmpty = graph !== null && graph.nodes.length === 0

  // T12 click-to-work (tether#20) — the selected node carries its own
  // `status`, already fetched as part of the graph (no extra request).
  const selectedNode = (graph?.nodes ?? []).find(n => n.id === selectedWiId) ?? null

  // queued/unclaimed (or an unrecognized/empty status) → not started yet;
  // anything else (running/paused/done/failed/…) → a session may already
  // exist for it, so offer to jump into chat instead. v1 heuristic, not
  // exhaustive over every status the daemon may report — good enough since
  // "jump to chat" is a harmless action regardless of terminal state.
  const isUnstarted = !selectedNode?.status || selectedNode.status === 'queued' || selectedNode.status === 'pending'

  const startWi = () => {
    if (!selectedNode) return
    const slug = selectedNode.slug
    window.dispatchEvent(new CustomEvent('tether:select-tab', { detail: 'chat' }))
    window.dispatchEvent(new CustomEvent('tether:inject-prompt', { detail: `/pf-work ${slug}` }))
    // Best-effort mapping so a later "→ 进入 chat" click can jump straight
    // back to this session — only known for wis started via this browser.
    localStorage.setItem('tether_wi_sid:' + slug, useStore.getState().sessionId ?? '')
  }

  const resumeWi = () => {
    if (!selectedNode) return
    const slug = selectedNode.slug
    const sid = localStorage.getItem('tether_wi_sid:' + slug)
    window.dispatchEvent(new CustomEvent('tether:select-tab', { detail: 'chat' }))
    if (sid) {
      window.dispatchEvent(new CustomEvent('tether:switch-session', { detail: sid }))
    } else {
      window.dispatchEvent(new CustomEvent('tether:inject-prompt', { detail: `/pf-work ${slug} --resume` }))
    }
  }

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
        <button className="icon-btn-sm" title="Refresh" aria-label="Refresh graph" onClick={() => void loadGraph(project)}>
          <Icon name="bolt" size={13} />
        </button>
      </div>

      {selectedNode && (
        <div className="work-action-bar">
          <span className="work-action-bar-slug mono">{selectedNode.slug}</span>
          <span style={{ flex: 1 }} />
          {isUnstarted
            ? <button className="btn-primary-sm" onClick={startWi}>▶ 开始做</button>
            : <button className="btn-ghost-sm" onClick={resumeWi}>→ 进入 chat</button>}
        </div>
      )}

      <div className="work-body scroll-thin">
        {projectsError && <div className="work-error">{projectsError}</div>}
        {/* Non-destructive: a transient poll failure surfaces here but never
            blanks the cached graph rendered below. */}
        {graphError && <div className="work-error">{graphError}</div>}
        {graph && isEmpty && <div className="work-empty">no active work items</div>}
        {graph && !isEmpty && (
          <div className="work-dag-wrap">
            <Dag
              nodes={dagNodes}
              edges={dagEdges}
              selectedId={selectedWiId ?? undefined}
              onSelect={onSelectNode}
            />
          </div>
        )}
        {!graph && !graphError && graphLoading && <div className="work-empty">loading…</div>}
        {!graph && !graphError && !graphLoading && <div className="work-empty">select a project</div>}
      </div>

      <div className="work-foot mono">
        {lastRefreshed ? `last refreshed ${fmtClock(lastRefreshed)}` : graphLoading ? 'loading…' : ''}
      </div>
    </div>
  )
}
