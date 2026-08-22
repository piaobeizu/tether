package workspace

// tether#156 — the workspace REST surface.
//
// Two defects that look unrelated and are both "a request is allowed to leave
// the daemon in a state nobody asked for":
//
//   - POST /api/v1/workspaces read an unbounded body into a json.Decoder, while
//     the sibling skill endpoints (and auth, and the MCP token routes) all cap
//     theirs;
//   - DELETE /api/v1/workspaces/{id} dropped the registration and left the
//     symlinks that registration had authorised on disk. Once the id is gone the
//     skill registry cannot resolve it, so the leftovers are not merely untidy —
//     nothing can reach them any more.
//
// Both tests were run against the unfixed tree and observed to fail there.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postWS(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestAddWorkspaceEndpoint_RefusesAnOversizedBody.
//
// The padding is in a field the handler ignores and every field it reads is
// valid, so the size limit is the only thing that can refuse this request:
// unfixed, the decoder skipped the padding and answered 201 with a registration.
func TestAddWorkspaceEndpoint_RefusesAnOversizedBody(t *testing.T) {
	reg := &Registry{path: t.TempDir() + "/workspaces.json"}
	mux := http.NewServeMux()
	RegisterAPI(mux, reg)

	dir := t.TempDir()
	body := `{"name":"w","path":"` + dir + `","pad":"` + strings.Repeat("A", 1<<20) + `"}`
	rec := postWS(t, mux, "/api/v1/workspaces", body)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /api/v1/workspaces with a %d-byte body -> %d, want 400; body %q",
			len(body), rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	if got := reg.List(); len(got) != 0 {
		t.Fatalf("an over-limit body still registered %+v", got)
	}
}

// TestAddWorkspaceEndpoint_StillAcceptsAnOrdinaryBody — the companion that keeps
// the cap from being satisfied by an endpoint that refuses everything.
func TestAddWorkspaceEndpoint_StillAcceptsAnOrdinaryBody(t *testing.T) {
	reg := &Registry{path: t.TempDir() + "/workspaces.json"}
	mux := http.NewServeMux()
	RegisterAPI(mux, reg)

	body, err := json.Marshal(map[string]string{"name": "w", "path": t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	rec := postWS(t, mux, "/api/v1/workspaces", string(body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST a normal body -> %d, want 201; body %q", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	if got := reg.List(); len(got) != 1 {
		t.Fatalf("List = %+v, want the one workspace", got)
	}
}

// TestRemove_DetachesTheOverlaysTheRegistrationAuthorised.
//
// The order is the assertion: the detach runs BEFORE the record disappears,
// because the thing that does the detaching resolves its workspace through this
// registry — run it afterwards and it is handed an id that no longer exists.
// Unfixed, the callback did not exist and the record simply vanished.
func TestRemove_DetachesTheOverlaysTheRegistrationAuthorised(t *testing.T) {
	reg := &Registry{path: t.TempDir() + "/workspaces.json"}
	ws, err := reg.Add("w", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	var calledWith []string
	var stillResolvable bool
	reg.BindOverlayCleanup(func(id string) error {
		calledWith = append(calledWith, id)
		_, stillResolvable = reg.Path(id)
		return nil
	})

	if err := reg.Remove(ws.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(calledWith) != 1 || calledWith[0] != ws.ID {
		t.Errorf("overlay cleanup called with %v, want exactly [%s]", calledWith, ws.ID)
	}
	if !stillResolvable {
		t.Errorf("the registration was already gone when the cleanup ran, so the cleanup could not "+
			"have resolved %s to a directory", ws.ID)
	}
	if got := reg.List(); len(got) != 0 {
		t.Errorf("List = %+v, want empty", got)
	}
}

// TestRemove_KeepsTheRecordWhenTheOverlaysCannotBeDetached.
//
// Fail closed. A 204 here is a promise that the registration and everything it
// authorised are both gone; if the second half did not happen the answer must not
// be 204, and the record has to stay so a retry can finish the job. The
// alternative — delete anyway — is the orphan this change exists to remove, with
// an error message on top.
func TestRemove_KeepsTheRecordWhenTheOverlaysCannotBeDetached(t *testing.T) {
	reg := &Registry{path: t.TempDir() + "/workspaces.json"}
	ws, err := reg.Add("w", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reg.BindOverlayCleanup(func(string) error { return errors.New("boom") })

	err = reg.Remove(ws.ID)
	if !errors.Is(err, ErrOverlayCleanup) {
		t.Fatalf("Remove with a failing cleanup: error = %v, want ErrOverlayCleanup", err)
	}
	if got := reg.List(); len(got) != 1 {
		t.Errorf("List = %+v, want the workspace still registered so a retry can finish", got)
	}
}

// TestRemove_AnUnknownIDIsStillANoOp — regression guard.
//
// Remove has always been silent about an id it does not hold, and the DELETE
// handler's 204 rests on that. The cleanup callback must not change it: there is
// no registration to unwind, so there is nothing to call and nothing to refuse.
func TestRemove_AnUnknownIDIsStillANoOp(t *testing.T) {
	reg := &Registry{path: t.TempDir() + "/workspaces.json"}
	if _, err := reg.Add("w", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	called := 0
	reg.BindOverlayCleanup(func(string) error { called++; return errors.New("must not run") })

	if err := reg.Remove("no-such-workspace"); err != nil {
		t.Errorf("Remove(unknown id) = %v, want nil", err)
	}
	if called != 0 {
		t.Errorf("overlay cleanup ran %d times for an id this registry does not hold, want 0", called)
	}
	if got := reg.List(); len(got) != 1 {
		t.Errorf("List = %+v, want the unrelated workspace untouched", got)
	}
}

// TestDeleteWorkspaceEndpoint_ReportsAFailedDetach — the wire half of the two
// tests above: a cleanup that could not run is a 409 (the filesystem is in a
// state the caller can see and fix), not a 204 and not a 500.
func TestDeleteWorkspaceEndpoint_ReportsAFailedDetach(t *testing.T) {
	reg := &Registry{path: t.TempDir() + "/workspaces.json"}
	ws, err := reg.Add("w", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reg.BindOverlayCleanup(func(string) error { return errors.New("boom") })

	mux := http.NewServeMux()
	RegisterAPI(mux, reg)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/workspaces/"+ws.ID, nil))

	if rec.Code != http.StatusConflict {
		t.Errorf("DELETE with a failing detach -> %d, want 409; body %q", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Errorf("the refusal echoed the underlying error %q, which can carry daemon-side paths",
			strings.TrimSpace(rec.Body.String()))
	}
}
