package server

import (
	"errors"
	"testing"

	"github.com/piaobeizu/tether/internal/permission/cchook"
)

const testPermEndpoint = "https://127.0.0.1:8443/api/v1/permission/request"

func okEnsure(string) error         { return nil }
func okInject(string, string) error { return nil }

// TestSetupPermGate_InjectFailureKeepsTheGateArmed pins tether#117 A4b.
//
// The two startup steps that arm the permission gate had opposite failure
// policies, and the milder one was pointed the wrong way: a failed
// settings.json patch (a transient write error, a read-only $HOME, a corrupt
// settings.json) logged a warning and left the endpoint empty. Nothing reached
// the cc subprocess, the hook's "no endpoint means this is not our cc" branch
// exited 0, and a hook entry left behind by a previous run turned the daemon
// into one serving with the permission UI entirely disarmed.
//
// The endpoint is a property of this daemon's listener, not of whether a file
// got rewritten, so it must survive the patch failing.
func TestSetupPermGate_InjectFailureKeepsTheGateArmed(t *testing.T) {
	failInject := func(string, string) error { return errors.New("settings.json is read-only") }

	gate, err := setupPermGate(false, "/tmp/hook", testPermEndpoint, okEnsure, failInject)
	if err != nil {
		t.Fatalf("a failed settings patch must not fail startup: %v", err)
	}
	if !gate.Managed {
		t.Error("gate.Managed = false: a tether-spawned cc would go unmarked and the hook would exit 0 (allow)")
	}
	if gate.Endpoint != testPermEndpoint {
		t.Errorf("gate.Endpoint = %q, want %q — the endpoint does not depend on the settings patch",
			gate.Endpoint, testPermEndpoint)
	}
	// The property that actually matters is what lands in the child's env.
	env := gate.Env()
	if len(env) != 2 {
		t.Fatalf("gate.Env() = %v, want the mark and the endpoint", env)
	}
}

// TestSetupPermGate_Success is the happy path: armed, with the endpoint.
func TestSetupPermGate_Success(t *testing.T) {
	gate, err := setupPermGate(false, "/tmp/hook", testPermEndpoint, okEnsure, okInject)
	if err != nil {
		t.Fatal(err)
	}
	if !gate.Managed || gate.Endpoint != testPermEndpoint {
		t.Fatalf("gate = %+v, want managed with the endpoint", gate)
	}
}

// TestSetupPermGate_CompileFailureIsFatal keeps the asymmetry deliberate. With
// no hook binary the PreToolUse command in settings.json exits 127, and cc
// blocks only on exit code 2 — every other non-zero code is non-blocking, so
// every tool would run unprompted. There is nothing to degrade to.
func TestSetupPermGate_CompileFailureIsFatal(t *testing.T) {
	failEnsure := func(string) error { return errors.New("no go toolchain") }

	gate, err := setupPermGate(false, "/tmp/hook", testPermEndpoint, failEnsure, okInject)
	if err == nil {
		t.Fatal("a hook that cannot be compiled must fail startup")
	}
	if gate != (cchook.Gate{}) {
		t.Errorf("gate = %+v, want the zero value on a fatal error", gate)
	}
}

// TestSetupPermGate_HookDisabledMarksNothing pins the opt-out.
// TETHER_NO_PERMISSION_HOOK=1 must leave cc children completely unmarked, so a
// hook entry surviving in settings.json keeps exiting 0 instead of denying every
// tool call. Marking them here would turn an opt-out into a hard block.
func TestSetupPermGate_HookDisabledMarksNothing(t *testing.T) {
	ensureCalled, injectCalled := false, false
	gate, err := setupPermGate(true, "/tmp/hook", testPermEndpoint,
		func(string) error { ensureCalled = true; return nil },
		func(string, string) error { injectCalled = true; return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if gate != (cchook.Gate{}) {
		t.Errorf("gate = %+v, want the zero value when the hook is disabled", gate)
	}
	if env := gate.Env(); env != nil {
		t.Errorf("gate.Env() = %v, want nil: an opted-out daemon must inject nothing", env)
	}
	if ensureCalled || injectCalled {
		t.Errorf("hook disabled but side effects ran: ensure=%v inject=%v", ensureCalled, injectCalled)
	}
}
