import { useEffect, useRef, useState } from 'react'
import { Icon } from '../../lib/icons'
import { useStore } from '../../lib/store'
import WorkspaceTree from './WorkspaceTree'

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
  const [sessions, setSessions] = useState<string[]>([])
  const [sessionsOpen, setSessionsOpen] = useState(false)
  const currentSid = useStore(s => s.sessionId)
  const [filter, setFilter] = useState('')
  const filterRef = useRef<HTMLInputElement>(null)
  const [adding, setAdding] = useState(false)
  const [newPath, setNewPath] = useState('')
  const [newName, setNewName] = useState('')

  const loadSessions = async () => {
    try {
      const res = await fetch('/api/v1/sessions')
      if (res.ok) setSessions(await res.json() as string[])
    } catch { /* ignore */ }
  }

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

  useEffect(() => { void load(); void loadSessions() }, [])

  // ⌘P / Ctrl+P focuses the workspace filter.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'p') {
        e.preventDefault()
        filterRef.current?.focus()
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [])

  const addWorkspace = async () => {
    const path = newPath.trim()
    if (!path) return
    const name = newName.trim() || (path.split('/').pop() ?? path)
    try {
      const res = await fetch('/api/v1/workspaces', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name, path }),
      })
      if (!res.ok) throw new Error(`HTTP ${res.status}`)
      setNewPath('')
      setNewName('')
      setAdding(false)
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
        <button className="icon-btn-sm" onClick={() => setAdding(a => !a)} title="Add workspace" aria-label="Add workspace">
          <Icon name="plus" size={12} />
        </button>
      </div>

      {adding && (
        <div className="ws-add-form">
          <input
            className="skill-input"
            placeholder="workspace path"
            autoFocus
            value={newPath}
            onChange={e => setNewPath(e.target.value)}
            onKeyDown={e => { if (e.key === 'Enter') void addWorkspace(); if (e.key === 'Escape') setAdding(false) }}
          />
          <input
            className="skill-input"
            placeholder="name (optional)"
            value={newName}
            onChange={e => setNewName(e.target.value)}
            onKeyDown={e => { if (e.key === 'Enter') void addWorkspace(); if (e.key === 'Escape') setAdding(false) }}
          />
          <div style={{ display: 'flex', gap: 6 }}>
            <button className="btn-primary-sm" disabled={!newPath.trim()} onClick={() => void addWorkspace()}>Add</button>
            <button className="btn-ghost-sm" onClick={() => { setAdding(false); setNewPath(''); setNewName('') }}>Cancel</button>
          </div>
        </div>
      )}

      <div className="dt-search">
        <Icon name="search" size={12} style={{ color: 'var(--ink-quat)', flexShrink: 0 }} />
        <input
          ref={filterRef}
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
                title="Remove workspace"
                aria-label={`Remove workspace ${ws.name}`}
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
            {activeId === ws.id && <WorkspaceTree workspaceId={ws.id} />}
          </div>
        ))}
      </div>

      {/* Session history list */}
      {sessions.length > 0 && (
        <div style={{ borderTop: '1px solid var(--line-soft)', flexShrink: 0 }}>
          <div
            className="dt-left-head"
            style={{ cursor: 'pointer', paddingTop: 8, paddingBottom: 8 }}
            onClick={() => setSessionsOpen(o => !o)}
          >
            <span className="section-label">Sessions</span>
            <span style={{ fontSize: 10, color: 'var(--ink-quat)', fontFamily: 'var(--font-mono)' }}>
              {sessions.length}
            </span>
          </div>
          {sessionsOpen && (
            <div style={{ maxHeight: 180, overflow: 'auto' }} className="scroll-thin">
              {[...sessions].reverse().map(sid => (
                <div
                  key={sid}
                  className={`tree-row${sid === currentSid ? ' active' : ''}`}
                  style={{ paddingLeft: 12, fontSize: 11 }}
                  onClick={() => {
                    fetch(`/api/v1/sessions/${encodeURIComponent(sid)}/messages`)
                      .then(r => r.ok ? r.json() : [])
                      .then((msgs: Array<{ role: string; text: string; ts: number }>) => {
                        if (msgs.length > 0) {
                          useStore.getState().loadHistory(msgs.map(m => ({
                            id: crypto.randomUUID(),
                            role: m.role as 'user' | 'assistant',
                            text: m.text,
                            ts: m.ts,
                          })))
                          useStore.getState().setSessionId(sid)
                        }
                      })
                      .catch(() => {})
                  }}
                >
                  <span className="ws-dot" style={{ background: sid === currentSid ? 'var(--success)' : undefined }} />
                  <span className="tree-label" style={{ fontFamily: 'var(--font-mono)', fontSize: 10 }}>
                    {sid.slice(0, 16)}…
                  </span>
                </div>
              ))}
            </div>
          )}
        </div>
      )}

      <div className="dt-left-foot">
        {workspaces.length} workspace{workspaces.length !== 1 ? 's' : ''}
        {liveCount > 0 && ` · ${liveCount} live`}
      </div>
    </>
  )
}
