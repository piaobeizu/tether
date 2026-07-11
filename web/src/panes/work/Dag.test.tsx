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
    const selected = container.querySelector('.dag-node-selected')
    expect(selected?.textContent).toContain('Node C')
  })
})
