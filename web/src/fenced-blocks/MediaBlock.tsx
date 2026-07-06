import { useMemo } from 'react'
import type { FencedBlock } from '../lib/wire.gen'
import type { MediaContent } from './types'
import { parseContent } from './types'

interface Props {
  block: FencedBlock
  expanded: boolean
  onToggle: () => void
}

function BlockFallback({ label }: { label: string }) {
  return <div className="fb-fallback mono">{label}: unreadable block content</div>
}

// meta chip like "1.4 MB · png"
function metaLine(data: MediaContent): string {
  return [data.size, data.format].filter(Boolean).join(' · ')
}

export function MediaBlock({ block, expanded, onToggle }: Props) {
  const data = useMemo(() => parseContent<MediaContent>(block.content), [block.content])

  if (!data) return <BlockFallback label="media" />

  const title = data.title ?? block.skill
  const meta = metaLine(data)

  // ── Compact: thumbnail + title + meta ──────────────────────
  if (!expanded) {
    return (
      <div
        className="fb fb-compact media-compact"
        onClick={onToggle}
        onKeyDown={e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); onToggle() } }}
        role="button"
        tabIndex={0}
        aria-label={`Expand ${title}`}
      >
        <div className="media-thumb">
          {data.url
            ? <img src={data.url} alt={title} />
            : <span className="media-thumb-ph mono" aria-hidden="true">IMG</span>}
        </div>
        <div className="media-compact-body">
          <div className="media-compact-title">{title}</div>
          <div className="mono media-compact-meta">
            {[data.format, data.size, 'expand'].filter(Boolean).join(' · ')}
          </div>
        </div>
      </div>
    )
  }

  // ── Full: 16:9 preview + metadata footer ───────────────────
  return (
    <div className="fb fb-full">
      <header className="fb-header">
        <span className="fb-kind mono">media</span>
        <span className="fb-title">{title}</span>
        {meta && <span className="pill fb-header-pill">{meta}</span>}
      </header>

      <div className="media-preview">
        {data.url
          ? <img src={data.url} alt={title} className="media-preview-img" />
          : (
            <div className="media-preview-ph">
              <div className="media-preview-frame">
                <div className="media-frame-bar">
                  <span /><span /><span />
                </div>
                <div className="media-frame-body">
                  <span className="media-frame-line" style={{ width: '60%' }} />
                  <span className="media-frame-line" style={{ width: '84%' }} />
                  <span className="media-frame-line" style={{ width: '40%' }} />
                </div>
              </div>
            </div>
          )}
      </div>

      <footer className="fb-footer media-footer">
        {data.capturedAt && <span className="mono media-time">{data.capturedAt}</span>}
        <button type="button" className="btn-ghost-sm media-footer-open" onClick={onToggle}>collapse ↑</button>
        {data.path && (
          <button
            type="button"
            className="btn-ghost-sm"
            onClick={() => { void navigator.clipboard?.writeText(data.path!) }}
          >copy path</button>
        )}
      </footer>
    </div>
  )
}
