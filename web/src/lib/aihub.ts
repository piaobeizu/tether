// aihub.ts — REST fetchers for the daemon's aihub-backed work-item endpoints
// (Phase A, shipped). Same-origin, cookie-authenticated like the rest of the
// SPA (see lib/auth.ts) — plain `fetch`, no explicit credentials needed since
// these calls never leave the origin.

import type { WorkProject, WorkQueue, WorkItemDetail, WorkEvents } from './wire.gen'

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
