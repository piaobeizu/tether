package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/piaobeizu/tether/internal/aihub"
	"github.com/piaobeizu/tether/internal/wire"
)

// Default paging values for the curated aihub proxy (Task A2). Callers may
// override via ?max= (queue) or ?limit= (events).
const (
	defaultQueueMax    = 10
	defaultEventsLimit = 50
)

// RegisterWorkAPI wires the curated, read-only /api/v1/work/* endpoints
// that proxy the polyforge aihub backend for the tether workbench MVP
// (tether spec §10.K, Task A2):
//
//	GET /api/v1/work/projects              → []wire.WorkProject
//	GET /api/v1/work/queue?project=&max=   → wire.WorkQueue
//	GET /api/v1/work/items/{id}            → wire.WorkItemDetail
//	GET /api/v1/work/items/{id}/events     → wire.WorkEvents
//
// Every route is GET-only (405 otherwise) and unrecognised sub-paths 404.
// client may be nil — e.g. aihub.LoadConfig() found no usable credentials
// at startup (see lifecycle.go) — in which case every route responds 503
// "aihub not configured" rather than dereferencing a nil pointer. The mux
// registration happens unconditionally (mux.go) so that 503 behavior is
// reachable instead of falling through to the generic /api/v1/ 501 stub.
func RegisterWorkAPI(mux *http.ServeMux, client *aihub.Client) {
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
