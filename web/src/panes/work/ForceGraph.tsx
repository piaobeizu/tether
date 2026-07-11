// ForceGraph.tsx — force-directed knowledge-graph renderer for the wi
// relationship graph (tether#23). Presentational: the caller owns data +
// selection, this component lays nodes out with d3-force and paints them.
//
// Positions are computed ONCE per distinct graph SHAPE, synchronously, to a
// settled state (no animation loop): a useMemo keyed on a CONTENT key (sorted
// node ids + sorted structural edge pairs), NOT on the array identity — so an
// 8s poll that returns the same graph in a fresh array reuses the cached
// positions and the map does NOT reshuffle (tether#23 review F4/F5). Node
// status/label are read from the LIVE props each render (so a status change
// recolors in place without a relayout). Block edges are an OVERLAY: they are
// drawn but NOT fed into the force sim, so selecting a wi never restructures
// the map (review F1).
//
// A force blob is roughly square, so a viewBox fit does not squash it (unlike
// the dagre relationship layout, tether#22). Pan = pointer-drag past 4px (NO
// setPointerCapture — that swallowed node clicks in tether#20); zoom = a
// non-passive native wheel listener so preventDefault actually stops the
// browser page zoom (tether#22 F2). Lazy-loaded behind WorkGraphView so
// d3-force stays out of the initial bundle.
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { PointerEvent as ReactPointerEvent } from 'react'
import {
  forceCenter,
  forceCollide,
  forceLink,
  forceManyBody,
  forceSimulation,
} from 'd3-force'
import type { SimulationLinkDatum, SimulationNodeDatum } from 'd3-force'

export interface FGNode {
  id: string
  label: string
  status?: string
  sub?: string
}

export interface FGEdge {
  from: string
  to: string
  kind?: 'parent' | 'block' | 'step'
}

export interface ForceGraphProps {
  nodes: FGNode[]
  edges: FGEdge[]
  selectedId?: string
  onSelect?: (id: string) => void
}

interface SimNode extends SimulationNodeDatum {
  id: string
}
interface SimLink extends SimulationLinkDatum<SimNode> {}

const NODE_R = 7
const LABEL_DX = NODE_R + 6
const TICKS = 300
const MIN_SCALE = 0.3
const MAX_SCALE = 4

type Pt = { x: number; y: number }

interface Box {
  x: number
  y: number
  w: number
  h: number
}

interface Positions {
  pos: Map<string, Pt>
  box: Box
}

function statusClass(status: string | undefined): string {
  switch (status) {
    case 'done':
    case 'wrapped':
      return 'fg-status-done'
    case 'current':
    case 'running':
      return 'fg-status-running'
    case 'failed':
    case 'cancelled':
    case 'blocked':
      return 'fg-status-failed'
    case 'queued':
    case 'pending':
      return 'fg-status-pending'
    default:
      return 'fg-status-default'
  }
}

/** Settle a d3-force layout synchronously and return each node's position
 *  plus a padded bounding box (accounting for label width). Only STRUCTURAL
 *  edges (non-block) drive the layout; block edges are drawn as an overlay by
 *  the caller so selecting a wi never reshapes the map. Edges to an unknown
 *  id are ignored. */
function computePositions(nodes: FGNode[], edges: FGEdge[]): Positions {
  const simNodes: SimNode[] = nodes.map((n) => ({ id: n.id }))
  const byId = new Map(simNodes.map((n) => [n.id, n]))
  const simLinks: SimLink[] = edges
    .filter((e) => e.kind !== 'block' && byId.has(e.from) && byId.has(e.to))
    .map((e) => ({ source: e.from, target: e.to }))

  const sim = forceSimulation<SimNode>(simNodes)
    .force('charge', forceManyBody<SimNode>().strength(-260))
    .force(
      'link',
      forceLink<SimNode, SimLink>(simLinks)
        .id((d) => d.id)
        .distance(96)
        .strength(0.5),
    )
    .force('center', forceCenter(0, 0))
    .force('collide', forceCollide<SimNode>(52))
    .stop()
  for (let i = 0; i < TICKS; i++) sim.tick()

  const pos = new Map<string, Pt>()
  for (const n of simNodes) pos.set(n.id, { x: n.x ?? 0, y: n.y ?? 0 })

  let minX = Infinity
  let minY = Infinity
  let maxX = -Infinity
  let maxY = -Infinity
  for (const n of nodes) {
    const p = pos.get(n.id)
    if (!p) continue
    const labelW = LABEL_DX + n.label.length * 6.7 + 6
    minX = Math.min(minX, p.x - NODE_R)
    maxX = Math.max(maxX, p.x + labelW)
    minY = Math.min(minY, p.y - NODE_R - 4)
    maxY = Math.max(maxY, p.y + NODE_R + 4)
  }
  if (!Number.isFinite(minX)) {
    minX = -100
    minY = -100
    maxX = 100
    maxY = 100
  }
  const pad = 28
  const box: Box = {
    x: minX - pad,
    y: minY - pad,
    w: maxX - minX + pad * 2,
    h: maxY - minY + pad * 2,
  }
  return { pos, box }
}

function edgePath(s: Pt, t: Pt): string {
  const mx = (s.x + t.x) / 2
  const my = (s.y + t.y) / 2
  const dx = t.x - s.x
  const dy = t.y - s.y
  // gentle perpendicular bow so overlapping straight edges stay distinguishable
  const cx = mx - dy * 0.12
  const cy = my + dx * 0.12
  return `M ${s.x} ${s.y} Q ${cx} ${cy} ${t.x} ${t.y}`
}

