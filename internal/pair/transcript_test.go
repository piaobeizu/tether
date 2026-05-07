package pair

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestTranscript_OrderMatters — appending the same frames in different
// orders produces different hashes. (The whole point of running-hash
// transcript binding.)
func TestTranscript_OrderMatters(t *testing.T) {
	inv := mustInvite(1)
	acc := mustAccept(2)

	t1 := NewTranscript()
	if err := t1.Append(inv); err != nil {
		t.Fatalf("append inv: %v", err)
	}
	if err := t1.Append(acc); err != nil {
		t.Fatalf("append acc: %v", err)
	}
	h1 := t1.Hash()

	t2 := NewTranscript()
	if err := t2.Append(acc); err != nil {
		t.Fatalf("append acc: %v", err)
	}
	if err := t2.Append(inv); err != nil {
		t.Fatalf("append inv: %v", err)
	}
	h2 := t2.Hash()

	if bytes.Equal(h1, h2) {
		t.Errorf("transcript hash collided across orders: %x", h1)
	}
}

// TestTranscript_HashStableAcrossSnapshots — calling Hash() twice
// without appends must yield the same bytes.
func TestTranscript_HashStableAcrossSnapshots(t *testing.T) {
	tr := NewTranscript()
	_ = tr.Append(mustInvite(1))
	h1 := tr.Hash()
	h2 := tr.Hash()
	if !bytes.Equal(h1, h2) {
		t.Errorf("Hash() mutated state: %x vs %x", h1, h2)
	}
	// Now appending another frame changes the hash.
	_ = tr.Append(mustAccept(2))
	h3 := tr.Hash()
	if bytes.Equal(h1, h3) {
		t.Errorf("Hash() unchanged across Append")
	}
}

// TestCanonicalJSON_KeyOrderIndependent — the canonical encoding sorts
// keys. We feed two map[string]any with same content / different
// insertion orders and verify the byte output matches.
func TestCanonicalJSON_KeyOrderIndependent(t *testing.T) {
	// Go maps already randomize iteration; we just call canonicalJSON
	// twice with the "same" map (literal) and verify byte-equality.
	a := map[string]any{
		"z": "last",
		"a": "first",
		"m": map[string]any{"y": 1, "b": 2},
	}
	b := map[string]any{
		"a": "first",
		"m": map[string]any{"b": 2, "y": 1},
		"z": "last",
	}
	ja, err := canonicalJSON(a)
	if err != nil {
		t.Fatalf("canonicalJSON a: %v", err)
	}
	jb, err := canonicalJSON(b)
	if err != nil {
		t.Fatalf("canonicalJSON b: %v", err)
	}
	if !bytes.Equal(ja, jb) {
		t.Errorf("canonicalJSON not deterministic: %s vs %s", ja, jb)
	}
	// And it parses as valid JSON.
	var out any
	if err := json.Unmarshal(ja, &out); err != nil {
		t.Errorf("canonicalJSON not valid JSON: %v", err)
	}
}

