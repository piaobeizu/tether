import { useState } from 'react'
import WorkspacePane from './panes/workspace'
import SkillPane from './panes/skill'
import ShellPane from './panes/shell'
import ChatPane from './panes/chat'

// D-19 layout: three-pane (≥768px) / single-pane chat-first (≤600px).
// CSS in index.css handles the responsive collapse.
export default function App() {
  const [rightTab, setRightTab] = useState<'skill' | 'shell'>('skill')

  const tabBtnStyle = (active: boolean): React.CSSProperties => ({
    background: active ? '#2a2a2a' : 'transparent',
    border: 'none',
    color: active ? '#e8e8e8' : '#888',
    padding: '4px 12px',
    cursor: active ? 'default' : 'pointer',
    fontSize: 12,
    borderRadius: 3,
  })

  return (
    <div className="layout-desktop">
      <div className="pane pane-workspace">
        <WorkspacePane />
      </div>
      <div className="pane pane-chat">
        <ChatPane />
      </div>
      <div className="pane pane-skill" style={{ display: 'flex', flexDirection: 'column' }}>
        <div style={{ display: 'flex', gap: 4, padding: '4px 8px', borderBottom: '1px solid #222', flexShrink: 0 }}>
          <button onClick={() => setRightTab('skill')} style={tabBtnStyle(rightTab === 'skill')}>Skill</button>
          <button onClick={() => setRightTab('shell')} style={tabBtnStyle(rightTab === 'shell')}>Shell</button>
        </div>
        <div style={{ flex: 1, minHeight: 0, display: 'flex', flexDirection: 'column' }}>
          {rightTab === 'skill' ? <SkillPane /> : <ShellPane />}
        </div>
      </div>
    </div>
  )
}
