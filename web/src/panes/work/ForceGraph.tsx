// ForceGraph.tsx — Work relationship map: deterministic column-card layout.
// (The file keeps its historical name; the d3-force physics it was named for
// was removed in tether#25 — layout is now pure arithmetic.) Presentational:
// the caller owns data + selection; this component places each wi as a labeled
// CARD in its status column and paints the relationship edges.
//
// Layout is PURE ARITHMETIC (no physics): x = status column, y = the card's
// sorted row within that column. Columns: blocked | running | ready | paused |
// done. Within a column, cards sort by priority (urgent first) then seq (newest
// first) and stack top-to-bottom under the column header — so a mostly-
// disconnected set reads as tidy ordered lists instead of scattered dots with
// big gaps (the "empty dots" complaint that drove tether#25).
//
// Positions are memoized on a CONTENT key (sorted ids + status column +
// priority/seq order + structural edges), NOT array identity — so an 8s poll
// returning the same graph in a fresh array reuses positions and the map does
// NOT reshuffle. A status change re-places that card WITHOUT resetting the
// user's pan/zoom (structure-key vs layout-key split, tether#24). Status and
// labels render from LIVE props.
//
// Block edges are an OVERLAY (drawn; they never shape the arithmetic layout).
// Pan = pointer-drag past 4px (NO setPointerCapture, tether#20); zoom =
// non-passive native wheel (tether#22). Lazy-loaded behind WorkGraphView.
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { PointerEvent as ReactPointerEvent } from 'react'

