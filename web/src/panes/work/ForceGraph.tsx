// ForceGraph.tsx — force-directed knowledge-graph renderer for the wi
// relationship graph (tether#23, scaled in tether#24). Presentational: the
// caller owns data + selection, this component lays nodes out with d3-force
// and paints them.
//
// Positions are computed ONCE per distinct graph SHAPE, synchronously, to a
// settled state (no animation loop): a useMemo keyed on a CONTENT key (sorted
// ids + structural edges + status), NOT array identity — so an 8s poll that
// returns the same graph in a fresh array reuses positions and the map does
// NOT reshuffle (tether#23 F4/F5). Status/label render from LIVE props.
//
// Scale features (tether#24):
//   - status CLUSTERING: forceX pulls each node toward its status column
//     (blocked | running | ready | paused | done), so a mostly-disconnected
//     set reads as ordered columns instead of a hairball. Column headers are
//     drawn at the top.
//   - semantic-zoom LABELS: dots always render; a node's label renders only
//     when it is selected, hovered, or the current zoom is past a threshold —
//     so labels don't overlap into mush at scale.
//
// Block edges are an OVERLAY (drawn, excluded from the sim, tether#23 F1). Pan
// = pointer-drag past 4px (NO setPointerCapture); zoom = non-passive native
// wheel (tether#22 F2). Lazy-loaded behind WorkGraphView.
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { PointerEvent as ReactPointerEvent } from 'react'
import {
  forceCollide,
  forceLink,
  forceManyBody,
  forceSimulation,
  forceX,
  forceY,
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
const COL_GAP = 220
const LABEL_ZOOM = 1.15 // show all labels once zoomed past this
const COL_LABELS = ['blocked', 'running', 'ready', 'paused', 'done']

type Pt = { x: number; y: number }

interface Box {
  x: number
  y: number
  w: number
  h: number
}

interface Col {
  label: string
  x: number
  count: number
}

interface Positions {
  pos: Map<string, Pt>
  box: Box
  cols: Col[]
}

/** Column index (0..4) a status clusters into. */
function statusCol(status: string | undefined): number {
  switch (status) {
    case 'blocked':
      return 0
    case 'running':
    case 'current':
      return 1
    case 'queued':
    case 'pending':
      return 2
    case 'paused':
      return 3
    default:
      return 4 // done / wrapped / cancelled / failed / unknown
  }
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

/** Settle a d3-force layout synchronously with status-column clustering.
 *  Only STRUCTURAL edges (non-block) drive the layout. Returns node positions,
 *  a padded bounding box, and the present status columns (for headers). */
function computePositions(nodes: FGNode[], edges: FGEdge[]): Positions {
  const statusOf = new Map(nodes.map((n) => [n.id, n.status]))
  const simNodes: SimNode[] = nodes.map((n) => ({ id: n.id }))
  const byId = new Map(simNodes.map((n) => [n.id, n]))
  const simLinks: SimLink[] = edges
    .filter((e) => e.kind !== 'block' && byId.has(e.from) && byId.has(e.to))
    .map((e) => ({ source: e.from, target: e.to }))

  const sim = forceSimulation<SimNode>(simNodes)
    .force('charge', forceManyBody<SimNode>().strength(-160))
    .force(
      'link',
      forceLink<SimNode, SimLink>(simLinks)
        .id((d) => d.id)
        .distance(70)
        .strength(0.25),
    )
    .force('x', forceX<SimNode>((d) => statusCol(statusOf.get(d.id)) * COL_GAP).strength(0.4))
    .force('y', forceY<SimNode>(0).strength(0.06))
    .force('collide', forceCollide<SimNode>(30))
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
  // Column headers sit above the nodes; reserve room for them.
  const headPad = 22
  const pad = 28
  const box: Box = {
    x: minX - pad,
    y: minY - pad - headPad,
    w: maxX - minX + pad * 2,
    h: maxY - minY + pad * 2 + headPad,
  }

  const colCount = new Map<number, number>()
  for (const n of nodes) {
    const c = statusCol(n.status)
    colCount.set(c, (colCount.get(c) ?? 0) + 1)
  }
  const cols: Col[] = [...colCount.entries()]
    .sort((a, b) => a[0] - b[0])
    .map(([c, count]) => ({ label: COL_LABELS[c] ?? 'other', x: c * COL_GAP, count }))

  return { pos, box, cols }
}

function edgePath(s: Pt, t: Pt): string {
  const mx = (s.x + t.x) / 2
  const my = (s.y + t.y) / 2
  const dx = t.x - s.x
  const dy = t.y - s.y
  const cx = mx - dy * 0.12
  const cy = my + dx * 0.12
  return `M ${s.x} ${s.y} Q ${cx} ${cy} ${t.x} ${t.y}`
}

export default function ForceGraph({ nodes, edges, selectedId, onSelect }: ForceGraphProps) {
  // Structure key drives the viewport RESET: only a change to the node SET or
  // structural edges refits/recenters the map. A status change must NOT reset
  // the user's pan/zoom (or drop the labels they zoomed in to read) — that
  // regressed tether#23's poll-stability (tether#24 review F1).
  const structureKey = useMemo(() => {
    const ids = nodes.map((n) => n.id).sort().join('|')
    const es = edges
      .filter((e) => e.kind !== 'block')
      .map((e) => `${e.from}>${e.to}`)
      .sort()
      .join('|')
    return `${ids}::${es}`
  }, [nodes, edges])

  // Layout key ALSO folds in each node's status column, so a status change
  // re-clusters that node into its new column — in place, WITHOUT a viewport
  // reset. Poll-stable: an unchanged poll yields an identical key.
  const layoutKey = useMemo(() => {
    const withStatus = nodes.map((n) => `${n.id}:${statusCol(n.status)}`).sort().join('|')
    return `${withStatus}::${structureKey}`
  }, [nodes, structureKey])

  // eslint-disable-next-line react-hooks/exhaustive-deps -- keyed on layoutKey (content), not the array refs
  const layout = useMemo(() => computePositions(nodes, edges), [layoutKey])

  const baseRef = useRef<Box>(layout.box)
  baseRef.current = layout.box

  const [resetKey, setResetKey] = useState(structureKey)
  const [view, setView] = useState<Box>(layout.box)
  if (resetKey !== structureKey) {
    setResetKey(structureKey)
    setView(layout.box)
  }
  const effectiveView = resetKey === structureKey ? view : layout.box

  const [hoveredId, setHoveredId] = useState<string | null>(null)

  const svgRef = useRef<SVGSVGElement>(null)
  const drag = useRef<{ startX: number; startY: number; lastX: number; lastY: number } | null>(null)
  const didDrag = useRef(false)

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
        const factor = e.deltaY > 0 ? 1.1 : 1 / 1.1
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

  const laidEdges = edges
    .map((e, i) => {
      const s = layout.pos.get(e.from)
      const t = layout.pos.get(e.to)
      return s && t ? { key: `${e.from}->${e.to}-${i}`, d: edgePath(s, t), block: e.kind === 'block' } : null
    })
    .filter((e): e is { key: string; d: string; block: boolean } => e !== null)

  // Semantic zoom: labels only when zoomed past LABEL_ZOOM, or per-node when
  // selected/hovered. scale = base width / current viewBox width.
  const scale = effectiveView.w > 0 ? layout.box.w / effectiveView.w : 1
  const showAllLabels = scale >= LABEL_ZOOM

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
        <g className="fg-cols">
          {layout.cols.map((c) => (
            <text key={c.label} className="fg-col-head" x={c.x} y={layout.box.y + 14} textAnchor="middle">
              {c.label} · {c.count}
            </text>
          ))}
        </g>
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
            const showLabel = selected || hoveredId === n.id || showAllLabels
            return (
              <g
                key={n.id}
                className={`fg-node ${statusClass(n.status)} ${selected ? 'fg-node-selected' : ''}`}
                transform={`translate(${p.x}, ${p.y})`}
                onClick={() => {
                  if (didDrag.current) return
                  onSelect?.(n.id)
                }}
                onPointerOver={() => setHoveredId(n.id)}
                onPointerOut={(e) => {
                  // ignore transitions to a child of this node group
                  if (e.currentTarget.contains(e.relatedTarget as Node | null)) return
                  setHoveredId((cur) => (cur === n.id ? null : cur))
                }}
                role={onSelect ? 'button' : undefined}
              >
                <circle className="fg-node-dot" r={NODE_R} />
                {showLabel && (
                  <text className="fg-node-label" x={LABEL_DX} y={3}>
                    {n.label}
                  </text>
                )}
                {showLabel && n.sub && (
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
