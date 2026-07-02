package lifecycle_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/piaobeizu/tether/internal/mcp/host"
	"github.com/piaobeizu/tether/internal/mcp/lifecycle"
	"github.com/piaobeizu/tether/internal/permission"
)

// stdioServerEnv, when set to "1", makes TestStdioMCPServerHelper act as a real
// stdio MCP server (used as a spawnable child process by the merge tests).
const stdioServerEnv = "TETHER_TEST_MCP_STDIO"

// TestStdioMCPServerHelper is not a real test: when invoked via re-exec with
// stdioServerEnv=1 it serves a minimal MCP server over stdin/stdout so that the
// LifecycleManager's synchronous connect succeeds without a fragile fake binary.
func TestStdioMCPServerHelper(t *testing.T) {
	if os.Getenv(stdioServerEnv) != "1" {
		t.Skip("helper process only")
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "test-stdio", Version: "v0.0.1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "noop", Description: "no-op"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
			return &mcp.CallToolResult{}, struct{}{}, nil
		})
	_ = srv.Run(context.Background(), &mcp.StdioTransport{})
}

// stdioServerCommand returns argv that re-execs the test binary as a stdio MCP
// server via TestStdioMCPServerHelper.
func stdioServerCommand() []string {
	return []string{os.Args[0], "-test.run=^TestStdioMCPServerHelper$"}
}

// stdioServerEnvMap returns the env needed to activate the helper server.
func stdioServerEnvMap() map[string]string {
	return map[string]string{stdioServerEnv: "1"}
}

func newPermManager() *permission.Manager {
	return permission.New()
}

func baseConfig(t *testing.T, pm *permission.Manager, taskID, slug string) lifecycle.TaskConfig {
	t.Helper()
	return lifecycle.TaskConfig{
		TaskID:      taskID,
		TaskSlug:    slug,
		WsRoot:      t.TempDir(),
		PermManager: pm,
		SkipInject:  true,
	}
}

func TestStartStopTask(t *testing.T) {
	pm := newPermManager()
	lm := lifecycle.New()
	ctx := context.Background()

	cfg := baseConfig(t, pm, "t-01LMTEST1", "lm-test-1")
	inst, err := lm.StartTask(ctx, cfg)
	if err != nil {
		t.Fatalf("StartTask() error: %v", err)
	}

	if inst.Port <= 0 {
		t.Fatalf("instance Port should be > 0, got %d", inst.Port)
	}
	if inst.Token == "" {
		t.Fatal("instance Token should be non-empty")
	}

	got, ok := lm.Get(cfg.TaskID)
	if !ok {
		t.Fatal("Get() should return true after StartTask")
	}
	if got != inst {
		t.Fatal("Get() returned different instance than StartTask")
	}

	stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := lm.StopTask(stopCtx, cfg.TaskID); err != nil {
		t.Fatalf("StopTask() error: %v", err)
	}

	_, ok = lm.Get(cfg.TaskID)
	if ok {
		t.Fatal("Get() should return false after StopTask")
	}
}

// writeManagerTaskConfig writes task-config.json under <dir>/.tether.
func writeManagerTaskConfig(t *testing.T, dir, body string) {
	t.Helper()
	td := filepath.Join(dir, ".tether")
	if err := os.MkdirAll(td, 0o755); err != nil {
		t.Fatalf("mkdir .tether: %v", err)
	}
	if err := os.WriteFile(filepath.Join(td, "task-config.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write task-config.json: %v", err)
	}
}

