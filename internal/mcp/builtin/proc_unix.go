//go:build !windows

package builtin

import (
	"os/exec"
	"syscall"
	"time"
)

// procWaitDelay bounds how long cmd.Wait() may keep the caller waiting after the
// process we started has exited — i.e. how much the escape hatch documented on
// setKillScope is allowed to cost. Same value as the windows build's, for the
// same reason; the price is that a timeout can report up to this much later than
// the deadline it was given, which is a bound where there was none.
const procWaitDelay = 5 * time.Second

// setKillScope widens what killOnTimeout will be able to reach: it makes the
// command the leader of a brand-new process group, so one signal can be aimed
// at the group rather than at the single process we started. Must be called
// before cmd.Start(); SysProcAttr is read by the fork/exec, not afterwards.
//
// It does not make the group escape-proof — a descendant that calls setpgid or
// setsid for itself leaves the group and outlives killOnTimeout. WaitDelay is
// what keeps that escape from being unbounded rather than merely untidy: with
// cmd.Stdout/cmd.Stderr set to ordinary io.Writers, os/exec copies from the
// command's pipe until EOF, and an escaped descendant that inherited the write
// end holds that EOF for as long as it lives. handleRunShell waits for
// cmd.Wait() before returning the timeout it has already decided on, so without
// this the tool call, its goroutine and both output buffers stay pinned for the
// descendant's lifetime: measured at 30s for a 1s timeout against `setsid sleep
// 30`, and 30s is not a ceiling — it is whatever the descendant chose. Real
// triggers in an AI coding workspace are anything that daemonizes itself without
// redirecting inherited descriptors: dev servers, language servers, a tmux or
// screen server.
//
// One knock-on this build cannot fix from here: proc_windows.go's own comments
// say WaitDelay is something "this build needs and the !windows build does not",
// and set the field as the sole compensation for having no killable process
// group. The first half stopped being true with the line below; the file is
// outside the change that added it.
func setKillScope(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = procWaitDelay
}

// killOnTimeout SIGKILLs the process group set up by setKillScope: the shell we
// started plus every descendant still in that group.
//
// A negative pid addresses the process group whose id equals that pid (kill(2)),
// which is why setKillScope has to have run first. Skip it and the child sits in
// the daemon's group instead, no group carries the child's own pid, and this
// call fails with ESRCH — it does not kill tether by mistake, it kills nothing
// at all. Measured cost of that silent miss: a command with a 1s timeout ran its
// full 20s before the handler returned, because cmd.Wait() went on reading pipes
// that the processes nobody had killed were still holding open.
func killOnTimeout(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
