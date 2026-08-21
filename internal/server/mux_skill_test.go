package server

// tether#142 — the WIRING between the skill overlay and the workspace registry.
//
// internal/skill/overlay_test.go proves the containment rule works when a
// WorkspaceIndex is bound. Nothing in that package can prove the daemon binds
// one. That hop is a single hand-written line in buildMux, and a containment rule
// with nothing connected to it is worse than no rule: every unit test stays green
// while every live request is refused with 503 (or, before the fix, accepted with
// 204 and a symlink outside every workspace).
//
// So these run against buildMux itself, with a REAL workspace.Registry and a REAL
// auth credential — the same reason activity_api_test.go does.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/piaobeizu/tether/internal/auth"
	mcplifecycle "github.com/piaobeizu/tether/internal/mcp/lifecycle"
	"github.com/piaobeizu/tether/internal/session"
	"github.com/piaobeizu/tether/internal/skill"
	"github.com/piaobeizu/tether/internal/workspace"
)

// skillRouteMux builds the daemon's real route table with a skill registry, and
// optionally a workspace registry holding one registered workspace.
//
// HOME is redirected to a temp dir before either registry is constructed: both
// resolve their files under ~/.tether, and a test that writes to the operator's
// real ~/.tether/skills.json would be editing live daemon state.
func skillRouteMux(t *testing.T, withWorkspace bool) (http.Handler, func(method, path, body string) *http.Request, skill.Skill, workspace.Workspace) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	skReg, err := skill.NewRegistry()
	if err != nil {
		t.Fatalf("skill registry: %v", err)
	}
	sk, err := skReg.Install("wiring", t.TempDir())
	if err != nil {
		t.Fatalf("install a skill: %v", err)
	}

	reg := session.NewRegistry()
	reg.History = session.NewHistoryStore(t.TempDir())
	cfg := &Config{Port: 0, Registry: reg, MCPLifecycle: mcplifecycle.New(), SkillRegistry: skReg}

	var ws workspace.Workspace
	if withWorkspace {
		wsReg, err := workspace.NewRegistry()
		if err != nil {
			t.Fatalf("workspace registry: %v", err)
		}
		ws, err = wsReg.Add("wired", t.TempDir())
		if err != nil {
			t.Fatalf("add a workspace: %v", err)
		}
		cfg.WsRegistry = wsReg
	}

	secret := []byte("test-secret-for-the-skill-wiring")
	authState := auth.NewState("test-token", secret)
	tok, err := auth.IssueJWT(secret, "skill-wiring-test")
	if err != nil {
		t.Fatalf("mint a session cookie: %v", err)
	}
	mux := buildMux(cfg, newCertHolder(mustGenCert(t)), nil, reg, nil, authState, nil, nil, nil, cfg.MCPLifecycle)

	return mux, func(method, path, body string) *http.Request {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: tok})
		return r
	}, sk, ws
}

// TestSkillOverlay_IsWiredToTheWorkspaceRegistry.
//
// The assertion that catches an unwired containment rule: a workspace id that IS
// registered must be accepted and must produce the link inside that workspace's
// own directory. Delete the BindWorkspaces line from buildMux and this answers
// 503 — which is exactly the failure a units-only suite cannot see.
func TestSkillOverlay_IsWiredToTheWorkspaceRegistry(t *testing.T) {
	mux, req, sk, ws := skillRouteMux(t, true)

	body, err := json.Marshal(map[string]string{"workspaceId": ws.ID})
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req(http.MethodPost, "/api/v1/skills/"+sk.ID+"/enable", string(body)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST enable {workspaceId:%q} -> %d, want 204; body %q",
			ws.ID, rec.Code, strings.TrimSpace(rec.Body.String()))
	}

	link := filepath.Join(ws.Path, ".claude", "plugins", sk.ID)
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("expected a link at %s: %v", link, err)
	}
	if target != sk.SourcePath {
		t.Fatalf("link -> %q, want the skill source %q", target, sk.SourcePath)
	}

	// And disable, through the same route table, takes it back out.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req(http.MethodPost, "/api/v1/skills/"+sk.ID+"/disable", string(body)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST disable -> %d, want 204; body %q", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	if _, err := os.Lstat(link); err == nil {
		t.Fatalf("link survived disable: %s", link)
	}
}

