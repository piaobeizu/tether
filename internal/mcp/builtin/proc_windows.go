//go:build windows

package builtin

import (
	"os/exec"
	"time"
)

// procWaitDelay bounds how long cmd.Wait() may keep the caller waiting after
// the process we started has exited. See setKillScope for why this build needs
// it and the !windows build does not.
const procWaitDelay = 5 * time.Second

// setKillScope cannot widen the kill scope on this platform, so all it does is
// bound the wait that the narrower scope can cause.
//
// The !windows build makes the command a process-group leader so one signal
// reaches every descendant (proc_unix.go). Windows process groups
// (CREATE_NEW_PROCESS_GROUP) are not that: they only route console Ctrl+C /
// Ctrl+Break events, which needs an attached console and a cooperating child,
// and they carry no way to terminate the group. The primitive that does carry
// it is a job object — CreateJobObject, AssignProcessToJobObject, then
// TerminateJobObject on timeout. This file does not implement one, so
// killOnTimeout reaches the direct child only; the consequences are listed
// there.
//
// WaitDelay is the fallout of that gap, not a nicety. cmd.Stdout/cmd.Stderr
// here are ordinary io.Writers, so os/exec gives the child a pipe and copies
// from it until EOF — and a grandchild we failed to kill still holds the write
// end. Without WaitDelay ("If WaitDelay is zero (the default), I/O pipes will
// be read until EOF, which might not occur until orphaned subprocesses of the
// command have also closed their descriptors for the pipes" — os/exec docs),
// the <-done in handleRunShell would block for as long as that grandchild
// lives, and the tool would never return the timeout it already decided on.
//
// The delay also applies when the command exits normally, which is a real
// behaviour difference from the !windows build: a command that leaves a
// background process holding the pipe returns after procWaitDelay rather than
// blocking, and Wait then reports exec.ErrWaitDelay in place of the nil it
// would otherwise have returned. handleRunShell has to special-case that error
// or a command that succeeded is reported as a failure carrying no output —
// see the switch on runErr in workspace.go.
func setKillScope(cmd *exec.Cmd) {
	cmd.WaitDelay = procWaitDelay
}

// killOnTimeout kills the process this package started — and nothing below it.
// This is a degradation from the !windows build, not a port of it:
//
//   - The !windows build SIGKILLs a whole process group. This build kills one
//     process, because setKillScope had no group to create.
//   - Anything that process spawned survives the timeout: `sh -c 'a & b'`
//     leaves a and b running until they exit on their own. They keep the
//     workspace's files, ports and CPU, and tether has no handle on them
//     afterwards.
//   - The timeout error itself is no worse here — handleRunShell discards the
//     captured output on a timeout regardless of platform. What this build
//     loses is on the path where the command *succeeds* and a survivor holds
//     the pipes; see setKillScope.
//
// Worth knowing when judging how much the above costs: workspace_run_shell runs
// `sh -c`, and Windows ships no sh. The tool does anything at all only on a host
// where something else — Git for Windows, MSYS — has put one on PATH; anywhere
// else it fails at exec.Cmd.Start ("executable file not found in %PATH%") and
// never reaches this function.
func killOnTimeout(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
