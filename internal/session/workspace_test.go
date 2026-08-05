package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/wire"
)

// Session-id fixtures shaped like the real thing (uuid v4, as cc mints), because
// ValidSessionID — the guard BindingStore and the /api/v1/sessions route share —
// bounds length and alphabet. A short "sid-a" would be rejected in the test while
// every production id passes, which is a double that lies about its subject.
const (
	sidFixtureA       = "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa"
	sidFixtureOffline = "cccccccc-3333-4333-8333-cccccccccccc"
)

// fakeLookup is a WorkspaceLookup over a literal map — the whole point of
// declaring the interface in this package rather than importing
// internal/workspace is that "the user's registry" is two lines in a test.
type fakeLookup map[string]string

func (f fakeLookup) Path(id string) (string, bool) {
	p, ok := f[id]
	return p, ok
}

// newBoundRegistry builds a registry with a workspace lookup and an on-disk
// binding store under t.TempDir(), i.e. the production wiring minus $HOME.
func newBoundRegistry(t *testing.T, p agent.AgentProvider, ws fakeLookup) *Registry {
	t.Helper()
	reg := NewRegistry(p)
	reg.Workspaces = ws
	reg.Bindings = NewBindingStore(t.TempDir())
	return reg
}

// ─── BindingStore ───────────────────────────────────────────────────────────

