package skill

// tether#142 — the HTTP surface of the overlay write path.
//
// These assert what a CALLER sees, which is where two of the fix's consequences
// live and cannot be observed from overlay_test.go: the request body no longer
// names a directory, and the three refusals no longer all say 500.

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

// serveSkills mounts the real RegisterAPI routes over a registry with one
// installed skill and one registered workspace.
func serveSkills(t *testing.T, bindIndex bool) (http.Handler, Skill, string) {
	t.Helper()
	sk, _ := installedSkill(t, "bbbb000000000001")
	wsDir := t.TempDir()

	var idx WorkspaceIndex
	if bindIndex {
		idx = fakeIndex{"ws1": wsDir}
	}
	reg := newTestRegistry(t, idx, sk)

	mux := http.NewServeMux()
	RegisterAPI(mux, reg)
	return mux, sk, wsDir
}

func post(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestOverlayEndpoints_RejectTheOldWorkspacePathBody.
//
// The pre-tether#142 wire shape was {"workspacePath":"<any host dir>"}. This
// replays it verbatim against both endpoints: the field is gone, so the decoded
// workspace id is empty and the request is refused before any filesystem call.
//
// It is the regression guard that matters most, because it is the exact request
// an attacker (or an old client) sends. Narrowing the wire is only a fix if the
// old shape cannot still get through.
//
// The BODY assertion is what makes this test about the rename. Checking only for
// 400 is not enough and a review proved it: revert the json tag to
// `workspacePath` and the request decodes, the outsider directory becomes the
// workspace id, and the registry lookup refuses it — also a 400. The assertion
// then holds in both states, which means it is not a gate. "bad request" is the
// handler refusing before it ever consults the registry, and only the un-renamed
// field produces it.
func TestOverlayEndpoints_RejectTheOldWorkspacePathBody(t *testing.T) {
	h, sk, _ := serveSkills(t, true)
	outsider := t.TempDir()
	body, err := json.Marshal(map[string]string{"workspacePath": outsider})
	if err != nil {
		t.Fatal(err)
	}

	for _, action := range []string{"enable", "disable"} {
		rec := post(t, h, "/api/v1/skills/"+sk.ID+"/"+action, string(body))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST %s with the old workspacePath body -> %d, want 400; body %q",
				action, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
		if got := strings.TrimSpace(rec.Body.String()); got != "bad request" {
			t.Errorf("POST %s with the old workspacePath body -> body %q, want exactly \"bad request\" "+
				"(anything else means the field was still decoded and the refusal came from somewhere later)", action, got)
		}
		if _, statErr := os.Lstat(filepath.Join(outsider, ".claude")); statErr == nil {
			t.Errorf("POST %s with the old body created something under %s", action, outsider)
		}
	}
}

// TestOverlayEndpoints_StatusCodes — the taxonomy. Every one of these was 500
// before, which told a caller "the daemon is broken" for three situations, two of
// which are the caller's own request being wrong.
//
// Each row asserts the BODY as well as the code, and that is not decoration. Two
// distinct refusals share the 400: "your request was malformed" and "that
// workspace is not registered". A row that checks only the code cannot tell which
// mechanism answered, so it stays green when the mechanism it is named for is
// deleted — a review found exactly that on two rows here.
func TestOverlayEndpoints_StatusCodes(t *testing.T) {
	for _, action := range []string{"enable", "disable"} {
		t.Run(action, func(t *testing.T) {
			h, sk, wsDir := serveSkills(t, true)

			// An ABSOLUTE directory that exists but is not registered. Using a
			// relative string here would be a weaker test than it looks: a
			// relative id is refused by the non-absolute check in workspaceDir,
			// so the row would stay green even if the registry lookup were
			// bypassed entirely. This is the shape of the actual tether#142
			// request — a real host path — moved into the new field name.
			outsider := t.TempDir()

			for _, tc := range []struct {
				name     string
				path     string
				body     string
				want     int
				wantBody string // substring; "" = no body assertion
				why      string
			}{
				{
					"registered workspace, installed skill",
					"/api/v1/skills/" + sk.ID + "/" + action,
					`{"workspaceId":"ws1"}`,
					http.StatusNoContent, "",
					"the happy path still works — otherwise the refusals below prove nothing",
				},
				{
					"unregistered workspace id",
					"/api/v1/skills/" + sk.ID + "/" + action,
					`{"workspaceId":"not-registered"}`,
					http.StatusBadRequest, "workspace is not registered",
					"the caller named a workspace this daemon does not have — and the body says so, which is what distinguishes it from a malformed request",
				},
				{
					"an absolute host path in the id field",
					"/api/v1/skills/" + sk.ID + "/" + action,
					`{"workspaceId":"` + outsider + `"}`,
					http.StatusBadRequest, "workspace is not registered",
					"the tether#142 request with the field renamed: a real directory is still not a workspace id, and this row (unlike the relative one above) can only pass via the registry lookup",
				},
				{
					"uninstalled skill id",
					"/api/v1/skills/nosuchskill/" + action,
					`{"workspaceId":"ws1"}`,
					http.StatusNotFound, "skill is not installed",
					"the id in the URL is not a skill; 404 is about the addressed resource",
				},
				{
					"empty workspace id",
					"/api/v1/skills/" + sk.ID + "/" + action,
					`{"workspaceId":""}`,
					http.StatusBadRequest, "bad request",
					"refused by the handler's own emptiness check BEFORE any lookup — the body is the only way to see that, since a lookup miss is also a 400",
				},
				{
					"malformed json",
					"/api/v1/skills/" + sk.ID + "/" + action,
					`{"workspaceId":`,
					http.StatusBadRequest, "bad request",
					"unchanged behaviour, pinned so the shared handler keeps it",
				},
			} {
				rec := post(t, h, tc.path, tc.body)
				if rec.Code != tc.want {
					t.Errorf("%s: POST %s %s -> %d, want %d (%s); body %q",
						tc.name, tc.path, tc.body, rec.Code, tc.want, tc.why, strings.TrimSpace(rec.Body.String()))
				}
				if tc.wantBody != "" && !strings.Contains(rec.Body.String(), tc.wantBody) {
					t.Errorf("%s: POST %s %s -> body %q, want it to contain %q (%s)",
						tc.name, tc.path, tc.body, strings.TrimSpace(rec.Body.String()), tc.wantBody, tc.why)
				}
			}

			// The happy path above must have actually written (enable) or been a
			// legitimate no-op (disable) inside the REGISTERED directory only.
			if action == "enable" {
				if _, err := os.Readlink(filepath.Join(wsDir, ".claude", "plugins", sk.ID)); err != nil {
					t.Errorf("enable returned 204 but left no link in the registered workspace: %v", err)
				}
			}
			// And nothing anywhere near the unregistered directory, whatever the
			// status codes said.
			if _, err := os.Lstat(filepath.Join(outsider, ".claude")); err == nil {
				t.Errorf("%s wrote into the unregistered directory %s", action, outsider)
			}
		})
	}
}

// TestOverlayEndpoints_RefuseAnOversizedBody pins the http.MaxBytesReader in
// overlayWrite, which review found had no gate at all — deleting it left the
// whole suite green.
//
// The payload is a workspace id; 4 KiB is already far more than one needs. The
// body below is a VALID request except for a megabyte of padding in a field the
// handler ignores, so the only thing that can refuse it is the size limit: remove
// the limit and the decoder skips the padding, reads a registered workspaceId and
// answers 204.
func TestOverlayEndpoints_RefuseAnOversizedBody(t *testing.T) {
	for _, action := range []string{"enable", "disable"} {
		t.Run(action, func(t *testing.T) {
			h, sk, wsDir := serveSkills(t, true)

			body := `{"workspaceId":"ws1","pad":"` + strings.Repeat("A", 1<<20) + `"}`
			rec := post(t, h, "/api/v1/skills/"+sk.ID+"/"+action, body)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("POST %s with a %d-byte body -> %d, want 400; body %q",
					action, len(body), rec.Code, strings.TrimSpace(rec.Body.String()))
			}
			// And it was refused rather than partially applied.
			if _, err := os.Lstat(filepath.Join(wsDir, ".claude", "plugins", sk.ID)); err == nil {
				t.Fatalf("%s acted on an over-limit body: the overlay link exists", action)
			}
		})
	}
}

