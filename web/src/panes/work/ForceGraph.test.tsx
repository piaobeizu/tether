import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, fireEvent } from '@testing-library/react'
import ForceGraph, { computePositions } from './ForceGraph'
import type { FGEdge, FGNode } from './ForceGraph'

const nodes: FGNode[] = [
  { id: 'a', label: 'tether#1', status: 'done', sub: 'feature' },
  { id: 'b', label: 'tether#2', status: 'running', sub: 'fix_bug' },
  { id: 'c', label: 'tether#3', status: 'queued' },
]
const edges: FGEdge[] = [
  { from: 'a', to: 'b', kind: 'parent' },
  { from: 'b', to: 'c', kind: 'block' },
]

afterEach(() => {
  cleanup()
})

function vbWidth(svg: SVGSVGElement) {
  return Number((svg.getAttribute('viewBox') ?? '0 0 0 0').split(/\s+/)[2])
}
function vbX(svg: SVGSVGElement) {
  return Number((svg.getAttribute('viewBox') ?? '0 0 0 0').split(/\s+/)[0])
}
function txX(el: Element): number {
  const m = /translate\(([-\d.]+)/.exec(el.getAttribute('transform') ?? '')
  return m ? parseFloat(m[1]) : NaN
}
function txY(el: Element): number {
  const m = /translate\([-\d.]+,\s*([-\d.]+)/.exec(el.getAttribute('transform') ?? '')
  return m ? parseFloat(m[1]) : NaN
}

describe('ForceGraph', () => {
  it('renders a card per node', () => {
    const { container } = render(<ForceGraph nodes={nodes} edges={edges} />)
    expect(container.querySelectorAll('.fg-card').length).toBe(3)
  })

  // Cards always carry their slug label (semantic-zoom label gating was removed
  // in tether#25 — the whole point of the card redesign).
  it('labels every card with its slug', () => {
    const { container } = render(<ForceGraph nodes={nodes} edges={edges} />)
    const slugs = [...container.querySelectorAll('.fg-card-slug')].map((e) => e.textContent)
    expect(slugs).toEqual(['tether#1', 'tether#2', 'tether#3'])
  })

  it('renders one edge path per valid edge', () => {
    const { container } = render(<ForceGraph nodes={nodes} edges={edges} />)
    expect(container.querySelectorAll('.fg-edge').length).toBe(2)
  })

  it('drops edges referencing an unknown node id', () => {
    const { container } = render(
      <ForceGraph nodes={nodes} edges={[...edges, { from: 'a', to: 'zzz' }]} />,
    )
    expect(container.querySelectorAll('.fg-edge').length).toBe(2)
  })

  it('calls onSelect with the clicked node id (no drag)', () => {
    const onSelect = vi.fn()
    const { container } = render(<ForceGraph nodes={nodes} edges={edges} onSelect={onSelect} />)
    fireEvent.click(container.querySelectorAll('.fg-node')[1]) // node 'b'
    expect(onSelect).toHaveBeenCalledWith('b')
  })

  it('marks the selected node', () => {
    const { container } = render(<ForceGraph nodes={nodes} edges={edges} selectedId="c" />)
    const sel = container.querySelector('.fg-node-selected')
    expect(sel?.textContent).toContain('tether#3')
  })

  it('suppresses node click after a pan drag, selects on a tap', () => {
    const onSelect = vi.fn()
    const { container } = render(<ForceGraph nodes={nodes} edges={edges} onSelect={onSelect} />)
    const svg = container.querySelector('svg.fg-svg') as SVGSVGElement
    const nodeB = () => container.querySelectorAll('.fg-node')[1]

    fireEvent.pointerDown(svg, { clientX: 10, clientY: 10 })
    fireEvent.pointerMove(svg, { clientX: 80, clientY: 80 })
    fireEvent.pointerUp(svg, { clientX: 80, clientY: 80 })
    fireEvent.click(nodeB())
    expect(onSelect).not.toHaveBeenCalled()

    fireEvent.pointerDown(svg, { clientX: 10, clientY: 10 })
    fireEvent.pointerUp(svg, { clientX: 10, clientY: 10 })
    fireEvent.click(nodeB())
    expect(onSelect).toHaveBeenCalledWith('b')
  })

  it('zooms the viewBox on wheel', () => {
    const { container } = render(<ForceGraph nodes={nodes} edges={edges} />)
    const svg = container.querySelector('svg.fg-svg') as SVGSVGElement
    const w0 = vbWidth(svg)
    fireEvent.wheel(svg, { deltaY: 100 }) // zoom out → wider viewBox
    expect(vbWidth(svg)).toBeGreaterThan(w0)
  })

  it('places nodes in status columns (x ordered by status)', () => {
    // render order: done, blocked, running, queued → columns 4,0,1,2
    const cnodes: FGNode[] = [
      { id: 'd', label: 'D', status: 'done' },
      { id: 'bl', label: 'BL', status: 'blocked' },
      { id: 'r', label: 'R', status: 'running' },
      { id: 'q', label: 'Q', status: 'queued' },
    ]
    const { container } = render(<ForceGraph nodes={cnodes} edges={[]} />)
    const g = container.querySelectorAll('.fg-node')
    const x = { d: txX(g[0]), bl: txX(g[1]), r: txX(g[2]), q: txX(g[3]) }
    expect(x.bl).toBeLessThan(x.r)
    expect(x.r).toBeLessThan(x.q)
    expect(x.q).toBeLessThan(x.d)
  })

  it('stacks a column top-to-bottom sorted by priority then seq', () => {
    // all queued (same column): urgent floats above normals; within normals,
    // newer seq is higher. render order q1,q2,q3 → expect y(q2) < y(q3) < y(q1)
    const cnodes: FGNode[] = [
      { id: 'q1', label: 'tether#5', status: 'queued', priority: 'normal' },
      { id: 'q2', label: 'tether#9', status: 'queued', priority: 'urgent' },
      { id: 'q3', label: 'tether#20', status: 'queued', priority: 'normal' },
    ]
    const { container } = render(<ForceGraph nodes={cnodes} edges={[]} />)
    const g = container.querySelectorAll('.fg-node')
    const y = { q1: txY(g[0]), q2: txY(g[1]), q3: txY(g[2]) }
    // same column → same x
    expect(txX(g[0])).toBe(txX(g[1]))
    expect(txX(g[1])).toBe(txX(g[2]))
    // urgent (q2) on top, then normals newest-first (q3 before q1)
    expect(y.q2).toBeLessThan(y.q3)
    expect(y.q3).toBeLessThan(y.q1)
  })

  it('renders a header per present status column', () => {
    const cnodes: FGNode[] = [
      { id: 'd', label: 'D', status: 'done' },
      { id: 'bl', label: 'BL', status: 'blocked' },
      { id: 'r', label: 'R', status: 'running' },
      { id: 'q', label: 'Q', status: 'queued' },
    ]
    const { container } = render(<ForceGraph nodes={cnodes} edges={[]} />)
    expect(container.querySelectorAll('.fg-col-head').length).toBe(4)
  })

  // Poll-stability: an 8s poll hands back the same data in a FRESH array. The
  // content-keyed memo must reuse positions so the map does not reshuffle and
  // the viewport does not reset (regressed in tether#23 F4/F5).
  it('keeps positions and viewport stable across a poll returning a fresh identical array', () => {
    const { container, rerender } = render(<ForceGraph nodes={nodes} edges={edges} />)
    const svg = container.querySelector('svg.fg-svg') as SVGSVGElement
    const vb0 = svg.getAttribute('viewBox')
    const t0 = [...container.querySelectorAll('.fg-node')].map((g) => g.getAttribute('transform'))
    rerender(<ForceGraph nodes={nodes.map((n) => ({ ...n }))} edges={edges.map((e) => ({ ...e }))} />)
    const t1 = [...container.querySelectorAll('.fg-node')].map((g) => g.getAttribute('transform'))
    expect(t1).toEqual(t0)
    expect(svg.getAttribute('viewBox')).toBe(vb0)
  })

  // A status change re-places that card into its new column WITHOUT resetting
  // the user's pan/zoom (structure-key vs layout-key split, tether#24 F1).
  it('re-places a card on status change without resetting the viewport', () => {
    const base: FGNode[] = [
      { id: 'x', label: 'tether#1', status: 'queued' },
      { id: 'y', label: 'tether#2', status: 'running' },
    ]
    const { container, rerender } = render(<ForceGraph nodes={base} edges={[]} />)
    const svg = container.querySelector('svg.fg-svg') as SVGSVGElement
    const vb0 = svg.getAttribute('viewBox')
    const x0 = txX(container.querySelectorAll('.fg-node')[0]) // 'x' in queued column
    rerender(<ForceGraph nodes={[{ ...base[0], status: 'blocked' }, base[1]]} edges={[]} />)
    const x1 = txX(container.querySelectorAll('.fg-node')[0]) // 'x' now in blocked column
    expect(x1).not.toBe(x0)
    expect(svg.getAttribute('viewBox')).toBe(vb0)
  })

  // But a status change that changes the present-column COUNT repacks the whole
  // geometry, so the viewport MUST re-fit (else the new column is clipped
  // offscreen) — tether#27 review F1.
  it('re-fits the viewport when a status change adds a column', () => {
    const base: FGNode[] = [
      { id: 'a', label: 'tether#1', status: 'queued' },
      { id: 'b', label: 'tether#2', status: 'queued' }, // one present column
    ]
    const { container, rerender } = render(<ForceGraph nodes={base} edges={[]} />)
    const svg = container.querySelector('svg.fg-svg') as SVGSVGElement
    const w0 = vbWidth(svg)
    // 'b' moves to a new column → two present columns → wider box → re-fit
    rerender(<ForceGraph nodes={[base[0], { ...base[1], status: 'running' }]} edges={[]} />)
    expect(vbWidth(svg)).toBeGreaterThan(w0)
  })

  it('gives each card a title tooltip of slug + goal (slug-only when no goal)', () => {
    const tnodes: FGNode[] = [
      { id: 'g', label: 'tether#7', status: 'running', title: 'do the thing' },
      { id: 'h', label: 'tether#8', status: 'running' },
    ]
    const { container } = render(<ForceGraph nodes={tnodes} edges={[]} />)
    const titles = [...container.querySelectorAll('.fg-node title')].map((t) =>
      (t.textContent ?? '').replace(/\s+/g, ' ').trim(),
    )
    expect(titles).toContain('tether#7 — do the thing')
    expect(titles).toContain('tether#8')
  })
})

// Search / jump (tether#29), CONTROLLED as of tether#90: the box itself lives in
// WorkGraphView's filter row now, so this component takes `query`/`activeIndex`
// as props and reports the match set back. Highlighting, dimming, the match
// ORDERING and the viewport centering stay here; the keyboard handling that used
// to sit on the input is covered in WorkGraphView.test.tsx, against the real pair.
describe('ForceGraph search / jump (tether#29, controlled in tether#90)', () => {
  const activeSlug = (c: HTMLElement) =>
    c.querySelector('.fg-node-active .fg-card-slug')?.textContent ?? null
  const lastMatch = (fn: ReturnType<typeof vi.fn>) => fn.mock.calls.at(-1)?.[0]

  it('renders no input of its own — the box moved to the filter row (tether#90)', () => {
    const { container } = render(<ForceGraph nodes={nodes} edges={edges} query="#2" />)
    expect(container.querySelector('.fg-scroll input')).toBeNull()
    expect(container.querySelector('.fg-search')).toBeNull()
  })

  it('highlights matches, dims the rest, and reports the match set', () => {
    const onMatchesChange = vi.fn()
    const { container } = render(
      <ForceGraph nodes={nodes} edges={edges} query="#2" onMatchesChange={onMatchesChange} />,
    )
    const g = container.querySelectorAll('.fg-node')
    expect(g[1].classList.contains('fg-node-match')).toBe(true) // 'b' tether#2
    expect(g[0].classList.contains('fg-node-dim')).toBe(true) // 'a' tether#1
    expect(g[2].classList.contains('fg-node-dim')).toBe(true) // 'c' tether#3
    expect(g[1].classList.contains('fg-node-dim')).toBe(false)
    expect(lastMatch(onMatchesChange)).toEqual({ total: 1, index: 0, id: 'b' })
    expect(activeSlug(container)).toBe('tether#2')
  })

  it('normalizes the query itself (untrimmed, mixed case still matches)', () => {
    const gnodes: FGNode[] = [{ id: 'g', label: 'tether#7', status: 'running', title: 'Fix The Parser' }]
    const onMatchesChange = vi.fn()
    render(
      <ForceGraph nodes={gnodes} edges={[]} query="  PARSER  " onMatchesChange={onMatchesChange} />,
    )
    expect(lastMatch(onMatchesChange)).toEqual({ total: 1, index: 0, id: 'g' })
  })

  it('treats a whitespace-only query as no search at all', () => {
    const onMatchesChange = vi.fn()
    const { container } = render(
      <ForceGraph nodes={nodes} edges={edges} query="   " onMatchesChange={onMatchesChange} />,
    )
    expect(container.querySelector('.fg-node-dim')).toBeNull()
    expect(lastMatch(onMatchesChange)).toEqual({ total: 0, index: -1, id: undefined })
  })

  it('matches on the goal (title), not just the slug', () => {
    const gnodes: FGNode[] = [
      { id: 'g', label: 'tether#7', status: 'running', title: 'fix the parser' },
      { id: 'h', label: 'tether#8', status: 'running', title: 'add a search box' },
    ]
    const { container } = render(<ForceGraph nodes={gnodes} edges={[]} query="parser" />)
    const g = container.querySelectorAll('.fg-node')
    expect(g[0].classList.contains('fg-node-match')).toBe(true)
    expect(g[1].classList.contains('fg-node-dim')).toBe(true)
  })

  // The ORDER is this component's contract to the filter row: matches are sorted
  // by laid-out position (left-to-right by column), so the caller's ↑/↓ walk them
  // in visual reading order. The running column packs leftmost here.
  it('orders matches by laid-out position, and the caller indexes into that order', () => {
    const onMatchesChange = vi.fn()
    const { container, rerender } = render(
      <ForceGraph nodes={nodes} edges={edges} query="#" activeIndex={0} onMatchesChange={onMatchesChange} />,
    )
    expect(lastMatch(onMatchesChange)).toEqual({ total: 3, index: 0, id: 'b' })
    expect(activeSlug(container)).toBe('tether#2') // running col is leftmost
    rerender(<ForceGraph nodes={nodes} edges={edges} query="#" activeIndex={1} onMatchesChange={onMatchesChange} />)
    expect(activeSlug(container)).toBe('tether#3')
    rerender(<ForceGraph nodes={nodes} edges={edges} query="#" activeIndex={2} onMatchesChange={onMatchesChange} />)
    expect(activeSlug(container)).toBe('tether#1')
  })

  // The caller may hand over an index that no longer fits (the match set shrank
  // before its reset landed). The clamp is here, and the index it reports is the
  // clamped one — so a single report never names a position and a card that
  // disagree. (That is a statement about one report. Whether the caller's RENDER
  // of the latest report is current is a two-component question and is covered in
  // WorkGraphView.test.tsx.)
  it('clamps an out-of-range activeIndex and reports the clamped index', () => {
    const onMatchesChange = vi.fn()
    const { container } = render(
      <ForceGraph nodes={nodes} edges={edges} query="#2" activeIndex={9} onMatchesChange={onMatchesChange} />,
    )
    expect(lastMatch(onMatchesChange)).toEqual({ total: 1, index: 0, id: 'b' })
    expect(activeSlug(container)).toBe('tether#2')
  })

  it('asks the caller to reset activeIndex when the match SET changes', () => {
    const onActiveIndexReset = vi.fn()
    const { rerender } = render(
      <ForceGraph nodes={nodes} edges={edges} query="#" onActiveIndexReset={onActiveIndexReset} />,
    )
    const atMount = onActiveIndexReset.mock.calls.length
    // A poll returning the same graph must NOT reset — that is what kept the
    // active match stable in tether#29.
    rerender(
      <ForceGraph
        nodes={nodes.map((n) => ({ ...n }))}
        edges={edges.map((e) => ({ ...e }))}
        query="#"
        onActiveIndexReset={onActiveIndexReset}
      />,
    )
    expect(onActiveIndexReset.mock.calls.length).toBe(atMount)
    // A different query changes the match set → reset.
    rerender(<ForceGraph nodes={nodes} edges={edges} query="#2" onActiveIndexReset={onActiveIndexReset} />)
    expect(onActiveIndexReset.mock.calls.length).toBe(atMount + 1)
  })

  // Nothing matched → the WHOLE map greys out (matches bright, non-matches dim, no
  // exceptions), so "0 results" reads clearly instead of a full-bright map that
  // looks like search did nothing (tether#29 live-verify feedback).
  it('dims the entire map when the query matches nothing', () => {
    const onMatchesChange = vi.fn()
    const { container } = render(
      <ForceGraph nodes={nodes} edges={edges} query="zzz" onMatchesChange={onMatchesChange} />,
    )
    const g = container.querySelectorAll('.fg-node')
    expect(g.length).toBe(3)
    expect([...g].every((n) => n.classList.contains('fg-node-dim'))).toBe(true)
    expect(container.querySelector('.fg-node-match')).toBeNull()
    expect(lastMatch(onMatchesChange)).toEqual({ total: 0, index: -1, id: undefined })
  })

  it('centers the viewport on the active match (viewport center ≈ card center)', () => {
    const { container } = render(<ForceGraph nodes={nodes} edges={edges} query="#2" />)
    const svg = container.querySelector('svg.fg-svg') as SVGSVGElement
    const bNode = container.querySelectorAll('.fg-node')[1]
    const cardW = computePositions(nodes, 0).cardW
    const cardCenterX = txX(bNode) + cardW / 2
    const viewCenterX = vbX(svg) + vbWidth(svg) / 2
    expect(Math.abs(viewCenterX - cardCenterX)).toBeLessThan(1)
  })

  it('preserves the active match across a poll returning a fresh identical array', () => {
    const onMatchesChange = vi.fn()
    const { container, rerender } = render(
      <ForceGraph nodes={nodes} edges={edges} query="#" activeIndex={1} onMatchesChange={onMatchesChange} />,
    )
    expect(activeSlug(container)).toBe('tether#3')
    rerender(
      <ForceGraph
        nodes={nodes.map((n) => ({ ...n }))}
        edges={edges.map((e) => ({ ...e }))}
        query="#"
        activeIndex={1}
        onMatchesChange={onMatchesChange}
      />,
    )
    expect(lastMatch(onMatchesChange)).toEqual({ total: 3, index: 1, id: 'c' })
    expect(activeSlug(container)).toBe('tether#3')
  })

  // Moving the active index doesn't just re-highlight — it re-centers the viewport.
  it('re-centers the viewport when the active match changes', () => {
    const { container, rerender } = render(
      <ForceGraph nodes={nodes} edges={edges} query="#" activeIndex={0} />,
    )
    const svg = container.querySelector('svg.fg-svg') as SVGSVGElement
    const cardW = computePositions(nodes, 0).cardW
    const centerX = () => vbX(svg) + vbWidth(svg) / 2
    const bCenter = txX(container.querySelectorAll('.fg-node')[1]) + cardW / 2
    expect(Math.abs(centerX() - bCenter)).toBeLessThan(1)
    rerender(<ForceGraph nodes={nodes} edges={edges} query="#" activeIndex={1} />)
    const cCenter = txX(container.querySelectorAll('.fg-node')[2]) + cardW / 2
    expect(cCenter).not.toBe(bCenter) // different column → it actually moved
    expect(Math.abs(centerX() - cCenter)).toBeLessThan(1)
  })
})

// Responsive sizing (tether#27): cards shrink to fit the container width so the
// map stays readable in a narrow pane, capped at the natural size.
// (jsdom has no ResizeObserver, so the component falls back to cw=0 / natural
// size — the sizing logic itself is unit-tested here on the pure function.)
describe('computePositions responsive sizing (tether#27)', () => {
  const ns: FGNode[] = [
    { id: 'a', label: 'tether#1', status: 'blocked' },
    { id: 'b', label: 'tether#2', status: 'running' },
    { id: 'c', label: 'tether#3', status: 'queued' },
    { id: 'd', label: 'tether#4', status: 'paused' },
    { id: 'e', label: 'tether#5', status: 'done' },
  ]

  it('caps at the natural card size in a wide container', () => {
    const r = computePositions(ns, 1200)
    expect(r.cardW).toBe(132) // MAX_CARD_W
    expect(r.compact).toBe(false)
  })

  it('shrinks cards + box to fit a narrow container', () => {
    const wide = computePositions(ns, 1200)
    const narrow = computePositions(ns, 380)
    expect(narrow.cardW).toBeLessThan(wide.cardW)
    expect(narrow.box.w).toBeLessThan(wide.box.w)
  })

  it('drops to compact cards below the width threshold', () => {
    const narrow = computePositions(ns, 380) // 5 columns in 380px → tiny cards
    expect(narrow.compact).toBe(true)
    expect(narrow.cardH).toBe(22) // CARD_H_COMPACT
  })

  it('falls back to the natural size when width is unmeasured (cw <= 0)', () => {
    const r = computePositions(ns, 0)
    expect(r.cardW).toBe(132)
    expect(r.compact).toBe(false)
  })

  it('packs present columns adjacently (box width reflects column count)', () => {
    const two = computePositions([ns[0], ns[1]], 1200) // blocked + running only
    const five = computePositions(ns, 1200)
    expect(two.cols.length).toBe(2)
    expect(two.box.w).toBeLessThan(five.box.w)
  })

  // tether#90 pins these three numbers because they are the whole argument for
  // where the map lives: a pane narrower than this shows compact cards (no wi
  // type, 22px tall) whatever else is true. They are consequences of the
  // constants at the top of ForceGraph.tsx, so a change to PAD / COL_MARGIN /
  // COMPACT_THRESHOLD that shifts the usable width shows up here rather than in
  // someone's judgement about whether the map "looks cramped".
  //
  // Do NOT read these as "the middle column is wide enough". At the DEFAULT
  // column widths (lib/layout.ts: left 240, right = 0.56 of the rest) the middle
  // is 478px on a 1440px window and 408px on a 1280px window — both compact with
  // five columns. Full-size cards need the user to have dragged the right
  // divider in. tether#90's report says so explicitly.
  it('pins the container width at which cards stop being compact, per column count', () => {
    const cols = (n: number) => ns.slice(0, n)
    const firstFull = (n: number) => {
      for (let w = 300; w <= 900; w++) if (!computePositions(cols(n), w).compact) return w
      return -1
    }
    expect(firstFull(5)).toBe(606)
    expect(firstFull(4)).toBe(496)
    expect(firstFull(3)).toBe(386)
    // and one either side of the five-column boundary, stated directly
    expect(computePositions(ns, 605).compact).toBe(true)
    expect(computePositions(ns, 606).compact).toBe(false)
  })
})
