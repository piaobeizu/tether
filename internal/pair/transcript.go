package pair

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash"
	"sort"
	"strings"
)

// Transcript is the running SHA-256 over all canonicalized pair frame
// bodies appended in order. Each frame is fed to the hash as
// `LP4(frameKind) || LP4(frameBodyBytes)` so frame ordering AND frame
// kind are pinned: shuffling the order or swapping a kind for another
// produces a different transcript_hash.
//
// Per spec §5: each frame body is JSON-canonicalized (sorted keys, no
// whitespace, UTF-8 — RFC 8785 / JCS) before being fed.
//
// Per spec §5 the frames that go into T are: pair.invite, pair.accept,
// pair.sas-confirm (both directions). pair.complete and pair.abort are
// NOT appended; they reference the transcript hash as it stood after
// the SAS-confirm frames.
type Transcript struct {
	h hash.Hash
}

// NewTranscript starts a fresh empty transcript.
func NewTranscript() *Transcript {
	return &Transcript{h: sha256.New()}
}

// Append feeds a frame into the transcript. The frame is canonicalized
// (JCS) and then `LP4(kind) || LP4(body)` is mixed into the hash.
func (t *Transcript) Append(frame Frame) error {
	body, err := frame.canonicalBody()
	if err != nil {
		return fmt.Errorf("transcript: canonicalize %s: %w", frame.Kind(), err)
	}
	kind := []byte(frame.Kind())
	if err := writeLP4(t.h, kind); err != nil {
		return err
	}
	return writeLP4(t.h, body)
}

// Hash returns a snapshot of the running hash. Subsequent Append calls
// continue from the same state — Hash does not mutate the transcript.
func (t *Transcript) Hash() []byte {
	// hash.Hash.Sum(nil) returns the digest WITHOUT mutating the
	// internal state, so calling Hash() between Appends is safe.
	return t.h.Sum(nil)
}

func writeLP4(h hash.Hash, b []byte) error {
	var lp [4]byte
	binary.BigEndian.PutUint32(lp[:], uint32(len(b)))
	if _, err := h.Write(lp[:]); err != nil {
		return fmt.Errorf("transcript: write lp4: %w", err)
	}
	if _, err := h.Write(b); err != nil {
		return fmt.Errorf("transcript: write body: %w", err)
	}
	return nil
}

// canonicalJSON emits a JSON encoding of v with keys sorted at every
// object level, no whitespace, and base64url-no-pad encoding for
// []byte (matching the spec's wire convention of "base64url-32B" etc).
//
// The implementation walks the value via the standard encoding/json
// Marshal first to get a valid JSON tree, then re-encodes that tree
// with sorted keys. This is simpler than maintaining a parallel
// reflection-based encoder and matches the JCS subset we need (our
// frame bodies are flat structs with primitive / []byte / string
// fields — no nested JSON Number subtleties).
//
// For []byte fields we cannot rely on encoding/json's default base64-
// std encoding; we use base64url-no-pad explicitly. Since []byte is
// only used in InviteFrame.InitiatorPubkey, AcceptFrame.ResponderPubkey,
// SASConfirmFrame.MAC, and the nonce fields, the per-frame encode
// helpers below format those slices via base64url before assembling
// the map for canonicalJSON.
func canonicalJSON(m map[string]any) ([]byte, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		sb.Write(kb)
		sb.WriteByte(':')
		v := m[k]
		vb, err := encodeCanonValue(v)
		if err != nil {
			return nil, err
		}
		sb.Write(vb)
	}
	sb.WriteByte('}')
	return []byte(sb.String()), nil
}

