// Package lifecycle provides the LifecycleManager — a thread-safe registry
// of per-task MCPInstances.  It is the integration point between the tether
// REST API (task MCP endpoints) and the per-task MCP stacks defined in
// internal/mcp/instance.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/piaobeizu/tether/internal/mcp/host"
	"github.com/piaobeizu/tether/internal/mcp/instance"
	"github.com/piaobeizu/tether/internal/permission"
)

// ErrShuttingDown is returned by StartTask once StopAll has begun. The manager
// is terminal from that point on: StopAll's whole job is to leave nothing
// running, and there is no way to honour that and also start something new.
//
// It is a state of the manager, not a fault in the request, and a caller
// surfacing it to a client should say so — 503, not 500. The handler behind
// POST /api/v1/tasks/{id}/mcp maps every StartTask error to 500 today
// (handleTaskMCPStart in internal/server/task_mcp.go, which this change does not
// touch); the sentinel is exported so that mapping can be corrected there
// without reaching in here to match on a string.
var ErrShuttingDown = errors.New("lifecycle: manager is shutting down")

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
	// gates serializes the start/stop transitions of one task against each
	// other, keyed by TaskID. Refcounted so the map tracks tasks in transition
	// rather than every task the daemon has ever started.
	gates map[string]*taskGate
	// closing is set by StopAll and never cleared: the manager is terminal once
	// shutdown has begun. Guarded by mu, and read inside the same critical
	// section that publishes into instances — that pairing is what closes the
	// window StopAll's snapshot leaves open.
	closing bool

	cfg          Config
	watchdogStop chan struct{}
	watchdogDone chan struct{}
	stopOnce     sync.Once
}

// taskGate serializes the transitions of a single task. waiters is guarded by
// LifecycleManager.mu, not by the gate's own mutex.
type taskGate struct {
	mu      sync.Mutex
	waiters int
}

// lockTask takes the gate for taskID and returns the function that releases it.
//
// This has to be a separate lock from m.mu, held for as long as it takes to
// build and start an instance. m.mu also guards Get and Active, and starting an
// instance spawns a child process per configured MCP server and completes a
// handshake with each — a manager-wide lock held across that would stall every
// other task's reads behind one task's slow or broken server. Per task it costs
// nothing that matters: two starts for the same task are a double click, and
// queueing the second one behind the first is what makes it a restart instead of
// a second live instance.
func (m *LifecycleManager) lockTask(taskID string) func() {
	m.mu.Lock()
	g := m.gates[taskID]
	if g == nil {
		g = &taskGate{}
		m.gates[taskID] = g
	}
	g.waiters++
	m.mu.Unlock()

	g.mu.Lock()
	return func() {
		g.mu.Unlock()
		m.mu.Lock()
		g.waiters--
		if g.waiters == 0 {
			delete(m.gates, taskID)
		}
		m.mu.Unlock()
	}
}

// shuttingDown reports whether StopAll has begun.
func (m *LifecycleManager) shuttingDown() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.closing
}

// New creates a LifecycleManager. With no options the idle watchdog is off
// (backward-compatible); pass WithIdleWatchdog to enable it.
func New(opts ...Option) *LifecycleManager {
	m := &LifecycleManager{
		instances: make(map[string]*instance.MCPInstance),
		gates:     make(map[string]*taskGate),
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
//
// Concurrent calls for one TaskID are serialized here, and here is the only
// place they can be: the handler behind POST /api/v1/tasks/{id}/mcp adds no
// serialization of its own, and two of those requests in flight at once is a
// double click or an SPA retry. Without it each caller built and started its own
// instance and only the last to reach the map was tracked, leaving the rest
// running — loopback bound, bearer token valid, mcpServers entry written, child
// servers alive — with no reference left anywhere to stop them by, so not even
// StopAll could reach them. Serialized, the second caller finds the first
// caller's instance and takes the restart path that already existed for it.
//
// Once StopAll has begun this returns ErrShuttingDown and starts nothing. The
// bound on when it will try at all is what makes StopAll's promise hold; see
// StopAll for what the alternative was and why it was not taken.
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

	release := m.lockTask(cfg.TaskID)
	defer release()

	// Nothing built below can outlive a shutdown that has already begun, so once
	// it has, do not build it. The check after the start is the one that holds
	// the invariant; this one keeps the side effects from happening at all on the
	// ordinary path, and they reach outside this package: instance.New binds a
	// loopback port before Start is even called, Start spawns a child process per
	// configured server, and unless SkipInject is set it writes an mcpServers
	// entry into the agent settings file that only Stop takes back out. A daemon
	// that exits partway through undoing that leaves the entry pointing at a port
	// nothing is listening on.
	if m.shuttingDown() {
		return nil, ErrShuttingDown
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
	// Reading this and publishing the replacement below are separated by the
	// whole of instance construction and start; the task gate is what makes the
	// pair atomic against another start for the same task.
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

	// Publishing and the closing check share one critical section, and StopAll
	// sets closing and takes its snapshot in another, which is what leaves no
	// third ordering: either this instance is in the map before the snapshot and
	// StopAll stops it, or closing is already true here and it never goes in. The
	// map is the only reference anyone keeps, so an instance published after the
	// snapshot would be one nothing could ever stop.
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_ = inst.Stop(stopCtx)
		return nil, ErrShuttingDown
	}
	m.instances[cfg.TaskID] = inst
	m.mu.Unlock()

	return inst, nil
}

// StopTask shuts down the running MCPInstance for taskID.
// Returns an error if no instance is found or shutdown fails.
//
// Takes the same per-task gate as StartTask, so a stop that arrives while a
// start for that task is in flight waits for it and then stops what it started,
// instead of reporting "no instance" and letting the start publish one behind it.
func (m *LifecycleManager) StopTask(ctx context.Context, taskID string) error {
	release := m.lockTask(taskID)
	defer release()

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
//
// This is terminal: the manager is marked closing here and every StartTask from
// then on returns ErrShuttingDown, starting nothing. That narrows what the type
// promised callers — StartTask used to make an attempt whenever it was called —
// and it is what makes "shuts down all instances" true instead of nearly true.
// The ids below are a snapshot, so without the flag a start that published an
// instance for a task the snapshot does not contain would walk out of here still
// running: loopback bound, child servers up, instance-scoped context alive, and
// nothing left holding a reference to stop it by. The per-task gate does not
// reach that case — it serializes two transitions of one task, and this task is
// by definition not one the snapshot saw.
//
// The alternative was to keep the old promise and loop here until a pass finds
// no new instances. Rejected on two counts. It has no bound (a client that keeps
// starting tasks keeps the daemon from ever exiting) and giving it one only
// moves the leak to the far side of the bound. And it does nothing for a start
// that arrives after this function has returned, which is the same defect with
// the concurrency taken out: the daemon is on its way out either way, so an
// instance published then is exactly as unreachable.
func (m *LifecycleManager) StopAll(ctx context.Context) {
	if m.watchdogStop != nil {
		m.stopOnce.Do(func() { close(m.watchdogStop) })
		<-m.watchdogDone
	}

	m.mu.Lock()
	m.closing = true
	ids := make([]string, 0, len(m.instances))
	for id := range m.instances {
		ids = append(ids, id)
	}
	m.mu.Unlock()

	for _, id := range ids {
		_ = m.StopTask(ctx, id)
	}
}
