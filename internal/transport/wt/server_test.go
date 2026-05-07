package wt

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// bootTestServer wires up a Server on a random loopback UDP port using
// the ServeListener entrypoint and returns the dialable URL plus an
// x509.CertPool the client can trust.
//
// `handler` is the per-session handler the server runs for each
// accepted session; tests inject a channel-aware echo or whatever they
// need to drive the router. Pass nil for the default sit-on-session
// handler.
func bootTestServer(t *testing.T, handler func(*Session)) (url string, pool *x509.CertPool, cancel func()) {
	t.Helper()

	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	port := udp.LocalAddr().(*net.UDPAddr).Port

	ctx, cancelCtx := context.WithCancel(context.Background())

	srv, err := New(ctx, Config{
		Addr:           fmt.Sprintf("127.0.0.1:%d", port),
		SessionHandler: handler,
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

	pool = x509.NewCertPool()
	leaf := srv.TLSCertificate().Leaf
	if leaf == nil {
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

// dialClient is the test-side helper around Dial that wires the
// server's self-signed cert pool.
func dialClient(t *testing.T, ctx context.Context, url string, pool *x509.CertPool) *Client {
	t.Helper()
	c, err := Dial(ctx, ClientConfig{
		URL: url,
		TLSClientConfig: &tls.Config{
			RootCAs:    pool,
			NextProtos: []string{alpnH3},
			MinVersion: tls.VersionTLS13,
		},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	return c
}

// echoSessionHandler runs concurrent per-channel echo loops on the
// server side. Used by the round-trip tests to verify that bytes
// written into a given channel come back unchanged.
//
// Direction matrix per §3.3.3:
//
//	control     — client opens, server accepts → echo back
//	events      — server opens, client receives → server writes
//	                preset bytes, then echoes whatever client writes
//	agent-bytes — server opens uni → server writes preset bytes, closes
//	catch-up    — client opens, server accepts → echo back
//	datagram    — server echoes datagram payloads back with the tag
func echoSessionHandler(events []byte, agentBytes []byte) func(*Session) {
	return func(sess *Session) {
		ctx := sess.Context()
		var wg sync.WaitGroup

		// control: accept + echo (loop, but only one expected per session
		// in tests — use a single accept).
		wg.Add(1)
		go func() {
			defer wg.Done()
			str, err := sess.Control(ctx)
			if err != nil {
				return
			}
			defer str.Close()
			_, _ = io.Copy(str, str)
		}()

		// events: server opens, writes preset bytes, then echoes anything
		// the client writes back.
		wg.Add(1)
		go func() {
			defer wg.Done()
			openCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			str, err := sess.Events(openCtx)
			if err != nil {
				return
			}
			defer str.Close()
			if len(events) > 0 {
				_, _ = str.Write(events)
			}
			_, _ = io.Copy(str, str)
		}()

		// agent-bytes: server opens uni, writes preset bytes, closes.
		wg.Add(1)
		go func() {
			defer wg.Done()
			openCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			w, err := sess.OpenAgentBytes(openCtx)
			if err != nil {
				return
			}
			defer w.Close()
			if len(agentBytes) > 0 {
				_, _ = w.Write(agentBytes)
			}
		}()

		// catch-up: accept + echo.
		wg.Add(1)
		go func() {
			defer wg.Done()
			str, err := sess.AcceptCatchUp(ctx)
			if err != nil {
				return
			}
			defer str.Close()
			_, _ = io.Copy(str, str)
		}()

		// datagram: echo loop. Receives one, sends it back.
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				dgCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				payload, err := sess.RecvDatagram(dgCtx)
				cancel()
				if err != nil {
					return
				}
				_ = sess.SendDatagram(payload)
			}
		}()

		wg.Wait()
	}
}

// TestChannel_ControlRoundTrip — control channel: client opens, sends
// payload, server echoes back.
func TestChannel_ControlRoundTrip(t *testing.T) {
	t.Parallel()

	url, pool, cancel := bootTestServer(t, echoSessionHandler(nil, nil))
	defer cancel()

	ctx, ctxCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ctxCancel()

	cli := dialClient(t, ctx, url, pool)
	defer cli.Close()

	str, err := cli.OpenControl(ctx)
	if err != nil {
		t.Fatalf("OpenControl: %v", err)
	}
	payload := []byte("hello-control")
	if _, err := str.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := str.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got, err := io.ReadAll(str)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("control echo: got %q want %q", got, payload)
	}
}

// TestChannel_EventsRoundTrip — server opens events bidi, writes a
// preset payload + echoes whatever the client writes.
func TestChannel_EventsRoundTrip(t *testing.T) {
	t.Parallel()

	preset := []byte("server-events-preset")
	url, pool, cancel := bootTestServer(t, echoSessionHandler(preset, nil))
	defer cancel()

	ctx, ctxCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ctxCancel()

	cli := dialClient(t, ctx, url, pool)
	defer cli.Close()

	str, err := cli.AcceptEvents(ctx)
	if err != nil {
		t.Fatalf("AcceptEvents: %v", err)
	}

	// First read the server-pushed preset.
	got := make([]byte, len(preset))
	if _, err := io.ReadFull(str, got); err != nil {
		t.Fatalf("ReadFull preset: %v", err)
	}
	if string(got) != string(preset) {
		t.Fatalf("events preset: got %q want %q", got, preset)
	}

	// Now write something and verify echo.
	payload := []byte("client-events-roundtrip")
	if _, err := str.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := str.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	rest, err := io.ReadAll(str)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(rest) != string(payload) {
		t.Fatalf("events echo: got %q want %q", rest, payload)
	}
}

// TestChannel_AgentBytesUni — server pushes a uni-stream tagged
// agent-bytes; client receives and reads to EOF.
func TestChannel_AgentBytesUni(t *testing.T) {
	t.Parallel()

	preset := []byte("server-pty-bytes-stream")
	url, pool, cancel := bootTestServer(t, echoSessionHandler(nil, preset))
	defer cancel()

	ctx, ctxCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ctxCancel()

	cli := dialClient(t, ctx, url, pool)
	defer cli.Close()

	str, err := cli.AcceptAgentBytes(ctx)
	if err != nil {
		t.Fatalf("AcceptAgentBytes: %v", err)
	}
	got, err := io.ReadAll(str)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != string(preset) {
		t.Fatalf("agent-bytes: got %q want %q", got, preset)
	}
}

// TestChannel_CatchUpRoundTrip — client opens catch-up, server echoes.
func TestChannel_CatchUpRoundTrip(t *testing.T) {
	t.Parallel()

	url, pool, cancel := bootTestServer(t, echoSessionHandler(nil, nil))
	defer cancel()

	ctx, ctxCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ctxCancel()

	cli := dialClient(t, ctx, url, pool)
	defer cli.Close()

	str, err := cli.OpenCatchUp(ctx)
	if err != nil {
		t.Fatalf("OpenCatchUp: %v", err)
	}
	payload := []byte("catchup-replay-please")
	if _, err := str.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := str.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got, err := io.ReadAll(str)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("catch-up echo: got %q want %q", got, payload)
	}
}

// TestChannel_DatagramRoundTrip — datagram with channel-id prefix.
func TestChannel_DatagramRoundTrip(t *testing.T) {
	t.Parallel()

	url, pool, cancel := bootTestServer(t, echoSessionHandler(nil, nil))
	defer cancel()

	ctx, ctxCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ctxCancel()

	cli := dialClient(t, ctx, url, pool)
	defer cli.Close()

	payload := []byte("ping-12345")
	if err := cli.SendDatagram(payload); err != nil {
		t.Fatalf("SendDatagram: %v", err)
	}
	got, err := cli.RecvDatagram(ctx)
	if err != nil {
		t.Fatalf("RecvDatagram: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("datagram echo: got %q want %q", got, payload)
	}
}

// TestChannel_MixedConcurrent — open control + catch-up concurrently;
// receive events; verify the demux picked them apart correctly.
func TestChannel_MixedConcurrent(t *testing.T) {
	t.Parallel()

	preset := []byte("events-stream-mixed")
	url, pool, cancel := bootTestServer(t, echoSessionHandler(preset, nil))
	defer cancel()

	ctx, ctxCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer ctxCancel()

	cli := dialClient(t, ctx, url, pool)
	defer cli.Close()

	var wg sync.WaitGroup
	var ctrlGot, catchupGot, eventsGot []byte
	var ctrlErr, catchupErr, eventsErr error

	// control
	wg.Add(1)
	go func() {
		defer wg.Done()
		str, err := cli.OpenControl(ctx)
		if err != nil {
			ctrlErr = fmt.Errorf("OpenControl: %w", err)
			return
		}
		_, _ = str.Write([]byte("ctrl-mix"))
		_ = str.Close()
		ctrlGot, _ = io.ReadAll(str)
	}()

	// catch-up
	wg.Add(1)
	go func() {
		defer wg.Done()
		str, err := cli.OpenCatchUp(ctx)
		if err != nil {
			catchupErr = fmt.Errorf("OpenCatchUp: %w", err)
			return
		}
		_, _ = str.Write([]byte("catch-mix"))
		_ = str.Close()
		catchupGot, _ = io.ReadAll(str)
	}()

	// events (server-initiated)
	wg.Add(1)
	go func() {
		defer wg.Done()
		str, err := cli.AcceptEvents(ctx)
		if err != nil {
			eventsErr = fmt.Errorf("AcceptEvents: %w", err)
			return
		}
		buf := make([]byte, len(preset))
		if _, err := io.ReadFull(str, buf); err != nil {
			eventsErr = fmt.Errorf("ReadFull events: %w", err)
			return
		}
		eventsGot = buf
	}()

	wg.Wait()

	if ctrlErr != nil {
		t.Errorf("control: %v", ctrlErr)
	}
	if catchupErr != nil {
		t.Errorf("catch-up: %v", catchupErr)
	}
	if eventsErr != nil {
		t.Errorf("events: %v", eventsErr)
	}
	if string(ctrlGot) != "ctrl-mix" {
		t.Errorf("control mixed echo: got %q", ctrlGot)
	}
	if string(catchupGot) != "catch-mix" {
		t.Errorf("catch-up mixed echo: got %q", catchupGot)
	}
	if string(eventsGot) != string(preset) {
		t.Errorf("events mixed preset: got %q", eventsGot)
	}
}

// TestChannel_BadChannelID — open a stream with a bogus tag (0x06).
// Server must reset the offending stream but keep the session alive
// so a follow-up well-formed control stream still works.
func TestChannel_BadChannelID(t *testing.T) {
	t.Parallel()

	var sessionLive atomic.Bool
	handler := func(sess *Session) {
		sessionLive.Store(true)
		// Run the standard control echo so the follow-up stream proves
		// the session is still healthy.
		ctx := sess.Context()
		for {
			str, err := sess.Control(ctx)
			if err != nil {
				return
			}
			go func(s io.ReadWriteCloser) {
				defer s.Close()
				_, _ = io.Copy(s, s)
			}(str)
		}
	}
	url, pool, cancel := bootTestServer(t, handler)
	defer cancel()

	ctx, ctxCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ctxCancel()

	cli := dialClient(t, ctx, url, pool)
	defer cli.Close()

	// Open a stream with a bad tag (0x06 — not a known channel).
	bad, err := cli.OpenRawTagged(ctx, 0x06)
	if err != nil {
		t.Fatalf("OpenRawTagged: %v", err)
	}
	// Write some payload too — the server should reset before reading
	// it. We just want the read to fail (eventually).
	_, _ = bad.Write([]byte("garbage"))
	_ = bad.Close()
	// The read on the offending stream should error out (not echo).
	got, err := io.ReadAll(bad)
	if err == nil && len(got) > 0 {
		t.Errorf("bad-id stream returned data %q (want reset)", got)
	}

	// Session must still be healthy: open a real control and round-trip.
	good, err := cli.OpenControl(ctx)
	if err != nil {
		t.Fatalf("post-bad OpenControl: %v", err)
	}
	if _, err := good.Write([]byte("still-alive")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	_ = good.Close()
	echoed, err := io.ReadAll(good)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(echoed) != "still-alive" {
		t.Fatalf("post-bad control echo: got %q", echoed)
	}
	if !sessionLive.Load() {
		t.Errorf("session handler never ran")
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

	if err := srv.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	cancel()
	wg.Wait()
}
