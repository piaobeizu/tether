// Invariant IDs are docs/tether-ui-invariants.md §3.11; AC-R9 is tether#173 §6.
//
// 🔴 Every error assertion here lands on RENDERED TEXT, and no module is mocked —
// only `fetch` is stubbed. That shape is AC-R9 arm 2 verbatim ("stub
// globalThis.fetch, mock no module, assert the textContent of a visible element,
// so fetch → extraction → render is all load-bearing"), and it is the arm the old
// suite never had: WorkspacePane.test.tsx's three DELETE tests all stubbed
// `{ok:true,status:204}`, which is why the defect tether#164 recorded was never
// touched by a test.

import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { useState } from 'react'
import { WorkspacePane } from './WorkspacePane'
import { resolveSelection, SELECTED_WORKSPACE_KEY } from './workspaces'

afterEach(cleanup)

const WS = [
  { id: 'w1', name: 'alpha', path: '/a' },
  { id: 'w2', name: 'beta', path: '/b' },
]

function memStore(seed: Record<string, string> = {}) {
  const m = new Map(Object.entries(seed))
  return { getItem: (k: string) => m.get(k) ?? null, setItem: (k: string, v: string) => void m.set(k, v) }
}

/** A fetch stub routing by URL, so nothing about the real modules is replaced. */
function stubFetch(routes: Record<string, () => Response>) {
  return vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input)
    for (const [prefix, make] of Object.entries(routes)) {
      if (url.startsWith(prefix)) return make()
    }
    throw new Error(`unrouted: ${url}`)
  }) as unknown as typeof fetch
}

const json = (body: unknown, init: ResponseInit = {}) =>
  new Response(JSON.stringify(body), { status: 200, ...init })

describe('resolveSelection', () => {
  it('WS-4: keeps the current selection when it is still in the registry', () => {
    expect(resolveSelection(WS, 'w2', 'w1')).toBe('w2')
  })

  it('WS-4: falls back to the remembered id when there is no current selection', () => {
    expect(resolveSelection(WS, null, 'w2')).toBe('w2')
  })

  it('WS-4: ignores a remembered id that is no longer in the registry', () => {
    // Carrying it would send the next files request at an id the daemon does not
    // know, which is `unknown_workspace` rather than an empty tree.
    expect(resolveSelection(WS, null, 'gone')).toBe('w1')
  })

  it('WS-4: ignores a current selection that has left the registry', () => {
    expect(resolveSelection(WS, 'gone', 'w2')).toBe('w2')
  })

  it('WS-4: falls back to the first entry the daemon listed', () => {
    expect(resolveSelection(WS, null, null)).toBe('w1')
  })

  it('WS-4: an empty registry is null, not a placeholder id', () => {
    expect(resolveSelection([], 'w1', 'w2')).toBeNull()
  })
})

describe('WorkspacePane — the registry', () => {
  it('WS-4: selects the remembered workspace when it is still registered', async () => {
    render(
      <WorkspacePane
        fetchFn={stubFetch({ '/api/v1/workspaces?': () => json([]), '/api/v1/workspaces': () => json(WS) })}
        storage={memStore({ [SELECTED_WORKSPACE_KEY]: 'w2' })}
      />,
    )
    await waitFor(() => {
      const rows = screen.getAllByRole('button', { name: /alpha|beta/ })
      expect(rows.filter(r => r.getAttribute('aria-current') === 'page').map(r => r.textContent)).toEqual(
        ['beta'],
      )
    })
  })

  it('says so plainly when the daemon has no workspaces, rather than showing an empty tree', async () => {
    render(<WorkspacePane fetchFn={stubFetch({ '/api/v1/workspaces': () => json([]) })} storage={memStore()} />)
    await screen.findByText(/no workspaces are registered/i)
    expect(screen.queryByRole('tree')).toBeNull()
  })

  // AC-R9. The daemon's sentence, on the screen. Not "an error occurred", not the
  // status code — R9 is "show what the daemon said, not its status code".
  it('AC-R9: a refused workspace listing puts the daemon\'s own sentence on the screen', async () => {
    const said = 'the workspace registry is not writable right now'
    render(
      <WorkspacePane
        fetchFn={stubFetch({
          '/api/v1/workspaces': () =>
            new Response(JSON.stringify({ error: said }), { status: 500 }),
        })}
        storage={memStore()}
      />,
    )
    const alert = await screen.findByRole('alert')
    expect(alert.textContent).toContain(said)
    expect(alert.textContent).not.toMatch(/^HTTP 500$/)
  })

  it('R10: a transport failure says what it knows and does not blame the daemon', async () => {
    const fetchFn = vi.fn(async () => {
      throw new Error('Failed to fetch')
    }) as unknown as typeof fetch
    render(<WorkspacePane fetchFn={fetchFn} storage={memStore()} />)
    const alert = await screen.findByRole('alert')
    expect(alert.textContent).toContain('Failed to fetch')
    expect(alert.textContent).not.toMatch(/refused|not a directory/i)
  })
})

