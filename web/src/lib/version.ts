// The version shown in the UI comes from the daemon, not from a constant here.
//
// The SPA is embedded in the daemon binary, so its version IS the daemon's —
// asking for it makes the two structurally incapable of disagreeing. The
// literal that used to live in this file had already drifted: the UI read
// v0.5.0 while the binary reported v0.5.1-0.20260805135746-69eedc4cac98
// (tether#70). That is the same failure as tether#67 — a version string that
// reads like an authority while being wired to nothing, kept correct only by
// someone remembering to bump it.
import { useEffect, useState } from 'react'
import type { VersionResponse } from './wire.gen'

/**
 * Shown until the daemon answers, and if it never does.
 *
 * Deliberately not a version number: an out-of-date but plausible-looking
 * version is worse than a visible gap, because it reads as fact. That is
 * exactly how the old constant misled.
 */
export const UNKNOWN_VERSION = '—'

// One request per page load, shared by every caller — four call sites render
// this string and none of them should cost a round trip of its own.
let inflight: Promise<string> | null = null

/** fetchAppVersion resolves to the daemon's version, or '' if unavailable. */
export function fetchAppVersion(): Promise<string> {
  if (inflight === null) {
    inflight = fetch('/api/v1/version')
      .then(r => (r.ok ? (r.json() as Promise<VersionResponse>) : Promise.reject(new Error(String(r.status)))))
      .then(v => (typeof v.version === 'string' ? v.version : ''))
      .catch(() => '') // never surface a version lookup failure as a UI error
  }
  return inflight
}

/** resetAppVersionCache drops the shared request. For tests only. */
export function resetAppVersionCache(): void {
  inflight = null
}

/**
 * useAppVersion returns the daemon's version string, falling back to
 * UNKNOWN_VERSION while the request is in flight or after it failed.
 */
export function useAppVersion(): string {
  const [version, setVersion] = useState('')
  useEffect(() => {
    let alive = true
    void fetchAppVersion().then(v => {
      if (alive) setVersion(v)
    })
    return () => {
      alive = false
    }
  }, [])
  return version || UNKNOWN_VERSION
}
