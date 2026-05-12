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

// LifecycleManager maintains a map of active per-task MCPInstances and ensures
// at most one instance per task is running at any time.
type LifecycleManager struct {
	mu        sync.RWMutex
	instances map[string]*instance.MCPInstance // keyed by TaskID
}

// New creates an empty LifecycleManager.
func New() *LifecycleManager {
	return &LifecycleManager{
		instances: make(map[string]*instance.MCPInstance),
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

	if err := inst.Start(ctx, cfg.ExtraServers); err != nil {
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

// StopAll shuts down all running instances.  Intended for daemon shutdown.
// Best-effort: errors are logged but not aggregated.
func (m *LifecycleManager) StopAll(ctx context.Context) {
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

