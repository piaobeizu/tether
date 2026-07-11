package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/piaobeizu/tether/internal/aihub"
	"github.com/piaobeizu/tether/internal/scenario"
	"github.com/piaobeizu/tether/internal/wire"
)

// Default paging values for the curated aihub proxy (Task A2). Callers may
// override via ?max= (queue) or ?limit= (events).
const (
	defaultQueueMax    = 10
	defaultEventsLimit = 50
	defaultRecentLimit = 20
)

// stepsEventsLimit bounds the event page fetched to compute step-completion
// status for the /steps endpoint (Task 5): generous enough to cover a whole
// scenario run's step_completed events without paging.
const stepsEventsLimit = 200

// recentStatuses is the default status filter for the done/recent view:
// terminal work items (wrapped + cancelled + failed).
var recentStatuses = []string{"wrapped", "cancelled", "failed"}

// graphStatuses is the status filter for the work graph view: active plus
// recently terminal work items, so the graph reflects the current state of
// the tree (tether#20 Work view).
var graphStatuses = []string{"queued", "running", "blocked", "paused", "wrapped", "cancelled", "failed"}

// defaultGraphLimit is a generous cap on the number of work items fetched
// for the graph view.
const defaultGraphLimit = 200

// RegisterWorkAPI wires the curated, read-only /api/v1/work/* endpoints
// that proxy the polyforge aihub backend for the tether workbench MVP
// (tether spec §10.K, Task A2):
//
//	GET /api/v1/work/projects                       → []wire.WorkProject
//	GET /api/v1/work/queue?project=&max=            → wire.WorkQueue
//	GET /api/v1/work/recent?project=&status=&limit= → wire.WorkRecent
//	GET /api/v1/work/graph?project=                 → wire.WorkGraph
//	GET /api/v1/work/items/{id}                     → wire.WorkItemDetail
//	GET /api/v1/work/items/{id}/events              → wire.WorkEvents
//	GET /api/v1/work/items/{id}/dependencies        → wire.WorkDependencies
//	GET /api/v1/work/items/{id}/steps               → wire.WorkSteps
//
// Every route is GET-only (405 otherwise) and unrecognised sub-paths 404.
// client may be nil — e.g. aihub.LoadConfig() found no usable credentials
// at startup (see lifecycle.go) — in which case every route responds 503
// "aihub not configured" rather than dereferencing a nil pointer. The mux
// registration happens unconditionally (mux.go) so that 503 behavior is
// reachable instead of falling through to the generic /api/v1/ 501 stub.
//
// workspaceRoot anchors the scenario-graph resolution for /steps (Task 5):
// scenario md files are looked up under workspaceRoot/.repo. An empty
// workspaceRoot simply means /steps never resolves a scenario file and
// always answers degraded (see handleWorkItemSteps).
func RegisterWorkAPI(mux *http.ServeMux, client *aihub.Client, workspaceRoot string) {
	mux.HandleFunc("/api/v1/work/projects", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if client == nil {
			http.Error(w, "aihub not configured", http.StatusServiceUnavailable)
			return
		}

		// D-15.3 caveat: aihub's GET /v1/projects (internal/domain.ListProjects)
		// returns every project with visible=true for a non-admin caller, not
		// just ones they own or are a member of. For the MVP single-operator
		// setup (one owner-scoped API key) that's a harmless superset, so this
		// passes the list through unfiltered. Revisit if tether ever serves
		// multiple distinct aihub identities behind one daemon.
		projects, err := client.Projects(r.Context())
		if err != nil {
			writeAihubError(w, err)
			return
		}
		out := make([]wire.WorkProject, len(projects))
		for i, p := range projects {
			out[i] = wire.WorkProject{Name: p.Name}
		}
		writeJSON(w, out)
	})

	mux.HandleFunc("/api/v1/work/queue", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		project := r.URL.Query().Get("project")
		if project == "" {
			http.Error(w, "project is required", http.StatusBadRequest)
			return
		}
		if client == nil {
			http.Error(w, "aihub not configured", http.StatusServiceUnavailable)
			return
		}

		max := defaultQueueMax
		if v := r.URL.Query().Get("max"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				max = n
			}
		}

		rq, err := client.ReadyQueue(r.Context(), project, max)
		if err != nil {
			writeAihubError(w, err)
			return
		}
		writeJSON(w, workQueueFromReadyQueue(rq))
	})

	mux.HandleFunc("/api/v1/work/recent", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		project := r.URL.Query().Get("project")
		if project == "" {
			http.Error(w, "project is required", http.StatusBadRequest)
			return
		}
		if client == nil {
			http.Error(w, "aihub not configured", http.StatusServiceUnavailable)
			return
		}

		statuses := recentStatuses
		if v := r.URL.Query().Get("status"); v != "" {
			statuses = strings.Split(v, ",")
		}
		limit := defaultRecentLimit
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}

		list, err := client.ListWorkItems(r.Context(), project, statuses, limit)
		if err != nil {
			writeAihubError(w, err)
			return
		}
		writeJSON(w, workRecentFromList(list))
	})

	mux.HandleFunc("/api/v1/work/graph", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		project := r.URL.Query().Get("project")
		if project == "" {
			http.Error(w, "project is required", http.StatusBadRequest)
			return
		}
		if client == nil {
			http.Error(w, "aihub not configured", http.StatusServiceUnavailable)
			return
		}

		list, err := client.ListWorkItems(r.Context(), project, graphStatuses, defaultGraphLimit)
		if err != nil {
			writeAihubError(w, err)
			return
		}
		writeJSON(w, workGraphFromList(list))
	})

	mux.HandleFunc("/api/v1/work/items/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/v1/work/items/")
		if rest == "" {
			http.NotFound(w, r)
			return
		}

		if id, ok := strings.CutSuffix(rest, "/events"); ok {
			if id == "" || strings.Contains(id, "/") {
				http.NotFound(w, r)
				return
			}
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if client == nil {
				http.Error(w, "aihub not configured", http.StatusServiceUnavailable)
				return
			}
			handleWorkItemEvents(w, r, client, id)
			return
		}

		if id, ok := strings.CutSuffix(rest, "/dependencies"); ok {
			if id == "" || strings.Contains(id, "/") {
				http.NotFound(w, r)
				return
			}
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if client == nil {
				http.Error(w, "aihub not configured", http.StatusServiceUnavailable)
				return
			}
			handleWorkItemDependencies(w, r, client, id)
			return
		}

		if id, ok := strings.CutSuffix(rest, "/steps"); ok {
			if id == "" || strings.Contains(id, "/") {
				http.NotFound(w, r)
				return
			}
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if client == nil {
				http.Error(w, "aihub not configured", http.StatusServiceUnavailable)
				return
			}
			handleWorkItemSteps(w, r, client, workspaceRoot, id)
			return
		}

		if strings.Contains(rest, "/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if client == nil {
			http.Error(w, "aihub not configured", http.StatusServiceUnavailable)
			return
		}
		handleWorkItemDetail(w, r, client, rest)
	})

	// Catch-all for the /api/v1/work/ namespace: anything that didn't match
	// a route above (e.g. /api/v1/work/bogus) 404s here instead of falling
	// through to the generic /api/v1/ 501-stub registered in buildMux —
	// "/api/v1/work/" is a longer, more specific ServeMux pattern than
	// "/api/v1/" so it wins for any path under this namespace.
	mux.HandleFunc("/api/v1/work/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
}

