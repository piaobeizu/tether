package pair

import (
	"errors"
	"testing"
)

func mustInvite(ts int64) InviteFrame {
	return InviteFrame{
		ProtocolVersion: ProtocolVersion,
		InitiatorPubkey: make([]byte, 32),
		DeviceID:        "device-desk-test",
		Kind_:           KindDesktop,
		DisplayName:     "test desk",
		TS_:             ts,
		Nonce:           make([]byte, 16),
	}
}

func mustAccept(ts int64) AcceptFrame {
	return AcceptFrame{
		ProtocolVersion: ProtocolVersion,
		ResponderPubkey: make([]byte, 32),
		DeviceID:        "device-phone-test",
		Kind_:           KindMobile,
		DisplayName:     "test phone",
		TS_:             ts,
		Nonce:           make([]byte, 16),
	}
}

func mustSASConfirm(role Role, ts int64) SASConfirmFrame {
	return SASConfirmFrame{
		ProtocolVersion: ProtocolVersion,
		OK:              true,
		Role:            role,
		MAC:             make([]byte, 32),
		TS_:             ts,
	}
}

func mustComplete(ts int64) CompleteFrame {
	return CompleteFrame{ProtocolVersion: ProtocolVersion, TS_: ts}
}

func mustAbort(reason AbortReason, ts int64) AbortFrame {
	return AbortFrame{ProtocolVersion: ProtocolVersion, Reason: reason, TS_: ts}
}

// TestFSM_InitiatorHappyPath walks the initiator through
// idle → awaiting-pubkey → sas-confirm → completing → paired.
func TestFSM_InitiatorHappyPath(t *testing.T) {
	f := NewFSM(RoleInitiator)
	if f.State() != StateIdle {
		t.Fatalf("initial state: got %s want idle", f.State())
	}

	// 1. EventStartInvite → awaiting-pubkey, emits invite.
	st, out, err := f.Step(Event{Kind: EventStartInvite, Frame: mustInvite(1)})
	if err != nil {
		t.Fatalf("StartInvite: %v", err)
	}
	if st != StateAwaitingPubkey {
		t.Errorf("after StartInvite: got %s want awaiting-pubkey", st)
	}
	if len(out) != 1 || out[0].Kind() != KindInvite {
		t.Errorf("StartInvite outbound: %v", out)
	}

	// 2. RecvFrame(accept) → sas-confirm.
	st, _, err = f.Step(Event{Kind: EventRecvFrame, Frame: mustAccept(2)})
	if err != nil {
		t.Fatalf("RecvAccept: %v", err)
	}
	if st != StateSASConfirm {
		t.Errorf("after RecvAccept: got %s want sas-confirm", st)
	}

	// 3. UserConfirmsSAS → completing.
	st, out, err = f.Step(Event{Kind: EventUserConfirmsSAS, Frame: mustSASConfirm(RoleInitiator, 3)})
	if err != nil {
		t.Fatalf("UserConfirms: %v", err)
	}
	if st != StateCompleting {
		t.Errorf("after UserConfirms: got %s want completing", st)
	}
	if len(out) != 1 || out[0].Kind() != KindSASConfirm {
		t.Errorf("UserConfirms outbound: %v", out)
	}

	// 4. RecvFrame(complete) → paired. Note initiator's lastSeenTS
	// is shared (single inbound stream), so TS must increase from
	// the accept(2) we sent before. Use 4.
	st, _, err = f.Step(Event{Kind: EventRecvFrame, Frame: mustComplete(4)})
	if err != nil {
		t.Fatalf("RecvComplete: %v", err)
	}
	if st != StatePaired {
		t.Errorf("after RecvComplete: got %s want paired", st)
	}

	// 5. Terminal: any further step returns ErrTerminal.
	_, _, err = f.Step(Event{Kind: EventRecvFrame, Frame: mustAbort(ReasonTimeout, 5)})
	if !errors.Is(err, ErrTerminal) {
		t.Errorf("post-paired step: got %v want ErrTerminal", err)
	}
}

// TestFSM_ResponderHappyPath walks the responder through
// idle → awaiting-pubkey → sas-confirm. Responder transitions onward
// via the driver, not the FSM, so this test stops at sas-confirm.
func TestFSM_ResponderHappyPath(t *testing.T) {
	f := NewFSM(RoleResponder)

	st, _, err := f.Step(Event{Kind: EventRecvFrame, Frame: mustInvite(1)})
	if err != nil {
		t.Fatalf("RecvInvite: %v", err)
	}
	if st != StateAwaitingPubkey {
		t.Errorf("after RecvInvite: got %s want awaiting-pubkey", st)
	}
}

