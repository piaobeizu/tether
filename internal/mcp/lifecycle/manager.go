// Package lifecycle provides the LifecycleManager — a thread-safe registry
// of per-task MCPInstances.  It is the integration point between the tether
// REST API (task MCP endpoints) and the per-task MCP stacks defined in
// internal/mcp/instance.
package lifecycle

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/piaobeizu/tether/internal/mcp/host"
	"github.com/piaobeizu/tether/internal/mcp/instance"
	"github.com/piaobeizu/tether/internal/permission"
)

// TaskConfig carries the parameters needed to start a per-task MCPInstance.
type TaskConfig struct {
	// TaskID is the canonical polyforge task ID.
	TaskID string
	// TaskSlug is the human-readable slug (used as the settings key suffix).
	TaskSlug string
	// WsRoot is the absolute path to the task's worktree / workspace root.
	WsRoot string
	// ExtraServers are optional stdio MCP server processes to start for this task.
	ExtraServers map[string]host.ServerConfig
	// PermManager is shared across all instances; must not be nil.
	PermManager *permission.Manager
	// Logger is the history logger.  Nil = noop.
	Logger host.HistoryLogger
	// SkipInject suppresses ~/.claude/settings.json mutation (tests/CI).
	SkipInject bool
}

// Config tunes the LifecycleManager. The zero value disables the idle watchdog.
type Config struct {
	// IdleThreshold is how long an instance may go without a tool call before
	// the watchdog hibernates it. <= 0 disables the watchdog entirely.
	IdleThreshold time.Duration
	// TickInterval is the watchdog scan period. If <= 0 it defaults to
	// IdleThreshold, capped at one minute.
	TickInterval time.Duration
}

// Option configures a LifecycleManager at construction.
type Option func(*Config)

// WithIdleWatchdog enables the idle-instance watchdog: instances idle for
// longer than threshold are hibernated (child servers stopped, revived on the
// next tool call). threshold <= 0 leaves the watchdog disabled; tick <= 0
// selects a sensible default (min(threshold, 1m)).
func WithIdleWatchdog(threshold, tick time.Duration) Option {
	return func(c *Config) {
		c.IdleThreshold = threshold
		c.TickInterval = tick
	}
}

// LifecycleManager maintains a map of active per-task MCPInstances and ensures
// at most one instance per task is running at any time. When configured with an
// idle watchdog it also hibernates instances that have gone quiet.
type LifecycleManager struct {
	mu        sync.RWMutex
	instances map[string]*instance.MCPInstance // keyed by TaskID

	cfg          Config
	watchdogStop chan struct{}
	watchdogDone chan struct{}
	stopOnce     sync.Once
}

// New creates a LifecycleManager. With no options the idle watchdog is off
// (backward-compatible); pass WithIdleWatchdog to enable it.
func New(opts ...Option) *LifecycleManager {
	m := &LifecycleManager{
		instances: make(map[string]*instance.MCPInstance),
	}
	for _, o := range opts {
		o(&m.cfg)
	}
	if m.cfg.IdleThreshold > 0 {
		if m.cfg.TickInterval <= 0 {
			m.cfg.TickInterval = m.cfg.IdleThreshold
			if m.cfg.TickInterval > time.Minute {
				m.cfg.TickInterval = time.Minute
			}
		}
		m.watchdogStop = make(chan struct{})
		m.watchdogDone = make(chan struct{})
		go m.runWatchdog()
	}
	return m
}

// runWatchdog periodically hibernates instances idle longer than the configured
// threshold. It exits when watchdogStop is closed.
func (m *LifecycleManager) runWatchdog() {
	defer close(m.watchdogDone)
	ticker := time.NewTicker(m.cfg.TickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.watchdogStop:
			return
		case <-ticker.C:
			m.sweepIdle(time.Now())
		}
	}
}

// sweepIdle hibernates every active instance idle past the threshold. Hibernate
// is a no-op for instances already dormant or without external servers.
func (m *LifecycleManager) sweepIdle(now time.Time) {
	for _, inst := range m.Active() {
		if inst.IdleFor(now) > m.cfg.IdleThreshold {
			_ = inst.Hibernate(context.Background())
		}
	}
}

