import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react'
import Canvas from './index'
import { useStore } from '../../lib/store'
import { fetchFile, fetchWorkspaces } from '../../lib/aihub'

// Mock the aihub client so FileMode/CanvasHome never hit the network — only
// fetchFile + fetchWorkspaces are exercised by these tests. fetchItem/fetchSteps
// are mocked too since Canvas imports them, but no test here selects a work item.
vi.mock('../../lib/aihub', async () => {
  const actual = await vi.importActual<typeof import('../../lib/aihub')>('../../lib/aihub')
  return {
    ...actual,
    fetchItem: vi.fn(),
    fetchSteps: vi.fn(),
    fetchFile: vi.fn(),
    fetchWorkspaces: vi.fn(),
  }
})

const mockFetchFile = vi.mocked(fetchFile)
const mockFetchWorkspaces = vi.mocked(fetchWorkspaces)

// ── Why this file needs a warmup and wider budgets (tether#65) ───────────────
//
// panes/canvas/index.tsx renders markdown through `lazy()` + <Suspense>, so every
// assertion below that looks for rendered `.md` output — or for the `.md-body`
// container, which exists ONLY inside <Markdown> — cannot be satisfied until
// vitest has resolved that dynamic import. Resolving it means transforming
// react-markdown + remark-gfm + rehype-highlight, a whole module graph, on first
// use.
//
// @testing-library's default 1000ms `findBy*` budget is calibrated for a state
// update settling — a microtask or two. Measuring a module-graph load against an
// update-sized budget is a category error, and it showed up as a flake: tether#63's
// verify run saw `findByText('Title')` time out and redden a PR that had not
// touched this pane. Only the FIRST crossing pays the cost (React caches the
// resolved lazy payload), which is why it was always that one assertion.
//
// Two changes, and it is worth being exact about which one does the work, because
// the first attempt at this fix was the timeout alone and it was measured failing:
//
//  1. warmUpMarkdown() below pays the import ONCE, in beforeAll. This is the
//     actual fix — it takes the module load off every test's critical path instead
//     of making the window bigger. Measured cost of that one load: ~370ms idle,
//     ~1.2s with 24 busy loops on 12 CPUs.
//  2. The budgets exist mainly FOR THE WARMUP, which is now the only place a
//     module-graph load is awaited. They were sized against what that load
//     actually cost when it still sat inside a per-test assertion: 4639ms and
//     4597ms under contention on this host — which is why a budget picked to fit
//     under vitest's default 5000ms `testTimeout` was measured FAILING 2 of 6
//     contended runs. Raising the enclosing `it()`/hook timeouts alongside is what
//     makes 20000 a number that can actually be reached rather than one the
//     harness silently caps.
//
// The per-assertion copies of the budget are a backstop, not the mechanism: once
// the module is cached, satisfying these assertions was measured at 29ms idle and
// 80ms under the same contention, so the default 1000ms would in fact do. They are
// kept so that deleting the warmup cannot quietly restore the flake, and so each
// site says why it is not on the default.
//
// These stay per-assertion / per-test rather than a global `testTimeout` or
// `asyncUtilTimeout` bump so the widening is visible exactly where it is
// justified, and a genuinely hung assertion anywhere else still fails fast. The
// happy path is unaffected: a `findBy*` returns as soon as the element appears, so
// a larger ceiling costs nothing when things work.
const LAZY_BUDGET_MS = 20_000
const LAZY_TEST_TIMEOUT_MS = 30_000