// TestCanonicalBody_InviteFullFields pins the EXACT byte sequence
// produced by InviteFrame.canonicalBody() when all optional spec
// §3.1 deviceMetadata fields are populated. This is the BLOCKER-2
// cross-stack invariant: the Rust side
// (tether-app/src-tauri/src/wt/pair.rs::canonical_body_invite_full_fields_golden)
// MUST produce the same bytes for the same inputs. Divergence ⇒
// transcript_hash diverges ⇒ SAS / MAC mismatch ⇒ pairing fails.
//
// Why this exact ordering: canonicalJSON sorts keys lexicographically
// at every object level. The expected layout reflects:
//
//	deviceId, deviceMetadata{appVersion, displayName, kind, model, osVersion},
//	ephemeralPubkey, nonce, ts, type, v
//
// Note that "v" sorts AFTER "type" (lex byte order: 't' < 'v'), and
// inside deviceMetadata the optional fields all sort before the
// required ones since their first-letter byte values are smaller.
func TestCanonicalBody_InviteFullFields(t *testing.T) {
	// Fixed-byte deterministic inputs — these match the Rust pinning.
	pubkey := bytes.Repeat([]byte{0xAB}, 32)
	nonce := bytes.Repeat([]byte{0xCD}, 16)
	f := InviteFrame{
		ProtocolVersion: 1,
		InitiatorPubkey: pubkey,
		DeviceID:        "device-desktop-aaaa",
		Kind_:           KindDesktop,
		DisplayName:     "Kang's MacBook",
		Model:           "MBP",
		OSVersion:       "macOS 14.5",
		AppVersion:      "tether 0.1.0-dev",
		TS_:             1714000000000,
		Nonce:           nonce,
	}
	got, err := f.canonicalBody()
	if err != nil {
		t.Fatalf("canonicalBody: %v", err)
	}
	// Pinned: lex-sorted keys, no whitespace, base64url-no-pad for
	// pubkey + nonce. Construct as a literal so any drift trips here.
	const want = `{"deviceId":"device-desktop-aaaa","deviceMetadata":{"appVersion":"tether 0.1.0-dev","displayName":"Kang's MacBook","kind":"desktop","model":"MBP","osVersion":"macOS 14.5"},"ephemeralPubkey":"q6urq6urq6urq6urq6urq6urq6urq6urq6urq6urq6s","nonce":"zc3Nzc3Nzc3Nzc3Nzc3NzQ","ts":1714000000000,"type":"pair.invite","v":1}`
	if string(got) != want {
		t.Errorf("invite canonical body diverged from cross-stack golden:\n got: %s\nwant: %s", string(got), want)
	}

	// Round-trip through decode — guarantees all-fields-included survives.
	parsed, err := decodeInviteBody(got)
	if err != nil {
		t.Fatalf("decodeInviteBody: %v", err)
	}
	if parsed.Model != "MBP" || parsed.OSVersion != "macOS 14.5" || parsed.AppVersion != "tether 0.1.0-dev" {
		t.Errorf("optional fields lost on round-trip: %+v", parsed)
	}
}

// TestCanonicalBody_InviteOptionalsOmitted — when the optionals are
// empty strings, they MUST be absent from the canonical body (matching
// Rust's `Option::None` skip semantics). Otherwise an honest peer that
// happens to omit them produces a different transcript than one that
// passes empty strings, breaking interop.
func TestCanonicalBody_InviteOptionalsOmitted(t *testing.T) {
	pubkey := bytes.Repeat([]byte{0xAB}, 32)
	nonce := bytes.Repeat([]byte{0xCD}, 16)
	f := InviteFrame{
		ProtocolVersion: 1,
		InitiatorPubkey: pubkey,
		DeviceID:        "device-desktop-aaaa",
		Kind_:           KindDesktop,
		DisplayName:     "Kang's MacBook",
		// Model / OSVersion / AppVersion intentionally empty.
		TS_:   1714000000000,
		Nonce: nonce,
	}
	got, err := f.canonicalBody()
	if err != nil {
		t.Fatalf("canonicalBody: %v", err)
	}
	const want = `{"deviceId":"device-desktop-aaaa","deviceMetadata":{"displayName":"Kang's MacBook","kind":"desktop"},"ephemeralPubkey":"q6urq6urq6urq6urq6urq6urq6urq6urq6urq6urq6s","nonce":"zc3Nzc3Nzc3Nzc3Nzc3NzQ","ts":1714000000000,"type":"pair.invite","v":1}`
	if string(got) != want {
		t.Errorf("omit-optionals canonical body:\n got: %s\nwant: %s", string(got), want)
	}
}

// TestEnvelopeRoundtrip — encode + decode an envelope through
// EnvelopeWrap / EnvelopeUnwrap and verify field equality.
func TestEnvelopeRoundtrip(t *testing.T) {
	frames := []Frame{
		mustInvite(100),
		mustAccept(200),
		mustSASConfirm(RoleInitiator, 300),
		mustComplete(400),
		mustAbort(ReasonTimeout, 500),
	}
	for _, f := range frames {
		env, err := EnvelopeWrap(f)
		if err != nil {
			t.Errorf("EnvelopeWrap %s: %v", f.Kind(), err)
			continue
		}
		if env.KeyVersion != KeyVersionPair {
			t.Errorf("envelope keyVersion: got %d want %d", env.KeyVersion, KeyVersionPair)
		}
		got, err := EnvelopeUnwrap(env)
		if err != nil {
			t.Errorf("EnvelopeUnwrap %s: %v", f.Kind(), err)
			continue
		}
		if got.Kind() != f.Kind() || got.TS() != f.TS() {
			t.Errorf("roundtrip kind/ts: got (%s,%d) want (%s,%d)",
				got.Kind(), got.TS(), f.Kind(), f.TS())
		}
	}
}
