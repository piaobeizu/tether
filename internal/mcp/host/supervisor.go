// internal/mcp/host/supervisor.go
package host

import (
	"context"
	"log/slog"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxRetries = 3

// RetryDelays controls backoff between reconnect attempts.
// Package-level var so tests can override without build tags.
var RetryDelays = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

// ConnectFn is called to establish a new connection to an MCP server.
// Returns the connection, the tools it exposed, and any error.
type ConnectFn func() (ServerConn, []mcp.Tool, error)

// ToolRegistry is the subset of registry.Registry used by Supervisor.
// Defined here to avoid an import cycle (registry imports host for ServerConfig).
type ToolRegistry interface {
	Register(cfg ServerConfig, tools []mcp.Tool) error
	Deregister(serverName string)
}

// Supervisor manages the lifecycle of a single MCP server connection,
// restarting it up to maxRetries times on crash.
type Supervisor struct {
	cfg         ServerConfig
	reg         ToolRegistry
	logger      HistoryLogger
	initialConn ServerConn // already-established conn; Run starts with this
	connectFn   ConnectFn  // called only for retries
}

// NewSupervisor creates a Supervisor. Call Run(ctx) to start it.
// initialConn is the already-established connection; connectFn is called only for retries.
func NewSupervisor(cfg ServerConfig, reg ToolRegistry, logger HistoryLogger, initialConn ServerConn, connectFn ConnectFn) *Supervisor {
	return &Supervisor{cfg: cfg, reg: reg, logger: logger, initialConn: initialConn, connectFn: connectFn}
}

// Run blocks until the server is permanently stopped (ctx cancelled or retries exhausted).
// It uses initialConn as the first connection, then calls connectFn up to maxRetries times on crash.
func (s *Supervisor) Run(ctx context.Context) {
	conn := s.initialConn

	for attempt := 1; attempt <= maxRetries; attempt++ {
		// Block until connection closes (crash or clean shutdown).
		waitErr := conn.Wait()

		// Clean shutdown.
		select {
		case <-ctx.Done():
			_ = conn.Close()
			s.reg.Deregister(s.cfg.Name)
			return
		default:
		}

		// Crash.
		slog.Warn("mcp/supervisor: server crashed", "server", s.cfg.Name, "attempt", attempt, "err", waitErr)
		s.reg.Deregister(s.cfg.Name)
		_ = conn.Close()

		// Always backoff before reconnect (even after failed connectFn — fixes I2).
		select {
		case <-ctx.Done():
			return
		case <-time.After(RetryDelays[attempt-1]):
		}

		newConn, newTools, err := s.connectFn()
		if err != nil {
			slog.Error("mcp/supervisor: reconnect failed", "server", s.cfg.Name, "attempt", attempt, "err", err)
			// deadConn sentinel: Wait() returns immediately → next iteration consumes another attempt with backoff.
			conn = &deadConn{err: err}
			continue
		}

		conn = newConn
		if len(newTools) > 0 {
			if regErr := s.reg.Register(s.cfg, newTools); regErr != nil {
				// I6: collision on re-register — exit loudly rather than silently losing tools.
				slog.Error("mcp/supervisor: re-register collision — server unrecoverable", "server", s.cfg.Name, "err", regErr)
				_ = conn.Close()
				s.reg.Deregister(s.cfg.Name)
				_ = s.logger.Append("mcp_server_unrecoverable", map[string]any{
					"server": s.cfg.Name, "reason": regErr.Error(),
				})
				return
			}
		}
	}

	// All retries exhausted — clean up.
	_ = conn.Close()
	s.reg.Deregister(s.cfg.Name)
	_ = s.logger.Append("mcp_server_crashed", map[string]any{
		"server":        s.cfg.Name,
		"attempt_count": maxRetries,
	})
}

// deadConn is a sentinel ServerConn whose Wait() returns immediately.
// Used when connectFn fails: allows the retry loop to consume backoff + attempt
// without calling Wait() on a nil or stale connection.
type deadConn struct{ err error }

func (d *deadConn) ListTools(context.Context) ([]mcp.Tool, error) { return nil, d.err }
func (d *deadConn) CallTool(context.Context, string, map[string]any) (*mcp.CallToolResult, error) {
	return nil, d.err
}
func (d *deadConn) Wait() error  { return d.err }
func (d *deadConn) Close() error { return nil }
