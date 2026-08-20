//go:build !windows

package builtin_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/piaobeizu/tether/internal/mcp/builtin"
)

// escapedProbeCommand builds a command whose surviving process has left the
// process group killOnTimeout aims at, while still holding the write end of the
// pipe os/exec handed the command for stdout.
//
// `setsid` is what puts it out of reach. A background job of a non-interactive
// shell stays in the shell's own process group — job control is off, so sh does
// not give it one — and setsid(2) then moves it into a fresh session, and so a
// fresh process group, of its own. `kill(-pgid)` no longer names it. It is
// deliberately not redirected, so it keeps the command's stdout pipe; the
// foreground sleep is redirected and contributes nothing but keeping sh alive
// until the timeout fires rather than the command finishing.
//
// This is the case the in-group sibling in proc_unix_test.go cannot express: a
// descendant that stays in the group dies with it, so a build with no WaitDelay
// at all returns promptly there and that test passes either way.
func escapedProbeCommand(pidFile string) string {
	return fmt.Sprintf("setsid sleep 60 & echo $! > %s; sleep 60 %s", pidFile, devNull)
}

// killEscapedPid SIGKILLs the process escapedProbeCommand left outside the
// group, if it is still alive. Registered as a cleanup before the tool call so
// it covers every exit path — including a Fatalf on the timing assertion, which
// is exactly the path on which the process is still running.
func killEscapedPid(t *testing.T, pidFile string) {
	t.Helper()
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return
	}
	if pidAlive(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// TestRunShell_TimeoutReturnsWhileEscapedDescendantHoldsPipe pins that the
// advertised timeout is a bound on how long the tool call takes, not merely the
// moment it decides to give up.
//
// handleRunShell waits for cmd.Wait() after killing the group, and cmd.Wait()
// reads the command's pipes until EOF. A descendant that left the group survives
// the kill and holds the write end, so without a WaitDelay the EOF arrives only
// when that descendant exits — measured at 60s for this command's `sleep 60`
// against a 1s timeout, and it is a bound on nothing: raise the sleep and the
// call, its goroutine and both output buffers stay pinned for exactly that long.
//
// The still-alive check is not decoration. It is what distinguishes "returned
// promptly because WaitDelay bounded the wait" from "returned promptly because
// the descendant never escaped after all" — the second would make this test
// pass on a build with no WaitDelay, i.e. no test at all.
func TestRunShell_TimeoutReturnsWhileEscapedDescendantHoldsPipe(t *testing.T) {
	root := t.TempDir()
	pidFile := filepath.Join(root, "escaped.pid")

	srv := mcp.NewServer(&mcp.Implementation{Name: "test"}, nil)
	reg, err := builtin.New(root)
	if err != nil {
		t.Fatal(err)
	}
	reg.RegisterInto(srv)

	t.Cleanup(func() { killEscapedPid(t, pidFile) })

	args, _ := json.Marshal(map[string]any{
		"command":      escapedProbeCommand(pidFile),
		"timeout_secs": 1,
	})

	start := time.Now()
	result := invokeBuiltin(t, srv, "workspace_run_shell", args)
	elapsed := time.Since(start)

	escapedPID := 0
	if raw, rerr := os.ReadFile(pidFile); rerr == nil {
		escapedPID, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
	}
	stillAlive := escapedPID > 0 && pidAlive(escapedPID)
	t.Logf("timeout_secs=1 returned after %s; escaped pid=%d alive_at_return=%v",
		elapsed.Round(10*time.Millisecond), escapedPID, stillAlive)

	if !result.IsError {
		t.Fatalf("expected the 1s timeout to fire, got a normal result: %v", result.Content)
	}
	if text := result.Content[0].(*mcp.TextContent).Text; !strings.Contains(text, "timed out") {
		t.Fatalf("expected 'timed out', got %q", text)
	}
	if escapedPID <= 0 {
		t.Fatalf("the probe command never recorded a pid in %s, so this test proved "+
			"nothing about a descendant outside the process group", pidFile)
	}
	if !stillAlive {
		t.Fatalf("pid %d was gone by the time the tool returned: this run had no "+
			"descendant outside the process group holding the pipe, so it cannot "+
			"tell a bounded wait from an unbounded one", escapedPID)
	}

	// The command's own processes sleep 60s against a 1s timeout, so anything in
	// that neighbourhood means the return was gated on the escaped process. The
	// bound is loose because only the order of magnitude is meaningful: the
	// intended cost is the timeout plus at most one procWaitDelay.
	if elapsed > 15*time.Second {
		t.Fatalf("workspace_run_shell took %s to report a 1s timeout: it waited for the "+
			"process outside the group that holds the command's stdout pipe, so the "+
			"advertised timeout bounds nothing", elapsed)
	}
}