func encodeCanonValue(v any) ([]byte, error) {
	switch vv := v.(type) {
	case map[string]any:
		return canonicalJSON(vv)
	case []any:
		var sb strings.Builder
		sb.WriteByte('[')
		for i, e := range vv {
			if i > 0 {
				sb.WriteByte(',')
			}
			eb, err := encodeCanonValue(e)
			if err != nil {
				return nil, err
			}
			sb.Write(eb)
		}
		sb.WriteByte(']')
		return []byte(sb.String()), nil
	default:
		// Fall back to encoding/json for primitives. Note: encoding/json
		// does NOT sort map keys — but at this leaf path v is always
		// a primitive (string, bool, int, float, nil), so no sort is
		// needed.
		return json.Marshal(vv)
	}
}

// b64u encodes bytes using base64url with no padding (matches spec
// "base64url-32B" notation).
var b64u = base64.RawURLEncoding

func b64uEncode(b []byte) string {
	if b == nil {
		return ""
	}
	return b64u.EncodeToString(b)
}

func b64uDecode(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	return b64u.DecodeString(s)
}

// canonicalBody implementations: each fills a sorted-key map and runs
// it through canonicalJSON.

func (f InviteFrame) canonicalBody() ([]byte, error) {
	return canonicalJSON(map[string]any{
		"type":            string(KindInvite),
		"v":               f.ProtocolVersion,
		"deviceId":        string(f.DeviceID),
		"ephemeralPubkey": b64uEncode(f.InitiatorPubkey),
		"deviceMetadata": map[string]any{
			"kind":        string(f.Kind_),
			"displayName": f.DisplayName,
		},
		"ts":    f.TS_,
		"nonce": b64uEncode(f.Nonce),
	})
}

func (f AcceptFrame) canonicalBody() ([]byte, error) {
	meta := map[string]any{
		"kind":        string(f.Kind_),
		"displayName": f.DisplayName,
	}
	body := map[string]any{
		"type":            string(KindAccept),
		"v":               f.ProtocolVersion,
		"deviceId":        string(f.DeviceID),
		"ephemeralPubkey": b64uEncode(f.ResponderPubkey),
		"deviceMetadata":  meta,
		"ts":              f.TS_,
		"nonce":           b64uEncode(f.Nonce),
	}
	if f.PushToken != "" {
		body["pushSubscription"] = map[string]any{
			"type":    "fcm",
			"payload": map[string]any{"token": f.PushToken},
		}
	}
	return canonicalJSON(body)
}

func (f SASConfirmFrame) canonicalBody() ([]byte, error) {
	body := map[string]any{
		"type": string(KindSASConfirm),
		"v":    f.ProtocolVersion,
		"ok":   f.OK,
		"role": string(f.Role),
		"ts":   f.TS_,
	}
	if f.OK {
		body["mac"] = b64uEncode(f.MAC)
	}
	return canonicalJSON(body)
}

func (f CompleteFrame) canonicalBody() ([]byte, error) {
	return canonicalJSON(map[string]any{
		"type": string(KindComplete),
		"v":    f.ProtocolVersion,
		"ts":   f.TS_,
	})
}

func (f AbortFrame) canonicalBody() ([]byte, error) {
	body := map[string]any{
		"type":   string(KindAbort),
		"v":      f.ProtocolVersion,
		"reason": string(f.Reason),
		"ts":     f.TS_,
	}
	if f.Detail != "" {
		body["detail"] = f.Detail
	}
	return canonicalJSON(body)
}

// Decode helpers — used by EnvelopeUnwrap. They round-trip through the
// generic map[string]any path so we tolerate field-order variation in
// what arrives over the wire.

