package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/piaobeizu/tether/internal/aihub"
	"github.com/piaobeizu/tether/internal/server"
	"github.com/piaobeizu/tether/internal/wire"
)

// aihubStub is a canned stand-in for the real aihub HTTP API. It records
// which paths were hit (so tests can assert both the item and step calls
// happened for /work/items/{id}) and returns fixed JSON bodies shaped like
// aihub's real responses (see internal/aihub/client_test.go for the same
// wire shapes used against the real client).
type aihubStub struct {
	workItemHits int32
	stepHits     int32
	eventsHits   int32
}

func (s *aihubStub) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/work_items/ready":
			_, _ = w.Write([]byte(`{
				"items": [
					{"id":"wi_1","slug":"proj-1","wi_type":"feature","priority":"high","goal":"do the thing","created_at":"2026-01-01T00:00:00Z"}
				],
				"running": [
					{"id":"wi_2","slug":"proj-2","goal":"running thing","owner_display":"alice","last_active_at":"2026-01-02T00:00:00Z"}
				],
				"stalled": [
					{"id":"wi_3","slug":"proj-3","stall_reason":"waiting","stalled_since":"2026-01-03T00:00:00Z","last_actor_display":"bob"}
				],
				"paused": [
					{"id":"wi_4","slug":"proj-4","paused_since":"2026-01-04T00:00:00Z","last_actor_display":"carol","pause_reason":"lunch"}
				],
				"needs_human_session": [],
				"unclassified": []
			}`))
		case r.URL.Path == "/v1/work_items/wi_1":
			atomic.AddInt32(&s.workItemHits, 1)
			_, _ = w.Write([]byte(`{"id":"wi_1","slug":"proj-1","goal":"do the thing","status":"in_progress","priority":"high","wi_type":"feature","labels":["a","b"],"content":"body text"}`))
		case r.URL.Path == "/v1/work_items/wi_1/step":
			atomic.AddInt32(&s.stepHits, 1)
			_, _ = w.Write([]byte(`{"current_step":"plan","current_step_status":"running"}`))
		case r.URL.Path == "/v1/events":
			atomic.AddInt32(&s.eventsHits, 1)
			_, _ = w.Write([]byte(`{
				"events": [
					{"created_at":"2026-01-05T00:00:00Z","event_type":"note","payload":{"x":1}}
				],
				"next_cursor": "cursor-abc"
			}`))
		case r.URL.Path == "/v1/projects":
			_, _ = w.Write([]byte(`{"items":[{"name":"tether"},{"name":"aihub"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func newTestMux(client *aihub.Client) http.Handler {
	mux := http.NewServeMux()
	server.RegisterWorkAPI(mux, client, "")
	return mux
}

// newTestMuxWithRoot is like newTestMux but wires a non-empty workspaceRoot,
// for the /steps endpoint (tether#20 Task 5) which resolves a scenario graph
// file under workspaceRoot/.repo.
func newTestMuxWithRoot(client *aihub.Client, workspaceRoot string) http.Handler {
	mux := http.NewServeMux()
	server.RegisterWorkAPI(mux, client, workspaceRoot)
	return mux
}

// 1. GET /work/queue?project=x → 200, sections mapped; missing project → 400.
func TestWorkQueue_MapsSections(t *testing.T) {
	stub := &aihubStub{}
	srv := stub.server()
	defer srv.Close()

	mux := newTestMux(aihub.New(srv.URL, "k"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/work/queue?project=proj", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got wire.WorkQueue
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if len(got.Items) != 1 || got.Items[0].ID != "wi_1" || got.Items[0].Slug != "proj-1" {
		t.Errorf("Items = %+v, want one item wi_1/proj-1", got.Items)
	}
	if got.Items[0].WIType == nil || *got.Items[0].WIType != "feature" {
		t.Errorf("Items[0].WIType = %v, want \"feature\"", got.Items[0].WIType)
	}
	if len(got.Running) != 1 || got.Running[0].OwnerDisplay != "alice" {
		t.Errorf("Running = %+v, want one item owned by alice", got.Running)
	}
	if len(got.Stalled) != 1 || got.Stalled[0].StallReason != "waiting" {
		t.Errorf("Stalled = %+v, want one item with reason waiting", got.Stalled)
	}
	if len(got.Paused) != 1 || got.Paused[0].PauseReason == nil || *got.Paused[0].PauseReason != "lunch" {
		t.Errorf("Paused = %+v, want one item with reason lunch", got.Paused)
	}
	if len(got.NeedsHumanSession) != 0 || len(got.Unclassified) != 0 {
		t.Errorf("NeedsHumanSession/Unclassified should be empty, got %+v / %+v", got.NeedsHumanSession, got.Unclassified)
	}

	// Missing project → 400.
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/work/queue", nil)
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("missing project: status = %d, want 400", rec2.Code)
	}
}

// 2. GET /work/items/{id} → 200, item+step merged (both aihub calls happened).
func TestWorkItemDetail_MergesItemAndStep(t *testing.T) {
	stub := &aihubStub{}
	srv := stub.server()
	defer srv.Close()

	mux := newTestMux(aihub.New(srv.URL, "k"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/work/items/wi_1", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got wire.WorkItemDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if got.ID != "wi_1" || got.Goal != "do the thing" || got.Status != "in_progress" {
		t.Errorf("item fields = %+v, missing/wrong", got)
	}
	if got.CurrentStep == nil || *got.CurrentStep != "plan" || got.CurrentStepStatus != "running" {
		t.Errorf("step fields = %+v, want current_step=plan status=running", got)
	}
	if atomic.LoadInt32(&stub.workItemHits) != 1 {
		t.Errorf("workItemHits = %d, want 1 (item call did not happen)", stub.workItemHits)
	}
	if atomic.LoadInt32(&stub.stepHits) != 1 {
		t.Errorf("stepHits = %d, want 1 (step call did not happen)", stub.stepHits)
	}
}

// 3. GET /work/items/{id}/events → 200, NextCursor passed through, and the
// forwarded limit/cursor query params actually reach the upstream request
// (not just that the response cursor round-trips).
func TestWorkItemEvents_PassesThroughCursor(t *testing.T) {
	var gotLimit, gotCursor, gotWorkItemID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/events" {
			gotLimit = r.URL.Query().Get("limit")
			gotCursor = r.URL.Query().Get("cursor")
			gotWorkItemID = r.URL.Query().Get("work_item_id")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"events": [
				{"created_at":"2026-01-05T00:00:00Z","event_type":"note","payload":{"x":1}}
			],
			"next_cursor": "cursor-abc"
		}`))
	}))
	defer srv.Close()

	mux := newTestMux(aihub.New(srv.URL, "k"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/work/items/wi_1/events?limit=5&cursor=in", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got wire.WorkEvents
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if len(got.Events) != 1 || got.Events[0].Type != "note" {
		t.Fatalf("Events = %+v, want one \"note\" event", got.Events)
	}
	if got.NextCursor == nil || *got.NextCursor != "cursor-abc" {
		t.Errorf("NextCursor = %v, want \"cursor-abc\"", got.NextCursor)
	}

	// The limit and cursor from the browser request must be forwarded to the
	// upstream aihub /v1/events call, along with the path work-item id.
	if gotLimit != "5" {
		t.Errorf("upstream limit query = %q, want 5", gotLimit)
	}
	if gotCursor != "in" {
		t.Errorf("upstream cursor query = %q, want \"in\"", gotCursor)
	}
	if gotWorkItemID != "wi_1" {
		t.Errorf("upstream work_item_id query = %q, want wi_1", gotWorkItemID)
	}
}

// 8. aihub.ErrForbidden → HTTP 403 (the writeAihubError forbidden branch).
// The upstream returns 403 for the work-item call; the handler must surface
// 403 to the client without leaking the api_key or an Authorization scheme.
func TestWorkAPI_ForbiddenMapsTo403(t *testing.T) {
	const secretKey = "super-secret-aihub-key-should-never-leak"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer upstream.Close()

	mux := newTestMux(aihub.New(upstream.URL, secretKey))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/work/items/wi_1", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secretKey) {
		t.Errorf("403 response leaked the api_key: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "Bearer") {
		t.Errorf("403 response leaked an Authorization scheme: %s", rec.Body.String())
	}
}

// 4. GET /work/projects → 200, list.
func TestWorkProjects_List(t *testing.T) {
	stub := &aihubStub{}
	srv := stub.server()
	defer srv.Close()

	mux := newTestMux(aihub.New(srv.URL, "k"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/work/projects", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got []wire.WorkProject
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if len(got) != 2 || got[0].Name != "tether" || got[1].Name != "aihub" {
		t.Fatalf("got = %+v, want [tether aihub]", got)
	}
}

// 5. Read-only: POST /api/v1/work/queue → 405; unknown /api/v1/work/bogus → 404.
func TestWorkAPI_ReadOnlyAndUnknownPaths(t *testing.T) {
	stub := &aihubStub{}
	srv := stub.server()
	defer srv.Close()

	mux := newTestMux(aihub.New(srv.URL, "k"))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/work/queue?project=x", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /work/queue: status = %d, want 405", rec.Code)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/work/bogus", nil)
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotFound {
		t.Errorf("GET /work/bogus: status = %d, want 404", rec2.Code)
	}

	// Also cover the other GET-only routes for good measure.
	for _, path := range []string{"/api/v1/work/projects", "/api/v1/work/recent?project=x", "/api/v1/work/graph?project=x", "/api/v1/work/items/wi_1", "/api/v1/work/items/wi_1/events", "/api/v1/work/items/wi_1/dependencies", "/api/v1/work/items/wi_1/steps"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: status = %d, want 405", path, rec.Code)
		}
	}
}

// 6. Key isolation: a handler backed by a client whose upstream returns 500
// must never leak the api_key into the response body/headers, and must
// never surface an Authorization value.
func TestWorkAPI_NeverLeaksAPIKey(t *testing.T) {
	const secretKey = "super-secret-aihub-key-should-never-leak"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sanity: confirm the outbound request really does carry the key,
		// so this test would fail loudly if the client stopped sending it.
		if got := r.Header.Get("Authorization"); got != "Bearer "+secretKey {
			t.Errorf("upstream saw Authorization = %q, want Bearer %s", got, secretKey)
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	mux := newTestMux(aihub.New(upstream.URL, secretKey))

	for _, path := range []string{
		"/api/v1/work/projects",
		"/api/v1/work/queue?project=x",
		"/api/v1/work/recent?project=x",
		"/api/v1/work/graph?project=x",
		"/api/v1/work/items/wi_1",
		"/api/v1/work/items/wi_1/events",
		"/api/v1/work/items/wi_1/dependencies",
		"/api/v1/work/items/wi_1/steps",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadGateway {
			t.Errorf("%s: status = %d, want 502", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), secretKey) {
			t.Errorf("%s: response body leaked the api_key: %s", path, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "Bearer") {
			t.Errorf("%s: response body leaked an Authorization scheme: %s", path, rec.Body.String())
		}
		if v := rec.Header().Get("Authorization"); v != "" {
			t.Errorf("%s: response set an Authorization header: %q", path, v)
		}
	}
}

// 7. nil client → 503.
func TestWorkAPI_NilClient(t *testing.T) {
	mux := newTestMux(nil)

	for _, path := range []string{
		"/api/v1/work/projects",
		"/api/v1/work/queue?project=x",
		"/api/v1/work/recent?project=x",
		"/api/v1/work/graph?project=x",
		"/api/v1/work/items/wi_1",
		"/api/v1/work/items/wi_1/events",
		"/api/v1/work/items/wi_1/dependencies",
		"/api/v1/work/items/wi_1/steps",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status = %d, want 503", path, rec.Code)
		}
	}
}

// Sanity check that limit/max query params are actually forwarded as
// integers (not just accepted syntactically) — regression guard for the
// strconv.Atoi parsing in RegisterWorkAPI.
func TestWorkQueue_MaxOverride(t *testing.T) {
	var gotMax string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/work_items/ready" {
			gotMax = r.URL.Query().Get("max")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"running":[],"stalled":[],"paused":[],"needs_human_session":[],"unclassified":[]}`))
	}))
	defer srv.Close()

	mux := newTestMux(aihub.New(srv.URL, "k"))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/work/queue?project=x&max=25", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if gotMax != strconv.Itoa(25) {
		t.Errorf("upstream max query = %q, want 25", gotMax)
	}
}

// 9. GET /work/recent?project=x → 200, terminal items mapped; the default
// status filter (wrapped,cancelled) and limit (20) are forwarded upstream;
// missing project → 400. (tether#19 done/recent view.)
func TestWorkRecent_MapsItems(t *testing.T) {
	var gotPath, gotStatus, gotLimit, gotProject string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotStatus = r.URL.Query().Get("status")
		gotLimit = r.URL.Query().Get("limit")
		gotProject = r.URL.Query().Get("project")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[
			{"id":"wi_18","slug":"tether#18","goal":"origin guard","status":"wrapped","priority":"high","wi_type":"fix_bug","closed_at":"2026-07-10T09:09:24Z"},
			{"id":"wi_13","slug":"tether#13","goal":"live-replace","status":"cancelled","priority":"normal","wi_type":"fix_bug","closed_at":"2026-07-08T09:53:21Z"}
		],"next_cursor":null}`))
	}))
	defer srv.Close()

	mux := newTestMux(aihub.New(srv.URL, "k"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/work/recent?project=tether", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got wire.WorkRecent
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if len(got.Items) != 2 {
		t.Fatalf("Items = %+v, want 2", got.Items)
	}
	if got.Items[0].Slug != "tether#18" || got.Items[0].Status != "wrapped" {
		t.Errorf("Items[0] = %+v, want tether#18/wrapped", got.Items[0])
	}
	if got.Items[0].ClosedAt == nil || *got.Items[0].ClosedAt == "" {
		t.Errorf("Items[0].ClosedAt should be set")
	}
	if got.Items[0].WIType == nil || *got.Items[0].WIType != "fix_bug" {
		t.Errorf("Items[0].WIType = %v, want fix_bug", got.Items[0].WIType)
	}
	if got.Items[1].Status != "cancelled" {
		t.Errorf("Items[1].Status = %q, want cancelled", got.Items[1].Status)
	}

	if gotPath != "/v1/work_items" {
		t.Errorf("upstream path = %q, want /v1/work_items", gotPath)
	}
	if gotProject != "tether" {
		t.Errorf("upstream project = %q, want tether", gotProject)
	}
	if gotStatus != "wrapped,cancelled,failed" {
		t.Errorf("upstream status = %q, want default wrapped,cancelled,failed", gotStatus)
	}
	if gotLimit != "20" {
		t.Errorf("upstream limit = %q, want default 20", gotLimit)
	}

	// Missing project → 400.
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/work/recent", nil)
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("missing project: status = %d, want 400", rec2.Code)
	}
}

