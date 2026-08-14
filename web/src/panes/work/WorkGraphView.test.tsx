import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import WorkGraphView from './WorkGraphView'
import { useStore } from '../../lib/store'
import { fetchDeps, fetchGraph } from '../../lib/aihub'
import type { WorkGraph } from '../../lib/wire.gen'

// WorkGraphView owns the graph fetch; mock the client so these never hit the
// network. ForceGraph itself is NOT mocked — the point of this file is the
// wiring between the filter row's search box and the real renderer, which is
// exactly the hop that a component-in-isolation test cannot see.
vi.mock('../../lib/aihub', async () => {
  const actual = await vi.importActual<typeof import('../../lib/aihub')>('../../lib/aihub')
  return { ...actual, fetchGraph: vi.fn(), fetchDeps: vi.fn() }
})

const mockGraph = vi.mocked(fetchGraph)
const mockDeps = vi.mocked(fetchDeps)

// 'wrapped' is terminal, so tether#3 is hidden by the default 'active' filter —
// which is what makes the "search spans the whole project" assertion meaningful.
const GRAPH: WorkGraph = {
  nodes: [
    { id: 'a', slug: 'tether#1', goal: 'first thing', status: 'running', priority: 'normal', wiType: 'feature' },
    { id: 'b', slug: 'tether#2', goal: 'second thing', status: 'queued', priority: 'normal', wiType: 'fix_bug' },
    { id: 'c', slug: 'tether#3', goal: 'third thing', status: 'wrapped', priority: 'normal', wiType: 'feature' },
  ],
}

function seed() {
  // A FRESH object per call, like a real poll: returning the same reference
  // would let React bail out of the re-render and make poll-stability tests
  // pass without ever exercising the stability.
  mockGraph.mockImplementation(async () => ({ nodes: GRAPH.nodes.map((n) => ({ ...n })) }))
  mockDeps.mockResolvedValue({ blockedBy: [], blocking: [] })
  useStore.setState({ workProject: 'tether', selectedWiId: null })
}

/** Fire the poll the component listens for (it reloads whenever the document
 *  becomes visible; jsdom reports 'visible'), and wait for the refetch. */
async function poll() {
  const before = mockGraph.mock.calls.length
  fireEvent(document, new Event('visibilitychange'))
  await waitFor(() => expect(mockGraph.mock.calls.length).toBeGreaterThan(before))
}

afterEach(() => {
  cleanup()
  vi.clearAllMocks()
  useStore.setState({ workProject: '', selectedWiId: null })
})

const input = (c: HTMLElement) => c.querySelector('.fg-search-input') as HTMLInputElement
const searchCount = (c: HTMLElement) => c.querySelector('.fg-search-count')?.textContent ?? null
const activeSlug = (c: HTMLElement) =>
  c.querySelector('.fg-node-active .fg-card-slug')?.textContent ?? null
const slugs = (c: HTMLElement, sel: string) =>
  [...c.querySelectorAll(sel)].map((n) => n.querySelector('.fg-card-slug')?.textContent)

/** Render and wait for the lazy <ForceGraph> to resolve and paint its cards. */
async function renderGraph() {
  const r = render(<WorkGraphView />)
  await screen.findByText('tether#1')
  return r
}

describe('WorkGraphView search box placement (tether#90)', () => {
  it('renders the search box inside the filter row, not over the map', async () => {
    seed()
    const { container } = await renderGraph()
    const box = container.querySelector('.fg-search')
    expect(box).not.toBeNull()
    // The placement claim, stated structurally: it is a descendant of the filter
    // row and NOT of the graph surface. Before tether#90 it was the other way
    // round, absolutely positioned at top-left of .fg-scroll where it covered the
    // two leftmost status columns at every pane width.
    expect(container.querySelector('.fg-filter')!.contains(box!)).toBe(true)
    expect(container.querySelector('.fg-scroll')!.contains(box!)).toBe(false)
    expect(container.querySelectorAll('.fg-scroll input').length).toBe(0)
  })

  it('sits after the mode/type controls and before the filter count', async () => {
    seed()
    const { container } = await renderGraph()
    const row = [...container.querySelector('.fg-filter')!.children]
    expect(row.findIndex((e) => e.classList.contains('fg-seg-group')))
      .toBeLessThan(row.findIndex((e) => e.classList.contains('fg-search')))
    expect(row.findIndex((e) => e.classList.contains('fg-search')))
      .toBeLessThan(row.findIndex((e) => e.classList.contains('fg-filter-count')))
  })
})

