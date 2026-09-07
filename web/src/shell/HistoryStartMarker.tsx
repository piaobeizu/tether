// Renders what historyStart() decided (tether#195, owner ruling ④).
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
//            more thing to keep true.
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
