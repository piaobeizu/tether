// Dag.tsx — presentational DAG renderer shared by the wi-relationship graph
// (parent/block edges over WorkGraph + WorkDependencies) and the scenario
// step graph (WorkSteps). Pure layout + SVG paint: callers own data
// fetching and selection state, this component only lays out nodes with
// dagre and draws them. Keep it dependency-free beyond dagre itself so it
// stays cheap to reuse in a narrow ~260px pane.
import { useCallback, useMemo, useRef, useState } from 'react'
import type { PointerEvent as ReactPointerEvent, WheelEvent as ReactWheelEvent } from 'react'
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
      return 'dag-status-done'
    case 'current':
    case 'running':
      return 'dag-status-running'
    case 'failed':
    case 'cancelled':
    case 'blocked':
      return 'dag-status-failed'
    case 'queued':
    case 'pending':
      return 'dag-status-pending'
    default:
      return 'dag-status-default'
  }
}

function edgePath(points: { x: number; y: number }[]): string {
  if (points.length === 0) return ''
  return points.map((p, i) => `${i === 0 ? 'M' : 'L'} ${p.x} ${p.y}`).join(' ')
}

interface ViewBox {
  x: number
  y: number
  w: number
  h: number
}

/** Compact pan/zoom DAG renderer. Zoom via wheel (scales the SVG viewBox
 *  around the cursor), pan via pointer-drag. No extra deps: both are plain
 *  viewBox arithmetic. */
export function Dag({ nodes, edges, selectedId, onSelect, direction = 'TB' }: DagProps) {
  const layout = useMemo(() => computeLayout(nodes, edges, direction), [nodes, edges, direction])

  const nodesKey = nodes.map((n) => n.id).join('|')
  const baseView = useMemo<ViewBox>(
    () => ({ x: 0, y: 0, w: layout.width, h: layout.height }),
    [layout.width, layout.height],
  )

  // Reset the viewport whenever the node set changes shape, per React's
  // "adjusting state when a prop changes" pattern — a pan/zoom computed for
  // the previous graph doesn't make sense once a new one loads.
  const [resetKey, setResetKey] = useState(nodesKey)
  const [view, setView] = useState<ViewBox>(baseView)
  if (resetKey !== nodesKey) {
    setResetKey(nodesKey)
    setView(baseView)
  }
  const effectiveView = resetKey === nodesKey ? view : baseView

  const svgRef = useRef<SVGSVGElement>(null)
  const drag = useRef<{ startX: number; startY: number; lastX: number; lastY: number } | null>(null)
  // Tracks whether the current pointer gesture became a pan. Read by the node
  // onClick (which fires right after pointerup) to suppress selection on a drag.
  const didDrag = useRef(false)

  const handleWheel = useCallback((e: ReactWheelEvent<SVGSVGElement>) => {
    e.preventDefault()
    const svg = svgRef.current
    if (!svg) return
    const rect = svg.getBoundingClientRect()
    const cx = rect.width > 0 ? (e.clientX - rect.left) / rect.width : 0.5
    const cy = rect.height > 0 ? (e.clientY - rect.top) / rect.height : 0.5

    setView((vb) => {
      const factor = e.deltaY > 0 ? 1.1 : 1 / 1.1
      const curScale = baseView.w / vb.w
      const nextScale = Math.min(MAX_SCALE, Math.max(MIN_SCALE, curScale / factor))
      const newW = baseView.w / nextScale
      const newH = baseView.h / nextScale
      const focusX = vb.x + cx * vb.w
      const focusY = vb.y + cy * vb.h
      return {
        x: focusX - cx * newW,
        y: focusY - cy * newH,
        w: newW,
        h: newH,
      }
    })
  }, [baseView])

  const handlePointerDown = useCallback((e: ReactPointerEvent<SVGSVGElement>) => {
    drag.current = { startX: e.clientX, startY: e.clientY, lastX: e.clientX, lastY: e.clientY }
    didDrag.current = false
    // Deliberately NOT setPointerCapture: capturing on the <svg> steals the
    // click from the node <g>, so node selection never fires. Instead we track
    // drag distance and only suppress the click when an actual pan happened.
  }, [])

  const handlePointerMove = useCallback((e: ReactPointerEvent<SVGSVGElement>) => {
    const d = drag.current
    if (!d) return
    const svg = svgRef.current
    const rect = svg?.getBoundingClientRect()
    const dxPx = e.clientX - d.lastX
    const dyPx = e.clientY - d.lastY
    d.lastX = e.clientX
    d.lastY = e.clientY
    // Below a small threshold treat it as a tap (let the node click through),
    // not a zero-distance pan.
    if (!didDrag.current) {
      if (Math.hypot(e.clientX - d.startX, e.clientY - d.startY) < 4) return
      didDrag.current = true
    }
    setView((vb) => {
      const scaleX = rect && rect.width > 0 ? vb.w / rect.width : 1
      const scaleY = rect && rect.height > 0 ? vb.h / rect.height : 1
      return { ...vb, x: vb.x - dxPx * scaleX, y: vb.y - dyPx * scaleY }
    })
  }, [])

  const handlePointerUp = useCallback((_e: ReactPointerEvent<SVGSVGElement>) => {
    // Leave didDrag set until the next pointerdown so the click that fires
    // immediately after this pointerup can see whether a drag occurred.
    drag.current = null
  }, [])

  return (
    <svg
      ref={svgRef}
      className="dag-svg"
      viewBox={`${effectiveView.x} ${effectiveView.y} ${effectiveView.w} ${effectiveView.h}`}
      onWheel={handleWheel}
      onPointerDown={handlePointerDown}
      onPointerMove={handlePointerMove}
      onPointerUp={handlePointerUp}
      onPointerLeave={handlePointerUp}
    >
      <g className="dag-edges">
        {layout.edges.map((e) => (
          <path
            key={`${e.from}->${e.to}`}
            className={`dag-edge ${e.kind === 'block' ? 'dag-edge-block' : 'dag-edge-solid'}`}
            d={edgePath(e.points)}
            fill="none"
          />
        ))}
      </g>
      <g className="dag-nodes">
        {layout.nodes.map((n) => {
          const selected = n.id === selectedId
          return (
            <g
              key={n.id}
              className={`dag-node ${statusClass(n.status)} ${selected ? 'dag-node-selected' : ''}`}
              transform={`translate(${n.x - NODE_W / 2}, ${n.y - NODE_H / 2})`}
              onClick={() => { if (didDrag.current) return; onSelect?.(n.id) }}
              role={onSelect ? 'button' : undefined}
            >
              <rect className="dag-node-rect" width={NODE_W} height={NODE_H} rx={NODE_RX} ry={NODE_RX} />
              <text className="dag-node-label" x={NODE_W / 2} y={n.sub ? NODE_H / 2 - 4 : NODE_H / 2} textAnchor="middle" dominantBaseline="middle">
                {n.label}
              </text>
              {n.sub && (
                <text className="dag-node-sub" x={NODE_W / 2} y={NODE_H / 2 + 12} textAnchor="middle" dominantBaseline="middle">
                  {n.sub}
                </text>
              )}
            </g>
          )
        })}
      </g>
    </svg>
  )
}
