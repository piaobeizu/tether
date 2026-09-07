// What the top of a transcript is allowed to claim about earlier history
// (tether#195, owner ruling ④).
//
// ── the defect this exists for ──────────────────────────────────────────────
//
// tether has two transcript stores and merges their session lists, tether's
// winning. Only ONE of them paginates: the cc-source store's cursor is a byte
// offset into a jsonl file, while tether's own `LoadHistory` is an unbounded
// `os.ReadFile` and answers any `?before=` with an empty page
// (internal/session/sessionlist.go, measured for docs/tether-ui-invariants.md
// §3.5). So on a tether-source session the five rounds of pagination work behind
// PAGE-* do not fail — they silently never arrive. A spinner at the top of the
// list waits for a page that structurally cannot come.
//
// Owner ruling ④: say so. The UI states plainly that there is no more history
// rather than appearing to load some. Real backend pagination is a separate wi;
// this module does not make the daemon say more, it stops the UI from implying
// the daemon said something it did not.
//
// ── why the answer is derivable at all ──────────────────────────────────────
//
// POLL-13: the cursor and the "other record store" both arrive as response
// headers, and the daemon expresses "there is none" by OMITTING the header. That
// convention is what turns absence into an answer instead of into a gap. The two
// header names come from web/src/lib/transcriptWatch.ts, which mirrors the Go
// constants and is pinned by a Go test (TestTranscriptPageHeadersAreMirroredInTypeScript)
// — so this module reads the daemon's own vocabulary rather than a copy of it.
//
// ── R10 ─────────────────────────────────────────────────────────────────────
//
// §2 R10 requires the three states (known-true / known-false / unknown) to be
// expressible in the TYPE, not folded into a boolean with a default. `'unknown'`
// below is that requirement: a probe that has not answered, or that failed,
// produces `'unknown'`, never `'start'`. The renderer has to handle both
// constructors, so "we could not tell" cannot be rendered as "this is the
// beginning" by forgetting a case — it is a compile error to forget one.

import {
  TRANSCRIPT_EARLIER_HEADER,
  TRANSCRIPT_OTHER_RECORD_HEADER,
} from '../lib/transcriptWatch'

/**
 * What the top-of-transcript marker may say.
 *
 * These are SCROLL-1's four states plus SCROLL-2's, in a discriminated union:
 *
 *   none      nothing to render — an empty transcript, or one holding only
 *             locally-generated notices (SCROLL-1 4th state, SCROLL-2)
 *   unknown   the daemon has not answered, or the probe failed (R10)
 *   more      an earlier page exists and can be asked for (SCROLL-1 1st)
 *   startOf   this is the beginning of the store that answered, and ANOTHER
 *             store also holds records for this session (SCROLL-1 3rd)
 *   start     this is the beginning, full stop (SCROLL-1 2nd)
 */
export type HistoryStart =
  | { kind: 'none' }
  | { kind: 'unknown' }
  | { kind: 'more' }
  | { kind: 'startOf'; otherStore: string }
  | { kind: 'start' }

/** What the caller knows about the transcript itself, independent of the daemon. */
export interface TranscriptShape {
  /** How many entries the transcript holds in total. */
  readonly entryCount: number
  /** How many of those were generated locally (session notices, permission cards, …). */
  readonly localEntryCount: number
}

/**
 * Decides what the marker may claim.
 *
 * `headers` is the response the transcript was served with, or `null` when there
 * has not been one yet or the request failed. `null` is not the same as a
 * response with no headers set: the first is "we do not know", the second is the
 * daemon saying "there is nothing earlier". POLL-14 makes the same distinction on
 * the request side — absence and zero are two different requests — and this is
 * that distinction on the response side.
 *
 * The emptiness check comes FIRST, before the headers are read. A transcript with
 * nothing in it renders no marker at all whatever the daemon says about earlier
 * pages, because a "this is the beginning" label above zero rows is a claim about
 * a list the reader cannot see.
 */
export function historyStart(
  headers: Headers | null,
  shape: TranscriptShape,
): HistoryStart {
  // SCROLL-1's fourth state and SCROLL-2, which are the same rule seen twice:
  // the marker describes SERVER-side history, so a transcript made entirely of
  // locally-generated entries has no server-side history to describe.
  if (shape.entryCount <= shape.localEntryCount) return { kind: 'none' }

  // R10. Not `{kind:'start'}`: "the probe has not answered" and "the daemon says
  // there is nothing earlier" are different facts and only one of them is
  // something to tell the reader.
  if (headers === null) return { kind: 'unknown' }

  if (headers.get(TRANSCRIPT_EARLIER_HEADER) !== null) return { kind: 'more' }

  const otherStore = headers.get(TRANSCRIPT_OTHER_RECORD_HEADER)
  if (otherStore !== null && otherStore !== '') return { kind: 'startOf', otherStore }

  return { kind: 'start' }
}
