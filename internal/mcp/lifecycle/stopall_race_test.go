package lifecycle_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/piaobeizu/tether/internal/mcp/host"
	"github.com/piaobeizu/tether/internal/mcp/instance"
	"github.com/piaobeizu/tether/internal/mcp/lifecycle"
)

// delayedStdioServerEnv names the env var that turns
// TestStdioMCPServerDelayedHelper into a real stdio MCP server, carrying the
// number of milliseconds it waits before answering anything.
const delayedStdioServerEnv = "TETHER_TEST_MCP_STDIO_DELAY_MS"

// TestStdioMCPServerDelayedHelper is not a real test: re-exec'd with
// delayedStdioServerEnv set it sleeps for that many milliseconds and then serves
// a minimal stdio MCP server over stdin/stdout.
//
// The sleep is this package's only handle on where a StartTask is when something
// else happens to the manager. mcp/host's initial connect is synchronous, so a
// start sits in the handshake with its child server for as long as the child
// takes to answer — which is after the point where the start could have declined
// to run at all, and before the point where it publishes its instance into the
// map. Trying to land inside that window by racing two goroutines instead would
// give a test that passes on a fast machine and reports nothing.
func TestStdioMCPServerDelayedHelper(t *testing.T) {
	ms := os.Getenv(delayedStdioServerEnv)
	if ms == "" {
		t.Skip("helper process only")
	}
	d, err := strconv.Atoi(ms)
	if err != nil {
		t.Fatalf("%s=%q is not a number of milliseconds", delayedStdioServerEnv, ms)
	}
	time.Sleep(time.Duration(d) * time.Millisecond)
	srv := mcp.NewServer(&mcp.Implementation{Name: "test-stdio-delayed", Version: "v0.0.1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "noop", Description: "no-op"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
			return &mcp.CallToolResult{}, struct{}{}, nil
		})
	_ = srv.Run(context.Background(), &mcp.StdioTransport{})
}

// delayedStdioServerCommand returns argv that re-execs the test binary as the
// delayed stdio MCP server above.
func delayedStdioServerCommand() []string {
	return []string{os.Args[0], "-test.run=^TestStdioMCPServerDelayedHelper$"}
}

// delayedStdioServerEnvMap returns the env that activates that helper with the
// given answer delay.
func delayedStdioServerEnvMap(d time.Duration) map[string]string {
	return map[string]string{delayedStdioServerEnv: strconv.Itoa(int(d.Milliseconds()))}
}

// TestStartTaskAfterStopAllPublishesNothing pins the fully-ordered case of the
// start-versus-shutdown defect, with no concurrency in it at all: StopAll has
// returned, and a start arrives afterwards.
//
// StopAll is the daemon's shutdown path (internal/server/lifecycle.go stops the
// per-task instances, then the global stack, then exits), so anything published
// after it returns has nobody left to stop it: the loopback stays bound, the
// bearer token stays valid, the child servers stay up and the instance-scoped
// context stays alive for as long as the machine is up.
//
// The assertions are about what is left running rather than about the error,
// deliberately: refusing the start is one way to hold the line, and cleaning up
// after allowing it is another, and this test should hold for either.
func TestStartTaskAfterStopAllPublishesNothing(t *testing.T) {
	pm := newPermManager()
	lm := lifecycle.New()
	ctx := context.Background()

	stopCtx, stopCancel := context.WithTimeout(ctx, 10*time.Second)
	defer stopCancel()
	lm.StopAll(stopCtx)

	inst, err := lm.StartTask(ctx, baseConfig(t, pm, "t-01POSTSTOPALL", "post-stopall"))
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		lm.StopAll(c)
		if inst != nil {
			_ = inst.Stop(c)
		}
	})

	if n := len(lm.Active()); n != 0 {
		t.Fatalf("the manager tracks %d instance(s) after a StartTask that arrived once StopAll "+
			"had already returned; nothing will ever stop them", n)
	}
	if err != nil {
		return
	}
	t.Logf("StartTask after StopAll returned an instance on port %d with no error", inst.Port)
	if loopbackAnswers(inst.Port) {
		t.Fatalf("port %d is still serving after a StartTask that arrived once StopAll had "+
			"returned: the instance outlived the shutdown it was started behind", inst.Port)
	}
	if !portIsFree(inst.Port) {
		t.Fatalf("port %d is still bound after a StartTask that arrived once StopAll had "+
			"returned", inst.Port)
	}
}

