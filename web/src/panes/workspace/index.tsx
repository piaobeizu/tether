import { useEffect, useState } from 'react'

interface Workspace {
  id: string
  name: string
  path: string
  addedAt: string
  activeSid?: string
}

export default function WorkspacePane() {
  const [workspaces, setWorkspaces] = useState<Workspace[]>([])
  const [error, setError] = useState<string | null>(null)

  const load = async () => {
    try {
      const res = await fetch('/api/v1/workspaces')
      if (!res.ok) throw new Error(`HTTP ${res.status}`)
      setWorkspaces(await res.json())
      setError(null)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }

  useEffect(() => { void load() }, [])

  const addWorkspace = async () => {
    const path = prompt('Workspace path:')
    if (!path) return
    const name = prompt('Workspace name:', path.split('/').pop() ?? path) ?? path
    try {
      const res = await fetch('/api/v1/workspaces', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name, path }),
      })
      if (!res.ok) throw new Error(`HTTP ${res.status}`)
      await load()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }

  const remove = async (id: string) => {
    await fetch(`/api/v1/workspaces/${id}`, { method: 'DELETE' })
    await load()
  }

  return (
    <>
      <div className="pane-header" style={{ display: 'flex', justifyContent: 'space-between' }}>
        <span>Workspaces</span>
        <button
          onClick={addWorkspace}
          style={{ background: 'none', border: '1px solid #444', borderRadius: 4, padding: '2px 8px', color: '#aaa', cursor: 'pointer', fontSize: 11 }}
        >
          + Add
        </button>
      </div>
      <div className="pane-body">
        {error && <div style={{ color: '#e57373', fontSize: 11, marginBottom: 8 }}>{error}</div>}
        {workspaces.length === 0 && (
          <div style={{ color: '#555', fontSize: 12 }}>No workspaces registered.</div>
        )}
        {workspaces.map((ws) => (
          <div
            key={ws.id}
            style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 8, padding: '6px 8px', background: '#1a1a1a', borderRadius: 4 }}
          >
            <div>
              <div style={{ fontSize: 13, fontWeight: 500 }}>{ws.name}</div>
              <div style={{ fontSize: 10, color: '#666', fontFamily: 'monospace' }}>{ws.path}</div>
            </div>
            <button
              onClick={() => remove(ws.id)}
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
