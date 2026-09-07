// A pane the shell can route to but that nothing has been built into yet
// (tether#195).
//
// 🔴 The wording is a requirement, not filler. tether#195's scope allows pane
// contents to be placeholders and forbids a placeholder from PRETENDING to have a
// capability — which is docs/tether-ui-invariants.md §2 R10 ("say you don't know
// when you don't know") applied to a UI surface rather than to a data value.
//
// So this component renders a sentence and nothing else. Specifically it does NOT
// render:
//
//   · a disabled button, a greyed toolbar or an empty list. Each of those is a
//     claim that the capability exists and is momentarily unavailable, which is
//     the same substitution R10 forbids — and the invariant doc records the
//     shape: tether#179 found cloudcli's interaction panel scraping `❯ 1. Yes`
//     out of prose with a regex and rendering it as disabled buttons, and
//     tether#173 §2 rules that "not implementing it is a net gain".
//   · a spinner. A spinner says "this is arriving". Nothing is arriving.
//
// It DOES render the pane's own name, because the shell's breadcrumb (LAY-15) is
// asserted against what the mounted pane says it is — a placeholder that did not
// name itself would make that assertion compare the crumb with a copy of itself
// rather than with the pane.

import { PANE_LABEL, type PaneId } from './panes'

export interface PanePlaceholderProps {
  readonly pane: PaneId
}

export function PanePlaceholder({ pane }: PanePlaceholderProps) {
  return (
    <section className="sh-placeholder" data-pane={pane} aria-labelledby={`sh-pane-title-${pane}`}>
      <h2 id={`sh-pane-title-${pane}`} className="sh-pane-title">
        {PANE_LABEL[pane]}
      </h2>
      <p className="sh-placeholder-note">
        This pane has no implementation on this branch yet. The shell can route to it; there is
        nothing behind it.
      </p>
    </section>
  )
}

export default PanePlaceholder
