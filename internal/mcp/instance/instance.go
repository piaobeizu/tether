// Package instance provides per-task MCP lifecycle management.
//
// Each active polyforge task gets its own MCPInstance: isolated child
// processes, a dedicated loopback port, a fresh bearer token, and builtin
// tools scoped to the task's worktree path.  The global singleton MCP stack
// (package server) is unaffected and continues to serve non-task sessions.
package instance

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/mcp/builtin"
	"github.com/piaobeizu/tether/internal/mcp/gateway"
	mcphost "github.com/piaobeizu/tether/internal/mcp/host"
	mcpreg "github.com/piaobeizu/tether/internal/mcp/registry"
	"github.com/piaobeizu/tether/internal/permission"
)

// InstanceConfig carries everything needed to spin up a per-task MCP stack.
type InstanceConfig struct {
	// TaskID is the canonical polyforge task ID (e.g. "t-01KRD1GYDZK5T1TW32RFAREEQV").
	TaskID string
	// TaskSlug is the human-readable slug used in settings keys
	// (e.g. "mcp-lifecycle" → mcpServers entry key "tether-mcp-lifecycle").
	TaskSlug string
	// WsRoot is the absolute path to the task's worktree / workspace root.
	// Builtin tools (workspace_read_file etc.) are scoped to this directory.
	WsRoot string
	// Port is the loopback port to bind.  0 = OS-assigned free port (recommended).
	Port int
	// PermManager is the shared permission gate.  Required.
	PermManager *permission.Manager
	// Logger is the history logger for the MCP host.  Nil = noop.
	Logger mcphost.HistoryLogger
	// SkipInject suppresses ~/.claude/settings.json mutation.
	// Set true in CI and unit tests.
	SkipInject bool
}

// MCPInstance is a self-contained MCP stack scoped to one polyforge task.
// Create with New(); call Start() to bind and serve; call Stop() to tear down.
//
// All exported fields are read-only after Start() returns successfully.
type MCPInstance struct {
	// TaskID is the polyforge task identifier.
	TaskID string
	// TaskSlug is the human-readable slug.
	TaskSlug string
	// WsRoot is the resolved workspace root used by builtin tools.
	WsRoot string
	// Port is the loopback port this instance is listening on.
	// Populated after Start() returns.
	Port int
	// Token is the bearer token required to authenticate against the loopback.
	// Populated after New() returns.
	Token string

	// internal wiring
	mgr      *mcphost.Manager
	reg      *mcpreg.Registry
	srv      *mcp.Server
	loopback *loopback

	skipInject bool
	started    bool
	// lastActive is updated on Start() and on each authorized loopback request.
	lastActive time.Time
	// extraServers records the stdio servers passed to Start().
	extraServers map[string]mcphost.ServerConfig
	// baseCtx scopes the child MCP server processes to the instance lifetime
	// (cancelled in Stop) rather than to the request context that triggered a
	// start/wake — so children survive the HTTP request that spawned them.
	baseCtx    context.Context
	baseCancel context.CancelFunc
	mu         sync.Mutex

	// dormant is true while the instance is hibernated: external child MCP
	// servers are stopped to reclaim resources, but the loopback and builtin
	// tools keep serving. The next external tool call triggers Wake.
	dormant atomic.Bool
	// suppressSrv, while true, makes the registry observer skip *mcp.Server
	// tool add/remove so the advertised tool set stays stable across a
	// hibernate/wake transition. Read via atomic, never under i.mu: the
	// observer fires from mgr.Start/StopAll while Wake/Hibernate hold i.mu.
	suppressSrv atomic.Bool
}

