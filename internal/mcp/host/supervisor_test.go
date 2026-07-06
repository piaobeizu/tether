// internal/mcp/host/supervisor_test.go
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

// fakeConn is a ServerConn whose Wait() blocks until Close() is called.
type fakeConn struct {
	crashErr error // nil → clean close; non-nil → crash
	closeCh  chan struct{}
	listResp []mcp.Tool
}

func newFakeConn(crashErr error, listResp ...mcp.Tool) *fakeConn {
	return &fakeConn{crashErr: crashErr, closeCh: make(chan struct{}), listResp: listResp}
}

func (f *fakeConn) ListTools(_ context.Context) ([]mcp.Tool, error) { return f.listResp, nil }
func (f *fakeConn) CallTool(_ context.Context, _ string, _ json.RawMessage) (*mcp.CallToolResult, error) {
	return nil, errors.New("not implemented in fake")
}
func (f *fakeConn) Wait() error {
	<-f.closeCh
	return f.crashErr
}
func (f *fakeConn) Close() error {
	select {
	case <-f.closeCh:
	default:
		close(f.closeCh)
	}
	return nil
}

type crashLogger struct {
	events []string
}

func (l *crashLogger) Append(eventType string, _ any) error {
	l.events = append(l.events, eventType)
	return nil
}

func TestSupervisorExhaustsRetries(t *testing.T) {
	origDelays := host.RetryDelays
	host.RetryDelays = []time.Duration{5 * time.Millisecond, 5 * time.Millisecond, 5 * time.Millisecond}
	defer func() { host.RetryDelays = origDelays }()

	reg := registry.New()
	logger := &crashLogger{}

	var connectCalls atomic.Int32
	connectFn := func() (host.ServerConn, []mcp.Tool, error) {
		connectCalls.Add(1)
		conn := newFakeConn(errors.New("crash"))
		go conn.Close()
		return conn, nil, nil
	}

	// Initial conn crashes immediately (separate from connectFn).
	initialConn := newFakeConn(errors.New("initial crash"))
	go initialConn.Close()

	cfg := host.ServerConfig{Name: "test-srv"}
	sup := host.NewSupervisor(cfg, reg, logger, initialConn, connectFn, func() { reg.Deregister(cfg.Name) })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sup.Run(ctx)

	// 3 retry connects (initial conn is separate, not counted in connectFn).
	if n := connectCalls.Load(); n != 3 {
		t.Fatalf("expected 3 connect calls, got %d", n)
	}
	if len(logger.events) == 0 || logger.events[len(logger.events)-1] != "mcp_server_crashed" {
		t.Fatalf("expected mcp_server_crashed event, got %v", logger.events)
	}
}

func TestSupervisorCleanShutdown(t *testing.T) {
	reg := registry.New()
	logger := &crashLogger{}

	var mu sync.Mutex
	var conn *fakeConn
	connectFn := func() (host.ServerConn, []mcp.Tool, error) {
		c := newFakeConn(nil)
		mu.Lock()
		conn = c
		mu.Unlock()
		return c, nil, nil
	}

	// Initial conn: clean close (nil crash error).
	initialConn := newFakeConn(nil)

	cfg := host.ServerConfig{Name: "srv2"}
	sup := host.NewSupervisor(cfg, reg, logger, initialConn, connectFn, func() { reg.Deregister(cfg.Name) })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sup.Run(ctx); close(done) }()

	time.Sleep(20 * time.Millisecond)
	cancel()
	initialConn.Close() // unblock initialConn.Wait()
	mu.Lock()
	c := conn
	mu.Unlock()
	if c != nil {
		c.Close()
	}

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("supervisor did not stop after context cancel")
	}
	if len(logger.events) > 0 {
		t.Fatalf("clean shutdown must not log crash events, got %v", logger.events)
	}
}

func TestSupervisorDeregistersBeforeRetry(t *testing.T) {
	origDelays := host.RetryDelays
	host.RetryDelays = []time.Duration{5 * time.Millisecond, 5 * time.Millisecond, 5 * time.Millisecond}
	defer func() { host.RetryDelays = origDelays }()

	reg := registry.New()
	logger := &crashLogger{}

	var calls atomic.Int32
	tools := []mcp.Tool{{Name: "foo"}}
	connectFn := func() (host.ServerConn, []mcp.Tool, error) {
		calls.Add(1)
		return nil, nil, errors.New("connect fail")
	}

	// Initial conn has tools registered by Manager (simulated here).
	initialConn := newFakeConn(errors.New("crash"), tools...)
	cfg := host.ServerConfig{Name: "srv3"}
	// Pre-register tools as Manager.startOne would.
	reg.Register(cfg, tools)
	go initialConn.Close()

	sup := host.NewSupervisor(cfg, reg, logger, initialConn, connectFn, func() { reg.Deregister(cfg.Name) })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sup.Run(ctx)

	for _, e := range reg.ListAll() {
		if e.ServerName == "srv3" {
			t.Fatalf("registry still has tool from crashed server: %+v", e)
		}
	}
}

// TestSupervisorDeregisterHonorsGenerationGuard proves the supervisor routes its
// deregister through the injected callback, and that a generation-guarded closure
// makes a stale generation's clean-shutdown deregister a no-op — so a newer Start
// that has re-registered the same name keeps its tools.
func TestSupervisorDeregisterHonorsGenerationGuard(t *testing.T) {
	reg := registry.New()
	logger := &crashLogger{}

	cfg := host.ServerConfig{Name: "srvG"}
	tools := []mcp.Tool{{Name: "foo"}}
	// Register the tool as a (newer) generation's Start would.
	if err := reg.Register(cfg, tools); err != nil {
		t.Fatalf("register: %v", err)
	}

	// genCur models the Manager's current generation; myGen is this supervisor's.
	var genCur atomic.Int64
	genCur.Store(1)
	const myGen = 1
	dereg := func() {
		if genCur.Load() == myGen {
			reg.Deregister("srvG")
		}
	}

	// Clean-shutdown fakeConn (nil crashErr): Wait() unblocks on Close().
	initialConn := newFakeConn(nil)
	connectFn := func() (host.ServerConn, []mcp.Tool, error) {
		return newFakeConn(nil), nil, nil
	}
	sup := host.NewSupervisor(cfg, reg, logger, initialConn, connectFn, dereg)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sup.Run(ctx); close(done) }()

	// A newer generation supersedes this supervisor before it cleans up.
	genCur.Store(2)

	// Trigger the clean-shutdown deregister path.
	cancel()
	initialConn.Close()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("supervisor did not stop after context cancel")
	}

	// The stale-generation deregister must have no-op'd: the tool survives.
	found := false
	for _, e := range reg.ListAll() {
		if e.ServerName == "srvG" {
			found = true
		}
	}
	if !found {
		t.Fatal("stale-generation deregister wiped a newer generation's tool")
	}
}
