// internal/permission/manager_test.go
package permission_test

import (
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/permission"
)

func TestManagerAddDecideAllow(t *testing.T) {
	m := permission.New()
	req := &permission.Request{Source: "claude_hook", ToolName: "bash", Args: []byte(`{}`)}
	ch := m.Add(req)
	if req.ID == "" {
		t.Fatal("Add must set req.ID")
	}
	go func() { m.Decide(req.ID, true, "") }()
	dec := <-ch
	if !dec.Allow {
		t.Fatalf("expected Allow=true, got %+v", dec)
	}
}

func TestManagerAddDecideDeny(t *testing.T) {
	m := permission.New()
	req := &permission.Request{Source: "mcp:polyforge-coding", ToolName: "commit"}
	ch := m.Add(req)
	go func() { m.Decide(req.ID, false, "user denied") }()
	dec := <-ch
	if dec.Allow || dec.Reason != "user denied" {
		t.Fatalf("expected deny with reason, got %+v", dec)
	}
}

func TestManagerTimeout(t *testing.T) {
	oldTimeout := permission.Timeout
	permission.Timeout = 50 * time.Millisecond
	defer func() { permission.Timeout = oldTimeout }()

	m := permission.New()
	req := &permission.Request{Source: "claude_hook", ToolName: "write"}
	ch := m.Add(req)
	dec := <-ch
	if dec.Allow {
		t.Fatal("timeout must produce Allow=false")
	}
}

func TestManagerDecideUnknownID(t *testing.T) {
	m := permission.New()
	if m.Decide("nonexistent", true, "") {
		t.Fatal("Decide on unknown ID must return false")
	}
}

func TestManagerGetPending(t *testing.T) {
	m := permission.New()
	req := &permission.Request{Source: "claude_hook", ToolName: "read"}
	m.Add(req)
	got := m.GetPending(req.ID)
	if got == nil || got.ToolName != "read" {
		t.Fatalf("GetPending returned wrong req: %+v", got)
	}
	m.Decide(req.ID, true, "")
	if m.GetPending(req.ID) != nil {
		t.Fatal("GetPending after Decide must return nil")
	}
}

// ============================================================================
// tether#132 — Pending() is the backfill a reconnecting chat client is given.
// ============================================================================

func idsOf(reqs []*permission.Request) []string {
	out := make([]string, len(reqs))
	for i, r := range reqs {
		out[i] = r.ToolName
	}
	return out
}

// TestPending_ListsEveryOutstandingRequestInArrivalOrder — the shape the backfill
// needs. GetPending answers only for an id the caller already has, which a client
// that never received the announcement by definition does not; this is the
// question it can actually ask.
//
// Order is asserted because it is consumed as a QUEUE: the frontend appends each
// arriving request and renders the list, so the order this returns is the order
// the cards appear in after a reload. A map iteration would shuffle them, and a
// timestamp would tie for the parallel-tool batch that motivated the queue in the
// first place (tether#40) — hence the counter.
func TestPending_ListsEveryOutstandingRequestInArrivalOrder(t *testing.T) {
	m := permission.New()
	for _, tool := range []string{"first", "second", "third"} {
		m.Add(&permission.Request{Source: "claude_hook", ToolName: tool})
	}

	// Repeated, because one pass over a 3-entry map can come out sorted by luck.
	for i := 0; i < 20; i++ {
		got := idsOf(m.Pending())
		if len(got) != 3 || got[0] != "first" || got[1] != "second" || got[2] != "third" {
			t.Fatalf("Pending() = %v, want [first second third]", got)
		}
	}
}

// TestPending_DropsADecidedRequest — Decide is one of the two ways an entry
// leaves the map, and a backfill that offered a decided request would prompt for
// a tool call that is already running.
func TestPending_DropsADecidedRequest(t *testing.T) {
	m := permission.New()
	keep := &permission.Request{Source: "claude_hook", ToolName: "keep"}
	gone := &permission.Request{Source: "claude_hook", ToolName: "gone"}
	m.Add(keep)
	m.Add(gone)
	m.Decide(gone.ID, true, "")

	if got := idsOf(m.Pending()); len(got) != 1 || got[0] != "keep" {
		t.Fatalf("Pending() after a decision = %v, want [keep]", got)
	}
}

// TestPending_DropsAnExpiredRequest — the other way an entry leaves, and the one
// the wi asked to be checked rather than assumed: Timeout is enforced by Add's
// time.AfterFunc DELETING the entry, so membership in the map already means "not
// expired" and Pending needs no deadline test of its own.
//
// What this pins is that the enforcement really is a delete and really does
// happen — a timer that only sent the decision would leave the entry behind, and
// every reconnect from then on would be handed a request whose HTTP caller has
// already been answered "timeout" and gone.
//
// The wait is 20x the timeout, so it is not a race being run: this fails only if
// the entry is never removed at all.
func TestPending_DropsAnExpiredRequest(t *testing.T) {
	oldTimeout := permission.Timeout
	permission.Timeout = 20 * time.Millisecond
	defer func() { permission.Timeout = oldTimeout }()

	m := permission.New()
	req := &permission.Request{Source: "claude_hook", ToolName: "expires"}
	ch := m.Add(req)

	if got := idsOf(m.Pending()); len(got) != 1 {
		t.Fatalf("Pending() before the deadline = %v, want one entry", got)
	}
	if dec := <-ch; dec.Reason != "timeout" {
		t.Fatalf("decision = %+v, want the timeout", dec)
	}

	deadline := time.Now().Add(400 * time.Millisecond)
	for {
		if len(m.Pending()) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Pending() still lists the expired request %q; every reconnecting client "+
				"would be prompted for a tool call that was already answered \"timeout\"", req.ID)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestPending_OfAnEmptyManagerIsEmpty — the daemon's ordinary state. A backfill
// that returned anything here would put a card on every reconnect.
func TestPending_OfAnEmptyManagerIsEmpty(t *testing.T) {
	if got := permission.New().Pending(); len(got) != 0 {
		t.Fatalf("Pending() on a fresh Manager = %v, want empty", got)
	}
}
