// envelope.go — §3.3.1 wire envelope for the WT events channel
// (slice #3 of the WT block).
//
// This file defines the OVER-NETWORK envelope shape that rides on the
// WT `events` channel (channel-id 0x02; see §3.3.3). It is DISTINCT
// from `internal/agent.LocalEnvelope`, which is the in-process /
// local-Unix-socket projection consumed by `tether attach`.
//
// Shape mapping (LocalEnvelope → WireEnvelope):
//
//	LocalEnvelope.Kind / .ProviderType / .SessionID / .Skill /
//	.PlaintextMetadata / .CiphertextPayload / .SourceUUID
//	──────── JSON-marshal the whole struct ────────────►
//	plaintext bytes ──── XChaCha20-Poly1305 Seal ───────►
//	WireEnvelope.Ciphertext  (with .Nonce, AD bound, etc.)
//
//	WireEnvelope adds the cross-device routing fields the daemon-local
//	shape lacks: id (per-envelope unique), fromDeviceId, toDeviceId,
//	ts (millis), keyVersion, nonce, AD-bound `kind`.
//
// The `kind` field is REPLICATED at the WireEnvelope top level — server
// routing needs to see kind in plaintext for §3.3.3 priority decisions
// — AND included in the AEAD additional-data so a malicious server
// cannot rewrite kind without breaking the auth tag (§3.3.1 "AD binds
// kind" invariant).
//
// **AD format** (deterministic, byte-exact across Go ↔ Rust ↔ TS):
//
//	AD = LP(sessionId) || LP(fromDeviceId) || LP(toDeviceId) ||
//	     uint32_BE(keyVersion) || LP(kind)
//
// where LP(s) = uint16_BE(len(s)) || utf8(s). Length-prefixed so that
// no two distinct field tuples can collide via byte-shifting. See the
// `buildAD` helper below for the canonical implementation; the Rust
// side mirrors the same assembly verbatim (tether-app
// src-tauri/src/wt/envelope.rs).
//
// **Encoding**: JSON for v0.1 (per §3.3.1 "v0.1 用 JSON; v0.2 看带宽
// 再考虑 CBOR / protobuf"). The whole WireEnvelope is JSON-marshaled
// and pushed as a single length-prefixed frame on the events stream
// (see `WriteFrame` / `ReadFrame`).
//
// v0.1 KEY MANAGEMENT — see `DevSharedKey`. There is NO real key
// negotiation in slice #3; the dev key is hardcoded so server +
// client can interop while pairing (slice #4) is being specced in
// parallel. Production must NOT ship with this constant.

package wt

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

// SharedKeySize is the byte length of the AEAD key used to seal
// WireEnvelope ciphertext. Mirrors crypto.AEADKeySize (32 bytes for
// XChaCha20-Poly1305).
const SharedKeySize = chacha20poly1305.KeySize

// NonceSize is the size of the XChaCha20 nonce in WireEnvelope.Nonce.
const NonceSize = chacha20poly1305.NonceSizeX

// CurrentKeyVersion is the §3.3.1 `keyVersion` value emitted by all
// v0.1 senders. Per the spec, this is constant 1 in v0.1 and only
// bumps on protocol upgrade. Receivers MUST reject envelopes with
// any other keyVersion.
const CurrentKeyVersion uint32 = 1

// DevSharedKey is the **HARDCODED v0.1 development key** used until
// slice #4 (pairing) ships a real per-device ECDH-negotiated key.
//
// SLICE #4 PAIRING REPLACES THIS WITH A REAL ECDH-NEGOTIATED KEY.
// SLICE #4 PAIRING REPLACES THIS WITH A REAL ECDH-NEGOTIATED KEY.
// SLICE #4 PAIRING REPLACES THIS WITH A REAL ECDH-NEGOTIATED KEY.
//
// Why a constant rather than zero bytes: a constant lets the cross-
// stack smoke test verify the AEAD path actually does encrypt/decrypt
// — both sides have the same non-trivial key and a successful Open is
// real evidence (vs all-zero where a buggy implementation that
// "happens to xor with zero" still trivially round-trips). The
// constant has no relationship to any production secret and offers
// zero confidentiality guarantees in v0.1; the daemon's WT listener
// is bound to localhost / LAN only until pairing exists.
//
// Do NOT use this anywhere outside the WT envelope dispatch path. Do
// NOT log it. Do NOT make it operator-visible. It is an interim
// crutch, not a key.
var DevSharedKey = [SharedKeySize]byte{
	0x74, 0x65, 0x74, 0x68, 0x65, 0x72, 0x2d, 0x77,
	0x74, 0x2d, 0x64, 0x65, 0x76, 0x2d, 0x73, 0x68,
	0x61, 0x72, 0x65, 0x64, 0x2d, 0x6b, 0x65, 0x79,
	0x2d, 0x73, 0x6c, 0x69, 0x63, 0x65, 0x2d, 0x33,
}

