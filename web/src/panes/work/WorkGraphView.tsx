// WorkGraphView — middle-canvas container for the wi relationship
// knowledge-graph (tether#23). Owns the graph fetch + poll for the current
// store.workProject and lazy-loads the d3-force <ForceGraph> renderer so
// d3-force stays out of the initial bundle. Clicking a node routes to the
// shared selection (store.select), which the right-pane Work detail reacts
// to; the selected node's block edges are lazily overlaid.
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

export default function WorkGraphView() {
  const project = useStore((s) => s.workProject)
  const selectedWiId = useStore((s) => s.selectedWiId)
  const select = useStore((s) => s.select)

  const [graph, setGraph] = useState<WorkGraph | null>(null)
  const [graphError, setGraphError] = useState<string | null>(null)
  const [blockEdges, setBlockEdges] = useState<FGEdge[]>([])

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
  // the force sim, so this never reshapes the layout. ──
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

  const fgNodes: FGNode[] = graph.nodes.map((n) => ({
    id: n.id,
    label: n.slug,
    status: n.status,
    sub: n.wiType,
  }))
  const parentEdges: FGEdge[] = graph.nodes
    .filter((n): n is WorkGraphNode & { parent: string } => !!n.parent)
    .map((n) => ({ from: n.parent, to: n.id, kind: 'parent' as const }))
  const fgEdges = [...parentEdges, ...blockEdges]

  return (
    <div className="work-graph-view">
      <Suspense fallback={<div className="work-empty work-graph-hint">loading graph…</div>}>
        <ForceGraph
          nodes={fgNodes}
          edges={fgEdges}
          selectedId={selectedWiId ?? undefined}
          onSelect={(id) => {
            select({ wiId: id })
            // bring the right Work tab forward so the detail is visible even if
            // the user was on Chat/Shell when they clicked a node (review F2)
            window.dispatchEvent(new CustomEvent('tether:select-tab', { detail: 'work' }))
          }}
        />
      </Suspense>
    </div>
  )
}
