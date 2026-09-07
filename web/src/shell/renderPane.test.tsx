// The shell's default pane wiring. Small on purpose — the point is that the seam
// exists and that exactly one pane is real, so a wi plugging in the Chat panel or
// the Shell pane changes one line here and nothing in Shell.tsx.

import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import { makeRenderPane, renderPane } from './renderPane'
import type { HidePolicy } from './WorkspacePane'
import { PANE_IDS, PANE_LABEL, type PaneId } from './panes'

afterEach(cleanup)

const emptyFetch = () =>
  vi.fn(async () => new Response('[]', { status: 200 })) as unknown as typeof fetch

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

  // The hide policy is threaded, not consulted here: what it DOES is
  // web/src/lib/hidden.ts's job and is pinned by that module's own suite.
  // Asserting behaviour against a stand-in would be a gate deriving from a copy
  // of the thing under test, which is the WIRE-8 defect.
  it('threads a hide policy through to the workspace pane', async () => {
    vi.stubGlobal('fetch', emptyFetch())
    const loadHidePatterns = vi.fn(() => ['node_modules'])
    const hide: HidePolicy = {
      partitionEntries: <T extends { name: string }>(e: readonly T[]) => ({
        visible: [...e],
        hidden: [] as T[],
      }),
      suggestHidePattern: () => '',
      toggleHidePattern: () => [],
      hidingPattern: () => '',
      loadHidePatterns,
      saveHidePatterns: () => {},
    }
    render(<>{makeRenderPane({ hide })('workspace')}</>)
    await waitFor(() => expect(screen.getByRole('region', { name: 'Workspace' })).toBeTruthy())
    // The pane reached for the policy rather than ignoring the prop.
    expect(loadHidePatterns).not.toHaveBeenCalled() // no workspace selected: no tree yet
    vi.unstubAllGlobals()
  })
})