// WireEnvelope is the §3.3.1 over-network envelope. JSON-marshaled
// onto the events channel. NOT to be confused with
// `internal/agent.LocalEnvelope`, which is the local-socket projection
// consumed by `tether attach` (see envelope_emitter.go's doc-comment
// on LocalEnvelope for the full distinction).
//
// JSON shape matches §3.3.1 verbatim:
//
//	{
//	  "id":           "uuid-v4",
//	  "fromDeviceId": "device-cli-1",
//	  "toDeviceId":   "device-app-2",
//	  "ts":           1714000000000,        // unix milliseconds
//	  "keyVersion":   1,
//	  "nonce":        "<base64 24-byte>",
//	  "kind":         "output.agent-event",
//	  "ciphertext":   "<base64>"
//	}
//
// `kind` is replicated AD-bound — see file-level docstring.
//
// `sessionId` is intentionally OMITTED from this struct surface. The
// spec §3.3.1 lists sessionId in the envelope, but for v0.1 the WT
// session is per-(daemon, client) and carries one cc session at a
// time; the LocalEnvelope payload's `sessionId` field is what
// downstream consumers need anyway. Slice #4+ may promote sessionId
// to an envelope-level field; until then we keep the wire footprint
// minimal.
type WireEnvelope struct {
	ID           string `json:"id"`
	FromDeviceID string `json:"fromDeviceId"`
	ToDeviceID   string `json:"toDeviceId"`
	TS           int64  `json:"ts"`
	KeyVersion   uint32 `json:"keyVersion"`
	Nonce        []byte `json:"nonce"`      // 24 bytes; base64 in JSON
	Kind         string `json:"kind"`       // AD-bound, plaintext for routing
	Ciphertext   []byte `json:"ciphertext"` // base64 in JSON
}

// ErrWireEnvelope is the wrap-error for envelope sealing/opening
// failures. Distinct from the wt package's stream-level errors so
// callers can branch.
var ErrWireEnvelope = errors.New("wt: wire envelope")

// SealOptions bundles the inputs to `Seal`. Splitting out keeps the
// callsite readable when more fields land (slice #4 will add
// `KeyVersion` overrides for rotation tests).
type SealOptions struct {
	// SharedKey is the 32-byte AEAD key (XChaCha20-Poly1305). Use
	// DevSharedKey[:] for v0.1 dev paths until slice #4 lands.
	SharedKey []byte

	// FromDeviceID / ToDeviceID — routing IDs. Both empty is rejected.
	FromDeviceID string
	ToDeviceID   string

	// Kind — the §3.3.2 op kind. Required, AD-bound.
	Kind string

	// Plaintext — the bytes to seal. Caller is expected to have already
	// JSON-marshaled the LocalEnvelope (or whatever payload is appropriate
	// for `Kind`). We do NOT marshal here so `Kind != "output.agent-event"`
	// callers can ship raw control-plane bytes if desired.
	Plaintext []byte

	// SessionID is mixed into AD via the kind-bound construction; v0.1
	// passes the cc session id through here so a stolen ciphertext from
	// session A cannot be replayed into session B even with the same
	// shared key. May be empty if not yet known (smoke tests).
	SessionID string
}

