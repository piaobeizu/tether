package pair

import (
	"errors"
	"fmt"
)

// ProtocolVersion is the wire-version constant for slice #4. Receivers
// that see a different value MUST emit pair.abort{version-incompatible}.
const ProtocolVersion = 1

// KeyVersionPair is the §3.3.1 envelope.keyVersion sentinel for
// pair-protocol frames. Per spec §3 the inner ciphertext is plaintext
// (no shared key exists for frames 1-2; frames 3+ keep the same shape
// for transcript-hash uniformity); keyVersion=0 is the marker for that
// "unencrypted, pair-protocol scope" mode.
const KeyVersionPair = 0

// DefaultUser is the v0.1 hardcoded user id (D-04 single-user). v0.2
// will let the responder validate userId from pair.invite; v0.1 just
// hardcodes "default" everywhere.
const DefaultUser = "default"

// DeviceID is the caller-stable identity of one device in a pair. Per
// spec §9.1 filenames are constrained to [a-zA-Z0-9-]+; ValidateDeviceID
// enforces that regex.
type DeviceID string

// DeviceKind enumerates the two device categories spec §3.1 lists in
// pair.invite.deviceMetadata.kind. "terminal" is added for completeness
// (see spec §11.D's lock.ClientID kinds); the wire only emits
// "desktop" / "mobile" today.
type DeviceKind string

const (
	KindDesktop  DeviceKind = "desktop"
	KindMobile   DeviceKind = "mobile"
	KindTerminal DeviceKind = "terminal"
)

// FrameKind discriminates the five pair frame types on the wire. These
// strings are what envelope.kind contains for pair.* frames.
type FrameKind string

const (
	KindInvite     FrameKind = "pair.invite"
	KindAccept     FrameKind = "pair.accept"
	KindSASConfirm FrameKind = "pair.sas-confirm"
	KindComplete   FrameKind = "pair.complete"
	KindAbort      FrameKind = "pair.abort"
)

// AbortReason enumerates the closed set of reasons pair.abort may carry
// (spec §3.5 + §12).
type AbortReason string

const (
	ReasonSASMismatch         AbortReason = "sas-mismatch"
	ReasonTimeout             AbortReason = "timeout"
	ReasonUserCancel          AbortReason = "user-cancel"
	ReasonVersionIncompatible AbortReason = "version-incompatible"
	ReasonDupDeviceID         AbortReason = "dup-deviceid"
	ReasonCertError           AbortReason = "cert-error"
	ReasonProtocolViolation   AbortReason = "protocol-violation"
)

// Role identifies which side computed a SAS-confirm MAC. Disambiguates
// the two MACs sharing the same sas_key + transcript_hash.
type Role string

const (
	RoleInitiator Role = "initiator"
	RoleResponder Role = "responder"
)

// Frame is the union of pair frame body types. Each concrete type holds
// the spec §3 fields verbatim; Kind() / TS() / canonicalBody() are how
// the FSM and transcript dispatch on a frame without knowing its
// concrete type.
type Frame interface {
	Kind() FrameKind
	TS() int64
	// canonicalBody returns the JCS-canonical (sorted keys, no
	// whitespace) JSON encoding of the frame body. Used by transcript.go
	// and by EnvelopeWrap.
	canonicalBody() ([]byte, error)
}

// InviteFrame is spec §3.1 pair.invite.
type InviteFrame struct {
	ProtocolVersion int        `json:"v"`
	InitiatorPubkey []byte     `json:"ephemeralPubkey"`
	DeviceID        DeviceID   `json:"deviceId"`
	Kind_           DeviceKind `json:"-"` // exposed via metadata.kind
	DisplayName     string     `json:"-"` // exposed via metadata.displayName
	TS_             int64      `json:"ts"`
	Nonce           []byte     `json:"nonce"`
}

func (f InviteFrame) Kind() FrameKind { return KindInvite }
func (f InviteFrame) TS() int64       { return f.TS_ }

// AcceptFrame is spec §3.2 pair.accept.
type AcceptFrame struct {
	ProtocolVersion int        `json:"v"`
	ResponderPubkey []byte     `json:"ephemeralPubkey"`
	DeviceID        DeviceID   `json:"deviceId"`
	Kind_           DeviceKind `json:"-"`
	DisplayName     string     `json:"-"`
	PushToken       string     `json:"-"`
	TS_             int64      `json:"ts"`
	Nonce           []byte     `json:"nonce"`
}