// handleWorkItemDetail serves GET /api/v1/work/items/{id} by merging
// client.WorkItem and client.StepState into a single wire.WorkItemDetail.
func handleWorkItemDetail(w http.ResponseWriter, r *http.Request, client *aihub.Client, id string) {
	item, err := client.WorkItem(r.Context(), id)
	if err != nil {
		// The item call is authoritative: 404/403/502 on it means the
		// whole detail response fails the same way.
		writeAihubError(w, err)
		return
	}

	// The step call is best-effort: a work item can legitimately have no
	// step-machine history yet (never claimed), and we don't want a
	// step-side error to take down an otherwise-successful item lookup. On
	// any error here we degrade to an empty StepState rather than failing
	// the request.
	var step aihub.StepState
	if s, err := client.StepState(r.Context(), id); err == nil {
		step = *s
	}

	writeJSON(w, wire.WorkItemDetail{
		ID:                item.ID,
		Slug:              item.Slug,
		Goal:              item.Goal,
		Status:            item.Status,
		Priority:          item.Priority,
		WIType:            item.WIType,
		Labels:            item.Labels,
		Content:           item.Content,
		CurrentStep:       step.CurrentStep,
		CurrentStepStatus: step.CurrentStepStatus,
	})
}

// handleWorkItemEvents serves GET /api/v1/work/items/{id}/events.
func handleWorkItemEvents(w http.ResponseWriter, r *http.Request, client *aihub.Client, id string) {
	limit := defaultEventsLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	cursor := r.URL.Query().Get("cursor")

	resp, err := client.Events(r.Context(), id, limit, cursor)
	if err != nil {
		writeAihubError(w, err)
		return
	}

	events := make([]wire.WorkEvent, len(resp.Events))
	for i, e := range resp.Events {
		events[i] = wire.WorkEvent{Ts: e.CreatedAt, Type: e.EventType, Payload: e.Payload}
	}
	writeJSON(w, wire.WorkEvents{Events: events, NextCursor: resp.NextCursor})
}