// TestSkillOverlay_RefusesAnUnregisteredWorkspaceEndToEnd — the other side of the
// same wiring. If the index were bound to something that vouches for everything,
// the test above would still pass; this one would not.
//
// Both rows matter and the second is the load-bearing one. A relative id like
// "not-a-registered-id" is refused by workspaceDir's non-absolute check, so that
// row stays green even if the registry lookup is bypassed altogether — it tests a
// different guard than the name suggests. An ABSOLUTE, existing directory can
// only be refused by the lookup, and it is also the literal tether#142 request.
func TestSkillOverlay_RefusesAnUnregisteredWorkspaceEndToEnd(t *testing.T) {
	mux, req, sk, _ := skillRouteMux(t, true)
	outsider := t.TempDir()

	for _, tc := range []struct{ name, id string }{
		{"a relative id (caught by the non-absolute check)", "not-a-registered-id"},
		{"an absolute host directory (only the registry lookup can refuse this)", outsider},
	} {
		rec := httptest.NewRecorder()
		body, err := json.Marshal(map[string]string{"workspaceId": tc.id})
		if err != nil {
			t.Fatal(err)
		}
		mux.ServeHTTP(rec, req(http.MethodPost, "/api/v1/skills/"+sk.ID+"/enable", string(body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: POST enable {workspaceId:%q} -> %d, want 400; body %q",
				tc.name, tc.id, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
	}
	if _, err := os.Lstat(filepath.Join(outsider, ".claude")); err == nil {
		t.Fatalf("an overlay was created under the unregistered directory %s", outsider)
	}
}

// TestSkillOverlay_RejectsTheOldUnvalidatedPathBodyEndToEnd replays the tether#142
// request through the real route table: an authenticated POST naming an arbitrary
// host directory. Before the fix this answered 204 and created
// `<that dir>/.claude/plugins/<id>` pointing at the skill source.
func TestSkillOverlay_RejectsTheOldUnvalidatedPathBodyEndToEnd(t *testing.T) {
	mux, req, sk, _ := skillRouteMux(t, true)

	outsider := t.TempDir()
	body, err := json.Marshal(map[string]string{"workspacePath": outsider})
	if err != nil {
		t.Fatal(err)
	}

	for _, action := range []string{"enable", "disable"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req(http.MethodPost, "/api/v1/skills/"+sk.ID+"/"+action, string(body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST %s {workspacePath:...} -> %d, want 400; body %q",
				action, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
		// The body, not just the code: with the json tag reverted to
		// `workspacePath` this request decodes and is refused by the registry
		// lookup instead — also a 400. Only "bad request" distinguishes "the field
		// no longer exists" from "the field exists and named nothing real", so
		// without this line the test passes in both states and gates nothing.
		if got := strings.TrimSpace(rec.Body.String()); got != "bad request" {
			t.Errorf("POST %s {workspacePath:...} -> body %q, want exactly \"bad request\"", action, got)
		}
	}
	if _, err := os.Lstat(filepath.Join(outsider, ".claude")); err == nil {
		t.Fatalf("the daemon created an overlay under a directory no registry vouched for: %s", outsider)
	}
}

// TestSkillOverlay_WithNoWorkspaceRegistryRefusesRatherThanPanics.
//
// A daemon whose workspaces.json is corrupt leaves cfg.WsRegistry nil
// (lifecycle.go Step 2b). Two things must hold: it answers 503, and it does NOT
// panic — the nil *workspace.Registry-in-an-interface trap lifecycle.go documents
// would make this a nil-receiver call on a mutex inside an HTTP handler.
func TestSkillOverlay_WithNoWorkspaceRegistryRefusesRatherThanPanics(t *testing.T) {
	mux, req, sk, _ := skillRouteMux(t, false)

	for _, action := range []string{"enable", "disable"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req(http.MethodPost, "/api/v1/skills/"+sk.ID+"/"+action, `{"workspaceId":"anything"}`))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("POST %s with no workspace registry -> %d, want 503; body %q",
				action, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
	}
}

// TestSkillOverlay_IsBehindTheAuthMiddleware. The whole threat model of tether#142
// is "what an authenticated caller can do", which is only the right question if
// these routes are in fact authenticated — mux.go's comment claims the group is,
// and this is the assertion rather than the claim.
func TestSkillOverlay_IsBehindTheAuthMiddleware(t *testing.T) {
	mux, _, sk, ws := skillRouteMux(t, true)

	body, err := json.Marshal(map[string]string{"workspaceId": ws.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		"/api/v1/skills",
		"/api/v1/skills/" + sk.ID + "/enable",
		"/api/v1/skills/" + sk.ID + "/disable",
	} {
		r := httptest.NewRequest(http.MethodPost, p, strings.NewReader(string(body)))
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("POST %s with no cookie -> %d, want 401", p, rec.Code)
		}
	}
}
