// What the shell puts inside each pane container (tether#195).
//
// One place, so that a wi adding a real pane changes one line and touches neither
// the shell's layout nor its selection state. That seam is the point of the
// skeleton: tether#173 §2 lists the shell and the Chat panel as two parallel
// scope lines, and the Chat rewrite plugs in here.
//
// 🔴 Everything except `workspace` is a labelled placeholder, and the placeholders
// are honest about it — see PanePlaceholder.tsx. `workspace` is real because owner
// ruling ③ requires the file tree and its hide control: hiding is read-only, and
// without it the tree is unusable on the workspace acceptance is judged against
// (41 `pf.*` directories at its root), which would leave phase 1 with no feedback
// loop at all.

import type { ReactNode } from 'react'
import { PanePlaceholder } from './PanePlaceholder'
import { WorkspacePane, type HidePolicy } from './WorkspacePane'
import type { PaneId } from './panes'

export interface RenderPaneOptions {
  /** web/src/lib/hidden.ts, once tether#196 has landed it. */
  readonly hide?: HidePolicy
}

export function makeRenderPane(options: RenderPaneOptions = {}) {
  return function renderPane(pane: PaneId): ReactNode {
    if (pane === 'workspace') return <WorkspacePane hide={options.hide} />
    return <PanePlaceholder pane={pane} />
  }
}

export const renderPane = makeRenderPane()
