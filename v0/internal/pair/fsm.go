package pair

import (
	"errors"
	"fmt"
)

// State is the FSM state per spec §2. Idle is the start state; Paired
// and Failed are terminal.
type State int

const (
	StateIdle State = iota
	StateInviting
	StateAwaitingPubkey
	StateSASConfirm
	StateCompleting
	StatePaired
	StateFailed
)

func (s State) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateInviting:
		return "inviting"
	case StateAwaitingPubkey:
		return "awaiting-pubkey"
	case StateSASConfirm:
		return "sas-confirm"
	case StateCompleting:
		return "completing"
	case StatePaired:
		return "paired"
	case StateFailed:
		return "failed"
	default:
		return fmt.Sprintf("state(%d)", int(s))
	}
}

// EventKind discriminates the kinds of events that drive Step.
type EventKind int

const (
	// EventRecvFrame: peer sent us a frame (frame is in Event.Frame).
	EventRecvFrame EventKind = iota
	// EventUserConfirmsSAS: user clicked "match" on the local UI; FSM
	// emits a pair.sas-confirm{ok:true,mac:...}.
	EventUserConfirmsSAS
	// EventUserRejectsSAS: user clicked "do not match"; FSM emits
	// pair.abort{sas-mismatch}.
	EventUserRejectsSAS
	// EventUserCancel: user cancelled; FSM emits pair.abort{user-cancel}.
	EventUserCancel
	// EventTimeout: the timer for the current state expired.
	EventTimeout
	// EventStartInvite: initiator-side trigger to leave Idle and emit
	// the invite. Frame field carries the populated InviteFrame.
	EventStartInvite
)

// Event is what Step consumes. Exactly one of (Frame, populated by
// EventRecvFrame / EventStartInvite) is meaningful per Kind.
type Event struct {
	Kind  EventKind
	Frame Frame
}

// FSM holds per-pairing state. It is intentionally minimal — it does
// NO crypto, NO IO. Drivers (client.go / server.go) own the keys and
// the io.ReadWriter; the FSM just decides which frames are legal in
// which state and tracks anti-replay timestamps.
type FSM struct {
	role  Role
	state State

	// lastSeenTS tracks per-direction strictly-monotonic ts (spec §6.1).
	// For an FSM running as the initiator, this is the responder's last
	// ts; for responder, the initiator's. Daemon-emitted frames go
	// through the same field — there is only one inbound peer per FSM
	// instance for v0.1 (the daemon mediates but presents as a single
	// inbound stream to each endpoint).
	lastSeenTS int64
}

// NewFSM returns a fresh FSM for the given role, starting at Idle.
func NewFSM(role Role) *FSM {
	return &FSM{role: role, state: StateIdle}
}

// State returns the current state. Read-only.
func (f *FSM) State() State { return f.state }

// Role returns the role this FSM was created for. Read-only.
func (f *FSM) Role() Role { return f.role }

// Errors that Step may surface.
var (
	// ErrUnexpectedFrame is returned when an inbound frame's kind is
	// not in the §6.2 allowed-frame set for the current state.
	ErrUnexpectedFrame = errors.New("pair: unexpected frame for state")
	// ErrReplay is returned when an inbound frame's ts is <= the
	// previously-seen ts from the same direction (spec §6.1).
	ErrReplay = errors.New("pair: replay (non-monotonic ts)")
	// ErrTerminal is returned when Step is called after the FSM has
	// reached a terminal state.
	ErrTerminal = errors.New("pair: FSM in terminal state")
	// ErrVersionIncompatible is returned when an inbound frame's
	// protocolVersion is not 1.
	ErrVersionIncompatible = errors.New("pair: version-incompatible")
)

