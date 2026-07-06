// internal/mcp/host/manager.go
package host

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Manager spawns and supervises MCP server connections.
type Manager struct {
	mu      sync.RWMutex
	conns   map[string]ServerConn
	cancels map[string]context.CancelFunc
	gen     map[string]uint64 // per-name generation counter; guards stale deregister/delete
	reg     ToolRegistry
	logger  HistoryLogger
}

// NewManager creates a Manager. reg and logger must not be nil.
func NewManager(reg ToolRegistry, logger HistoryLogger) *Manager {
	return &Manager{
		conns:   make(map[string]ServerConn),
		cancels: make(map[string]context.CancelFunc),
		gen:     make(map[string]uint64),
		reg:     reg,
		logger:  logger,
	}
}

// Start spawns all servers in cfg. Returns on first spawn error.
func (m *Manager) Start(ctx context.Context, cfg *Config) error {
	for name, serverCfg := range cfg.Servers {
		serverCfg.Name = name
		if err := m.startOne(ctx, serverCfg); err != nil {
			return fmt.Errorf("mcp/manager: start %s: %w", name, err)
		}
	}
	return nil
}

func (m *Manager) startOne(ctx context.Context, cfg ServerConfig) error {
	supCtx, cancel := context.WithCancel(ctx)

	client := mcp.NewClient(&mcp.Implementation{Name: "tether", Version: "v0.3"}, nil)

	spawnConn := func() (ServerConn, []mcp.Tool, error) {
		if len(cfg.Command) == 0 {
			return nil, nil, fmt.Errorf("command is empty")
		}
		cmd := exec.CommandContext(supCtx, cfg.Command[0], cfg.Command[1:]...)
		// I4: curated env — inherit only safe keys; use cfg.InheritEnv for additional opt-ins.
		safeKeys := []string{"PATH", "HOME", "USER", "TMPDIR", "TZ", "LANG", "LC_ALL"}
		for _, k := range append(safeKeys, cfg.InheritEnv...) {
			if v := os.Getenv(k); v != "" {
				cmd.Env = append(cmd.Env, k+"="+v)
			}
		}
		for k, v := range cfg.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		transport := &mcp.CommandTransport{Command: cmd}
		session, err := client.Connect(supCtx, transport, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("connect: %w", err)
		}
		c := NewLiveConn(session)
		tools, err := c.ListTools(supCtx)
		if err != nil {
			_ = c.Close()
			return nil, nil, fmt.Errorf("list tools: %w", err)
		}
		return c, tools, nil
	}

	// C1: synchronous initial connect — caller gets the error.
	conn, tools, err := spawnConn()
	if err != nil {
		cancel()
		return fmt.Errorf("mcp/manager: start %s: %w", cfg.Name, err)
	}
	if len(tools) > 0 {
		if regErr := m.reg.Register(cfg, tools); regErr != nil {
			_ = conn.Close()
			cancel()
			return fmt.Errorf("mcp/manager: register %s: %w", cfg.Name, regErr)
		}
	}

	m.mu.Lock()
	// Bump the generation BEFORE storing conn/cancel so this supervisor's
	// deregister/delete become no-ops once a newer Start supersedes it.
	m.gen[cfg.Name]++
	g := m.gen[cfg.Name]
	m.conns[cfg.Name] = conn
	m.cancels[cfg.Name] = cancel
	m.mu.Unlock()

	// deregister only removes tools if this generation is still current;
	// a stale supervisor's cleanup must not wipe a newer generation's tools.
	deregister := func() {
		m.mu.Lock()
		current := m.gen[cfg.Name] == g
		m.mu.Unlock()
		if current {
			m.reg.Deregister(cfg.Name)
		}
	}

	// connectFn for supervisor retries: creates a new connection and updates m.conns.
	connectFn := func() (ServerConn, []mcp.Tool, error) {
		c, t, err := spawnConn()
		if err != nil {
			return nil, nil, err
		}
		m.mu.Lock()
		if m.gen[cfg.Name] == g {
			m.conns[cfg.Name] = c
		}
		m.mu.Unlock()
		return c, t, nil
	}

	sup := NewSupervisor(cfg, m.reg, m.logger, conn, connectFn, deregister)
	go func() {
		sup.Run(supCtx)
		// C2: clear stale conn when supervisor exits (retries exhausted or ctx cancelled).
		// Only touch the maps if this generation is still current, so a fast
		// Stop→Start for the same name is not clobbered by the old supervisor.
		m.mu.Lock()
		if m.gen[cfg.Name] == g {
			delete(m.conns, cfg.Name)
			delete(m.cancels, cfg.Name)
		}
		m.mu.Unlock()
		deregister()
	}()

	return nil
}

// Stop shuts down a named server and deregisters its tools.
func (m *Manager) Stop(name string) error {
	m.mu.Lock()
	cancel, hasCancel := m.cancels[name]
	conn, hasConn := m.conns[name]
	delete(m.cancels, name)
	delete(m.conns, name)
	// Invalidate the stopped generation so the exiting supervisor's async
	// deregister/delete cannot wipe a subsequent Start's tools/conn.
	m.gen[name]++
	m.mu.Unlock()

	if hasCancel {
		cancel()
	}
	m.reg.Deregister(name)
	if hasConn {
		return conn.Close()
	}
	return nil
}

// StopAll shuts down all running servers.
// Assumes no concurrent Start calls are in flight (safe for shutdown-only use).
func (m *Manager) StopAll() {
	m.mu.Lock()
	names := make([]string, 0, len(m.conns))
	for name := range m.conns {
		names = append(names, name)
	}
	m.mu.Unlock()
	for _, name := range names {
		_ = m.Stop(name)
	}
}

// GetConn returns the live connection for a server name.
func (m *Manager) GetConn(name string) (ServerConn, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.conns[name]
	return c, ok
}
