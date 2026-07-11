// WorkGraphView — container for the wi relationship map (cards + deterministic
// columns as of tether#25). Hosted in the right Work tab as of tether#26. Owns
// the graph fetch + poll for the current store.workProject and lazy-loads the
// <ForceGraph> renderer to keep it out of the initial bundle. Clicking a card
// routes to the shared selection (store.select), which opens the DetailDrawer
// over the map; the selected node's block edges are lazily overlaid.
import { lazy, Suspense, useEffect, useRef, useState } from 'react'
import { useStore } from '../../lib/store'
import { AihubError, fetchDeps, fetchGraph } from '../../lib/aihub'
import type { WorkGraph, WorkGraphNode } from '../../lib/wire.gen'
import type { FGEdge, FGNode } from './ForceGraph'

const ForceGraph = lazy(() => import('./ForceGraph'))
const POLL_MS = 8000

function describeError(e: unknown): string {
  if (e instanceof AihubError) {
    if (e.status === 503) return 'aihub not configured'
    if (e.status === 403) return 'not authorized for this project'
    return `error (HTTP ${e.status})`
  }
  return e instanceof Error ? e.message : String(e)
}

// Terminal statuses are hidden by the default 'active' filter (tether#24).
const TERMINAL = new Set(['done', 'wrapped', 'cancelled', 'failed'])
function isTerminal(status: string | undefined): boolean {
  return !!status && TERMINAL.has(status)
}

type FilterMode = 'active' | 'done' | 'all'

