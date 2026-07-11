// WorkPane — right-pane Work tab (tether#23 panel inversion). Owns the
// project selector (writes the shared store.workProject that the middle
// WorkGraphView renders) and shows the selected wi's detail + scenario step
// DAG + click-to-work action bar (WorkDetail). The relationship
// knowledge-graph itself moved to the middle canvas (WorkGraphView); this
// pane no longer fetches or renders the graph.
import { useEffect, useState } from 'react'
import { useStore } from '../../lib/store'
import { AihubError, fetchProjects } from '../../lib/aihub'
import type { WorkProject } from '../../lib/wire.gen'
import WorkDetail from './WorkDetail'

interface Props {
  /** Whether the Work tab is the active right-pane tab. Unused now that the
   *  graph (and its polling) live in the middle WorkGraphView; kept for the
   *  App tab-mount contract. */
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

export default function WorkPane(_props: Props) {
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
    select(null) // don't carry the previous project's selection into the new map
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

      <div className="work-body scroll-thin">
        {projectsError && <div className="work-error">{projectsError}</div>}
        {selectedWiId
          ? <WorkDetail id={selectedWiId} />
          : <div className="work-empty work-detail-hint">从中间地图点一个 wi，查看详情与步骤</div>}
      </div>
    </div>
  )
}
