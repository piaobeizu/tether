package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/permission/cchook"
	"github.com/piaobeizu/tether/internal/session"
)

// The daemon spawns cc down exactly two paths, and each builds the child's
// environment itself:
//
//	chat  — session.Registry.spawnEntry fills agent.SpawnConfig.Env, which both
//	        providers append to os.Environ() before exec (agent.buildEnv).
//	shell — server.buildPTYEnv builds the PTY child's env directly.
//
// Every other test in the tree sees at most one of them, because each lives in
// its own package. That is not a gap in the test suite so much as the shape of
// the defect tether#149 exists to make catchable: a permission-gate variable
// added to one path and forgotten on the other leaves the forgotten path's
// children UNMARKED, the hook reads unmarked as "not a cc the daemon spawned"
// and exits 0, and every tool call down that path runs without a prompt. Both
// packages stay green throughout.
//
// This file is the one place both paths are visible at once. A third spawn path
// belongs in spawnPathEnvs below.

// spawnPathEnv is one cc spawn path plus the environment its child would be
// exec'd with, built by asking that path — never by restating what it ought to
// produce.
type spawnPathEnv struct {
	name string
	env  []string
}

// recordingProvider captures the SpawnConfig the chat path built and then
// refuses to spawn. Refusing is what keeps this a test about the environment:
// spawnEntry fills cfg.Env well before it calls Spawn, so the recording is
// complete, and returning an error stops the registry from registering an entry
// and starting a fanOut goroutine over a session that does not exist.
type recordingProvider struct {
	lastCfg agent.SpawnConfig
}

func (p *recordingProvider) Name() string { return "recording" }

func (p *recordingProvider) Spawn(_ context.Context, cfg agent.SpawnConfig) (agent.Session, error) {
	p.lastCfg = cfg
	return nil, errors.New("recordingProvider never spawns")
}

// spawnPathEnvs drives BOTH cc spawn paths with one gate and returns the
// environment each one would hand its child.
//
// The chat arm reconstructs the child env the way agent.buildEnv does —
// os.Environ() first, SpawnConfig.Env last — because SpawnConfig.Env alone is
// the daemon's ADDITION to the environment, not the environment. Appending last
// is also what makes the gate's entries win over any stale TETHER_DAEMON_* the
// daemon inherited, since exec keeps the last value of a duplicated key.
func spawnPathEnvs(t *testing.T, gate cchook.Gate) []spawnPathEnv {
	t.Helper()

	prov := &recordingProvider{}
	reg := session.NewRegistry(prov)
	reg.PermGate = gate
	if _, err := reg.GetOrSpawnEntry(context.Background(), "", "recording"); err == nil {
		t.Fatal("recordingProvider must refuse to spawn, so no entry is registered")
	}

	return []spawnPathEnv{
		{name: "chat", env: append(os.Environ(), prov.lastCfg.Env...)},
		{name: "shell", env: buildPTYEnv(gate)},
	}
}

const permHookStdin = `{"tool_name":"Bash","tool_input":{"command":"echo hi"}}`

// compilePermHook builds the real PreToolUse hook. The hook is a string constant
// compiled at runtime, so nothing short of running it can establish that the
// variable names the spawn paths write are the names it reads.
func compilePermHook(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("no go toolchain on PATH: %v", err)
	}
	binPath := filepath.Join(t.TempDir(), "tether-permission-hook")
	if err := cchook.EnsureHookBinary(binPath); err != nil {
		t.Fatalf("EnsureHookBinary: %v", err)
	}
	return binPath
}

