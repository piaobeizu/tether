import WorkspacePane from './panes/workspace'
import SkillPane from './panes/skill'
import ChatPane from './panes/chat'

// D-19 layout: three-pane (≥768px) / single-pane chat-first (≤600px).
// CSS in index.css handles the responsive collapse.
export default function App() {
  return (
    <div className="layout-desktop">
      <div className="pane pane-workspace">
        <WorkspacePane />
      </div>
      <div className="pane pane-chat">
        <ChatPane />
      </div>
      <div className="pane pane-skill">
        <SkillPane />
      </div>
    </div>
  )
}
