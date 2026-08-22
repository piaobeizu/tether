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
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/piaobeizu/tether/internal/mcp/builtin"
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
// READ handlers in the same route file — /files, /file and /tree — were left
// sending err.Error(), one of them leaking a daemon-side absolute path
// (`?dir=<a regular file>` reached os.ReadDir and answered 500 with `open <abs
// path>: not a directory`, because builtin.SafeJoin checked containment but not
// that the target was a directory). tether#159 closed that, and the gate for it
// is TestWorkspaceReads_DoNotEchoDaemonSideValues below — a separate test,
// because it covers a separate set of handlers through a separate refusal map
// (refuseRead/readRefusal, next to this one's refuse/registryRefusal). The name
// here stays as it is: it says which half it holds.
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

// -- tether#159: what the three READ handlers say when they refuse -----------

// TestWorkspaceReads_DoNotEchoDaemonSideValues — the other half of
// TestRegistryMutations_DoNotEchoDaemonSideValues, for /files and /file.
//
// Row 1 is the live leak this wi exists for and was observed to FAIL on c278802:
// `?dir=<a regular file>` answered 500 with `open /tmp/.../a.txt: not a
// directory`. Every other row was already refused before this change; they are
// here because the refusal TEXT is what changed, and because a fix that turned
// one of them into a 500 (or into a 404, unregistering the deliberate 404
// mapping) would be a regression the leak test alone would not catch.
//
// # Three assertions per row, and the exact-equality one is the gate
//
// The status is checked, and then the body is checked for EQUALITY with the
// expected refusal — not for absence of the path. "Does not contain <path>" is
// satisfied by any wording that happens not to mention the one path a test
// thought to look for, and every one of these bodies used to be assembled from
// err.Error(), where the next error to carry a path leaks it. Equality is the
// assertion that the body came from the refusal's IDENTITY. The containment check
// is kept as well, on the daemon-side workspace root, purely so a failure reads
// as "it leaked the path" rather than "the string differs".
//
// # Row 1 is NOT the gate on where the fix went, and the mutation battery says so
//
// The non-directory refusal has two independent layers — SafeJoinDir's stat, and
// readRefusal's syscall.ENOTDIR arm, which catches os.ReadDir's own errno if the
// first is gone — so removing EITHER leaves row 1 green. It fails on c278802,
// where neither exists, and it is the right end-to-end assertion for the leak;
// it is just not a discriminating one. Measured, not assumed: deleting
// SafeJoinDir's IsDir check leaves this whole test passing and kills only
// builtin.TestSafeJoinDir_RefusesANonDirectory, which is therefore the gate on
// the source-layer half, and builtin.TestSafeJoin_StillAcceptsARegularFile is the
// gate on that half not having been put one layer lower.
func TestWorkspaceReads_DoNotEchoDaemonSideValues(t *testing.T) {
	// The registry stores a canonicalised path, so resolve the fixture the same
	// way before using it as the needle: on a host where TMPDIR is itself a
	// symlink, an unresolved root appears in no body and the containment check
	// would pass without measuring anything.
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// outside.txt must EXIST for the traversal rows to reach the escape branch: a
	// traversal to a target that is not there fails in EvalSymlinks first and is
	// indistinguishable from a plain missing file, i.e. a 404 (the same fixture
	// design TestFileHandler_TraversalPath400 explains).
	root := filepath.Join(parent, "ws")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "outside.txt"), []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	reg, id := newTestRegistry(t, root)
	mux := http.NewServeMux()
	RegisterAPI(mux, reg)

	for _, tc := range []struct {
		name     string
		url      string
		wantCode int
		wantBody string
	}{
		{
			"files: dir names a regular file",
			"/files?dir=a.txt",
			http.StatusBadRequest, readNotADirectoryBody,
		},
		{
			// The same mistake spelled differently: EvalSymlinks fails before any
			// stat, with ENOTDIR, and must still arrive as the same refusal.
			"files: dir names something under a regular file",
			"/files?dir=a.txt/b",
			http.StatusBadRequest, readNotADirectoryBody,
		},
		{
			"files: dir leaves the workspace",
			"/files?dir=..",
			http.StatusBadRequest, readOutsideWorkspaceBody,
		},
		{
			"files: dir is absolute",
			"/files?dir=/etc",
			http.StatusBadRequest, readMustBeRelativeBody,
		},
		{
			// Deliberate and pre-existing: a well-formed path whose target is not
			// there is a 404, and tether#159 was not allowed to change it.
			"files: dir does not exist",
			"/files?dir=nope",
			http.StatusNotFound, "404 page not found",
		},
		{
			"file: path names a directory",
			"/file?path=sub",
			http.StatusBadRequest, ErrPathIsDirectory.Error(),
		},
		{
			"file: path names something under a regular file",
			"/file?path=a.txt/b",
			http.StatusBadRequest, readNotADirectoryBody,
		},
		{
			"file: path leaves the workspace",
			"/file?path=../outside.txt",
			http.StatusBadRequest, readOutsideWorkspaceBody,
		},
		{
			"file: path does not exist",
			"/file?path=nope",
			http.StatusNotFound, "404 page not found",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := "/api/v1/workspaces/" + id + tc.url
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))

			if rec.Code != tc.wantCode {
				t.Errorf("GET %s -> %d, want %d; body %q", tc.url, rec.Code, tc.wantCode,
					strings.TrimSpace(rec.Body.String()))
			}
			if got := strings.TrimSpace(rec.Body.String()); got != tc.wantBody {
				t.Errorf("GET %s body = %q, want exactly %q — the body must come from the "+
					"sentinel, not from err.Error()", tc.url, got, tc.wantBody)
			}
			if strings.Contains(rec.Body.String(), root) {
				t.Errorf("GET %s named the daemon's own workspace root: %q", tc.url,
					strings.TrimSpace(rec.Body.String()))
			}
		})
	}

	// The companion that keeps every row above from being satisfied by handlers
	// that refuse everything.
	if got := getFiles(t, mux, id, "sub"); len(got) != 0 {
		t.Errorf("GET /files?dir=sub = %+v, want an empty listing of the real directory", got)
	}
}