func TestStartTask_FileServersReachInstance(t *testing.T) {
	pm := newPermManager()
	lm := lifecycle.New()
	ctx := context.Background()

	ws := t.TempDir()
	cmdJSON, err := json.Marshal(stdioServerCommand())
	if err != nil {
		t.Fatalf("marshal command: %v", err)
	}
	envJSON, err := json.Marshal(stdioServerEnvMap())
	if err != nil {
		t.Fatalf("marshal env: %v", err)
	}
	writeManagerTaskConfig(t, ws, `{
		"version": 1,
		"servers": {
			"file-srv": {"command": `+string(cmdJSON)+`, "env": `+string(envJSON)+`, "prefix": "fs"}
		}
	}`)

	cfg := lifecycle.TaskConfig{
		TaskID:      "t-01FILESRV",
		TaskSlug:    "file-srv-test",
		WsRoot:      ws,
		PermManager: pm,
		SkipInject:  true,
	}
	inst, err := lm.StartTask(ctx, cfg)
	if err != nil {
		t.Fatalf("StartTask() error: %v", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_ = lm.StopTask(stopCtx, cfg.TaskID)
	}()

	es := inst.ExtraServers()
	sc, ok := es["file-srv"]
	if !ok {
		t.Fatalf("file server not passed to instance: %v", es)
	}
	if sc.Prefix != "fs" {
		t.Fatalf("prefix not threaded: %q", sc.Prefix)
	}
}

func TestStartTask_RequestOverridesFile(t *testing.T) {
	pm := newPermManager()
	lm := lifecycle.New()
	ctx := context.Background()

	// Use the re-exec stdio MCP helper so StartTask's synchronous connect
	// succeeds; the point of this test is the merge, not child-process behavior.
	ws := t.TempDir()
	cmdJSON, err := json.Marshal(stdioServerCommand())
	if err != nil {
		t.Fatalf("marshal command: %v", err)
	}
	envJSON, err := json.Marshal(stdioServerEnvMap())
	if err != nil {
		t.Fatalf("marshal env: %v", err)
	}
	writeManagerTaskConfig(t, ws, `{
		"version": 1,
		"servers": {
			"shared": {"command": `+string(cmdJSON)+`, "env": `+string(envJSON)+`, "prefix": "fromfile"},
			"only-file": {"command": `+string(cmdJSON)+`, "env": `+string(envJSON)+`}
		}
	}`)

	cfg := lifecycle.TaskConfig{
		TaskID:      "t-01OVERRIDE",
		TaskSlug:    "override-test",
		WsRoot:      ws,
		PermManager: pm,
		SkipInject:  true,
		ExtraServers: map[string]host.ServerConfig{
			"shared":   {Command: stdioServerCommand(), Env: stdioServerEnvMap(), Prefix: "fromreq"},
			"only-req": {Command: stdioServerCommand(), Env: stdioServerEnvMap()},
		},
	}
	inst, err := lm.StartTask(ctx, cfg)
	if err != nil {
		t.Fatalf("StartTask() error: %v", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_ = lm.StopTask(stopCtx, cfg.TaskID)
	}()

	es := inst.ExtraServers()
	if len(es) != 3 {
		t.Fatalf("expected 3 merged servers, got %d: %v", len(es), es)
	}
	// Request key wins on collision.
	if es["shared"].Prefix != "fromreq" {
		t.Fatalf("request should override file for 'shared': %q", es["shared"].Prefix)
	}
	if _, ok := es["only-file"]; !ok {
		t.Fatalf("file-only server 'only-file' missing: %v", es)
	}
	if _, ok := es["only-req"]; !ok {
		t.Fatalf("request-only server 'only-req' missing: %v", es)
	}
}

func TestStartTask_NoFileRequestOnlyUnchanged(t *testing.T) {
	pm := newPermManager()
	lm := lifecycle.New()
	ctx := context.Background()

	cfg := lifecycle.TaskConfig{
		TaskID:      "t-01NOFILE",
		TaskSlug:    "nofile-test",
		WsRoot:      t.TempDir(), // no .tether/task-config.json
		PermManager: pm,
		SkipInject:  true,
		ExtraServers: map[string]host.ServerConfig{
			"req": {Command: stdioServerCommand(), Env: stdioServerEnvMap()},
		},
	}
	inst, err := lm.StartTask(ctx, cfg)
	if err != nil {
		t.Fatalf("StartTask() error: %v", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_ = lm.StopTask(stopCtx, cfg.TaskID)
	}()

	es := inst.ExtraServers()
	if len(es) != 1 {
		t.Fatalf("expected only the request server, got %d: %v", len(es), es)
	}
	if _, ok := es["req"]; !ok {
		t.Fatalf("request server 'req' missing: %v", es)
	}
}

func TestStartTask_MalformedFileErrors(t *testing.T) {
	pm := newPermManager()
	lm := lifecycle.New()
	ctx := context.Background()

	ws := t.TempDir()
	writeManagerTaskConfig(t, ws, `{"version": 1, "servers": {`) // malformed

	cfg := lifecycle.TaskConfig{
		TaskID:      "t-01BADFILE",
		TaskSlug:    "badfile-test",
		WsRoot:      ws,
		PermManager: pm,
		SkipInject:  true,
	}
	_, err := lm.StartTask(ctx, cfg)
	if err == nil {
		t.Fatal("StartTask() should error on malformed task-config")
	}
	if _, ok := lm.Get(cfg.TaskID); ok {
		t.Fatal("no instance should be registered when task-config load fails")
	}
}

func TestStopNonExistent(t *testing.T) {
	lm := lifecycle.New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := lm.StopTask(ctx, "t-NONEXISTENT")
	if err == nil {
		t.Fatal("StopTask on unknown ID should return error")
	}
}

func TestStopAll(t *testing.T) {
	pm := newPermManager()
	lm := lifecycle.New()
	ctx := context.Background()

	cfg1 := baseConfig(t, pm, "t-01STOPALL1", "stopall-1")
	cfg2 := baseConfig(t, pm, "t-01STOPALL2", "stopall-2")

	if _, err := lm.StartTask(ctx, cfg1); err != nil {
		t.Fatalf("StartTask(1) error: %v", err)
	}
	if _, err := lm.StartTask(ctx, cfg2); err != nil {
		t.Fatalf("StartTask(2) error: %v", err)
	}

	if len(lm.Active()) != 2 {
		t.Fatalf("expected 2 active instances, got %d", len(lm.Active()))
	}

	stopCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lm.StopAll(stopCtx)

	if len(lm.Active()) != 0 {
		t.Fatalf("expected 0 active instances after StopAll, got %d", len(lm.Active()))
	}

	_, ok := lm.Get(cfg1.TaskID)
	if ok {
		t.Fatal("Get(cfg1) should return false after StopAll")
	}
	_, ok = lm.Get(cfg2.TaskID)
	if ok {
		t.Fatal("Get(cfg2) should return false after StopAll")
	}
}

// waitCond polls cond up to within, failing the test if it never becomes true.
func waitCond(t *testing.T, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", within)
}

// extConfig returns a TaskConfig with one external stdio MCP server.
func extConfig(t *testing.T, pm *permission.Manager, taskID, slug string) lifecycle.TaskConfig {
	t.Helper()
	cfg := baseConfig(t, pm, taskID, slug)
	cfg.ExtraServers = map[string]host.ServerConfig{
		"ext": {Command: stdioServerCommand(), Env: stdioServerEnvMap(), Prefix: "ext"},
	}
	return cfg
}

func TestWatchdogHibernatesIdle(t *testing.T) {
	pm := newPermManager()
	lm := lifecycle.New(lifecycle.WithIdleWatchdog(60*time.Millisecond, 20*time.Millisecond))
	ctx := context.Background()
	defer func() {
		c, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		lm.StopAll(c)
	}()

	inst, err := lm.StartTask(ctx, extConfig(t, pm, "t-01WDIDLE", "wd-idle"))
	if err != nil {
		t.Fatalf("StartTask: %v", err)
	}
	if inst.Dormant() {
		t.Fatal("should not be dormant immediately after start")
	}
	// The instance is idle; the watchdog should hibernate it past the threshold.
	waitCond(t, 3*time.Second, inst.Dormant)
}

func TestWatchdogSkipsRecentlyActive(t *testing.T) {
	pm := newPermManager()
	lm := lifecycle.New(lifecycle.WithIdleWatchdog(5*time.Second, 20*time.Millisecond))
	ctx := context.Background()
	defer func() {
		c, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		lm.StopAll(c)
	}()

	inst, err := lm.StartTask(ctx, extConfig(t, pm, "t-01WDACTIVE", "wd-active"))
	if err != nil {
		t.Fatalf("StartTask: %v", err)
	}
	// Many watchdog ticks pass but we are well within the 5s threshold.
	time.Sleep(300 * time.Millisecond)
	if inst.Dormant() {
		t.Fatal("recently-active instance must not be hibernated")
	}
}

func TestWatchdogDisabled(t *testing.T) {
	pm := newPermManager()
	ctx := context.Background()
	cases := map[string]*lifecycle.LifecycleManager{
		"default_off":    lifecycle.New(),
		"threshold_zero": lifecycle.New(lifecycle.WithIdleWatchdog(0, 10*time.Millisecond)),
	}
	for name, lm := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				c, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				lm.StopAll(c)
			}()
			inst, err := lm.StartTask(ctx, extConfig(t, pm, "t-01WDOFF"+name, "wd-off-"+name))
			if err != nil {
				t.Fatalf("StartTask: %v", err)
			}
			time.Sleep(200 * time.Millisecond)
			if inst.Dormant() {
				t.Fatal("watchdog disabled: instance must not hibernate")
			}
		})
	}
}

func TestStopAllStopsWatchdogIdempotent(t *testing.T) {
	lm := lifecycle.New(lifecycle.WithIdleWatchdog(50*time.Millisecond, 20*time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// First StopAll must stop the watchdog goroutine without hanging.
	lm.StopAll(ctx)
	// Second StopAll must be a no-op (no double-close panic, no hang).
	lm.StopAll(ctx)
}
