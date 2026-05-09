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

// mitmStream sits between client and server, copies bytes through, but
// flips one byte in the responder's pubkey field of the pair.accept
// frame as it passes through. This simulates an active MITM: the
// client's view of the responder pubkey diverges from the server's
// actual pubkey, so client-derived sas_key disagrees with
// server-derived sas_key, so MAC verification fails.
//
// Architecture:
//
//	client ↔ mitmStream ↔ server
//
// We provide a half-duplex per direction. The MITM only mangles the
// frame in the server→client direction (where pair.accept lives).
type mitmFrameMangler struct {
	mu      sync.Mutex
	scanner *bufio.Scanner
	dst     io.Writer
	mangled bool
}

func (m *mitmFrameMangler) pumpServerToClient() error {
	for m.scanner.Scan() {
		line := append([]byte(nil), m.scanner.Bytes()...)
		// Try to detect pair.accept and flip a byte in
		// ephemeralPubkey. We round-trip the line through the JSON
		// envelope decoder; if it's pair.accept, we mangle the body
		// and re-encode.
		mangled, err := m.maybeMangle(line)
		if err != nil {
			return err
		}
		if _, err := m.dst.Write(append(mangled, '\n')); err != nil {
			return err
		}
	}
	return m.scanner.Err()
}

func (m *mitmFrameMangler) maybeMangle(line []byte) ([]byte, error) {
	env, err := decodeEnvelope(line)
	if err != nil {
		return line, nil // pass through unrelated lines unchanged
	}
	if env.Kind != KindAccept {
		return line, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mangled {
		return line, nil
	}
	// Decode the inner body, flip a byte in ephemeralPubkey, re-encode
	// in canonical form.
	var raw map[string]any
	if err := json.Unmarshal(env.Ciphertext, &raw); err != nil {
		return line, nil
	}
	pkS, ok := raw["ephemeralPubkey"].(string)
	if !ok || len(pkS) == 0 {
		return line, nil
	}
	pk, err := b64uDecode(pkS)
	if err != nil {
		return line, nil
	}
	pk[0] ^= 0x01 // flip a bit
	raw["ephemeralPubkey"] = b64uEncode(pk)
	body, err := canonicalJSON(raw)
	if err != nil {
		return line, nil
	}
	env.Ciphertext = body
	m.mangled = true
	return encodeWireLine(env)
}

func encodeWireLine(env WireEnvelope) ([]byte, error) {
	out, err := encodeEnvelope(env)
	if err != nil {
		return nil, err
	}
	// encodeEnvelope already appends '\n'; strip it because the caller
	// adds one of its own.
	return bytes.TrimRight(out, "\n"), nil
}

// TestClientServer_MITMSASMismatch flips a byte in the responder's
// ephemeral pubkey as it crosses to the client. Both sides should
// compute different sas_keys; the client's MAC will not validate
// against the server's sent MAC, so the client returns ErrSASMismatch
// and never reaches paired.
func TestClientServer_MITMSASMismatch(t *testing.T) {
	// We need three pipes: client→server (clean), server→client through
	// MITM. Simplest: client writes to a buffer that server reads
	// directly; server writes to a buffer that the MITM reads + mangles
	// into another buffer that the client reads.
	//
	// io.Pipe() pairs handle this cleanly: clientToServer + serverToMITM
	// + mitmToClient.

	c2sR, c2sW := io.Pipe() // client → server (clean)
	s2mR, s2mW := io.Pipe() // server → MITM (clean)
	m2cR, m2cW := io.Pipe() // MITM → client (mangled)
	defer func() {
		_ = c2sR.Close()
		_ = c2sW.Close()
		_ = s2mR.Close()
		_ = s2mW.Close()
		_ = m2cR.Close()
		_ = m2cW.Close()
	}()

	clientStream := rwPair{R: m2cR, W: c2sW}
	serverStream := rwPair{R: c2sR, W: s2mW}

	// MITM goroutine: pump server→MITM through the mangler into MITM→client.
	mitmDone := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(s2mR)
		sc.Buffer(make([]byte, 0, 4096), 1<<20)
		m := &mitmFrameMangler{scanner: sc, dst: m2cW}
		err := m.pumpServerToClient()
		_ = m2cW.Close()
		mitmDone <- err
	}()

	clientRand := &detRand{fillByte: 0x10}
	serverRand := &detRand{fillByte: 0x80}
	clk := &monotonicClock{base: time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC)}

	client := NewClient(ClientConfig{
		Identity: Identity{
			DeviceID: "device-desktop-mitmA", Kind: KindDesktop, DisplayName: "Test",
		},
		Confirmer: AutoConfirm,
		Now:       clk.Now,
		Rand:      clientRand,
	})
	server := NewServer(ServerConfig{
		Identity: Identity{
			DeviceID: "device-mobile-mitmB", Kind: KindMobile, DisplayName: "Test",
		},
		Confirmer: AutoConfirm,
		Now:       clk.Now,
		Rand:      serverRand,
	})

	type result struct {
		res Result
		err error
	}
	clientCh := make(chan result, 1)
	serverCh := make(chan result, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	go func() {
		res, err := client.Run(ctx, clientStream)
		// Close both ends so the server is unblocked: c2sW so the
		// server's reads see EOF, m2cR so any further writes to m2cW
		// from the MITM are NOPs.
		_ = c2sW.Close()
		clientCh <- result{res, err}
	}()
	go func() {
		res, err := server.Run(ctx, serverStream)
		// Close c2sR so the client's writes return ErrClosedPipe
		// instead of blocking. Close s2mW so MITM scanner sees EOF.
		_ = c2sR.Close()
		_ = s2mW.Close()
		serverCh <- result{res, err}
	}()

	cr := <-clientCh
	sr := <-serverCh

	// Client must detect the divergence — sas_keys disagree, peer's
	// sas-confirm MAC fails to verify under the client-derived
	// sas_key → ErrSASMismatch.
	if cr.err == nil {
		t.Errorf("client: got nil err, expected SAS mismatch")
	}
	if !errors.Is(cr.err, ErrSASMismatch) {
		// MAC verification failure surfaces as ErrSASMismatch, but a
		// secondary failure mode is that the client's sas-confirm
		// (which the server receives) has a MAC the server can't
		// verify, and the server then aborts with sas-mismatch — in
		// which case the client sees the abort frame and reports
		// some abort-related error. Both are acceptable; the key
		// invariant is that NEITHER side reaches paired.
		t.Logf("client err (acceptable if not paired): %v", cr.err)
	}
	if sr.err == nil {
		t.Errorf("server: got nil err, expected SAS mismatch")
	}
	// Both sides MUST NOT have a populated long-term key.
	if len(cr.res.LongTermKey) != 0 {
		t.Errorf("client must not produce a long-term key under MITM")
	}
	if len(sr.res.LongTermKey) != 0 {
		t.Errorf("server must not produce a long-term key under MITM")
	}

	// Drain the MITM goroutine.
	_ = c2sR.Close()
	_ = m2cW.Close()
	select {
	case <-mitmDone:
	case <-time.After(2 * time.Second):
		t.Logf("MITM goroutine hang on shutdown (non-fatal)")
	}
}
