import { useEffect, useState } from 'react'
import { httpErrorMessage } from '../../lib/httpError'

interface Skill {
  id: string
  name: string
  description?: string
  sourcePath: string
  addedAt: string
}

interface Props {
  /** Open Settings on the skills sub-page (management lives there now). */
  onManage: () => void
}

// Right-pane Skills tab: a read-only glance at installed skills. Management
// (install / remove / enable) moved to the Settings "skills" sub-page — this
// tab points there via onManage. See .claude/claude-design.md freeze-rule 3.
export default function SkillPane({ onManage }: Props) {
  const [skills, setSkills] = useState<Skill[]>([])
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let alive = true
    const load = () => {
      fetch('/api/v1/skills')
        // tether#161 — the daemon's words, not its status code. This tab is a
        // SECOND reader of this endpoint (Settings owns the other), and it had
        // its own copy of the status-only throw; fixing one and not the other
        // would make the same refusal legible on one screen and opaque on this
        // one. `async` because the body has to be read before it can be shown.
        .then(async res => {
          if (!res.ok) throw new Error(await httpErrorMessage(res))
          return res.json()
        })
        .then((list: Skill[]) => { if (alive) { setSkills(list); setError(null) } })
        .catch(e => { if (alive) setError(e instanceof Error ? e.message : String(e)) })
    }
    load()
    // Refetch when skills are installed/removed from the Settings sub-page.
    window.addEventListener('tether:skills-changed', load)
    return () => { alive = false; window.removeEventListener('tether:skills-changed', load) }
  }, [])

  return (
    <>
      <div style={{ padding: '8px 12px', display: 'flex', justifyContent: 'space-between', alignItems: 'center', borderBottom: '1px solid var(--line-soft)', flexShrink: 0 }}>
        <span style={{ fontSize: 11, color: 'var(--ink-tertiary)', fontFamily: 'var(--font-mono)' }}>
          {skills.length} installed
        </span>
        <button onClick={onManage} className="btn-ghost-sm">Manage in Settings →</button>
      </div>
      <div className="pane-body">
        {error && <div style={{ color: 'var(--danger)', fontSize: 11, marginBottom: 8 }}>{error}</div>}
        {!error && skills.length === 0 && (
          <div style={{ color: 'var(--ink-secondary)', fontSize: 12 }}>
            No skills installed. <button onClick={onManage} className="btn-link">Install one →</button>
          </div>
        )}
        {skills.map(sk => (
          <div
            key={sk.id}
            style={{ marginBottom: 8, padding: '6px 8px', background: 'var(--bg-surface)', borderRadius: 4 }}
          >
            <div style={{ fontSize: 13, fontWeight: 500 }}>{sk.name}</div>
            {sk.description && (
              <div style={{ fontSize: 11, color: 'var(--ink-secondary)' }}>{sk.description}</div>
            )}
            <div style={{ fontSize: 10, color: 'var(--ink-tertiary)', fontFamily: 'var(--font-mono)', wordBreak: 'break-all' }}>{sk.sourcePath}</div>
          </div>
        ))}
      </div>
    </>
  )
}