// handleWorkItemDependencies serves GET /api/v1/work/items/{id}/dependencies.
func handleWorkItemDependencies(w http.ResponseWriter, r *http.Request, client *aihub.Client, id string) {
	deps, err := client.Dependencies(r.Context(), id)
	if err != nil {
		writeAihubError(w, err)
		return
	}
	writeJSON(w, wire.WorkDependencies{
		Blocking:  workDepEntries(deps.Blocking),
		BlockedBy: workDepEntries(deps.BlockedBy),
	})
}

// handleWorkItemSteps serves GET /api/v1/work/items/{id}/steps: it resolves
// the polyforge-coding scenario graph file for the work item's (wi_type,
// project) under workspaceRoot/.repo, parses it into a step DAG (Task 4),
// and annotates each node with its progress status (done/current/pending)
// from the step-machine current-step state and step_completed events.
//
// If no scenario file can be resolved (or it fails to parse), the response
// degrades gracefully: Degraded=true and Nodes is synthesized best-effort
// from the completed-step set and the current step, with no Prev edges.
func handleWorkItemSteps(w http.ResponseWriter, r *http.Request, client *aihub.Client, workspaceRoot, id string) {
	item, err := client.WorkItem(r.Context(), id)
	if err != nil {
		writeAihubError(w, err)
		return
	}
	wiType := ""
	if item.WIType != nil {
		wiType = *item.WIType
	}

	// Best-effort: an item without step-machine history yet degrades to an
	// empty current step rather than failing the whole request.
	var current string
	var inProgress bool
	if st, err := client.StepState(r.Context(), id); err == nil {
		if st.CurrentStep != nil {
			current = *st.CurrentStep
		}
		inProgress = st.CurrentStepStatus == "in_progress"
	}

	// Best-effort: an events fetch error just means no steps show as done.
	completed := map[string]bool{}
	if resp, err := client.Events(r.Context(), id, stepsEventsLimit, ""); err == nil {
		for _, e := range resp.Events {
			if e.EventType != "step_completed" {
				continue
			}
			var payload struct {
				Step string `json:"step"`
			}
			if err := json.Unmarshal(e.Payload, &payload); err == nil && payload.Step != "" {
				completed[payload.Step] = true
			}
		}
	}

	var graph *scenario.StepGraph
	if path, ok := scenario.ResolveScenarioFile(workspaceRoot, wiType, item.Project); ok {
		graph, _ = scenario.ParseStepGraph(path)
	}

	if graph == nil {
		writeJSON(w, wire.WorkSteps{
			Nodes:    synthesizeDegradedSteps(completed, current, inProgress),
			Degraded: true,
		})
		return
	}

	nodes := make([]wire.WorkStepNode, len(graph.Nodes))
	for i, n := range graph.Nodes {
		nodes[i] = wire.WorkStepNode{
			ID:     n.ID,
			Status: stepStatus(n.ID, completed, current, inProgress),
			Prev:   n.Prev,
		}
	}
	writeJSON(w, wire.WorkSteps{Nodes: nodes, Degraded: false})
}

// stepStatus classifies a single step id as "done" (in the completed set),
// "current" (it's the in-progress current step), or "pending" (neither).
func stepStatus(id string, completed map[string]bool, current string, inProgress bool) string {
	switch {
	case completed[id]:
		return "done"
	case id == current && inProgress:
		return "current"
	default:
		return "pending"
	}
}

// synthesizeDegradedSteps builds a best-effort, edge-less node list from the
// completed-step set and the current step, for when no scenario graph file
// could be resolved. Completed step ids are sorted for determinism; the
// current step (if not already completed) is appended last.
func synthesizeDegradedSteps(completed map[string]bool, current string, inProgress bool) []wire.WorkStepNode {
	ids := make([]string, 0, len(completed)+1)
	for id := range completed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if current != "" && !completed[current] {
		ids = append(ids, current)
	}

	nodes := make([]wire.WorkStepNode, len(ids))
	for i, id := range ids {
		nodes[i] = wire.WorkStepNode{ID: id, Status: stepStatus(id, completed, current, inProgress)}
	}
	return nodes
}

func workDepEntries(entries []aihub.DependencyEntry) []wire.WorkDepEntry {
	out := make([]wire.WorkDepEntry, len(entries))
	for i, e := range entries {
		out[i] = wire.WorkDepEntry{
			ID:      e.ID,
			Slug:    e.Slug,
			Project: e.Project,
			Kind:    e.Kind,
			Note:    e.Note,
		}
	}
	return out
}