// TestOverlayEndpoints_UnavailableWithoutAWorkspaceIndex.
//
// 503 rather than 500: the daemon is not broken, it is unable — its workspace
// registry did not load, so it cannot say whether any directory is a workspace.
// Distinguishing the two is the same argument workspace/state.go makes for the
// chat handshake's two error codes.
func TestOverlayEndpoints_UnavailableWithoutAWorkspaceIndex(t *testing.T) {
	h, sk, _ := serveSkills(t, false) // index deliberately not bound

	for _, action := range []string{"enable", "disable"} {
		rec := post(t, h, "/api/v1/skills/"+sk.ID+"/"+action, `{"workspaceId":"ws1"}`)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("POST %s with no workspace index -> %d, want 503; body %q",
				action, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
	}
}

// -- tether#156: the HTTP surface --------------------------------------------

// TestSkillEndpoints_RefuseAnOversizedInstallBody.
//
// tether#142 bounded the enable/disable body and left the install handler on the
// same route file unbounded, which is the more attractive of the two: it is a
// POST that takes a string the daemon then stores. Same limit, same reason —
// without one an authenticated request streams unbounded input into a
// json.Decoder.
//
// The padding sits in a field the handler ignores and every field it reads is
// valid, so nothing but the size limit can refuse this: unfixed, the decoder
// skipped the padding and answered 201 with a new skill.
func TestSkillEndpoints_RefuseAnOversizedInstallBody(t *testing.T) {
	reg := newTestRegistry(t, fakeIndex{"ws1": t.TempDir()})
	mux := http.NewServeMux()
	RegisterAPI(mux, reg)

	src := t.TempDir()
	body := `{"name":"s","sourcePath":"` + src + `","pad":"` + strings.Repeat("A", 1<<20) + `"}`
	rec := post(t, mux, "/api/v1/skills", body)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST install with a %d-byte body -> %d, want 400; body %q",
			len(body), rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	if got := reg.List(); len(got) != 0 {
		t.Fatalf("an over-limit install body still registered %+v", got)
	}
}

// TestSkillEndpoints_DeleteAnUnknownIDIs404 — tether#156 fact 5, at the wire.
//
// DELETE answered 204 for an id that is not installed while enable and disable
// answered 404 for the same id, and nothing asserted either way: the existing
// DELETE test only ever removes an id it just created. 404 is the answer the rest
// of this route file already gives, for the reason its own table states — the id
// in the URL is not a skill, and 404 is about the addressed resource.
//
// Unfixed: 204, indistinguishable from a delete that did something.
func TestSkillEndpoints_DeleteAnUnknownIDIs404(t *testing.T) {
	h, sk, _ := serveSkills(t, true)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/skills/not-installed", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("DELETE an uninstalled id -> %d, want 404; body %q", rec.Code, strings.TrimSpace(rec.Body.String()))
	}

	// The installed id still answers 204 — the refusal has to be about the id,
	// not about the endpoint.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/skills/"+sk.ID, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("DELETE an installed id -> %d, want 204; body %q", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}

// TestOverlayEndpoints_ConflictWhenTheLocationIsUnusable.
//
// Both rows describe the filesystem the caller can see and can fix, which is
// what 409 says and what 500 ("this daemon is broken") does not. Unfixed, the
// first row was a 500 whose body was `symlink <source> <full daemon path>: file
// exists` and the second was a 204 that had just written outside the workspace.
func TestOverlayEndpoints_ConflictWhenTheLocationIsUnusable(t *testing.T) {
	t.Run("the overlay name is occupied", func(t *testing.T) {
		h, sk, wsDir := serveSkills(t, true)
		occupied := filepath.Join(wsDir, ".claude", "plugins", sk.ID)
		if err := os.MkdirAll(occupied, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(occupied, "x.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}

		rec := post(t, h, "/api/v1/skills/"+sk.ID+"/enable", `{"workspaceId":"ws1"}`)
		if rec.Code != http.StatusConflict {
			t.Errorf("POST enable onto an occupied name -> %d, want 409; body %q", rec.Code, strings.TrimSpace(rec.Body.String()))
		}
	})

	t.Run("the overlay path leaves the workspace", func(t *testing.T) {
		h, sk, wsDir := serveSkills(t, true)
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(wsDir, ".claude")); err != nil {
			t.Fatal(err)
		}

		for _, action := range []string{"enable", "disable"} {
			rec := post(t, h, "/api/v1/skills/"+sk.ID+"/"+action, `{"workspaceId":"ws1"}`)
			if rec.Code != http.StatusConflict {
				t.Errorf("POST %s through a symlinked .claude -> %d, want 409; body %q",
					action, rec.Code, strings.TrimSpace(rec.Body.String()))
			}
		}
		if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
			t.Errorf("the directory outside the workspace holds %d entries (err %v), want 0", len(entries), err)
		}
	})
}

// TestOverlayEndpoints_DoNotEchoDaemonSideValues.
//
// Two separate leaks, both of them values the CALLER did not send:
//
//   - the non-absolute refusal quoted the registry's STORED path back at the
//     requester, who supplied an id;
//   - every unclassified failure answered 500 with err.Error(), which is how a
//     `mkdir /some/daemon/path: ...` ends up in an HTTP body.
//
// Low severity on its own — an authenticated caller can read the workspace list —
// but a stable, value-free refusal is also the one that does not change shape
// when the daemon's internals do. The detail belongs in the daemon's log.
func TestOverlayEndpoints_DoNotEchoDaemonSideValues(t *testing.T) {
	t.Run("a registered but non-absolute path", func(t *testing.T) {
		sk, _ := installedSkill(t, "bbbb000000000002")
		stored := "some/where/on/the/daemon"
		reg := newTestRegistry(t, fakeIndex{"ws1": stored}, sk)
		mux := http.NewServeMux()
		RegisterAPI(mux, reg)

		rec := post(t, mux, "/api/v1/skills/"+sk.ID+"/enable", `{"workspaceId":"ws1"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("-> %d, want 400; body %q", rec.Code, strings.TrimSpace(rec.Body.String()))
		}
		if strings.Contains(rec.Body.String(), stored) {
			t.Errorf("the refusal echoed the registry's stored path %q back to a caller that sent an id: %q",
				stored, strings.TrimSpace(rec.Body.String()))
		}
	})

	t.Run("an unclassified failure", func(t *testing.T) {
		// A hand-edited registry entry is the one route to overlayRefusal's default
		// branch a test can arrange: it is documented as a 500 precisely because it
		// means this daemon's own skills.json is wrong.
		//
		// %2e%2e rather than a literal "..": ServeMux cleans the ESCAPED path while
		// the handler reads the DECODED one, which is the routing quirk
		// ErrUnsafeSkillID's own doc describes, and it is what delivers an id of
		// ".." to a handler at all.
		reg := newTestRegistry(t, fakeIndex{"ws1": t.TempDir()},
			Skill{ID: "..", Name: "hand-edited", SourcePath: t.TempDir()})
		mux := http.NewServeMux()
		RegisterAPI(mux, reg)

		rec := post(t, mux, "/api/v1/skills/%2e%2e/enable", `{"workspaceId":"ws1"}`)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("-> %d, want 500; body %q", rec.Code, strings.TrimSpace(rec.Body.String()))
		}
		if got := strings.TrimSpace(rec.Body.String()); got != overlayInternalErrorBody {
			t.Errorf("500 body = %q, want exactly %q — a 500 must not carry err.Error(), which is "+
				"where daemon-side paths reach a client", got, overlayInternalErrorBody)
		}
	})
}

// TestInstallEndpoint_RefusesASourceThatIsNotAnExistingDirectory — tether#147 at
// the wire, and the half of it that overlay_test.go cannot see.
//
// Three assertions, three distinct pre-fix defects:
//
//   - 400, not 201 (the file row) and not 500 (the missing row). A caller that
//     sent a bad path gets an answer it can act on.
//   - the registry is still empty. "400 but installed anyway" would pass a
//     status-only test.
//   - the body is exactly the sentinel. Pre-fix the missing row answered
//     `skill path not found: stat <path>: no such file or directory` — the stat
//     error verbatim, which both named the path and distinguished "absent" from
//     "permission denied", making the endpoint a filesystem probe.
func TestInstallEndpoint_RefusesASourceThatIsNotAnExistingDirectory(t *testing.T) {
	for _, tc := range []struct {
		name     string
		build    func(t *testing.T, base string) string
		wantBody string
	}{
		{"does not exist", func(t *testing.T, base string) string {
			return filepath.Join(base, "not-there")
		}, ErrSkillSourceUnusable.Error()},
		{"exists but is a regular file", func(t *testing.T, base string) string {
			p := filepath.Join(base, "skill.md")
			if err := os.WriteFile(p, []byte("# not a skill dir"), 0o600); err != nil {
				t.Fatal(err)
			}
			return p
		}, ErrSkillSourceUnusable.Error()},
		{"a relative path", func(t *testing.T, base string) string {
			if err := os.Mkdir(filepath.Join(base, "a-skill"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Chdir(base)
			return "a-skill" // exists, so only the IsAbs check can refuse it
		}, ErrSkillSourceNotAbsolute.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := newTestRegistry(t, fakeIndex{"ws1": t.TempDir()})
			mux := http.NewServeMux()
			RegisterAPI(mux, reg)

			src := tc.build(t, t.TempDir())
			body, err := json.Marshal(map[string]string{"name": "s", "sourcePath": src})
			if err != nil {
				t.Fatal(err)
			}
			rec := post(t, mux, "/api/v1/skills", string(body))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("POST install %q -> %d, want 400; body %q",
					src, rec.Code, strings.TrimSpace(rec.Body.String()))
			}
			if got := reg.List(); len(got) != 0 {
				t.Errorf("a refused install still registered %+v, want an empty registry", got)
			}
			if _, statErr := os.Stat(reg.path); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("a refused install wrote %s (stat error %v), want no file at all",
					reg.path, statErr)
			}
			if got := strings.TrimSpace(rec.Body.String()); got != tc.wantBody {
				t.Errorf("400 body = %q, want exactly %q", got, tc.wantBody)
			}
			if strings.Contains(rec.Body.String(), src) {
				t.Errorf("the refusal quoted the path back, so the body came from err.Error() "+
					"rather than from the sentinel: %q", strings.TrimSpace(rec.Body.String()))
			}
		})
	}
}

// TestSkillEndpoints_ListInstallRemoveUnaffected — regression guard. The three
// endpoints the SPA actually calls are outside this change; folding the two
// overlay handlers together must not have disturbed the routing around them.
func TestSkillEndpoints_ListInstallRemoveUnaffected(t *testing.T) {
	reg := newTestRegistry(t, fakeIndex{"ws1": t.TempDir()})
	mux := http.NewServeMux()
	RegisterAPI(mux, reg)

	// list
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/skills", nil))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("GET list -> %d %q, want 200 []", rec.Code, strings.TrimSpace(rec.Body.String()))
	}

	// install, from an arbitrary local directory — still allowed on purpose; see
	// the residual-gap note in Registry.Install's caller (the Settings UI lets a
	// user type any path, so narrowing sourcePath is a product decision, not a
	// containment fix that can be smuggled in here).
	src := t.TempDir()
	body, err := json.Marshal(map[string]string{"name": "s", "sourcePath": src})
	if err != nil {
		t.Fatal(err)
	}
	rec = post(t, mux, "/api/v1/skills", string(body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST install -> %d %q, want 201", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	var created Skill
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	// remove
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/skills/"+created.ID, nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE -> %d, want 204", rec.Code)
	}
	if got := reg.List(); len(got) != 0 {
		t.Fatalf("after DELETE, List = %+v, want empty", got)
	}
}
