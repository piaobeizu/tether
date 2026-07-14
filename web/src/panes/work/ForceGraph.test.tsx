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

// Search / jump (tether#29): a floating box finds a wi in a large map by slug or
// goal substring, highlighting matches, dimming the rest, and centering the
// viewport on the active match; ↑/↓ walk matches, ↵ opens the drawer, Esc clears.
describe('ForceGraph search / jump (tether#29)', () => {
  const input = (c: HTMLElement) => c.querySelector('.fg-search-input') as HTMLInputElement
  const count = (c: HTMLElement) => c.querySelector('.fg-search-count')?.textContent ?? null
  const activeSlug = (c: HTMLElement) =>
    c.querySelector('.fg-node-active .fg-card-slug')?.textContent ?? null

  it('highlights matches, dims the rest, and shows a count', () => {
    const { container } = render(<ForceGraph nodes={nodes} edges={edges} />)
    fireEvent.change(input(container), { target: { value: '#2' } })
    const g = container.querySelectorAll('.fg-node')
    expect(g[1].classList.contains('fg-node-match')).toBe(true) // 'b' tether#2
    expect(g[0].classList.contains('fg-node-dim')).toBe(true) // 'a' tether#1
    expect(g[2].classList.contains('fg-node-dim')).toBe(true) // 'c' tether#3
    expect(g[1].classList.contains('fg-node-dim')).toBe(false)
    expect(count(container)).toBe('1/1')
    expect(activeSlug(container)).toBe('tether#2')
  })

  it('matches on the goal (title), not just the slug', () => {
    const gnodes: FGNode[] = [
      { id: 'g', label: 'tether#7', status: 'running', title: 'fix the parser' },
      { id: 'h', label: 'tether#8', status: 'running', title: 'add a search box' },
    ]
    const { container } = render(<ForceGraph nodes={gnodes} edges={[]} />)
    fireEvent.change(input(container), { target: { value: 'parser' } })
    const g = container.querySelectorAll('.fg-node')
    expect(g[0].classList.contains('fg-node-match')).toBe(true)
    expect(g[1].classList.contains('fg-node-dim')).toBe(true)
    expect(count(container)).toBe('1/1')
  })

  // Matches walk in visual reading order (left-to-right by column): the running
  // column packs leftmost, so tether#2 is match #1; ↓ wraps forward, ↑ wraps back.
  it('walks matches with ArrowDown / ArrowUp, wrapping at both ends', () => {
    const { container } = render(<ForceGraph nodes={nodes} edges={edges} />)
    fireEvent.change(input(container), { target: { value: '#' } }) // all 3 match
    expect(count(container)).toBe('1/3')
    expect(activeSlug(container)).toBe('tether#2') // running col is leftmost
    fireEvent.keyDown(input(container), { key: 'ArrowDown' })
    expect(count(container)).toBe('2/3')
    expect(activeSlug(container)).toBe('tether#3')
    fireEvent.keyDown(input(container), { key: 'ArrowDown' })
    expect(activeSlug(container)).toBe('tether#1')
    fireEvent.keyDown(input(container), { key: 'ArrowDown' }) // forward wrap
    expect(count(container)).toBe('1/3')
    expect(activeSlug(container)).toBe('tether#2')
    fireEvent.keyDown(input(container), { key: 'ArrowUp' }) // backward wrap
    expect(count(container)).toBe('3/3')
    expect(activeSlug(container)).toBe('tether#1')
  })

  it('opens the active match on Enter (onSelect)', () => {
    const onSelect = vi.fn()
    const { container } = render(<ForceGraph nodes={nodes} edges={edges} onSelect={onSelect} />)
    fireEvent.change(input(container), { target: { value: '#3' } })
    fireEvent.keyDown(input(container), { key: 'Enter' })
    expect(onSelect).toHaveBeenCalledWith('c')
  })

  it('clears the search on Escape', () => {
    const { container } = render(<ForceGraph nodes={nodes} edges={edges} />)
    fireEvent.change(input(container), { target: { value: '#2' } })
    expect(container.querySelector('.fg-node-dim')).not.toBeNull()
    fireEvent.keyDown(input(container), { key: 'Escape' })
    expect(input(container).value).toBe('')
    expect(container.querySelector('.fg-node-dim')).toBeNull()
    expect(container.querySelector('.fg-search-count')).toBeNull()
  })

  // Nothing matched → the WHOLE map greys out (matches bright, non-matches dim, no
  // exceptions), so "0 results" reads clearly instead of a full-bright map that
  // looks like search did nothing (tether#29 live-verify feedback).
  it('dims the entire map when the query matches nothing', () => {
    const { container } = render(<ForceGraph nodes={nodes} edges={edges} />)
    fireEvent.change(input(container), { target: { value: 'zzz' } })
    const g = container.querySelectorAll('.fg-node')
    expect(g.length).toBe(3)
    expect([...g].every((n) => n.classList.contains('fg-node-dim'))).toBe(true)
    expect(container.querySelector('.fg-node-match')).toBeNull()
    expect(count(container)).toBe('0')
  })

  it('centers the viewport on the active match (viewport center ≈ card center)', () => {
    const { container } = render(<ForceGraph nodes={nodes} edges={edges} />)
    const svg = container.querySelector('svg.fg-svg') as SVGSVGElement
    fireEvent.change(input(container), { target: { value: '#2' } }) // active = 'b'
    const bNode = container.querySelectorAll('.fg-node')[1]
    const cardW = computePositions(nodes, 0).cardW
    const cardCenterX = txX(bNode) + cardW / 2
    const viewCenterX = vbX(svg) + vbWidth(svg) / 2
    expect(Math.abs(viewCenterX - cardCenterX)).toBeLessThan(1)
  })

  it('preserves the active match across a poll returning a fresh identical array', () => {
    const { container, rerender } = render(<ForceGraph nodes={nodes} edges={edges} />)
    fireEvent.change(input(container), { target: { value: '#' } })
    fireEvent.keyDown(input(container), { key: 'ArrowDown' }) // active idx 1 → '2/3'
    expect(count(container)).toBe('2/3')
    rerender(<ForceGraph nodes={nodes.map((n) => ({ ...n }))} edges={edges.map((e) => ({ ...e }))} />)
    expect(count(container)).toBe('2/3')
    expect(activeSlug(container)).toBe('tether#3')
  })

  // ↑/↓ don't just re-highlight — they re-center the viewport on each match.
  it('re-centers the viewport when navigating to a different match', () => {
    const { container } = render(<ForceGraph nodes={nodes} edges={edges} />)
    const svg = container.querySelector('svg.fg-svg') as SVGSVGElement
    const cardW = computePositions(nodes, 0).cardW
    const centerX = () => vbX(svg) + vbWidth(svg) / 2
    fireEvent.change(input(container), { target: { value: '#' } }) // active = tether#2 (node b)
    const bCenter = txX(container.querySelectorAll('.fg-node')[1]) + cardW / 2
    expect(Math.abs(centerX() - bCenter)).toBeLessThan(1)
    fireEvent.keyDown(input(container), { key: 'ArrowDown' }) // active = tether#3 (node c)
    const cCenter = txX(container.querySelectorAll('.fg-node')[2]) + cardW / 2
    expect(cCenter).not.toBe(bCenter) // different column → it actually moved
    expect(Math.abs(centerX() - cCenter)).toBeLessThan(1)
  })

  // Esc clears a non-empty search WITHOUT bubbling — else it would also slam an
  // open detail drawer shut (#26 DetailDrawer's document-level Esc) in one press.
  it('consumes Escape (stopPropagation) when clearing a non-empty search', () => {
    const docEsc = vi.fn()
    const h = (e: KeyboardEvent) => {
      if (e.key === 'Escape') docEsc()
    }
    document.addEventListener('keydown', h)
    try {
      const { container } = render(<ForceGraph nodes={nodes} edges={edges} />)
      fireEvent.change(input(container), { target: { value: '#2' } })
      fireEvent.keyDown(input(container), { key: 'Escape' })
      expect(input(container).value).toBe('')
      expect(docEsc).not.toHaveBeenCalled()
    } finally {
      document.removeEventListener('keydown', h)
    }
  })

  // But an EMPTY search box lets Esc bubble, so it can still close a drawer.
  it('lets Escape bubble when the search box is empty', () => {
    const docEsc = vi.fn()
    const h = (e: KeyboardEvent) => {
      if (e.key === 'Escape') docEsc()
    }
    document.addEventListener('keydown', h)
    try {
      const { container } = render(<ForceGraph nodes={nodes} edges={edges} />)
      fireEvent.keyDown(input(container), { key: 'Escape' }) // empty query
      expect(docEsc).toHaveBeenCalled()
    } finally {
      document.removeEventListener('keydown', h)
    }
  })

  // The container widens the node set while a search is active (tether#29): the
  // trimmed/lowercased query is reported up via onQueryChange so WorkGraphView can
  // bypass the active/done filter and let search span the whole project.
  it('reports the trimmed query to the parent via onQueryChange', () => {
    const onQueryChange = vi.fn()
    const { container } = render(
      <ForceGraph nodes={nodes} edges={edges} onQueryChange={onQueryChange} />,
    )
    expect(onQueryChange).toHaveBeenLastCalledWith('') // mount: no active search
    fireEvent.change(input(container), { target: { value: '  #2  ' } })
    expect(onQueryChange).toHaveBeenLastCalledWith('#2') // trimmed + lowercased
    fireEvent.keyDown(input(container), { key: 'Escape' })
    expect(onQueryChange).toHaveBeenLastCalledWith('')
  })
})

// Responsive sizing (tether#27): cards shrink to fit the container width so the
// map stays readable in the narrow right Work tab, capped at the natural size.
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
})