// warmUpMarkdown resolves the lazy <Markdown> boundary once, before any test runs.
//
// It goes through <Canvas> itself rather than calling `import('./Markdown')`
// directly, deliberately: an explicit import here would duplicate a specifier that
// lives in index.tsx, and if that file ever lazy-imports a different path this
// warmup would still succeed while silently warming the wrong module — a guard
// that has quietly stopped guarding. Driving the real component means the warmup
// exercises whatever index.tsx actually imports, whatever that becomes.
async function warmUpMarkdown() {
  mockFetchFile.mockResolvedValue({ path: 'warmup.md', content: '# warmup', truncated: false })
  useStore.getState().select({ file: { wsId: 'warmup', path: 'warmup.md' } })
  render(<Canvas />)
  // Matching on the rendered <h1> text (not the raw '# warmup' the <pre> fallback
  // shows) is what makes this wait for the boundary rather than for the fallback.
  await screen.findByText('warmup', undefined, { timeout: LAZY_BUDGET_MS })
  cleanup()
  useStore.getState().select(null)
  // mockReset, not clearAllMocks: the latter drops call history but KEEPS the
  // resolved value set above, which would leak 'warmup.md' into any later test that
  // renders without stubbing fetchFile itself.
  mockFetchFile.mockReset()
}

beforeAll(warmUpMarkdown, LAZY_TEST_TIMEOUT_MS)

// @testing-library/react's auto-cleanup relies on a global `afterEach`, which
// isn't registered since vitest's `globals` option is off (matches Dag.test.tsx
// — no implicit globals). Clean up explicitly instead, and reset the shared
// zustand store's selection so tests don't leak into each other.
afterEach(() => {
  cleanup()
  useStore.getState().select(null)
  // tether#129 — `activeWorkspace` is on the same zustand singleton as the
  // selection, so a test that publishes one must not leak it into the next file
  // or the next test (WorkspacePane.test.tsx resets it for the same reason).
  // Reset here rather than in the one describe that writes it, so a later test
  // added below cannot inherit a workspace it never set.
  useStore.setState({ activeWorkspace: null })
  vi.clearAllMocks()
})

describe('Canvas — FileMode markdown rendering (tether#21)', () => {
  it('renders a .md file as real markdown, not a <pre> block', async () => {
    mockFetchFile.mockResolvedValue({
      path: 'a.md',
      content: '# Title\n\n- item',
      truncated: false,
    })
    useStore.getState().select({ file: { wsId: 'ws1', path: 'a.md' } })

    const { container } = render(<Canvas />)

    // Crosses the lazy() boundary: the <pre> Suspense fallback holds the RAW
    // '# Title\n\n- item', so nothing matches 'Title' exactly until <Markdown>
    // has loaded and rendered the <h1>. See LAZY_BUDGET_MS.
    await screen.findByText('Title', undefined, { timeout: LAZY_BUDGET_MS })
    const h1 = container.querySelector('h1')
    expect(h1?.textContent).toBe('Title')

    const li = container.querySelector('li')
    expect(li?.textContent).toBe('item')

    // must NOT be wrapped in the plain-text <pre> fallback once resolved
    expect(container.querySelector('pre')).toBeNull()
  }, LAZY_TEST_TIMEOUT_MS)

  it('renders a non-markdown file in a <pre> block, unrendered', async () => {
    mockFetchFile.mockResolvedValue({
      path: 'main.go',
      content: 'package main',
      truncated: false,
    })
    useStore.getState().select({ file: { wsId: 'ws1', path: 'main.go' } })

    const { container } = render(<Canvas />)

    await screen.findByText('package main')
    const pre = container.querySelector('pre')
    expect(pre?.textContent).toBe('package main')
    expect(container.querySelector('h1')).toBeNull()
  })
})

describe('Canvas — FileMode header is a single full-path line (tether#21)', () => {
  it('shows just the name for a root file (path == filename)', async () => {
    mockFetchFile.mockResolvedValue({
      path: 'CHANGELOG.md',
      content: 'notes',
      truncated: false,
    })
    useStore.getState().select({ file: { wsId: 'ws1', path: 'CHANGELOG.md' } })

    const { container } = render(<Canvas />)

    await screen.findByText('notes')
    expect(container.querySelector('.canvas-slug')?.textContent).toBe('CHANGELOG.md')
    // no separate basename / duplicate line
    expect(container.querySelector('.canvas-file-path')).toBeNull()
  })

  it('shows the full relative path (no separate basename) for a nested file', async () => {
    mockFetchFile.mockResolvedValue({
      path: 'internal/x.go',
      content: 'package internal',
      truncated: false,
    })
    useStore.getState().select({ file: { wsId: 'ws1', path: 'internal/x.go' } })

    const { container } = render(<Canvas />)

    await screen.findByText('package internal')
    expect(container.querySelector('.canvas-slug')?.textContent).toBe('internal/x.go')
    expect(container.querySelector('.canvas-file-path')).toBeNull()
  })
})

