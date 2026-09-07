// Renders what historyStart() decided (tether#195, owner ruling ④).
//
// 🔴 NOTHING RENDERS THIS YET, and that is a scope boundary rather than an
// oversight — but do not read this file as evidence that a user is being told
// anything today.
//
// Ruling ④'s landing point is the top of the transcript, and the transcript lives
// in the Chat panel, which tether#195 explicitly does not rewrite (it is
// web/src/panes/chat/index.tsx on `main`, a single 3,241-line file, and
// tether#173 §2 lists it as a parallel scope line with its own wi). So this wi
// builds and pins the surface; the wi that rewrites the Chat panel mounts it, and
// until then a tether-source session still shows whatever the old UI on `main`
// shows.
//
// The gap is written down here on purpose. web/src/lib/fileTreeCache.ts did the
// same thing for its refusal wording — "there is currently NO renderer … an open
// tether#173 AC-R9 gap, owed by the wi that next renders a file tree" — and that
// note is the only reason this wi knew it had inherited that debt. This is the
// same note for the same kind of debt: OWED BY THE WI THAT NEXT RENDERS A
// TRANSCRIPT.
//
// SCROLL-10 pins the marker at "exactly one grid cell with exactly one visible
// child, in both states". That is a layout property of the transcript viewport,
// which belongs to the Chat-panel rewrite, so this component does not claim
// SCROLL-10 — but it is written not to make it harder: one element, one child,
// no branching on height.
//
// The wording is the point of the component. R10's discipline in
// docs/tether-ui-invariants.md §2 is not "show an error state", it is that a
// sentence must not read as a fact the code does not have. Each case below says
// only what its input supports:
//
//   more     no sentence at all — the affordance to load is the message, and a
//            label saying "there is more" beside a button that loads more is one
//            more thing to keep true. 🔴 With no `onLoadEarlier` there is no
//            affordance either, and then this component renders NOTHING — see
//            below.
//   start    a plain statement of the end of the record.
//   startOf  names WHOSE beginning this is, because another store also has
//            records for this session and "the beginning" would be false of the
//            session as a whole. This is SCROLL-1's third state and it exists
//            because the two-store merge is real.
//   unknown  says it cannot tell. Not "no more history" — that is the exact
//            substitution R10 forbids, and ruling ④ is an instance of it.
//   none     renders nothing.

import type { HistoryStart } from './historyStart'

/** Names the store the marker mentions, for a reader who has never heard of it. */
const STORE_LABEL: Readonly<Record<string, string>> = {
  cc: 'the Claude Code session log',
}

function storeLabel(store: string): string {
  return STORE_LABEL[store] ?? store
}

export interface HistoryStartMarkerProps {
  readonly state: HistoryStart
  /** Invoked for the 'more' case. Absent means there is no way to ask for a page. */
  readonly onLoadEarlier?: () => void
}

export function HistoryStartMarker({ state, onLoadEarlier }: HistoryStartMarkerProps) {
  if (state.kind === 'none') return null

  // 🔴 The same standard Shell.tsx's `Resizer` holds itself to (`if (!columns)
  // return null`), and it was NOT held here at first: the 'more' case rendered a
  // "Load earlier messages" button with `onClick={undefined}` whenever no handler
  // was passed, and the test pinned that the button EXISTED with no handler. A
  // control on screen is a claim that the capability is there — the one sentence
  // this file's header spends forty lines on — and a button that does nothing
  // when pressed is the loudest form of it. Same PR, same argument, opposite
  // standard, on the ruling ④ / R10 axis the whole component exists for.
  //
  // Silence rather than a substitute sentence, deliberately. The alternative
  // considered was a note like "there is earlier history, but it cannot be
  // loaded here", and it was rejected: `more` is not a state a mounted consumer
  // can be IN without a handler — the handler and the page request are the same
  // wiring — so the sentence would describe a misconfiguration to the reader
  // instead of to the developer. Rendering nothing claims nothing, which is
  // what 'none' already does and is R10's floor.
  //
  // Check: restore `onClick={onLoadEarlier}` without the guard and
  // HistoryStartMarker.test.tsx's "renders no control when it has no way to load"
  // case fails.
  if (state.kind === 'more' && onLoadEarlier === undefined) return null

  return (
    <div className="sh-history-start" data-history-start={state.kind}>
      {state.kind === 'more' ? (
        <button type="button" className="sh-history-start-more" onClick={onLoadEarlier}>
          Load earlier messages
        </button>
      ) : (
        <p className="sh-history-start-note">{noteFor(state)}</p>
      )}
    </div>
  )
}

function noteFor(state: Exclude<HistoryStart, { kind: 'none' } | { kind: 'more' }>): string {
  switch (state.kind) {
    case 'start':
      return 'No more history — this is the beginning of this conversation.'
    case 'startOf':
      // Deliberately two clauses. The first is what this store can answer for;
      // the second is the part the reader would otherwise have to guess at, and
      // guessing wrong here means concluding messages were lost.
      return `No more history in this record. Earlier messages for this session may still be in ${storeLabel(
        state.otherStore,
      )}.`
    case 'unknown':
      return 'Cannot tell whether there is earlier history.'
  }
}

export default HistoryStartMarker
