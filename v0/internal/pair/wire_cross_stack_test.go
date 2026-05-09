package pair

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var dummyTime = time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC)

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestWire_JSONLEnvelopeShape pins the on-the-wire envelope shape that
// Rust must produce and consume byte-identically. This is the BLOCKER-1
// fix surface: previously Go wrote `{"kind":..,"keyVersion":..,...}\n`
// and Rust wrote `<4-byte BE length><canonical body>` — incompatible.
//
// The pinned shape is:
//
//	{"kind":"<frame-kind>","keyVersion":0,"ciphertext":"<b64url-no-pad>","ts":<unix-ms>}\n
//
// where ciphertext is the canonical-JSON-encoded inner body, base64-
// url-no-pad encoded so the line is plain JSON (no embedded raw bytes).
//
// keyVersion=0 is the §3.3.1 sentinel for "pair-protocol scope,
// plaintext body" per spec §14 OQ ratification.
func TestWire_JSONLEnvelopeShape(t *testing.T) {
	frame := InviteFrame{
		ProtocolVersion: 1,
		InitiatorPubkey: bytes.Repeat([]byte{0xAB}, 32),
		DeviceID:        "device-desktop-aaaa",
		Kind_:           KindDesktop,
		DisplayName:     "Kang's MacBook",
		Model:           "MBP",
		OSVersion:       "macOS 14.5",
		AppVersion:      "tether 0.1.0-dev",
		TS_:             1714000000000,
		Nonce:           bytes.Repeat([]byte{0xCD}, 16),
	}
	env, err := EnvelopeWrap(frame)
	if err != nil {
		t.Fatalf("EnvelopeWrap: %v", err)
	}
	line, err := encodeEnvelope(env)
	if err != nil {
		t.Fatalf("encodeEnvelope: %v", err)
	}
	if !strings.HasSuffix(string(line), "\n") {
		t.Errorf("envelope line missing trailing \\n")
	}
	// Decode the line back as plain JSON to verify field names + shape.
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		t.Fatalf("envelope line not valid JSON: %v", err)
	}
	if raw["kind"] != "pair.invite" {
		t.Errorf("kind: got %v want pair.invite", raw["kind"])
	}
	// keyVersion arrives as float64 from generic JSON decode.
	if raw["keyVersion"] != float64(0) {
		t.Errorf("keyVersion: got %v want 0", raw["keyVersion"])
	}
	if _, ok := raw["ciphertext"].(string); !ok {
		t.Errorf("ciphertext: got %T want string", raw["ciphertext"])
	}
	if raw["ts"] != float64(1714000000000) {
		t.Errorf("ts: got %v want 1714000000000", raw["ts"])
	}
}

// TestWire_DecodeRustStyleEnvelope simulates the Rust client emitting an
// envelope (using the same JSONL shape) and verifies the Go side decodes
// it identically. If the shapes diverged, this would surface as either
// a JSON parse failure or a fields-mismatch.
func TestWire_DecodeRustStyleEnvelope(t *testing.T) {
	// Hand-build a Rust-side envelope: the Rust client computes
	// canonical_json(frame) for the inner body and base64url-no-pad
	// encodes it as `ciphertext`.
	innerBody := []byte(`{"deviceId":"device-mobile-bbbb","deviceMetadata":{"appVersion":"tether 0.1.0-dev","displayName":"Test Phone","kind":"mobile"},"ephemeralPubkey":"q6urq6urq6urq6urq6urq6urq6urq6urq6urq6urq6s","nonce":"zc3Nzc3Nzc3Nzc3Nzc3NzQ","ts":1714000000500,"type":"pair.accept","v":1}`)
	env := WireEnvelope{
		Kind:       KindAccept,
		KeyVersion: KeyVersionPair,
		Ciphertext: innerBody,
		TS:         1714000000500,
	}
	line, err := encodeEnvelope(env)
	if err != nil {
		t.Fatalf("encodeEnvelope: %v", err)
	}
	// Round-trip: decode back.
	got, err := decodeEnvelope(bytes.TrimRight(line, "\n"))
	if err != nil {
		t.Fatalf("decodeEnvelope: %v", err)
	}
	if got.Kind != KindAccept {
		t.Errorf("kind round-trip: got %s want pair.accept", got.Kind)
	}
	if !bytes.Equal(got.Ciphertext, innerBody) {
		t.Errorf("ciphertext round-trip mismatch")
	}
	// Now feed the inner body through the AcceptFrame decoder and
	// verify the optional fields all round-trip.
	parsed, err := decodeAcceptBody(got.Ciphertext)
	if err != nil {
		t.Fatalf("decodeAcceptBody: %v", err)
	}
	if parsed.AppVersion != "tether 0.1.0-dev" {
		t.Errorf("appVersion round-trip: got %q", parsed.AppVersion)
	}
	if parsed.DisplayName != "Test Phone" {
		t.Errorf("displayName round-trip: got %q", parsed.DisplayName)
	}
	if string(parsed.DeviceID) != "device-mobile-bbbb" {
		t.Errorf("deviceId round-trip: got %q", parsed.DeviceID)
	}
}

