package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// 15. Every caller-supplied page size in the aihub proxy is bounded before it
// reaches aihub (tether#143). Before the fix each entry point checked only
// `n > 0` and then forwarded the value verbatim, so one browser request could
// make the daemon ask the upstream for an unbounded page.
//
// The three rows below are the complete set of query params in
// aihub_proxy.go that flow into a count. Before this change that file had
// eight r.URL.Query().Get() call sites: these three plus five that carry no
// count (project on /queue, /recent and /graph; the status filter on
// /recent; the opaque cursor on /events). The three collapsed into the
// shared pageSize helper, so the file now greps as six — pageSize's own read
// plus those five. /graph's page size is the hardcoded defaultGraphLimit,
// not a caller value, and /steps uses stepsEventsLimit the same way.
//
// Each row asserts the same three-part contract, so a fourth paging param
// added without a cap has to be added here too.
func TestWorkPaging_BoundedAndDefaulted(t *testing.T) {
	// The count the proxy actually asked the upstream for, as seen on the
	// wire. Written by the stub handler and read after mux.ServeHTTP returns,
	// same as the other forwarding tests in this file.
	var gotCount string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Whichever of the two upstream page-size params is present on this
		// request is the count under test (aihub.Client spells it "max" for
		// the ready queue and "limit" for the other two).
		gotCount = ""
		for _, p := range []string{"max", "limit"} {
			if v := r.URL.Query().Get(p); v != "" {
				gotCount = v
			}
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/work_items/ready":
			_, _ = w.Write([]byte(`{"items":[],"running":[],"stalled":[],"paused":[],"needs_human_session":[],"unclassified":[]}`))
		case "/v1/work_items":
			_, _ = w.Write([]byte(`{"items":[],"next_cursor":null}`))
		case "/v1/events":
			_, _ = w.Write([]byte(`{"events":[],"next_cursor":null}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	mux := newTestMux(aihub.New(srv.URL, "k"))

	// base already ends in the separator the paging param gets appended to,
	// so the "param absent" case is just base on its own.
	for _, ep := range []struct {
		name  string
		base  string
		param string
		def   int
		upper int
	}{
		{name: "queue_max", base: "/api/v1/work/queue?project=x&", param: "max", def: 10, upper: 100},
		{name: "recent_limit", base: "/api/v1/work/recent?project=x&", param: "limit", def: 20, upper: 200},
		{name: "events_limit", base: "/api/v1/work/items/wi_1/events?", param: "limit", def: 50, upper: 500},
	} {
		t.Run(ep.name, func(t *testing.T) {
			ask := func(t *testing.T, url string) string {
				t.Helper()
				// Sentinel, not "": the stub only overwrites gotCount when it
				// is actually reached, so without this a request the proxy
				// answers without calling upstream would silently be asserted
				// against the *previous* request's count.
				gotCount = "<upstream not called>"
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
				if rec.Code != http.StatusOK {
					t.Fatalf("GET %s: status = %d, want 200; body=%s", url, rec.Code, rec.Body.String())
				}
				return gotCount
			}
			with := func(v string) string { return ep.base + ep.param + "=" + v }

			// Absent → the endpoint's default page size reaches aihub.
			if got := ask(t, ep.base); got != strconv.Itoa(ep.def) {
				t.Errorf("no %s: upstream count = %q, want default %d", ep.param, got, ep.def)
			}

			// Under the cap, and exactly at it → forwarded verbatim. The cap
			// must not shrink page sizes callers are allowed to ask for, and
			// must be inclusive (off-by-one guard).
			for _, ok := range []int{1, ep.upper - 1, ep.upper} {
				if got := ask(t, with(strconv.Itoa(ok))); got != strconv.Itoa(ok) {
					t.Errorf("%s=%d: upstream count = %q, want %d passed through", ep.param, ok, got, ok)
				}
			}

			// Over the cap → clamped down to the cap. This is the tether#143
			// regression: the value used to reach aihub unchanged.
			for _, over := range []int{ep.upper + 1, 10 * ep.upper, 1000000} {
				if got := ask(t, with(strconv.Itoa(over))); got != strconv.Itoa(ep.upper) {
					t.Errorf("%s=%d: upstream count = %q, want clamp to %d", ep.param, over, got, ep.upper)
				}
			}

			// Zero, negative, unparseable and out-of-int-range values keep the
			// pre-existing silent fallback to the default. Adding the cap must
			// not turn any of these into a 400 — ask() already fails the test
			// if the response is not 200. "%207" is " 7", which strconv
			// rejects; the last entry overflows int, which strconv reports as
			// a range error rather than a value the clamp could act on.
			for _, junk := range []string{"", "0", "-1", "-9999", "abc", "12.5", "1e6", "%207", "99999999999999999999999999"} {
				if got := ask(t, with(junk)); got != strconv.Itoa(ep.def) {
					t.Errorf("%s=%q: upstream count = %q, want default %d", ep.param, junk, got, ep.def)
				}
			}

			// Repeating the param is bounded either way round: url.Values.Get
			// answers with the first value, so order decides which one is
			// honoured, but neither order gets past the cap.
			if got := ask(t, with("99999")+"&"+ep.param+"=1"); got != strconv.Itoa(ep.upper) {
				t.Errorf("%s=99999&%s=1: upstream count = %q, want clamp to %d", ep.param, ep.param, got, ep.upper)
			}
			if got := ask(t, with("1")+"&"+ep.param+"=99999"); got != "1" {
				t.Errorf("%s=1&%s=99999: upstream count = %q, want the first value, 1", ep.param, ep.param, got)
			}
		})
	}
}

// 20. GET /work/recent?status= is filtered against the status vocabulary this
// file already maintains, de-duplicated, and therefore bounded.
//
// Until tether#146 the raw strings.Split of the query value went straight to
// aihub.Client.ListWorkItems, which joins it back into the upstream query
// string — so one request could make the daemon build an arbitrarily long
// upstream URL out of arbitrary status names. This is the sibling of the page
// size bound in TestWorkPaging_BoundedAndDefaulted: same endpoint, same "a
// caller-supplied value reaches aihub unchecked" shape, except the unbounded
// dimension here is set cardinality rather than a count.
func TestWorkRecentStatus_WhitelistedAndBounded(t *testing.T) {
	// The vocabulary the proxy is willing to forward: recentStatuses ∪
	// graphStatuses as declared in aihub_proxy.go, which is also exactly the
	// set aihub's own schema allows (0002_work_items.sql CHECK constraint), so
	// the filter rejects only values that were already meaningless upstream.
	//
	// Spelled out here rather than derived from the production values, because
	// it is the contract under test. Be exact about how much of a tripwire
	// that is, since it is less than it looks: *narrowing* either production
	// set fails below (whole_vocabulary), and widening recentStatuses fails
	// below (defaultCSV), but widening graphStatuses with some status not
	// named anywhere in this test would pass — the invariants are all upper
	// bounds on a set the table only ever populates from `known`. The
	// vocabulary_is_closed row covers the plausible candidates; beyond those,
	// keeping this list in step with the two production sets is a manual job.
	known := []string{"queued", "running", "blocked", "paused", "wrapped", "cancelled", "failed"}
	isKnown := map[string]bool{}
	for _, s := range known {
		isKnown[s] = true
	}
	// recentStatuses, in declaration order: what an absent/unusable ?status=
	// falls back to.
	const defaultCSV = "wrapped,cancelled,failed"

	var gotStatus string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotStatus = r.URL.Query().Get("status")
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/work_items" {
			_, _ = w.Write([]byte(`{"items":[],"next_cursor":null}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	mux := newTestMux(aihub.New(srv.URL, "k"))

	// askRaw issues GET /work/recent with an already-encoded query string and
	// returns the status CSV that reached upstream.
	askRaw := func(t *testing.T, label, rawQuery string) string {
		t.Helper()
		// Sentinel for the same reason as TestWorkPaging_BoundedAndDefaulted:
		// the stub only writes gotStatus when it is actually reached, so
		// without it a request answered without calling upstream would be
		// asserted against the *previous* request's value.
		gotStatus = "<upstream not called>"
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/work/recent?"+rawQuery, nil))
		// Unusable values keep this endpoint's pre-existing silent fallback
		// (?status= empty already fell back to the default) instead of
		// becoming a 400 — the same disposition PR #218 chose for page sizes
		// it could not use.
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: response = %d, want 200; body=%s", label, rec.Code, rec.Body.String())
		}
		return gotStatus
	}

	// ask issues GET /work/recent with the given raw ?status= value (nil =
	// param absent) and returns the status CSV that reached upstream.
	ask := func(t *testing.T, status *string) string {
		t.Helper()
		q := url.Values{}
		q.Set("project", "x")
		if status != nil {
			q.Set("status", *status)
		}
		return askRaw(t, "status="+derefOrAbsent(status), q.Encode())
	}

	// checkBounded asserts the invariants that must hold for every input in
	// this test, whatever the expected value is: what reaches aihub is always
	// a de-duplicated subset of the vocabulary, which is what actually bounds
	// the upstream URL. Only the first offender of each kind is reported — the
	// pathological cases carry 10k+ elements and one message per element would
	// bury the rest of the run.
	checkBounded := func(t *testing.T, got string) {
		t.Helper()
		seen := map[string]bool{}
		unknownCount, unknownFirst := 0, ""
		dupCount, dupFirst := 0, ""
		for _, s := range strings.Split(got, ",") {
			if !isKnown[s] {
				if unknownCount == 0 {
					unknownFirst = s
				}
				unknownCount++
			}
			if seen[s] {
				if dupCount == 0 {
					dupFirst = s
				}
				dupCount++
			}
			seen[s] = true
		}
		if unknownCount > 0 {
			t.Errorf("upstream status %q has %d out-of-vocabulary element(s), first %q",
				elide(got), unknownCount, elide(unknownFirst))
		}
		if dupCount > 0 {
			t.Errorf("upstream status %q has %d repeated element(s), first %q",
				elide(got), dupCount, elide(dupFirst))
		}
		if n := len(strings.Split(got, ",")); n > len(known) {
			t.Errorf("upstream status has %d elements, want at most %d", n, len(known))
		}
		if len(got) > len(strings.Join(known, ",")) {
			t.Errorf("upstream status is %d bytes, want at most %d", len(got), len(strings.Join(known, ",")))
		}
	}

	// Pathological inputs, built here so the table below stays readable.
	manyDupes := strings.Repeat("wrapped,", 10000) + "wrapped"
	manyUnknown := make([]string, 10000)
	for i := range manyUnknown {
		manyUnknown[i] = "bogus" + strconv.Itoa(i)
	}
	manyMixed := make([]string, 0, 20000)
	for i := 0; i < 10000; i++ {
		// Cycling through known in order means the surviving set comes out in
		// exactly the vocabulary's own order.
		manyMixed = append(manyMixed, known[i%len(known)], "junk"+strconv.Itoa(i))
	}

	for _, tc := range []struct {
		name string
		in   *string
		want string
	}{
		// Absent → the endpoint's own default vocabulary, unchanged behavior.
		{name: "absent", in: nil, want: defaultCSV},
		{name: "empty", in: ptr(""), want: defaultCSV},

		// A legal subset is forwarded verbatim, in the caller's order. The
		// filter must not narrow what callers are already allowed to ask for
		// — including statuses outside the /recent default (that is the whole
		// point of the override).
		{name: "one_legal", in: ptr("wrapped"), want: "wrapped"},
		{name: "legal_subset_keeps_order", in: ptr("failed,running"), want: "failed,running"},
		{name: "whole_vocabulary", in: ptr(strings.Join(known, ",")), want: strings.Join(known, ",")},

		// Unknown values are dropped and the request continues; if nothing
		// usable is left, it falls back to the default like an empty param.
		{name: "unknown_only", in: ptr("bogus"), want: defaultCSV},
		{name: "unknown_dropped_legal_kept", in: ptr("wrapped,bogus,failed"), want: "wrapped,failed"},
		{name: "unknown_smuggling_extra_param", in: ptr("wrapped,limit=999999"), want: "wrapped"},
		{name: "unknown_path_traversal_shape", in: ptr("../../v1/api_keys"), want: defaultCSV},

		// Status-shaped names that exist elsewhere in the tree but are not
		// work-item statuses: "in_progress" is a step status
		// (0005_step_state.sql), "done"/"current"/"pending" are step-graph
		// node states (wire.WorkStepNode), "archived" is a memory state
		// (0006_events_memories.sql). None may be forwarded — and if a future
		// edit adds one of them to graphStatuses, this row is what notices.
		{name: "vocabulary_is_closed", in: ptr("in_progress,done,current,pending,archived"), want: defaultCSV},

		// Not normalized, deliberately: PR #218 already treats " 7" as junk
		// that falls back rather than a 7 to be trimmed, so neither case nor
		// surrounding whitespace is fixed up here.
		{name: "wrong_case_is_unknown", in: ptr("WRAPPED"), want: defaultCSV},
		{name: "padded_is_unknown", in: ptr("wrapped, failed"), want: "wrapped"},

		// Empty elements are dropped rather than forwarded as empty statuses.
		{name: "empty_element", in: ptr("wrapped,,failed"), want: "wrapped,failed"},
		{name: "only_empty_elements", in: ptr(",,,"), want: defaultCSV},
		{name: "leading_trailing_commas", in: ptr(",wrapped,"), want: "wrapped"},

		// Duplicates collapse — without this the whitelist alone would not
		// bound cardinality, since a legal value may be repeated forever.
		{name: "adjacent_duplicate", in: ptr("wrapped,wrapped"), want: "wrapped"},
		{name: "non_adjacent_duplicate", in: ptr("wrapped,failed,wrapped"), want: "wrapped,failed"},

		// The cardinality bound itself: 10k elements in, at most one per
		// vocabulary entry out.
		{name: "10k_duplicates", in: ptr(manyDupes), want: "wrapped"},
		{name: "10k_unknown", in: ptr(strings.Join(manyUnknown, ",")), want: defaultCSV},
		{name: "20k_mixed", in: ptr(strings.Join(manyMixed, ",")), want: strings.Join(known, ",")},

		// One enormous element, rather than many: a 1 MiB status name must not
		// reach the upstream URL either.
		{name: "one_1mib_element", in: ptr(strings.Repeat("a", 1<<20)), want: defaultCSV},
		{name: "1mib_element_beside_legal", in: ptr("wrapped," + strings.Repeat("a", 1<<20)), want: "wrapped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ask(t, tc.in)
			if got != tc.want {
				t.Errorf("upstream status = %q, want %q", elide(got), tc.want)
			}

			checkBounded(t, got)
		})
	}

	// Repeating the whole param is bounded either way round: url.Values.Get
	// answers with the first value, so order decides which one is honoured,
	// but neither order gets an unknown status or an unbounded set upstream.
	// This goes through askRaw rather than ask because url.Values.Set cannot
	// express a repeated key, and through checkBounded so it is gated on the
	// same invariants as every table row above.
	t.Run("repeated_param", func(t *testing.T) {
		for _, tc := range []struct{ name, query, want string }{
			{name: "legal_first", query: "status=wrapped&status=" + strings.Repeat("bogus,", 5000) + "bogus", want: "wrapped"},
			{name: "junk_first", query: "status=" + strings.Repeat("bogus,", 5000) + "bogus&status=wrapped", want: defaultCSV},
		} {
			got := askRaw(t, tc.name, "project=x&"+tc.query)
			if got != tc.want {
				t.Errorf("%s: upstream status = %q, want %q", tc.name, elide(got), tc.want)
			}
			checkBounded(t, got)
		}
	})
}

func ptr(s string) *string { return &s }

func derefOrAbsent(s *string) string {
	if s == nil {
		return "<absent>"
	}
	return elide(*s)
}

// elide keeps failure messages readable when the input or the forwarded value
// is one of the pathological multi-kilobyte cases.
func elide(s string) string {
	const max = 120
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(" + strconv.Itoa(len(s)) + " bytes)"
}
