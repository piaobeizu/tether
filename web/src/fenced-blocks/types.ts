// Fenced-block content schemas (D-19).
//
// A FencedBlock arrives over the wire as { kind, skill, content, blockId },
// where `content` is an OPAQUE JSON STRING that the emitting skill defines.
// The browser dispatches on `kind` and each block component JSON.parses the
// `content` string into the matching shape below.
//
// These types describe the UI's EXPECTED content shape — derived from the
// Claude Design handoff store (store.jsx). Daemon-side emission of this JSON
// is out of scope; the shapes are intentionally small and every field a
// component relies on is optional-guarded at parse time so a malformed or
// partial payload degrades gracefully rather than throwing.

/** Per-node status in a DAG block. Unknown values are treated as "queued". */
export type DagNodeStatus = 'done' | 'running' | 'queued' | 'failed'

export interface DagNode {
  /** stable node id, e.g. "n4" */
  id: string
  /** short human label, e.g. "write tests" */
  label: string
  /** execution status */
  status: DagNodeStatus
  /** duration in ms once finished; null/absent while queued or running */
  ms?: number | null
}

/** dag: a task-graph progress view (nodes + statuses + pause/rollback). */
export interface DagContent {
  /** headline shown in the block header */
  title?: string
  nodes: DagNode[]
  /** directed edges as [fromId, toId] pairs; optional (compact never uses them) */
  edges?: [string, string][]
  /** whether the run is currently paused */
  paused?: boolean
  /** total elapsed wall-clock time in ms */
  elapsedMs?: number
}

/** One field descriptor in a form block. */
export interface FormField {
  /** stable key used as the value map key, e.g. "name" */
  key: string
  /** field label, e.g. "task name" */
  label: string
  /** optional helper text under the label */
  hint?: string
  /** input widget kind; defaults to "text" */
  type?: 'text' | 'segmented' | 'switch'
  /** options for a "segmented" field */
  options?: string[]
  /** initial value: string for text/segmented, boolean for switch */
  value?: string | boolean
}

/** form: a labelled field grid with a submit footer. */
export interface FormContent {
  title?: string
  /** progress marker, e.g. "2/3" */
  step?: string
  fields: FormField[]
  /** primary button label; defaults to "apply" */
  submitLabel?: string
}

/** One selectable candidate row. */
export interface Candidate {
  /** stable id, e.g. "c2" */
  id: string
  /** headline */
  title: string
  /** one-line description */
  desc?: string
  /** short category tag, e.g. "modular" */
  tag?: string
}

/** candidates: a filterable checkbox list with a selection footer. */
export interface CandidatesContent {
  title?: string
  candidates: Candidate[]
  /** ids pre-selected by the skill */
  picked?: string[]
}

/** media: a 16:9 preview with a metadata footer. */
export interface MediaContent {
  title?: string
  /** image URL; when absent a placeholder is rendered */
  url?: string
  /** file type, e.g. "png" */
  format?: string
  /** human-readable size, e.g. "1.4 MB" */
  size?: string
  /** capture timestamp, free-form, e.g. "2026-05-02 · 14:08" */
  capturedAt?: string
  /** original path, for the "copy path" affordance */
  path?: string
}

/**
 * Parse an opaque fenced-block content string.
 * Returns the parsed object on success, or null on any failure
 * (malformed JSON, or a non-object payload). Callers render a
 * graceful fallback on null.
 */
export function parseContent<T>(content: string): T | null {
  try {
    const parsed = JSON.parse(content)
    if (parsed && typeof parsed === 'object') return parsed as T
    return null
  } catch {
    return null
  }
}