// workQueueFromReadyQueue maps an aihub.ReadyQueue onto the whitelisted
// wire.WorkQueue shape the browser consumes.
func workQueueFromReadyQueue(rq *aihub.ReadyQueue) wire.WorkQueue {
	return wire.WorkQueue{
		Items:             workReadyItems(rq.Items),
		Running:           workRunningItems(rq.Running),
		Stalled:           workStalledItems(rq.Stalled),
		Paused:            workPausedItems(rq.Paused),
		NeedsHumanSession: workReadyItems(rq.NeedsHumanSession),
		Unclassified:      workReadyItems(rq.Unclassified),
		StaleRunning:      workRunningItems(rq.StaleRunning),
	}
}

func workReadyItems(items []aihub.ReadyItem) []wire.WorkReadyItem {
	out := make([]wire.WorkReadyItem, len(items))
	for i, it := range items {
		out[i] = wire.WorkReadyItem{
			ID:          it.ID,
			Slug:        it.Slug,
			WIType:      it.WIType,
			Priority:    it.Priority,
			Goal:        it.Goal,
			UnblockedAt: it.UnblockedAt,
			CreatedAt:   it.CreatedAt,
		}
	}
	return out
}

func workRunningItems(items []aihub.RunningItem) []wire.WorkRunningItem {
	out := make([]wire.WorkRunningItem, len(items))
	for i, it := range items {
		out[i] = wire.WorkRunningItem{
			ID:           it.ID,
			Slug:         it.Slug,
			Goal:         it.Goal,
			OwnerDisplay: it.OwnerDisplay,
			LastActiveAt: it.LastActiveAt,
		}
	}
	return out
}

func workStalledItems(items []aihub.StalledItem) []wire.WorkStalledItem {
	out := make([]wire.WorkStalledItem, len(items))
	for i, it := range items {
		out[i] = wire.WorkStalledItem{
			ID:               it.ID,
			Slug:             it.Slug,
			StallReason:      it.StallReason,
			StalledSince:     it.StalledSince,
			LastActorDisplay: it.LastActorDisplay,
		}
	}
	return out
}

func workPausedItems(items []aihub.PausedItem) []wire.WorkPausedItem {
	out := make([]wire.WorkPausedItem, len(items))
	for i, it := range items {
		out[i] = wire.WorkPausedItem{
			ID:               it.ID,
			Slug:             it.Slug,
			PausedSince:      it.PausedSince,
			LastActorDisplay: it.LastActorDisplay,
			PauseReason:      it.PauseReason,
		}
	}
	return out
}

// workRecentFromList maps an aihub.WorkItemList onto the whitelisted
// wire.WorkRecent shape the browser consumes for the done/recent view.
func workRecentFromList(list *aihub.WorkItemList) wire.WorkRecent {
	out := make([]wire.WorkRecentItem, len(list.Items))
	for i, it := range list.Items {
		out[i] = wire.WorkRecentItem{
			ID:       it.ID,
			Slug:     it.Slug,
			Goal:     it.Goal,
			Status:   it.Status,
			Priority: it.Priority,
			WIType:   it.WIType,
			ClosedAt: it.ClosedAt,
		}
	}
	return wire.WorkRecent{Items: out}
}

// workGraphFromList maps an aihub.WorkItemList onto the whitelisted
// wire.WorkGraph shape the browser consumes for the work graph view,
// carrying each item's parent through to WorkGraphNode.Parent.
func workGraphFromList(list *aihub.WorkItemList) wire.WorkGraph {
	out := make([]wire.WorkGraphNode, len(list.Items))
	for i, it := range list.Items {
		out[i] = wire.WorkGraphNode{
			ID:       it.ID,
			Slug:     it.Slug,
			Goal:     it.Goal,
			Status:   it.Status,
			Priority: it.Priority,
			WIType:   it.WIType,
			Parent:   it.ParentWorkItemID,
		}
	}
	return wire.WorkGraph{Nodes: out}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// writeAihubError maps an aihub client error to an HTTP response without
// ever echoing the underlying error text to the client. aihub.Client's own
// error strings never include the API key or Authorization header (they
// carry only the request path and HTTP status), but this generic mapping
// is a deliberate second line of defence so a future change to that error
// text can't leak credentials to a tether browser client.
func writeAihubError(w http.ResponseWriter, err error) {
	if errors.Is(err, aihub.ErrForbidden) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	http.Error(w, "aihub upstream error", http.StatusBadGateway)
}
