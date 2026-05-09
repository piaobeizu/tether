import { useEffect, useState } from 'react'

interface Skill {
  id: string
  name: string
  description?: string
  sourcePath: string
  addedAt: string
}

export default function SkillPane() {
  const [skills, setSkills] = useState<Skill[]>([])
  const [error, setError] = useState<string | null>(null)

  const load = async () => {
    try {
      const res = await fetch('/api/v1/skills')
      if (!res.ok) throw new Error(`HTTP ${res.status}`)
      setSkills(await res.json())
      setError(null)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }

  useEffect(() => { void load() }, [])

  const install = async () => {
    const sourcePath = prompt('Skill source path:')
    if (!sourcePath) return
    const name = prompt('Skill name:', sourcePath.split('/').pop() ?? sourcePath) ?? sourcePath
    try {
      const res = await fetch('/api/v1/skills', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name, sourcePath }),
      })
      if (!res.ok) throw new Error(`HTTP ${res.status}`)
      await load()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }

  const remove = async (id: string) => {
    await fetch(`/api/v1/skills/${id}`, { method: 'DELETE' })
    await load()
  }

  return (
    <>
      <div className="pane-header" style={{ display: 'flex', justifyContent: 'space-between' }}>
        <span>Skills</span>
        <button
          onClick={install}
          style={{ background: 'none', border: '1px solid #444', borderRadius: 4, padding: '2px 8px', color: '#aaa', cursor: 'pointer', fontSize: 11 }}
        >
          + Install
        </button>
      </div>
      <div className="pane-body">
        {error && <div style={{ color: '#e57373', fontSize: 11, marginBottom: 8 }}>{error}</div>}
        {skills.length === 0 && (
          <div style={{ color: '#555', fontSize: 12 }}>No skills installed.</div>
        )}
        {skills.map((sk) => (
          <div
            key={sk.id}
            style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 8, padding: '6px 8px', background: '#1a1a1a', borderRadius: 4 }}
          >
            <div>
              <div style={{ fontSize: 13, fontWeight: 500 }}>{sk.name}</div>
              <div style={{ fontSize: 10, color: '#666', fontFamily: 'monospace' }}>{sk.sourcePath}</div>
            </div>
            <button
              onClick={() => remove(sk.id)}
              style={{ background: 'none', border: 'none', color: '#666', cursor: 'pointer', fontSize: 14 }}
            >
              ×
            </button>
          </div>
        ))}
      </div>
    </>
  )
}
