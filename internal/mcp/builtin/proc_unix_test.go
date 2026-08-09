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

// devNull keeps a process off the pipes os/exec sets up for the command's
// stdout and stderr. Anything still holding a write end keeps cmd.Wait() — and
// so handleRunShell — from returning, whatever the timeout says.
const devNull = ">/dev/null 2>&1"

// runShellPidProbe runs a command that backgrounds `sleep 60`, records its pid,
// and then blocks so the tool's timeout is what ends the command. Returns the
// backgrounded pid and how long the tool call took.
//
// bgRedirect is appended to the background sleep only; the foreground one is
// always redirected. That asymmetry is the point: with the foreground sleep on
// the pipes, a build that kills only the direct child waits 60s for it, every
// process in the command dies of old age in the meantime, and a check for
// survivors run afterwards finds none — which is how the first two versions of
// the test below passed against a build with no process group at all.
func runShellPidProbe(t *testing.T, bgRedirect string) (pid int, elapsed time.Duration) {
	t.Helper()

	root := t.TempDir()
	pidFile := filepath.Join(root, "grandchild.pid")

	srv := mcp.NewServer(&mcp.Implementation{Name: "test"}, nil)
	reg, err := builtin.New(root)
	if err != nil {
		t.Fatal(err)
	}
	reg.RegisterInto(srv)

	// `sleep 60 &` stays in the same process group as the sh that started it: a
	// non-interactive shell has job control off, so it does not give background
	// jobs a group of their own. The trailing `sleep 60` keeps sh itself alive
	// so the 1s timeout is what ends the command, not the command finishing.
	command := fmt.Sprintf("sleep 60 %s & echo $! > %s; sleep 60 %s", bgRedirect, pidFile, devNull)
	args, _ := json.Marshal(map[string]any{"command": command, "timeout_secs": 1})

	start := time.Now()
	result := invokeBuiltin(t, srv, "workspace_run_shell", args)
	elapsed = time.Since(start)

	if !result.IsError {
		t.Fatalf("expected the 1s timeout to fire, got a normal result: %v", result.Content)
	}
	if text := result.Content[0].(*mcp.TextContent).Text; !strings.Contains(text, "timed out") {
		t.Fatalf("expected 'timed out', got %q", text)
	}

	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read %s: %v — the command never recorded its background pid, so "+
			"this test proved nothing about what the timeout reached", pidFile, err)
	}
	pid, err = strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		t.Fatalf("bad pid %q in %s: %v", raw, pidFile, err)
	}
	t.Cleanup(func() {
		// Guarded, not unconditional: on the passing path the group kill has
		// already reaped this pid by now, and signalling a number the kernel
		// has since handed to something else is a hazard this test would be
		// creating for the rest of the suite.
		if pidAlive(pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	return pid, elapsed
}

// pidAlive reports whether pid is still the `sleep` the command backgrounded.
//
// Signal 0 alone answers "is this pid addressable", which is not the same
// question once the kernel is free to reuse the number — so on Linux the answer
// is corroborated against /proc. Where /proc is not mounted (darwin) it cannot
// be, and the signal-0 answer stands; a reused pid there reads as still alive.
func pidAlive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return true
	}
	return strings.Contains(string(cmdline), "sleep")
}

// TestRunShell_TimeoutKillsBackgroundGrandchild pins the reason setKillScope
// exists on this platform: the timeout has to reach past the one process
// handleRunShell started, and the process group is what gives it that reach.
// Nothing else in the suite would notice if the group went away —
// TestRunShell_CustomTimeout passes on a build that leaves the command's
// children running, because the tool still returns "timed out".
//
// Every process in the command is redirected off the pipes here, so the tool
// returns as soon as the direct child dies and this check runs while a survivor
// would still be alive. See runShellPidProbe for what leaving them on the pipes
// does to this check; the sibling test is where that belongs.
func TestRunShell_TimeoutKillsBackgroundGrandchild(t *testing.T) {
	pid, _ := runShellPidProbe(t, devNull)

	// Polled rather than read once, because the group SIGKILL and the tool's
	// return race each other.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if !pidAlive(pid) {
			return // gone — the kill reached past the direct child
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d (the `sleep 60` this command backgrounded) is still alive "+
				"5s after workspace_run_shell reported a timeout: the kill reached the "+
				"direct child only", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRunShell_TimeoutReturnsWhileGrandchildHoldsPipe pins the second thing the
// process group buys, which the test above deliberately excludes.
//
// cmd.Stdout and cmd.Stderr here are ordinary io.Writers, so os/exec hands the
// command a pipe and copies from it until EOF — and a background process that
// inherited the write end holds it open after the command itself is gone. Since
// handleRunShell waits for cmd.Wait() before returning the timeout, a kill that
// misses that process makes the caller wait for it: measured at 60.0s against
// this command's `sleep 60`, versus 1.0s when the group kill takes it out.
//
// This is what os/exec's Cmd.WaitDelay covers, and why proc_windows.go sets it
// and this file does not.
func TestRunShell_TimeoutReturnsWhileGrandchildHoldsPipe(t *testing.T) {
	_, elapsed := runShellPidProbe(t, "")

	// The command's own processes sleep 60s and the timeout is 1s, so anything
	// in between means the return was gated on a process the kill missed. The
	// bound is loose because only the order of magnitude is meaningful.
	if elapsed > 15*time.Second {
		t.Fatalf("workspace_run_shell took %s to report a 1s timeout: it waited on the "+
			"background process holding the command's stdout pipe instead of killing it", elapsed)
	}
}