func decodeInviteBody(b []byte) (InviteFrame, error) {
	var raw struct {
		V               int    `json:"v"`
		DeviceID        string `json:"deviceId"`
		EphemeralPubkey string `json:"ephemeralPubkey"`
		DeviceMetadata  struct {
			Kind        string `json:"kind"`
			DisplayName string `json:"displayName"`
		} `json:"deviceMetadata"`
		TS    int64  `json:"ts"`
		Nonce string `json:"nonce"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return InviteFrame{}, fmt.Errorf("pair: decode invite: %w", err)
	}
	pk, err := b64uDecode(raw.EphemeralPubkey)
	if err != nil {
		return InviteFrame{}, fmt.Errorf("pair: invite pubkey: %w", err)
	}
	nonce, err := b64uDecode(raw.Nonce)
	if err != nil {
		return InviteFrame{}, fmt.Errorf("pair: invite nonce: %w", err)
	}
	return InviteFrame{
		ProtocolVersion: raw.V,
		InitiatorPubkey: pk,
		DeviceID:        DeviceID(raw.DeviceID),
		Kind_:           DeviceKind(raw.DeviceMetadata.Kind),
		DisplayName:     raw.DeviceMetadata.DisplayName,
		TS_:             raw.TS,
		Nonce:           nonce,
	}, nil
}

func decodeAcceptBody(b []byte) (AcceptFrame, error) {
	var raw struct {
		V               int    `json:"v"`
		DeviceID        string `json:"deviceId"`
		EphemeralPubkey string `json:"ephemeralPubkey"`
		DeviceMetadata  struct {
			Kind        string `json:"kind"`
			DisplayName string `json:"displayName"`
		} `json:"deviceMetadata"`
		PushSubscription *struct {
			Type    string `json:"type"`
			Payload struct {
				Token string `json:"token"`
			} `json:"payload"`
		} `json:"pushSubscription"`
		TS    int64  `json:"ts"`
		Nonce string `json:"nonce"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return AcceptFrame{}, fmt.Errorf("pair: decode accept: %w", err)
	}
	pk, err := b64uDecode(raw.EphemeralPubkey)
	if err != nil {
		return AcceptFrame{}, fmt.Errorf("pair: accept pubkey: %w", err)
	}
	nonce, err := b64uDecode(raw.Nonce)
	if err != nil {
		return AcceptFrame{}, fmt.Errorf("pair: accept nonce: %w", err)
	}
	out := AcceptFrame{
		ProtocolVersion: raw.V,
		ResponderPubkey: pk,
		DeviceID:        DeviceID(raw.DeviceID),
		Kind_:           DeviceKind(raw.DeviceMetadata.Kind),
		DisplayName:     raw.DeviceMetadata.DisplayName,
		TS_:             raw.TS,
		Nonce:           nonce,
	}
	if raw.PushSubscription != nil {
		out.PushToken = raw.PushSubscription.Payload.Token
	}
	return out, nil
}

func decodeSASConfirmBody(b []byte) (SASConfirmFrame, error) {
	var raw struct {
		V    int    `json:"v"`
		OK   bool   `json:"ok"`
		Role string `json:"role"`
		MAC  string `json:"mac"`
		TS   int64  `json:"ts"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return SASConfirmFrame{}, fmt.Errorf("pair: decode sas-confirm: %w", err)
	}
	mac, err := b64uDecode(raw.MAC)
	if err != nil {
		return SASConfirmFrame{}, fmt.Errorf("pair: sas-confirm mac: %w", err)
	}
	return SASConfirmFrame{
		ProtocolVersion: raw.V,
		OK:              raw.OK,
		Role:            Role(raw.Role),
		MAC:             mac,
		TS_:             raw.TS,
	}, nil
}

func decodeCompleteBody(b []byte) (CompleteFrame, error) {
	var raw struct {
		V  int   `json:"v"`
		TS int64 `json:"ts"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return CompleteFrame{}, fmt.Errorf("pair: decode complete: %w", err)
	}
	return CompleteFrame{ProtocolVersion: raw.V, TS_: raw.TS}, nil
}

func decodeAbortBody(b []byte) (AbortFrame, error) {
	var raw struct {
		V      int    `json:"v"`
		Reason string `json:"reason"`
		Detail string `json:"detail"`
		TS     int64  `json:"ts"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return AbortFrame{}, fmt.Errorf("pair: decode abort: %w", err)
	}
	return AbortFrame{
		ProtocolVersion: raw.V,
		Reason:          AbortReason(raw.Reason),
		Detail:          raw.Detail,
		TS_:             raw.TS,
	}, nil
}
