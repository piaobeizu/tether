import { useMemo, useState } from 'react'
import type { FencedBlock } from '../lib/wire.gen'
import type { Candidate, CandidatesContent } from './types'
import { parseContent } from './types'

interface Props {
  block: FencedBlock
  expanded: boolean
  onToggle: () => void
}

const COMPACT_LIMIT = 3

function BlockFallback({ label }: { label: string }) {
  return <div className="fb-fallback mono">{label}: unreadable block content</div>
}

export function CandidatesBlock({ block, expanded, onToggle }: Props) {
  const data = useMemo(() => parseContent<CandidatesContent>(block.content), [block.content])
  const candidates = useMemo<Candidate[]>(
    () => (Array.isArray(data?.candidates) ? data.candidates : []),
    [data]
  )
  // Selection is local for now — no daemon submit API yet.
  const [picked, setPicked] = useState<string[]>(() =>
    Array.isArray(data?.picked) ? data!.picked!.filter(id => candidates.some(c => c.id === id)) : []
  )
  const [filter, setFilter] = useState('')

  if (!data || candidates.length === 0) return <BlockFallback label="candidates" />

  const title = data.title ?? block.skill
  const toggle = (id: string) =>
    setPicked(s => (s.includes(id) ? s.filter(x => x !== id) : [...s, id]))

  // ── Compact: first N as tappable cards + view-all ──────────
  if (!expanded) {
    return (
      <div className="fb fb-compact">
        <div className="fb-compact-head">
          <span className="fb-tag mono">CANDIDATES</span>
          <span className="fb-compact-title">{title}</span>
          <span className="fb-compact-count">{picked.length} picked</span>
        </div>
        <div className="cand-compact-list">
          {candidates.slice(0, COMPACT_LIMIT).map(c => {
            const on = picked.includes(c.id)
            return (
              <div
                key={c.id}
                className={`cand-compact-item${on ? ' on' : ''}`}
                onClick={() => toggle(c.id)}
              >
                <span className="cand-compact-title">{c.title}</span>
                {c.tag && <span className="mono cand-tag">{c.tag}</span>}
              </div>
            )
          })}
          {candidates.length > COMPACT_LIMIT && (
            <button type="button" className="cand-viewall" onClick={onToggle}>
              view all {candidates.length} →
            </button>
          )}
          {candidates.length <= COMPACT_LIMIT && (
            <button type="button" className="cand-viewall" onClick={onToggle}>expand →</button>
          )}
        </div>
      </div>
    )
  }

  // ── Full: filterable checkbox list + selection footer ──────
  const filtered = candidates.filter(c => {
    if (!filter) return true
    const q = filter.toLowerCase()
    return c.title.toLowerCase().includes(q) || (c.tag?.toLowerCase().includes(q) ?? false)
  })

  return (
    <div className="fb fb-full">
      <header className="fb-header">
        <span className="fb-kind mono">candidates</span>
        <span className="fb-title">{title}</span>
        <input
          className="fb-input mono cand-filter"
          placeholder="filter…"
          value={filter}
          onChange={e => setFilter(e.target.value)}
        />
      </header>

      <ul className="cand-list">
        {filtered.map((c, i) => {
          const on = picked.includes(c.id)
          return (
            <li
              key={c.id}
              className={`cand-row${on ? ' on' : ''}${i < filtered.length - 1 ? ' bordered' : ''}`}
              onClick={() => toggle(c.id)}
            >
              <span className={`cand-check${on ? ' on' : ''}`} aria-hidden="true">
                {on && '✓'}
              </span>
              <div className="cand-row-body">
                <div className="cand-row-head">
                  <span className="cand-row-title">{c.title}</span>
                  {c.tag && <span className="mono cand-tag">· {c.tag}</span>}
                </div>
                {c.desc && <div className="cand-row-desc">{c.desc}</div>}
              </div>
              <span className="mono cand-id">{c.id}</span>
            </li>
          )
        })}
        {filtered.length === 0 && <li className="cand-empty">no matches</li>}
      </ul>

      <footer className="fb-footer">
        <span className="fb-footer-note">{picked.length} of {candidates.length} selected</span>
        <div className="fb-footer-actions">
          <button type="button" className="btn-ghost-sm" onClick={onToggle}>collapse</button>
          <button type="button" className="btn-primary-sm" disabled title="submit not wired yet">
            submit selection
          </button>
        </div>
      </footer>
    </div>
  )
}
