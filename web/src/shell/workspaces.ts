// Which workspace the tree is showing (tether#195).
//
// Split from the component so the selection rule is exercised as arithmetic. The
// rule itself is WS-4 in <workspace>/docs/tether-ui-invariants.md §3.11,
// extracted from the old WorkspacePane.test.tsx — a C-grade file whose
// assertions were bound to a store that no longer exists, but whose rule is not.

/** A workspace, as far as selecting one is concerned. `GET /api/v1/workspaces`. */
export interface WorkspaceSummary {
  readonly id: string
  readonly name: string
  readonly path: string
}

/** localStorage key holding the last selected workspace id. */
export const SELECTED_WORKSPACE_KEY = 'tether_workspace_id'

/**
 * WS-4. Priority: the live selection, then the remembered id, then the first
 * entry the daemon listed.
 *
 * A remembered id that is no longer in the registry is IGNORED rather than
 * carried: the workspace it names is gone, and keeping it selected would send the
 * next file request at an id the daemon does not know. The same reasoning applies
 * to `current`, which is why both are filtered through the registry rather than
 * only the stored one — a workspace can be removed while it is on screen.
 *
 * An empty registry returns null, and null is a real answer here: it means "there
 * is nothing to show", not "the default". A caller that substituted a placeholder
 * id would send that id to the daemon.
 */
export function resolveSelection(
  registry: readonly WorkspaceSummary[],
  current: string | null,
  remembered: string | null,
): string | null {
  const has = (id: string | null): boolean => id !== null && registry.some(w => w.id === id)
  if (has(current)) return current
  if (has(remembered)) return remembered
  return registry[0]?.id ?? null
}
