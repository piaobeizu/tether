import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen, fireEvent } from '@testing-library/react'
import { Dag } from './Dag'
import type { DagEdge, DagNode } from './Dag'

const nodes: DagNode[] = [
  { id: 'a', label: 'Node A', status: 'done' },
  { id: 'b', label: 'Node B', status: 'current' },
  { id: 'c', label: 'Node C', status: 'pending' },
]

const edges: DagEdge[] = [
  { from: 'a', to: 'b', kind: 'step' },
  { from: 'b', to: 'c', kind: 'block' },
]

// @testing-library/react's auto-cleanup relies on a global `afterEach`, which
// isn't registered since vitest's `globals` option is off (matches the rest
// of this project — no implicit globals). Clean up explicitly instead.
afterEach(() => {
  cleanup()
})

function vbWidth(svg: SVGSVGElement): number {
  return parseFloat((svg.getAttribute('viewBox') ?? '0 0 0 0').split(/\s+/)[2])
}

describe('Dag', () => {
  it('renders every node label', () => {
    render(<Dag nodes={nodes} edges={edges} />)
    expect(screen.getByText('Node A')).toBeTruthy()
    expect(screen.getByText('Node B')).toBeTruthy()
    expect(screen.getByText('Node C')).toBeTruthy()
  })

  it('calls onSelect with the clicked node id', () => {
    const onSelect = vi.fn()
    render(<Dag nodes={nodes} edges={edges} onSelect={onSelect} />)
    fireEvent.click(screen.getByText('Node B'))
    expect(onSelect).toHaveBeenCalledWith('b')
    expect(onSelect).toHaveBeenCalledTimes(1)
  })

  it('marks the selected node', () => {
    const { container } = render(<Dag nodes={nodes} edges={edges} selectedId="c" />)
    const selected = container.querySelector('.rdag-node-selected')
    expect(selected?.textContent).toContain('Node C')
  })

  // Renders inside a scroll container so many nodes stay reachable (the
  // pane scrolls) rather than being squeezed to fit.
  it('wraps the svg in a scroll container', () => {
    const { container } = render(<Dag nodes={nodes} edges={edges} />)
    const scroll = container.querySelector('.rdag-scroll')
    expect(scroll).toBeTruthy()
    expect(scroll?.querySelector('svg.rdag-svg')).toBeTruthy()
  })

  // Natural size at scale 1: the SVG's pixel width equals its viewBox width
  // (1 user unit == 1 px), so node text renders at its authored size instead
  // of being shrunk to fit the pane.
  it('renders at natural size (svg width == viewBox width) at scale 1', () => {
    const { container } = render(<Dag nodes={nodes} edges={edges} />)
    const svg = container.querySelector('svg.rdag-svg') as SVGSVGElement
    const w = parseFloat(svg.getAttribute('width') ?? '0')
    expect(w).toBeGreaterThan(0)
    expect(w).toBeCloseTo(vbWidth(svg), 3)
  })

  // Plain wheel is left to the scroll container (native pan) — it must not
  // resize/zoom the graph. Only Ctrl/Cmd+wheel zooms (scales the svg).
  it('ignores a plain wheel (native scroll), zooms on Ctrl+wheel', () => {
    const { container } = render(<Dag nodes={nodes} edges={edges} />)
    const svg = container.querySelector('svg.rdag-svg') as SVGSVGElement
    const w0 = parseFloat(svg.getAttribute('width') ?? '0')

    fireEvent.wheel(svg, { deltaY: -100 }) // no modifier → no zoom
    expect(parseFloat(svg.getAttribute('width') ?? '0')).toBeCloseTo(w0, 3)

    fireEvent.wheel(svg, { deltaY: -100, ctrlKey: true }) // zoom in
    expect(parseFloat(svg.getAttribute('width') ?? '0')).toBeGreaterThan(w0)
  })
})
