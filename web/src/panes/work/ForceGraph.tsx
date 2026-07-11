// ForceGraph.tsx — Work relationship map: deterministic column-card layout.
// (The file keeps its historical name; the d3-force physics it was named for
// was removed in tether#25 — layout is now pure arithmetic.) Presentational:
// the caller owns data + selection; this component places each wi as a labeled
// CARD in its status column and paints the relationship edges.
//
// Layout is PURE ARITHMETIC (no physics): x = status column, y = the card's
// sorted row within that column. Columns: blocked | running | ready | paused |
// done. Within a column, cards sort by priority (urgent first) then seq (newest
// first) and stack top-to-bottom under the column header.
//
// RESPONSIVE (tether#27): the map lives in the narrow right Work tab (tether#26),
// so column/card sizes are computed from the measured container WIDTH — present
// columns pack left-to-right and shrink to fit, capped at the natural size, and
// below a threshold cards drop to a compact form (slug + status bar only). The
// initial viewBox is a width-fit window (scale ≈ 1, readable) with vertical PAN
// for tall columns — NOT a shrink-everything-to-fit meet, which is what made the
// map illegible in the narrow pane.
//
// Positions are memoized on a CONTENT key (sorted ids + status column +
// priority/seq order + structural edges + container-width bucket), NOT array
// identity — an 8s poll returning the same graph reuses positions (no reshuffle);
// a status change re-places a card WITHOUT resetting pan/zoom (structure-key vs
// layout-key split, tether#24); a container resize re-fits.
//
// Block edges are an OVERLAY (drawn; never shape the layout). Pan = pointer-drag
// past 4px (NO setPointerCapture, tether#20); zoom = non-passive native wheel
// (tether#22). Lazy-loaded behind WorkGraphView.
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

const MAX_CARD_W = 132 // natural card width (cap in a wide pane)
const MIN_CARD_W = 64
const MAX_COL_GAP = 210 // natural column center-to-center spacing (cap)
const COL_MARGIN = 14 // horizontal gap between a card and its column slot edge
const COMPACT_THRESHOLD = 96 // cardW below this → compact card (slug + bar only)
const CARD_H = 34
const CARD_H_COMPACT = 22
const ROW_GAP = 10
const HEAD_H = 24
const BAR_W = 4
const PAD = 28
const MIN_SCALE = 0.3
const MAX_SCALE = 4
const CW_BUCKET = 20 // quantise the measured width so a 1px resize doesn't thrash
const CHAR_W = 6.6 // ~mono glyph advance at the 11px slug font, for truncation
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
  cardW: number
  cardH: number
  compact: boolean
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

/** Truncate a label to fit an available pixel width at the slug font. */
function fitText(label: string, availPx: number): string {
  const max = Math.max(1, Math.floor(availPx / CHAR_W))
  if (label.length <= max) return label
  if (max <= 1) return label.slice(0, 1)
  return label.slice(0, max - 1) + '…'
}

/** Place each node as a card sized to the container width `cw`: present columns
 *  pack left-to-right and shrink to fit; cards cap at the natural size and drop
 *  to a compact form when narrow. Pure arithmetic — no physics. `cw <= 0`
 *  (unmeasured / no ResizeObserver) falls back to the natural sizes. */
export function computePositions(nodes: FGNode[], cw: number): Positions {
  const buckets = new Map<number, FGNode[]>()
  for (const n of nodes) {
    const c = statusCol(n.status)
    const arr = buckets.get(c)
    if (arr) arr.push(n)
    else buckets.set(c, [n])
  }
  const presentCols = [...buckets.keys()].sort((a, b) => a - b)
  const nCols = Math.max(presentCols.length, 1)

  // Responsive column/card sizing from the container width.
  let colGap: number
  let cardW: number
  if (cw <= 0) {
    colGap = MAX_COL_GAP
    cardW = MAX_CARD_W
  } else {
    const slot = Math.max((cw - PAD * 2) / nCols, MIN_CARD_W + COL_MARGIN)
    colGap = Math.min(slot, MAX_COL_GAP)
    cardW = Math.min(Math.max(colGap - COL_MARGIN, MIN_CARD_W), MAX_CARD_W)
  }
  const compact = cardW < COMPACT_THRESHOLD
  const cardH = compact ? CARD_H_COMPACT : CARD_H
  const topY = HEAD_H + cardH / 2 // center-y of each column's first row

  // pack present columns left-to-right (index in presentCols, not statusCol)
  const packIndex = new Map<number, number>()
  presentCols.forEach((c, i) => packIndex.set(c, i))

  const pos = new Map<string, Pt>()
  let maxRows = 0
  for (const [c, arr] of buckets) {
    arr.sort((a, b) => {
      const pr = priorityRank(a.priority) - priorityRank(b.priority)
      if (pr !== 0) return pr
      return parseSeq(b.label) - parseSeq(a.label) // newer (higher seq) first
    })
    maxRows = Math.max(maxRows, arr.length)
    const cx = (packIndex.get(c) ?? 0) * colGap
    arr.forEach((n, i) => {
      pos.set(n.id, { x: cx, y: topY + i * (cardH + ROW_GAP) })
    })
  }

  const minX = -cardW / 2 - PAD
  const maxX = (nCols - 1) * colGap + cardW / 2 + PAD
  const minY = -PAD
  const rowsH = maxRows > 0 ? topY + (maxRows - 1) * (cardH + ROW_GAP) + cardH / 2 : topY
  const maxY = rowsH + PAD
  const box: Box = { x: minX, y: minY, w: maxX - minX, h: maxY - minY }

  const cols: Col[] = presentCols.map((c) => ({
    label: COL_LABELS[c] ?? 'other',
    x: (packIndex.get(c) ?? 0) * colGap,
    count: buckets.get(c)?.length ?? 0,
  }))

  return { pos, box, cols, cardW, cardH, compact }
}