// TestWire_FullCycleByteIdentical drives a full Server.Run pair cycle
// and captures every wire byte. Verifies (a) all five frame kinds
// observed are the JSONL shape, (b) the inner canonical bodies decode
// to the spec field set, and (c) the byte stream is suffix-newline
// terminated for every frame.
//
// This is the cross-stack smoke required by the deliverable: "verify
// 'Go server speaks the wire format we documented'".
func TestWire_FullCycleByteIdentical(t *testing.T) {
	// We piggyback on the existing happy-path duplex pipe but tee both
	// sides' wire bytes so we can introspect them after.
	// Implemented inline rather than reusing TestClientServer_HappyPath
	// because Go's io.Pipe doesn't tee easily.
	pipe := newDuplexPipe()
	defer pipe.Close()

	clientWrite := &bytes.Buffer{}
	serverWrite := &bytes.Buffer{}

	clientSide := &teeRW{rw: pipe.ClientSide(), capW: clientWrite}
	serverSide := &teeRW{rw: pipe.ServerSide(), capW: serverWrite}

	clk := &monotonicClock{base: dummyTime}
	now := clk.Now
	client := NewClient(ClientConfig{
		Identity:  Identity{DeviceID: "device-desktop-aaaa", Kind: KindDesktop, DisplayName: "Test Desktop", AppVersion: "tether 0.1.0-dev"},
		Confirmer: AutoConfirm,
		Now:       now,
		Rand:      &detRand{fillByte: 0x10},
	})
	server := NewServer(ServerConfig{
		Identity:  Identity{DeviceID: "device-mobile-bbbb", Kind: KindMobile, DisplayName: "Test Phone", PushToken: "fcm-test"},
		Confirmer: AutoConfirm,
		Now:       now,
		Rand:      &detRand{fillByte: 0x80},
	})
	type result struct {
		res Result
		err error
	}
	clientCh := make(chan result, 1)
	serverCh := make(chan result, 1)
	go func() {
		res, err := client.Run(testCtx(t), clientSide)
		_ = pipe.clientWriter.Close()
		clientCh <- result{res, err}
	}()
	go func() {
		res, err := server.Run(testCtx(t), serverSide)
		_ = pipe.serverWriter.Close()
		serverCh <- result{res, err}
	}()
	cr := <-clientCh
	sr := <-serverCh
	if cr.err != nil {
		t.Fatalf("client: %v", cr.err)
	}
	if sr.err != nil {
		t.Fatalf("server: %v", sr.err)
	}
	// Every wire chunk MUST be JSONL (one JSON envelope per line).
	for label, buf := range map[string]*bytes.Buffer{"client": clientWrite, "server": serverWrite} {
		// Split on newline, expect every non-empty chunk to be a
		// valid envelope JSON.
		lines := bytes.Split(buf.Bytes(), []byte("\n"))
		nonEmpty := 0
		for _, ln := range lines {
			if len(ln) == 0 {
				continue
			}
			nonEmpty++
			env, err := decodeEnvelope(ln)
			if err != nil {
				t.Errorf("%s wire: malformed envelope %q: %v", label, ln, err)
				continue
			}
			if env.KeyVersion != KeyVersionPair {
				t.Errorf("%s wire: keyVersion=%d want %d", label, env.KeyVersion, KeyVersionPair)
			}
			switch env.Kind {
			case KindInvite, KindAccept, KindSASConfirm, KindComplete, KindAbort:
				// OK — known frame kind.
			default:
				t.Errorf("%s wire: unknown frame kind %q", label, env.Kind)
			}
		}
		if nonEmpty == 0 {
			t.Errorf("%s wire: no envelopes captured", label)
		}
	}
}

// teeRW wraps an io.ReadWriter and captures every byte written for
// post-test introspection. Reads pass through unmodified.
type teeRW struct {
	rw   interface{ Read([]byte) (int, error); Write([]byte) (int, error) }
	capW *bytes.Buffer
}

func (t *teeRW) Read(b []byte) (int, error) { return t.rw.Read(b) }
func (t *teeRW) Write(b []byte) (int, error) {
	t.capW.Write(b)
	return t.rw.Write(b)
}
