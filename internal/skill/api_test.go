package skill

// tether#142 — the HTTP surface of the overlay write path.
//
// These assert what a CALLER sees, which is where two of the fix's consequences
// live and cannot be observed from overlay_test.go: the request body no longer
// names a directory, and the three refusals no longer all say 500.

import (
	"encoding/json"
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
