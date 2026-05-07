package pair

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// TestSealOpenCompleteTag_RoundTrip verifies the AEAD seal/open round
// trip works for honest peers (BLOCKER-4 happy path).
func TestSealOpenCompleteTag_RoundTrip(t *testing.T) {
	ltk := bytes.Repeat([]byte{0xEE}, 32)
	thash := bytes.Repeat([]byte{0x11}, 32)
	nonce, tag, err := SealCompleteTag(ltk, thash, nil)
	if err != nil {
		t.Fatalf("SealCompleteTag: %v", err)
	}
	if len(nonce) != 24 {
		t.Errorf("nonce length: got %d want 24", len(nonce))
	}
	if len(tag) != 16 {
		t.Errorf("tag length: got %d want 16", len(tag))
	}
	if err := OpenCompleteTag(ltk, thash, nonce, tag); err != nil {
		t.Errorf("OpenCompleteTag honest: got %v want nil", err)
	}
}

// TestOpenCompleteTag_TamperDetected — flipping any byte in the tag,
// nonce, ltk, or transcript_hash trips ErrCompleteAEAD.
func TestOpenCompleteTag_TamperDetected(t *testing.T) {
	ltk := bytes.Repeat([]byte{0xEE}, 32)
	thash := bytes.Repeat([]byte{0x11}, 32)
	nonce, tag, err := SealCompleteTag(ltk, thash, nil)
	if err != nil {
		t.Fatalf("SealCompleteTag: %v", err)
	}

	// Tamper tag.
	bad := append([]byte(nil), tag...)
	bad[0] ^= 0x01
	if err := OpenCompleteTag(ltk, thash, nonce, bad); !errors.Is(err, ErrCompleteAEAD) {
		t.Errorf("tampered tag: got %v want ErrCompleteAEAD", err)
	}

	// Tamper nonce.
	badN := append([]byte(nil), nonce...)
	badN[0] ^= 0x01
	if err := OpenCompleteTag(ltk, thash, badN, tag); !errors.Is(err, ErrCompleteAEAD) {
		t.Errorf("tampered nonce: got %v want ErrCompleteAEAD", err)
	}

	// Wrong ltk.
	badLtk := append([]byte(nil), ltk...)
	badLtk[31] ^= 0x01
	if err := OpenCompleteTag(badLtk, thash, nonce, tag); !errors.Is(err, ErrCompleteAEAD) {
		t.Errorf("wrong ltk: got %v want ErrCompleteAEAD", err)
	}

	// Wrong transcript_hash.
	badTh := append([]byte(nil), thash...)
	badTh[15] ^= 0x01
	if err := OpenCompleteTag(ltk, badTh, nonce, tag); !errors.Is(err, ErrCompleteAEAD) {
		t.Errorf("wrong transcript_hash: got %v want ErrCompleteAEAD", err)
	}
}

// TestOpenCompleteTag_BadSizes — defense-in-depth: malformed sizes
// surface as ErrCompleteAEAD, not a different error class. Callers can
// uniformly map to pair.abort{cert-error}.
func TestOpenCompleteTag_BadSizes(t *testing.T) {
	ltk := bytes.Repeat([]byte{0xEE}, 32)
	thash := bytes.Repeat([]byte{0x11}, 32)
	if err := OpenCompleteTag(ltk[:31], thash, make([]byte, 24), make([]byte, 16)); !errors.Is(err, ErrCompleteAEAD) {
		t.Errorf("short ltk: got %v want ErrCompleteAEAD", err)
	}
	if err := OpenCompleteTag(ltk, thash, make([]byte, 23), make([]byte, 16)); !errors.Is(err, ErrCompleteAEAD) {
		t.Errorf("short nonce: got %v want ErrCompleteAEAD", err)
	}
	if err := OpenCompleteTag(ltk, thash, make([]byte, 24), make([]byte, 15)); !errors.Is(err, ErrCompleteAEAD) {
		t.Errorf("short tag: got %v want ErrCompleteAEAD", err)
	}
	if err := OpenCompleteTag(ltk, nil, make([]byte, 24), make([]byte, 16)); !errors.Is(err, ErrCompleteAEAD) {
		t.Errorf("empty thash: got %v want ErrCompleteAEAD", err)
	}
}

