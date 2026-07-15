// Dag.tsx — presentational DAG renderer shared by the wi-relationship graph
// (parent/block edges over WorkGraph + WorkDependencies) and the scenario
// step graph (WorkSteps). Pure layout + SVG paint: callers own data
// fetching and selection state, this component only lays out nodes with
// dagre and draws them. Keep it dependency-free beyond dagre itself so it
// stays cheap to reuse in a narrow ~260px pane.
//
// Rendering model (tether#22): the SVG is drawn at its *natural* layout size
// (1 user unit == 1 px at scale 1) inside a `.rdag-scroll` overflow:auto
// container, so nodes keep a legible size and the pane scrolls to reach
// off-screen nodes — rather than scaling the whole graph down to fit (which
// made a many-node graph microscopic). Plain wheel is left to the container
// for native pan; Ctrl/Cmd+wheel zooms (pure scale). CSS classes are
// namespaced `.rdag-*` to avoid colliding with the legacy chat DagBlock's
// `.dag-*` rules.
import { useEffect, useMemo, useRef, useState } from 'react'
import * as dagre from 'dagre'

export interface DagNode {
  id: string
  label: string
  status?: string
  sub?: string
}

export interface DagEdge {
  from: string
  to: string
  kind?: 'parent' | 'block' | 'step'
}

export interface DagProps {
  nodes: DagNode[]
  edges: DagEdge[]
  selectedId?: string
  onSelect?: (id: string) => void
  direction?: 'TB' | 'LR'
}

const NODE_W = 136
const NODE_H = 40
const NODE_RX = 6

const MIN_SCALE = 0.4
const MAX_SCALE = 3

interface LaidNode extends DagNode {
  x: number
  y: number
}

interface LaidEdge extends DagEdge {
  points: { x: number; y: number }[]
}

interface Layout {
  nodes: LaidNode[]
  edges: LaidEdge[]
  width: number
  height: number
}

/** Runs dagre layout over the given nodes/edges. Edges referencing an id
 *  not present in `nodes` are dropped rather than throwing, since callers
 *  (graph + dependency responses) can't guarantee referential closure. */
function computeLayout(nodes: DagNode[], edges: DagEdge[], direction: 'TB' | 'LR'): Layout {
  const g = new dagre.graphlib.Graph()
  g.setGraph({ rankdir: direction, nodesep: 20, ranksep: 44, marginx: 16, marginy: 16 })
  g.setDefaultEdgeLabel(() => ({}))

  for (const n of nodes) {
    g.setNode(n.id, { width: NODE_W, height: NODE_H })
  }
  for (const e of edges) {
    if (g.hasNode(e.from) && g.hasNode(e.to)) {
      g.setEdge(e.from, e.to)
    }
  }

  dagre.layout(g)

  const laidNodes: LaidNode[] = nodes
    .filter((n) => g.hasNode(n.id))
    .map((n) => {
      const gn = g.node(n.id)
      return { ...n, x: gn.x, y: gn.y }
    })

  const laidEdges: LaidEdge[] = edges
    .filter((e) => g.hasNode(e.from) && g.hasNode(e.to))
    .map((e) => ({ ...e, points: g.edge(e.from, e.to)?.points ?? [] }))

  const info = g.graph()
  return {
    nodes: laidNodes,
    edges: laidEdges,
    width: Math.max(info?.width ?? 0, NODE_W + 32),
    height: Math.max(info?.height ?? 0, NODE_H + 32),
  }
}

function statusClass(status: string | undefined): string {
  switch (status) {
    case 'done':
    case 'wrapped':
      return 'rdag-status-done'
    case 'current':
    case 'running':
      return 'rdag-status-running'
    case 'failed':
    case 'cancelled':
    case 'blocked':
      return 'rdag-status-failed'
    case 'queued':
    case 'pending':
      return 'rdag-status-pending'
    default:
      return 'rdag-status-default'
  }
}

function edgePath(points: { x: number; y: number }[]): string {
  if (points.length === 0) return ''
  return points.map((p, i) => `${i === 0 ? 'M' : 'L'} ${p.x} ${p.y}`).join(' ')
}

/** Compact DAG renderer. Drawn at natural size inside a scroll container so
 *  node text stays legible; Ctrl/Cmd+wheel zooms via a plain scale scalar
 *  (plain wheel is left to the container for native pan). No pointer-drag
 *  pan and no viewBox arithmetic — which also removes the whole class of
 *  pointer-capture-swallows-click bugs. */
