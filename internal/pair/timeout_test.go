package pair

import (
	"testing"
	"time"
)

// TestTimeout_SpecValues — the timeout constants match the spec §7
// table exactly. Future spec revisions must update both this test
// and the consumers.
func TestTimeout_SpecValues(t *testing.T) {
	if TimeoutAwaitingPubkey != 30*time.Second {
		t.Errorf("awaiting-pubkey timeout: got %s want 30s", TimeoutAwaitingPubkey)
	}
	if TimeoutSASConfirm != 60*time.Second {
		t.Errorf("sas-confirm timeout: got %s want 60s", TimeoutSASConfirm)
	}
	if TimeoutCompleting != 10*time.Second {
		t.Errorf("completing timeout: got %s want 10s", TimeoutCompleting)
	}
}

// TestTimeoutForState — TimeoutForState returns the spec-pinned
// duration per state.
func TestTimeoutForState(t *testing.T) {
	cases := []struct {
		state State
		want  time.Duration
	}{
		{StateIdle, 0},
		{StateInviting, 0},
		{StateAwaitingPubkey, TimeoutAwaitingPubkey},
		{StateSASConfirm, TimeoutSASConfirm},
		{StateCompleting, TimeoutCompleting},
		{StatePaired, 0},
		{StateFailed, 0},
	}
	for _, c := range cases {
		if got := TimeoutForState(c.state); got != c.want {
			t.Errorf("TimeoutForState(%s) = %s want %s", c.state, got, c.want)
		}
	}
}

// TestTimeout_FSMEmitsAbortOnExpiry — feeding an EventTimeout fails
// the FSM with the supplied pair.abort{timeout}.
func TestTimeout_FSMEmitsAbortOnExpiry(t *testing.T) {
	f := NewFSM(RoleInitiator)
	_, _, _ = f.Step(Event{Kind: EventStartInvite, Frame: mustInvite(1)})

	timeoutAbort := mustAbort(ReasonTimeout, 999)
	_, out, err := f.Step(Event{Kind: EventTimeout, Frame: timeoutAbort})
	if err != nil {
		t.Fatalf("timeout step: %v", err)
	}
	if f.State() != StateFailed {
		t.Errorf("state: got %s want failed", f.State())
	}
	if len(out) != 1 || out[0].Kind() != KindAbort {
		t.Fatalf("outbound: %v want pair.abort", out)
	}
	ab := out[0].(AbortFrame)
	if ab.Reason != ReasonTimeout {
		t.Errorf("abort reason: got %s want %s", ab.Reason, ReasonTimeout)
	}
}
