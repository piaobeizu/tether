package wt

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"
)

// bootTestServer wires up a Server on a random loopback UDP port using
// the ServeListener entrypoint and returns the dialable URL plus an
// x509.CertPool the client can trust.
//
// Lifecycle: the returned cancel function shuts the server down + waits
// for Serve to return. Tests should always defer it.
func bootTestServer(t *testing.T) (url string, pool *x509.CertPool, cancel func()) {
	t.Helper()

	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	port := udp.LocalAddr().(*net.UDPAddr).Port

	ctx, cancelCtx := context.WithCancel(context.Background())

	srv, err := New(ctx, Config{
		Addr: fmt.Sprintf("127.0.0.1:%d", port),
		// Logger left nil → discard.
	})
	if err != nil {
		_ = udp.Close()
		cancelCtx()
		t.Fatalf("New: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.ServeListener(ctx, udp)
	}()

	// Build a client trust pool from the auto-generated cert.
	pool = x509.NewCertPool()
	leaf := srv.TLSCertificate().Leaf
	if leaf == nil {
		// Should never happen — generateDevCert sets Leaf — but be
		// defensive in case future Cert paths skip the parse.
		_ = srv.Close()
		<-done
		_ = udp.Close()
		cancelCtx()
		t.Fatalf("server cert leaf nil")
	}
	pool.AddCert(leaf)

	cancel = func() {
		cancelCtx()
		_ = srv.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Errorf("server Serve did not return within 3s after cancel")
		}
		_ = udp.Close()
	}

	return fmt.Sprintf("https://127.0.0.1:%d%s", port, EndpointPath), pool, cancel
}

// TestServer_EchoControlEnvelope is the integration smoke: boot a real
// WT server, dial it with the same lib's client, open one bidi
// stream, send a JSON envelope, read it back. This is the wire-level
// proof that slice #1 actually works end-to-end.
func TestServer_EchoControlEnvelope(t *testing.T) {
	t.Parallel()

	url, pool, cancel := bootTestServer(t)
	defer cancel()

	d := &webtransport.Dialer{
		TLSClientConfig: &tls.Config{
			RootCAs:    pool,
			NextProtos: []string{alpnH3},
			MinVersion: tls.VersionTLS13,
		},
		QUICConfig: &quic.Config{
			MaxIncomingStreams:               16,
			MaxIncomingUniStreams:            16,
			EnableDatagrams:                  true,
			EnableStreamResetPartialDelivery: true,
			KeepAlivePeriod:                  5 * time.Second,
		},
	}

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()

	rsp, sess, err := d.Dial(dialCtx, url, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if rsp.StatusCode != 200 {
		t.Fatalf("status: %d", rsp.StatusCode)
	}
	defer func() { _ = sess.CloseWithError(0, "test done") }()

	str, err := sess.OpenStreamSync(dialCtx)
	if err != nil {
		t.Fatalf("OpenStreamSync: %v", err)
	}

	type helloEnv struct {
		Hello string `json:"hello"`
		ID    int    `json:"id"`
	}
	send := helloEnv{Hello: "world", ID: 42}
	payload, err := json.Marshal(send)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if _, err := str.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Half-close so the server's io.Copy returns and we can read the
	// echoed bytes off the receive side.
	if err := str.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, err := io.ReadAll(str)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAll: %v", err)
	}

	var back helloEnv
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatalf("unmarshal echoed bytes %q: %v", got, err)
	}
	if back != send {
		t.Fatalf("echo mismatch: sent=%+v got=%+v", send, back)
	}
}

// TestServer_RejectsHalfCert ensures Cert without Key (or vice-versa)
// is rejected at construction.
func TestServer_RejectsHalfCert(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cfg  Config
	}{
		{"cert-without-key", Config{Cert: []byte("placeholder")}},
		{"key-without-cert", Config{Key: []byte("placeholder")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(context.Background(), tc.cfg)
			if err == nil {
				t.Fatal("want error, got nil")
			}
		})
	}
}

// TestServer_CloseIdempotent verifies Close can be called multiple
// times safely.
func TestServer_CloseIdempotent(t *testing.T) {
	t.Parallel()

	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	defer func() { _ = udp.Close() }()

	srv, err := New(context.Background(), Config{
		Addr: udp.LocalAddr().String(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = srv.ServeListener(ctx, udp)
	}()

	// Close twice — both must succeed without panicking.
	if err := srv.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	cancel()
	wg.Wait()
}