// StartTask creates and starts an MCPInstance for the given task.
// If an instance for TaskID already exists and is running, it is stopped first
// before the new one is started (handles resume-with-new-config).
// Returns the started instance so callers can read Port and Token.
func (m *LifecycleManager) StartTask(ctx context.Context, cfg TaskConfig) (*instance.MCPInstance, error) {
	if cfg.TaskID == "" {
		return nil, fmt.Errorf("lifecycle: TaskID is required")
	}
	if cfg.TaskSlug == "" {
		return nil, fmt.Errorf("lifecycle: TaskSlug is required")
	}
	if cfg.PermManager == nil {
		return nil, fmt.Errorf("lifecycle: PermManager is required")
	}

	// Load per-task MCP servers from <WsRoot>/.tether/task-config.json.
	// A missing file yields no servers; a malformed file aborts start.
	fileServers, err := LoadTaskConfig(cfg.WsRoot)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: load task-config for %s: %w", cfg.TaskSlug, err)
	}
	// Merge: file servers first, then request ExtraServers overlaid so request
	// keys win on collision.
	var mergedServers map[string]host.ServerConfig
	if len(fileServers) > 0 || len(cfg.ExtraServers) > 0 {
		mergedServers = make(map[string]host.ServerConfig, len(fileServers)+len(cfg.ExtraServers))
		for k, v := range fileServers {
			mergedServers[k] = v
		}
		for k, v := range cfg.ExtraServers {
			mergedServers[k] = v
		}
	}

	// Stop any existing instance for this task first (idempotent restart).
	m.mu.Lock()
	existing, exists := m.instances[cfg.TaskID]
	if exists {
		delete(m.instances, cfg.TaskID)
	}
	m.mu.Unlock()

	if exists {
		stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_ = existing.Stop(stopCtx) // best-effort; proceed regardless
	}

	inst, err := instance.New(instance.InstanceConfig{
		TaskID:      cfg.TaskID,
		TaskSlug:    cfg.TaskSlug,
		WsRoot:      cfg.WsRoot,
		PermManager: cfg.PermManager,
		Logger:      cfg.Logger,
		SkipInject:  cfg.SkipInject,
	})
	if err != nil {
		return nil, fmt.Errorf("lifecycle: new instance for %s: %w", cfg.TaskSlug, err)
	}

	if err := inst.Start(ctx, mergedServers); err != nil {
		return nil, fmt.Errorf("lifecycle: start instance for %s: %w", cfg.TaskSlug, err)
	}

	m.mu.Lock()
	m.instances[cfg.TaskID] = inst
	m.mu.Unlock()

	return inst, nil
}

// StopTask shuts down the running MCPInstance for taskID.
// Returns an error if no instance is found or shutdown fails.
func (m *LifecycleManager) StopTask(ctx context.Context, taskID string) error {
	m.mu.Lock()
	inst, ok := m.instances[taskID]
	if ok {
		delete(m.instances, taskID)
	}
	m.mu.Unlock()

	if !ok {
		return fmt.Errorf("lifecycle: no instance for task %s", taskID)
	}
	return inst.Stop(ctx)
}

// Get returns the active instance for taskID, or (nil, false) if absent.
func (m *LifecycleManager) Get(taskID string) (*instance.MCPInstance, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	inst, ok := m.instances[taskID]
	return inst, ok
}

// Active returns a snapshot of all currently running instances.
func (m *LifecycleManager) Active() []*instance.MCPInstance {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*instance.MCPInstance, 0, len(m.instances))
	for _, inst := range m.instances {
		out = append(out, inst)
	}
	return out
}

// StopAll stops the idle watchdog (if running) and shuts down all instances.
// Intended for daemon shutdown. Best-effort: per-instance errors are ignored.
func (m *LifecycleManager) StopAll(ctx context.Context) {
	if m.watchdogStop != nil {
		m.stopOnce.Do(func() { close(m.watchdogStop) })
		<-m.watchdogDone
	}

	m.mu.Lock()
	ids := make([]string, 0, len(m.instances))
	for id := range m.instances {
		ids = append(ids, id)
	}
	m.mu.Unlock()

	for _, id := range ids {
		_ = m.StopTask(ctx, id)
	}
}
