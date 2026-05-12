package instance_test

import (
	"context"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/mcp/instance"
	"github.com/piaobeizu/tether/internal/permission"
)

func newPermManager() *permission.Manager {
	return permission.New()
}

func TestNew_validation(t *testing.T) {
	pm := newPermManager()

	t.Run("TaskID empty", func(t *testing.T) {
		_, err := instance.New(instance.InstanceConfig{
			TaskID:      "",
			TaskSlug:    "test-slug",
			PermManager: pm,
			SkipInject:  true,
		})
		if err == nil {
			t.Fatal("expected error for empty TaskID")
		}
	})

	t.Run("TaskSlug empty", func(t *testing.T) {
		_, err := instance.New(instance.InstanceConfig{
			TaskID:      "t-01TESTID",
			TaskSlug:    "",
			PermManager: pm,
			SkipInject:  true,
		})
		if err == nil {
			t.Fatal("expected error for empty TaskSlug")
		}
	})

	t.Run("PermManager nil", func(t *testing.T) {
		_, err := instance.New(instance.InstanceConfig{
			TaskID:      "t-01TESTID",
			TaskSlug:    "test-slug",
			PermManager: nil,
			SkipInject:  true,
		})
		if err == nil {
			t.Fatal("expected error for nil PermManager")
		}
	})
}

func TestStartStop(t *testing.T) {
	pm := newPermManager()

	inst, err := instance.New(instance.InstanceConfig{
		TaskID:      "t-01STARTSTOP",
		TaskSlug:    "start-stop-test",
		WsRoot:      t.TempDir(),
		PermManager: pm,
		SkipInject:  true,
		Port:        0, // OS-assigned
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	if inst.Token == "" {
		t.Fatal("Token should be non-empty after New()")
	}

	ctx := context.Background()
	if err := inst.Start(ctx, nil); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	if inst.Port <= 0 {
		t.Fatalf("Port should be > 0 after Start(), got %d", inst.Port)
	}

	// Start is idempotent
	if err := inst.Start(ctx, nil); err != nil {
		t.Fatalf("second Start() should be a no-op, got error: %v", err)
	}

	stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := inst.Stop(stopCtx); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}

	// Stop twice is a no-op
	if err := inst.Stop(stopCtx); err != nil {
		t.Fatalf("second Stop() should be a no-op, got error: %v", err)
	}
}
