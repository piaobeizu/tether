// The version shown in the UI comes from the daemon, not from a constant here.
//
// The SPA is embedded in the daemon binary, so its version IS the daemon's —
// asking for it makes the two structurally incapable of disagreeing. The
// literal that used to live in this file had already drifted: the UI read
// v0.5.0 while the binary reported v0.5.1-0.20260805135746-69eedc4cac98
// (tether#70). That is the same failure as tether#67 — a version string that
// reads like an authority while being wired to nothing, kept correct only by
// someone remembering to bump it.
//
// 🔴 Ported back onto the phase-1 branch by tether#187, after tether#174 deleted
// the old SPA. What came back is the DATA half only: `fetchAppVersion`, its
// cache reset, and the fallback constant — which is exactly the surface
// version.test.ts imports.
//
// The React hook that used to close this file, `useAppVersion`, did NOT come
// back, for three reasons that hold independently:
//   - it had zero test coverage, so porting it would have moved untested code
//     into the new tree under the label of a ported A-grade module;
//   - both of its consumers (App.tsx, Settings.tsx) were deleted with the
//     shell, so it would have arrived with no call site at all; and
//   - whether the new shell reads this through a React hook is a SHELL
//     decision, and a port must not pre-empt it by shipping the answer.
// Same call web/src/lib/sessionActivity.ts already made on this branch:
// declarations port, shell code does not.
//
// The server half is untouched and live: internal/server/mux.go routes
// GET /api/v1/version (handleVersion), internal/server/version_api_test.go
// covers it, and `VersionResponse` below is the tygo-generated mirror of
// internal/wire's type — so the request shape is codegen-gated, not copied.
import type { VersionResponse } from './wire.gen'

/**
 * Shown until the daemon answers, and if it never does.
 *
 * Deliberately not a version number: an out-of-date but plausible-looking
 * version is worse than a visible gap, because it reads as fact. That is
 * exactly how the old constant misled.
 */
export const UNKNOWN_VERSION = '—'

// One request per page load, shared by every caller: no call site should cost a
// round trip of its own. The old shell rendered this string in four places,
// which is where the requirement came from; the new shell has none yet, which
// is precisely why the sharing lives in this module rather than in a caller —
// nothing that gets rewritten can lose it.
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
