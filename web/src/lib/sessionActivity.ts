// The TypeScript half of a cross-language contract that a GO TEST enforces.
//
// tether#174 deleted the old SPA, and this file with it — then `go test ./...`
// went red on TestSessionActivityContractIsMirroredInTypeScript
// (internal/session/activity_test.go:350), which does
// `os.ReadFile("../../web/src/lib/sessionActivity.ts")` and t.Fatalf's when it is
// missing. What came back is the DECLARATIONS only; the polling, subscription and
// React hooks that used to live here are the shell's job, and the shell is being
// rewritten.
//
// 🔴 Do not delete this file because nothing imports it yet — see the same note in
// wiSession.ts. It is one side of a contract whose other side reads this path.
//
// What the guard checks, and why it is a count as well as a loop: it collects
// every `SessionActivity*` const in the Go package by walking the AST — so a
// fourth state added later is covered without anyone remembering the test exists —
// requires at least four of them, and requires each VALUE to appear here as a
// QUOTED literal in code. Quoted, because the dangerous near-miss is a state name
// surviving only in a doc comment after a rename, which is exactly what a
// half-finished rename leaves behind. The failure being guarded was measured in
// tether#101: a frontend rename that is internally consistent passes `tsc -b` with
// exit 0 and leaves every typed-fixture test green, because a typed fixture is
// immune to the NAME.

/**
 * The three states, as a union derived from the literals below.
 *
 * Derived rather than written out, so the literals stay the single declaration.
 */
export type SessionActivityState =
  | typeof SESSION_ACTIVITY_WORKING
  | typeof SESSION_ACTIVITY_IDLE
  | typeof SESSION_ACTIVITY_HELD

/**
 * A turn is in flight. Mirrors session.SessionActivityWorking (Go).
 *
 * Declared WITHOUT a `: SessionActivityState` annotation on purpose: the union
 * above is derived from these three literals, so annotating them would widen each
 * one back to the union and make `Record<SessionActivityState, …>` uncheckable —
 * which turns an exhaustive label map into three optional keys.
 *
 * Deliberately NOT "the model is emitting output", which is not obtainable: the
 * agent's registry reports busy for the whole turn, tool execution included.
 */
export const SESSION_ACTIVITY_WORKING = 'working'

/**
 * A process has this conversation open and no turn is in flight. Mirrors
 * session.SessionActivityIdle (Go).
 *
 * "No turn in flight", not "between turns": this is also the state reported for
 * the agent's `waiting` (blocked on the user) and `shell` (a shell task running
 * while the agent is idle), and "between turns" would be false for both.
 */
export const SESSION_ACTIVITY_IDLE = 'idle'

/**
 * A live agent process has this conversation open, and whether a turn is in flight
 * is NOT OBSERVABLE. Mirrors session.SessionActivityHeld (Go).
 *
 * Not called `unknown`, because "a coding agent has this open" is a fact — only
 * the inside of it is opaque. It is also where a status this build has not been
 * taught lands, so an unfamiliar word degrades to "cannot tell" rather than to a
 * claim in either direction.
 */
export const SESSION_ACTIVITY_HELD = 'held'

/**
 * The endpoint. Mirrors session.SessionActivityPath (Go). A top-level path, NOT a
 * leaf under `/api/v1/sessions/`.
 */
export const SESSION_ACTIVITY_PATH = '/api/v1/session-activity'

/** How often the state is refreshed while at least one row is on screen. */
export const SESSION_ACTIVITY_POLL_MS = 3000

/** sid -> state. A sid nothing holds is absent. */
export type SessionActivityMap = Record<string, SessionActivityState>