// 10. GET /work/recent with explicit ?status=&limit= overrides the defaults
// and forwards them upstream (regression guard for the override parsing).
func TestWorkRecent_StatusLimitOverride(t *testing.T) {
	var gotStatus, gotLimit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotStatus = r.URL.Query().Get("status")
		gotLimit = r.URL.Query().Get("limit")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"next_cursor":null}`))
	}))
	defer srv.Close()

	mux := newTestMux(aihub.New(srv.URL, "k"))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/work/recent?project=x&status=wrapped&limit=5", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if gotStatus != "wrapped" {
		t.Errorf("upstream status = %q, want override \"wrapped\"", gotStatus)
	}
	if gotLimit != "5" {
		t.Errorf("upstream limit = %q, want override \"5\"", gotLimit)
	}
}

// 11. GET /work/graph?project=x → 200, nodes mapped (including parent);
// missing project → 400. (tether#20 Work view graph.)
func TestWorkGraph_MapsParent(t *testing.T) {
	var gotPath, gotStatus, gotLimit, gotProject string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotStatus = r.URL.Query().Get("status")
		gotLimit = r.URL.Query().Get("limit")
		gotProject = r.URL.Query().Get("project")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[
			{"id":"wi_1","slug":"tether#1","goal":"epic","status":"running","priority":"high","wi_type":"epic"},
			{"id":"wi_2","slug":"tether#2","goal":"child task","status":"queued","priority":"normal","wi_type":"feature","parent_work_item_id":"wi_1"}
		],"next_cursor":null}`))
	}))
	defer srv.Close()

	mux := newTestMux(aihub.New(srv.URL, "k"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/work/graph?project=tether", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got wire.WorkGraph
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if len(got.Nodes) != 2 {
		t.Fatalf("Nodes = %+v, want 2", got.Nodes)
	}
	if got.Nodes[0].ID != "wi_1" || got.Nodes[0].Parent != nil {
		t.Errorf("Nodes[0] = %+v, want wi_1 with no parent", got.Nodes[0])
	}
	if got.Nodes[1].ID != "wi_2" || got.Nodes[1].Parent == nil || *got.Nodes[1].Parent != "wi_1" {
		t.Errorf("Nodes[1] = %+v, want wi_2 with parent wi_1", got.Nodes[1])
	}

	if gotPath != "/v1/work_items" {
		t.Errorf("upstream path = %q, want /v1/work_items", gotPath)
	}
	if gotProject != "tether" {
		t.Errorf("upstream project = %q, want tether", gotProject)
	}
	if gotStatus == "" {
		t.Errorf("upstream status should be a non-empty status filter, got empty")
	}
	if gotLimit == "" {
		t.Errorf("upstream limit should be set, got empty")
	}

	// Missing project → 400.
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/work/graph", nil)
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("missing project: status = %d, want 400", rec2.Code)
	}
}