// New constructs (but does not start) an MCPInstance from cfg.
// Returns an error if the configuration is invalid (e.g. unresolvable WsRoot,
// empty TaskID/TaskSlug, nil PermManager).
func New(cfg InstanceConfig) (*MCPInstance, error) {
	if cfg.TaskID == "" {
		return nil, fmt.Errorf("mcp/instance: TaskID is required")
	}
	if cfg.TaskSlug == "" {
		return nil, fmt.Errorf("mcp/instance: TaskSlug is required")
	}
	if cfg.PermManager == nil {
		return nil, fmt.Errorf("mcp/instance: PermManager is required")
	}

	logger := cfg.Logger
	if logger == nil {
		logger = mcphost.NoopLogger()
	}

	token, err := generateToken()
	if err != nil {
		return nil, fmt.Errorf("mcp/instance: token generation: %w", err)
	}

	// Build the MCP sub-stack: registry → manager → builtin registry → gateway → server.
	reg := mcpreg.New()
	mgr := mcphost.NewManager(reg, logger)

	bi, err := builtin.New(cfg.WsRoot)
	if err != nil {
		return nil, fmt.Errorf("mcp/instance: builtin registry: %w", err)
	}

	gw := gateway.New(mgr, reg, cfg.PermManager, logger)

	// Construct the instance first (minus srv/loopback) so buildServer's
	// observer and per-tool handlers can capture it for hibernate/wake wiring.
	baseCtx, baseCancel := context.WithCancel(context.Background())
	inst := &MCPInstance{
		TaskID:     cfg.TaskID,
		TaskSlug:   cfg.TaskSlug,
		WsRoot:     cfg.WsRoot,
		Token:      token,
		mgr:        mgr,
		reg:        reg,
		skipInject: cfg.SkipInject,
		baseCtx:    baseCtx,
		baseCancel: baseCancel,
	}

	srv := buildServer(gw, bi, reg, cfg.TaskSlug, inst)
	inst.srv = srv

	lb, err := newLoopback(cfg.Port, srv, token)
	if err != nil {
		baseCancel()
		return nil, fmt.Errorf("mcp/instance: loopback init: %w", err)
	}
	inst.Port = lb.port
	inst.loopback = lb
	// Wire loopback activity back to the instance so authorized requests
	// keep the idle clock fresh.
	lb.onActivity = inst.Touch
	return inst, nil
}

// Start binds the loopback listener, starts any configured extra servers,
// and injects the mcpServers entry into ~/.claude/settings.json.
// It is idempotent: calling Start() on an already-started instance is a no-op.
func (i *MCPInstance) Start(ctx context.Context, extraServers map[string]mcphost.ServerConfig) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.started {
		return nil
	}

	i.extraServers = extraServers
	i.lastActive = time.Now()

	if len(extraServers) > 0 {
		cfg := &mcphost.Config{Servers: extraServers}
		// Use the instance-scoped context so child servers outlive the request
		// that triggered Start; the passed ctx (often r.Context()) would kill
		// them when the HTTP handler returns.
		if err := i.mgr.Start(i.baseCtx, cfg); err != nil {
			return fmt.Errorf("mcp/instance %s: start extra servers: %w", i.TaskSlug, err)
		}
	}

	if err := i.loopback.Start(); err != nil {
		i.mgr.StopAll()
		return fmt.Errorf("mcp/instance %s: loopback listen: %w", i.TaskSlug, err)
	}

	if !i.skipInject {
		name := "tether-" + i.TaskSlug
		if err := agent.InjectMCPServer(i.Port, i.Token, name); err != nil {
			// Non-fatal: log and continue.  The caller can fall back or retry.
			slog.Warn("mcp/instance: settings inject failed",
				"task_slug", i.TaskSlug,
				"err", err)
		}
	}

	i.started = true
	slog.Info("mcp/instance: started",
		"task_id", i.TaskID,
		"task_slug", i.TaskSlug,
		"port", i.Port)
	return nil
}