// TestStartTaskDuringStopAllPublishesNothing pins the concurrent case: a start
// that is already in flight when shutdown begins, for a task StopAll therefore
// never sees.
//
// StopAll snapshots the task ids under the manager lock and then stops them one
// at a time. The per-task gate that serializes two starts of one task does not
// reach this: it serializes per task, and the task here is by construction not
// one the snapshot contains. So the ordering that matters is between the
// snapshot and the publish, and it is decided inside the manager, not by who
// calls first.
//
// The window is opened rather than raced for. The late start's child server
// answers only after a fixed delay, so the start is provably still inside its
// handshake when StopAll runs — the same fixture in both the broken and the
// fixed arm, which leaves the manager as the only difference between them. Two
// checks below refuse to let the test pass without that window: the late start
// must finish after StopAll returned, and it must have taken long enough that
// the delay was really in play.
func TestStartTaskDuringStopAllPublishesNothing(t *testing.T) {
	const answerDelay = 1500 * time.Millisecond

	pm := newPermManager()
	lm := lifecycle.New()
	ctx := context.Background()
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		lm.StopAll(c)
	})

	// A task the snapshot will contain, so the shutdown loop has work to do and
	// this is not merely the ordered case above with extra steps.
	if _, err := lm.StartTask(ctx, baseConfig(t, pm, "t-01INSNAPSHOT", "in-snapshot")); err != nil {
		t.Fatalf("StartTask for the snapshotted task: %v", err)
	}

	late := baseConfig(t, pm, "t-01LATEPUBLISH", "late-publish")
	late.ExtraServers = map[string]host.ServerConfig{
		"delayed": {
			Command: delayedStdioServerCommand(),
			Env:     delayedStdioServerEnvMap(answerDelay),
			Prefix:  "delayed",
		},
	}

	type startOutcome struct {
		inst    *instance.MCPInstance
		err     error
		endedAt time.Time
	}
	lateDone := make(chan startOutcome, 1)
	lateBegan := time.Now()
	go func() {
		i, err := lm.StartTask(ctx, late)
		lateDone <- startOutcome{inst: i, err: err, endedAt: time.Now()}
	}()

	// Long enough for the goroutine above to be inside the child handshake, and
	// well short of answerDelay so it is still there.
	time.Sleep(300 * time.Millisecond)

	stopCtx, stopCancel := context.WithTimeout(ctx, 15*time.Second)
	defer stopCancel()
	lm.StopAll(stopCtx)
	stopAllEndedAt := time.Now()

	got := <-lateDone
	lateElapsed := got.endedAt.Sub(lateBegan)
	t.Logf("a start that began %s before StopAll returned ended %s after it (took %s); err=%v",
		stopAllEndedAt.Sub(lateBegan).Round(time.Millisecond),
		got.endedAt.Sub(stopAllEndedAt).Round(time.Millisecond),
		lateElapsed.Round(time.Millisecond), got.err)

	if !got.endedAt.After(stopAllEndedAt) {
		t.Fatalf("the late start finished before StopAll returned, so it was never in the "+
			"window this test is about and nothing here was measured (start took %s)", lateElapsed)
	}
	if lateElapsed < answerDelay/2 {
		t.Fatalf("the late start took only %s against a %s child answer delay, so it never "+
			"reached the handshake and nothing here was measured", lateElapsed, answerDelay)
	}

	if n := len(lm.Active()); n != 0 {
		t.Fatalf("StopAll returned and the manager still tracks %d instance(s): a start that "+
			"was in flight published one behind the shutdown", n)
	}
	if got.err != nil {
		return
	}
	if loopbackAnswers(got.inst.Port) {
		t.Fatalf("port %d is still serving after StopAll returned: the start that was in "+
			"flight outlived the shutdown", got.inst.Port)
	}
	if !portIsFree(got.inst.Port) {
		t.Fatalf("port %d is still bound after StopAll returned: the instance a start "+
			"published behind the shutdown was never released", got.inst.Port)
	}
}

// TestStartTaskAfterStopAllSpawnsNoChildProcess pins the other half of what
// refusing a start during shutdown is for: not doing the work in the first
// place.
//
// Building an instance is not free of side effects outside the manager. New()
// binds a loopback port before Start() is even called, Start() spawns one child
// process per configured MCP server, and unless SkipInject is set it writes an
// mcpServers entry into the user's agent settings file that only Stop() takes
// back out. Doing all of that during shutdown and then undoing it leaves a
// window in which the daemon exits partway through — with a settings entry
// pointing at a port nothing is listening on, and orphaned children. Declining
// at the door removes the window instead of shrinking it.
func TestStartTaskAfterStopAllSpawnsNoChildProcess(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "spawned")
	spawnMarker := map[string]host.ServerConfig{
		// Not an MCP server: it writes the marker and exits, which ends the
		// handshake and makes StartTask report an error. The spawn is the part
		// under test, not the outcome of the start.
		"marker": {Command: []string{"sh", "-c", "printf x > '" + marker + "'"}},
	}

	// Positive control. "The marker never appeared" is also what a config that
	// could never spawn anything looks like, so prove the config does spawn when
	// the manager is live before reading anything into its absence.
	func() {
		live := lifecycle.New()
		cfg := baseConfig(t, newPermManager(), "t-01SPAWNCONTROL", "spawn-control")
		cfg.ExtraServers = spawnMarker
		_, err := live.StartTask(context.Background(), cfg)
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		live.StopAll(c)
		if _, statErr := os.Stat(marker); statErr != nil {
			t.Fatalf("positive control: this server config spawned nothing on a live manager "+
				"(%v, StartTask err=%v), so the assertion below would hold for the wrong reason",
				statErr, err)
		}
		if rmErr := os.Remove(marker); rmErr != nil {
			t.Fatalf("positive control: remove marker: %v", rmErr)
		}
	}()

	lm := lifecycle.New()
	ctx := context.Background()
	stopCtx, stopCancel := context.WithTimeout(ctx, 10*time.Second)
	defer stopCancel()
	lm.StopAll(stopCtx)

	cfg := baseConfig(t, newPermManager(), "t-01SPAWNSHUTDOWN", "spawn-shutdown")
	cfg.ExtraServers = spawnMarker
	inst, err := lm.StartTask(ctx, cfg)
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		lm.StopAll(c)
		if inst != nil {
			_ = inst.Stop(c)
		}
	})
	t.Logf("StartTask after StopAll: err=%v", err)

	// Room for a spawn that did happen to reach the marker.
	time.Sleep(300 * time.Millisecond)
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatalf("a StartTask that arrived after StopAll spawned a child MCP server process "+
			"(%s exists): shutdown is spawning processes it then has to take back down", marker)
	}
}
