package lifecycle_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/mcp/host"
	"github.com/piaobeizu/tether/internal/mcp/instance"
	"github.com/piaobeizu/tether/internal/mcp/lifecycle"
)

// loopbackAnswers reports whether something on 127.0.0.1:port is still an
// MCPInstance loopback: the bearer check rejects an unauthenticated /mcp request
// with 401 before the request reaches the MCP handler, and nothing else in this
// test binary listens, so a 401 identifies a live instance.
//
// Liveness is asked this way rather than by dialling, because a bare TCP connect
// also succeeds against a port the kernel has since handed to something else.
func loopbackAnswers(port int) bool {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/mcp", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusUnauthorized
}

// portIsFree reports whether port can be bound again — the only check that
// distinguishes an instance that was shut down from one that merely stopped
// being reachable through the manager.
func portIsFree(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// TestStartTaskConcurrentSameTaskKeepsOneAlive pins the invariant the type
// documents for itself — "at most one instance per task is running at any time"
// — against the trigger that reaches it: two POST /api/v1/tasks/{id}/mcp in
// flight at once (a double click, an SPA retry). The handler adds no
// serialization of its own, so whatever holds this line has to be here.
//
// What made the invariant false was the window between deciding to replace the
// existing instance and publishing the replacement: every caller built and
// started its own instance inside that window, and only the last one to reach
// the map was ever tracked. The others stayed fully alive — loopback bound,
// bearer token valid, child servers running — with nothing left holding a
// reference to stop them, so StopAll could not reach them either.
//
// Both assertions are about released resources rather than returned errors, and
// deliberately not about len(Active()): the map only ever held one entry, so
// counting it is true before and after and gates nothing.
func TestStartTaskConcurrentSameTaskKeepsOneAlive(t *testing.T) {
	const racers = 6

	pm := newPermManager()
	lm := lifecycle.New()
	ctx := context.Background()

	// One shared config, including WsRoot: these are competing starts of the
	// same task, and a per-caller temp dir would make them distinguishable in a
	// way the real double click is not.
	cfg := baseConfig(t, pm, "t-01STARTRACE", "start-race")

	var wg sync.WaitGroup
	var mu sync.Mutex
	got := make([]*instance.MCPInstance, 0, racers)
	errs := make([]error, 0, racers)

	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			inst, err := lm.StartTask(ctx, cfg)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			got = append(got, inst)
		}()
	}
	close(start)
	wg.Wait()

	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		lm.StopAll(c)
	})

	for _, err := range errs {
		t.Fatalf("StartTask failed: %v", err)
	}

	tracked := lm.Active()
	ports := make([]int, 0, len(got))
	alive := make([]int, 0, len(got))
	for _, inst := range got {
		ports = append(ports, inst.Port)
		if loopbackAnswers(inst.Port) {
			alive = append(alive, inst.Port)
		}
	}
	t.Logf("%d concurrent StartTask for one TaskID -> %d instances, ports %v; "+
		"manager tracks %d; still answering on the loopback: %v",
		racers, len(got), ports, len(tracked), alive)

	if len(alive) != 1 {
		t.Fatalf("%d of the %d instances built for one task are still serving on their "+
			"loopback (%v); the manager tracks %d of them, so the rest cannot be stopped "+
			"by anyone", len(alive), len(got), alive, len(tracked))
	}
	survivor := alive[0]
	if len(tracked) != 1 || tracked[0].Port != survivor {
		t.Fatalf("the instance still serving (port %d) is not the one the manager tracks (%v)",
			survivor, tracked)
	}

	// Every instance the manager decided to replace must have had its port
	// released at that moment; only the survivor keeps its port until StopAll.
	for _, inst := range got {
		if inst.Port == survivor {
			continue
		}
		if !portIsFree(inst.Port) {
			t.Fatalf("port %d is still bound after its instance was superseded: the "+
				"replaced instance was dropped without being stopped", inst.Port)
		}
	}

	stopCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	lm.StopAll(stopCtx)

	for _, port := range ports {
		if !portIsFree(port) {
			t.Fatalf("port %d is still bound after StopAll: an instance survived the "+
				"manager that was supposed to own it", port)
		}
	}
	if n := len(lm.Active()); n != 0 {
		t.Fatalf("expected 0 tracked instances after StopAll, got %d", n)
	}
}

// TestStartTaskSlowStartDoesNotBlockOtherTasks keeps the serialization that
// fixes the race above scoped to one task.
//
// A manager-wide start lock passes every assertion in that test and quietly
// serializes the daemon: starting a task means spawning child MCP servers and
// completing an MCP handshake with each, so one task whose server is slow — or
// broken — would stall every other task's start behind it. This test makes that
// difference observable by holding one task's start open for a known interval
// and requiring an unrelated task to get through in the meantime.
//
// `sleep` is not an MCP server, so the handshake for the slow task blocks until
// the child exits and the transport closes; that is what bounds this test's
// runtime and what makes the blocked start clean itself up.
func TestStartTaskSlowStartDoesNotBlockOtherTasks(t *testing.T) {
	const slowChildLife = 3 * time.Second
	const fastBudget = 1500 * time.Millisecond

	pm := newPermManager()
	lm := lifecycle.New()
	ctx := context.Background()

	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		lm.StopAll(c)
	})

	slowCfg := baseConfig(t, pm, "t-01SLOWSTART", "slow-start")
	slowCfg.ExtraServers = map[string]host.ServerConfig{
		// Speaks nothing; the connect blocks until this exits.
		"never-answers": {Command: []string{"sleep", fmt.Sprint(int(slowChildLife.Seconds()))}},
	}

	slowDone := make(chan time.Duration, 1)
	go func() {
		begin := time.Now()
		_, err := lm.StartTask(ctx, slowCfg)
		if err == nil {
			// Not a failure of this test's subject, but it would mean the start
			// did not block and the test measured nothing.
			t.Log("slow StartTask unexpectedly succeeded")
		}
		slowDone <- time.Since(begin)
	}()

	// Give the slow start time to be inside StartTask and past the point where a
	// manager-wide lock would already be held.
	time.Sleep(300 * time.Millisecond)

	fastCfg := baseConfig(t, pm, "t-01FASTSTART", "fast-start")
	begin := time.Now()
	if _, err := lm.StartTask(ctx, fastCfg); err != nil {
		t.Fatalf("StartTask for an unrelated task failed: %v", err)
	}
	fastElapsed := time.Since(begin)

	slowElapsed := <-slowDone
	t.Logf("unrelated task started in %s while a %s start for another task was in flight "+
		"(that one returned after %s)",
		fastElapsed.Round(time.Millisecond), slowChildLife, slowElapsed.Round(time.Millisecond))

	if fastElapsed > fastBudget {
		t.Fatalf("starting an unrelated task took %s while another task's start was in "+
			"flight: starts are serialized across tasks, not per task", fastElapsed)
	}
	if slowElapsed < time.Second {
		t.Fatalf("the slow start returned after only %s, so nothing was in flight while "+
			"the other task started and this test measured nothing", slowElapsed)
	}
}
