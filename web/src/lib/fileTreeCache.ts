// fileTreeCache.ts — fetch + per-directory cache for the workspace file tree.
//
// Originally split out of WorkspaceTree.tsx so the fetch/caching logic could be
// exercised with an injected `fetch` instead of a rendered component; `fetchFn`
// below is what that split bought. Ported to web/src/lib/ in tether#188 — it has
// zero pane dependencies, and the new tree has no panes/ layer at all.
//
// 🔴 The reason the old header gave for testing it this way — "no jsdom/React
// Testing Library is configured in this project" — was FALSE by the time anyone
// read it and is deleted rather than moved: web/vite.config.ts sets
// `test.environment: 'jsdom'`, and jsdom + @testing-library/react are both in
// web/package.json devDependencies. Anyone extending this module CAN render.
//
// Tests: web/test/workspace-tree.spec.ts (kept under that misleading name on
// purpose — see its header).

import { httpErrorMessage } from './httpError'

export interface FileEntry {
  name: string
  isDir: boolean
  dirty: boolean
}

export interface FileTreeCache {
  /** Load entries for `dir` (repo-relative, '' = workspace root). Cached after first success. */
  load: (dir: string) => Promise<FileEntry[]>
  /** Drop the cached entries for `dir`, forcing the next load() to re-fetch. */
  invalidate: (dir: string) => void
}

type FetchFn = typeof fetch

/** Creates a per-workspace file tree cache backed by GET /api/v1/workspaces/{id}/files. */
export function createFileTreeCache(workspaceId: string, fetchFn: FetchFn = fetch): FileTreeCache {
  const cache = new Map<string, FileEntry[]>()
  const inflight = new Map<string, Promise<FileEntry[]>>()

  const load = async (dir: string): Promise<FileEntry[]> => {
    const cached = cache.get(dir)
    if (cached) return cached

    const pending = inflight.get(dir)
    if (pending) return pending

    const url = `/api/v1/workspaces/${workspaceId}/files?dir=${encodeURIComponent(dir)}`
    const promise = (async () => {
      try {
        const res = await fetchFn(url)
        // tether#161 — the daemon's own sentence, so whoever renders this Error
        // can show it verbatim. Three of tether#159's read-path refusals are only
        // ever reachable here ("that path must be relative to the workspace
        // root", "…is outside the workspace", "…is not a directory") and all
        // three used to arrive as "HTTP 400". A source with no `text()` still
        // gets `HTTP ${status}`, which is what pre-tether#161 fetch stubs expect.
        //
        // ⚠️ There is currently NO renderer: WorkspaceTree.tsx, the only consumer
        // this line ever had, was deleted in tether#174. Both halves are pinned
        // at the unit layer by web/test/workspace-tree.spec.ts (tether#161 arms 1
        // and 2); that the wording reaches a SCREEN is an open tether#173 AC-R9
        // gap, owed by the wi that next renders a file tree. Do not read this
        // comment as a claim that a component is relying on it today.
        if (!res.ok) throw new Error(await httpErrorMessage(res))
        const entries = await res.json() as FileEntry[]
        cache.set(dir, entries)
        return entries
      } finally {
        inflight.delete(dir)
      }
    })()
    inflight.set(dir, promise)
    return promise
  }

  const invalidate = (dir: string): void => {
    cache.delete(dir)
  }

  return { load, invalidate }
}