// TestReadRefusal_EveryRefusalItCannotNameIsAn500WithNoDetail — the map itself,
// including the case no request can reach.
//
// The default arm is the one that matters and the one no handler test can drive:
// an *fs.PathError from os.ReadDir, os.Open or a read is what carried the
// daemon's absolute path, and on this host every such failure is unreachable
// because the tests run as root — chmod cannot make a directory unreadable, and
// the only other route (a target that vanishes between the stat and the read) is
// a race, not a fixture. So it is asserted here directly rather than left to a
// handler test that would silently be covering nothing (tether#159).
func TestReadRefusal_EveryRefusalItCannotNameIsAn500WithNoDetail(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		wantCode int
		wantBody string
	}{
		{"missing target", fs.ErrNotExist, http.StatusNotFound, ""},
		{"wrapped missing target", fmt.Errorf("stat /home/u/ws/x: %w", fs.ErrNotExist),
			http.StatusNotFound, ""},
		{"absolute", builtin.ErrAbsolutePath, http.StatusBadRequest, readMustBeRelativeBody},
		{"escapes", builtin.ErrPathEscapesRoot, http.StatusBadRequest, readOutsideWorkspaceBody},
		{"not a directory", builtin.ErrNotDirectory, http.StatusBadRequest, readNotADirectoryBody},
		{
			// What /file gets for `a.txt/b`: it resolves through plain SafeJoin, so
			// EvalSymlinks' errno arrives with no sentinel around it.
			"bare ENOTDIR", syscall.ENOTDIR, http.StatusBadRequest, readNotADirectoryBody,
		},
		{"is a directory", ErrPathIsDirectory, http.StatusBadRequest, ErrPathIsDirectory.Error()},
		{
			// The shape of every leak this wi closed.
			"an unclassified path error",
			&fs.PathError{Op: "open", Path: "/home/u/ws/secret", Err: errors.New("permission denied")},
			http.StatusInternalServerError, registryInternalErrorBody,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := readRefusal(tc.err)
			if code != tc.wantCode || body != tc.wantBody {
				t.Errorf("readRefusal(%v) = (%d, %q), want (%d, %q)", tc.err, code, body,
					tc.wantCode, tc.wantBody)
			}
			if strings.Contains(body, "/home/u/ws") {
				t.Errorf("readRefusal built its body from the error value: %q", body)
			}
		})
	}
}