describe('Canvas — markdown XSS safety (tether#21)', () => {
  it('renders an <img onerror> payload inert, no script execution surface', async () => {
    mockFetchFile.mockResolvedValue({
      path: 'evil.md',
      content: '<img src=x onerror="alert(1)">',
      truncated: false,
    })
    useStore.getState().select({ file: { wsId: 'ws1', path: 'evil.md' } })

    const { container } = render(<Canvas />)

    // react-markdown's default (no rehype-raw) escapes embedded HTML — it
    // never becomes a real <img> element with a live onerror handler.
    // `selector: '.md-body'` restricts the match to that single container so
    // the substring matcher doesn't hit every ancestor element too.
    // `.md-body` only exists inside the lazily-imported <Markdown> — see LAZY_BUDGET_MS.
    await screen.findByText(t => t.includes('onerror'), { selector: '.md-body' }, { timeout: LAZY_BUDGET_MS })
    expect(container.querySelector('img')).toBeNull()
  }, LAZY_TEST_TIMEOUT_MS)

  it('renders a <script> payload as inert text, never as a real script element', async () => {
    mockFetchFile.mockResolvedValue({
      path: 'evil2.md',
      content: '<script>alert(1)</script>',
      truncated: false,
    })
    useStore.getState().select({ file: { wsId: 'ws1', path: 'evil2.md' } })

    const { container } = render(<Canvas />)

    // `.md-body` only exists inside the lazily-imported <Markdown> — see LAZY_BUDGET_MS.
    await screen.findByText(t => t.includes('alert(1)'), { selector: '.md-body' }, { timeout: LAZY_BUDGET_MS })
    expect(container.querySelector('script')).toBeNull()
  }, LAZY_TEST_TIMEOUT_MS)
})

