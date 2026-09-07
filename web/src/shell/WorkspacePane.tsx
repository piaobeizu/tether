// The workspace pane: the registered workspaces, and the file tree of the
// selected one (tether#195, owner ruling ③).
//
// ── read-only, and that is a boundary not an omission ───────────────────────
//
// tether#173 §2 says the file tree stays read-only, and §3 puts workspace CRUD
// out of scope. So there is no add, no remove and no editor here. In particular
// the DELETE path that tether#173 §6 (AC-R9, inherited from tether#164) is about
// is not reimplemented — the defect it records lives in code this branch deleted,
// and re-adding the button in order to fix it would be adding the feature.
//
// ── AC-R9, which this wi owes ───────────────────────────────────────────────
//
// web/src/lib/fileTreeCache.ts turns a refused listing into `new
// Error(await httpErrorMessage(res))` so the daemon's own sentence survives to
// whoever renders it — and its header records that since tether#174 deleted
// WorkspaceTree.tsx there has been NO renderer, naming the gap as owed by "the wi
// that next renders a file tree". This is that wi, so the refusal text is
// rendered, and WorkspacePane.test.tsx asserts it by stubbing `fetch` and reading
// the screen rather than by asserting that a function threw.
//
// Three of tether#159's read-path refusals are only reachable here — "that path
// must be relative to the workspace root", "…is outside the workspace", "…is not
// a directory" — and all three used to arrive at the user as "HTTP 400".
//
// R9's design requirement is one response path: every response in this file goes
// through `httpErrorMessage` exactly once, at the one place that checks `res.ok`,
// and the tree's responses go through fileTreeCache's. There is no second
// extractor.
//
// ── the hide control ────────────────────────────────────────────────────────
//
// `hide` is the socket for web/src/lib/hidden.ts, which tether#196 is porting onto
// this branch. Its shape is that module's public API exactly, so wiring it is a
// namespace import and nothing else.
//
// 🔴 It is NOT a local reimplementation and must not become one. With no policy
// supplied the tree shows every entry and renders no hide control at all: a
// button that cannot change what is shown is a control claiming a capability, and
// an inert "+N hidden" row is worse — it claims entries are being withheld. R10.
//
// ── a labelled list, NOT `role="tree"` ──────────────────────────────────────
//
// 🔴 The first version of this file announced `role="tree"`, `role="group"` and
// `role="treeitem"`. All three were removed, for the same reason `ea2093e`
// removed `role="tablist"` from the pane strip: those roles carry a keyboard
// contract — Up/Down move between visible rows, Right/Left expand and collapse,
// Home/End jump to the ends, and the whole tree is ONE tab stop with a roving
// tabindex — and none of it is implemented here. Every row is a plain `<button>`
// in natural tab order. `aria-expanded` made the announcement MORE specific, not
// less, because it named a widget that answers to arrow keys.
//
// The check is in WorkspacePane.test.tsx — "R10 in the accessibility tree: claims
// no role whose keyboard contract is unimplemented" — which renders the pane and
// asserts `document.querySelectorAll('[role="tree"]')` (and `treeitem`, `group`,
// and any `[tabindex]`) is empty. So re-adding a role without the behaviour turns
// red rather than shipping.
//
// 🔴 This paragraph used to print a `grep` for those constructs and call the
// command itself the check — "returns nothing, which is the check". It does not
// return nothing: it matches the tombstone sentence above and the line the grep
// was written on, and it would go on matching them however the code changed. A
// codebase that documents what it removed cannot use a source-text grep as the
// gate for the removal, because writing the gate down is enough to satisfy it. A
// DOM assertion cannot match its own documentation, which is why that is the one
// named here. Until the contract exists, a `<ul>`/`<li>` with an accessible name
// is a complete and honest pattern, and `aria-expanded` sits on the element it is
// actually true of — the button that toggles the subtree.
//
// This is R10 applied to the accessibility tree instead of to a sentence, and it
// is the standard the rest of this branch is held to.

