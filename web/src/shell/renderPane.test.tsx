// The shell's default pane wiring. Small on purpose — the point is that the seam
// exists and that exactly one pane is real, so a wi plugging in the Chat panel or
// the Shell pane changes one line here and nothing in Shell.tsx.

import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen, waitFor, within } from '@testing-library/react'
import { makeRenderPane, renderPane } from './renderPane'
import type { HidePolicy } from './WorkspacePane'
import { PANE_IDS, PANE_LABEL, type PaneId } from './panes'

afterEach(cleanup)

const emptyFetch = () =>
  vi.fn(async () => new Response('[]', { status: 200 })) as unknown as typeof fetch

const json = (body: unknown) => new Response(JSON.stringify(body), { status: 200 })

/**
 * A registered workspace with one entry in it, so the file TREE really mounts.
 *
 * The distinction matters and is the whole reason this helper exists rather than
 * `emptyFetch`: with no workspaces the pane stops at "No workspaces are
 * registered" and the tree — the only thing that consults the hide policy — is
 * never constructed.
 */
const treeFetch = () =>
  vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input)
    if (url.includes('/files')) return json([{ name: 'src', isDir: true, dirty: false }])
    return json([{ id: 'w1', name: 'alpha', path: '/a' }])
  }) as unknown as typeof fetch

describe('renderPane', () => {
  it('routes the workspace pane to the real implementation', async () => {
    vi.stubGlobal('fetch', emptyFetch())
    render(<>{renderPane('workspace')}</>)
    await waitFor(() => expect(screen.getByRole('region', { name: 'Workspace' })).toBeTruthy())
    vi.unstubAllGlobals()
  })

  // R10, enumerated over the whole id space rather than over a list of "the ones
  // that are not built": a pane added to panes.ts and forgotten here renders a
  // placeholder that SAYS it is one, which is the honest failure. Silence, or a
  // pane rendering nothing at all, is what this rules out.
  it('R10: every other pane renders a placeholder that says it is one', () => {
    for (const pane of PANE_IDS.filter(p => p !== 'workspace') as PaneId[]) {
      cleanup()
      render(<>{renderPane(pane)}</>)
      const text = document.body.textContent ?? ''
      expect(text, pane).toContain(PANE_LABEL[pane])
      expect(text, pane).toMatch(/no implementation on this branch yet/i)
    }
  })

  it('R10: a placeholder offers no control, so it cannot imply a capability', () => {
    render(<>{renderPane('chat')}</>)
    expect(screen.queryAllByRole('button')).toHaveLength(0)
    expect(screen.queryAllByRole('textbox')).toHaveLength(0)
    expect(document.body.textContent ?? '').not.toMatch(/loading|coming soon/i)
  })

  // 🔴 THE WIRING HOP, and it is the reason this file matters more than its size
  // suggests: `renderPane` is the ONLY point at which `web/src/lib/hidden.ts` is
  // ever handed to anything. WS-11/12/14 are deferred to tether#196 on the
  // argument that this socket holds the contract, so if this case is vacuous the
  // deferral rests on nothing.
  //
  // The previous version was exactly that. Its whole assertion was
  // `expect(loadHidePatterns).not.toHaveBeenCalled()`, annotated "the pane reached
  // for the policy rather than ignoring the prop" — the opposite of what it
  // checked — and its fetch stub returned no workspaces, so no tree mounted and
  // the mock could not have been called WHETHER OR NOT the prop was threaded at
  // all. Measured: deleting `hide={options.hide}` from renderPane.tsx left this
  // file at 4 passed.
  //
  // What is asserted now is only what a stand-in cannot fake — that the pane
  // reached for the injected policy, and that a control derived from it reached
  // the screen. What hidden.ts's functions DECIDE is still not asserted here:
  // that module is not on this branch (tether#196 is porting it), and pinning its
  // behaviour against a stand-in would be a gate deriving from a copy of the thing
  // under test, which is the WIRE-8 defect.
  //
  // Check: delete `hide={options.hide}` from renderPane.tsx and this case fails.
  it('threads a hide policy through to the workspace pane, and the tree consults it', async () => {
    vi.stubGlobal('fetch', treeFetch())
    const loadHidePatterns = vi.fn(() => ['node_modules'])
    const hide: HidePolicy = {
      partitionEntries: <T extends { name: string }>(e: readonly T[]) => ({
        visible: [...e],
        hidden: [] as T[],
      }),
      // Returns the entry's own name, so the control's accessible name is
      // derivable from the fixture rather than from a second copy of a label.
      suggestHidePattern: (name: string) => name,
      toggleHidePattern: () => [],
      hidingPattern: () => '',
      loadHidePatterns,
      saveHidePatterns: () => {},
    }
    try {
      render(<>{makeRenderPane({ hide })('workspace')}</>)
      // The tree really mounted — without this the assertions below could pass on
      // an empty pane, which is how the previous version failed.
      const tree = await screen.findByRole('list', { name: 'Files' })
      await waitFor(() => expect(within(tree).getByText('src')).toBeTruthy())
      expect(loadHidePatterns).toHaveBeenCalled()
      // …and the affordance the policy makes possible is on the screen, named
      // after what the policy said to hide.
      expect(within(tree).getByRole('button', { name: 'Hide src' })).toBeTruthy()
    } finally {
      vi.unstubAllGlobals()
    }
  })

  // The other direction of the same seam, and the reason the case above cannot be
  // satisfied by threading a constant: with NO policy the tree renders and offers
  // no hide control at all. R10 — an inert one would claim entries are being
  // withheld when the module that withholds them is not on this branch.
  it('R10: the default wiring supplies no policy, so the tree offers no hide control', async () => {
    vi.stubGlobal('fetch', treeFetch())
    try {
      render(<>{renderPane('workspace')}</>)
      const tree = await screen.findByRole('list', { name: 'Files' })
      await waitFor(() => expect(within(tree).getByText('src')).toBeTruthy())
      expect(within(tree).queryByRole('button', { name: /^Hide |^Unhide / })).toBeNull()
    } finally {
      vi.unstubAllGlobals()
    }
  })
})
