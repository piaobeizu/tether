import { useMemo, useState } from 'react'
import type { FencedBlock } from '../lib/wire.gen'
import type { DagContent, DagNode, DagNodeStatus } from './types'
import { parseContent } from './types'

interface Props {
  block: FencedBlock
  expanded: boolean
  onToggle: () => void
}

// Auto-layout: place nodes on a serpentine grid so any node count renders
// without the design's hard-coded n1..n6 coordinates.
const COL_W = 160
const ROW_H = 120
const PER_ROW = 4
const NODE_W = 120
const NODE_H = 36

function layout(nodes: DagNode[]): Record<string, { x: number; y: number }> {
  const pos: Record<string, { x: number; y: number }> = {}
  nodes.forEach((n, i) => {
    const row = Math.floor(i / PER_ROW)
    const inRow = i % PER_ROW
    // even rows go left→right, odd rows right→left (serpentine) to keep edges short
    const col = row % 2 === 0 ? inRow : PER_ROW - 1 - inRow
    pos[n.id] = { x: 20 + col * COL_W, y: 20 + row * ROW_H }
  })
  return pos
}

function normStatus(s: string | undefined): DagNodeStatus {
  return s === 'done' || s === 'running' || s === 'failed' ? s : 'queued'
}

const STATUS_STROKE: Record<DagNodeStatus, string> = {
  done: 'var(--success)',
  running: 'var(--accent)',
  queued: 'var(--ink-quat)',
  failed: 'var(--danger)',
}

function fmtElapsed(ms: number): string {
  const s = Math.floor(ms / 1000)
  const m = Math.floor(s / 60)
  return `${String(m).padStart(2, '0')}:${String(s % 60).padStart(2, '0')}`
}

function BlockFallback({ label }: { label: string }) {
  return <div className="fb-fallback mono">{label}: unreadable block content</div>
}

