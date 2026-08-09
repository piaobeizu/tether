//go:build !windows

package builtin

import (
	"os/exec"
	"syscall"
)

// setKillScope widens what killOnTimeout will be able to reach: it makes the
// command the leader of a brand-new process group, so one signal can be aimed
// at the group rather than at the single process we started. Must be called
// before cmd.Start(); SysProcAttr is read by the fork/exec, not afterwards.
//
// It does not make the group escape-proof — a descendant that calls setpgid or
// setsid for itself leaves the group and outlives killOnTimeout.
func setKillScope(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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
