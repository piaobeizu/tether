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
//
// Optional deviceMetadata fields (Model / OSVersion / AppVersion) are
// included in the canonical body when non-empty. These cross-stack
// optionals are the BLOCKER-2 fix surface — Rust serializes Some(_) and
// skips None, so the Go side mirrors via empty-string-omit.
type InviteFrame struct {
	ProtocolVersion int        `json:"v"`
	InitiatorPubkey []byte     `json:"ephemeralPubkey"`
	DeviceID        DeviceID   `json:"deviceId"`
	Kind_           DeviceKind `json:"-"` // exposed via metadata.kind
	DisplayName     string     `json:"-"` // exposed via metadata.displayName
	Model           string     `json:"-"` // exposed via metadata.model (omit if empty)
	OSVersion       string     `json:"-"` // exposed via metadata.osVersion (omit if empty)
	AppVersion      string     `json:"-"` // exposed via metadata.appVersion (omit if empty)
	TS_             int64      `json:"ts"`
	Nonce           []byte     `json:"nonce"`
}

func (f InviteFrame) Kind() FrameKind { return KindInvite }
func (f InviteFrame) TS() int64       { return f.TS_ }

// AcceptFrame is spec §3.2 pair.accept.
//
// Optional deviceMetadata fields (Model / OSVersion / AppVersion)
// participate in the canonical body via empty-string-omit, matching
// Rust's Option::None skip behavior. PushToken is the optional spec
// §3.2 pushSubscription.payload.token.
type AcceptFrame struct {
	ProtocolVersion int        `json:"v"`
	ResponderPubkey []byte     `json:"ephemeralPubkey"`
	DeviceID        DeviceID   `json:"deviceId"`
	Kind_           DeviceKind `json:"-"`
	DisplayName     string     `json:"-"`
	Model           string     `json:"-"`
	OSVersion       string     `json:"-"`
	AppVersion      string     `json:"-"`
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
// Per spec §3.4, the frame carries an AEAD authenticator over the
// derived long-term key:
//
//	tag = XChaCha20-Poly1305.Seal(
//	    key       = long_term_key,
//	    nonce     = random 24B,
//	    plaintext = "" (empty),
//	    AD        = transcript_hash || "tether-pair-complete-v1",
//	)
//
// `Tag` carries the 16-byte AEAD tag (which is the entire ciphertext
// since plaintext is empty). Receivers MUST verify the tag with their
// own derived `long_term_key` + the transcript_hash they observed.
// Mismatch → emit pair.abort{cert-error} and DO NOT save the registry
// record. Without this verification, a rogue daemon (or any in-path
// actor that survived TLS) can forge a pair.complete and trick both
// endpoints into "succeeding" with an attacker-mediated key.
//
// RegisteredAs and LongTermKeyID are §3.4 informational fields the
// daemon assigns; they are echoed back to the UI for display but do
// NOT enter the AEAD AD (only `transcript_hash` does).
type CompleteFrame struct {
	ProtocolVersion int    `json:"v"`
	TS_             int64  `json:"ts"`
	Nonce           []byte `json:"-"` // 24B XChaCha20 nonce
	Tag             []byte `json:"-"` // 16B Poly1305 tag (= AEAD ciphertext over empty plaintext)
	// Optional informational fields (spec §3.4) — omitted from canonical
	// body when empty. The daemon assigns these post-pairing; they're
	// for UI / audit display only and do NOT enter the AEAD AD.
	RegisteredInitiatorDeviceID string `json:"-"`
	RegisteredResponderDeviceID string `json:"-"`
	LongTermKeyID               string `json:"-"`
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
