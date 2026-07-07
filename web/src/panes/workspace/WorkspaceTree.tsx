import { useEffect, useMemo, useState } from 'react'
import { Icon } from '../../lib/icons'
import { createFileTreeCache, type FileEntry } from './fileTreeCache'

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
        <TreeChildren dir="" entries={rootNode.entries} depth={0} nodes={nodes} onToggle={toggle} />
      )}
    </div>
  )
}

interface TreeChildrenProps {
  dir: string
  entries: FileEntry[]
  depth: number
  nodes: Record<string, NodeState>
  onToggle: (dir: string) => void
}

function TreeChildren({ dir, entries, depth, nodes, onToggle }: TreeChildrenProps) {
  return (
    <>
      {entries.map(entry => {
        const childPath = dir ? `${dir}/${entry.name}` : entry.name
        return (
          <TreeNode
            key={childPath}
            path={childPath}
            entry={entry}
            depth={depth}
            nodes={nodes}
            onToggle={onToggle}
          />
        )
      })}
    </>
  )
}

interface TreeNodeProps {
  path: string
  entry: FileEntry
  depth: number
  nodes: Record<string, NodeState>
  onToggle: (dir: string) => void
}

function TreeNode({ path, entry, depth, nodes, onToggle }: TreeNodeProps) {
  const node = nodes[path]
  const expanded = entry.isDir && !!node?.expanded

  return (
    <>
      <div
        className="tree-row"
        style={{ paddingLeft: 8 }}
        onClick={() => entry.isDir && onToggle(path)}
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
      </div>
      {entry.isDir && expanded && node?.loading && (
        <div className="tree-row" style={{ paddingLeft: 8 + (depth + 1) * 14 }}>loading…</div>
      )}
      {entry.isDir && expanded && node?.error && (
        <div className="tree-row" style={{ paddingLeft: 8 + (depth + 1) * 14, color: 'var(--danger)' }}>{node.error}</div>
      )}
      {entry.isDir && expanded && node?.entries && (
        <TreeChildren dir={path} entries={node.entries} depth={depth + 1} nodes={nodes} onToggle={onToggle} />
      )}
    </>
  )
}
