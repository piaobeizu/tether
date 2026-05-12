package lifecycle_test

import (
	"context"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/mcp/lifecycle"
	"github.com/piaobeizu/tether/internal/permission"
)

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
