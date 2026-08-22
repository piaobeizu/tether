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
	"os"
	"path/filepath"
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

// -- tether#147: what POST will accept, and what a refusal says --------------
//
// All three were run against the pre-fix tree and observed to fail there.

// TestAddWorkspaceEndpoint_RefusesAPathItCannotUse — the wire half of
// TestAdd_RefusesAPathThatIsNotAnExistingDirectory and
// TestAdd_RefusesARelativePath.
//
// Three assertions per row, and they are three different defects:
//
//   - the status is 400 (pre-fix: 201);
//   - the registry is still empty (pre-fix: one entry — and "400 but written
//     anyway" would pass a status-only test);
//   - the body does not contain the path the request sent. That one is not about
//     leaking the caller's own input back to it, which is harmless; it is the
//     assertion that the body comes from the SENTINEL rather than from
//     err.Error(). Add's error wraps the CANONICALISED path, so a body built from
//     err.Error() carries a daemon-side resolution of the caller's string — and
//     the next error to carry a path would leak that too.
func TestAddWorkspaceEndpoint_RefusesAPathItCannotUse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		path     func(t *testing.T) string
		wantBody string
	}{
		{"a directory that does not exist", func(t *testing.T) string {
			return filepath.Join(t.TempDir(), "not-created-yet")
		}, ErrWorkspacePathUnusable.Error()},
		{"a regular file", func(t *testing.T) string {
			p := filepath.Join(t.TempDir(), "a-file")
			if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			return p
		}, ErrWorkspacePathUnusable.Error()},
		{"a relative path", func(t *testing.T) string {
			parent := t.TempDir()
			if err := os.Mkdir(filepath.Join(parent, "child"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Chdir(parent)
			return "child" // exists, so only the IsAbs check can refuse it
		}, ErrWorkspacePathNotAbsolute.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := &Registry{path: filepath.Join(t.TempDir(), "workspaces.json")}
			mux := http.NewServeMux()
			RegisterAPI(mux, reg)

			path := tc.path(t)
			body, err := json.Marshal(map[string]string{"name": "w", "path": path})
			if err != nil {
				t.Fatal(err)
			}
			rec := postWS(t, mux, "/api/v1/workspaces", string(body))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("POST %q -> %d, want 400; body %q\nPre-fix: 201 with a registration.",
					path, rec.Code, strings.TrimSpace(rec.Body.String()))
			}
			if got := reg.List(); len(got) != 0 {
				t.Errorf("a refused POST still registered %+v, want an empty registry", got)
			}
			if got := strings.TrimSpace(rec.Body.String()); got != tc.wantBody {
				t.Errorf("400 body = %q, want exactly %q", got, tc.wantBody)
			}
			if strings.Contains(rec.Body.String(), path) {
				t.Errorf("the refusal quoted the path %q, so the body came from err.Error() "+
					"rather than from the sentinel: %q", path, strings.TrimSpace(rec.Body.String()))
			}
		})
	}
}

// TestRegistryMutations_DoNotEchoDaemonSideValues — the error-body convergence
// (tether#147). The skill route file took this rule in tether#156 and this one was
// left behind: both mutation handlers sent err.Error() as a 500 body.
//
// A registry whose file lives under a directory that does not exist is the one
// route to a 500 a test can arrange without mocking: saveLocked's os.WriteFile
// fails with an *os.PathError naming the daemon's own path. Pre-fix, that path was
// the response body verbatim.
//
// # What this does NOT cover, named because the obvious name for it would lie
//
// "RegistryMutations", not "WorkspaceEndpoints": POST and DELETE are the two
// handlers that mutate the registry and the two this change converged. The three
// READ handlers in the same route file — /files, /file and /tree — still build
// bodies with err.Error(), and at least one of them leaks a daemon-side absolute
// path today (`?dir=<a regular file>` reaches os.ReadDir and answers 500 with
// `open <abs path>: not a directory`, because builtin.SafeJoin checks containment
// but not that the target is a directory). That is a pre-existing defect on a
// different set of handlers, it needs a decision about what /files should answer
// for a non-directory, and it is tracked as tether#159 rather than folded in
// here. A test called "WorkspaceEndpoints_..." would be read as covering it.
func TestRegistryMutations_DoNotEchoDaemonSideValues(t *testing.T) {
	// A path component that is distinctive enough that finding it in a body cannot
	// be a coincidence.
	unwritable := func(t *testing.T) string {
		t.Helper()
		return filepath.Join(t.TempDir(), "no-such-directory-4f2a", "workspaces.json")
	}

	t.Run("POST", func(t *testing.T) {
		file := unwritable(t)
		reg := &Registry{path: file}
		mux := http.NewServeMux()
		RegisterAPI(mux, reg)

		body, err := json.Marshal(map[string]string{"name": "w", "path": t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		rec := postWS(t, mux, "/api/v1/workspaces", string(body))

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("POST with an unwritable registry -> %d, want 500; body %q",
				rec.Code, strings.TrimSpace(rec.Body.String()))
		}
		assertNoDaemonPath(t, rec, file)
	})

	t.Run("DELETE", func(t *testing.T) {
		reg := &Registry{path: filepath.Join(t.TempDir(), "workspaces.json")}
		ws, err := reg.Add("w", t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		// Only now does the registry become unwritable, so the entry above is real.
		file := unwritable(t)
		reg.path = file

		mux := http.NewServeMux()
		RegisterAPI(mux, reg)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/workspaces/"+ws.ID, nil))

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("DELETE with an unwritable registry -> %d, want 500; body %q",
				rec.Code, strings.TrimSpace(rec.Body.String()))
		}
		assertNoDaemonPath(t, rec, file)
	})
}

// assertNoDaemonPath is the shared half of the two rows above: a 500 says exactly
// registryInternalErrorBody and nothing else. Asserting equality rather than
// "does not contain the path" is deliberate — an equality check also refuses the
// next error whose text happens not to include a path this test knows to look
// for.
func assertNoDaemonPath(t *testing.T, rec *httptest.ResponseRecorder, file string) {
	t.Helper()
	got := strings.TrimSpace(rec.Body.String())
	if got != registryInternalErrorBody {
		t.Errorf("500 body = %q, want exactly %q — a 500 must not carry err.Error(), which is "+
			"where daemon-side paths reach a client", got, registryInternalErrorBody)
	}
	if strings.Contains(rec.Body.String(), filepath.Dir(file)) {
		t.Errorf("the 500 body names the daemon's own registry directory: %q", got)
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