export function Dag({ nodes, edges, selectedId, onSelect, direction = 'TB' }: DagProps) {
  const layout = useMemo(() => computeLayout(nodes, edges, direction), [nodes, edges, direction])

  const nodesKey = nodes.map((n) => n.id).join('|')

  // Reset the zoom whenever the node set changes shape, per React's
  // "adjusting state when a prop changes" pattern — a scale computed for the
  // previous graph doesn't make sense once a new one loads.
  const [resetKey, setResetKey] = useState(nodesKey)
  const [scale, setScale] = useState(1)
  if (resetKey !== nodesKey) {
    setResetKey(nodesKey)
    setScale(1)
  }
  const effectiveScale = resetKey === nodesKey ? scale : 1

  // Zoom on Ctrl/Cmd+wheel only (plain wheel is left to .rdag-scroll for
  // native pan; Ctrl/Cmd+wheel also fires for trackpad pinch). Attached as a
  // NON-passive native listener via ref: React registers its delegated
  // `wheel` handler as passive, where preventDefault() is a no-op — so an
  // onWheel prop would let the browser's native Ctrl+wheel page-zoom fire
  // alongside our graph zoom (tether#22 review F2).
  const svgRef = useRef<SVGSVGElement>(null)
  useEffect(() => {
    const svg = svgRef.current
    if (!svg) return
    const onWheel = (e: WheelEvent) => {
      if (!(e.ctrlKey || e.metaKey)) return
      e.preventDefault()
      setScale((s) => {
        const factor = e.deltaY > 0 ? 1 / 1.1 : 1.1
        return Math.min(MAX_SCALE, Math.max(MIN_SCALE, s * factor))
      })
    }
    svg.addEventListener('wheel', onWheel, { passive: false })
    return () => svg.removeEventListener('wheel', onWheel)
  }, [])

  const w = layout.width * effectiveScale
  const h = layout.height * effectiveScale

  return (
    <div className="rdag-scroll">
      <svg
        ref={svgRef}
        className="rdag-svg"
        width={w}
        height={h}
        viewBox={`0 0 ${layout.width} ${layout.height}`}
      >
        <defs>
          {/* Arrowhead for solid (step/parent) edges — gives the LR pipeline a
              direction cue. Fill is set via CSS (.rdag-arrow-head) so it tracks
              the theme. Fixed id is fine: only one Dag renders at a time, and
              identical same-id markers share the same visual if not. */}
          <marker
            id="rdag-arrow"
            viewBox="0 0 8 8"
            refX="7"
            refY="4"
            markerWidth="6"
            markerHeight="6"
            orient="auto"
          >
            <path className="rdag-arrow-head" d="M0,0 L8,4 L0,8 z" />
          </marker>
        </defs>
        <g className="rdag-edges">
          {layout.edges.map((e) => (
            <path
              key={`${e.from}->${e.to}`}
              className={`rdag-edge ${e.kind === 'block' ? 'rdag-edge-block' : 'rdag-edge-solid'}`}
              d={edgePath(e.points)}
              fill="none"
              markerEnd={e.kind === 'block' ? undefined : 'url(#rdag-arrow)'}
            />
          ))}
        </g>
        <g className="rdag-nodes">
          {layout.nodes.map((n) => {
            const selected = n.id === selectedId
            return (
              <g
                key={n.id}
                className={`rdag-node ${statusClass(n.status)} ${selected ? 'rdag-node-selected' : ''}`}
                transform={`translate(${n.x - NODE_W / 2}, ${n.y - NODE_H / 2})`}
                onClick={() => onSelect?.(n.id)}
                role={onSelect ? 'button' : undefined}
              >
                <rect className="rdag-node-rect" width={NODE_W} height={NODE_H} rx={NODE_RX} ry={NODE_RX} />
                <text className="rdag-node-label" x={NODE_W / 2} y={n.sub ? NODE_H / 2 - 4 : NODE_H / 2} textAnchor="middle" dominantBaseline="middle">
                  {n.label}
                </text>
                {n.sub && (
                  <text className="rdag-node-sub" x={NODE_W / 2} y={NODE_H / 2 + 12} textAnchor="middle" dominantBaseline="middle">
                    {n.sub}
                  </text>
                )}
              </g>
            )
          })}
        </g>
      </svg>
    </div>
  )
}
