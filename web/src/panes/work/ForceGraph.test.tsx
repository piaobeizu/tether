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

function vb(svg: SVGSVGElement) {
  return (svg.getAttribute('viewBox') ?? '0 0 0 0').split(/\s+/).map(Number)
}

describe('ForceGraph', () => {
  it('renders every node label', () => {
    render(<ForceGraph nodes={nodes} edges={edges} />)
    expect(screen.getByText('tether#1')).toBeTruthy()
    expect(screen.getByText('tether#2')).toBeTruthy()
    expect(screen.getByText('tether#3')).toBeTruthy()
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
    render(<ForceGraph nodes={nodes} edges={edges} onSelect={onSelect} />)
    fireEvent.click(screen.getByText('tether#2'))
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
    const nodeB = screen.getByText('tether#2')

    fireEvent.pointerDown(svg, { clientX: 10, clientY: 10 })
    fireEvent.pointerMove(svg, { clientX: 80, clientY: 80 })
    fireEvent.pointerUp(svg, { clientX: 80, clientY: 80 })
    fireEvent.click(nodeB)
    expect(onSelect).not.toHaveBeenCalled()

    fireEvent.pointerDown(svg, { clientX: 10, clientY: 10 })
    fireEvent.pointerUp(svg, { clientX: 10, clientY: 10 })
    fireEvent.click(nodeB)
    expect(onSelect).toHaveBeenCalledWith('b')
  })

  it('zooms the viewBox on wheel', () => {
    const { container } = render(<ForceGraph nodes={nodes} edges={edges} />)
    const svg = container.querySelector('svg.fg-svg') as SVGSVGElement
    const w0 = vb(svg)[2]
    fireEvent.wheel(svg, { deltaY: 100 }) // zoom out → wider viewBox
    expect(vb(svg)[2]).toBeGreaterThan(w0)
  })
})
