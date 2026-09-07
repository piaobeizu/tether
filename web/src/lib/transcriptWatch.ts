// The TypeScript half of a cross-language contract that GO TESTS enforce.
//
// tether#174 deleted the old SPA, and this file with it — then `go test ./...`
// went red on TestTranscriptUpdatedAtHeaderIsMirroredInTypeScript
// (internal/session/transcriptversion_test.go:160) and
// TestTranscriptPageHeadersAreMirroredInTypeScript (:184), both of which do
// `os.ReadFile("../../web/src/lib/transcriptWatch.ts")` and t.Fatalf when it is
// missing. What came back is the header NAMES only; the probe, the version
// bookkeeping and the fetch wrappers are the shell's job, and the shell is being
// rewritten.
//
// 🔴 Do not delete this file because nothing imports it yet — see the same note in
// wiSession.ts. It is one side of a contract whose other side reads this path.
//
// A header name is a plain string on both sides. Rename it in Go alone and every
// Go test still passes; rename it here alone and `tsc -b` exits 0, every typed
// fixture stays green, and the probe reads `undefined` from every response forever
// — which does not error, it just never reports a change. The feature is then dead
// in precisely the way that looks alive. The guards require a QUOTED literal, so a
// name surviving only in a doc comment does not satisfy them.

/**
 * The header carrying the served transcript's version (mtime, Unix ms). Mirrors
 * session.TranscriptUpdatedAtHeader (Go).
 */
export const TRANSCRIPT_UPDATED_AT_HEADER = 'X-Tether-Transcript-Updated-At'

/**
 * The header carrying the byte offset to ask for to read the page BEFORE the one
 * in the response (tether#107); absent when there is no such page. Mirrors
 * session.TranscriptEarlierHeader (Go).
 *
 * A one-sided rename costs more here than for the version header above: that one
 * going quiet freezes the transcript, which a reader eventually notices, whereas
 * this one going quiet makes the pane stop offering the button AND start
 * asserting, at the top of a 117 MiB transcript, that this is where the
 * conversation began. A confident false statement, at exit code 0.
 */
export const TRANSCRIPT_EARLIER_HEADER = 'X-Tether-Transcript-Earlier'

/**
 * The header naming a store OTHER than the one that answered which also holds a
 * record for this session (tether#107), or absent when there is none. Mirrors
 * session.TranscriptOtherRecordHeader (Go). `cc` is the only value the daemon can
 * currently produce.
 */
export const TRANSCRIPT_OTHER_RECORD_HEADER = 'X-Tether-Transcript-Other-Record'

/** How often the probe runs while a session with no live stream is on screen. */
export const TRANSCRIPT_POLL_MS = 3000
