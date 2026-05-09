package wt

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestSealOpenRoundtrip — happy path: seal a payload then open with
// matching device IDs / session id / shared key, recover the original
// plaintext. Confirms the AD construction is symmetric across Seal /
// Open in this process.
func TestSealOpenRoundtrip(t *testing.T) {
	t.Parallel()
	plaintext := []byte(`{"kind":"output.agent-event","sessionId":"sess-A","providerType":"claude-code"}`)
	env, err := Seal(SealOptions{
		SharedKey:    DevSharedKey[:],
		FromDeviceID: "device-cli-1",
		ToDeviceID:   "device-app-2",
		Kind:         "output.agent-event",
		Plaintext:    plaintext,
		SessionID:    "sess-A",
	})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if env.ID == "" || len(env.ID) != 36 {
		t.Errorf("ID looks bogus: %q", env.ID)
	}
	if env.KeyVersion != CurrentKeyVersion {
		t.Errorf("KeyVersion=%d want %d", env.KeyVersion, CurrentKeyVersion)
	}
	if len(env.Nonce) != NonceSize {
		t.Errorf("Nonce len=%d want %d", len(env.Nonce), NonceSize)
	}
	if env.Kind != "output.agent-event" {
		t.Errorf("Kind=%q want output.agent-event", env.Kind)
	}
	if len(env.Ciphertext) <= len(plaintext) {
		t.Errorf("Ciphertext len %d not greater than plaintext %d (missing tag?)", len(env.Ciphertext), len(plaintext))
	}

	got, err := Open(env, OpenOptions{SharedKey: DevSharedKey[:], SessionID: "sess-A"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("plaintext mismatch:\n got %q\nwant %q", got, plaintext)
	}
}

// TestSealOpen_ADBindsKind — flipping `Kind` between Seal and Open
// MUST cause AEAD auth failure. This is the §3.3.1 "kind is AD-bound"
// invariant — protects against a malicious server rewriting kind to
// reroute traffic.
func TestSealOpen_ADBindsKind(t *testing.T) {
	t.Parallel()
	env, err := Seal(SealOptions{
		SharedKey:    DevSharedKey[:],
		FromDeviceID: "a",
		ToDeviceID:   "b",
		Kind:         "output.agent-event",
		Plaintext:    []byte("payload"),
		SessionID:    "sess",
	})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// Tamper: rewrite kind to a different op category.
	env.Kind = "control.lock-takeover"
	_, err = Open(env, OpenOptions{SharedKey: DevSharedKey[:], SessionID: "sess"})
	if err == nil {
		t.Fatal("Open should have failed after Kind tamper")
	}
	if !errors.Is(err, ErrWireEnvelope) {
		t.Errorf("expected ErrWireEnvelope, got %v", err)
	}
}

// TestSealOpen_ADBindsDeviceIDs — flipping fromDeviceId or toDeviceId
// MUST cause AEAD auth failure. Stops a server from rerouting an
// envelope to a different device.
func TestSealOpen_ADBindsDeviceIDs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*WireEnvelope)
	}{
		{"flip-from", func(e *WireEnvelope) { e.FromDeviceID = "evil-from" }},
		{"flip-to", func(e *WireEnvelope) { e.ToDeviceID = "evil-to" }},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, err := Seal(SealOptions{
				SharedKey:    DevSharedKey[:],
				FromDeviceID: "a",
				ToDeviceID:   "b",
				Kind:         "output.agent-event",
				Plaintext:    []byte("payload"),
				SessionID:    "sess",
			})
			if err != nil {
				t.Fatalf("Seal: %v", err)
			}
			tc.mutate(env)
			if _, err := Open(env, OpenOptions{SharedKey: DevSharedKey[:], SessionID: "sess"}); err == nil {
				t.Fatal("Open should have failed after device-id tamper")
			}
		})
	}
}

// TestSealOpen_ADBindsSessionID — open with the wrong sessionId in
// AD MUST fail. Blocks a stolen-ciphertext-from-session-A replay
// into session B even with the same shared key.
func TestSealOpen_ADBindsSessionID(t *testing.T) {
	t.Parallel()
	env, err := Seal(SealOptions{
		SharedKey:    DevSharedKey[:],
		FromDeviceID: "a",
		ToDeviceID:   "b",
		Kind:         "output.agent-event",
		Plaintext:    []byte("payload"),
		SessionID:    "sess-A",
	})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := Open(env, OpenOptions{SharedKey: DevSharedKey[:], SessionID: "sess-B"}); err == nil {
		t.Fatal("Open with wrong sessionId should have failed")
	}
}

// TestOpen_RejectsKeyVersionMismatch — explicit reject for v0.1's
// keyVersion-must-be-1 invariant.
func TestOpen_RejectsKeyVersionMismatch(t *testing.T) {
	t.Parallel()
	env, err := Seal(SealOptions{
		SharedKey:    DevSharedKey[:],
		FromDeviceID: "a",
		ToDeviceID:   "b",
		Kind:         "output.agent-event",
		Plaintext:    []byte("p"),
	})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	env.KeyVersion = 2
	_, err = Open(env, OpenOptions{SharedKey: DevSharedKey[:]})
	if err == nil || !strings.Contains(err.Error(), "keyVersion") {
		t.Fatalf("expected keyVersion-mismatch error, got %v", err)
	}
}

