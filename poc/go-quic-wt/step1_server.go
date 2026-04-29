//go:build step1

// Step 1: WebTransport-over-HTTP/3 echo server.
//
// Goal: confirm webtransport-go server-side basics work.
//
// Run:
//   cd poc/go-quic-wt && go run -tags step1 .
//
// Listens on UDP/4433 with self-signed cert. Mounts /wt endpoint for
// WebTransport upgrades and echoes back any bytes received on each stream.
// Also receives datagrams and echoes them back.
//
// PASS criteria: server prints "✓ STEP 1 listener up" + cert fingerprint.
// step2_client connects and verifies bidirectional echo.

package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

func main() {
	cert, hashes, err := generateCert()
	if err != nil {
		log.Fatalf("generateCert: %v", err)
	}

	mux := http.NewServeMux()
	h3 := &http3.Server{
		Addr: ":4433",
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			NextProtos:   []string{"h3"},
		},
		Handler:         mux,
		EnableDatagrams: true, // RFC 9297 (HTTP/3 datagrams)
		QUICConfig: &quic.Config{
			EnableDatagrams:                  true,
			EnableStreamResetPartialDelivery: true,
		},
	}
	// REQUIRED: sets AdditionalSettings[settingsEnableWebtransport]=1 +
	// EnableDatagrams=true on the H3 server. webtransport.Server does NOT
	// do this implicitly — discovered by PoC-2 step 1.
	webtransport.ConfigureHTTP3Server(h3)

	wt := &webtransport.Server{
		H3:          h3,
		CheckOrigin: func(*http.Request) bool { return true }, // PoC: allow any origin
	}

	mux.HandleFunc("/wt", func(w http.ResponseWriter, r *http.Request) {
		sess, err := wt.Upgrade(w, r)
		if err != nil {
			log.Printf("upgrade error: %v", err)
			return
		}
		log.Printf("→ session accepted from %s", r.RemoteAddr)
		go handleSession(sess)
	})

	fmt.Printf("✓ STEP 1 listener up\n")
	fmt.Printf("  endpoint:        https://127.0.0.1:4433/wt\n")
	fmt.Printf("  fingerprint DER: %s\n", formatFingerprint(hashes.DER))
	fmt.Printf("  fingerprint SPKI:%s\n", formatFingerprint(hashes.SPKI))

	// write hash file (DER) for step 3 static server's /cert-hash to consume,
	// and SPKI alternate at /cert-hash-spki — Chrome's WT API has historically
	// alternated between expecting DER vs SPKI hash.
	if err := writeCertHashFile(hashes.DER); err != nil {
		log.Printf("warn: write hash file: %v", err)
	} else {
		fmt.Printf("  hash files:      DER=/tmp/tether-poc2-cert.hash  SPKI=/tmp/tether-poc2-cert-spki.hash\n")
	}
	if err := writeCertHashFileTo("/tmp/tether-poc2-cert-spki.hash", hashes.SPKI); err != nil {
		log.Printf("warn: write spki hash file: %v", err)
	}
	fmt.Printf("  (Ctrl+C to stop)\n")

	// run server in goroutine so we can handle SIGINT cleanly
	errCh := make(chan error, 1)
	go func() { errCh <- wt.ListenAndServe() }()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errCh:
		log.Fatalf("server: %v", err)
	case <-sigCh:
		fmt.Println("\nshutting down…")
		_ = wt.Close()
	}
}

func handleSession(sess *webtransport.Session) {
	// echo bidi streams
	go func() {
		for {
			str, err := sess.AcceptStream(sess.Context())
			if err != nil {
				log.Printf("accept stream: %v", err)
				return
			}
			go func() {
				defer str.Close()
				n, err := io.Copy(str, str)
				log.Printf("  bidi stream echoed %d bytes (err=%v)", n, err)
			}()
		}
	}()
	// echo datagrams
	go func() {
		for {
			b, err := sess.ReceiveDatagram(sess.Context())
			if err != nil {
				log.Printf("recv datagram: %v", err)
				return
			}
			if err := sess.SendDatagram(b); err != nil {
				log.Printf("send datagram: %v", err)
				return
			}
			log.Printf("  datagram echoed %d bytes", len(b))
		}
	}()
}