func (f AcceptFrame) Kind() FrameKind { return KindAccept }
func (f AcceptFrame) TS() int64       { return f.TS_ }

// SASConfirmFrame is spec §3.3 pair.sas-confirm.
type SASConfirmFrame struct {
	ProtocolVersion int    `json:"v"`
	OK              bool   `json:"ok"`
	Role            Role   `json:"role"`
	MAC             []byte `json:"mac,omitempty"`
	TS_             int64  `json:"ts"`
}

func (f SASConfirmFrame) Kind() FrameKind { return KindSASConfirm }
func (f SASConfirmFrame) TS() int64       { return f.TS_ }

// CompleteFrame is spec §3.4 pair.complete (daemon-emitted ack).
//
// v0.1 simplification: the spec describes an AEAD tag computed under
// the derived long_term_key; the slice-#4 implementation models this
// as a frame the daemon emits once both endpoints' SAS-confirm MACs
// have been observed. The TS field is the only field the bare ack
// needs to round-trip; richer payload (registeredAs, longTermKeyId,
// nonce, tag) lives in CompleteFrameExt for the daemon-driven path.
type CompleteFrame struct {
	ProtocolVersion int   `json:"v"`
	TS_             int64 `json:"ts"`
}

func (f CompleteFrame) Kind() FrameKind { return KindComplete }
func (f CompleteFrame) TS() int64       { return f.TS_ }

// AbortFrame is spec §3.5 pair.abort.
type AbortFrame struct {
	ProtocolVersion int         `json:"v"`
	Reason          AbortReason `json:"reason"`
	Detail          string      `json:"detail,omitempty"`
	TS_             int64       `json:"ts"`
}

func (f AbortFrame) Kind() FrameKind { return KindAbort }
func (f AbortFrame) TS() int64       { return f.TS_ }

// WireEnvelope is the §3.3.1 outer envelope shape limited to the fields
// pair-protocol cares about. The pair package never builds the full
// crypto-envelope (that's §11.C / internal/crypto's job); for pair
// frames keyVersion=0 marks the body as plaintext.
type WireEnvelope struct {
	Kind       FrameKind `json:"kind"`
	KeyVersion int       `json:"keyVersion"`
	Ciphertext []byte    `json:"ciphertext"`
	TS         int64     `json:"ts"`
}

// EnvelopeWrap wraps a pair frame into the §3.3.1 envelope shape with
// keyVersion=0 (per spec §14 OQ ratification — pair frames pre-shared-
// key are intentionally plaintext-marked) and the canonical-encoded
// body in Ciphertext.
func EnvelopeWrap(frame Frame) (WireEnvelope, error) {
	body, err := frame.canonicalBody()
	if err != nil {
		return WireEnvelope{}, fmt.Errorf("pair: canonical encode %s: %w", frame.Kind(), err)
	}
	return WireEnvelope{
		Kind:       frame.Kind(),
		KeyVersion: KeyVersionPair,
		Ciphertext: body,
		TS:         frame.TS(),
	}, nil
}

// ErrUnknownFrameKind is returned by EnvelopeUnwrap when an envelope's
// Kind is not one of the five pair.* values.
var ErrUnknownFrameKind = errors.New("pair: unknown frame kind")

// EnvelopeUnwrap decodes an envelope back into a concrete Frame. Used
// by the FSM driver layer (client.go / server.go) when reading frames
// off the control stream.
func EnvelopeUnwrap(env WireEnvelope) (Frame, error) {
	if env.KeyVersion != KeyVersionPair {
		return nil, fmt.Errorf("pair: keyVersion %d invalid for pair frame (must be %d)", env.KeyVersion, KeyVersionPair)
	}
	switch env.Kind {
	case KindInvite:
		return decodeInviteBody(env.Ciphertext)
	case KindAccept:
		return decodeAcceptBody(env.Ciphertext)
	case KindSASConfirm:
		return decodeSASConfirmBody(env.Ciphertext)
	case KindComplete:
		return decodeCompleteBody(env.Ciphertext)
	case KindAbort:
		return decodeAbortBody(env.Ciphertext)
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownFrameKind, env.Kind)
	}
}
