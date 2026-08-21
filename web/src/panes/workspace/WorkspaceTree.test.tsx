// tether#20 Task 11 — clicking a file (not dir) row must select it in the
// shared canvas-selection store slice, with the path relative to the
// workspace root (matching fetchFile's `path` param). Directory rows must
// keep expanding/collapsing rather than selecting.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, render, screen, fireEvent, waitFor } from '@testing-library/react'
import WorkspaceTree from './WorkspaceTree'
import { useStore } from '../../lib/store'

interface Entry { name: string; isDir: boolean; dirty: boolean }

function mockFiles(entries: Entry[]) {
  vi.stubGlobal('fetch', vi.fn(async () => ({
    ok: true,
    json: async () => entries,
  })))
}

/**
 * A file-listing stub that answers the workspace ROOT immediately and holds every
 * other directory open until it is released by hand.
 *
 * mockFiles above cannot express the window this exists to test: it resolves in a
 * microtask, so "the user clicked while the fetch was in flight" has nowhere to
 * happen. Holding the promise is what makes that window arbitrarily wide, which
 * is the only way to test the race rather than to try to win it.
 */
function deferredFiles(root: Entry[]) {
  // The raw resolvers, so `release` and `fail` each build their own Response
  // shape. An earlier version stored a pre-wrapped ok-resolver and had `fail`
  // push a bad status through it, which resolved as `ok: true` carrying the
  // status object as the listing — a stub that fails in a way the daemon never
  // could is not a test of the error path.
  const pending = new Map<string, (res: unknown) => void>()
  const calls: string[] = []
  // Directories whose landed listing has been read back out of the response
  // body. releaseDuring asserts on this rather than assuming its own wait was
  // long enough — a listing that never got as far as fileTreeCache would leave
  // the very race it is trying to set up unset, silently.
  const bodiesRead = new Set<string>()
  vi.stubGlobal('fetch', vi.fn((url: string) => {
    calls.push(url)
    const dir = new URL(url, 'http://localhost').searchParams.get('dir') ?? ''
    if (dir === '') return Promise.resolve({ ok: true, json: async () => root })
    return new Promise((resolve) => { pending.set(dir, resolve) })
  }))
  const take = (dir: string) => {
    const resolve = pending.get(dir)
    if (!resolve) throw new Error(`no in-flight listing for "${dir}"`)
    pending.delete(dir)
    return resolve
  }
  const respond = (dir: string, entries: Entry[]) => ({
    ok: true,
    json: async () => { bodiesRead.add(dir); return entries },
  })
  return {
    /** How many listings have been requested, per directory. */
    countFor: (dir: string) => calls.filter((u) => u.endsWith(`dir=${dir}`)).length,
    /** Land `dir`'s listing and let React flush the resulting state write. */
    release: async (dir: string, entries: Entry[]) => {
      const resolve = take(dir)
      await act(async () => { resolve(respond(dir, entries)) })
    },
    /**
     * Land `dir`'s listing and run `interleave` inside the SAME React batch —
     * after the listing's state write has been queued, but before React has
     * re-rendered — so whatever `interleave` clicks is still holding the
     * pre-listing render's closures.
     *
     * `release` above cannot express that: it awaits its own act scope, so the
     * commit has always happened by the time it returns and every handler is
     * fresh. Yielding to a TIMER is what opens the gap. It drains the microtask
     * queue to empty — so fileTreeCache's fetch -> json -> load -> component-
     * .then chain runs to completion, however many hops it grows — while React
     * does not flush an act scope from a timer either: updates go on the act
     * queue and are drained when the scope exits.
     *
     * An earlier version ticked `await Promise.resolve()` a fixed number of
     * times instead, and that count was load-bearing in the worst way: at three
     * ticks or fewer `interleave` ran BEFORE the state write was queued, which
     * is merely the fold-then-listing order this component already survives, so
     * the case passed with the defect present and nothing said so. A drain with
     * no number in it cannot drift into that. `bodiesRead` then holds: once the
     * body has been read inside a microtask, every continuation behind it has
     * run too, because macrotasks wait for the microtask queue to empty.
     */
    releaseDuring: async (dir: string, entries: Entry[], interleave: () => void) => {
      const resolve = take(dir)
      bodiesRead.delete(dir)
      await act(async () => {
        resolve(respond(dir, entries))
        // Twice: one yield proves the promise chain drained, a second would also
        // absorb a hop that itself goes through a timer, should one ever appear
        // between the fetch and the state write.
        await new Promise(r => setTimeout(r, 0))
        await new Promise(r => setTimeout(r, 0))
        if (!bodiesRead.has(dir)) throw new Error(`listing for "${dir}" never reached fileTreeCache`)
        interleave()
      })
    },
    /** Fail `dir`'s listing the way fileTreeCache surfaces a bad status. */
    fail: async (dir: string) => {
      const resolve = take(dir)
      await act(async () => { resolve({ ok: false, status: 500 }) })
    },
  }
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

// tether#129 defect 2 — a directory folded shut while its listing was still in
// flight sprang back open on its own.
//
// `expand` wrote `expanded: true` optimistically, then BOTH of its async
// callbacks wrote `expanded: true` again unconditionally. A collapse landing in
// between set it false, and the arriving listing overwrote that with an intent
// the user had already withdrawn — the callback re-asserting a decision instead
// of reporting the one fact it actually learned.
//
// Two assertions per case, and the second is what makes the first mean anything.
// "Still collapsed" is also what a listing that never arrived would look like, so
// each case ALSO proves the write landed: one more click reveals the children
// with no second request. That doubles as the guard against the obvious wrong
// fix, which is to stop writing `entries` along with `loading` and quietly turn
// every interrupted expand into a refetch.
describe('WorkspaceTree collapse during load (tether#129)', () => {
  const root: Entry[] = [{ name: 'src', isDir: true, dirty: false }]
  const children: Entry[] = [{ name: 'deep.txt', isDir: false, dirty: false }]

  it('stays collapsed when the listing lands after the fold', async () => {
    const files = deferredFiles(root)
    render(<WorkspaceTree workspaceId="ws-1" />)
    await waitFor(() => screen.getByText('src'))

    fireEvent.click(screen.getByText('src'))            // expand — listing now in flight
    expect(screen.getByText('loading…')).toBeTruthy()
    fireEvent.click(screen.getByText('src'))            // fold it back before the listing lands
    expect(screen.queryByText('loading…')).toBeNull()

    await files.release('src', children)

    expect(screen.queryByText('deep.txt')).toBeNull()   // it must not have re-opened itself
    expect(files.countFor('src')).toBe(1)

    // ...and the listing really did arrive: one click shows it, no second fetch.
    fireEvent.click(screen.getByText('src'))
    expect(screen.getByText('deep.txt')).toBeTruthy()
    expect(files.countFor('src')).toBe(1)
  })

  // The error callback had the identical unconditional write. It is a separate
  // clause in a separate `.catch`, so it needs its own case — a fix applied to
  // the success path alone leaves the failure path popping directories open, and
  // that is the path a user on a flaky daemon actually sees.
  it('stays collapsed when the listing FAILS after the fold', async () => {
    const files = deferredFiles(root)
    render(<WorkspaceTree workspaceId="ws-1" />)
    await waitFor(() => screen.getByText('src'))

    fireEvent.click(screen.getByText('src'))
    fireEvent.click(screen.getByText('src'))

    await files.fail('src')

    expect(screen.queryByText('HTTP 500')).toBeNull()
    expect(screen.queryByText('deep.txt')).toBeNull()
  })

  // The other half of the contract, so the fix cannot be "never expand from a
  // callback". Left alone, an expand that is never interrupted must still open.
  it('still opens when nothing interrupts the expand', async () => {
    const files = deferredFiles(root)
    render(<WorkspaceTree workspaceId="ws-1" />)
    await waitFor(() => screen.getByText('src'))

    fireEvent.click(screen.getByText('src'))
    await files.release('src', children)

    expect(screen.getByText('deep.txt')).toBeTruthy()
  })
})

// tether#144 — `toggle` read the node it was about to write from the RENDER-TIME
// closure (`{ ...node, expanded: false }`) while taking `prev` from the
// functional update, so the write was only correct as long as nothing had
// touched `nodes[dir]` since that render. The arriving listing is exactly such a
// something: land it in the same batch as the fold and the fold shipped the
// pre-listing node back to React, taking `entries` with it.
//
// This is a DIFFERENT ordering from tether#129 above, which is why that suite
// stayed green through this defect. There the fold landed first and the listing
// second, so the listing's own write repaired anything the fold got wrong. Here
// the listing is already in `prev` when the fold is applied, and last write
// wins — so a fold built from a stale snapshot is the final state.
describe('WorkspaceTree fold racing the arriving listing (tether#144)', () => {
  const root: Entry[] = [{ name: 'src', isDir: true, dirty: false }]
  const children: Entry[] = [{ name: 'deep.txt', isDir: false, dirty: false }]

  it('keeps the listing when the fold lands in the same batch as it', async () => {
    const files = deferredFiles(root)
    render(<WorkspaceTree workspaceId="ws-1" />)
    await waitFor(() => screen.getByText('src'))

    fireEvent.click(screen.getByText('src'))            // expand — listing in flight
    expect(screen.getByText('loading…')).toBeTruthy()

    await files.releaseDuring('src', children, () => {
      // The window, asserted rather than assumed: the listing's write is queued
      // but nothing has re-rendered, so the children are not on screen and this
      // row's onClick is still the one built before the listing existed.
      expect(screen.queryByText('deep.txt')).toBeNull()
      expect(screen.getByText('loading…')).toBeTruthy()
      fireEvent.click(screen.getByText('src'))         // fold, from that stale render
    })

    // The fold is honoured — the user asked for it last and it stands...
    expect(screen.queryByText('deep.txt')).toBeNull()
    // ...but it did not take the listing down with it. One click brings the
    // children back synchronously, off the node's own `entries`, with no second
    // request. Before the fix the fold wrote `entries: null` back over them and
    // this click fell through to a fresh expand(), which paints nothing until
    // its promise settles — so `getByText` here is what goes red.
    fireEvent.click(screen.getByText('src'))
    expect(screen.getByText('deep.txt')).toBeTruthy()
    expect(files.countFor('src')).toBe(1)
  })
})
