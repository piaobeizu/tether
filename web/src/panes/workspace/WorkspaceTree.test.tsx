// tether#20 Task 11 — clicking a file (not dir) row must select it in the
// shared canvas-selection store slice, with the path relative to the
// workspace root (matching fetchFile's `path` param). Directory rows must
// keep expanding/collapsing rather than selecting.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen, fireEvent, waitFor } from '@testing-library/react'
import WorkspaceTree from './WorkspaceTree'
import { useStore } from '../../lib/store'

interface Entry { name: string; isDir: boolean; dirty: boolean }

function mockFiles(entries: Entry[]) {
  vi.stubGlobal('fetch', vi.fn(async () => ({
    ok: true,
    json: async () => entries,
  })))
}

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
  localStorage.clear()
})

describe('WorkspaceTree file selection', () => {
  beforeEach(() => {
    useStore.setState({ selectedWiId: null, selectedFile: null })
  })

  it('clicking a file row selects it with workspace id + relative path', async () => {
    mockFiles([
      { name: 'src', isDir: true, dirty: false },
      { name: 'README.md', isDir: false, dirty: false },
    ])
    render(<WorkspaceTree workspaceId="ws-1" />)

    await waitFor(() => screen.getByText('README.md'))
    fireEvent.click(screen.getByText('README.md'))

    expect(useStore.getState().selectedFile).toEqual({ wsId: 'ws-1', path: 'README.md' })
    expect(useStore.getState().selectedWiId).toBeNull()
  })

  it('clicking a directory row expands it instead of selecting a file', async () => {
    mockFiles([{ name: 'src', isDir: true, dirty: false }])
    render(<WorkspaceTree workspaceId="ws-1" />)

    await waitFor(() => screen.getByText('src'))
    fireEvent.click(screen.getByText('src'))

    expect(useStore.getState().selectedFile).toBeNull()
  })
})

// tether#71 — the tree used to render every entry the daemon listed, so a
// workspace root full of ephemeral or generated directories buried everything
// worth reading. These cover the two halves of the fix: the seeded defaults,
// and the one-click family hide that makes the control usable when the noise
// has a local shape the defaults cannot know about.
describe('WorkspaceTree hidden entries', () => {
  it('folds a default-noise directory behind a "+N hidden" row', async () => {
    mockFiles([
      { name: 'src', isDir: true, dirty: false },
      { name: 'node_modules', isDir: true, dirty: false },
      { name: 'README.md', isDir: false, dirty: false },
    ])
    render(<WorkspaceTree workspaceId="ws-1" />)

    await waitFor(() => screen.getByText('src'))
    expect(screen.queryByText('node_modules')).toBeNull()
    expect(screen.getByTestId('tree-hidden-row').textContent).toBe('+1 hidden')
    // and it hid only the noise
    expect(screen.getByText('README.md')).toBeTruthy()
  })

  it('reveals hidden entries in place when the fold row is clicked', async () => {
    mockFiles([
      { name: 'src', isDir: true, dirty: false },
      { name: 'dist', isDir: true, dirty: false },
    ])
    render(<WorkspaceTree workspaceId="ws-1" />)

    await waitFor(() => screen.getByText('src'))
    fireEvent.click(screen.getByTestId('tree-hidden-row'))

    expect(screen.getByText('dist')).toBeTruthy()
    expect(screen.getByTestId('tree-hidden-row').textContent).toBe('− 1 hidden')
  })

  it('hides a whole family of siblings from one click, not one click each', async () => {
    const worktrees = ['pf.aihub-185', 'pf.ieops-51', 'pf.ieops-57', 'pf.global-routing-8']
    mockFiles([
      { name: 'docs', isDir: true, dirty: false },
      ...worktrees.map(name => ({ name, isDir: true, dirty: false })),
      { name: 'go.work', isDir: false, dirty: false },
    ])
    render(<WorkspaceTree workspaceId="ws-1" />)

    await waitFor(() => screen.getByText('pf.aihub-185'))
    // The button announces the pattern and its reach before the click.
    const row = screen.getByText('pf.aihub-185').closest('.tree-row')!
    const hide = row.querySelector('[data-testid="tree-hide-btn"]')!
    expect(hide.getAttribute('aria-label')).toBe('hide pf.*')
    fireEvent.click(hide)

    for (const name of worktrees) expect(screen.queryByText(name)).toBeNull()
    expect(screen.getByTestId('tree-hidden-row').textContent).toBe('+4 hidden')
    // the entries that were being buried are still there
    expect(screen.getByText('docs')).toBeTruthy()
    expect(screen.getByText('go.work')).toBeTruthy()
    // and the rule persisted, not just the render
    expect(JSON.parse(localStorage.getItem('tether_tree_hidden')!)).toContain('pf.*')
  })

  it('hiding a lone entry offers the literal name, never a prefix glob', async () => {
    mockFiles([
      { name: 'docs', isDir: true, dirty: false },
      { name: 'notes.md', isDir: false, dirty: false },
    ])
    render(<WorkspaceTree workspaceId="ws-1" />)

    await waitFor(() => screen.getByText('notes.md'))
    const row = screen.getByText('notes.md').closest('.tree-row')!
    expect(row.querySelector('[data-testid="tree-hide-btn"]')!.getAttribute('aria-label'))
      .toBe('hide notes.md')
  })

  it('a revealed hidden row unhides by dropping the RULE, not the name', async () => {
    // Removing the entry's name would leave `pf.*` in place and the row would
    // spring straight back — the bug this asserts against.
    localStorage.setItem('tether_tree_hidden', JSON.stringify(['pf.*']))
    mockFiles([
      { name: 'docs', isDir: true, dirty: false },
      { name: 'pf.ieops-51', isDir: true, dirty: false },
      { name: 'pf.ieops-57', isDir: true, dirty: false },
    ])
    render(<WorkspaceTree workspaceId="ws-1" />)

    await waitFor(() => screen.getByText('docs'))
    fireEvent.click(screen.getByTestId('tree-hidden-row'))
    const row = screen.getByText('pf.ieops-51').closest('.tree-row')!
    const show = row.querySelector('[data-testid="tree-show-btn"]')!
    expect(show.getAttribute('aria-label')).toBe('show pf.*')
    fireEvent.click(show)

    expect(screen.queryByTestId('tree-hidden-row')).toBeNull()
    expect(screen.getByText('pf.ieops-57')).toBeTruthy()
    expect(JSON.parse(localStorage.getItem('tether_tree_hidden')!)).toEqual([])
  })

  it('shows no fold row at all when nothing is hidden', async () => {
    mockFiles([{ name: 'src', isDir: true, dirty: false }])
    render(<WorkspaceTree workspaceId="ws-1" />)

    await waitFor(() => screen.getByText('src'))
    expect(screen.queryByTestId('tree-hidden-row')).toBeNull()
  })
})