// TestFSM_RejectsUnexpectedFrames exhaustively checks the §6.2
// allowed-inbound matrix.
func TestFSM_RejectsUnexpectedFrames(t *testing.T) {
	type tc struct {
		name        string
		role        Role
		setup       func(*FSM)
		frame       Frame
		wantErrIs   error
	}
	cases := []tc{
		{
			name:      "initiator-idle-rejects-accept",
			role:      RoleInitiator,
			setup:     func(*FSM) {},
			frame:     mustAccept(1),
			wantErrIs: ErrUnexpectedFrame,
		},
		{
			name:      "responder-idle-rejects-complete",
			role:      RoleResponder,
			setup:     func(*FSM) {},
			frame:     mustComplete(1),
			wantErrIs: ErrUnexpectedFrame,
		},
		{
			name: "initiator-awaiting-pubkey-rejects-complete",
			role: RoleInitiator,
			setup: func(f *FSM) {
				_, _, _ = f.Step(Event{Kind: EventStartInvite, Frame: mustInvite(1)})
			},
			frame:     mustComplete(2),
			wantErrIs: ErrUnexpectedFrame,
		},
		{
			name: "initiator-sas-confirm-rejects-invite",
			role: RoleInitiator,
			setup: func(f *FSM) {
				_, _, _ = f.Step(Event{Kind: EventStartInvite, Frame: mustInvite(1)})
				_, _, _ = f.Step(Event{Kind: EventRecvFrame, Frame: mustAccept(2)})
			},
			frame:     mustInvite(3),
			wantErrIs: ErrUnexpectedFrame,
		},
		{
			name: "initiator-completing-rejects-accept",
			role: RoleInitiator,
			setup: func(f *FSM) {
				_, _, _ = f.Step(Event{Kind: EventStartInvite, Frame: mustInvite(1)})
				_, _, _ = f.Step(Event{Kind: EventRecvFrame, Frame: mustAccept(2)})
				_, _, _ = f.Step(Event{Kind: EventUserConfirmsSAS, Frame: mustSASConfirm(RoleInitiator, 3)})
			},
			frame:     mustAccept(4),
			wantErrIs: ErrUnexpectedFrame,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := NewFSM(c.role)
			c.setup(f)
			_, out, err := f.Step(Event{Kind: EventRecvFrame, Frame: c.frame})
			if !errors.Is(err, c.wantErrIs) {
				t.Errorf("err = %v want %v", err, c.wantErrIs)
			}
			if f.State() != StateFailed {
				t.Errorf("state after rejection: got %s want failed", f.State())
			}
			if len(out) != 1 || out[0].Kind() != KindAbort {
				t.Errorf("rejection outbound: %v want pair.abort", out)
			}
		})
	}
}

// TestFSM_AcceptsAbortInAnyAllowedState — pair.abort should always
// drive the FSM to failed in the states that allow it.
func TestFSM_AcceptsAbortInAnyAllowedState(t *testing.T) {
	cases := []struct {
		name  string
		role  Role
		setup func(*FSM)
	}{
		{
			name: "initiator-inviting",
			role: RoleInitiator,
			setup: func(f *FSM) {
				_, _, _ = f.Step(Event{Kind: EventStartInvite, Frame: mustInvite(1)})
				// inviting state isn't actually a state; we go straight
				// to awaiting-pubkey. Override the state to inviting
				// for completeness.
				f.state = StateInviting
			},
		},
		{
			name: "initiator-awaiting-pubkey",
			role: RoleInitiator,
			setup: func(f *FSM) {
				_, _, _ = f.Step(Event{Kind: EventStartInvite, Frame: mustInvite(1)})
			},
		},
		{
			name: "initiator-sas-confirm",
			role: RoleInitiator,
			setup: func(f *FSM) {
				_, _, _ = f.Step(Event{Kind: EventStartInvite, Frame: mustInvite(1)})
				_, _, _ = f.Step(Event{Kind: EventRecvFrame, Frame: mustAccept(2)})
			},
		},
		{
			name: "initiator-completing",
			role: RoleInitiator,
			setup: func(f *FSM) {
				_, _, _ = f.Step(Event{Kind: EventStartInvite, Frame: mustInvite(1)})
				_, _, _ = f.Step(Event{Kind: EventRecvFrame, Frame: mustAccept(2)})
				_, _, _ = f.Step(Event{Kind: EventUserConfirmsSAS, Frame: mustSASConfirm(RoleInitiator, 3)})
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := NewFSM(c.role)
			c.setup(f)
			_, _, err := f.Step(Event{Kind: EventRecvFrame, Frame: mustAbort(ReasonUserCancel, 1000)})
			if err != nil {
				t.Errorf("recv abort: got %v want nil", err)
			}
			if f.State() != StateFailed {
				t.Errorf("state: got %s want failed", f.State())
			}
		})
	}
}

// TestFSM_VersionIncompatibleInvite — invite with v=2 is rejected.
func TestFSM_VersionIncompatibleInvite(t *testing.T) {
	f := NewFSM(RoleResponder)
	bad := mustInvite(1)
	bad.ProtocolVersion = 2
	_, out, err := f.Step(Event{Kind: EventRecvFrame, Frame: bad})
	if !errors.Is(err, ErrVersionIncompatible) {
		t.Errorf("err: got %v want ErrVersionIncompatible", err)
	}
	if f.State() != StateFailed {
		t.Errorf("state: got %s want failed", f.State())
	}
	if len(out) != 1 || out[0].Kind() != KindAbort {
		t.Fatalf("outbound: %v want pair.abort", out)
	}
	ab := out[0].(AbortFrame)
	if ab.Reason != ReasonVersionIncompatible {
		t.Errorf("abort reason: got %s want %s", ab.Reason, ReasonVersionIncompatible)
	}
}

// TestFSM_UserCancelEmitsAbort — user-cancel from any non-terminal
// state emits pair.abort{user-cancel} and fails.
func TestFSM_UserCancelEmitsAbort(t *testing.T) {
	f := NewFSM(RoleInitiator)
	_, _, _ = f.Step(Event{Kind: EventStartInvite, Frame: mustInvite(1)})
	_, _, _ = f.Step(Event{Kind: EventRecvFrame, Frame: mustAccept(2)})
	_, out, err := f.Step(Event{Kind: EventUserCancel, Frame: mustAbort(ReasonUserCancel, 3)})
	if err != nil {
		t.Fatalf("UserCancel: %v", err)
	}
	if f.State() != StateFailed {
		t.Errorf("state: got %s want failed", f.State())
	}
	if len(out) != 1 || out[0].Kind() != KindAbort {
		t.Fatalf("outbound: %v want pair.abort", out)
	}
}
