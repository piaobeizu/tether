package crypto

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestReplay_AcceptsFreshThenRejectsDup(t *testing.T) {
	g := NewReplayGuard()
	now := time.Now()
	if err := g.Check("env-1", now); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := g.Check("env-1", now); !errors.Is(err, ErrReplay) {
		t.Fatalf("dup: want ErrReplay, got %v", err)
	}
	if err := g.Check("env-2", now); err != nil {
		t.Fatalf("different id: %v", err)
	}
}

func TestReplay_RejectsTooOld(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	g := NewReplayGuard(WithReplayClock(func() time.Time { return now }))
	stale := now.Add(-DefaultReplayWindow - time.Second)
	err := g.Check("env-old", stale)
	if !errors.Is(err, ErrReplay) {
		t.Fatalf("want ErrReplay for stale ts, got %v", err)
	}
}

func TestReplay_RejectsTooFuture(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	g := NewReplayGuard(WithReplayClock(func() time.Time { return now }))
	future := now.Add(DefaultReplayWindow + time.Second)
	err := g.Check("env-future", future)
	if !errors.Is(err, ErrReplay) {
		t.Fatalf("want ErrReplay for far-future ts, got %v", err)
	}
}

func TestReplay_AcceptsWithinWindow(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	g := NewReplayGuard(WithReplayClock(func() time.Time { return now }))
	cases := []time.Duration{
		-DefaultReplayWindow + time.Second,
		0,
		DefaultReplayWindow - time.Second,
	}
	for i, d := range cases {
		id := "env-" + time.Now().Format("150405") + "-" + string(rune('a'+i))
		if err := g.Check(id, now.Add(d)); err != nil {
			t.Fatalf("offset %s: %v", d, err)
		}
	}
}

func TestReplay_LRUEviction(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	g := NewReplayGuard(
		WithReplayClock(func() time.Time { return now }),
		WithReplayCacheSize(3),
	)
	// Fill exactly to capacity.
	for _, id := range []string{"a", "b", "c"} {
		if err := g.Check(id, now); err != nil {
			t.Fatalf("fill %s: %v", id, err)
		}
	}
	if g.Size() != 3 {
		t.Fatalf("size after fill = %d", g.Size())
	}
	// One more — evicts "a".
	if err := g.Check("d", now); err != nil {
		t.Fatalf("d: %v", err)
	}
	if g.Size() != 3 {
		t.Fatalf("size after evict = %d", g.Size())
	}
	// "b" and "c" still in cache → still rejected as dup.
	if err := g.Check("b", now); !errors.Is(err, ErrReplay) {
		t.Fatalf("b should still be in cache, got %v", err)
	}
	if err := g.Check("c", now); !errors.Is(err, ErrReplay) {
		t.Fatalf("c should still be in cache, got %v", err)
	}
	// "a" was evicted → can now be re-checked. Doing so will evict "b"
	// (the new oldest) — so we test "a" last to keep the assertion order
	// independent. In real deployment the timestamp window would still
	// reject "a" if it were genuinely a replay outside the 5-min window.
	if err := g.Check("a", now); err != nil {
		t.Fatalf("re-check evicted entry: %v", err)
	}
}

func TestReplay_TimeBasedGC(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	clk := now
	g := NewReplayGuard(WithReplayClock(func() time.Time { return clk }))

	if err := g.Check("old", clk); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Advance clock past window — "old" should be GC'd on next call.
	clk = clk.Add(DefaultReplayWindow + time.Minute)
	if err := g.Check("new", clk); err != nil {
		t.Fatalf("new: %v", err)
	}
	if g.Size() != 1 {
		t.Fatalf("size after GC = %d, want 1", g.Size())
	}
}

func TestReplay_EmptyIDRejected(t *testing.T) {
	g := NewReplayGuard()
	if err := g.Check("", time.Now()); !errors.Is(err, ErrReplay) {
		t.Fatalf("empty id: want ErrReplay, got %v", err)
	}
}

func TestReplay_ConcurrentDedup(t *testing.T) {
	g := NewReplayGuard()
	now := time.Now()
	const N = 100
	var wg sync.WaitGroup
	successes := make(chan struct{}, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := g.Check("hot-id", now); err == nil {
				successes <- struct{}{}
			}
		}()
	}
	wg.Wait()
	close(successes)
	count := 0
	for range successes {
		count++
	}
	if count != 1 {
		t.Fatalf("concurrent dedup: %d successes, want exactly 1", count)
	}
}