describe('WorkspacePane — the file tree', () => {
  const listOK = { '/api/v1/workspaces?': () => json([]), '/api/v1/workspaces': () => json(WS) }

  it('renders the entries the daemon listed', async () => {
    render(
      <WorkspacePane
        fetchFn={stubFetch({
          '/api/v1/workspaces/w1/files': () =>
            json([
              { name: 'src', isDir: true, dirty: false },
              { name: 'README.md', isDir: false, dirty: false },
            ]),
          ...listOK,
        })}
        storage={memStore()}
      />,
    )
    const tree = await screen.findByRole('tree', { name: 'Files' })
    await waitFor(() => expect(within(tree).getByText('README.md')).toBeTruthy())
    expect(within(tree).getByText('src')).toBeTruthy()
  })

  // AC-R9 again, on the path fileTreeCache.ts's own header says has had no
  // renderer since tether#174. Three of tether#159's read refusals are reachable
  // only here and all three used to arrive as "HTTP 400".
  it("AC-R9: a refused directory listing shows the daemon's sentence, not its status", async () => {
    const said = 'that path is not a directory'
    render(
      <WorkspacePane
        fetchFn={stubFetch({
          '/api/v1/workspaces/w1/files': () =>
            new Response(JSON.stringify({ error: said }), { status: 400 }),
          ...listOK,
        })}
        storage={memStore()}
      />,
    )
    await waitFor(() => {
      const alerts = screen.getAllByRole('alert').map(a => a.textContent)
      expect(alerts.some(t => t?.includes(said))).toBe(true)
      expect(alerts.some(t => t?.trim() === 'HTTP 400')).toBe(false)
    })
  })

  // 🔴 The production path, and it is here because every OTHER test in this file
  // avoids it. Injecting `fetchFn` hands the component a STABLE function identity;
  // the shipped default is a fresh arrow per render, and on that path a new
  // identity discards the memoised tree cache and re-reads every open directory.
  //
  // Two things about the shape of this test are the point:
  //
  //  · it drives the pane from a PARENT that re-renders. `setEntries` lives in a
  //    descendant, so the pane's own state changes do not reproduce this — only
  //    an ancestor's do, and Shell re-renders on every pane switch.
  //  · it counts requests, so the assertion is a measurement rather than a
  //    threshold picked to pass. Measured differentially, over five parent
  //    re-renders: 1 listing with the memo, 6 without it.
  it('an ancestor re-render does not re-read the tree (the shipped, un-injected path)', async () => {
    const calls: string[] = []
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input)
        calls.push(url)
        if (url.includes('/files')) return json([{ name: 'src', isDir: true, dirty: false }])
        return json(WS)
      }),
    )
    try {
      function Parent() {
        const [n, setN] = useState(0)
        return (
          <div>
            <button onClick={() => setN(n + 1)}>bump</button>
            {/* No fetchFn and no storage: exactly what renderPane mounts. */}
            <WorkspacePane />
          </div>
        )
      }
      render(<Parent />)
      const tree = await screen.findByRole('tree', { name: 'Files' })
      await waitFor(() => expect(within(tree).getByText('src')).toBeTruthy())

      const listings = () => calls.filter(u => u.includes('/files')).length
      const afterMount = listings()
      expect(afterMount).toBe(1)

      const bumps = 5
      for (let i = 0; i < bumps; i++) fireEvent.click(screen.getByRole('button', { name: 'bump' }))
      await new Promise(r => setTimeout(r, 80))

      // Exactly the mount's listing, still. Phrased against `afterMount` rather
      // than against the literal 1 so the assertion is "the re-renders cost
      // nothing", which is the claim, and not "the number is 1".
      expect(listings(), `listings grew by ${listings() - afterMount} over ${bumps} re-renders`).toBe(
        afterMount,
      )
    } finally {
      vi.unstubAllGlobals()
    }
  })

  // R10 at the surface: with no hide policy supplied nothing is being withheld, so
  // there is no hide control and no "+N hidden" row. An inert one would claim
  // entries are hidden when none are.
  it('R10: renders no hide control while no hide policy is wired', async () => {
    render(
      <WorkspacePane
        fetchFn={stubFetch({
          '/api/v1/workspaces/w1/files': () =>
            json([{ name: 'node_modules', isDir: true, dirty: false }]),
          ...listOK,
        })}
        storage={memStore()}
      />,
    )
    const tree = await screen.findByRole('tree', { name: 'Files' })
    await waitFor(() => expect(within(tree).getByText('node_modules')).toBeTruthy())
    expect(within(tree).queryByText(/hidden$/)).toBeNull()
    expect(within(tree).queryByRole('button', { name: /^Hide / })).toBeNull()
  })
})
