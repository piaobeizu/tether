// fileTreeCache.ts — fetch + per-directory cache for the workspace file tree.
//
// Split out from WorkspaceTree.tsx so the fetch/caching logic is directly
// unit-testable with a mocked `fetch` (no jsdom/React Testing Library is
// configured in this project; see web/test/workspace-tree.spec.ts).

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
        if (!res.ok) throw new Error(`HTTP ${res.status}`)
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