export default function WorkGraphView() {
  const project = useStore((s) => s.workProject)
  const selectedWiId = useStore((s) => s.selectedWiId)
  const select = useStore((s) => s.select)

  const [graph, setGraph] = useState<WorkGraph | null>(null)
  const [graphError, setGraphError] = useState<string | null>(null)
  const [blockEdges, setBlockEdges] = useState<FGEdge[]>([])
  // Scale filters (tether#24): default to active (non-terminal) wi so the map
  // isn't buried under a long tail of finished work.
  const [mode, setMode] = useState<FilterMode>('active')
  const [typeF, setTypeF] = useState<string>('')

  // Monotonic token: a newer load/project-switch supersedes a slower in-flight
  // fetch so it can't clobber the current project's graph (stale-response guard).
  const graphEpoch = useRef(0)
  const depsEpoch = useRef(0)
  // Latest graph, read by the block-edge effect without keying it on `graph`
  // (which changes identity every poll) so the effect doesn't re-fire per poll.
  const graphRef = useRef<WorkGraph | null>(null)
  graphRef.current = graph

  // ── Graph: (re)load on project change + poll while the tab is visible ──
  useEffect(() => {
    setGraph(null)
    setGraphError(null)
    setBlockEdges([])
    if (!project) return

    let alive = true
    const load = () => {
      const epoch = ++graphEpoch.current
      fetchGraph(project)
        .then((g) => {
          if (!alive || epoch !== graphEpoch.current) return
          setGraph(g)
          setGraphError(null)
        })
        .catch((e) => {
          if (!alive || epoch !== graphEpoch.current) return
          setGraphError(describeError(e))
        })
    }
    load()
    const timer = setInterval(() => {
      if (document.visibilityState === 'visible') load()
    }, POLL_MS)
    const onVisible = () => {
      if (document.visibilityState === 'visible') load()
    }
    document.addEventListener('visibilitychange', onVisible)
    return () => {
      alive = false
      clearInterval(timer)
      document.removeEventListener('visibilitychange', onVisible)
    }
  }, [project])

  // ── Block-edge overlay for the selected node (best-effort). Keyed on the
  // SELECTION only (not `graph`), so an 8s poll doesn't clear+refetch it and
  // jitter the map; the current node set is read from a ref (tether#23 F1).
  // Block edges are drawn as an overlay only — ForceGraph excludes them from
  // its structural layout key, so overlaying them never reshapes the columns. ──
  useEffect(() => {
    setBlockEdges([])
    const g = graphRef.current
    if (!selectedWiId || !g) return
    const epoch = ++depsEpoch.current
    const nodeIds = new Set(g.nodes.map((n) => n.id))
    fetchDeps(selectedWiId)
      .then((deps) => {
        if (epoch !== depsEpoch.current) return
        const edges: FGEdge[] = []
        for (const b of deps.blockedBy) {
          if (nodeIds.has(b.id)) edges.push({ from: b.id, to: selectedWiId, kind: 'block' })
        }
        for (const b of deps.blocking) {
          if (nodeIds.has(b.id)) edges.push({ from: selectedWiId, to: b.id, kind: 'block' })
        }
        setBlockEdges(edges)
      })
      .catch(() => {
        /* deps overlay is best-effort; leave prior state */
      })
    // eslint-disable-next-line react-hooks/exhaustive-deps -- graph read via ref on purpose
  }, [selectedWiId])

  if (!project) {
    return <div className="work-empty work-graph-hint">select a project</div>
  }
  if (graphError) {
    return <div className="work-error work-graph-hint">{graphError}</div>
  }
  if (!graph) {
    return <div className="work-empty work-graph-hint">loading…</div>
  }
  if (graph.nodes.length === 0) {
    return <div className="work-empty work-graph-hint">no active work items</div>
  }

  // Client-side filter of the already-fetched full graph (no backend change).
  const wiTypes = [...new Set(graph.nodes.map((n) => n.wiType).filter((t): t is string => !!t))].sort()
  const shown = graph.nodes.filter((n) => {
    const terminal = isTerminal(n.status)
    const passMode = mode === 'all' ? true : mode === 'done' ? terminal : !terminal
    const passType = !typeF || n.wiType === typeF
    return passMode && passType
  })
  const shownIds = new Set(shown.map((n) => n.id))
  const fgNodes: FGNode[] = shown.map((n) => ({
    id: n.id,
    label: n.slug,
    status: n.status,
    sub: n.wiType,
    priority: n.priority,
    title: n.goal,
  }))
  const parentEdges: FGEdge[] = shown
    .filter((n): n is WorkGraphNode & { parent: string } => !!n.parent && shownIds.has(n.parent))
    .map((n) => ({ from: n.parent, to: n.id, kind: 'parent' as const }))
  const fgEdges = [
    ...parentEdges,
    ...blockEdges.filter((e) => shownIds.has(e.from) && shownIds.has(e.to)),
  ]

  return (
    <div className="work-graph-view">
      <div className="fg-filter">
        <div className="fg-seg-group">
          {(['active', 'done', 'all'] as const).map((m) => (
            <button
              key={m}
              type="button"
              className={`fg-seg${mode === m ? ' on' : ''}`}
              onClick={() => setMode(m)}
            >
              {m}
            </button>
          ))}
        </div>
        {wiTypes.length > 0 && (
          <select className="fg-type-select" value={typeF} onChange={(e) => setTypeF(e.target.value)}>
            <option value="">all types</option>
            {wiTypes.map((t) => <option key={t} value={t}>{t}</option>)}
          </select>
        )}
        <span style={{ flex: 1 }} />
        <span className="fg-filter-count mono">{shown.length}/{graph.nodes.length}</span>
      </div>
      <div className="fg-graph-slot">
        {fgNodes.length === 0 ? (
          <div className="work-empty work-graph-hint">no work items match the filter</div>
        ) : (
          <Suspense fallback={<div className="work-empty work-graph-hint">loading graph…</div>}>
            <ForceGraph
              nodes={fgNodes}
              edges={fgEdges}
              selectedId={selectedWiId ?? undefined}
              // the map lives in the Work tab now (tether#26), so selecting a
              // card just opens the DetailDrawer here — no tab switch needed.
              onSelect={(id) => select({ wiId: id })}
            />
          </Suspense>
        )}
      </div>
    </div>
  )
}