// allowedInbound encodes the §6.2 per-state allowed-inbound-frame
// matrix. The boolean tells whether `kind` is allowed in `state` for
// the given `role`.
func allowedInbound(role Role, state State, kind FrameKind) bool {
	switch state {
	case StateIdle:
		// Initiator does not receive in idle; responder accepts only
		// pair.invite.
		return role == RoleResponder && kind == KindInvite
	case StateInviting:
		// Inviting (initiator only) accepts only pair.abort.
		return kind == KindAbort
	case StateAwaitingPubkey:
		// Initiator: pair.accept or pair.abort. Responder: nothing
		// inbound (the responder transitions through this state by
		// emitting an outbound pair.accept; any inbound frame here is
		// a violation).
		if role == RoleInitiator {
			return kind == KindAccept || kind == KindAbort
		}
		return false
	case StateSASConfirm:
		return kind == KindSASConfirm || kind == KindAbort
	case StateCompleting:
		return kind == KindComplete || kind == KindAbort
	case StatePaired, StateFailed:
		return false
	}
	return false
}

// Step is the pure transition function. It returns the new state, any
// outbound frames the FSM wants emitted, and an error.
//
// Drivers MUST NOT mutate the returned outbound frame slice (or the
// FSM's internal state); Step has already advanced the FSM by the time
// it returns. On error the state is updated to StateFailed and the
// outbound slice carries the appropriate pair.abort frame, so the
// driver can emit it before tearing down.
//
// Note on EventStartInvite + EventUserConfirmsSAS: these don't carry a
// fully-populated frame from the FSM's perspective — the driver
// composes the InviteFrame / SASConfirmFrame (with its keys, nonces,
// MAC) and passes it in via Event.Frame. The FSM appends it to the
// outbound list and advances state. This keeps crypto out of the FSM.
func (f *FSM) Step(ev Event) (State, []Frame, error) {
	if f.state == StatePaired || f.state == StateFailed {
		// §6.2: paired silently drops; failed silently drops. Spec
		// says "log warning, do NOT transition". Return the existing
		// state with no outbound frames and ErrTerminal so the caller
		// knows to stop pumping.
		return f.state, nil, ErrTerminal
	}

	switch ev.Kind {
	case EventRecvFrame:
		return f.stepRecvFrame(ev.Frame)

	case EventStartInvite:
		if f.state != StateIdle || f.role != RoleInitiator {
			return f.fail("start-invite outside idle")
		}
		invite, ok := ev.Frame.(InviteFrame)
		if !ok {
			return f.fail("start-invite without InviteFrame")
		}
		f.state = StateAwaitingPubkey
		return f.state, []Frame{invite}, nil

	case EventUserConfirmsSAS:
		if f.state != StateSASConfirm {
			return f.fail("user-confirms-sas outside sas-confirm")
		}
		conf, ok := ev.Frame.(SASConfirmFrame)
		if !ok {
			return f.fail("user-confirms-sas without SASConfirmFrame")
		}
		// Spec §2.1 (initiator) / §2.2 (responder): on user-confirms,
		// emit our own sas-confirm{ok:true} and step to completing if
		// initiator (initiator is the side that gates progress); for
		// responder we stay in sas-confirm until peer also confirms,
		// then completing on pair.complete arrival.
		if f.role == RoleInitiator {
			f.state = StateCompleting
		}
		return f.state, []Frame{conf}, nil

	case EventUserRejectsSAS:
		// Both roles: emit pair.abort{sas-mismatch} and fail.
		ab, ok := ev.Frame.(AbortFrame)
		if !ok {
			return f.fail("user-rejects-sas without AbortFrame")
		}
		f.state = StateFailed
		return f.state, []Frame{ab}, nil

	case EventUserCancel:
		ab, ok := ev.Frame.(AbortFrame)
		if !ok {
			return f.fail("user-cancel without AbortFrame")
		}
		f.state = StateFailed
		return f.state, []Frame{ab}, nil

	case EventTimeout:
		ab, ok := ev.Frame.(AbortFrame)
		if !ok {
			return f.fail("timeout without AbortFrame")
		}
		f.state = StateFailed
		return f.state, []Frame{ab}, nil
	}

	return f.fail(fmt.Sprintf("unhandled event kind %d", ev.Kind))
}

