//go:build integration

// Run with:
//
//	GOWORK=off go test -tags=integration -run TestResumeRefusedByALiveBackgroundAgent ./internal/session -v
//
// tether#101's PROBE. It needs the real `claude` binary on PATH and nothing else:
// no API key, no OAuth session, no transcript, and no network. The refusal this
// exercises happens before cc contacts anything — it reads its own live-session
// registry, writes to stderr and exits 1 — which is precisely why the failure was
// invisible to tether and why this test is cheap enough to keep.
//
// # It is a probe, not a fidelity backstop
//
// The everyday guards are in attach_cc_test.go and ccregistry_test.go, and they run
// against fabricated registry records and a fake agent. What THIS adds is the one
// thing they cannot: it proves the fabricated record is the same thing real cc acts
// on. If cc ever stops refusing, or refuses under different conditions, the fakes
// keep passing and this stops.
//
// # It is also the before/after probe, and that is a requirement
//
// On a build WITHOUT the classification in Attachment.resolve, this test fails with
// "Resolve succeeded and handed back a DIFFERENT sid" — i.e. it reproduces the
// original defect (a silent fresh session) rather than merely failing to see the
// new behaviour. A probe that only passes on the new build and errors vaguely on the
// old one cannot tell "the fix works" from "the fix is untested".
//
// # It never touches the real store
//
// CLAUDE_CONFIG_DIR is redirected to a t.TempDir() and the record is written there.
// The user's ~/.claude is neither read nor written by this test, and cc — which
// honours that variable (env.CLAUDE_CONFIG_DIR ?? join(homedir(), ".claude")) —
// reads the temporary one too, which is what makes the whole thing self-contained.

package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/wire"
)

func TestResumeRefusedByALiveBackgroundAgent(t *testing.T) {
	ccPath, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude CLI not on PATH")
	}

	// A uuid-shaped sid: cc validates the argument to --resume against
	// /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i, and a sid
	// that fails it never reaches the gate under test.
	sid := newUUIDv4(t)

	ccHome := t.TempDir()
	sessionsDir := filepath.Join(ccHome, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", sessionsDir, err)
	}
	// The holder is THIS TEST PROCESS: alive for the duration, and its start token
	// is whatever the kernel says, so the record is live by construction rather than
	// by a sleep. cc excludes only its OWN pid, and cc is a child, so this qualifies.
	pid := os.Getpid()
	token, ok := ccProcStartToken(pid)
	if !ok {
		t.Skip("cannot read this process's start token; /proc is required for both cc's liveness check and tether's")
	}
	rec := map[string]any{
		"pid": pid, "sessionId": sid, "cwd": t.TempDir(),
		"startedAt": time.Now().UnixMilli(), "procStart": token,
		"version": "2.1.233", "peerProtocol": 1,
		"kind": "bg", "entrypoint": "cli", "jobId": "probe-job", "status": "busy",
	}
	body, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionsDir, fmt.Sprintf("%d.json", pid)), body, 0o644); err != nil {
		t.Fatalf("write record: %v", err)
	}
	// Read by BOTH sides of this test: cc, when it decides whether to refuse, and
	// the reader below, when it decides what to say about the refusal. One directory,
	// so the test cannot pass by the two of them disagreeing in the same direction.
	// ClaudeCodeProvider passes os.Environ() through to the subprocess (buildEnv), so
	// t.Setenv is enough.
	t.Setenv("CLAUDE_CONFIG_DIR", ccHome)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	reg := NewRegistry(agent.NewClaudeCodeProvider(ccPath))
	reg.History = NewHistoryStore(filepath.Join(t.TempDir(), "sessions"))
	reg.Workdir = t.TempDir()
	reg.CCJobs = NewCCRegistry(CCSessionsDir(ccHome))

	// Sanity-check the fixture before the behaviour: if the reader does not consider
	// this record live, the test below would pass or fail for the wrong reason.
	job, held := reg.ccLiveJob(sid)
	if !held {
		t.Fatalf("the fixture record is not classified as a live holder; the probe would be testing nothing (dir %s)", sessionsDir)
	}
	if job.Kind != "bg" || job.JobID != "probe-job" {
		t.Fatalf("fixture classified as %+v, want kind bg / job probe-job", job)
	}

	att, err := reg.Attach(ctx, sid, "claude-code", "")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	// Real cc under --input-format stream-json reads a user message before it
	// resolves the session, so the prompt is what makes it reach the gate. The error
	// is expected and ignored: cc exits without reading stdin, so this is a broken
	// pipe, which is the ordinary shape of this path (see SendPrompt's doc).
	_ = att.SendPrompt(ctx, "carry on where we left off")

	res, err := att.Resolve(ctx)
	if err == nil {
		// THE OLD BUILD LANDS HERE, and this is the original defect reproduced: a
		// session the user asked for, answered by a brand-new empty conversation
		// under a different id, with nothing said about it.
		if res.SID != sid {
			t.Fatalf("Resolve succeeded and handed back a DIFFERENT sid (%s, asked for %s, Recovered=%v): "+
				"real cc refused this resume, and the daemon silently started a fresh session instead of saying so",
				res.SID, sid, res.Recovered)
		}
		t.Fatalf("Resolve succeeded with the requested sid (%s); cc was expected to refuse it outright", res.SID)
	}

	var ref *Refusal
	if !errors.As(err, &ref) {
		t.Fatalf("Resolve error is %T (%v); an unclassified error reaches the browser as retryable and says nothing useful", err, err)
	}
	if ref.Code != wire.ErrCodeSessionHeldByBackgroundAgent {
		t.Fatalf("Refusal.Code = %q, want %q (message: %v)", ref.Code, wire.ErrCodeSessionHeldByBackgroundAgent, err)
	}
	if !ref.Code.Terminal() {
		t.Error("the code is not Terminal; the browser would retry once a second against a refusal that cannot clear while the job runs")
	}
	if res.SID != "" {
		t.Errorf("Resolution.SID = %q, want empty; nothing was started, so there is no session to report", res.SID)
	}
	t.Logf("real cc refused the resume and the daemon reported it: code=%s message=%v", ref.Code, err)
}

// newUUIDv4 builds a random uuid without pulling in a dependency for one string.
// Only the SHAPE matters here — cc validates the argument to --resume against a
// uuid regexp, and the id names a session that deliberately does not exist.
func newUUIDv4(t *testing.T) string {
	t.Helper()
	var b [16]byte
	f, err := os.Open("/dev/urandom")
	if err != nil {
		t.Fatalf("open /dev/urandom: %v", err)
	}
	defer f.Close()
	if _, err := f.Read(b[:]); err != nil {
		t.Fatalf("read /dev/urandom: %v", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
