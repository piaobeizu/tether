package server

import (
	"context"
	"os"
	"testing"

	"github.com/piaobeizu/tether/internal/session"
)

// TestBuildPTYCommand_SetsWorkdir — the PTY shell pane resumes the CHAT
// session's sid, and cc scopes `--resume` to the cwd it stores conversations
// under (~/.claude/projects/<encoded-cwd>/). So the shell's cwd must be the same
// workspace root the chat provider spawns in, or every shell attach lands in a
// fresh conversation instead of the one the user was just chatting in
// (tether#51 — before the fix neither path set cmd.Dir, which hid the coupling
// behind "they both inherit the daemon's cwd").
func TestBuildPTYCommand_SetsWorkdir(t *testing.T) {
	const workdir = "/some/workspace"
	cmd := buildPTYCommand(context.Background(), "/bin/true", "sid-abc", workdir)
	if cmd.Dir != workdir {
		t.Errorf("cmd.Dir = %q, want %q", cmd.Dir, workdir)
	}
	// The resume args must survive the extraction.
	want := []string{"/bin/true", "--resume", "sid-abc"}
	if len(cmd.Args) != len(want) {
		t.Fatalf("cmd.Args = %v, want %v", cmd.Args, want)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Fatalf("cmd.Args = %v, want %v", cmd.Args, want)
		}
	}
}

// TestBuildPTYCommand_EmptyWorkdirFallsBackToProcessCwd — an unwired
// Registry.Workdir must resolve to the daemon's own cwd (the pre-tether#51
// behaviour), via the same agent.ResolveWorkdir the providers use.
func TestBuildPTYCommand_EmptyWorkdirFallsBackToProcessCwd(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	cmd := buildPTYCommand(context.Background(), "/bin/true", "", "")
	if cmd.Dir != wd {
		t.Errorf("cmd.Dir = %q, want %q (process cwd)", cmd.Dir, wd)
	}
}

// TestBuildPTYCommand_NoSidOmitsResume — a shell attached with no sid starts a
// fresh cc, so no --resume flag may be passed (cc errors on an empty value).
func TestBuildPTYCommand_NoSidOmitsResume(t *testing.T) {
	cmd := buildPTYCommand(context.Background(), "/bin/true", "", "/some/workspace")
	if len(cmd.Args) != 1 {
		t.Errorf("cmd.Args = %v, want just the binary (no --resume)", cmd.Args)
	}
}

// TestShellWorkdirFollowsTheChatSessionsWorkspace — tether#52 composition. The
// shell pane is handed the CHAT session's sid, and since chat may now run in any
// registered workspace, handleWTShell asks the registry where THAT session lives
// instead of passing the daemon-global root. Passing the root would resume in a
// directory the conversation was never created in and drop the user into an empty
// one (see buildPTYCommand's doc for why cc behaves that way).
//
// This pins the pair — WorkdirForSession feeding buildPTYCommand — not the
// handler: handleWTShell takes a concrete *webtransport.Session, so reaching the
// call site from a test needs a real QUIC connection and no such harness exists
// here. Same known gap as admitChat's call site (see wt_chat.go); the composition
// is pinned, the wiring is covered by live_verify only.
func TestShellWorkdirFollowsTheChatSessionsWorkspace(t *testing.T) {
	reg := session.NewRegistry()
	reg.Bindings = session.NewBindingStore(t.TempDir())
	reg.Workdir = "/daemon/default"
	if err := reg.Bindings.Save("chat-sid", session.WorkspaceBinding{
		WorkspaceID: "ws-a", Path: "/srv/project-a",
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	cmd := buildPTYCommand(context.Background(), "/bin/true", "chat-sid", reg.WorkdirForSession("chat-sid"))
	if cmd.Dir != "/srv/project-a" {
		t.Errorf("cmd.Dir = %q, want the chat session's workspace %q", cmd.Dir, "/srv/project-a")
	}

	// A sid the daemon knows nothing about is ordinary (a shell opened before any
	// chat session, or a sid from a previous daemon) and falls back to the default.
	cmd = buildPTYCommand(context.Background(), "/bin/true", "unknown-sid", reg.WorkdirForSession("unknown-sid"))
	if cmd.Dir != "/daemon/default" {
		t.Errorf("cmd.Dir = %q, want the daemon default %q", cmd.Dir, "/daemon/default")
	}
}
