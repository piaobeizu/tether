//go:build step2

// Step 2: Go CLI client using webtransport-go client mode.
//
// Validates the §11.T risk point: webtransport-go client mode is the
// CLI's transport in v0.1. We need to confirm Dial → OpenStream → bidi
// echo + Datagram echo all work end-to-end.
//
// Run (separate terminal from step1):
//   cd poc/go-quic-wt && go run -tags step1 .   # terminal A
//   cd poc/go-quic-wt && go run -tags step2 .   # terminal B
//
// PASS criteria: prints "✓ STEP 2 PASS" and exits 0.

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"
)

func main() {
	const (
		serverURL = "https://127.0.0.1:4433/wt"
		bidiMsg   = "hello-bidi"
		dgramMsg  = "hello-dgram"
	)

	d := &webtransport.Dialer{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, // PoC: cert pinning out of scope
			NextProtos:         []string{"h3"},
		},
		QUICConfig: &quic.Config{
			MaxIncomingStreams:               256,
			MaxIncomingUniStreams:            256,
			EnableDatagrams:                  true,
			EnableStreamResetPartialDelivery: true, // required by webtransport-go v0.10
			KeepAlivePeriod:                  15 * time.Second,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rsp, sess, err := d.Dial(ctx, serverURL, nil)
	if err != nil {
		fail("dial: %v", err)
	}
	if rsp.StatusCode != 200 {
		fail("unexpected status: %d", rsp.StatusCode)
	}
	fmt.Printf("→ session opened (status=%d, remote=%s)\n", rsp.StatusCode, sess.RemoteAddr())

	// (a) bidi stream echo
	str, err := sess.OpenStreamSync(ctx)
	if err != nil {
		fail("open stream: %v", err)
	}
	if _, err := str.Write([]byte(bidiMsg)); err != nil {
		fail("stream write: %v", err)
	}
	if err := str.Close(); err != nil {
		fail("stream close: %v", err)
	}
	got, err := io.ReadAll(str)
	if err != nil && err != io.EOF {
		fail("stream read: %v", err)
	}
	if !bytes.Equal(got, []byte(bidiMsg)) {
		fail("bidi mismatch: want %q got %q", bidiMsg, got)
	}
	fmt.Printf("✓ bidi stream echo: %q\n", got)

	// (b) datagram echo
	if err := sess.SendDatagram([]byte(dgramMsg)); err != nil {
		fail("send datagram: %v", err)
	}
	dctx, dcancel := context.WithTimeout(ctx, 3*time.Second)
	defer dcancel()
	dgot, err := sess.ReceiveDatagram(dctx)
	if err != nil {
		fail("recv datagram: %v", err)
	}
	if !bytes.Equal(dgot, []byte(dgramMsg)) {
		fail("dgram mismatch: want %q got %q", dgramMsg, dgot)
	}
	fmt.Printf("✓ datagram echo: %q\n", dgot)

	if err := sess.CloseWithError(0, "bye"); err != nil {
		log.Printf("close: %v", err)
	}
	fmt.Println("✓ STEP 2 PASS")
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "✗ STEP 2 FAIL: "+format+"\n", a...)
	os.Exit(1)
}
