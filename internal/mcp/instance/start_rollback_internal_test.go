package instance

// White-box tests for the rollback Start owes on failure. Internal package so
// the child-connection map and the instance-scoped context can be inspected
// directly, the same reason hibernate_internal_test.go is internal.

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/mcp/host"
	"github.com/piaobeizu/tether/internal/permission"
)

// portIsFree reports whether port can be bound again. New() binds the loopback
// before Start() runs, so this is the check that says whether a failed Start
// gave it back.
func portIsFree(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

func newUnstartedInstance(t *testing.T, slug string) *MCPInstance {
	t.Helper()
	inst, err := New(InstanceConfig{
		TaskID:      "t-01STARTFAIL",
		TaskSlug:    slug,
		WsRoot:      t.TempDir(),
		PermManager: permission.New(),
		SkipInject:  true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return inst
}

// TestStartFailureReleasesLoopbackPort is the deterministic half of the pair: a
// single unspawnable child, so the failure happens on the first server every
// time and the only resource in question is the loopback New() already bound.
//
// Stop() cannot be the answer here — it returns early while i.started is false,
// which it is on every path out of a failed Start — so the port is released by
// Start or by nobody. Each retry of a start that fails this way (one typo in
// <wsRoot>/.tether/task-config.json is enough) would otherwise strand another
// port and file descriptor for the daemon's lifetime.
func TestStartFailureReleasesLoopbackPort(t *testing.T) {
	inst := newUnstartedInstance(t, "startfail-solo")
	port := inst.Port
	if port <= 0 {
		t.Fatalf("New did not bind a loopback port: %d", port)
	}

	ctx := context.Background()
	err := inst.Start(ctx, map[string]host.ServerConfig{
		"bad": {Command: []string{"/nonexistent-tether-test-binary"}, Prefix: "bad"},
	})
	if err == nil {
		t.Fatal("expected Start to fail when the only child cannot spawn")
	}
	// Stop is called the way lifecycle.StartTask's caller would on the error
	// path, to show it changes nothing either way.
	stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = inst.Stop(stopCtx)

	free := portIsFree(port)
	t.Logf("New bound port %d; after a failed Start (%v) and Stop, port free=%v", port, err, free)
	if !free {
		t.Fatalf("port %d is still bound after a failed Start followed by Stop: the "+
			"listener New() opened has no owner left to close it", port)
	}

	// The instance-scoped context outlives the request that triggered Start by
	// design, so a failed Start has to cancel it or the goroutines and child
	// processes it scopes stay alive until the daemon exits.
	select {
	case <-inst.baseCtx.Done():
	default:
		t.Fatal("baseCtx is still live after a failed Start: everything scoped to the " +
			"instance lifetime leaks with it")
	}
}

// TestStartFailureRollsBackPartiallyStartedChildren covers the other half: the
// Manager starts servers by iterating a map and returns on the first failure,
// leaving the ones it already spawned running and registered. Which servers
// those are is not decided by this test — map iteration order is randomized —
// so the mixed case is repeated, and every repetition has to come back clean.
//
// The port assertion is the deterministic one and it is repeated here too; the
// child-connection assertion is the one whose trigger depends on the draw. This
// is the twin of the rollback Wake already does for the same call: see the
// comment on i.mgr.Start in Wake and TestWakeFailureRollsBackAndStaysDormant.
func TestStartFailureRollsBackPartiallyStartedChildren(t *testing.T) {
	const rounds = 8
	ctx := context.Background()

	leakedRounds := 0
	for round := 0; round < rounds; round++ {
		inst := newUnstartedInstance(t, fmt.Sprintf("startfail-mixed-%d", round))
		port := inst.Port

		err := inst.Start(ctx, map[string]host.ServerConfig{
			"good": {Command: stdioCmdInst(), Env: stdioEnvInst(), Prefix: "good"},
			"bad":  {Command: []string{"/nonexistent-tether-test-binary"}, Prefix: "bad"},
		})
		if err == nil {
			stopInst(inst)
			t.Fatalf("round %d: expected Start to fail when a child cannot spawn", round)
		}

		if _, ok := inst.mgr.GetConn("good"); ok {
			leakedRounds++
			t.Errorf("round %d: the 'good' child spawned before 'bad' failed is still "+
				"connected and registered after Start returned an error", round)
			// Do not leave the child running for the rest of the suite.
			inst.mgr.StopAll()
		}
		if _, ok := inst.mgr.GetConn("bad"); ok {
			t.Errorf("round %d: a conn exists for the child that could not spawn", round)
		}
		if !portIsFree(port) {
			t.Fatalf("round %d: port %d is still bound after a failed Start", round, port)
		}
		select {
		case <-inst.baseCtx.Done():
		default:
			t.Fatalf("round %d: baseCtx is still live after a failed Start", round)
		}
	}
	t.Logf("%d rounds of a mixed good/bad server map; rounds that leaked a spawned child: %d",
		rounds, leakedRounds)
}

// TestStartAfterRollbackRefuses records that a rolled-back instance is spent.
// Everything Start tore down — the loopback listener, the instance-scoped
// context — is gone for good, and i.started stayed false, so a second Start
// would otherwise walk straight through and report success while serving
// nothing. The caller's retry path is a fresh New(), which is what
// lifecycle.StartTask does.
func TestStartAfterRollbackRefuses(t *testing.T) {
	inst := newUnstartedInstance(t, "startfail-respawn")
	ctx := context.Background()

	if err := inst.Start(ctx, map[string]host.ServerConfig{
		"bad": {Command: []string{"/nonexistent-tether-test-binary"}, Prefix: "bad"},
	}); err == nil {
		t.Fatal("expected the first Start to fail")
	}

	err := inst.Start(ctx, nil)
	if err == nil {
		t.Fatal("a second Start on a rolled-back instance reported success: it has no " +
			"listener and no live context, so nothing it claims to serve is reachable")
	}
	t.Logf("second Start on a rolled-back instance: %v", err)
}