export default function ForceGraph({ nodes, edges, selectedId, onSelect }: ForceGraphProps) {
  // Content key: same node id-set + same structural edge-set → same layout,
  // regardless of array identity or ordering (poll-stable, order-independent).
  const shapeKey = useMemo(() => {
    const ids = nodes.map((n) => n.id).sort().join('|')
    const es = edges
      .filter((e) => e.kind !== 'block')
      .map((e) => `${e.from}>${e.to}`)
      .sort()
      .join('|')
    return `${ids}::${es}`
  }, [nodes, edges])

  // eslint-disable-next-line react-hooks/exhaustive-deps -- keyed on shapeKey (content), not the array refs
  const layout = useMemo(() => computePositions(nodes, edges), [shapeKey])

  const baseRef = useRef<Box>(layout.box)
  baseRef.current = layout.box

  // Reset the viewport to the fresh fit only when the graph SHAPE changes (not
  // on every poll) — preserves the user's pan/zoom across polls.
  const [resetKey, setResetKey] = useState(shapeKey)
  const [view, setView] = useState<Box>(layout.box)
  if (resetKey !== shapeKey) {
    setResetKey(shapeKey)
    setView(layout.box)
  }
  const effectiveView = resetKey === shapeKey ? view : layout.box

  const svgRef = useRef<SVGSVGElement>(null)
  const drag = useRef<{ startX: number; startY: number; lastX: number; lastY: number } | null>(null)
  const didDrag = useRef(false)

  // Zoom via a NON-passive native wheel listener (React's delegated wheel is
  // passive → preventDefault is a no-op, tether#22 F2). Plain wheel zooms the
  // graph — it IS the primary view, no scroll container to defer to.
  useEffect(() => {
    const svg = svgRef.current
    if (!svg) return
    const onWheel = (e: WheelEvent) => {
      e.preventDefault()
      const rect = svg.getBoundingClientRect()
      const cx = rect.width > 0 ? (e.clientX - rect.left) / rect.width : 0.5
      const cy = rect.height > 0 ? (e.clientY - rect.top) / rect.height : 0.5
      setView((vb) => {
        const base = baseRef.current
        const factor = e.deltaY > 0 ? 1.1 : 1 / 1.1 // scroll down → zoom out
        const curScale = base.w / vb.w
        const nextScale = Math.min(MAX_SCALE, Math.max(MIN_SCALE, curScale / factor))
        const newW = base.w / nextScale
        const newH = base.h / nextScale
        const focusX = vb.x + cx * vb.w
        const focusY = vb.y + cy * vb.h
        return { x: focusX - cx * newW, y: focusY - cy * newH, w: newW, h: newH }
      })
    }
    svg.addEventListener('wheel', onWheel, { passive: false })
    return () => svg.removeEventListener('wheel', onWheel)
  }, [])

  const onPointerDown = useCallback((e: ReactPointerEvent<SVGSVGElement>) => {
    drag.current = { startX: e.clientX, startY: e.clientY, lastX: e.clientX, lastY: e.clientY }
    didDrag.current = false
  }, [])

  const onPointerMove = useCallback((e: ReactPointerEvent<SVGSVGElement>) => {
    const d = drag.current
    if (!d) return
    const rect = svgRef.current?.getBoundingClientRect()
    const dxPx = e.clientX - d.lastX
    const dyPx = e.clientY - d.lastY
    d.lastX = e.clientX
    d.lastY = e.clientY
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

  const onPointerUp = useCallback(() => {
    drag.current = null
  }, [])

  // Draw from the LIVE props at the memoized positions: edges whose endpoints
  // are both placed (block edges included as an overlay), nodes coloured by
  // their current status.
  const laidEdges = edges
    .map((e, i) => {
      const s = layout.pos.get(e.from)
      const t = layout.pos.get(e.to)
      return s && t ? { key: `${e.from}->${e.to}-${i}`, d: edgePath(s, t), block: e.kind === 'block' } : null
    })
    .filter((e): e is { key: string; d: string; block: boolean } => e !== null)

  return (
    <div className="fg-scroll">
      <svg
        ref={svgRef}
        className="fg-svg"
        viewBox={`${effectiveView.x} ${effectiveView.y} ${effectiveView.w} ${effectiveView.h}`}
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={onPointerUp}
        onPointerLeave={onPointerUp}
      >
        <g className="fg-edges">
          {laidEdges.map((e) => (
            <path key={e.key} className={`fg-edge ${e.block ? 'fg-edge-block' : ''}`} d={e.d} fill="none" />
          ))}
        </g>
        <g className="fg-nodes">
          {nodes.map((n) => {
            const p = layout.pos.get(n.id)
            if (!p) return null
            const selected = n.id === selectedId
            return (
              <g
                key={n.id}
                className={`fg-node ${statusClass(n.status)} ${selected ? 'fg-node-selected' : ''}`}
                transform={`translate(${p.x}, ${p.y})`}
                onClick={() => {
                  if (didDrag.current) return
                  onSelect?.(n.id)
                }}
                role={onSelect ? 'button' : undefined}
              >
                <circle className="fg-node-dot" r={NODE_R} />
                <text className="fg-node-label" x={LABEL_DX} y={3}>
                  {n.label}
                </text>
                {n.sub && (
                  <text className="fg-node-sub" x={LABEL_DX} y={15}>
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
