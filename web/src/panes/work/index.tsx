// WorkPane — the Work main view. Owns the project selector (writes the shared
// store.workProject) and hosts the Work relationship map (WorkGraphView).
// Clicking a card selects a wi, which slides a DetailDrawer (WorkDetail: detail
// + step DAG + action bar) up from the bottom over the map; dismissing it clears
// the selection.
//
// It has moved twice: out of the middle canvas into a right-pane tab (tether#26,
// the middle map read as cluttered), and back into the middle column behind the
// left activity bar (tether#90, where it is a main-area surface rather than a
// fourth tab). The right pane is Chat / Skills / Shell again.
import { useEffect, useState } from 'react'
import { useStore } from '../../lib/store'
import { describeError, fetchProjects } from '../../lib/aihub'
import type { WorkProject } from '../../lib/wire.gen'
import WorkGraphView from './WorkGraphView'
import DetailDrawer from './DetailDrawer'

interface Props {
  /** Whether Work is the main view currently on screen. Gates the detail
   *  drawer's global Esc-to-close so a drawer left mounted behind another
   *  surface doesn't swallow Esc from Chat/Shell (tether#26 review F1).
   *  Still load-bearing after tether#90: App keeps this pane mounted behind
   *  Canvas rather than tearing its state down, so the drawer really can be
   *  alive and invisible. */
  active?: boolean
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
