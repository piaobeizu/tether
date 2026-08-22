// aihub.ts — REST fetchers for the daemon's aihub-backed work-item endpoints
// (Phase A, shipped). Same-origin, cookie-authenticated like the rest of the
// SPA (see lib/auth.ts) — plain `fetch`, no explicit credentials needed since
// these calls never leave the origin.

import { httpErrorMessage, httpStatusFallback } from './httpError'
import type {
  WorkProject,
  WorkQueue,
  WorkItemDetail,
  WorkEvents,
  WorkRecent,
  WorkGraph,
  WorkDependencies,
  WorkSteps,
} from './wire.gen'

/**
 * Thrown on any non-2xx response from a route this module fetches. Carries the
 * HTTP status so callers can distinguish 403 (forbidden) / 503 (aihub not
 * configured) from other failures per the daemon's error contract, AND the
 * message the response body carried, which is the only thing that distinguishes
 * one 400 from another.
 *
 * `message` defaults to the same `HTTP ${status}` that httpStatusFallback
 * returns, so a hand-built AihubError (the test suite has several) is
 * indistinguishable from one whose response had an empty body — both mean "the
 * status is all there is".
 */
export class AihubError extends Error {
  status: number
  constructor(status: number, message?: string) {
    super(message ?? httpStatusFallback(status))
    this.name = 'AihubError'
    this.status = status
  }
}

async function getJSON<T>(url: string): Promise<T> {
  const res = await fetch(url)
  // Read the body. Until tether#163 this threw the status alone, and the body
  // — the whole output of tether#147/#159 on these routes — went on the floor.
  // httpErrorMessage rather than a local res.text(): the JSON-vs-text and
  // over-long-body decisions are already made once in lib/httpError.ts, and it
  // never rejects, so this still fails with an AihubError and not a TypeError.
  if (!res.ok) throw new AihubError(res.status, await httpErrorMessage(res))
  return res.json() as Promise<T>
}

/**
 * Wording this SPA prefers over whatever the response body said, by status.
 *
 * Two rules are in tension here — show the daemon's own sentence, or show the
 * sentence written for this surface — and the reason the second wins for exactly
 * these three statuses is that on every route this module fetches, a body
 * carrying one of them is a generic token rather than one of the tether#147 /
 * tether#159 refusals:
 *
 *  - 503 is only ever `aihub not configured`, written verbatim at nine sites in
 *    internal/server/aihub_proxy.go. Byte-identical to the entry below, so here
 *    the two rules do not actually disagree.
 *  - 403 is only ever `forbidden` (writeAihubError, aihub_proxy.go). One word,
 *    and it does not say what the caller is not authorized FOR; the entry below
 *    does.
 *  - 404 is net/http's own `404 page not found` on both families — the proxy's
 *    unmatched-subpath catch-all, and workspace's refuseRead on fs.ErrNotExist,
 *    which routes deliberately through http.NotFound so its body stays the one
 *    its tests pin. Boilerplate either way.
 *
 * So nothing readable is suppressed. What IS suppressed is a future body: the
 * day the daemon answers a 403 with something specific, this map is the line
 * that has to be revisited, and there is nothing in the daemon that will make
 * that obvious. It is the deliberate cost of keeping wording written for a
 * screen ahead of wording written for a wire.
 *
 * Deliberately NOT a general status→wording table: every other status this SPA
 * can see (502 `aihub upstream error`, 400 `project is required`, and the whole
 * workspace read family — `workspace: that path is a directory, not a file`,
 * `… is outside the workspace`, `… must be relative to the workspace root`)
 * carries a sentence chosen for the specific mistake, which no generic wording
 * can improve on.
 */
const STATUS_WORDING: Record<number, string> = {
  503: 'aihub not configured',
  403: 'not authorized for this project',
  404: 'not found',
}

/**
 * The string to put on screen for a failure from any fetcher in this module.
 *
 * One copy, in the module the errors come from. Until tether#163 there were five
 * hand-copies (panes/canvas, panes/work × 4), born in three separate commits
 * (#101, #107, #110) — and they had already drifted three ways: two had no 404
 * branch at all, and a third shortened the 403 to `not authorized`. That drift
 * is the argument: the precedence decision above is one decision about one
 * daemon, and five copies of it are five chances to answer it differently.
 *
 * The final fallback keeps `error (HTTP ${status})` rather than the bare
 * `HTTP ${status}` the Error already carries, because that is what these panes
 * have always rendered when there was nothing else to say, and a status with no
 * body is still exactly that case.
 */
export function describeError(e: unknown): string {
  if (e instanceof AihubError) {
    const wording = STATUS_WORDING[e.status]
    if (wording) return wording
    // A message equal to the status fallback means the body said nothing a human
    // can read — see httpErrorText, which collapses an empty, non-JSON-message
    // or unreadable body to exactly this.
    if (e.message !== httpStatusFallback(e.status)) return e.message
    return `error (HTTP ${e.status})`
  }
  return e instanceof Error ? e.message : String(e)
}

export function fetchProjects(): Promise<WorkProject[]> {
  return getJSON<WorkProject[]>('/api/v1/work/projects')
}

export function fetchQueue(project: string): Promise<WorkQueue> {
  return getJSON<WorkQueue>(`/api/v1/work/queue?project=${encodeURIComponent(project)}`)
}

export function fetchItem(id: string): Promise<WorkItemDetail> {
  return getJSON<WorkItemDetail>(`/api/v1/work/items/${encodeURIComponent(id)}`)
}

export function fetchEvents(id: string, cursor?: string): Promise<WorkEvents> {
  const qs = cursor ? `?cursor=${encodeURIComponent(cursor)}` : ''
  return getJSON<WorkEvents>(`/api/v1/work/items/${encodeURIComponent(id)}/events${qs}`)
}

/** Terminal (wrapped/cancelled) work items for the done/recent history view. */
export function fetchRecent(project: string): Promise<WorkRecent> {
  return getJSON<WorkRecent>(`/api/v1/work/recent?project=${encodeURIComponent(project)}`)
}

/** Curated dependency/parent graph of active work items, for the wi-relationship view. */
export function fetchGraph(project: string): Promise<WorkGraph> {
  return getJSON<WorkGraph>(`/api/v1/work/graph?project=${encodeURIComponent(project)}`)
}

/** Blocking/blockedBy dependency edges for a single work item. */
export function fetchDeps(id: string): Promise<WorkDependencies> {
  return getJSON<WorkDependencies>(`/api/v1/work/items/${encodeURIComponent(id)}/dependencies`)
}

/** Scenario step graph (with progress status) for a single work item. */
export function fetchSteps(id: string): Promise<WorkSteps> {
  return getJSON<WorkSteps>(`/api/v1/work/items/${encodeURIComponent(id)}/steps`)
}

/** One file's content from a workspace, for the file-preview pane. */
export function fetchFile(
  wsId: string,
  path: string,
): Promise<{ path: string; content: string; truncated: boolean }> {
  return getJSON<{ path: string; content: string; truncated: boolean }>(
    `/api/v1/workspaces/${encodeURIComponent(wsId)}/file?path=${encodeURIComponent(path)}`,
  )
}

/** A registered workspace (matches the daemon's GET /api/v1/workspaces shape,
 *  mirrored from WorkspacePane's local type). */
export interface Workspace {
  id: string
  name: string
  path: string
  addedAt?: string
  activeSid?: string
}

/** All registered workspaces — used by the middle-pane home to show context. */
export function fetchWorkspaces(): Promise<Workspace[]> {
  return getJSON<Workspace[]>('/api/v1/workspaces')
}