// Seal builds a WireEnvelope from a sealed plaintext. Generates a
// fresh UUID v4 for ID, a random 24-byte nonce, sets ts = unix-millis
// now, keyVersion = CurrentKeyVersion. The AD is computed
// deterministically — see file-level docstring.
//
// Returns the WireEnvelope ready to JSON-marshal + push down the
// events channel. The Ciphertext field includes the 16-byte Poly1305
// tag suffix (chacha20poly1305 convention).
func Seal(opts SealOptions) (*WireEnvelope, error) {
	if len(opts.SharedKey) != SharedKeySize {
		return nil, fmt.Errorf("%w: shared key must be %d bytes (got %d)", ErrWireEnvelope, SharedKeySize, len(opts.SharedKey))
	}
	if opts.Kind == "" {
		return nil, fmt.Errorf("%w: kind required (AD-bound)", ErrWireEnvelope)
	}
	if opts.FromDeviceID == "" || opts.ToDeviceID == "" {
		return nil, fmt.Errorf("%w: fromDeviceId and toDeviceId required", ErrWireEnvelope)
	}
	aead, err := chacha20poly1305.NewX(opts.SharedKey)
	if err != nil {
		return nil, fmt.Errorf("%w: new XChaCha20-Poly1305: %v", ErrWireEnvelope, err)
	}
	nonce := make([]byte, NonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("%w: read nonce: %v", ErrWireEnvelope, err)
	}
	id, err := newUUIDv4()
	if err != nil {
		return nil, fmt.Errorf("%w: generate id: %v", ErrWireEnvelope, err)
	}
	ad := buildAD(opts.SessionID, opts.FromDeviceID, opts.ToDeviceID, CurrentKeyVersion, opts.Kind)
	ct := aead.Seal(nil, nonce, opts.Plaintext, ad)
	return &WireEnvelope{
		ID:           id,
		FromDeviceID: opts.FromDeviceID,
		ToDeviceID:   opts.ToDeviceID,
		TS:           time.Now().UnixMilli(),
		KeyVersion:   CurrentKeyVersion,
		Nonce:        nonce,
		Kind:         opts.Kind,
		Ciphertext:   ct,
	}, nil
}

// OpenOptions bundles the inputs to `Open`. SessionID has the same AD
// role as in SealOptions — must match what the sender used.
type OpenOptions struct {
	SharedKey []byte
	SessionID string
}

// Open verifies + decrypts a WireEnvelope. Rejects:
//
//   - keyVersion != CurrentKeyVersion (no negotiation in v0.1)
//   - nonce length != NonceSize
//   - AEAD auth failure (wrong key, tampered AD, tampered ciphertext)
//   - empty Kind (AD construction would be ambiguous)
//
// Replay/dedup checks (LRU on env.ID, monotonic seq, ts skew window)
// are NOT done here — that's the consumer's responsibility because
// the LRU state is per-subscriber.
func Open(env *WireEnvelope, opts OpenOptions) ([]byte, error) {
	if env == nil {
		return nil, fmt.Errorf("%w: nil envelope", ErrWireEnvelope)
	}
	if len(opts.SharedKey) != SharedKeySize {
		return nil, fmt.Errorf("%w: shared key must be %d bytes (got %d)", ErrWireEnvelope, SharedKeySize, len(opts.SharedKey))
	}
	if env.KeyVersion != CurrentKeyVersion {
		return nil, fmt.Errorf("%w: unsupported keyVersion %d (want %d)", ErrWireEnvelope, env.KeyVersion, CurrentKeyVersion)
	}
	if len(env.Nonce) != NonceSize {
		return nil, fmt.Errorf("%w: nonce length %d != %d", ErrWireEnvelope, len(env.Nonce), NonceSize)
	}
	if env.Kind == "" {
		return nil, fmt.Errorf("%w: empty kind", ErrWireEnvelope)
	}
	aead, err := chacha20poly1305.NewX(opts.SharedKey)
	if err != nil {
		return nil, fmt.Errorf("%w: new XChaCha20-Poly1305: %v", ErrWireEnvelope, err)
	}
	ad := buildAD(opts.SessionID, env.FromDeviceID, env.ToDeviceID, env.KeyVersion, env.Kind)
	pt, err := aead.Open(nil, env.Nonce, env.Ciphertext, ad)
	if err != nil {
		return nil, fmt.Errorf("%w: open: %v", ErrWireEnvelope, err)
	}
	return pt, nil
}

// buildAD assembles the AD bytes for Seal/Open.
//
//	AD = LP(sessionId) || LP(fromDeviceId) || LP(toDeviceId) ||
//	     uint32_BE(keyVersion) || LP(kind)
//
// Length-prefixed UTF-8 strings (uint16_BE length) so an attacker
// cannot byte-shift between the fields. The Rust mirror in
// tether-app/src-tauri/src/wt/envelope.rs MUST produce byte-identical
// output.
func buildAD(sessionID, fromDeviceID, toDeviceID string, keyVersion uint32, kind string) []byte {
	out := make([]byte, 0, 2+len(sessionID)+2+len(fromDeviceID)+2+len(toDeviceID)+4+2+len(kind))
	out = appendLPString(out, sessionID)
	out = appendLPString(out, fromDeviceID)
	out = appendLPString(out, toDeviceID)
	out = binary.BigEndian.AppendUint32(out, keyVersion)
	out = appendLPString(out, kind)
	return out
}

