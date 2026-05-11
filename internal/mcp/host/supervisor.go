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
	cfg       ServerConfig
	reg       ToolRegistry
	logger    HistoryLogger
	connectFn ConnectFn
}

// NewSupervisor creates a Supervisor. Call Run(ctx) to start it.
func NewSupervisor(cfg ServerConfig, reg ToolRegistry, logger HistoryLogger, connectFn ConnectFn) *Supervisor {
	return &Supervisor{cfg: cfg, reg: reg, logger: logger, connectFn: connectFn}
}

// Run blocks until the server is permanently stopped (ctx cancelled or retries exhausted).
// It calls connectFn once initially, then up to maxRetries times on crash.
func (s *Supervisor) Run(ctx context.Context) {
	conn, tools, err := s.connectFn()
	if err != nil {
		slog.Error("mcp/supervisor: initial connect failed", "server", s.cfg.Name, "err", err)
		_ = s.logger.Append("mcp_server_crashed", map[string]any{
			"server": s.cfg.Name, "attempt": 0, "error": err.Error(),
		})
		return
	}
	if len(tools) > 0 {
		_ = s.reg.Register(s.cfg, tools)
	}

	for attempt := 1; attempt <= maxRetries; attempt++ {
		// Block until session closes (crash or clean shutdown).
		waitErr := conn.Wait()

		// Check if ctx was cancelled (clean shutdown).
		select {
		case <-ctx.Done():
			_ = conn.Close()
			s.reg.Deregister(s.cfg.Name)
			return
		default:
		}

		// Crash detected.
		slog.Warn("mcp/supervisor: server crashed", "server", s.cfg.Name, "attempt", attempt, "err", waitErr)
		s.reg.Deregister(s.cfg.Name)
		_ = conn.Close()

		// Backoff before retry.
		select {
		case <-ctx.Done():
			return
		case <-time.After(RetryDelays[attempt-1]):
		}

		// Attempt reconnect.
		var newConn ServerConn
		var newTools []mcp.Tool
		newConn, newTools, err = s.connectFn()
		if err != nil {
			slog.Error("mcp/supervisor: reconnect failed", "server", s.cfg.Name, "attempt", attempt, "err", err)
			continue
		}
		conn = newConn
		if len(newTools) > 0 {
			if regErr := s.reg.Register(s.cfg, newTools); regErr != nil {
				slog.Warn("mcp/supervisor: re-register failed", "err", regErr)
			}
		}
	}

	// All retries exhausted — clean up and log permanent crash.
	_ = conn.Close()
	s.reg.Deregister(s.cfg.Name)
	_ = s.logger.Append("mcp_server_crashed", map[string]any{
		"server":        s.cfg.Name,
		"attempt_count": maxRetries,
	})
}