// Stop drains active sessions (within the context deadline), kills child
// processes, and removes the settings.json entry.
// Calling Stop() on an instance that was never started is a no-op.
func (i *MCPInstance) Stop(ctx context.Context) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.started {
		return nil
	}

	var errs []error

	if err := i.loopback.Stop(ctx); err != nil {
		errs = append(errs, fmt.Errorf("loopback shutdown: %w", err))
	}
	i.mgr.StopAll()
	if i.baseCancel != nil {
		i.baseCancel()
	}

	if !i.skipInject {
		name := "tether-" + i.TaskSlug
		if err := agent.RemoveMCPServer(name); err != nil {
			errs = append(errs, fmt.Errorf("settings cleanup: %w", err))
		}
	}

	i.dormant.Store(false)
	i.suppressSrv.Store(false)
	i.started = false
	slog.Info("mcp/instance: stopped",
		"task_id", i.TaskID,
		"task_slug", i.TaskSlug)

	if len(errs) > 0 {
		return fmt.Errorf("mcp/instance %s stop: %v", i.TaskSlug, errs)
	}
	return nil
}

// Hibernate stops the instance's external child MCP servers to reclaim
// resources while keeping the loopback listener and in-process builtin tools
// serving. The advertised tool set is preserved (clients keep seeing the now
// cold external tools); the next external tool call triggers Wake. It is a
// no-op if the instance is not started, is already dormant, or has no external
// servers to reclaim.
func (i *MCPInstance) Hibernate(_ context.Context) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.started || i.dormant.Load() || len(i.extraServers) == 0 {
		return nil
	}
	// Observer keeps srv tool defs through the teardown (Deregister → Removed).
	i.suppressSrv.Store(true)
	// Set dormant BEFORE teardown so a tool call arriving mid-StopAll takes the
	// lazy-wake path (serialized on i.mu) instead of routing to a child that is
	// being deregistered.
	i.dormant.Store(true)
	i.mgr.StopAll()
	slog.Info("mcp/instance: hibernated", "task_id", i.TaskID, "task_slug", i.TaskSlug)
	return nil
}

// Wake re-spawns the external child MCP servers of a hibernated instance and
// restores the registry routing table. It is safe under concurrent calls
// (later callers observe a no-op once awake) and a no-op if not dormant.
func (i *MCPInstance) Wake(_ context.Context) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.dormant.Load() {
		return nil
	}
	// suppressSrv is already true (set at Hibernate); keep it set through
	// mgr.Start so the observer's Added events don't double-register tools the
	// server still advertises. Clear it only once the servers are back.
	if len(i.extraServers) > 0 {
		cfg := &mcphost.Config{Servers: i.extraServers}
		if err := i.mgr.Start(i.baseCtx, cfg); err != nil {
			// Roll back any partially-started children so they don't leak. The
			// instance stays dormant (suppressSrv still true → tools stay cold)
			// and the next external tool call retries Wake cleanly.
			i.mgr.StopAll()
			return fmt.Errorf("mcp/instance %s: wake: %w", i.TaskSlug, err)
		}
	}
	i.suppressSrv.Store(false)
	i.dormant.Store(false)
	slog.Info("mcp/instance: woke", "task_id", i.TaskID, "task_slug", i.TaskSlug)
	return nil
}

// Dormant reports whether the instance is currently hibernated.
func (i *MCPInstance) Dormant() bool { return i.dormant.Load() }

// Touch records that the instance is active as of now.
func (i *MCPInstance) Touch() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.lastActive = time.Now()
}

// IdleFor returns how long the instance has been idle relative to now
// (now.Sub(lastActive)).
func (i *MCPInstance) IdleFor(now time.Time) time.Duration {
	i.mu.Lock()
	defer i.mu.Unlock()
	return now.Sub(i.lastActive)
}

// LastActive returns the timestamp of the most recent activity.
func (i *MCPInstance) LastActive() time.Time {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.lastActive
}

// ExtraServers returns a snapshot of the stdio MCP servers that were passed to
// Start(). The returned map is a shallow copy safe for the caller to read.
func (i *MCPInstance) ExtraServers() map[string]mcphost.ServerConfig {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.extraServers == nil {
		return nil
	}
	out := make(map[string]mcphost.ServerConfig, len(i.extraServers))
	for k, v := range i.extraServers {
		out[k] = v
	}
	return out
}

