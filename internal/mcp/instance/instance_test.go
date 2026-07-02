package instance_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
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

func TestTouchAndIdleFor(t *testing.T) {
	pm := newPermManager()

	inst, err := instance.New(instance.InstanceConfig{
		TaskID:      "t-01TOUCH",
		TaskSlug:    "touch-test",
		WsRoot:      t.TempDir(),
		PermManager: pm,
		SkipInject:  true,
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	ctx := context.Background()
	if err := inst.Start(ctx, nil); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_ = inst.Stop(stopCtx)
	}()

	// Start() sets lastActive to ~now.
	first := inst.LastActive()
	if first.IsZero() {
		t.Fatal("LastActive() should be non-zero after Start()")
	}

	// IdleFor computes now.Sub(lastActive).
	now := first.Add(2 * time.Second)
	if got := inst.IdleFor(now); got != 2*time.Second {
		t.Fatalf("IdleFor() = %v, want 2s", got)
	}

	// Touch advances lastActive.
	time.Sleep(2 * time.Millisecond)
	inst.Touch()
	second := inst.LastActive()
	if !second.After(first) {
		t.Fatalf("Touch() should advance lastActive: first=%v second=%v", first, second)
	}
}

func TestAuthorizedRequestTouches(t *testing.T) {
	pm := newPermManager()

	inst, err := instance.New(instance.InstanceConfig{
		TaskID:      "t-01REQTOUCH",
		TaskSlug:    "req-touch-test",
		WsRoot:      t.TempDir(),
		PermManager: pm,
		SkipInject:  true,
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	ctx := context.Background()
	if err := inst.Start(ctx, nil); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_ = inst.Stop(stopCtx)
	}()

	before := inst.LastActive()
	time.Sleep(2 * time.Millisecond)

	url := fmt.Sprintf("http://127.0.0.1:%d/mcp", inst.Port)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+inst.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatalf("authorized request unexpectedly rejected: %d", resp.StatusCode)
	}

	after := inst.LastActive()
	if !after.After(before) {
		t.Fatalf("authorized request should advance lastActive: before=%v after=%v", before, after)
	}
}

func TestUnauthorizedRequestDoesNotTouch(t *testing.T) {
	pm := newPermManager()

	inst, err := instance.New(instance.InstanceConfig{
		TaskID:      "t-01NOAUTH",
		TaskSlug:    "noauth-test",
		WsRoot:      t.TempDir(),
		PermManager: pm,
		SkipInject:  true,
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	ctx := context.Background()
	if err := inst.Start(ctx, nil); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_ = inst.Stop(stopCtx)
	}()

	before := inst.LastActive()
	time.Sleep(2 * time.Millisecond)

	url := fmt.Sprintf("http://127.0.0.1:%d/mcp", inst.Port)
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	// No / bad Authorization header.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}

	after := inst.LastActive()
	if !after.Equal(before) {
		t.Fatalf("unauthorized request should NOT advance lastActive: before=%v after=%v", before, after)
	}
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
