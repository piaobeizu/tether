package builtin

import (
	"os/exec"
	"syscall"
)

// TEMPORARY — tether#85. Reintroduces the exact construct the cross-compile
// gate exists to catch, so the gate can be observed turning CI red on a real
// push rather than only on this machine. Reverted in the commit after this one;
// if you are reading this on main, the revert was lost.
func gateProbe(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