// 12. GET /work/items/{id}/dependencies → 200, blocking/blockedBy mapped.
// (tether#20 Work view dependency panel.)
func TestWorkItemDependencies(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"blocking": [
				{"id":"wi_2","slug":"tether-2","project":"tether","kind":"blocks","note":"needs api first"}
			],
			"blocked_by": [
				{"id":"wi_3","slug":"tether-3","project":"tether","kind":"blocks","note":""}
			]
		}`))
	}))
	defer srv.Close()

	mux := newTestMux(aihub.New(srv.URL, "k"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/work/items/wi_1/dependencies", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got wire.WorkDependencies
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if len(got.Blocking) != 1 || got.Blocking[0].ID != "wi_2" || got.Blocking[0].Slug != "tether-2" {
		t.Fatalf("Blocking = %+v, want one entry wi_2/tether-2", got.Blocking)
	}
	if got.Blocking[0].Note != "needs api first" || got.Blocking[0].Kind != "blocks" {
		t.Errorf("Blocking[0] = %+v, unexpected fields", got.Blocking[0])
	}
	if len(got.BlockedBy) != 1 || got.BlockedBy[0].ID != "wi_3" || got.BlockedBy[0].Slug != "tether-3" {
		t.Fatalf("BlockedBy = %+v, want one entry wi_3/tether-3", got.BlockedBy)
	}

	if gotPath != "/v1/work_items/wi_1/dependencies" {
		t.Errorf("upstream path = %q, want /v1/work_items/wi_1/dependencies", gotPath)
	}
}

// 13. GET /work/items/{id}/steps → 200, scenario graph resolved from
// workspaceRoot/.repo, nodes annotated with done/current/pending status from
// step_completed events + the current-step state. (tether#20 Task 5.)
func TestWorkSteps_GraphWithStatus(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, ".repo", "tether")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "## Step: a\n" +
		"first step\n" +
		"## Step: b\n" +
		"second step, no explicit reference\n" +
		"## Step: c\n" +
		`x = previous_steps["a"]` + "\n"
	if err := os.WriteFile(filepath.Join(repoDir, "feature.tether.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/work_items/wi_1":
			_, _ = w.Write([]byte(`{"id":"wi_1","slug":"tether-1","goal":"g","status":"running","priority":"high","wi_type":"feature","project":"tether","labels":[],"content":null}`))
		case "/v1/work_items/wi_1/step":
			_, _ = w.Write([]byte(`{"current_step":"b","current_step_status":"in_progress"}`))
		case "/v1/events":
			_, _ = w.Write([]byte(`{"events":[
				{"created_at":"2026-01-01T00:00:00Z","event_type":"step_completed","payload":{"step":"a"}},
				{"created_at":"2026-01-01T00:01:00Z","event_type":"note","payload":{"x":1}}
			],"next_cursor":null}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	mux := newTestMuxWithRoot(aihub.New(srv.URL, "k"), root)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/work/items/wi_1/steps", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got wire.WorkSteps
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if got.Degraded {
		t.Errorf("Degraded = true, want false (scenario graph resolved)")
	}
	if len(got.Nodes) != 3 {
		t.Fatalf("Nodes = %+v, want 3 nodes", got.Nodes)
	}
	a, b, c := got.Nodes[0], got.Nodes[1], got.Nodes[2]
	if a.ID != "a" || a.Status != "done" {
		t.Errorf("Nodes[0] = %+v, want a/done", a)
	}
	if b.ID != "b" || b.Status != "current" || len(b.Prev) != 1 || b.Prev[0] != "a" {
		t.Errorf("Nodes[1] = %+v, want b/current with Prev=[a] (sequential fallback)", b)
	}
	if c.ID != "c" || c.Status != "pending" || len(c.Prev) != 1 || c.Prev[0] != "a" {
		t.Errorf("Nodes[2] = %+v, want c/pending with Prev=[a] (explicit reference)", c)
	}
}

