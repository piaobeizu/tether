// internal/mcp/host/supervisor_retry_test.go
package host_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/piaobeizu/tether/internal/mcp/host"
	"github.com/piaobeizu/tether/internal/mcp/registry"
)

// countingConn is a ServerConn that records what the supervisor did with it.
// The counters are the point: TestSupervisorExhaustsRetries can only see how
// many connections were created, and its fake conns crash the instant they are
// handed over, so it cannot tell a connection that was supervised from one that
// was registered and thrown away.
type countingConn struct {
	crashErr error
	tools    []mcp.Tool

	closeCh   chan struct{}
	closeOnce sync.Once

	waited    chan struct{} // closed when Wait() is first entered
	waitsOnce sync.Once

	waits  atomic.Int32
	closes atomic.Int32
}

func newCountingConn(crashErr error, tools ...mcp.Tool) *countingConn {
	return &countingConn{
		crashErr: crashErr,
		tools:    tools,
		closeCh:  make(chan struct{}),
		waited:   make(chan struct{}),
	}
}

func (c *countingConn) ListTools(context.Context) ([]mcp.Tool, error) { return c.tools, nil }
func (c *countingConn) CallTool(context.Context, string, json.RawMessage) (*mcp.CallToolResult, error) {
	return nil, errors.New("not implemented in fake")
}

func (c *countingConn) Wait() error {
	c.waits.Add(1)
	c.waitsOnce.Do(func() { close(c.waited) })
	<-c.closeCh
	return c.crashErr
}

func (c *countingConn) Close() error {
	c.closes.Add(1)
	c.closeOnce.Do(func() { close(c.closeCh) })
	return nil
}

// TestSupervisorLastRestartIsSupervised pins that every restart the supervisor
// spends is a restart the server actually gets.
//
// The loop counted one iteration per crash and did its reconnect at the end of
// the iteration, so the reconnect belonging to the last iteration was spawned,
// had its tools registered, and was then dropped by the loop condition — closed
// and deregistered without ever being waited on. Three consequences, in
// ascending order of how much they cost: a child process, an MCP handshake and a
// tools/list round trip paid for nothing after a 4s backoff; a window between
// Register and deregister in which the registry observer advertises tools that
// immediately vanish, so a tools/list landing there is a lie; and a server that
// crashed twice but would have been stable on its third start killed anyway. The
// documented three restarts were two.
func TestSupervisorLastRestartIsSupervised(t *testing.T) {
	origDelays := host.RetryDelays
	host.RetryDelays = []time.Duration{5 * time.Millisecond, 5 * time.Millisecond, 5 * time.Millisecond}
	defer func() { host.RetryDelays = origDelays }()

	reg := registry.New()
	logger := &crashLogger{}
	cfg := host.ServerConfig{Name: "srv-last", Prefix: "last"}
	tools := []mcp.Tool{{Name: "stable"}}

	var connectCalls atomic.Int32
	healthyCh := make(chan *countingConn, 1)
	connectFn := func() (host.ServerConn, []mcp.Tool, error) {
		n := connectCalls.Add(1)
		if n < 3 {
			c := newCountingConn(errors.New("crash"))
			go c.Close() // crashes as soon as it is handed over
			return c, nil, nil
		}
		// The third replacement is the one that would have stabilised.
		c := newCountingConn(errors.New("crash"), tools...)
		healthyCh <- c
		return c, tools, nil
	}

	initialConn := newCountingConn(errors.New("initial crash"))
	go initialConn.Close()

	sup := host.NewSupervisor(cfg, reg, logger, initialConn, connectFn, func() { reg.Deregister(cfg.Name) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { sup.Run(ctx); close(done) }()

	var healthy *countingConn
	select {
	case healthy = <-healthyCh:
	case <-done:
		t.Fatalf("Run returned after %d connect calls without ever producing a third "+
			"replacement", connectCalls.Load())
	case <-time.After(5 * time.Second):
		t.Fatalf("no third replacement after 5s; connect calls: %d", connectCalls.Load())
	}

	// The third replacement is healthy: its Wait() blocks. The supervisor must be
	// sitting in it.
	select {
	case <-healthy.waited:
	case <-done:
		t.Fatalf("Run returned instead of supervising the third replacement it spawned "+
			"(Wait calls on it: %d, Close calls: %d, tools left in the registry: %d): the "+
			"last restart was spent on a connection that was never given a chance to run",
			healthy.waits.Load(), healthy.closes.Load(), len(reg.ListAll()))
	case <-time.After(5 * time.Second):
		t.Fatalf("the third replacement was never waited on after 5s (Wait: %d, Close: %d)",
			healthy.waits.Load(), healthy.closes.Load())
	}

	if n := healthy.closes.Load(); n != 0 {
		t.Fatalf("the third replacement was closed %d time(s) while it was still healthy", n)
	}
	// Its tools have to stay advertised for as long as it is being supervised;
	// registering and deregistering them back to back is the tools/list lie.
	found := false
	for _, e := range reg.ListAll() {
		if e.ServerName == cfg.Name {
			found = true
		}
	}
	if !found {
		t.Fatalf("the third replacement's tools are not in the registry while it is being "+
			"supervised: %+v", reg.ListAll())
	}
	t.Logf("third replacement is being supervised: Wait=%d Close=%d registry entries=%d",
		healthy.waits.Load(), healthy.closes.Load(), len(reg.ListAll()))

	// Now let it crash too: the retries are spent, so the supervisor gives up
	// without paying for a fourth connection.
	healthy.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the last supervised connection crashed")
	}

	if n := connectCalls.Load(); n != 3 {
		t.Fatalf("expected exactly 3 reconnects, got %d", n)
	}
	if len(logger.events) == 0 || logger.events[len(logger.events)-1] != "mcp_server_crashed" {
		t.Fatalf("expected mcp_server_crashed once the retries were spent, got %v", logger.events)
	}
	for _, e := range reg.ListAll() {
		if e.ServerName == cfg.Name {
			t.Fatalf("tools left in the registry after the supervisor gave up: %+v", e)
		}
	}
	t.Logf("connect calls=%d events=%v", connectCalls.Load(), logger.events)
}