describe('WorkGraphView search wiring (tether#29 behaviour, tether#90 wiring)', () => {
  it('typing in the filter row highlights matches and dims the rest of the real map', async () => {
    seed()
    const { container } = await renderGraph()
    fireEvent.change(input(container), { target: { value: '#2' } })
    await waitFor(() => expect(container.querySelector('.fg-node-match')).not.toBeNull())
    expect(slugs(container, '.fg-node-match')).toEqual(['tether#2'])
    expect(slugs(container, '.fg-node-dim').sort()).toEqual(['tether#1', 'tether#3'])
    expect(searchCount(container)).toBe('1/1')
  })

  // The whole reason the query had to be ONE piece of state: an active search
  // widens the node set to the entire project, so a terminal wi hidden by the
  // default 'active' filter is still findable (tether#29 live-verify). Before
  // tether#90 that was a mirror of ForceGraph's own copy; now the filter and the
  // renderer read the same string, and this is what would break if they didn't.
  it('an active search reaches a wi the active filter hides', async () => {
    seed()
    const { container } = await renderGraph()
    expect(screen.queryByText('tether#3')).toBeNull() // terminal → filtered out
    fireEvent.change(input(container), { target: { value: '#3' } })
    expect(await screen.findByText('tether#3')).toBeTruthy()
    await waitFor(() => expect(slugs(container, '.fg-node-match')).toEqual(['tether#3']))
    expect(container.querySelector('.fg-filter-count')?.textContent).toBe('search · all')
  })

  it('mode and type controls go inert while a search is active', async () => {
    seed()
    const { container } = await renderGraph()
    expect([...container.querySelectorAll('.fg-seg')].every((b) => (b as HTMLButtonElement).disabled)).toBe(false)
    fireEvent.change(input(container), { target: { value: 'thing' } })
    await waitFor(() =>
      expect([...container.querySelectorAll('.fg-seg')].every((b) => (b as HTMLButtonElement).disabled)).toBe(true),
    )
    expect((container.querySelector('.fg-type-select') as HTMLSelectElement).disabled).toBe(true)
  })

  // Columns pack left-to-right: running, then ready, then done. ↑/↓ must walk the
  // matches in that visual order — the ordering lives in ForceGraph's layout, the
  // keys are handled up here, and this is the only test that sees both halves.
  it('ArrowDown / ArrowUp walk matches in reading order and wrap at both ends', async () => {
    seed()
    const { container } = await renderGraph()
    fireEvent.change(input(container), { target: { value: 'tether' } })
    await waitFor(() => expect(searchCount(container)).toBe('1/3'))
    expect(activeSlug(container)).toBe('tether#1')

    fireEvent.keyDown(input(container), { key: 'ArrowDown' })
    await waitFor(() => expect(searchCount(container)).toBe('2/3'))
    expect(activeSlug(container)).toBe('tether#2')

    fireEvent.keyDown(input(container), { key: 'ArrowDown' })
    await waitFor(() => expect(activeSlug(container)).toBe('tether#3'))

    fireEvent.keyDown(input(container), { key: 'ArrowDown' }) // forward wrap
    await waitFor(() => expect(searchCount(container)).toBe('1/3'))
    expect(activeSlug(container)).toBe('tether#1')

    fireEvent.keyDown(input(container), { key: 'ArrowUp' }) // backward wrap
    await waitFor(() => expect(searchCount(container)).toBe('3/3'))
    expect(activeSlug(container)).toBe('tether#3')
  })

  it('Enter opens the active match (shared selection)', async () => {
    seed()
    const { container } = await renderGraph()
    fireEvent.change(input(container), { target: { value: '#2' } })
    await waitFor(() => expect(searchCount(container)).toBe('1/1'))
    fireEvent.keyDown(input(container), { key: 'Enter' })
    expect(useStore.getState().selectedWiId).toBe('b')
  })

  // A query that changes the match SET resets the walk to the first match. The
  // numbers are chosen so a reset and a mere clamp give different answers: from
  // 3/3 (index 2), narrowing to a 2-match set would land on 2/2 if the index were
  // only clamped, and lands on 1/2 because it is genuinely reset.
  it('a query that changes the match set resets the walk to the first match', async () => {
    seed()
    const { container } = await renderGraph()
    fireEvent.change(input(container), { target: { value: 'thing' } }) // a, b, c
    await waitFor(() => expect(searchCount(container)).toBe('1/3'))
    fireEvent.keyDown(input(container), { key: 'ArrowDown' })
    fireEvent.keyDown(input(container), { key: 'ArrowDown' })
    await waitFor(() => expect(searchCount(container)).toBe('3/3'))
    expect(activeSlug(container)).toBe('tether#3')

    fireEvent.change(input(container), { target: { value: 'ir' } }) // first/third → a, c
    await waitFor(() => expect(searchCount(container)).toBe('1/2'))
    expect(activeSlug(container)).toBe('tether#1')
  })

  // Editing the query restarts the walk at the first match, even when the match
  // set happens to be identical — the same way a browser's find-in-page does.
  // Restarting on the EDIT (rather than waiting for the match set to change) is
  // also what keeps a narrowing keystroke from committing a frame that names a
  // position the new set does not have; see the onChange handler.
  it('every query edit restarts the walk at the first match', async () => {
    seed()
    const { container } = await renderGraph()
    fireEvent.change(input(container), { target: { value: 'thing' } })
    await waitFor(() => expect(searchCount(container)).toBe('1/3'))
    fireEvent.keyDown(input(container), { key: 'ArrowDown' })
    await waitFor(() => expect(searchCount(container)).toBe('2/3'))
    fireEvent.change(input(container), { target: { value: 'tether' } }) // same set: a, b, c
    await waitFor(() => expect(input(container).value).toBe('tether'))
    expect(searchCount(container)).toBe('1/3')
    expect(activeSlug(container)).toBe('tether#1')
  })

  // The invariant that actually has to hold across the lift (tether#29): the 8s
  // poll must not disturb an in-progress search. Before tether#90 both the query
  // and the walk lived inside a component the poll re-rendered; now they live in
  // the parent and are fed back down, so this is worth asserting end to end
  // rather than trusting the memo keys.
  it('a poll returning the same graph does not disturb an in-progress search', async () => {
    seed()
    const { container } = await renderGraph()
    fireEvent.change(input(container), { target: { value: 'thing' } })
    await waitFor(() => expect(searchCount(container)).toBe('1/3'))
    fireEvent.keyDown(input(container), { key: 'ArrowDown' })
    await waitFor(() => expect(searchCount(container)).toBe('2/3'))

    await poll()

    expect(input(container).value).toBe('thing')
    expect(searchCount(container)).toBe('2/3')
    expect(activeSlug(container)).toBe('tether#2')
  })

  it('Escape clears the query, the dimming and the counter', async () => {
    seed()
    const { container } = await renderGraph()
    fireEvent.change(input(container), { target: { value: '#2' } })
    await waitFor(() => expect(container.querySelector('.fg-node-dim')).not.toBeNull())
    fireEvent.keyDown(input(container), { key: 'Escape' })
    expect(input(container).value).toBe('')
    await waitFor(() => expect(container.querySelector('.fg-node-dim')).toBeNull())
    expect(container.querySelector('.fg-search-count')).toBeNull()
  })

  // Esc clears a non-empty search WITHOUT bubbling — else it would also slam an
  // open detail drawer shut (tether#26 DetailDrawer's document-level Esc) in one
  // press. An EMPTY box lets it through so the drawer can still be closed.
  it('consumes Escape only when there is a query to clear', async () => {
    seed()
    const docEsc = vi.fn()
    const h = (e: KeyboardEvent) => { if (e.key === 'Escape') docEsc() }
    document.addEventListener('keydown', h)
    try {
      const { container } = await renderGraph()
      fireEvent.keyDown(input(container), { key: 'Escape' }) // empty
      expect(docEsc).toHaveBeenCalledTimes(1)

      fireEvent.change(input(container), { target: { value: '#2' } })
      fireEvent.keyDown(input(container), { key: 'Escape' }) // non-empty
      expect(docEsc).toHaveBeenCalledTimes(1) // still one — this one was consumed
      expect(input(container).value).toBe('')
    } finally {
      document.removeEventListener('keydown', h)
    }
  })
})