// TestServer_RogueComplete_Rejects — end-to-end test that a forged
// pair.complete (tag flipped after server emits it) is rejected by
// the initiator with cert-error.
//
// We intercept the responder→initiator stream; once we see pair.complete,
// flip a byte in the AEAD tag, forward the rest. Initiator (Client.Run)
// must surface ErrCompleteAEAD and emit pair.abort{cert-error} on its
// outbound side.
func TestServer_RogueComplete_Rejects(t *testing.T) {
	clientPipeR, serverPipeW := io.Pipe()       // server → mitm → client
	mitmPipeR, mitmPipeW := io.Pipe()           // mitm → client (post-tamper)
	clientToServerR, clientToServerW := io.Pipe()

	clientSide := rwPair{R: mitmPipeR, W: clientToServerW}
	serverSide := rwPair{R: clientToServerR, W: serverPipeW}

	// Tamper-relay: scan envelopes, when we hit KindComplete flip a tag byte.
	mitm := &completeTamper{
		scanner: bufio.NewScanner(clientPipeR),
		dst:     mitmPipeW,
	}
	mitm.scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	go func() {
		_ = mitm.pump()
		_ = mitmPipeW.Close()
	}()

	clk := &monotonicClock{base: time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC)}
	now := clk.Now
	clientRand := &detRand{fillByte: 0x10}
	serverRand := &detRand{fillByte: 0x80}

	client := NewClient(ClientConfig{
		Identity: Identity{
			DeviceID:    "device-desktop-rogue",
			Kind:        KindDesktop,
			DisplayName: "rogue test",
		},
		Confirmer: AutoConfirm,
		Now:       now,
		Rand:      clientRand,
	})
	server := NewServer(ServerConfig{
		Identity: Identity{
			DeviceID:    "device-mobile-rogue",
			Kind:        KindMobile,
			DisplayName: "rogue phone",
		},
		Confirmer: AutoConfirm,
		Now:       now,
		Rand:      serverRand,
	})

	type result struct {
		res Result
		err error
	}
	clientCh := make(chan result, 1)
	serverCh := make(chan result, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		res, err := client.Run(ctx, clientSide)
		_ = clientToServerW.Close()
		clientCh <- result{res, err}
	}()
	go func() {
		res, err := server.Run(ctx, serverSide)
		_ = serverPipeW.Close()
		// Close the client→server reader so the client's "best-effort
		// pair.abort{cert-error}" writeFrame doesn't deadlock against an
		// io.Pipe with no remaining reader. Without this, the
		// synchronous io.Pipe blocks the client goroutine forever.
		_ = clientToServerR.Close()
		serverCh <- result{res, err}
	}()

	cr := <-clientCh
	<-serverCh

	if cr.err == nil {
		t.Fatalf("expected client to fail on rogue complete; got nil err and result %+v", cr.res)
	}
	if !errors.Is(cr.err, ErrCompleteAEAD) {
		t.Errorf("client err: got %v want ErrCompleteAEAD", cr.err)
	}
	if !mitm.flipped {
		t.Errorf("MITM didn't observe a pair.complete to flip — test plumbing bug")
	}
}

// completeTamper relays envelopes server→client; when it sees
// KindComplete, flips a single byte in the base64-decoded `tag` field
// of the inner body, re-encodes, and forwards.
type completeTamper struct {
	mu      sync.Mutex
	scanner *bufio.Scanner
	dst     io.Writer
	flipped bool
}

func (c *completeTamper) pump() error {
	for c.scanner.Scan() {
		line := append([]byte(nil), c.scanner.Bytes()...)
		out := c.maybeFlip(line)
		if _, err := c.dst.Write(append(out, '\n')); err != nil {
			return err
		}
	}
	return c.scanner.Err()
}

func (c *completeTamper) maybeFlip(line []byte) []byte {
	env, err := decodeEnvelope(line)
	if err != nil {
		return line
	}
	if env.Kind != KindComplete {
		return line
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var raw map[string]any
	if err := json.Unmarshal(env.Ciphertext, &raw); err != nil {
		return line
	}
	tagS, ok := raw["tag"].(string)
	if !ok || tagS == "" {
		return line
	}
	tag, err := b64uDecode(tagS)
	if err != nil || len(tag) == 0 {
		return line
	}
	tag[0] ^= 0x01
	raw["tag"] = b64uEncode(tag)
	body, err := json.Marshal(raw)
	if err != nil {
		return line
	}
	env.Ciphertext = body
	out, err := encodeEnvelope(env)
	if err != nil {
		return line
	}
	c.flipped = true
	// encodeEnvelope already appends '\n'; strip it because pump adds.
	return bytes.TrimRight(out, "\n")
}