export function DagBlock({ block, expanded, onToggle }: Props) {
  const data = useMemo(() => parseContent<DagContent>(block.content), [block.content])
  const nodes = useMemo<DagNode[]>(
    () => (Array.isArray(data?.nodes) ? data.nodes : []),
    [data]
  )
  // Selected node id for the full-view side panel; default to the running node.
  const runningId = nodes.find(n => normStatus(n.status) === 'running')?.id
  const [selected, setSelected] = useState<string | null>(null)

  if (!data || nodes.length === 0) return <BlockFallback label="dag" />

  const title = data.title ?? block.skill
  const doneCount = nodes.filter(n => normStatus(n.status) === 'done').length
  const running = nodes.find(n => normStatus(n.status) === 'running')

  // ── Compact strip ──────────────────────────────────────────
  if (!expanded) {
    return (
      <div className="fb fb-compact">
        <div className="fb-compact-head">
          <span className="fb-tag mono">DAG</span>
          <span className="fb-compact-title">{title}</span>
          <span className="fb-compact-count mono">{doneCount}/{nodes.length}</span>
        </div>
        <div className="dag-strip">
          {nodes.map((n, i) => {
            const st = normStatus(n.status)
            return (
              <div key={n.id} className="dag-strip-item">
                <span className={`dag-seg dag-seg-${st}`} title={n.label} />
                {i < nodes.length - 1 && <span className="dag-seg-gap" />}
              </div>
            )
          })}
        </div>
        <div className="fb-compact-foot">
          <span className="mono">{running ? 'running →' : 'complete →'}</span>
          &nbsp;<span className="fb-compact-cur">{running?.label ?? nodes[nodes.length - 1].label}</span>
          <button type="button" className="fb-expand" onClick={onToggle}>expand →</button>
        </div>
      </div>
    )
  }

  // ── Full graph + side panel ────────────────────────────────
  const pos = layout(nodes)
  const edges: [string, string][] = Array.isArray(data.edges) && data.edges.length > 0
    ? data.edges
    // fall back to a simple linear chain if the skill omitted edges
    : nodes.slice(1).map((n, i) => [nodes[i].id, n.id] as [string, string])
  const rows = Math.ceil(nodes.length / PER_ROW)
  const vbW = 40 + PER_ROW * COL_W
  const vbH = 40 + rows * ROW_H
  const selNode = nodes.find(n => n.id === (selected ?? runningId)) ?? nodes[0]

  return (
    <div className="fb fb-full">
      <header className="fb-header">
        <span className="fb-kind mono">dag</span>
        <span className="fb-title">{title}</span>
        <span className="pill fb-header-pill">
          <span className="dot" style={{ background: data.paused ? 'var(--ink-quat)' : 'var(--accent)' }} />
          {doneCount} / {nodes.length}{data.paused ? ' · paused' : ''}
        </span>
      </header>

      <div className="dag-body">
        <div className="dag-canvas">
          <svg viewBox={`0 0 ${vbW} ${vbH}`} className="dag-svg" role="img" aria-label={`${title} graph`}>
            {edges.map(([a, b], i) => {
              const A = pos[a], B = pos[b]
              if (!A || !B) return null
              const aN = nodes.find(n => n.id === a)
              const bN = nodes.find(n => n.id === b)
              const isActive = normStatus(aN?.status) === 'done' && normStatus(bN?.status) === 'running'
              return (
                <path
                  key={i}
                  d={`M ${A.x + NODE_W} ${A.y + NODE_H / 2} C ${A.x + NODE_W + 40} ${A.y + NODE_H / 2}, ${B.x - 40} ${B.y + NODE_H / 2}, ${B.x} ${B.y + NODE_H / 2}`}
                  fill="none"
                  stroke={isActive ? 'var(--accent)' : 'var(--line)'}
                  strokeWidth={isActive ? 2 : 1.3}
                  strokeDasharray={isActive ? '4 4' : undefined}
                />
              )
            })}
            {nodes.map(n => {
              const p = pos[n.id]
              const st = normStatus(n.status)
              const isSel = n.id === (selected ?? runningId)
              const fill = st === 'running' ? 'var(--accent-tint)'
                : st === 'done' ? 'var(--success-tint)'
                : 'var(--bg-elevated)'
              return (
                <g
                  key={n.id}
                  transform={`translate(${p.x},${p.y})`}
                  className="dag-node"
                  onClick={() => setSelected(n.id)}
                >
                  <rect
                    width={NODE_W} height={NODE_H} rx={6}
                    fill={fill}
                    stroke={isSel ? 'var(--ink-primary)' : STATUS_STROKE[st]}
                    strokeWidth={isSel ? 2 : 1.2}
                  />
                  <circle cx={12} cy={NODE_H / 2} r={4} fill={STATUS_STROKE[st]} />
                  <text x={24} y={NODE_H / 2 + 4} className="dag-node-label">{n.label}</text>
                </g>
              )
            })}
          </svg>
        </div>

        <aside className="dag-side">
          <div className="fb-side-eyebrow mono">NODE · {selNode.id}</div>
          <div className="fb-side-title">{selNode.label}</div>
          <div className="fb-side-rows">
            <div className="fb-side-row">
              <span className="mono fb-side-k">status</span>
              <span className="mono fb-side-v">
                {normStatus(selNode.status) === 'running' && <span className="dot-anim" />}
                {normStatus(selNode.status)}
              </span>
            </div>
            <div className="fb-side-row">
              <span className="mono fb-side-k">elapsed</span>
              <span className="mono fb-side-v">{fmtElapsed(data.elapsedMs ?? 0)}</span>
            </div>
            {typeof selNode.ms === 'number' && (
              <div className="fb-side-row">
                <span className="mono fb-side-k">duration</span>
                <span className="mono fb-side-v">{selNode.ms}ms</span>
              </div>
            )}
          </div>
          {/* No daemon action API exists yet (D-19 action callbacks are a
              future step): render the controls disabled with a TODO title so
              the affordance is visible without inventing a backend call. */}
          <div className="fb-side-actions">
            <button type="button" className="btn-ghost-sm" disabled title="action callbacks not wired yet">
              {data.paused ? '▶ resume' : '❚❚ pause'}
            </button>
            <button type="button" className="btn-ghost-sm" disabled title="action callbacks not wired yet">↺ rollback</button>
            <button type="button" className="btn-ghost-sm" disabled title="action callbacks not wired yet">✓ approve</button>
          </div>
        </aside>
      </div>

      <footer className="fb-footer">
        <span className="fb-footer-note mono">{running ? `running · ${running.label}` : 'run complete'}</span>
        <button type="button" className="fb-expand" onClick={onToggle}>collapse ↑</button>
      </footer>
    </div>
  )
}