func appendLPString(out []byte, s string) []byte {
	out = binary.BigEndian.AppendUint16(out, uint16(len(s)))
	out = append(out, s...)
	return out
}

// newUUIDv4 mints an RFC-4122 version-4 UUID using crypto/rand. Avoids
// pulling in github.com/google/uuid for one call site. The returned
// string is canonical lowercase 8-4-4-4-12 hex.
func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return "", err
	}
	// version 4 (random)
	b[6] = (b[6] & 0x0f) | 0x40
	// variant RFC 4122
	b[8] = (b[8] & 0x3f) | 0x80
	const hex = "0123456789abcdef"
	out := make([]byte, 36)
	pos := 0
	for i, x := range b {
		out[pos] = hex[x>>4]
		out[pos+1] = hex[x&0x0f]
		pos += 2
		if i == 3 || i == 5 || i == 7 || i == 9 {
			out[pos] = '-'
			pos++
		}
	}
	return string(out), nil
}

// --- Frame I/O on the events channel --------------------------------------
//
// The events channel is a single QUIC stream carrying a sequence of
// JSON-encoded WireEnvelopes. We use a 4-byte big-endian length prefix
// so the receiver can split frames without lookahead. Format:
//
//	frame := uint32_BE(len(json)) || json
//
// A length of 0 is reserved for future use (e.g. a keep-alive). A
// length > FrameSizeMax is treated as a wire-protocol error and the
// reader bails out.

// FrameSizeMax caps a single envelope frame at 1 MiB. The largest v0.1
// payload (a cc agent-event with embedded tool-result) is comfortably
// in the 64 KiB range; 1 MiB leaves slack for v0.2 fenced-block media
// without giving an attacker an unbounded alloc. Bumping this requires
// a coordinated bump on the Rust side.
const FrameSizeMax = 1 << 20

// WriteFrame JSON-marshals env and writes a length-prefixed frame to
// w. Returns the number of payload bytes written (excluding the
// length prefix).
//
// Caller must hold any write-side mutex on the underlying stream;
// WriteFrame is NOT goroutine-safe by itself.
func WriteFrame(w io.Writer, env *WireEnvelope) (int, error) {
	if env == nil {
		return 0, fmt.Errorf("%w: nil envelope", ErrWireEnvelope)
	}
	body, err := json.Marshal(env)
	if err != nil {
		return 0, fmt.Errorf("%w: marshal: %v", ErrWireEnvelope, err)
	}
	if len(body) > FrameSizeMax {
		return 0, fmt.Errorf("%w: frame size %d exceeds max %d", ErrWireEnvelope, len(body), FrameSizeMax)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	// Wrap with %w so callers (dispatch.go::PushEnvelopeStream) can
	// errors.Is(err, io.EOF) / io.ErrClosedPipe / context.Canceled to
	// distinguish clean peer-close from session-fatal write failures.
	// %v stripped the chain and broke that classification (R1 #4).
	if _, err := w.Write(hdr[:]); err != nil {
		return 0, fmt.Errorf("%w: write length: %w", ErrWireEnvelope, err)
	}
	if _, err := w.Write(body); err != nil {
		return 0, fmt.Errorf("%w: write body: %w", ErrWireEnvelope, err)
	}
	return len(body), nil
}

// ReadFrame reads one length-prefixed JSON frame off r and unmarshals
// it into a fresh WireEnvelope. Returns io.EOF cleanly when the stream
// has been closed at a frame boundary (caller's "peer is done" signal).
//
// A partial-read mid-frame returns io.ErrUnexpectedEOF (wrapped). A
// length-prefix > FrameSizeMax returns ErrWireEnvelope so the reader
// can decide whether to tear the session down (probable misbehavior).
func ReadFrame(r io.Reader) (*WireEnvelope, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err // io.EOF / io.ErrUnexpectedEOF passed through
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 {
		return nil, fmt.Errorf("%w: zero-length frame", ErrWireEnvelope)
	}
	if n > FrameSizeMax {
		return nil, fmt.Errorf("%w: frame size %d exceeds max %d", ErrWireEnvelope, n, FrameSizeMax)
	}
	body := make([]byte, n)
	// Wrap underlying read error with %w so callers can errors.Is
	// against io.EOF / io.ErrUnexpectedEOF / context.Canceled.
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("%w: read body: %w", ErrWireEnvelope, err)
	}
	var env WireEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("%w: unmarshal: %v", ErrWireEnvelope, err)
	}
	return &env, nil
}