import { useCallback, useEffect, useMemo, useState } from 'react'
import { createFileTreeCache, type FileEntry, type FileTreeCache } from '../lib/fileTreeCache'
import { httpErrorMessage } from '../lib/httpError'
import { Button } from '../ui/primitives'
import {
  resolveSelection,
  SELECTED_WORKSPACE_KEY,
  type WorkspaceSummary,
} from './workspaces'

/**
 * The hide-pattern rules, injected.
 *
 * Structurally identical to `web/src/lib/hidden.ts`'s exports, so
 * `import * as hidden from '../lib/hidden'` satisfies it once that module lands.
 */
export interface HidePolicy {
  partitionEntries<T extends { name: string }>(
    entries: readonly T[],
    patterns: readonly string[],
  ): { visible: T[]; hidden: T[] }
  suggestHidePattern(name: string, siblings: readonly string[]): string
  toggleHidePattern(patterns: readonly string[], pattern: string): string[]
  hidingPattern(name: string, patterns: readonly string[]): string
  loadHidePatterns(): string[]
  saveHidePatterns(patterns: readonly string[]): void
}

export interface WorkspacePaneProps {
  readonly fetchFn?: typeof fetch
  readonly storage?: { getItem(k: string): string | null; setItem(k: string, v: string): void }
  readonly hide?: HidePolicy
}

type ListState =
  | { readonly kind: 'loading' }
  | { readonly kind: 'ready'; readonly items: WorkspaceSummary[] }
  | { readonly kind: 'refused'; readonly message: string }

