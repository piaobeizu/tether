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
	reg     ToolRegistry
	logger  HistoryLogger
}

// NewManager creates a Manager. reg and logger must not be nil.
func NewManager(reg ToolRegistry, logger HistoryLogger) *Manager {
	return &Manager{
		conns:   make(map[string]ServerConn),
		cancels: make(map[string]context.CancelFunc),
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
	m.mu.Lock()
	m.cancels[cfg.Name] = cancel
	m.mu.Unlock()

	client := mcp.NewClient(&mcp.Implementation{Name: "tether", Version: "v0.3"}, nil)

	connectFn := func() (ServerConn, []mcp.Tool, error) {
		if len(cfg.Command) == 0 {
			return nil, nil, fmt.Errorf("command is empty")
		}
		cmd := exec.CommandContext(supCtx, cfg.Command[0], cfg.Command[1:]...)
		extra := make([]string, 0, len(cfg.Env))
		for k, v := range cfg.Env {
			extra = append(extra, k+"="+v)
		}
		cmd.Env = append(os.Environ(), extra...)
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
		m.mu.Lock()
		m.conns[cfg.Name] = c
		m.mu.Unlock()
		return c, tools, nil
	}

	sup := NewSupervisor(cfg, m.reg, m.logger, connectFn)
	go sup.Run(supCtx)
	return nil
}

// Stop shuts down a named server and deregisters its tools.
func (m *Manager) Stop(name string) error {
	m.mu.Lock()
	cancel, hasCancel := m.cancels[name]
	conn, hasConn := m.conns[name]
	delete(m.cancels, name)
	delete(m.conns, name)
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

// GetConn returns the live connection for a server name.
func (m *Manager) GetConn(name string) (ServerConn, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.conns[name]
	return c, ok
}