func (f *FSM) stepRecvFrame(frame Frame) (State, []Frame, error) {
	if frame == nil {
		return f.fail("recv-frame with nil frame")
	}

	// §6.1 anti-replay: per-direction strictly-monotonic ts. ts <=
	// last-seen → ErrReplay (treated as protocol-violation per spec).
	if frame.TS() <= f.lastSeenTS {
		f.state = StateFailed
		return f.state, []Frame{abortNow(ReasonProtocolViolation, "ts not strictly monotonic")}, ErrReplay
	}

	// §6.2 allowed-inbound matrix.
	if !allowedInbound(f.role, f.state, frame.Kind()) {
		f.state = StateFailed
		return f.state, []Frame{abortNow(ReasonProtocolViolation, fmt.Sprintf("frame %s not allowed in state %s/%s", frame.Kind(), f.role, f.state))}, ErrUnexpectedFrame
	}

	// Update last-seen ts BEFORE the kind-specific dispatch (spec §6.1
	// updates after acceptance; our acceptance is the matrix pass).
	f.lastSeenTS = frame.TS()

	switch frame.Kind() {
	case KindInvite:
		// Responder, idle → awaiting-pubkey.
		inv, ok := frame.(InviteFrame)
		if !ok {
			return f.fail("invite kind with wrong concrete type")
		}
		if inv.ProtocolVersion != ProtocolVersion {
			f.state = StateFailed
			return f.state, []Frame{abortNow(ReasonVersionIncompatible, fmt.Sprintf("invite v=%d unsupported", inv.ProtocolVersion))}, ErrVersionIncompatible
		}
		f.state = StateAwaitingPubkey
		return f.state, nil, nil

	case KindAccept:
		// Initiator, awaiting-pubkey → sas-confirm.
		acc, ok := frame.(AcceptFrame)
		if !ok {
			return f.fail("accept kind with wrong concrete type")
		}
		if acc.ProtocolVersion != ProtocolVersion {
			f.state = StateFailed
			return f.state, []Frame{abortNow(ReasonVersionIncompatible, fmt.Sprintf("accept v=%d unsupported", acc.ProtocolVersion))}, ErrVersionIncompatible
		}
		f.state = StateSASConfirm
		return f.state, nil, nil

	case KindSASConfirm:
		// Both roles, sas-confirm. Stay in sas-confirm until *both*
		// sides have observed the other's MAC AND the local user has
		// confirmed. The driver layer is responsible for tracking
		// "have I seen peer's mac AND have I sent mine"; the FSM just
		// reflects "peer's mac arrived, structurally OK".
		conf, ok := frame.(SASConfirmFrame)
		if !ok {
			return f.fail("sas-confirm kind with wrong concrete type")
		}
		if !conf.OK {
			// Peer rejected SAS. We fail.
			f.state = StateFailed
			return f.state, nil, nil
		}
		// MAC verification is the driver's responsibility (it has the
		// sas_key + transcript_hash). FSM just accepts the frame
		// arrived in the right slot.
		return f.state, nil, nil

	case KindComplete:
		// Completing → paired. AEAD tag verification is driver-side
		// (driver has the long_term_key); FSM accepts structural
		// arrival.
		f.state = StatePaired
		return f.state, nil, nil

	case KindAbort:
		// Any state with abort allowed → failed.
		f.state = StateFailed
		return f.state, nil, nil
	}

	return f.fail(fmt.Sprintf("unhandled frame kind %s", frame.Kind()))
}

func (f *FSM) fail(detail string) (State, []Frame, error) {
	f.state = StateFailed
	return f.state, []Frame{abortNow(ReasonProtocolViolation, detail)}, fmt.Errorf("pair: fsm fault: %s", detail)
}

// abortNow constructs a pair.abort with the given reason. The TS field
// is left at zero; the driver fills it in before sending (the FSM has
// no clock).
func abortNow(reason AbortReason, detail string) AbortFrame {
	return AbortFrame{
		ProtocolVersion: ProtocolVersion,
		Reason:          reason,
		Detail:          detail,
	}
}
