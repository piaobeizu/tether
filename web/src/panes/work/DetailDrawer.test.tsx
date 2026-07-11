import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, fireEvent, screen } from '@testing-library/react'
import DetailDrawer from './DetailDrawer'
import { fetchItem, fetchSteps } from '../../lib/aihub'
import type { WorkItemDetail, WorkSteps } from '../../lib/wire.gen'

// WorkDetail (inside the drawer) fetches item + steps; mock them so the drawer
// tests never hit the network.
vi.mock('../../lib/aihub', async () => {
  const actual = await vi.importActual<typeof import('../../lib/aihub')>('../../lib/aihub')
  return { ...actual, fetchItem: vi.fn(), fetchSteps: vi.fn() }
})

const mockItem = vi.mocked(fetchItem)
const mockSteps = vi.mocked(fetchSteps)

const ITEM: WorkItemDetail = {
  id: 'wi_x',
  slug: 'tether#9',
  goal: 'a goal',
  status: 'queued',
  priority: 'normal',
  wiType: 'feature',
  labels: [],
  content: '',
  currentStepStatus: 'idle',
}
const STEPS: WorkSteps = { nodes: [], degraded: false }

function seed() {
  mockItem.mockResolvedValue(ITEM)
  mockSteps.mockResolvedValue(STEPS)
}

afterEach(() => {
  cleanup()
  vi.clearAllMocks()
})

describe('DetailDrawer (tether#26)', () => {
  it('renders the drawer panel with WorkDetail for the selected wi', async () => {
    seed()
    const { container } = render(<DetailDrawer id="wi_x" onClose={() => {}} />)
    expect(container.querySelector('.work-drawer-panel')).toBeTruthy()
    expect(await screen.findByText('tether#9')).toBeTruthy()
  })

  it('calls onClose when the dimmed backdrop overlay is clicked', () => {
    seed()
    const onClose = vi.fn()
    const { container } = render(<DetailDrawer id="wi_x" onClose={onClose} />)
    fireEvent.click(container.querySelector('.work-drawer-overlay')!)
    expect(onClose).toHaveBeenCalledTimes(1)
  })

  it('does NOT call onClose when the panel itself is clicked (stopPropagation)', () => {
    seed()
    const onClose = vi.fn()
    const { container } = render(<DetailDrawer id="wi_x" onClose={onClose} />)
    fireEvent.click(container.querySelector('.work-drawer-panel')!)
    expect(onClose).not.toHaveBeenCalled()
  })

  it('calls onClose on Escape', () => {
    seed()
    const onClose = vi.fn()
    render(<DetailDrawer id="wi_x" onClose={onClose} />)
    fireEvent.keyDown(document, { key: 'Escape' })
    expect(onClose).toHaveBeenCalledTimes(1)
  })

  it('does NOT close on Escape when escActive is false (drawer behind another tab)', () => {
    seed()
    const onClose = vi.fn()
    render(<DetailDrawer id="wi_x" onClose={onClose} escActive={false} />)
    fireEvent.keyDown(document, { key: 'Escape' })
    expect(onClose).not.toHaveBeenCalled()
  })

  it('calls onClose once when the × button is clicked (does not double-fire via backdrop)', () => {
    seed()
    const onClose = vi.fn()
    const { container } = render(<DetailDrawer id="wi_x" onClose={onClose} />)
    fireEvent.click(container.querySelector('.work-drawer-grip .icon-btn-sm')!)
    expect(onClose).toHaveBeenCalledTimes(1)
  })
})