// TestSeal_NonceUniqueness — over many calls with the same key the
// nonce MUST be drawn from a 24-byte random space; collisions in 100
// calls would be vanishingly unlikely. This is a smoke check rather
// than a statistical guarantee, but a non-random / counter-based bug
// would surface as collisions inside this loop.
func TestSeal_NonceUniqueness(t *testing.T) {
	t.Parallel()
	seen := make(map[string]struct{}, 100)
	for i := 0; i < 100; i++ {
		env, err := Seal(SealOptions{
			SharedKey:    DevSharedKey[:],
			FromDeviceID: "a",
			ToDeviceID:   "b",
			Kind:         "output.agent-event",
			Plaintext:    []byte("p"),
		})
		if err != nil {
			t.Fatalf("Seal[%d]: %v", i, err)
		}
		k := string(env.Nonce)
		if _, dup := seen[k]; dup {
			t.Fatalf("nonce collision at i=%d", i)
		}
		seen[k] = struct{}{}
	}
}

// TestWriteFrame_ReadFrame_Roundtrip — length-prefixed framing
// roundtrip over an io.Pipe, decoupled from the real WT stream.
func TestWriteFrame_ReadFrame_Roundtrip(t *testing.T) {
	t.Parallel()
	in, err := Seal(SealOptions{
		SharedKey:    DevSharedKey[:],
		FromDeviceID: "a",
		ToDeviceID:   "b",
		Kind:         "output.agent-event",
		Plaintext:    []byte(`{"hello":"world"}`),
		SessionID:    "sess",
	})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	var buf bytes.Buffer
	n, err := WriteFrame(&buf, in)
	if err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	if n <= 0 {
		t.Fatalf("WriteFrame returned 0 bytes")
	}

	got, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	// JSON roundtrip equivalence: marshal both, compare bytes.
	a, _ := json.Marshal(in)
	b, _ := json.Marshal(got)
	if !bytes.Equal(a, b) {
		t.Fatalf("frame mismatch:\n in: %s\nout: %s", a, b)
	}
}

// TestReadFrame_RejectsOversizeLength — a malicious / buggy peer
// shipping a 4-byte length-prefix > FrameSizeMax must be rejected
// without allocating the buffer.
func TestReadFrame_RejectsOversizeLength(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	// Length-prefix = FrameSizeMax + 1.
	hdr := []byte{0, 0x10, 0, 1} // 0x100001 > 1 << 20 (which is 0x100000)
	buf.Write(hdr)
	_, err := ReadFrame(&buf)
	if err == nil {
		t.Fatal("ReadFrame should have rejected oversize length")
	}
	if !errors.Is(err, ErrWireEnvelope) {
		t.Errorf("expected ErrWireEnvelope, got %v", err)
	}
}

// TestReadFrame_EOFAtBoundary — an empty stream returns io.EOF cleanly.
func TestReadFrame_EOFAtBoundary(t *testing.T) {
	t.Parallel()
	_, err := ReadFrame(bytes.NewReader(nil))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF, got %v", err)
	}
}

// TestSeal_RejectsBadInputs — required-field validation.
func TestSeal_RejectsBadInputs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		opts SealOptions
		want string
	}{
		{
			name: "bad-key-len",
			opts: SealOptions{SharedKey: []byte("short"), FromDeviceID: "a", ToDeviceID: "b", Kind: "x", Plaintext: []byte("p")},
			want: "shared key",
		},
		{
			name: "missing-kind",
			opts: SealOptions{SharedKey: DevSharedKey[:], FromDeviceID: "a", ToDeviceID: "b", Kind: "", Plaintext: []byte("p")},
			want: "kind",
		},
		{
			name: "missing-from",
			opts: SealOptions{SharedKey: DevSharedKey[:], FromDeviceID: "", ToDeviceID: "b", Kind: "x", Plaintext: []byte("p")},
			want: "DeviceId",
		},
		{
			name: "missing-to",
			opts: SealOptions{SharedKey: DevSharedKey[:], FromDeviceID: "a", ToDeviceID: "", Kind: "x", Plaintext: []byte("p")},
			want: "DeviceId",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Seal(tc.opts)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want substring %q in error %q", tc.want, err.Error())
			}
		})
	}
}

// TestNewUUIDv4_Format — sanity-check the homemade UUID generator:
// canonical 8-4-4-4-12 form, version 4 + RFC 4122 variant bits.
func TestNewUUIDv4_Format(t *testing.T) {
	t.Parallel()
	for i := 0; i < 50; i++ {
		s, err := newUUIDv4()
		if err != nil {
			t.Fatalf("newUUIDv4[%d]: %v", i, err)
		}
		if len(s) != 36 {
			t.Fatalf("len=%d want 36 (%q)", len(s), s)
		}
		// Version nibble at index 14, variant nibble at index 19.
		if s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
			t.Fatalf("bad dashes: %q", s)
		}
		if s[14] != '4' {
			t.Fatalf("version nibble != 4: %q", s)
		}
		v := s[19]
		if v != '8' && v != '9' && v != 'a' && v != 'b' {
			t.Fatalf("variant nibble not RFC 4122: %q", s)
		}
	}
}
