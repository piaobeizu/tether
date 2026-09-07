// The TypeScript half of a cross-language contract that a GO TEST enforces.
//
// tether#174 deleted the old SPA, and this file with it — then `go test ./...`
// went red on TestSessionSummaryIsMirroredInTypeScript
// (internal/session/sessionlist_test.go:345), which does
// `os.ReadFile("../../web/src/lib/wiSession.ts")` and t.Fatalf's when it is
// missing. What came back is the DECLARATION only: the 400-odd lines of fetching,
// store wiring and label formatting that used to live here went with the rest of
// the SPA, because they are the shell's job and the shell is being rewritten.
//
// 🔴 Do not "clean this up" by deleting it because nothing imports it yet. It is
// not dead code; it is one side of a contract, and the other side is a test that
// reads this exact path. The next thing that needs a session row imports
// SessionSummary from here rather than declaring its own — which is the rule that
// made the guard possible in the first place.
//
// tygo covers internal/wire -> wire.gen.ts and nothing else, so this type is
// hand-mirrored. Before tether#101, a field added to the Go struct and forgotten
// here compiled cleanly on BOTH sides, every test passed, and the feature was a
// silent no-op that looked finished; eight frontend files consumed the type and
// none of them would have complained. The guard reads the Go struct's json tags by
// reflection and requires a property here for each one.

/**
 * Which store a session row's transcript came from.
 *
 * Mirrors session.SourceTether / session.SourceCC (Go). It is a FACT about where
 * the row came from and deliberately not a resumability flag: cc's `--resume`
 * reports failure only after a prompt has been delivered, so a "resumable" bit
 * computed daemon-side would be a guess that goes stale. `runningAs` below carries
 * the one obstacle that IS observable, and nothing here predicts anything.
 */
export type SessionSource = 'tether' | 'cc'

/**
 * One row of GET /api/v1/sessions — mirrors session.SessionSummary (Go).
 *
 * Hand-written, and gated by TestSessionSummaryIsMirroredInTypeScript
 * (internal/session/sessionlist_test.go). It is THE declaration: a component that
 * needs a field adds it here rather than widening the type at its own call site.
 */
export interface SessionSummary {
  sid: string
  workItem?: string
  title?: string
  updatedAt: number
  /**
   * Optional in the TYPE although the daemon always sends it, so that hand-built
   * fixtures in tests that are not about provenance stay valid. An absent source
   * reads as 'tether', which is what every row was before tether#92.
   */
  source?: SessionSource
  /**
   * The kind of live agent process holding this session AT THE MOMENT THE DAEMON
   * BUILT THE LIST — the agent's own value, 'bg' / 'daemon' / 'daemon-worker',
   * never 'interactive'. Absent when none was observed (tether#101).
   *
   * An OBSERVATION, not a promise, and both ways of being out of date are fine: a
   * job that has since finished simply opens, and one still running meets a
   * refusal that explains itself (wire code session_held_by_background_agent). The
   * authority is the attach path, never this field — which is why a row carrying it
   * must stay CLICKABLE. Disabling it would be a lie a minute later, and an
   * unexplained disabled row is worse than a click that answers.
   *
   * A plain string and not a union of the four kinds: it is quoted from another
   * program's file, so a kind this build has not heard of must arrive intact rather
   * than fail a narrowing. Non-empty means "something is holding this", which stays
   * correct for a fifth kind.
   */
  runningAs?: string
}
