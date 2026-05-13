import { useEffect, useState } from 'react'
import { Icon } from '../../lib/icons'

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
  const [activeId, setActiveId] = useState<string | null>(null)
  const [filter, setFilter] = useState('')

  const load = async () => {
    try {
      const res = await fetch('/api/v1/workspaces')
      if (!res.ok) throw new Error(`HTTP ${res.status}`)
      const data = await res.json() as Workspace[]
      setWorkspaces(data)
      setError(null)
      if (data.length > 0 && !activeId) setActiveId(data[0].id)
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

  const remove = async (id: string, e: React.MouseEvent) => {
    e.stopPropagation()
    await fetch(`/api/v1/workspaces/${id}`, { method: 'DELETE' })
    if (activeId === id) setActiveId(null)
    await load()
  }

  const filtered = workspaces.filter(ws =>
    !filter ||
    ws.name.toLowerCase().includes(filter.toLowerCase()) ||
    ws.path.toLowerCase().includes(filter.toLowerCase())
  )

  const liveCount = workspaces.filter(ws => ws.activeSid).length

  return (
    <>
      <div className="dt-left-head">
        <span className="section-label">Workspaces</span>
        <button className="icon-btn-sm" onClick={addWorkspace} title="Add workspace">
          <Icon name="plus" size={12} />
        </button>
      </div>

      <div className="dt-search">
        <Icon name="search" size={12} style={{ color: 'var(--ink-quat)', flexShrink: 0 }} />
        <input
          value={filter}
          onChange={e => setFilter(e.target.value)}
          placeholder="filter…"
        />
        <span className="kbd">⌘P</span>
      </div>

      <div className="dt-tree scroll-thin">
        {error && (
          <div style={{ padding: '8px 12px', color: 'var(--danger)', fontSize: 11 }}>{error}</div>
        )}
        {filtered.length === 0 && !error && (
          <div style={{ padding: '8px 12px', color: 'var(--ink-quat)', fontSize: 12, fontFamily: 'var(--font-mono)' }}>
            {filter ? 'no matches' : 'no workspaces'}
          </div>
        )}
        {filtered.map(ws => (
          <div key={ws.id}>
            <div
              className={`tree-row${activeId === ws.id ? ' active' : ''}`}
              style={{ paddingLeft: 8 }}
              onClick={() => setActiveId(activeId === ws.id ? null : ws.id)}
            >
              <Icon
                name={activeId === ws.id ? 'chev-down' : 'chevron'}
                size={11}
                style={{ color: 'var(--ink-quat)', flexShrink: 0 }}
              />
              <span className={`ws-dot${ws.activeSid ? ' live' : ''}`} />
              <span className="tree-label" style={{ fontWeight: 600, flex: 1 }}>{ws.name}</span>
              <button
                onClick={e => remove(ws.id, e)}
                className="ws-remove-btn"
                title="Remove"
                style={{ background: 'none', border: 'none', color: 'var(--ink-quat)', cursor: 'pointer', padding: '0 2px', fontSize: 13, lineHeight: 1, flexShrink: 0, opacity: 0 }}
              >×</button>
            </div>
            {activeId === ws.id && (
              <div style={{ paddingLeft: 32, paddingRight: 10, paddingTop: 2, paddingBottom: 6 }}>
                <div style={{ fontFamily: 'var(--font-mono)', fontSize: 10.5, color: 'var(--ink-quat)', whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>
                  {ws.path}
                </div>
                {ws.activeSid && (
                  <div style={{ fontFamily: 'var(--font-mono)', fontSize: 10.5, color: 'var(--success)', marginTop: 2 }}>
                    {ws.activeSid.slice(0, 12)}…
                  </div>
                )}
              </div>
            )}
          </div>
        ))}
      </div>

      <div className="dt-left-foot">
        {workspaces.length} workspace{workspaces.length !== 1 ? 's' : ''}
        {liveCount > 0 && ` · ${liveCount} live`}
      </div>

      <style>{`.ws-remove-btn { transition: opacity .1s } .tree-row:hover .ws-remove-btn { opacity: 1 !important }`}</style>
    </>
  )
}
