import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen, fireEvent } from '@testing-library/react'
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

describe('ForceGraph', () => {
  it('renders a dot for every node', () => {
    const { container } = render(<ForceGraph nodes={nodes} edges={edges} />)
    expect(container.querySelectorAll('.fg-node-dot').length).toBe(3)
  })

  // Semantic zoom: labels are hidden at the default fit-scale to avoid overlap.
  it('hides node labels at default zoom', () => {
    const { container } = render(<ForceGraph nodes={nodes} edges={edges} />)
    expect(container.querySelectorAll('.fg-node-label').length).toBe(0)
  })

  it('shows the selected node label', () => {
    render(<ForceGraph nodes={nodes} edges={edges} selectedId="b" />)
    expect(screen.getByText('tether#2')).toBeTruthy()
  })

  it('shows a node label on hover', () => {
    const { container } = render(<ForceGraph nodes={nodes} edges={edges} />)
    const first = container.querySelectorAll('.fg-node')[0]
    fireEvent.pointerOver(first)
    expect(screen.getByText('tether#1')).toBeTruthy()
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

  it('clusters nodes into status columns (x ordered by status)', () => {
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
})