describe('Canvas — home when nothing is selected (tether#33)', () => {
  it('renders the branded home with quick actions (not a lone hint)', () => {
    mockFetchWorkspaces.mockResolvedValue([])
    render(<Canvas />)
    expect(screen.getByText('tether')).toBeTruthy()
    expect(screen.getByText('Chat')).toBeTruthy()
    expect(screen.getByText('Pick a wi')).toBeTruthy()
    expect(screen.getByText('Open file')).toBeTruthy()
  })

  it('shows the primary workspace name + path (and count when multiple)', async () => {
    mockFetchWorkspaces.mockResolvedValue([
      { id: 'w1', name: 'tether', path: '/root/code/tether' },
      { id: 'w2', name: 'aihub', path: '/root/code/aihub' },
    ])
    const { container } = render(<Canvas />)
    await screen.findByText('/root/code/tether')
    const wsText = container.querySelector('.canvas-home-ws')?.textContent ?? ''
    expect(wsText).toContain('workspace')
    expect(wsText).toContain('tether')
    expect(wsText).toContain('+1 more')
  })

  it('still renders home (brand + actions) when the workspace fetch fails, with no workspace line', async () => {
    mockFetchWorkspaces.mockRejectedValue(new Error('boom'))
    const { container } = render(<Canvas />)
    expect(screen.getByText('tether')).toBeTruthy()
    expect(screen.getByText('Chat')).toBeTruthy()
    // Flush microtasks + a macrotask so the rejected fetch fully settles; this
    // would catch a regression where the .catch erroneously set workspace state.
    await new Promise((r) => setTimeout(r, 0))
    expect(container.querySelector('.canvas-home-ws')).toBeNull()
  })

  // tether#129 defect 3 — the home called `ws[0]` "the workspace" and never read
  // store.activeWorkspace, which is the one the rest of the app is pointed at:
  // the left tree lists it and chatUrl.ts pins a new session's cwd to it. With
  // more than one workspace registered and the active one not first in the
  // daemon's listing, the home labelled the wrong name and the wrong path — and
  // put `· +N more` beside them, which reads as "and here are the others".
  //
  // Registration order is not selection order and nothing makes it so, so `ws[0]`
  // was only ever right by luck; asserting the SECOND entry is what distinguishes
  // a fix from a test that agrees with the bug.
  describe('names the workspace the app is actually pointed at (tether#129)', () => {
    const two = [
      { id: 'w1', name: 'tether', path: '/root/code/tether' },
      { id: 'w2', name: 'aihub', path: '/root/code/aihub' },
    ]
    const wsLine = (c: HTMLElement) => c.querySelector('.canvas-home-ws')?.textContent ?? ''
    const wsPath = (c: HTMLElement) => c.querySelector('.canvas-home-path')?.textContent ?? ''

    it('shows the active workspace, not the first one registered', async () => {
      mockFetchWorkspaces.mockResolvedValue(two)
      useStore.setState({ activeWorkspace: { id: 'w2', path: '/root/code/aihub' } })
      const { container } = render(<Canvas />)
      await screen.findByText('/root/code/aihub')

      expect(wsLine(container)).toContain('aihub')
      expect(wsPath(container)).toBe('/root/code/aihub')
      // The name of the pane's own fallback must not still be on screen. Without
      // this, a home that rendered BOTH lines would satisfy everything above.
      expect(wsLine(container)).not.toContain('tether')
      // The count is a count of what is registered and is unaffected.
      expect(wsLine(container)).toContain('+1 more')
    })

    it('follows the selection when it moves', async () => {
      mockFetchWorkspaces.mockResolvedValue(two)
      const { container } = render(<Canvas />)
      await screen.findByText('/root/code/tether') // nothing selected yet: the fallback

      act(() => { useStore.setState({ activeWorkspace: { id: 'w2', path: '/root/code/aihub' } }) })
      expect(wsPath(container)).toBe('/root/code/aihub')

      act(() => { useStore.setState({ activeWorkspace: { id: 'w1', path: '/root/code/tether' } }) })
      expect(wsPath(container)).toBe('/root/code/tether')
    })

    // The fallback, asserted so it stays a deliberate one rather than becoming a
    // blank line. `activeWorkspace` is null until WorkspacePane's fetch settles
    // (store.workspacesLoaded is the gate ChatPane waits on), so this is what the
    // home shows for the first frames of every cold load.
    it('falls back to the first registered workspace when nothing is selected', async () => {
      mockFetchWorkspaces.mockResolvedValue(two)
      const { container } = render(<Canvas />)
      await screen.findByText('/root/code/tether')
      expect(wsPath(container)).toBe('/root/code/tether')
    })

    // A selection the daemon's listing does not contain — a workspace removed in
    // another tab, or a remembered id from a previous run (activeWorkspace is
    // persisted, see store's rememberWorkspace). The home has no name for it, so
    // it falls back rather than rendering an empty label.
    it('falls back when the active workspace is not in the listing', async () => {
      mockFetchWorkspaces.mockResolvedValue(two)
      useStore.setState({ activeWorkspace: { id: 'w-gone', path: '/gone' } })
      const { container } = render(<Canvas />)
      await screen.findByText('/root/code/tether')
      expect(wsPath(container)).toBe('/root/code/tether')
    })
  })

  it('quick actions dispatch the expected window events', () => {
    mockFetchWorkspaces.mockResolvedValue([])
    const events: string[] = []
    const onSelect = (e: Event) => events.push('select-tab:' + (e as CustomEvent).detail)
    const onFocus = () => events.push('focus-files')
    window.addEventListener('tether:select-tab', onSelect)
    window.addEventListener('tether:focus-files', onFocus)
    render(<Canvas />)
    fireEvent.click(screen.getByText('Chat'))
    fireEvent.click(screen.getByText('Pick a wi'))
    fireEvent.click(screen.getByText('Open file'))
    window.removeEventListener('tether:select-tab', onSelect)
    window.removeEventListener('tether:focus-files', onFocus)
    expect(events).toEqual(['select-tab:chat', 'select-tab:work', 'focus-files'])
  })
})