// TestBindingStore_RoundTripSurvivesANewStore is the reason this is a FILE and
// not a map. A reconnect sends a sid and no workspace, so after a daemon restart
// the file is the only thing that knows where that session lives; a second store
// over the same directory stands in for the restarted daemon.
func TestBindingStore_RoundTripSurvivesANewStore(t *testing.T) {
	dir := t.TempDir()
	want := WorkspaceBinding{WorkspaceID: "731367a619c4dff3", Path: "/srv/project-a"}

	if err := NewBindingStore(dir).Save(sidFixtureA, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, ok := NewBindingStore(dir).Load(sidFixtureA)
	if !ok {
		t.Fatal("Load = not found from a fresh store; the binding did not survive, so a restarted daemon would reconnect into the wrong directory")
	}
	if got != want {
		t.Errorf("Load = %+v, want %+v", got, want)
	}
}

// TestBindingStore_AbsentIsNotAnError — no binding is the ordinary case (every
// session created before tether#52, and every session that selected no
// workspace), so it must read as "nothing remembered" rather than a failure.
func TestBindingStore_AbsentIsNotAnError(t *testing.T) {
	if b, ok := NewBindingStore(t.TempDir()).Load("never-saved"); ok {
		t.Errorf("Load = (%+v, true) for a sid with no binding, want not-found", b)
	}
}

// TestBindingStore_RefusesTraversalSID — the sid on the reconnect path comes from
// the client, and both Load and Save join it into a filesystem path. A
// `..`-shaped id must not read or write outside the sessions directory: a
// workspace.json found somewhere unintended would be read back as "this session's
// directory", which is the caller-supplied-path hole the whole design closes,
// reintroduced through the store.
func TestBindingStore_RefusesTraversalSID(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A binding that a traversal would reach if the guard were missing.
	if err := os.WriteFile(filepath.Join(outside, "workspace.json"),
		[]byte(`{"workspaceId":"planted","path":"/etc"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	s := NewBindingStore(filepath.Join(base, "sessions"))

	for _, sid := range []string{"../outside", "..", ".", "", "a/b", `a\b`} {
		if b, ok := s.Load(sid); ok {
			t.Errorf("Load(%q) = (%+v, true); a path-shaped sid must not resolve to a binding", sid, b)
		}
		if err := s.Save(sid, WorkspaceBinding{WorkspaceID: "x", Path: "/tmp"}); err == nil {
			t.Errorf("Save(%q) = nil error; a path-shaped sid must be refused, not written", sid)
		}
	}
	// The planted file is untouched. Asserting on its CONTENTS, not on the absence
	// of a temp file: a Save that escaped would rename its temp over this file, so
	// "no .tmp left behind" is exactly what a successful attack also looks like.
	planted, err := os.ReadFile(filepath.Join(outside, "workspace.json"))
	if err != nil {
		t.Fatalf("read planted file: %v", err)
	}
	if string(planted) != `{"workspaceId":"planted","path":"/etc"}` {
		t.Errorf("a path-shaped sid overwrote a file outside the sessions directory: %s", planted)
	}
}

// TestBindingStore_CorruptOrPathlessFileIsIgnored — a truncated or hand-edited
// file must degrade to "nothing remembered" (which lands the session in the
// daemon default and lets tether#50's fallback recover it), never to a binding
// with an empty path that would pin an agent to "".
func TestBindingStore_CorruptOrPathlessFileIsIgnored(t *testing.T) {
	dir := t.TempDir()
	s := NewBindingStore(dir)
	for sid, body := range map[string]string{
		"corrupt":  `{"workspaceId":"a","pa`,
		"pathless": `{"workspaceId":"a"}`,
	} {
		if err := os.MkdirAll(filepath.Join(dir, sid), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, sid, "workspace.json"), []byte(body), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if b, ok := s.Load(sid); ok {
			t.Errorf("Load(%q) = (%+v, true), want not-found", sid, b)
		}
	}
}

// ─── the id → path gate ─────────────────────────────────────────────────────

// TestAttach_UnknownWorkspaceRejectedBeforeAnySpawn is the security assertion.
// An id that is not in the user's registry must cost NOTHING: no subprocess in
// any directory, and no silent fall back to --workspace-root, which would turn a
// rejected request into a redirected one.
func TestAttach_UnknownWorkspaceRejectedBeforeAnySpawn(t *testing.T) {
	p := &pinningProvider{}
	reg := newBoundRegistry(t, p, fakeLookup{"known": "/srv/known"})
	reg.Workdir = "/daemon/default"

	att, err := reg.Attach(context.Background(), "", "fake", "forged-id")
	if err == nil {
		t.Fatal("Attach with an unregistered workspace id returned no error; the daemon would run the agent in a directory the request chose")
	}
	if att != nil {
		t.Error("Attach returned an Attachment alongside its error")
	}
	if got := p.Spawns(); got != 0 {
		t.Fatalf("spawns = %d, want 0 — the refusal must happen BEFORE the spawn, not be cleaned up after it", got)
	}
}

// TestAttach_WorkspaceRequestWithoutARegistryIsRefused — a daemon whose workspace
// registry failed to load cannot resolve any id, so it must say so. Substituting
// its own default here is the same silent redirect as accepting a forged id.
func TestAttach_WorkspaceRequestWithoutARegistryIsRefused(t *testing.T) {
	p := &pinningProvider{}
	reg := NewRegistry(p) // Workspaces deliberately nil
	reg.Workdir = "/daemon/default"

	if _, err := reg.Attach(context.Background(), "", "fake", "any-id"); err == nil {
		t.Fatal("Attach honoured a workspace id with no registry to check it against")
	}
	if got := p.Spawns(); got != 0 {
		t.Errorf("spawns = %d, want 0", got)
	}
}

// TestAttach_UnknownWorkspaceVsNoRegistry_DistinctCodes pins the hard
// requirement from resolveWorkspace's doc comment (the nil-registry branch is
// "kept a separate branch rather than folded into a deny-everything
// WorkspaceLookup, because the two states deserve different operator-facing
// errors"): tether#63 gives each its own wire.ErrorCode, and this test makes
// collapsing them back into one a failing test rather than a silent
// regression. See wire.TestUnknownWorkspaceVsNoRegistryDiffer for the
// wire-level half of the same guarantee (that the two consts themselves are
// distinct).
func TestAttach_UnknownWorkspaceVsNoRegistry_DistinctCodes(t *testing.T) {
	codeOf := func(t *testing.T, err error) wire.ErrorCode {
		t.Helper()
		var ref *Refusal
		if !errors.As(err, &ref) {
			t.Fatalf("error %v (%T) is not a *Refusal", err, err)
		}
		return ref.Code
	}

	p := &pinningProvider{}
	registered := newBoundRegistry(t, p, fakeLookup{"known": "/srv/known"})
	registered.Workdir = "/daemon/default"
	_, unknownErr := registered.Attach(context.Background(), "", "fake", "forged-id")
	if unknownErr == nil {
		t.Fatal("Attach with an unregistered workspace id returned no error")
	}

	noRegistry := NewRegistry(p) // Workspaces deliberately nil
	noRegistry.Workdir = "/daemon/default"
	_, noRegErr := noRegistry.Attach(context.Background(), "", "fake", "any-id")
	if noRegErr == nil {
		t.Fatal("Attach with no workspace registry returned no error")
	}

	unknownCode := codeOf(t, unknownErr)
	noRegCode := codeOf(t, noRegErr)

	if unknownCode != wire.ErrCodeUnknownWorkspace {
		t.Errorf("unknown-id code = %q, want %q", unknownCode, wire.ErrCodeUnknownWorkspace)
	}
	if noRegCode != wire.ErrCodeNoWorkspaceRegistry {
		t.Errorf("no-registry code = %q, want %q", noRegCode, wire.ErrCodeNoWorkspaceRegistry)
	}
	if unknownCode == noRegCode {
		t.Fatalf("both paths produced the same code %q — an operator chasing a deleted workspace and one chasing a registry that failed to load at startup must be able to tell those apart", unknownCode)
	}
}

// TestAttach_KnownWorkspaceBecomesTheAgentCwd — the feature itself.
func TestAttach_KnownWorkspaceBecomesTheAgentCwd(t *testing.T) {
	p := &pinningProvider{}
	reg := newBoundRegistry(t, p, fakeLookup{"ws-a": "/srv/project-a"})
	reg.Workdir = "/daemon/default"

	if _, err := reg.Attach(context.Background(), "", "fake", "ws-a"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if got := p.LastCfg().Workdir; got != "/srv/project-a" {
		t.Errorf("SpawnConfig.Workdir = %q, want %q (the selected workspace, not the daemon default)", got, "/srv/project-a")
	}
}

// TestAttach_NoWorkspaceKeepsTheDaemonDefault — backward compatibility. A client
// that sends no `ws` must behave exactly as it did before tether#52.
func TestAttach_NoWorkspaceKeepsTheDaemonDefault(t *testing.T) {
	p := &pinningProvider{}
	reg := newBoundRegistry(t, p, fakeLookup{"ws-a": "/srv/project-a"})
	reg.Workdir = "/daemon/default"

	if _, err := reg.Attach(context.Background(), "", "fake", ""); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if got := p.LastCfg().Workdir; got != "/daemon/default" {
		t.Errorf("SpawnConfig.Workdir = %q, want the daemon default %q", got, "/daemon/default")
	}
}

// ─── workspace is part of session identity ──────────────────────────────────

// TestAttach_RememberedWorkspaceHonouredWhenClientSendsNone is the MAIN
// reconnect path, not a fallback: the browser sends `ws` only when it is starting
// a new session (web/src/lib/chatUrl.ts), so on every reconnect the remembered
// binding is the only thing that knows where to resume. If the daemon used its
// default here, cc would `--resume` in a directory the conversation was never
// created in and the context would be lost every time.
func TestAttach_RememberedWorkspaceHonouredWhenClientSendsNone(t *testing.T) {
	dir := t.TempDir()
	if err := NewBindingStore(dir).Save(sidFixtureA,
		WorkspaceBinding{WorkspaceID: "ws-a", Path: "/srv/project-a"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	p := &pinningProvider{}
	reg := NewRegistry(p)
	reg.Workspaces = fakeLookup{"ws-a": "/srv/project-a"}
	reg.Bindings = NewBindingStore(dir) // a "restarted" daemon: nothing in memory
	reg.Workdir = "/daemon/default"

	if _, err := reg.Attach(context.Background(), sidFixtureA, "fake", ""); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	cfg := p.LastCfg()
	if cfg.Workdir != "/srv/project-a" {
		t.Errorf("SpawnConfig.Workdir = %q, want the session's OWN workspace %q", cfg.Workdir, "/srv/project-a")
	}
	if cfg.ResumeSessionID != sidFixtureA {
		t.Errorf("ResumeSessionID = %q, want \"sid-a\" — agreeing on the workspace must still resume", cfg.ResumeSessionID)
	}
}

// TestAttach_MatchingWorkspaceStillResumes — a client that DOES send the sid's own
// workspace must be indistinguishable from one that sends none.
func TestAttach_MatchingWorkspaceStillResumes(t *testing.T) {
	dir := t.TempDir()
	if err := NewBindingStore(dir).Save(sidFixtureA,
		WorkspaceBinding{WorkspaceID: "ws-a", Path: "/srv/project-a"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	p := &pinningProvider{}
	reg := NewRegistry(p)
	reg.Workspaces = fakeLookup{"ws-a": "/srv/project-a"}
	reg.Bindings = NewBindingStore(dir)

	if _, err := reg.Attach(context.Background(), sidFixtureA, "fake", "ws-a"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	cfg := p.LastCfg()
	if cfg.ResumeSessionID != sidFixtureA || cfg.Workdir != "/srv/project-a" {
		t.Errorf("cfg = {Resume:%q Workdir:%q}, want {sid-a /srv/project-a}", cfg.ResumeSessionID, cfg.Workdir)
	}
}

// TestAttach_UnrememberedSidWithNoWorkspaceResumesInTheDefault — row 2. Every
// session created before tether#52, and every session that selected no workspace,
// reconnects like this. It must keep the pre-tether#52 behaviour exactly: the
// daemon default is the one directory whose `--resume` can succeed for such a
// session.
func TestAttach_UnrememberedSidWithNoWorkspaceResumesInTheDefault(t *testing.T) {
	p := &pinningProvider{}
	reg := newBoundRegistry(t, p, fakeLookup{"ws-a": "/srv/project-a"})
	reg.Workdir = "/daemon/default"

	if _, err := reg.Attach(context.Background(), sidFixtureA, "fake", ""); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	cfg := p.LastCfg()
	if cfg.ResumeSessionID != sidFixtureA {
		t.Errorf("ResumeSessionID = %q, want %q — a pre-#52 session must still be resumed", cfg.ResumeSessionID, sidFixtureA)
	}
	if cfg.Workdir != "/daemon/default" {
		t.Errorf("Workdir = %q, want the daemon default %q", cfg.Workdir, "/daemon/default")
	}
}

// TestAttach_UnrememberedSidUnderAWorkspaceIsNotResumedThere — row 3, and the
// bug an earlier version of this slice shipped.
//
// A sid the daemon remembers NOTHING about, presented under a workspace, used to
// be resumed in that workspace: `cc --resume <sid>` in a directory the session had
// never lived in — which the whole design says never happens — and, worse,
// spawnEntry then RECORDED the requested workspace under that client-supplied sid.
// That permanently rebound a session which had actually run in the daemon default:
// every later reconnect would resume into the wrong place and fail, and the shell
// pane would follow it there. A recoverable session became unrecoverable.
//
// Absent evidence is not evidence: no resume, and nothing written under that sid.
func TestAttach_UnrememberedSidUnderAWorkspaceIsNotResumedThere(t *testing.T) {
	p := &pinningProvider{}
	dir := t.TempDir()
	reg := NewRegistry(p)
	reg.Workspaces = fakeLookup{"ws-b": "/srv/project-b"}
	reg.Bindings = NewBindingStore(dir)
	reg.Workdir = "/daemon/default"

	if _, err := reg.Attach(context.Background(), sidFixtureA, "fake", "ws-b"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	cfg := p.LastCfg()
	if cfg.ResumeSessionID != "" {
		t.Errorf("ResumeSessionID = %q, want \"\" — resuming a session in a directory it was never in is what row 3 exists to stop", cfg.ResumeSessionID)
	}
	if cfg.Workdir != "/srv/project-b" {
		t.Errorf("Workdir = %q, want the requested workspace %q", cfg.Workdir, "/srv/project-b")
	}
	if b, ok := NewBindingStore(dir).Load(sidFixtureA); ok {
		t.Errorf("a binding was written under the CLIENT's sid (%+v); that permanently rebinds a session the daemon never created there", b)
	}
	// The replacement session's own id IS recorded — it is the one we created.
	if _, ok := NewBindingStore(dir).Load(cfg.SessionID); !ok {
		t.Error("no binding under the fresh session's minted id; its own reconnect would miss it")
	}
}

// TestAttach_SidFromAnotherWorkspaceStartsAFreshSessionThere — resolveWorkspace
// row 5, and the constraint it exists for: a sid presented under a DIFFERENT
// workspace must never be run in that workspace's directory. cc would fail such a
// resume deterministically (mem_2ruSlrHR ③/④), so the sid is dropped and a fresh
// session starts in the requested workspace instead.
func TestAttach_SidFromAnotherWorkspaceStartsAFreshSessionThere(t *testing.T) {
	dir := t.TempDir()
	if err := NewBindingStore(dir).Save(sidFixtureA,
		WorkspaceBinding{WorkspaceID: "ws-a", Path: "/srv/project-a"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	p := &pinningProvider{}
	reg := NewRegistry(p)
	reg.Workspaces = fakeLookup{"ws-a": "/srv/project-a", "ws-b": "/srv/project-b"}
	reg.Bindings = NewBindingStore(dir)
	reg.Workdir = "/daemon/default"

	if _, err := reg.Attach(context.Background(), sidFixtureA, "fake", "ws-b"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	cfg := p.LastCfg()
	if cfg.ResumeSessionID != "" {
		t.Errorf("ResumeSessionID = %q, want \"\" — resuming a session from another workspace runs it in the wrong directory", cfg.ResumeSessionID)
	}
	if cfg.SessionID == "" {
		t.Error("SessionID = \"\"; the replacement session must pin a fresh minted id so it is itself resumable")
	}
	if cfg.SessionID == sidFixtureA {
		t.Error("the dropped sid was reused as the fresh session's id")
	}
	if cfg.Workdir != "/srv/project-b" {
		t.Errorf("Workdir = %q, want the REQUESTED workspace %q", cfg.Workdir, "/srv/project-b")
	}
}

// TestAttach_ReboundSessionIsReportedAsARecovery — having dropped the sid, the
// daemon must SAY so. Otherwise the browser adopts a new sid and refetches an
// empty transcript with no explanation, which is the "where did my conversation
// go" failure tether#50's notice was added to prevent.
func TestAttach_ReboundSessionIsReportedAsARecovery(t *testing.T) {
	dir := t.TempDir()
	if err := NewBindingStore(dir).Save(sidFixtureA,
		WorkspaceBinding{WorkspaceID: "ws-a", Path: "/srv/project-a"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	p := &pinningProvider{}
	reg := NewRegistry(p)
	reg.Workspaces = fakeLookup{"ws-a": "/srv/project-a", "ws-b": "/srv/project-b"}
	reg.Bindings = NewBindingStore(dir)
	// History present AND non-empty for the dropped sid, so the notice gate opens.
	reg.History = NewHistoryStore(t.TempDir())
	reg.History.RecordUser(sidFixtureA, "the conversation being left behind")

	att, err := reg.Attach(context.Background(), sidFixtureA, "fake", "ws-b")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	res, err := att.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !res.Recovered {
		t.Error("Recovered = false; a rebound session is a new session and the browser must replace its sid")
	}
	if !res.Notice {
		t.Error("Notice = false; the dropped session HAD a transcript, so the user must be told it did not come with them")
	}
	// Rebound is what picks the WORDS. A failed resume means the conversation is
	// gone; a rebind means it is intact in another workspace, so serveChat must not
	// tell this user their context "could not be restored" — see wt_chat.go.
	if !res.Rebound {
		t.Error("Rebound = false; serveChat would then use the failed-resume wording and tell the user their conversation was lost when it is safe in the other workspace")
	}
	if res.SID == sidFixtureA {
		t.Errorf("Resolution.SID = %q, want the fresh session's id", res.SID)
	}
}

// TestAttach_ReboundWithNoHistoryStaysQuiet — the notice is only honest when there
// was a conversation to lose (HistoryStore.HasHistory). A rebind of a sid that
// never said anything must not cry wolf.
func TestAttach_ReboundWithNoHistoryStaysQuiet(t *testing.T) {
	dir := t.TempDir()
	if err := NewBindingStore(dir).Save(sidFixtureA,
		WorkspaceBinding{WorkspaceID: "ws-a", Path: "/srv/project-a"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	p := &pinningProvider{}
	reg := NewRegistry(p)
	reg.Workspaces = fakeLookup{"ws-a": "/srv/project-a", "ws-b": "/srv/project-b"}
	reg.Bindings = NewBindingStore(dir)
	reg.History = NewHistoryStore(t.TempDir())

	att, err := reg.Attach(context.Background(), sidFixtureA, "fake", "ws-b")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	res, err := att.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !res.Recovered {
		t.Error("Recovered = false; the sid still changed")
	}
	if res.Notice {
		t.Error("Notice = true for a dropped session with no transcript")
	}
}

// TestAttach_LiveSessionReusedWhenTheWorkspaceAgrees — the tether#54/#55 reuse
// path must survive tether#52: a reconnect to a live session in its own workspace
// still gets that session, not a second one.
func TestAttach_LiveSessionReusedWhenTheWorkspaceAgrees(t *testing.T) {
	p := &pinningProvider{}
	reg := newBoundRegistry(t, p, fakeLookup{"ws-a": "/srv/project-a"})
	reg.Workdir = "/daemon/default"

	first, err := reg.Attach(context.Background(), "", "fake", "ws-a")
	if err != nil {
		t.Fatalf("Attach #1: %v", err)
	}
	sid := p.LastCfg().SessionID

	second, err := reg.Attach(context.Background(), sid, "fake", "")
	if err != nil {
		t.Fatalf("Attach #2: %v", err)
	}
	if got := p.Spawns(); got != 1 {
		t.Fatalf("spawns = %d, want 1 — the second attach must REUSE the live session, not duplicate it", got)
	}
	if first.entry != second.entry {
		t.Error("the second attach bound to a different Entry than the live one")
	}
}

// TestAttach_LiveSessionInAnotherDirectoryIsLeftAlone — the corner
// resolveWorkspace cannot rule out: with neither a workspace nor
// --workspace-root, a live entry's workdir is "" and so is its remembered
// binding, yet a connection may still ask for a real workspace. Reusing that
// entry would put this connection in a directory it did not ask for; `--resume`ing
// a still-live sid would duplicate the session tether#54 made findable. Neither:
// leave it alone and start fresh.
func TestAttach_LiveSessionInAnotherDirectoryIsLeftAlone(t *testing.T) {
	p := &pinningProvider{}
	reg := newBoundRegistry(t, p, fakeLookup{"ws-b": "/srv/project-b"})
	// Workdir deliberately unset: this is the only way a live entry can have an
	// empty workdir and therefore no derivable workspace.

	if _, err := reg.Attach(context.Background(), "", "fake", ""); err != nil {
		t.Fatalf("Attach #1: %v", err)
	}
	sid := p.LastCfg().SessionID

	if _, err := reg.Attach(context.Background(), sid, "fake", "ws-b"); err != nil {
		t.Fatalf("Attach #2: %v", err)
	}
	if got := p.Spawns(); got != 2 {
		t.Fatalf("spawns = %d, want 2 (the live session in another directory must not be reused)", got)
	}
	cfg := p.LastCfg()
	if cfg.ResumeSessionID != "" {
		t.Errorf("ResumeSessionID = %q, want \"\" — a second --resume of a LIVE sid duplicates the session", cfg.ResumeSessionID)
	}
	if cfg.Workdir != "/srv/project-b" {
		t.Errorf("Workdir = %q, want %q", cfg.Workdir, "/srv/project-b")
	}
	// The live entry is still the one registered under its own sid.
	if _, ok := reg.liveEntry(sid); !ok {
		t.Error("the pre-existing live session was un-registered; it must be left running and reachable")
	}
}

// ─── remembering ────────────────────────────────────────────────────────────

// TestAttach_RecordsTheBindingUnderTheSessionsOwnID — written at spawn under the
// registration key, i.e. the id the client will reconnect with.
func TestAttach_RecordsTheBindingUnderTheSessionsOwnID(t *testing.T) {
	p := &pinningProvider{}
	dir := t.TempDir()
	reg := NewRegistry(p)
	reg.Workspaces = fakeLookup{"ws-a": "/srv/project-a"}
	reg.Bindings = NewBindingStore(dir)

	if _, err := reg.Attach(context.Background(), "", "fake", "ws-a"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	sid := p.LastCfg().SessionID
	got, ok := NewBindingStore(dir).Load(sid)
	if !ok {
		t.Fatal("no binding recorded; a reconnect after a daemon restart would land in the default directory")
	}
	want := WorkspaceBinding{WorkspaceID: "ws-a", Path: "/srv/project-a"}
	if got != want {
		t.Errorf("binding = %+v, want %+v", got, want)
	}
}

// TestAttach_DefaultWorkspaceIsNotRecorded — a session that selected nothing must
// leave no file. An absent binding already means "the daemon default"; writing
// today's default down would PIN it across a restart that changed
// --workspace-root, which is strictly worse than nothing.
func TestAttach_DefaultWorkspaceIsNotRecorded(t *testing.T) {
	p := &pinningProvider{}
	dir := t.TempDir()
	reg := NewRegistry(p)
	reg.Bindings = NewBindingStore(dir)
	reg.Workdir = "/daemon/default"

	if _, err := reg.Attach(context.Background(), "", "fake", ""); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if b, ok := NewBindingStore(dir).Load(p.LastCfg().SessionID); ok {
		t.Errorf("binding = %+v, want none for a session that selected no workspace", b)
	}
}

// TestRekey_MovesTheBindingToTheAnnouncedID — a provider that mints its own id
// (opencode) is registered under the pinned one until it announces. Without
// moving the binding, it would be filed under an id nobody ever asks about and
// every reconnect of that session would miss it.
func TestRekey_MovesTheBindingToTheAnnouncedID(t *testing.T) {
	fs := &fakeSession{sid: "minted-by-provider", events: make(chan agent.Event, 4)}
	p := &fakeProvider{sess: fs}
	dir := t.TempDir()
	reg := NewRegistry(p)
	reg.Workspaces = fakeLookup{"ws-a": "/srv/project-a"}
	reg.Bindings = NewBindingStore(dir)

	if _, err := reg.Attach(context.Background(), "", "fake", "ws-a"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	fs.announceInit()
	waitForRegistered(t, reg, "minted-by-provider")

	// Polled, not asserted immediately: rekey writes the binding AFTER releasing
	// Registry.mu (holding the registry-wide lock across file I/O is what liveEntry
	// and rekey both deliberately avoid), so the registration this test waited for
	// is observable slightly before the file is. That ordering is the product's, not
	// the test's — a reconnect landing inside that window simply finds no binding and
	// falls back to the daemon default.
	var got WorkspaceBinding
	deadline := time.Now().Add(2 * time.Second)
	for {
		var ok bool
		if got, ok = NewBindingStore(dir).Load("minted-by-provider"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no binding under the announced id; a reconnect with that id would miss it")
		}
		time.Sleep(time.Millisecond)
	}
	if got.Path != "/srv/project-a" {
		t.Errorf("binding path = %q, want %q", got.Path, "/srv/project-a")
	}
}

// TestFailedResumeFallback_StaysInTheSameWorkspace — recovering a conversation
// must not MOVE it. Spawning the replacement in the daemon default would relocate
// the user's session as the price of restoring it, and every later reconnect
// would then resume it there, making the move permanent and invisible.
func TestFailedResumeFallback_StaysInTheSameWorkspace(t *testing.T) {
	dead := newDeadSession()
	live := &fakeSession{sid: "recovered-sid", events: make(chan agent.Event, 8)}
	dp := &deadThenLiveProvider{dead: dead, live: live}
	dir := t.TempDir()
	reg := NewRegistry(dp)
	reg.Workspaces = fakeLookup{"ws-a": "/srv/project-a"}
	reg.Bindings = NewBindingStore(dir)
	reg.Workdir = "/daemon/default"

	// The sid must be one the daemon REMEMBERS in ws-a, or there is no resume to
	// fail: a sid with no recorded workspace presented under one is rebound to a
	// fresh session instead (resolveWorkspace row 3), which is a different path.
	if err := reg.Bindings.Save("gone-sid",
		WorkspaceBinding{WorkspaceID: "ws-a", Path: "/srv/project-a"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	att, err := reg.Attach(context.Background(), "gone-sid", "fake", "ws-a")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	_ = att.SendPrompt(context.Background(), "remember ALPHA")
	if _, err := att.Resolve(context.Background()); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	cfgs := dp.Configs()
	if len(cfgs) != 2 {
		t.Fatalf("spawns = %d, want 2 (the resume attempt, then the fallback)", len(cfgs))
	}
	if cfgs[0].Workdir != "/srv/project-a" {
		t.Errorf("resume spawn Workdir = %q, want %q", cfgs[0].Workdir, "/srv/project-a")
	}
	if cfgs[1].Workdir != "/srv/project-a" {
		t.Errorf("fallback spawn Workdir = %q, want %q — the replacement must stay in the same workspace", cfgs[1].Workdir, "/srv/project-a")
	}
}

// ─── WorkdirForSession (the shell pane's question) ──────────────────────────

// TestWorkdirForSession prefers the live registration, then the file, then the
// daemon default — the order the PTY shell pane needs to land in the same
// directory as the chat session it is about to `--resume`.
func TestWorkdirForSession(t *testing.T) {
	p := &pinningProvider{}
	dir := t.TempDir()
	reg := NewRegistry(p)
	reg.Workspaces = fakeLookup{"ws-a": "/srv/project-a"}
	reg.Bindings = NewBindingStore(dir)
	reg.Workdir = "/daemon/default"

	// (1) live session in a workspace.
	if _, err := reg.Attach(context.Background(), "", "fake", "ws-a"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	liveSID := p.LastCfg().SessionID
	if got := reg.WorkdirForSession(liveSID); got != "/srv/project-a" {
		t.Errorf("live session: WorkdirForSession = %q, want %q", got, "/srv/project-a")
	}

	// (2) no live entry, binding on disk only (a restarted daemon).
	if err := reg.Bindings.Save(sidFixtureOffline,
		WorkspaceBinding{WorkspaceID: "ws-a", Path: "/srv/project-a"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := reg.WorkdirForSession(sidFixtureOffline); got != "/srv/project-a" {
		t.Errorf("offline session: WorkdirForSession = %q, want %q", got, "/srv/project-a")
	}

	// (3) unknown sid, and (4) no sid at all → the daemon default. A shell opened
	// before any chat session exists is ordinary, not an error.
	for _, sid := range []string{"never-heard-of-it", ""} {
		if got := reg.WorkdirForSession(sid); got != "/daemon/default" {
			t.Errorf("WorkdirForSession(%q) = %q, want the daemon default %q", sid, got, "/daemon/default")
		}
	}
}

// TestWorkdirForSession_DoesNotEvictADeadSession — it asks the map directly rather
// than through liveEntry, which un-registers what it finds dead. The shell pane
// asking where a chat session lives must not be what ends that session.
func TestWorkdirForSession_DoesNotEvictADeadSession(t *testing.T) {
	fs := &fakeSession{sid: "sid-x", events: make(chan agent.Event, 4)}
	p := &fakeProvider{sess: fs}
	reg := NewRegistry(p)
	reg.Workspaces = fakeLookup{"ws-a": "/srv/project-a"}
	reg.Bindings = NewBindingStore(t.TempDir())
	reg.Workdir = "/daemon/default"

	if _, err := reg.Attach(context.Background(), "", "fake", "ws-a"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	sid := p.lastCfg.SessionID
	fs.dead.Store(true) // registered, but its agent has gone (tether#55's state)

	if got := reg.WorkdirForSession(sid); got != "/srv/project-a" {
		t.Errorf("WorkdirForSession = %q, want %q — a dead-but-registered session still has a directory", got, "/srv/project-a")
	}
	reg.mu.RLock()
	_, still := reg.sessions[sid]
	reg.mu.RUnlock()
	if !still {
		t.Error("asking where a session lives un-registered it")
	}
}

// TestSessionBinding_LiveRegistrationBeatsTheFile — the live entry is where the
// process actually IS, so it must win over a stale file. Staged by planting a file
// that disagrees with the running session; if the file won, a reconnect would
// resume in a directory the live agent is not in.
func TestSessionBinding_LiveRegistrationBeatsTheFile(t *testing.T) {
	p := &pinningProvider{}
	dir := t.TempDir()
	reg := NewRegistry(p)
	reg.Workspaces = fakeLookup{"ws-a": "/srv/project-a"}
	reg.Bindings = NewBindingStore(dir)
	reg.Workdir = "/daemon/default"

	if _, err := reg.Attach(context.Background(), "", "fake", "ws-a"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer p.closeAll()
	sid := p.LastCfg().SessionID

	if err := reg.Bindings.Save(sid,
		WorkspaceBinding{WorkspaceID: "stale", Path: "/srv/somewhere-else"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := reg.WorkdirForSession(sid); got != "/srv/project-a" {
		t.Errorf("WorkdirForSession = %q, want the LIVE registration's %q, not the file's", got, "/srv/project-a")
	}
}

// TestAttach_LiveSessionInAnotherWorkspaceIsLeftAlone — row 6 with the requested
// session LIVE, which is the ordinary shape of that row (the sibling test covers
// only the corner where the entry has no workdir at all). The running session must
// keep running and stay reachable under its own sid; this connection gets a fresh
// session in the workspace it asked for.
func TestAttach_LiveSessionInAnotherWorkspaceIsLeftAlone(t *testing.T) {
	p := &pinningProvider{}
	reg := newBoundRegistry(t, p, fakeLookup{
		"ws-a": "/srv/project-a",
		"ws-b": "/srv/project-b",
	})
	reg.Workdir = "/daemon/default"

	if _, err := reg.Attach(context.Background(), "", "fake", "ws-a"); err != nil {
		t.Fatalf("Attach #1: %v", err)
	}
	defer p.closeAll()
	sidA := p.LastCfg().SessionID

	if _, err := reg.Attach(context.Background(), sidA, "fake", "ws-b"); err != nil {
		t.Fatalf("Attach #2: %v", err)
	}
	if got := p.Spawns(); got != 2 {
		t.Fatalf("spawns = %d, want 2", got)
	}
	cfg := p.LastCfg()
	if cfg.ResumeSessionID != "" {
		t.Errorf("ResumeSessionID = %q, want \"\" — the live session in ws-a must not be resumed into ws-b", cfg.ResumeSessionID)
	}
	if cfg.Workdir != "/srv/project-b" {
		t.Errorf("Workdir = %q, want %q", cfg.Workdir, "/srv/project-b")
	}
	if _, ok := reg.liveEntry(sidA); !ok {
		t.Error("the session in ws-a was un-registered; it must keep running and stay reachable")
	}
	if got := reg.WorkdirForSession(sidA); got != "/srv/project-a" {
		t.Errorf("the ws-a session's workdir became %q; a rebind must not move it", got)
	}
}

// ─── ValidSessionID / the shared path guard ─────────────────────────────────

// TestValidSessionID pins the ONE guard every place a sid becomes a path segment
// shares (BindingStore, HistoryStore.HasHistory, /api/v1/sessions/<sid>/messages).
// Before tether#52 there were two functions with this job and different contracts.
func TestValidSessionID(t *testing.T) {
	for _, sid := range []string{
		"aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa", // cc: uuid v4
		"ses_0123456789abcdef",                 // opencode-shaped
		"t-01KR6E5V5PG3CXS598FABDY5WZ",         // task-shaped
		"12345678",                             // exactly the minimum length
	} {
		if !ValidSessionID(sid) {
			t.Errorf("ValidSessionID(%q) = false; real session ids must pass", sid)
		}
	}
	for _, sid := range []string{
		"", ".", "..", "1234567", // too short
		"../../etc/passwd", "a/b", `a\b`, // traversal
		"sid with spaces", "sid\x00nul", "sid.dot", "sid:colon", "sid%2e%2e",
	} {
		if ValidSessionID(sid) {
			t.Errorf("ValidSessionID(%q) = true; it would become a path segment", sid)
		}
	}
	long := make([]byte, 129)
	for i := range long {
		long[i] = 'a'
	}
	if ValidSessionID(string(long)) {
		t.Error("ValidSessionID accepted a 129-char sid; unbounded names become ENAMETOOLONG at MkdirAll")
	}
}

// TestListSessions_IgnoresABindingWithoutATranscript — BindingStore shares
// HistoryStore's directory and creates <baseDir>/<sid>/ at SPAWN time to record the
// workspace, i.e. before any message exists. GET /api/v1/sessions is backed by
// ListSessions, so enumerating directories would list every session that ever
// connected and closed without saying anything — each rendering in the workspace
// pane as a clickable entry with an empty transcript.
func TestListSessions_IgnoresABindingWithoutATranscript(t *testing.T) {
	dir := t.TempDir()
	h := NewHistoryStore(dir)
	b := NewBindingStore(dir)

	const spoke = "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa"
	const silent = "bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb"

	if err := b.Save(spoke, WorkspaceBinding{WorkspaceID: "ws-a", Path: "/srv/a"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := b.Save(silent, WorkspaceBinding{WorkspaceID: "ws-a", Path: "/srv/a"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	h.RecordUser(spoke, "hello")

	got := h.ListSessions()
	if len(got) != 1 || got[0] != spoke {
		t.Errorf("ListSessions = %v, want exactly [%s] — a binding is not a conversation", got, spoke)
	}
}

// ─── workdirFor ─────────────────────────────────────────────────────────────

// TestWorkdirFor_ZeroBindingIsTheDaemonDefault pins the one place the zero
// binding becomes a directory, including the pre-tether#51 "inherit the daemon's
// cwd" case (Registry.Workdir unset ⇒ "", resolved by agent.ResolveWorkdir).
func TestWorkdirFor_ZeroBindingIsTheDaemonDefault(t *testing.T) {
	reg := NewRegistry()
	reg.Workdir = "/daemon/default"
	if got := reg.workdirFor(WorkspaceBinding{}); got != "/daemon/default" {
		t.Errorf("workdirFor(zero) = %q, want %q", got, "/daemon/default")
	}
	if got := reg.workdirFor(WorkspaceBinding{WorkspaceID: "ws-a", Path: "/srv/a"}); got != "/srv/a" {
		t.Errorf("workdirFor(ws-a) = %q, want %q", got, "/srv/a")
	}
	reg.Workdir = ""
	if got := reg.workdirFor(WorkspaceBinding{}); got != "" {
		t.Errorf("workdirFor(zero) with no default = %q, want \"\" (the registry must not invent one)", got)
	}
}
