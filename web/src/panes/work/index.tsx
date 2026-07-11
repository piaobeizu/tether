// WorkPane — right-pane Work tab. Owns the project selector (writes the shared
// store.workProject) and hosts the Work relationship map (WorkGraphView) that
// moved here from the middle canvas in tether#26. Clicking a card selects a wi,
// which slides a DetailDrawer (WorkDetail: detail + step DAG + action bar) up
// from the bottom over the map; dismissing it clears the selection.
import { useEffect, useState } from 'react'
import { useStore } from '../../lib/store'
import { AihubError, fetchProjects } from '../../lib/aihub'
import type { WorkProject } from '../../lib/wire.gen'
import WorkGraphView from './WorkGraphView'
import DetailDrawer from './DetailDrawer'

interface Props {
  /** Whether the Work tab is the active right-pane tab. Gates the detail
   *  drawer's global Esc-to-close so a drawer left mounted behind another tab
   *  doesn't swallow Esc from Chat/Shell (tether#26 review F1). */
  active?: boolean
}

function describeError(e: unknown): string {
  if (e instanceof AihubError) {
    if (e.status === 503) return 'aihub not configured'
    if (e.status === 403) return 'not authorized for this project'
    return `error (HTTP ${e.status})`
  }
  return e instanceof Error ? e.message : String(e)
}

export default function WorkPane({ active }: Props) {
  const [projects, setProjects] = useState<WorkProject[]>([])
  const [projectsError, setProjectsError] = useState<string | null>(null)

  const project = useStore((s) => s.workProject)
  const setWorkProject = useStore((s) => s.setWorkProject)
  const selectedWiId = useStore((s) => s.selectedWiId)
  const select = useStore((s) => s.select)

  // Load projects once; seed the shared workProject if nothing is picked yet.
  useEffect(() => {
    let alive = true
    fetchProjects()
      .then((ps) => {
        if (!alive) return
        setProjects(ps)
        setProjectsError(null)
        if (!useStore.getState().workProject && ps[0]) setWorkProject(ps[0].name)
      })
      .catch((e) => { if (alive) setProjectsError(describeError(e)) })
    return () => { alive = false }
  }, [setWorkProject])

  const onProjectChange = (p: string) => {
    setWorkProject(p)
    // clear only the wi drawer (its selection belongs to the old project's map);
    // the middle file is workspace-scoped, unrelated to the Work project (tether#28).
    select({ wiId: null })
  }

  return (
    <div className="work-pane">
      <div className="work-head">
        <select
          className="work-project-select"
          value={project}
          onChange={(e) => onProjectChange(e.target.value)}
          disabled={projects.length === 0}
        >
          {(projects.length === 0 || project === '') && <option value="">no projects</option>}
          {projects.map((p) => <option key={p.name} value={p.name}>{p.name}</option>)}
        </select>
      </div>

      <div className="work-body">
        {projectsError && <div className="work-error">{projectsError}</div>}
        <WorkGraphView />
        {selectedWiId && (
          <DetailDrawer id={selectedWiId} onClose={() => select({ wiId: null })} escActive={active !== false} />
        )}
      </div>
    </div>
  )
}
