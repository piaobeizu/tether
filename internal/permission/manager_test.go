// internal/permission/manager_test.go
package permission_test

import (
	"context"
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

// ============================================================================
// tether#137 — WithdrawSession stops offering a request whose decision has no
// consumer left. The whole point is that it is SELECTIVE, so most of what is
// below is about what it must NOT take.
// ============================================================================

// TestWithdrawSession_TakesBackTheRequestOfTheSessionThatEnded — the affirmative
// half. After this the request is out of Pending(), so the backfill cannot hand
// it to the next client that attaches, and the blocked HTTP handler behind it is
// released rather than left to wait out the remaining timeout.
func TestWithdrawSession_TakesBackTheRequestOfTheSessionThatEnded(t *testing.T) {
	m := permission.New()
	req := &permission.Request{Source: "unknown", SessionID: "sid-dead", ToolName: "bash"}
	ch := m.Add(req)

	got := m.WithdrawSession("sid-dead")
	if len(got) != 1 || got[0].ID != req.ID {
		t.Fatalf("WithdrawSession returned %v, want the one request %q", idsOf(got), req.ID)
	}
	if p := m.Pending(); len(p) != 0 {
		t.Fatalf("Pending() after the withdrawal = %v, want empty: the backfill would "+
			"still hand this to the next client that attaches", idsOf(p))
	}
	if m.GetPending(req.ID) != nil {
		t.Fatal("GetPending still answers for a withdrawn request")
	}
	// The waiter is released, not abandoned. A gate left blocked holds a process and
	// a daemon goroutine until the 60s timer, for an answer nobody will act on.
	select {
	case dec := <-ch:
		if dec.Allow {
			t.Fatalf("withdrawal decided Allow=true: %+v", dec)
		}
		if dec.Reason != permission.WithdrawnReason {
			t.Fatalf("Reason = %q, want %q", dec.Reason, permission.WithdrawnReason)
		}
	default:
		t.Fatal("WithdrawSession left the request's waiter blocked")
	}
	// A second call must be a no-op, because teardown is once-per-session but the
	// same sid can be reconnected and torn down again.
	if got := m.WithdrawSession("sid-dead"); len(got) != 0 {
		t.Fatalf("second WithdrawSession returned %v, want nothing", idsOf(got))
	}
}

// TestWithdrawSession_LeavesADaemonConsumedRequestAlone — the regression this
// change is most likely to cause, pinned (tether#134 §2.7).
//
// permission.Manager.Check is the MCP gateway's blocking check. The thing waiting
// for that decision is a goroutine inside this daemon, and it does not die with a
// chat connection — so the request is STILL ANSWERABLE after the chat session it
// happens to name has ended, and withdrawing it would break a working feature by
// answering the tool call "denied" behind the user's back.
//
// The sid is deliberately THE SAME as the withdrawn one, because a sid-only
// filter is the obvious implementation and it would pass a test that used
// different sids. Both of today's production Check call sites leave SessionID
// empty (internal/server/mcp_loopback.go, internal/mcp/instance/instance.go), so
// in the real daemon this collision cannot happen yet — which is exactly why it
// has to be the fixture: threading the sid through those call sites is an
// ordinary change, and this is what makes it safe.
func TestWithdrawSession_LeavesADaemonConsumedRequestAlone(t *testing.T) {
	m := permission.New()

	gate := &permission.Request{Source: "unknown", SessionID: "sid-shared", ToolName: "gate-request"}
	m.Add(gate)

	// Check blocks, so it runs on its own goroutine; waitForPendingCount is what
	// makes the request actually be in the map before the withdrawal. Without it
	// the withdrawal could win the race and the test would pass having never had
	// anything to protect.
	mcpDone := make(chan *permission.Decision, 1)
	mcpReq := permission.Request{Source: "mcp:polyforge", SessionID: "sid-shared", ToolName: "mcp-request"}
	go func() {
		dec, err := m.Check(context.Background(), &mcpReq)
		if err != nil {
			mcpDone <- nil
			return
		}
		mcpDone <- dec
	}()
	waitForPendingCount(t, m, 2)

	withdrawn := m.WithdrawSession("sid-shared")
	if len(withdrawn) != 1 || withdrawn[0].ToolName != "gate-request" {
		t.Fatalf("WithdrawSession took %v, want only [gate-request]", idsOf(withdrawn))
	}

	remaining := m.Pending()
	if len(remaining) != 1 || remaining[0].ToolName != "mcp-request" {
		t.Fatalf("Pending() after the withdrawal = %v, want only [mcp-request]: an "+
			"MCP tool call's permission check is consumed by this daemon and survives "+
			"a chat session ending", idsOf(remaining))
	}
	select {
	case dec := <-mcpDone:
		t.Fatalf("Check returned %+v — the MCP tool call was answered by a chat "+
			"session's teardown", dec)
	default:
	}

	// Positive control: it is still a live, answerable request and not merely one
	// that WithdrawSession failed to reach.
	if !m.Decide(remaining[0].ID, true, "user allowed") {
		t.Fatal("Decide could not resolve the surviving MCP request")
	}
	dec := <-mcpDone
	if dec == nil || !dec.Allow {
		t.Fatalf("Check returned %+v, want an allow", dec)
	}
}

// TestWithdrawSession_LeavesAnotherSessionsRequestAlone — one session ending must
// not clear the daemon. Two live chats is the ordinary state of this product.
func TestWithdrawSession_LeavesAnotherSessionsRequestAlone(t *testing.T) {
	m := permission.New()
	m.Add(&permission.Request{Source: "unknown", SessionID: "sid-a", ToolName: "for-a"})
	m.Add(&permission.Request{Source: "unknown", SessionID: "sid-b", ToolName: "for-b"})

	if got := m.WithdrawSession("sid-a"); len(got) != 1 || got[0].ToolName != "for-a" {
		t.Fatalf("WithdrawSession(sid-a) took %v, want [for-a]", idsOf(got))
	}
	if got := idsOf(m.Pending()); len(got) != 1 || got[0] != "for-b" {
		t.Fatalf("Pending() = %v, want [for-b]", got)
	}
}

// TestWithdrawSession_OfAnEmptySidTakesNothing — a request whose producer named no
// session carries SessionID "". "Every unattributed request in the daemon" is not
// what teardown means by "this session's", and teardown reaches here with an empty
// sid for any entry that never got registered under one.
func TestWithdrawSession_OfAnEmptySidTakesNothing(t *testing.T) {
	m := permission.New()
	m.Add(&permission.Request{Source: "unknown", ToolName: "unattributed"})

	if got := m.WithdrawSession(""); len(got) != 0 {
		t.Fatalf(`WithdrawSession("") took %v, want nothing`, idsOf(got))
	}
	if got := idsOf(m.Pending()); len(got) != 1 || got[0] != "unattributed" {
		t.Fatalf("Pending() = %v, want [unattributed]", got)
	}
}

// TestWithdrawSession_ReturnsTheBatchInArrivalOrder — the caller announces these
// to a UI that rendered them as a queue (tether#40's parallel-tool batch), so the
// order is the same property Pending's own order test defends.
func TestWithdrawSession_ReturnsTheBatchInArrivalOrder(t *testing.T) {
	// Repeated with a fresh Manager each time, because one pass over a 3-entry map
	// can come out sorted by luck.
	for i := 0; i < 20; i++ {
		mm := permission.New()
		for _, tool := range []string{"first", "second", "third"} {
			mm.Add(&permission.Request{Source: "unknown", SessionID: "sid-batch", ToolName: tool})
		}
		got := idsOf(mm.WithdrawSession("sid-batch"))
		if len(got) != 3 || got[0] != "first" || got[1] != "second" || got[2] != "third" {
			t.Fatalf("WithdrawSession = %v, want [first second third]", got)
		}
	}
}

// waitForPendingCount polls rather than sleeping, because Check registers its
// request on another goroutine and a fixed sleep is either flaky or slow.
func waitForPendingCount(t *testing.T, m *permission.Manager, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if len(m.Pending()) == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Pending() never reached %d entries (got %d)", want, len(m.Pending()))
		}
		time.Sleep(time.Millisecond)
	}
}
