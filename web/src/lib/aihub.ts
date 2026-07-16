// aihub.ts — REST fetchers for the daemon's aihub-backed work-item endpoints
// (Phase A, shipped). Same-origin, cookie-authenticated like the rest of the
// SPA (see lib/auth.ts) — plain `fetch`, no explicit credentials needed since
// these calls never leave the origin.

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
 * Thrown on any non-2xx response from a work/* endpoint. Carries the HTTP
 * status so callers can distinguish 403 (forbidden) / 503 (aihub not
 * configured) from other failures per the daemon's error contract.
 */
export class AihubError extends Error {
  status: number
  constructor(status: number, message?: string) {
    super(message ?? `HTTP ${status}`)
    this.name = 'AihubError'
    this.status = status
  }
}

async function getJSON<T>(url: string): Promise<T> {
  const res = await fetch(url)
  if (!res.ok) throw new AihubError(res.status)
  return res.json() as Promise<T>
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