// Server returns the underlying *mcp.Server so callers can mount it on
// additional HTTP handlers (e.g. the HTTPS /mcp route).
// Valid after New(); do not call AddTool/RemoveTool directly.
func (i *MCPInstance) Server() *mcp.Server { return i.srv }

// buildServer constructs a *mcp.Server wired to the gateway and builtins.
// This is an instance-scoped variant of server.BuildMCPServer; it lives here
// to avoid an import cycle (server ← instance ← server).
func buildServer(gw *gateway.Gateway, bi *builtin.Registry, reg *mcpreg.Registry, taskSlug string, inst *MCPInstance) *mcp.Server {
	srv := mcp.NewServer(
		&mcp.Implementation{Name: "tether-" + taskSlug, Version: "v0.4.0"},
		nil,
	)

	addOne := func(entry mcpreg.ToolEntry) {
		t := entry.Tool
		t.Name = entry.PrefixedName
		srv.AddTool(&t, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			// Lazy wake: an external tool call on a hibernated instance
			// re-spawns its child servers (repopulating the registry) before
			// the call is routed. Builtin tools are registered directly on srv
			// (not through addOne), so they never wake a dormant instance.
			if inst.dormant.Load() {
				if err := inst.Wake(ctx); err != nil {
					return nil, fmt.Errorf("mcp/instance: wake before tool %q: %w", entry.PrefixedName, err)
				}
			}
			return gw.CallTool(ctx, gateway.CallRequest{
				ToolName:  entry.PrefixedName,
				Arguments: req.Params.Arguments,
			})
		})
	}

	for _, e := range gw.ListToolsSnapshot() {
		addOne(e)
	}
	bi.RegisterInto(srv)

	reg.AddObserver(func(e mcpreg.RegistryEvent) {
		// During a hibernate/wake transition keep the advertised tool set
		// stable: skip srv mutations while only the registry routing table and
		// child processes are toggling. (Registry entries still change so
		// gateway routing stays correct once awake.)
		if inst.suppressSrv.Load() {
			return
		}
		if len(e.Removed) > 0 {
			names := make([]string, len(e.Removed))
			for idx, t := range e.Removed {
				names[idx] = t.PrefixedName
			}
			srv.RemoveTools(names...)
		}
		for _, entry := range e.Added {
			addOne(entry)
		}
	})

	return srv
}

// loopback is a minimal bearer-auth HTTP wrapper around an *mcp.Server,
// equivalent to server.MCPLoopback but scoped to one instance.
type loopback struct {
	srv     *http.Server
	ln      net.Listener
	port    int
	token   string
	handler http.Handler
	// onActivity, if non-nil, is invoked after a request passes the bearer
	// check and before it is forwarded to the handler.
	onActivity func()
}

func newLoopback(port int, mcpSrv *mcp.Server, token string) (*loopback, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, err
	}
	actualPort := ln.Addr().(*net.TCPAddr).Port

	sdkHandler := mcp.NewStreamableHTTPHandler(
		func(_ *http.Request) *mcp.Server { return mcpSrv },
		&mcp.StreamableHTTPOptions{SessionTimeout: 30 * time.Minute},
	)
	l := &loopback{token: token, handler: sdkHandler, ln: ln, port: actualPort}
	mux := http.NewServeMux()
	mux.Handle("/mcp", l)
	mux.Handle("/mcp/", l)
	l.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 30 * time.Second,
	}
	return l, nil
}

func (l *loopback) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	got := r.Header.Get("Authorization")
	want := "Bearer " + l.token
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if l.onActivity != nil {
		l.onActivity()
	}
	l.handler.ServeHTTP(w, r)
}

func (l *loopback) Start() error {
	go func() {
		if err := l.srv.Serve(l.ln); err != nil && err != http.ErrServerClosed {
			slog.Error("mcp/instance: loopback serve error", "err", err)
		}
	}()
	return nil
}

func (l *loopback) Stop(ctx context.Context) error {
	return l.srv.Shutdown(ctx)
}

// generateToken returns a cryptographically random 32-byte hex string.
func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
