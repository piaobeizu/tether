package agent

import (
	"context"
	"testing"
	"time"
)

// TestIsTerminal pins which event kinds must be delivered reliably (tether#14).
func TestIsTerminal(t *testing.T) {
	terminal := []EventKind{EventResult, EventError}
	best := []EventKind{EventInit, EventText, EventToolUse, EventRateLimit}
	for _, k := range terminal {
		if !isTerminal(k) {
			t.Errorf("%q should be terminal (must-deliver)", k)
		}
	}
	for _, k := range best {
		if isTerminal(k) {
			t.Errorf("%q should be best-effort (droppable), not terminal", k)
		}
	}
}

// waitReturned reports whether close(done) happened within d.
func waitReturned(done <-chan struct{}, d time.Duration) bool {
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// --- opencode emit -----------------------------------------------------------

func TestOpencodeEmit_TerminalBlocksThenDelivers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &opencodeSession{spawnCtx: ctx, events: make(chan Event, 1)}

	// Fill the single-slot buffer.
	s.emit(Event{Kind: EventText, Text: "fill"})

	// A non-terminal emit on a full buffer must DROP (return immediately).
	dropped := make(chan struct{})
	go func() { s.emit(Event{Kind: EventText, Text: "drop"}); close(dropped) }()
	if !waitReturned(dropped, time.Second) {
		t.Fatal("non-terminal emit blocked on a full buffer; must drop")
	}

	// A terminal emit on a full buffer must BLOCK (not drop).
	term := make(chan struct{})
	go func() { s.emit(Event{Kind: EventResult, Text: "stop"}); close(term) }()
	if waitReturned(term, 50*time.Millisecond) {
		t.Fatal("terminal emit returned while buffer full; must block until delivered")
	}

	// Drain one slot → the blocked terminal send now proceeds.
	if got := <-s.events; got.Text != "fill" {
		t.Fatalf("first buffered event = %q, want \"fill\" (the dropped one must be gone)", got.Text)
	}
	if !waitReturned(term, time.Second) {
		t.Fatal("terminal emit did not complete after draining a slot")
	}
	if got := <-s.events; got.Kind != EventResult {
		t.Fatalf("delivered event kind = %q, want EventResult", got.Kind)
	}
}

func TestOpencodeEmit_TerminalUnblocksOnCtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &opencodeSession{spawnCtx: ctx, events: make(chan Event, 1)}
	s.emit(Event{Kind: EventText}) // fill

	term := make(chan struct{})
	go func() { s.emit(Event{Kind: EventError}); close(term) }()
	if waitReturned(term, 50*time.Millisecond) {
		t.Fatal("terminal emit returned before ctx cancel with a full buffer")
	}
	cancel() // session torn down; no consumer will ever drain
	if !waitReturned(term, time.Second) {
		t.Fatal("terminal emit did not unblock on ctx cancel (goroutine leak)")
	}
}

// TestOpencodeEmit_CloseEventsRaceWithBlockedTerminal pins the deadlock-freedom
// claim: a terminal emit blocked mid-send holds eventsMu.RLock while closeEvents
// contends for the write Lock. Safety relies on the consumer draining until the
// channel closes (as fanOut does), so here a drainer mirrors fanOut. Whichever
// wins the race — closeEvents sets closed first (terminal dropped) or the send
// lands first (terminal drained) — must not deadlock.
func TestOpencodeEmit_CloseEventsRaceWithBlockedTerminal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &opencodeSession{spawnCtx: ctx, events: make(chan Event, 1)}
	s.emit(Event{Kind: EventText}) // fill the single slot

	drained := make(chan struct{})
	go func() {
		for range s.events { //nolint:revive // draining like fanOut
		}
		close(drained)
	}()

	go s.emit(Event{Kind: EventResult, Text: "stop"})
	time.Sleep(20 * time.Millisecond) // best-effort: let the terminal emit park
	s.closeEvents()

	if !waitReturned(drained, 2*time.Second) {
		t.Fatal("deadlock: closeEvents vs blocked terminal emit never resolved")
	}
}

func TestOpencodeEmit_ClosedIsNoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &opencodeSession{spawnCtx: ctx, events: make(chan Event, 1)}
	s.closeEvents()
	// Must not panic on send-to-closed and must return promptly.
	done := make(chan struct{})
	go func() { s.emit(Event{Kind: EventResult}); close(done) }()
	if !waitReturned(done, time.Second) {
		t.Fatal("emit after closeEvents blocked; must be a no-op")
	}
}

// --- cc emit -----------------------------------------------------------------

func TestCCEmit_TerminalBlocksThenDelivers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &ccSession{ctx: ctx, events: make(chan Event, 1), sidReady: make(chan struct{})}

	s.emit(Event{Kind: EventText, Text: "fill"})

	dropped := make(chan struct{})
	go func() { s.emit(Event{Kind: EventText, Text: "drop"}); close(dropped) }()
	if !waitReturned(dropped, time.Second) {
		t.Fatal("non-terminal emit blocked on a full buffer; must drop")
	}

	term := make(chan struct{})
	go func() { s.emit(Event{Kind: EventResult, Text: "stop"}); close(term) }()
	if waitReturned(term, 50*time.Millisecond) {
		t.Fatal("terminal emit returned while buffer full; must block until delivered")
	}

	if got := <-s.events; got.Text != "fill" {
		t.Fatalf("first buffered event = %q, want \"fill\"", got.Text)
	}
	if !waitReturned(term, time.Second) {
		t.Fatal("terminal emit did not complete after draining a slot")
	}
	if got := <-s.events; got.Kind != EventResult {
		t.Fatalf("delivered event kind = %q, want EventResult", got.Kind)
	}
}

func TestCCEmit_TerminalUnblocksOnCtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &ccSession{ctx: ctx, events: make(chan Event, 1), sidReady: make(chan struct{})}
	s.emit(Event{Kind: EventText}) // fill

	term := make(chan struct{})
	go func() { s.emit(Event{Kind: EventResult}); close(term) }()
	if waitReturned(term, 50*time.Millisecond) {
		t.Fatal("terminal emit returned before ctx cancel with a full buffer")
	}
	cancel()
	if !waitReturned(term, time.Second) {
		t.Fatal("terminal emit did not unblock on ctx cancel (goroutine leak)")
	}
}
