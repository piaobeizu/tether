package cchook

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// runHook executes the compiled hook with exactly the given environment (nothing
// is inherited from the ambient one) and returns its exit code.
func runHook(t *testing.T, binPath string, env []string, stdin string) int {
	t.Helper()
	cmd := exec.Command(binPath)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	t.Logf("hook env=%v err=%v stderr=%q", env, err, stderr.String())
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	t.Fatalf("running hook: %v", err)
	return -1
}

const toolUseStdin = `{"tool_name":"Bash","tool_input":{"command":"echo hi"}}`

// TestHook_NoSentinel_Allows keeps the deliberate fail-open the sentinel exists
// to NARROW rather than remove: a cc the daemon did not spawn (the owner's own
// terminal, an IDE) must not be broken by a hook entry sitting in settings.json.
func TestHook_NoSentinel_Allows(t *testing.T) {
	if code := runHook(t, sharedHook(t), []string{}, toolUseStdin); code != 0 {
		t.Errorf("hook exit = %d, want 0: a cc tether did not spawn must be left alone", code)
	}
}

// TestHook_SentinelWithoutEndpoint_Denies pins tether#117 A4b.
//
// InjectPermHook failing left Registry.PermEndpoint == "", so no
// TETHER_DAEMON_PERM_ENDPOINT reached the cc subprocess — and the hook's
// "endpoint unset means this is not our cc" branch exited 0. A hook entry left
// in settings.json by a previous run therefore produced a daemon serving with
// the permission UI completely disarmed, announced by a single slog line at
// startup. With the mark the two states are distinguishable and this one fails
// CLOSED.
func TestHook_SentinelWithoutEndpoint_Denies(t *testing.T) {
	env := []string{EnvManaged + "=" + ManagedValue}
	if code := runHook(t, sharedHook(t), env, toolUseStdin); code != 2 {
		t.Errorf("hook exit = %d, want 2 (deny): a tether-spawned cc with no endpoint must fail closed. "+
			"Only 2 blocks a tool call — cc treats every other non-zero code as non-blocking", code)
	}
}

// TestHook_SentinelWithUnreachableEndpoint_Denies confirms the pre-existing
// fail-closed path still holds, i.e. the new branch did not displace it.
func TestHook_SentinelWithUnreachableEndpoint_Denies(t *testing.T) {
	env := []string{
		EnvManaged + "=" + ManagedValue,
		// Port 1 on loopback: refused immediately, so no timeout wait.
		EnvEndpoint + "=https://127.0.0.1:1/api/v1/permission/request",
	}
	if code := runHook(t, sharedHook(t), env, toolUseStdin); code != 2 {
		t.Errorf("hook exit = %d, want 2 when the daemon is unreachable", code)
	}
}

// TestHook_EndpointWithoutSentinel_StillEnforces covers the transitional shape:
// a spawn path that plumbs the endpoint but not the mark must still be gated.
// The mark narrows a fail-open; it must never become a NEW way to opt out of the
// gate by omitting it.
func TestHook_EndpointWithoutSentinel_StillEnforces(t *testing.T) {
	env := []string{EnvEndpoint + "=https://127.0.0.1:1/api/v1/permission/request"}
	if code := runHook(t, sharedHook(t), env, toolUseStdin); code != 2 {
		t.Errorf("hook exit = %d, want 2: an endpoint present without the mark must still be enforced", code)
	}
}

// TestHook_SentinelWrongValue_IsNotAMark pins that the mark is an exact value,
// not "any non-empty string" — so an unrelated TETHER_DAEMON_MANAGED=0 in the
// owner's shell profile cannot start denying their standalone cc's tools.
func TestHook_SentinelWrongValue_IsNotAMark(t *testing.T) {
	if code := runHook(t, sharedHook(t), []string{EnvManaged + "=0"}, toolUseStdin); code != 0 {
		t.Errorf("hook exit = %d, want 0: %s=0 is not the mark", code, EnvManaged)
	}
}

// TestHookSource_ReadsDeclaredEnvVars pins the names. The hook is a string
// constant compiled at runtime, so renaming a constant here cannot be caught by
// the compiler — only by comparing the two halves.
func TestHookSource_ReadsDeclaredEnvVars(t *testing.T) {
	for _, name := range []string{EnvEndpoint, EnvManaged} {
		if !strings.Contains(hookSource, `os.Getenv("`+name+`")`) {
			t.Errorf("hookSource does not read %s — the constant and the hook have drifted apart", name)
		}
	}
	if !strings.Contains(hookSource, `== "`+ManagedValue+`"`) {
		t.Errorf("hookSource does not compare against ManagedValue %q", ManagedValue)
	}
}

// TestGate_Env is the contract the two cc spawn paths consume (tether#149 wires
// the second one). It is a table because the four rows ARE the semantics.
func TestGate_Env(t *testing.T) {
	const ep = "https://127.0.0.1:8443/api/v1/permission/request"
	tests := []struct {
		name string
		gate Gate
		want []string
	}{{
		name: "unmanaged injects nothing",
		gate: Gate{},
		want: nil,
	}, {
		name: "unmanaged ignores a stray endpoint",
		gate: Gate{Endpoint: ep},
		want: nil,
	}, {
		name: "managed with endpoint arms the gate",
		gate: Gate{Managed: true, Endpoint: ep},
		want: []string{EnvManaged + "=" + ManagedValue, EnvEndpoint + "=" + ep},
	}, {
		name: "managed without endpoint still marks the child, so the hook denies",
		gate: Gate{Managed: true},
		want: []string{EnvManaged + "=" + ManagedValue},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.gate.Env()
			if len(got) != len(tc.want) {
				t.Fatalf("Env() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("Env()[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestGate_Env_RoundTripsThroughTheHook closes the loop the two unit tests above
// leave open: Gate.Env() is what the spawn paths inject, so the exit code the
// hook produces FROM Env() output is the property that actually matters. A
// Gate.Env() that is individually correct and a hook that is individually
// correct can still disagree about the variable names.
func TestGate_Env_RoundTripsThroughTheHook(t *testing.T) {
	binPath := sharedHook(t)

	if code := runHook(t, binPath, Gate{}.Env(), toolUseStdin); code != 0 {
		t.Errorf("unmanaged gate: hook exit = %d, want 0", code)
	}
	if code := runHook(t, binPath, Gate{Managed: true}.Env(), toolUseStdin); code != 2 {
		t.Errorf("managed gate with no endpoint: hook exit = %d, want 2 (fail closed)", code)
	}
	armed := Gate{Managed: true, Endpoint: "https://127.0.0.1:1/api/v1/permission/request"}
	if code := runHook(t, binPath, armed.Env(), toolUseStdin); code != 2 {
		t.Errorf("armed gate, daemon down: hook exit = %d, want 2", code)
	}
}
