import { useMemo, useState } from 'react'
import type { FencedBlock } from '../lib/wire.gen'
import type { FormContent, FormField } from './types'
import { parseContent } from './types'

interface Props {
  block: FencedBlock
  expanded: boolean
  onToggle: () => void
}

type FieldValue = string | boolean

function initialValues(fields: FormField[]): Record<string, FieldValue> {
  const out: Record<string, FieldValue> = {}
  for (const f of fields) {
    if (f.type === 'switch') out[f.key] = typeof f.value === 'boolean' ? f.value : false
    else out[f.key] = typeof f.value === 'string' ? f.value : ''
  }
  return out
}

function BlockFallback({ label }: { label: string }) {
  return <div className="fb-fallback mono">{label}: unreadable block content</div>
}

export function FormBlock({ block, expanded, onToggle }: Props) {
  const data = useMemo(() => parseContent<FormContent>(block.content), [block.content])
  const fields = useMemo<FormField[]>(
    () => (Array.isArray(data?.fields) ? data.fields : []),
    [data]
  )
  // Local-only edits for now — no daemon submit API exists yet.
  const [values, setValues] = useState<Record<string, FieldValue>>(() => initialValues(fields))

  if (!data || fields.length === 0) return <BlockFallback label="form" />

  const title = data.title ?? block.skill
  const setVal = (key: string, v: FieldValue) => setValues(s => ({ ...s, [key]: v }))
  const firstText = fields.find(f => !f.type || f.type === 'text')

  // ── Compact: title + first text field + continue/expand ────
  if (!expanded) {
    return (
      <div className="fb fb-compact">
        <div className="fb-compact-head">
          <span className="fb-tag mono">FORM</span>
          <span className="fb-compact-title">{title}</span>
          {data.step && <span className="fb-compact-count mono">{data.step}</span>}
        </div>
        <div className="form-compact-body">
          {firstText && (
            <input
              className="fb-input mono"
              placeholder={firstText.label}
              value={String(values[firstText.key] ?? '')}
              onChange={e => setVal(firstText.key, e.target.value)}
            />
          )}
          <div className="form-compact-actions">
            <button type="button" className="btn-primary-sm" style={{ flex: 1 }} disabled title="submit not wired yet">
              {data.submitLabel ?? 'continue'} →
            </button>
            <button type="button" className="btn-ghost-sm" onClick={onToggle}>expand</button>
          </div>
        </div>
      </div>
    )
  }

  // ── Full field grid ────────────────────────────────────────
  return (
    <div className="fb fb-full">
      <header className="fb-header">
        <span className="fb-kind mono">form</span>
        <span className="fb-title">{title}</span>
        {data.step && <span className="fb-header-step mono">{data.step}</span>}
      </header>

      <div className="form-grid">
        {fields.map(f => (
          <div key={f.key} className="form-field">
            <div className="form-field-label">
              <div className="mono form-field-name">{f.label}</div>
              {f.hint && <div className="form-field-hint">{f.hint}</div>}
            </div>
            <div className="form-field-control">
              {f.type === 'segmented' ? (
                <div className="form-seg-group">
                  {(f.options ?? []).map(opt => (
                    <button
                      key={opt}
                      type="button"
                      className={`form-seg${values[f.key] === opt ? ' on' : ''}`}
                      onClick={() => setVal(f.key, opt)}
                    >{opt}</button>
                  ))}
                </div>
              ) : f.type === 'switch' ? (
                <label className="fb-switch">
                  <input
                    type="checkbox"
                    checked={Boolean(values[f.key])}
                    onChange={e => setVal(f.key, e.target.checked)}
                  />
                  <span />
                </label>
              ) : (
                <input
                  className="fb-input mono"
                  value={String(values[f.key] ?? '')}
                  onChange={e => setVal(f.key, e.target.value)}
                />
              )}
            </div>
          </div>
        ))}
      </div>

      <footer className="fb-footer">
        <span className="fb-footer-note">
          <span className="kbd">⌘</span> <span className="kbd">↵</span> to submit
        </span>
        <div className="fb-footer-actions">
          <button type="button" className="btn-ghost-sm" onClick={onToggle}>collapse</button>
          <button type="button" className="btn-primary-sm" disabled title="submit not wired yet">
            {data.submitLabel ?? 'apply'}
          </button>
        </div>
      </footer>
    </div>
  )
}
