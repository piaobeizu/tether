package pair

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"
)

// duplexPipe wraps two io.Pipe pairs into a bidirectional in-memory
// stream. clientSide is what the Client.Run sees; serverSide is what
// Server.Run sees; bytes the client writes are read by the server and
// vice versa.
type duplexPipe struct {
	clientReader *io.PipeReader
	clientWriter *io.PipeWriter
	serverReader *io.PipeReader
	serverWriter *io.PipeWriter
}

func newDuplexPipe() *duplexPipe {
	cr, sw := io.Pipe() // client reads what server writes
	sr, cw := io.Pipe() // server reads what client writes
	return &duplexPipe{
		clientReader: cr,
		clientWriter: cw,
		serverReader: sr,
		serverWriter: sw,
	}
}

func (d *duplexPipe) ClientSide() io.ReadWriter {
	return rwPair{R: d.clientReader, W: d.clientWriter}
}

func (d *duplexPipe) ServerSide() io.ReadWriter {
	return rwPair{R: d.serverReader, W: d.serverWriter}
}

func (d *duplexPipe) Close() {
	_ = d.clientReader.Close()
	_ = d.clientWriter.Close()
	_ = d.serverReader.Close()
	_ = d.serverWriter.Close()
}

type rwPair struct {
	R io.Reader
	W io.Writer
}

func (p rwPair) Read(b []byte) (int, error)  { return p.R.Read(b) }
func (p rwPair) Write(b []byte) (int, error) { return p.W.Write(b) }

// monotonicClock returns a strictly-monotonic time on every Now call,
// stepping by 1ms. Used by tests so frame ts values never tie even
// when several frames are produced within one wall-clock millisecond.
type monotonicClock struct {
	mu   sync.Mutex
	base time.Time
	step int64
}

func (c *monotonicClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.step++
	return c.base.Add(time.Duration(c.step) * time.Millisecond)
}

// detRand is a deterministic byte source — an alternating fill for
// tests so client + server pubkeys / nonces don't collide. Each call
// to Read fills with byte fillByte and increments.
type detRand struct {
	mu       sync.Mutex
	fillByte byte
}

func (d *detRand) Read(b []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := range b {
		b[i] = d.fillByte
	}
	d.fillByte++
	return len(b), nil
}

// TestClientServer_HappyPath runs initiator + responder against an
// in-memory duplex pipe and verifies both sides reach `paired` with
// matching long-term keys.
func TestClientServer_HappyPath(t *testing.T) {
	pipe := newDuplexPipe()
	defer pipe.Close()

	clientRand := &detRand{fillByte: 0x10}
	serverRand := &detRand{fillByte: 0x80}
	// Shared monotonic clock — every call advances by 1ms. This keeps
	// each frame's ts strictly monotonic without depending on wall-clock
	// resolution (test machines may run multiple Step calls inside one
	// millisecond).
	clk := &monotonicClock{base: time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC)}
	now := clk.Now

	client := NewClient(ClientConfig{
		Identity: Identity{
			DeviceID:    "device-desktop-aaa1",
			Kind:        KindDesktop,
			DisplayName: "Test Desktop",
		},
		Confirmer: AutoConfirm,
		Now:       now,
		Rand:      clientRand,
	})
	server := NewServer(ServerConfig{
		Identity: Identity{
			DeviceID:    "device-mobile-bbb2",
			Kind:        KindMobile,
			DisplayName: "Test Phone",
			PushToken:   "fcm-token-test",
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
		res, err := client.Run(ctx, pipe.ClientSide())
		// Close client's outbound pipe so server reads see EOF when
		// we exit (defensive — happy path doesn't trigger this).
		_ = pipe.clientWriter.Close()
		clientCh <- result{res, err}
	}()
	go func() {
		res, err := server.Run(ctx, pipe.ServerSide())
		// Close server's outbound pipe so client reads see EOF.
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

	if !bytes.Equal(cr.res.LongTermKey, sr.res.LongTermKey) {
		t.Errorf("long-term keys diverge:\n client: %x\n server: %x",
			cr.res.LongTermKey, sr.res.LongTermKey)
	}
	if !bytes.Equal(cr.res.TransportBindingKey, sr.res.TransportBindingKey) {
		t.Errorf("transport binding keys diverge")
	}
	if !bytes.Equal(cr.res.TranscriptHash, sr.res.TranscriptHash) {
		t.Errorf("transcript hashes diverge:\n client: %x\n server: %x",
			cr.res.TranscriptHash, sr.res.TranscriptHash)
	}
	if cr.res.SAS != sr.res.SAS {
		t.Errorf("SAS values diverge: client=%q server=%q", cr.res.SAS, sr.res.SAS)
	}
	if cr.res.SAS == "" {
		t.Errorf("empty SAS")
	}
	if sr.res.PeerName != "Test Desktop" {
		t.Errorf("server PeerName: got %q want %q", sr.res.PeerName, "Test Desktop")
	}
	if cr.res.PeerPushToken != "fcm-token-test" {
		t.Errorf("client PeerPushToken: got %q want fcm-token-test", cr.res.PeerPushToken)
	}
}