/** Curved edge anchored at the facing card edges (horizontal), or centers when
 *  the two cards share a column. */
function edgePath(s: Pt, t: Pt, cardW: number): string {
  let sx = s.x
  let tx = t.x
  if (t.x > s.x) {
    sx = s.x + cardW / 2
    tx = t.x - cardW / 2
  } else if (t.x < s.x) {
    sx = s.x - cardW / 2
    tx = t.x + cardW / 2
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
  const containerRef = useRef<HTMLDivElement>(null)
  // Measured container size (bucketed). {0,0} until the ResizeObserver fires
  // (or forever under jsdom, which has no ResizeObserver) → natural-size fallback.
  const [size, setSize] = useState<{ cw: number; ch: number }>({ cw: 0, ch: 0 })

  useEffect(() => {
    const el = containerRef.current
    if (!el || typeof ResizeObserver === 'undefined') return
    const ro = new ResizeObserver((entries) => {
      const r = entries[0]?.contentRect
      if (!r) return
      const cw = Math.round(r.width / CW_BUCKET) * CW_BUCKET
      const ch = Math.round(r.height / CW_BUCKET) * CW_BUCKET // bucket too (no per-px re-render)
      setSize((prev) => (prev.cw === cw && prev.ch === ch ? prev : { cw, ch }))
    })
    ro.observe(el)
    return () => ro.disconnect()
  }, [])

  // Structure key drives the viewport RE-FIT: a change to the node SET,
  // structural edges, container SIZE, or the present-COLUMN COUNT refits/
  // recenters. Adding/removing a column repacks the whole geometry
  // (colGap/cardW/box), so a frozen frame would clip it (tether#27 review F1);
  // a resize refits (F2). A status/priority change that keeps the same set of
  // columns must NOT reset pan/zoom (tether#24 review F1) — and doesn't, since
  // nCols is unchanged and packing-by-index keeps the moved card's x stable.
  const structureKey = useMemo(() => {
    const ids = nodes.map((n) => n.id).sort().join('|')
    const es = edges
      .filter((e) => e.kind !== 'block')
      .map((e) => `${e.from}>${e.to}`)
      .sort()
      .join('|')
    const nCols = new Set(nodes.map((n) => statusCol(n.status))).size
    return `${ids}::${es}::w${size.cw}::h${size.ch}::n${nCols}`
  }, [nodes, edges, size.cw, size.ch])

  // Layout key ALSO folds in each node's column + priority + seq, so a status or
  // priority change re-places that card — in place, WITHOUT a viewport reset.
  const layoutKey = useMemo(() => {
    const withOrder = nodes
      .map((n) => `${n.id}:${statusCol(n.status)}:${priorityRank(n.priority)}:${parseSeq(n.label)}`)
      .sort()
      .join('|')
    return `${withOrder}::${structureKey}`
  }, [nodes, structureKey])

  // eslint-disable-next-line react-hooks/exhaustive-deps -- keyed on layoutKey (content + width), not the array refs
  const layout = useMemo(() => computePositions(nodes, size.cw), [layoutKey])

  // Initial viewport = a width-fit window: viewBox width = box width so meet-fit
  // renders at ~1:1 (readable); its height matches the container aspect so tall
  // columns are reached by vertical PAN instead of being shrunk to fit.
  const fitH = size.cw > 0 && size.ch > 0 ? (layout.box.w * size.ch) / size.cw : layout.box.h
  const fitView: Box = { x: layout.box.x, y: layout.box.y, w: layout.box.w, h: fitH }

  const baseRef = useRef<Box>(fitView)
  baseRef.current = fitView

  const [resetKey, setResetKey] = useState(structureKey)
  const [view, setView] = useState<Box>(fitView)
  if (resetKey !== structureKey) {
    setResetKey(structureKey)
    setView(fitView)
  }
  const effectiveView = resetKey === structureKey ? view : fitView

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
        ? { key: `${e.from}->${e.to}-${i}`, d: edgePath(s, t, layout.cardW), block: e.kind === 'block' }
        : null
    })
    .filter((e): e is { key: string; d: string; block: boolean } => e !== null)

  const slugAvail = layout.cardW - BAR_W - 13

  return (
    <div className="fg-scroll" ref={containerRef}>
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
                transform={`translate(${p.x - layout.cardW / 2}, ${p.y - layout.cardH / 2})`}
                onClick={() => {
                  if (didDrag.current) return
                  onSelect?.(n.id)
                }}
                role={onSelect ? 'button' : undefined}
              >
                <rect className="fg-card" width={layout.cardW} height={layout.cardH} rx={5} />
                <rect className="fg-card-bar" width={BAR_W} height={layout.cardH} />
                <text className="fg-card-slug" x={BAR_W + 9} y={layout.compact ? layout.cardH / 2 + 4 : 14}>
                  {fitText(n.label, slugAvail)}
                </text>
                {!layout.compact && n.sub && (
                  <text className="fg-card-type" x={BAR_W + 9} y={26}>
                    {fitText(n.sub, slugAvail)}
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