// 14. GET /work/items/{id}/steps with no resolvable scenario file →
// Degraded=true, nodes synthesized best-effort from step_completed events +
// current step. (tether#20 Task 5.)
func TestWorkSteps_DegradedNoScenario(t *testing.T) {
	root := t.TempDir() // no .repo dir at all

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/work_items/wi_1":
			_, _ = w.Write([]byte(`{"id":"wi_1","slug":"tether-1","goal":"g","status":"running","priority":"high","wi_type":"feature","project":"tether","labels":[],"content":null}`))
		case "/v1/work_items/wi_1/step":
			_, _ = w.Write([]byte(`{"current_step":"x","current_step_status":"in_progress"}`))
		case "/v1/events":
			_, _ = w.Write([]byte(`{"events":[
				{"created_at":"2026-01-01T00:00:00Z","event_type":"step_completed","payload":{"step":"y"}}
			],"next_cursor":null}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	mux := newTestMuxWithRoot(aihub.New(srv.URL, "k"), root)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/work/items/wi_1/steps", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got wire.WorkSteps
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if !got.Degraded {
		t.Errorf("Degraded = false, want true (no scenario graph resolvable)")
	}
	byID := map[string]wire.WorkStepNode{}
	for _, n := range got.Nodes {
		byID[n.ID] = n
	}
	if n, ok := byID["y"]; !ok || n.Status != "done" {
		t.Errorf("Nodes missing y/done, got %+v", got.Nodes)
	}
	if n, ok := byID["x"]; !ok || n.Status != "current" {
		t.Errorf("Nodes missing x/current, got %+v", got.Nodes)
	}
}