export interface FGNode {
  id: string
  label: string
  status?: string
  sub?: string
  priority?: string
  title?: string // hover tooltip (e.g. the wi goal); falls back to label
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

const CARD_W = 132
const CARD_H = 34
const ROW_GAP = 10
const COL_GAP = 210
const HEAD_H = 24
const BAR_W = 4
const PAD = 28
const MIN_SCALE = 0.3
const MAX_SCALE = 4
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
  pos: Map<string, Pt> // card CENTER
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

/** Sort rank for priority (lower = higher priority = nearer the top). */
function priorityRank(p: string | undefined): number {
  switch (p) {
    case 'urgent':
      return 0
    case 'high':
      return 1
    case 'low':
      return 3
    default:
      return 2 // normal / unknown
  }
}

/** Parse the trailing seq from a slug like "tether#25" → 25 (0 if absent). */
function parseSeq(label: string): number {
  const m = /#(\d+)/.exec(label)
  return m ? Number(m[1]) : 0
}

/** Place each node as a card: x = status column, y = its sorted row in that
 *  column. Pure arithmetic — no physics. Returns card CENTER positions, a
 *  padded bounding box, and the present status columns (for headers). */
function computePositions(nodes: FGNode[]): Positions {
  const buckets = new Map<number, FGNode[]>()
  for (const n of nodes) {
    const c = statusCol(n.status)
    const arr = buckets.get(c)
    if (arr) arr.push(n)
    else buckets.set(c, [n])
  }

  const topY = HEAD_H + CARD_H / 2 // center-y of each column's first row
  const pos = new Map<string, Pt>()
  let maxRows = 0
  for (const [c, arr] of buckets) {
    arr.sort((a, b) => {
      const pr = priorityRank(a.priority) - priorityRank(b.priority)
      if (pr !== 0) return pr
      return parseSeq(b.label) - parseSeq(a.label) // newer (higher seq) first
    })
    maxRows = Math.max(maxRows, arr.length)
    arr.forEach((n, i) => {
      pos.set(n.id, { x: c * COL_GAP, y: topY + i * (CARD_H + ROW_GAP) })
    })
  }

  const presentCols = [...buckets.keys()].sort((a, b) => a - b)
  const minCol = presentCols[0] ?? 0
  const maxCol = presentCols[presentCols.length - 1] ?? 0
  const minX = minCol * COL_GAP - CARD_W / 2 - PAD
  const maxX = maxCol * COL_GAP + CARD_W / 2 + PAD
  const minY = -PAD
  const rowsH = maxRows > 0 ? topY + (maxRows - 1) * (CARD_H + ROW_GAP) + CARD_H / 2 : topY
  const maxY = rowsH + PAD
  const box: Box = { x: minX, y: minY, w: maxX - minX, h: maxY - minY }

  const cols: Col[] = presentCols.map((c) => ({
    label: COL_LABELS[c] ?? 'other',
    x: c * COL_GAP,
    count: buckets.get(c)?.length ?? 0,
  }))

  return { pos, box, cols }
}

/** Curved edge anchored at the facing card edges (horizontal), or centers when
 *  the two cards share a column. */
function edgePath(s: Pt, t: Pt): string {
  let sx = s.x
  let tx = t.x
  if (t.x > s.x) {
    sx = s.x + CARD_W / 2
    tx = t.x - CARD_W / 2
  } else if (t.x < s.x) {
    sx = s.x - CARD_W / 2
    tx = t.x + CARD_W / 2
  }
  const mx = (sx + tx) / 2
  const my = (s.y + t.y) / 2
  const dx = tx - sx
  const dy = t.y - s.y
  const cx = mx - dy * 0.12
  const cy = my + dx * 0.12
  return `M ${sx} ${s.y} Q ${cx} ${cy} ${tx} ${t.y}`
}

export default function ForceGraph({ nodes, edges, selectedId, onSelect }: ForceGraphProps) {
  // Structure key drives the viewport RESET: only a change to the node SET or
  // structural edges refits/recenters the map. A status/priority change must
  // NOT reset the user's pan/zoom (tether#24 review F1).
  const structureKey = useMemo(() => {
    const ids = nodes.map((n) => n.id).sort().join('|')
    const es = edges
      .filter((e) => e.kind !== 'block')
      .map((e) => `${e.from}>${e.to}`)
      .sort()
      .join('|')
    return `${ids}::${es}`
  }, [nodes, edges])

  // Layout key ALSO folds in each node's column + priority + seq, so a status
  // or priority change re-places that card — in place, WITHOUT a viewport
  // reset. Poll-stable: an unchanged poll yields an identical key.
  const layoutKey = useMemo(() => {
    const withOrder = nodes
      .map((n) => `${n.id}:${statusCol(n.status)}:${priorityRank(n.priority)}:${parseSeq(n.label)}`)
      .sort()
      .join('|')
    return `${withOrder}::${structureKey}`
  }, [nodes, structureKey])

  // eslint-disable-next-line react-hooks/exhaustive-deps -- keyed on layoutKey (content), not the array refs
  const layout = useMemo(() => computePositions(nodes), [layoutKey])

  const baseRef = useRef<Box>(layout.box)
  baseRef.current = layout.box

  const [resetKey, setResetKey] = useState(structureKey)
  const [view, setView] = useState<Box>(layout.box)
  if (resetKey !== structureKey) {
    setResetKey(structureKey)
    setView(layout.box)
  }
  const effectiveView = resetKey === structureKey ? view : layout.box

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
      return s && t
        ? { key: `${e.from}->${e.to}-${i}`, d: edgePath(s, t), block: e.kind === 'block' }
        : null
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
        <g className="fg-cols">
          {layout.cols.map((c) => (
            <text key={c.label} className="fg-col-head" x={c.x} y={14} textAnchor="middle">
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
            return (
              <g
                key={n.id}
                className={`fg-node ${statusClass(n.status)} ${selected ? 'fg-node-selected' : ''}`}
                transform={`translate(${p.x - CARD_W / 2}, ${p.y - CARD_H / 2})`}
                onClick={() => {
                  if (didDrag.current) return
                  onSelect?.(n.id)
                }}
                role={onSelect ? 'button' : undefined}
              >
                <rect className="fg-card" width={CARD_W} height={CARD_H} rx={5} />
                <rect className="fg-card-bar" width={BAR_W} height={CARD_H} />
                <text className="fg-card-slug" x={BAR_W + 9} y={14}>
                  {n.label}
                </text>
                {n.sub && (
                  <text className="fg-card-type" x={BAR_W + 9} y={26}>
                    {n.sub}
                  </text>
                )}
                <title>
                  {n.label}
                  {n.title ? ` — ${n.title}` : ''}
                </title>
              </g>
            )
          })}
        </g>
      </svg>
    </div>
  )
}
