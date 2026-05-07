package pair

import (
	"errors"
	"testing"
)

// TestReplay_TSNonMonotonicRejected — a frame with ts <= last-seen-ts
// is rejected with ErrReplay (treated as protocol-violation per
// spec §6.1).
func TestReplay_TSNonMonotonicRejected(t *testing.T) {
	f := NewFSM(RoleInitiator)
	_, _, err := f.Step(Event{Kind: EventStartInvite, Frame: mustInvite(1)})
	if err != nil {
		t.Fatalf("StartInvite: %v", err)
	}
	// Receive accept with ts=100.
	_, _, err = f.Step(Event{Kind: EventRecvFrame, Frame: mustAccept(100)})
	if err != nil {
		t.Fatalf("first accept: %v", err)
	}
	// Now a "replay" with ts=100 (<=) of any allowed frame must
	// trigger ErrReplay.
	bad := mustSASConfirm(RoleResponder, 100)
	_, out, err := f.Step(Event{Kind: EventRecvFrame, Frame: bad})
	if !errors.Is(err, ErrReplay) {
		t.Errorf("replay (ts==last): got %v want ErrReplay", err)
	}
	if f.State() != StateFailed {
		t.Errorf("state: got %s want failed", f.State())
	}
	if len(out) != 1 || out[0].Kind() != KindAbort {
		t.Errorf("outbound: %v want pair.abort", out)
	}
	ab := out[0].(AbortFrame)
	if ab.Reason != ReasonProtocolViolation {
		t.Errorf("abort reason: got %s want %s", ab.Reason, ReasonProtocolViolation)
	}
}

// TestReplay_TSStrictlyLessRejected — covers the < case explicitly.
func TestReplay_TSStrictlyLessRejected(t *testing.T) {
	f := NewFSM(RoleInitiator)
	_, _, _ = f.Step(Event{Kind: EventStartInvite, Frame: mustInvite(1)})
	_, _, _ = f.Step(Event{Kind: EventRecvFrame, Frame: mustAccept(50)})
	bad := mustSASConfirm(RoleResponder, 49) // strictly less
	_, _, err := f.Step(Event{Kind: EventRecvFrame, Frame: bad})
	if !errors.Is(err, ErrReplay) {
		t.Errorf("replay (ts<last): got %v want ErrReplay", err)
	}
}

// TestReplay_MonotonicTSAccepted — strictly increasing ts is fine.
func TestReplay_MonotonicTSAccepted(t *testing.T) {
	f := NewFSM(RoleInitiator)
	_, _, _ = f.Step(Event{Kind: EventStartInvite, Frame: mustInvite(1)})
	for i, ts := range []int64{10, 20, 30} {
		// Use a frame valid in the current state for each iteration:
		// after StartInvite we're in awaiting-pubkey, so accept (which
		// transitions to sas-confirm) is valid; after that, sas-confirm
		// frames are valid in sas-confirm; etc.
		var fr Frame
		switch i {
		case 0:
			fr = mustAccept(ts)
		case 1, 2:
			fr = mustSASConfirm(RoleResponder, ts)
		}
		if _, _, err := f.Step(Event{Kind: EventRecvFrame, Frame: fr}); err != nil {
			t.Fatalf("step #%d ts=%d: %v", i, ts, err)
		}
	}
}