// runPermHook runs the hook with exactly env and returns its exit code. Only 2
// blocks a tool call; cc treats every other non-zero code as non-blocking, so 0
// and 2 are the only two answers worth asserting on.
func runPermHook(t *testing.T, binPath string, env []string) int {
	t.Helper()
	cmd := exec.Command(binPath)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(permHookStdin)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	if stderr.Len() > 0 {
		t.Logf("hook stderr: %s", strings.TrimSpace(stderr.String()))
	}
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

// TestBothCCSpawnPathsCarryTheGate asserts the property that matters — what the
// real hook DOES with the environment each spawn path produces — rather than
// which strings that environment contains. A correct Gate.Env() and a correct
// hook can still disagree about a variable name, and only running one against
// the other rules that out.
//
// Each arm is chosen so that dropping the gate wiring from a path flips that
// path's answer, which is not automatic here:
//
//   - "armed and reachable" cannot rest on the exit code. A path that dropped
//     the endpoint produces an unmarked child, the hook takes its "not our cc"
//     branch and ALSO exits 0 — the same code as a fully wired path whose daemon
//     said allow. What separates them is whether the daemon was asked at all, so
//     this arm counts requests.
//   - "marked, no endpoint" is the arm the mark exists for, and there the exit
//     code IS the discriminator: 2 with the mark, 0 without it. That is the
//     fail-open tether#117 A4b described, reduced to one comparison.
//   - "unmanaged" is the opposite guard: the mark must never leak onto a child
//     of a daemon that opted out (TETHER_NO_PERMISSION_HOOK=1), or the opt-out
//     turns into a hard deny on every tool call.
func TestBothCCSpawnPathsCarryTheGate(t *testing.T) {
	// Neutralise the ambient environment. Both paths start from os.Environ(), so
	// a TETHER_DAEMON_MANAGED inherited from whatever launched `go test` would
	// otherwise mark children this test needs to observe as UNMARKED, and the
	// "unmanaged" arm would report the wiring healthy no matter what it does.
	// Setting to empty is enough: the hook compares os.Getenv against "".
	t.Setenv(cchook.EnvManaged, "")
	t.Setenv(cchook.EnvEndpoint, "")

	hook := compilePermHook(t)

	t.Run("armed and reachable: the daemon is asked and its answer honoured", func(t *testing.T) {
		var asked atomic.Int32
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			asked.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"allow":true}`))
		}))
		defer srv.Close()

		gate := cchook.Gate{Managed: true, Endpoint: srv.URL + "/api/v1/permission/request"}
		for _, p := range spawnPathEnvs(t, gate) {
			t.Run(p.name, func(t *testing.T) {
				before := asked.Load()
				if code := runPermHook(t, hook, p.env); code != 0 {
					t.Errorf("hook exit = %d, want 0 (the daemon allowed this call)", code)
				}
				if got := asked.Load() - before; got != 1 {
					t.Errorf("the daemon was asked %d times, want 1. Exit 0 alone does not "+
						"distinguish 'asked and allowed' from 'never asked because the %s path "+
						"handed the child no endpoint' — this count is what does",
						got, p.name)
				}
			})
		}
	})

	t.Run("marked but no endpoint: fails closed", func(t *testing.T) {
		for _, p := range spawnPathEnvs(t, cchook.Gate{Managed: true}) {
			t.Run(p.name, func(t *testing.T) {
				if code := runPermHook(t, hook, p.env); code != 2 {
					t.Errorf("hook exit = %d, want 2 (deny). The %s path handed its child no %s, "+
						"so the hook cannot tell a cc this daemon spawned from the owner's own "+
						"cc and lets every tool call through",
						code, p.name, cchook.EnvManaged)
				}
			})
		}
	})

	t.Run("unmanaged: the child is left completely unmarked", func(t *testing.T) {
		for _, p := range spawnPathEnvs(t, cchook.Gate{}) {
			t.Run(p.name, func(t *testing.T) {
				if code := runPermHook(t, hook, p.env); code != 0 {
					t.Errorf("hook exit = %d, want 0. A daemon that opted out of the hook "+
						"(TETHER_NO_PERMISSION_HOOK=1) must leave the %s path's children unmarked, "+
						"or a hook entry surviving in settings.json denies every tool call",
						code, p.name)
				}
			})
		}
	})
}

// TestBuildPTYEnv_ShellPathAddsExactlyTheGatesEntries pins the shell half at a
// level the round-trip above cannot reach: that the entries are the gate's OWN,
// appended verbatim, and not a second set this package derived that happens to
// agree with the gate today.
//
// Stated as a difference — buildPTYEnv(gate) minus buildPTYEnv(zero gate) — so
// it also pins position, which is load-bearing. buildPTYEnv starts from
// os.Environ(), which on a daemon relaunched by a previous daemon can already
// carry a TETHER_DAEMON_PERM_ENDPOINT for a port nobody is listening on. exec
// keeps the LAST value of a duplicated key, so the gate's entries win only if
// they are the tail; the stale values seeded below are what makes that
// observable instead of vacuous.
func TestBuildPTYEnv_ShellPathAddsExactlyTheGatesEntries(t *testing.T) {
	t.Setenv(cchook.EnvManaged, "stale-mark")
	t.Setenv(cchook.EnvEndpoint, "https://127.0.0.1:9/a-port-nobody-is-listening-on")

	gate := cchook.Gate{Managed: true, Endpoint: "https://127.0.0.1:8443/api/v1/permission/request"}

	// The zero gate injects nothing, so this is the shell env with no gate at all.
	base := buildPTYEnv(cchook.Gate{})
	armed := buildPTYEnv(gate)

	want := gate.Env()
	if len(armed) != len(base)+len(want) {
		t.Fatalf("buildPTYEnv added %d entries, want exactly the gate's %d (%v)",
			len(armed)-len(base), len(want), want)
	}
	for i, w := range want {
		if got := armed[len(base)+i]; got != w {
			t.Errorf("shell env entry %d after the base = %q, want the gate's %q", i, got, w)
		}
	}
}
