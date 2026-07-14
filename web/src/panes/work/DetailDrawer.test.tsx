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
  localStorage.clear()
})

const LS_KEY = 'tether:work-drawer-h'

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

// Draggable height + remembered size (tether#30). jsdom has no layout, so the
// container clientHeight is 0 → the component's `|| 1` fallback makes any drag
// delta blow past the clamps; that's fine for asserting DIRECTION (up → taller /
// max, down → close) and persistence, which is what these cover.
describe('DetailDrawer draggable height (tether#30)', () => {
  const panel = (c: HTMLElement) => c.querySelector('.work-drawer-panel') as HTMLElement
  const grip = (c: HTMLElement) => c.querySelector('.work-drawer-grip') as HTMLElement

  it('starts at the stored height, or the default when none/invalid', () => {
    seed()
    localStorage.setItem(LS_KEY, '60')
    const { container, unmount } = render(<DetailDrawer id="wi_x" onClose={() => {}} />)
    expect(panel(container).style.height).toBe('60%')
    unmount()
    localStorage.clear()
    const a = render(<DetailDrawer id="wi_x" onClose={() => {}} />)
    expect(panel(a.container).style.height).toBe('85%') // DEFAULT_PCT
    a.unmount()
    localStorage.setItem(LS_KEY, '5') // below REST_MIN → rejected → default
    const b = render(<DetailDrawer id="wi_x" onClose={() => {}} />)
    expect(panel(b.container).style.height).toBe('85%')
  })

  it('drags up to a taller height and persists it', () => {
    seed()
    const { container } = render(<DetailDrawer id="wi_x" onClose={() => {}} />)
    fireEvent.pointerDown(grip(container), { clientY: 300 })
    fireEvent.pointerMove(window, { clientY: 100 }) // moved up → taller
    fireEvent.pointerUp(window)
    expect(panel(container).style.height).toBe('92%') // clamped to MAX_PCT
    expect(localStorage.getItem(LS_KEY)).toBe('92')
  })

  it('closes when the grip is dragged down past the rest minimum', () => {
    seed()
    const onClose = vi.fn()
    const { container } = render(<DetailDrawer id="wi_x" onClose={onClose} />)
    fireEvent.pointerDown(grip(container), { clientY: 100 })
    fireEvent.pointerMove(window, { clientY: 400 }) // moved down → below REST_MIN
    fireEvent.pointerUp(window)
    expect(onClose).toHaveBeenCalledTimes(1)
    expect(localStorage.getItem(LS_KEY)).toBeNull() // sub-min height not persisted
  })

  it('does not start a drag when the pointer goes down on the × button', () => {
    seed()
    const onClose = vi.fn()
    const { container } = render(<DetailDrawer id="wi_x" onClose={onClose} />)
    const h0 = panel(container).style.height
    fireEvent.pointerDown(container.querySelector('.work-drawer-grip .icon-btn-sm')!, { clientY: 300 })
    fireEvent.pointerMove(window, { clientY: 50 }) // no drag armed → no resize
    fireEvent.pointerUp(window)
    expect(panel(container).style.height).toBe(h0)
    expect(onClose).not.toHaveBeenCalled() // pointer sequence alone doesn't close
  })

  // review WARN #1: dragging up past the clamp releases the pointer over the
  // backdrop, so the browser fires a click on the OVERLAY — it must not close.
  it('swallows the synthesized click after a drag (drag-to-max does not close)', () => {
    seed()
    const onClose = vi.fn()
    const { container } = render(<DetailDrawer id="wi_x" onClose={onClose} />)
    const overlay = container.querySelector('.work-drawer-overlay')!
    fireEvent.pointerDown(grip(container), { clientY: 300 })
    fireEvent.pointerMove(window, { clientY: 50 }) // drag up
    fireEvent.pointerUp(window)
    fireEvent.click(overlay) // post-drag synthesized click on the overlay
    expect(onClose).not.toHaveBeenCalled()
  })

  // ...but a real backdrop click AFTER a drag still closes (the stale drag flag
  // is cleared on the next pointerdown, so it can't eat a genuine click).
  it('still closes on a genuine backdrop click after a prior drag', () => {
    seed()
    const onClose = vi.fn()
    const { container } = render(<DetailDrawer id="wi_x" onClose={onClose} />)
    const overlay = container.querySelector('.work-drawer-overlay')!
    fireEvent.pointerDown(grip(container), { clientY: 300 })
    fireEvent.pointerMove(window, { clientY: 200 }) // a drag that rests (no overshoot click)
    fireEvent.pointerUp(window)
    fireEvent.pointerDown(overlay) // real backdrop press clears the stale flag
    fireEvent.click(overlay)
    expect(onClose).toHaveBeenCalledTimes(1)
  })

  // review WARN #2: a pointercancel must tear the drag down (no sticky resize).
  it('tears down the drag on pointercancel (no resize on a later stray move)', () => {
    seed()
    const { container } = render(<DetailDrawer id="wi_x" onClose={() => {}} />)
    fireEvent.pointerDown(grip(container), { clientY: 300 })
    fireEvent.pointerMove(window, { clientY: 100 }) // taller
    fireEvent.pointerCancel(window) // gesture cancelled → teardown
    const settled = panel(container).style.height
    fireEvent.pointerMove(window, { clientY: 400 }) // stray move must be ignored
    expect(panel(container).style.height).toBe(settled)
  })
})
