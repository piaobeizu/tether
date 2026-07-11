import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, fireEvent } from '@testing-library/react'
import ForceGraph from './ForceGraph'
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
