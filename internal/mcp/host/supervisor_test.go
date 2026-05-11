// internal/mcp/host/supervisor_test.go
package host_test

import (
	"context"
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
	crashErr error      // nil → clean close; non-nil → crash
	closeCh  chan struct{}
	listResp []mcp.Tool
}

func newFakeConn(crashErr error, listResp ...mcp.Tool) *fakeConn {
	return &fakeConn{crashErr: crashErr, closeCh: make(chan struct{}), listResp: listResp}
}

func (f *fakeConn) ListTools(_ context.Context) ([]mcp.Tool, error) { return f.listResp, nil }
func (f *fakeConn) CallTool(_ context.Context, _ string, _ map[string]any) (*mcp.CallToolResult, error) {
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
		go conn.Close() // immediately trigger crash
		return conn, nil, nil
	}

	cfg := host.ServerConfig{Name: "test-srv"}
	sup := host.NewSupervisor(cfg, reg, logger, connectFn)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sup.Run(ctx)

	// 1 initial + 3 retries = 4 total connects
	if n := connectCalls.Load(); n != 4 {
		t.Fatalf("expected 4 connect calls, got %d", n)
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

	cfg := host.ServerConfig{Name: "srv2"}
	sup := host.NewSupervisor(cfg, reg, logger, connectFn)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sup.Run(ctx); close(done) }()

	time.Sleep(20 * time.Millisecond)
	cancel()
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
	connectFn := func() (host.ServerConn, []mcp.Tool, error) {
		n := calls.Add(1)
		tools := []mcp.Tool{{Name: "foo"}}
		if n == 1 {
			conn := newFakeConn(errors.New("crash"), tools...)
			go conn.Close()
			return conn, tools, nil
		}
		return nil, nil, errors.New("connect fail")
	}

	cfg := host.ServerConfig{Name: "srv3"}
	sup := host.NewSupervisor(cfg, reg, logger, connectFn)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sup.Run(ctx)

	for _, e := range reg.ListAll() {
		if e.ServerName == "srv3" {
			t.Fatalf("registry still has tool from crashed server: %+v", e)
		}
	}
}