export function WorkspacePane({ fetchFn, storage, hide }: WorkspacePaneProps) {
  // 🔴 `useMemo`, and it is load-bearing: without it the tree drops its cache and
  // re-requests every open directory on each re-render of an ANCESTOR.
  //
  // The default is a fresh arrow per render, so it is a new identity every time.
  // `FileTree` memoises the tree cache on it and `Directory`'s effect depends on
  // the cache, so a new identity means a new empty cache and a fresh listing.
  //
  // MEASURED, both arms, because the first version of this comment was wrong. It
  // claimed an infinite loop — render → new cache → effect → setState → render —
  // and that does not happen: `setEntries` lives in `Directory`, a descendant, so
  // it re-renders `Directory` and not this component, and the identity is stable
  // until something above re-renders. What the differential actually shows, over
  // five re-renders of a parent:
  //
  //     with this useMemo      1 listing after mount, 1 after the re-renders
  //     without it             1 listing after mount, 6 after the re-renders
  //
  // One per ancestor re-render. That matters here specifically because Shell
  // re-renders on every pane switch and this pane is mounted in a column, so
  // switching panes would re-read the visible tree each time.
  //
  // The suite could not have caught it: every other test in this file injects
  // `fetchFn`, which is a stable identity, so the defect lives exactly in the
  // production path the tests do not take. `WorkspacePane.test.tsx`'s
  // "an ancestor re-render does not re-read the tree" case takes that path.
  const doFetch = useMemo(
    () => fetchFn ?? ((...a: Parameters<typeof fetch>) => fetch(...a)),
    [fetchFn],
  )
  // One storage object, resolved once. The alternative — `storage?.setItem(...)`
  // with a `localStorage` fallback beside it — is two paths through every read
  // and write, which is the shape R9 is about even though this one is not a fetch.
  const store = useMemo(
    () => storage ?? (typeof localStorage !== 'undefined' ? localStorage : null),
    [storage],
  )
  const [list, setList] = useState<ListState>({ kind: 'loading' })
  const [selected, setSelected] = useState<string | null>(null)

  useEffect(() => {
    let live = true
    void (async () => {
      try {
        const res = await doFetch('/api/v1/workspaces')
        // The single `res.ok` check on this path, and the single call to the
        // shared extractor. R9's requirement is that this is not repeated per
        // call site; there is one call site.
        if (!res.ok) {
          const message = await httpErrorMessage(res)
          if (live) setList({ kind: 'refused', message })
          return
        }
        const items = (await res.json()) as WorkspaceSummary[]
        if (!live) return
        setList({ kind: 'ready', items })
        setSelected(prev => resolveSelection(items, prev, safeGet(store, SELECTED_WORKSPACE_KEY)))
      } catch (e) {
        // A transport failure has no response to extract a sentence from, so it
        // says what it knows and no more. R10: not "the daemon refused".
        if (live) setList({ kind: 'refused', message: messageOf(e) })
      }
    })()
    return () => {
      live = false
    }
    // Deliberately mount-once: this loads the registry, and re-running it on a
    // changed `doFetch` identity would re-read the list on every ancestor
    // re-render — the same defect the memo above exists to stop, through the other
    // door. The `live` flag is what makes that safe under StrictMode's
    // effect → cleanup → effect (R5): the superseded run's response is discarded
    // rather than racing the live one.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const choose = useCallback(
    (id: string) => {
      setSelected(id)
      try {
        store?.setItem(SELECTED_WORKSPACE_KEY, id)
      } catch {
        /* private mode / quota — the session keeps working, it just will not persist */
      }
    },
    [store],
  )

  return (
    <section className="sh-workspace" aria-label="Workspace">
      <h2 className="sh-pane-title">Workspace</h2>

      {list.kind === 'loading' && <p className="sh-note">Loading workspaces…</p>}

      {list.kind === 'refused' && (
        // The daemon's own sentence, verbatim. This element is what AC-R9 asks
        // for: the wording on the screen, not in a rejected promise.
        <p className="sh-error" role="alert">
          {list.message}
        </p>
      )}

      {list.kind === 'ready' && list.items.length === 0 && (
        <p className="sh-note">No workspaces are registered.</p>
      )}

      {list.kind === 'ready' && list.items.length > 0 && (
        <>
          <ul className="sh-ws-list" aria-label="Workspaces">
            {list.items.map(ws => (
              <li key={ws.id}>
                <button
                  type="button"
                  className="sh-ws-row"
                  aria-current={ws.id === selected ? 'page' : undefined}
                  onClick={() => choose(ws.id)}
                >
                  {ws.name || ws.path}
                </button>
              </li>
            ))}
          </ul>
          {selected !== null && (
            <FileTree key={selected} workspaceId={selected} fetchFn={doFetch} hide={hide} />
          )}
        </>
      )}
    </section>
  )
}

interface FileTreeProps {
  readonly workspaceId: string
  readonly fetchFn: typeof fetch
  readonly hide?: HidePolicy
}

function FileTree({ workspaceId, fetchFn, hide }: FileTreeProps) {
  const cache = useMemo<FileTreeCache>(
    () => createFileTreeCache(workspaceId, fetchFn),
    [workspaceId, fetchFn],
  )
  const [patterns, setPatterns] = useState<readonly string[]>(() => hide?.loadHidePatterns() ?? [])

  const setAndSave = useCallback(
    (next: readonly string[]) => {
      setPatterns(next)
      hide?.saveHidePatterns(next)
    },
    [hide],
  )

  return (
    // The accessible name goes on the root `<ul>` rather than on this wrapper: a
    // bare `<div>` with an `aria-label` and no role exposes nothing, so the name
    // would be unreachable. `.sh-tree` is the styling container and nothing else.
    <div className="sh-tree">
      <Directory
        dir=""
        label="Files"
        cache={cache}
        hide={hide}
        patterns={patterns}
        onPatterns={setAndSave}
        depth={0}
      />
    </div>
  )
}

interface DirectoryProps {
  readonly dir: string
  readonly cache: FileTreeCache
  readonly hide?: HidePolicy
  readonly patterns: readonly string[]
  readonly onPatterns: (next: readonly string[]) => void
  readonly depth: number
  /** Accessible name for this level's list. Only the root level has one. */
  readonly label?: string
}

function Directory({ dir, cache, hide, patterns, onPatterns, depth, label }: DirectoryProps) {
  const [entries, setEntries] = useState<FileEntry[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [open, setOpen] = useState<readonly string[]>([])
  const [revealed, setRevealed] = useState(false)

  useEffect(() => {
    let live = true
    void cache
      .load(dir)
      .then(e => {
        if (live) setEntries(e)
      })
      .catch(e => {
        // fileTreeCache already put the daemon's sentence in the Error's message
        // (its `httpErrorMessage(res)` call). Re-deriving one here would be the
        // second extractor R9 exists to prevent.
        if (live) setError(messageOf(e))
      })
    return () => {
      live = false
    }
  }, [cache, dir])

  if (error !== null) {
    return (
      <p className="sh-error" role="alert">
        {error}
      </p>
    )
  }
  if (entries === null) return <p className="sh-note">Loading…</p>

  const split = hide
    ? hide.partitionEntries(entries, patterns)
    : { visible: entries, hidden: [] as FileEntry[] }
  const shown = revealed ? entries : split.visible
  const siblings = entries.map(e => e.name)

  return (
    <ul className="sh-tree-level" aria-label={label} data-dir={dir} data-depth={depth}>
      {shown.map(entry => {
        const path = dir === '' ? entry.name : `${dir}/${entry.name}`
        const expanded = open.includes(path)
        const hidingBy = hide ? hide.hidingPattern(entry.name, patterns) : ''
        return (
          <li key={path} className="sh-tree-row" data-dirty={String(entry.dirty)}>
            <span className="sh-tree-line">
              {entry.isDir ? (
                <button
                  type="button"
                  className="sh-tree-name"
                  data-kind="dir"
                  // On the BUTTON, not on the row: `aria-expanded` is true of the
                  // control that toggles the subtree. On an `<li>` it was part of
                  // a `role="treeitem"` announcement whose keyboard contract does
                  // not exist here — see this file's header.
                  aria-expanded={expanded}
                  onClick={() =>
                    setOpen(prev =>
                      prev.includes(path) ? prev.filter(p => p !== path) : [...prev, path],
                    )
                  }
                >
                  {entry.name}
                </button>
              ) : (
                <span className="sh-tree-name" data-kind="file">
                  {entry.name}
                </span>
              )}
              {hide && (
                // WS-12/WS-13/WS-14 all live in this one button, because the
                // control is one button whose meaning flips with the row it sits
                // on: on a shown row it hides (with whatever glob covers the most
                // siblings), on a revealed hidden row it drops the glob that is
                // hiding it — not the name, which a glob would not match.
                //
                // The vendored `Button` primitive rather than a bare `<button>`:
                // this is a real control and web/src/ui/primitives.ts is the seam
                // that exists so tether's controls look like one system. The
                // structural rules (where it sits, how it reveals on hover) are
                // shell.css's; the control's own appearance is the primitive's.
                <Button
                  type="button"
                  variant="ghost"
                  size="icon"
                  className="sh-tree-hide"
                  aria-label={
                    hidingBy === ''
                      ? `Hide ${hide.suggestHidePattern(entry.name, siblings)}`
                      : `Unhide ${hidingBy}`
                  }
                  onClick={() =>
                    onPatterns(
                      hide.toggleHidePattern(
                        patterns,
                        hidingBy === '' ? hide.suggestHidePattern(entry.name, siblings) : hidingBy,
                      ),
                    )
                  }
                >
                  {hidingBy === '' ? '×' : '+'}
                </Button>
              )}
            </span>
            {entry.isDir && expanded && (
              <Directory
                dir={path}
                cache={cache}
                hide={hide}
                patterns={patterns}
                onPatterns={onPatterns}
                depth={depth + 1}
              />
            )}
          </li>
        )
      })}
      {/* WS-11. Hidden is never gone: a directory with hidden children says how
          many, and one click reveals them in place. Rendered only when something
          is actually hidden, so it is never a claim about nothing. */}
      {!revealed && split.hidden.length > 0 && (
        <li className="sh-tree-hidden-row">
          <Button
            type="button"
            variant="ghost"
            size="sm"
            className="sh-tree-reveal"
            onClick={() => setRevealed(true)}
          >
            {`+${split.hidden.length} hidden`}
          </Button>
        </li>
      )}
    </ul>
  )
}

function messageOf(e: unknown): string {
  return e instanceof Error && e.message !== '' ? e.message : 'Request failed.'
}

function safeGet(store: WorkspacePaneProps['storage'] | null, key: string): string | null {
  try {
    return store?.getItem(key) ?? null
  } catch {
    return null
  }
}

export default WorkspacePane
