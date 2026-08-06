import { useEffect, useMemo, useState } from 'react'
import { Icon } from '../../lib/icons'
import { useStore } from '../../lib/store'
import { createFileTreeCache, type FileEntry } from './fileTreeCache'
import {
  hidingPattern,
  loadHidePatterns,
  matches,
  partitionEntries,
  saveHidePatterns,
  suggestHidePattern,
  toggleHidePattern,
} from './hidden'

interface WorkspaceTreeProps {
  workspaceId: string
}

interface NodeState {
  expanded: boolean
  loading: boolean
  error: string | null
  entries: FileEntry[] | null // null until first successful load
}

/** Lazy, collapsible file tree for a single workspace, rooted at '' (workspace root). */
export default function WorkspaceTree({ workspaceId }: WorkspaceTreeProps) {
  const cache = useMemo(() => createFileTreeCache(workspaceId), [workspaceId])
  const [nodes, setNodes] = useState<Record<string, NodeState>>({})
  const select = useStore(s => s.select)

  // tether#71 — which entries the tree declines to show. Component state rather
  // than a store slice because panes/workspace/index.tsx mounts at most ONE
  // WorkspaceTree (`expandedId` is a single id), so there is no second copy to
  // go stale; a remount re-reads localStorage, which is also the fix if that
  // ever stops being true.
  const [hidePatterns, setHidePatterns] = useState<string[]>(loadHidePatterns)

  const toggleHide = (pattern: string) => {
    setHidePatterns(prev => {
      const next = toggleHidePattern(prev, pattern)
      saveHidePatterns(next)
      return next
    })
  }

  // Clicking a file (not dir) row focuses it in the middle canvas. `path` is
  // already relative to the workspace root — same shape fetchFile expects.
  const selectFile = (path: string) => {
    select({ file: { wsId: workspaceId, path } })
  }

  const expand = (dir: string) => {
    setNodes(prev => ({
      ...prev,
      [dir]: { expanded: true, loading: true, error: null, entries: prev[dir]?.entries ?? null },
    }))
    cache.load(dir).then(entries => {
      setNodes(prev => ({ ...prev, [dir]: { expanded: true, loading: false, error: null, entries } }))
    }).catch((e: unknown) => {
      setNodes(prev => ({
        ...prev,
        [dir]: { expanded: true, loading: false, error: e instanceof Error ? e.message : String(e), entries: null },
      }))
    })
  }

  const toggle = (dir: string) => {
    const node = nodes[dir]
    if (node?.expanded) {
      setNodes(prev => ({ ...prev, [dir]: { ...node, expanded: false } }))
      return
    }
    // Already cached client-side from a prior expand — no re-fetch, just show it.
    if (node?.entries) {
      setNodes(prev => ({ ...prev, [dir]: { ...node, expanded: true } }))
      return
    }
    expand(dir)
  }

  // Auto-expand the workspace root exactly once when the tree mounts (or the
  // workspace changes), so the top-level listing is visible without an
  // explicit click.
  useEffect(() => {
    setNodes({})
    expand('')
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [workspaceId])

  const rootNode = nodes['']

  return (
    <div className="ws-tree">
      {rootNode?.loading && <div className="tree-row" style={{ paddingLeft: 8 }}>loading…</div>}
      {rootNode?.error && <div className="tree-row" style={{ paddingLeft: 8, color: 'var(--danger)' }}>{rootNode.error}</div>}
      {rootNode?.entries && (
        <TreeChildren
          dir=""
          entries={rootNode.entries}
          depth={0}
          nodes={nodes}
          hidePatterns={hidePatterns}
          onToggleHide={toggleHide}
          onToggle={toggle}
          onSelectFile={selectFile}
        />
      )}
    </div>
  )
}

interface TreeChildrenProps {
  dir: string
  entries: FileEntry[]
  depth: number
  nodes: Record<string, NodeState>
  hidePatterns: string[]
  onToggleHide: (pattern: string) => void
  onToggle: (dir: string) => void
  onSelectFile: (path: string) => void
}

function TreeChildren({
  dir, entries, depth, nodes, hidePatterns, onToggleHide, onToggle, onSelectFile,
}: TreeChildrenProps) {
  // tether#71 — reveal is per-directory and NOT persisted: the pattern list is
  // the durable preference, this is just "let me look". One useState per
  // TreeChildren instance gives that for free, since React keeps an instance
  // per directory across re-renders.
  const [revealed, setRevealed] = useState(false)

  const { visible, hidden } = useMemo(
    () => partitionEntries(entries, hidePatterns),
    [entries, hidePatterns],
  )
  const siblings = useMemo(() => entries.map(e => e.name), [entries])

  // When revealed, render the ORIGINAL listing rather than visible-then-hidden,
  // so revealing does not also reshuffle the directory.
  const shown = revealed ? entries : visible

  return (
    <>
      {shown.map(entry => {
        const childPath = dir ? `${dir}/${entry.name}` : entry.name
        const by = hidingPattern(entry.name, hidePatterns)
        return (
          <TreeNode
            key={childPath}
            path={childPath}
            entry={entry}
            depth={depth}
            nodes={nodes}
            hidePatterns={hidePatterns}
            siblings={siblings}
            hiddenBy={by}
            onToggleHide={onToggleHide}
            onToggle={onToggle}
            onSelectFile={onSelectFile}
          />
        )
      })}
      {hidden.length > 0 && (
        <div
          className="tree-row tree-hidden-row"
          data-testid="tree-hidden-row"
          style={{ paddingLeft: 8 }}
          onClick={() => setRevealed(r => !r)}
          title={
            revealed
              ? `fold ${hidden.length} hidden ${hidden.length === 1 ? 'entry' : 'entries'} away again`
              : `show ${hidden.length} hidden ${hidden.length === 1 ? 'entry' : 'entries'}`
          }
        >
          {depth > 0 && <span className="ftree-indent" style={{ width: depth * 10 }} aria-hidden="true" />}
          <span className="tree-chevron" aria-hidden="true" />
          <span className="tree-label">
            {revealed ? `− ${hidden.length} hidden` : `+${hidden.length} hidden`}
          </span>
        </div>
      )}
    </>
  )
}

interface TreeNodeProps {
  path: string
  entry: FileEntry
  depth: number
  nodes: Record<string, NodeState>
  hidePatterns: string[]
  /** Every name in this directory, for deriving a family pattern. */
  siblings: string[]
  /** The pattern currently hiding this entry, or '' when it is shown normally. */
  hiddenBy: string
  onToggleHide: (pattern: string) => void
  onToggle: (dir: string) => void
  onSelectFile: (path: string) => void
}

function TreeNode({
  path, entry, depth, nodes, hidePatterns, siblings, hiddenBy, onToggleHide, onToggle, onSelectFile,
}: TreeNodeProps) {
  const node = nodes[path]
  const expanded = entry.isDir && !!node?.expanded

  // tether#71 — the pattern this row's button would act on. For a hidden row it
  // is whatever is hiding it (removing the NAME would leave a glob like `pf.*`
  // in place and the row would spring straight back); for a visible row it is
  // the family suggestion, so one click can clear a directory full of siblings
  // instead of asking for one click each.
  const suggested = useMemo(
    () => (hiddenBy ? hiddenBy : suggestHidePattern(entry.name, siblings)),
    [hiddenBy, entry.name, siblings],
  )
  // How many siblings that click would take with it — stated in the tooltip so
  // the blast radius of a one-click family hide is visible BEFORE the click.
  const covers = useMemo(
    () => (hiddenBy ? 0 : siblings.filter(s => matches(s, suggested)).length),
    [hiddenBy, siblings, suggested],
  )

  return (
    <>
      <div
        className={`tree-row${hiddenBy ? ' tree-row-hidden' : ''}`}
        style={{ paddingLeft: 8 }}
        onClick={() => entry.isDir ? onToggle(path) : onSelectFile(path)}
      >
        {depth > 0 && <span className="ftree-indent" style={{ width: depth * 10 }} aria-hidden="true" />}
        <span className="tree-chevron" aria-hidden="true">
          {entry.isDir && (
            <Icon name={expanded ? 'chev-down' : 'chevron'} size={11} style={{ color: 'var(--ink-quat)' }} />
          )}
        </span>
        <span className="file-glyph" aria-hidden="true">
          <Icon
            name={entry.isDir ? (expanded ? 'folder-open' : 'folder') : 'file'}
            size={13}
            style={{ color: 'var(--ink-quat)' }}
          />
        </span>
        <span className="tree-label" style={{ flex: 1 }}>{entry.name}</span>
        {entry.dirty && <span className="dirty-dot" data-testid="dirty-dot" />}
        <button
          type="button"
          className="tree-hide-btn"
          data-testid={hiddenBy ? 'tree-show-btn' : 'tree-hide-btn'}
          title={
            hiddenBy
              ? `show again — drops the "${hiddenBy}" rule`
              : covers > 1
                ? `hide "${suggested}" — ${covers} entries here`
                : `hide "${suggested}"`
          }
          aria-label={hiddenBy ? `show ${suggested}` : `hide ${suggested}`}
          onClick={e => { e.stopPropagation(); onToggleHide(suggested) }}
        >{/* A glyph, not the word: the button occupies its slot even at opacity
             0 (same as .ws-remove-btn), and "hide" would cost ~27px of label
             width on every row — working against the very thing this feature
             exists to fix. Neither glyph is an ✕: on a file row that would read
             as "delete", which this never does. The word is in the tooltip and
             the aria-label. */}
          {hiddenBy ? '↺' : '⊘'}</button>
      </div>
      {entry.isDir && expanded && node?.loading && (
        <div className="tree-row" style={{ paddingLeft: 8 + (depth + 1) * 14 }}>loading…</div>
      )}
      {entry.isDir && expanded && node?.error && (
        <div className="tree-row" style={{ paddingLeft: 8 + (depth + 1) * 14, color: 'var(--danger)' }}>{node.error}</div>
      )}
      {entry.isDir && expanded && node?.entries && (
        <TreeChildren
          dir={path}
          entries={node.entries}
          depth={depth + 1}
          nodes={nodes}
          hidePatterns={hidePatterns}
          onToggleHide={onToggleHide}
          onToggle={onToggle}
          onSelectFile={onSelectFile}
        />
      )}
    </>
  )
}
