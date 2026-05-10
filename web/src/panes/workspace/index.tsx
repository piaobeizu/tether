import { useEffect, useRef, useState } from 'react'
import { connectEventsOnly, type Envelope } from '../../lib/wt'

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

  // Multi-tab attach state
  const [attachedSid, setAttachedSid] = useState<string | null>(null)
  const [attachedEvents, setAttachedEvents] = useState<string[]>([])
  const attachWtRef = useRef<WebTransport | null>(null)

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

  // Cleanup attach on unmount
  useEffect(() => {
    return () => {
      attachWtRef.current?.close()
    }
  }, [])

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

  const handleAttach = async (sid: string) => {
    // Close existing attach if any
    if (attachWtRef.current) {
      attachWtRef.current.close()
      attachWtRef.current = null
    }
    setAttachedSid(sid)
    setAttachedEvents([])
    try {
      const wt = await connectEventsOnly(
        sid,
        (env: Envelope) => setAttachedEvents(prev => [...prev.slice(-99), JSON.stringify(env)]),
        () => { setAttachedSid(null); setAttachedEvents([]); attachWtRef.current = null },
      )
      attachWtRef.current = wt
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
      setAttachedSid(null)
    }
  }

  const handleDetach = () => {
    attachWtRef.current?.close()
    attachWtRef.current = null
    setAttachedSid(null)
    setAttachedEvents([])
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
            <div style={{ display: 'flex', alignItems: 'center', gap: 4 }}>
              {ws.activeSid && (
                <button
                  onClick={() => handleAttach(ws.activeSid!)}
                  style={{ fontSize: 11, color: '#4ade80', background: 'transparent', border: '1px solid #333', borderRadius: 3, padding: '2px 6px', cursor: 'pointer' }}
                >
                  Attach
                </button>
              )}
              <button
                onClick={() => remove(ws.id)}
                style={{ background: 'none', border: 'none', color: '#666', cursor: 'pointer', fontSize: 14 }}
              >
                ×
              </button>
            </div>
          </div>
        ))}
        {attachedSid && (
          <div style={{ padding: 8, borderTop: '1px solid #222', fontSize: 12, marginTop: 8 }}>
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
              <span style={{ color: '#4ade80' }}>
                Attached: <span style={{ fontFamily: 'monospace' }}>{attachedSid.slice(0, 12)}…</span> [read-only]
              </span>
              <button
                onClick={handleDetach}
                style={{ fontSize: 11, color: '#888', background: 'none', border: 'none', cursor: 'pointer' }}
              >
                Detach
              </button>
            </div>
            <div style={{ maxHeight: 120, overflow: 'auto', color: '#aaa', marginTop: 4, fontFamily: 'monospace', fontSize: 10 }}>
              {attachedEvents.length === 0
                ? <span style={{ color: '#555' }}>Waiting for events…</span>
                : attachedEvents.map((e, i) => <div key={i}>{e}</div>)
              }
            </div>
          </div>
        )}
      </div>
    </>
  )
}
