//go:build step13

// Step 13: Single-port HTTP/3 mux PoC — TCP + UDP on the same port, one TLS cert.
//
// H3 finding (light chair review 2026-05-09):
//   step10 跑的是 TCP :8082 (静态) + UDP :4433 (HTTP/3 + WT) 双端口。
//   v2 §10.B 描述的是单 HTTP/3 listener 同时托管静态 + WT
//   (HTTP/2 over TCP fallback + HTTP/3 over UDP, Alt-Svc 协商)。
//   两套不一样，spec 假设没验证。
//
// What this step proves:
//   1. Linux 网络栈允许 TCP :PORT + UDP :PORT 同时 bind（不同协议族不冲突）。
//   2. 同一张 ECDSA P-256 self-signed cert 同时给 HTTP/2 (TCP) 和 HTTP/3 (UDP) 用。
//   3. TCP HTTP/2 响应头带 Alt-Svc: h3=":PORT"；浏览器学到 h3 endpoint 后下次请求自动 upgrade。
//   4. WebTransport handshake 走 UDP HTTP/3 + serverCertificateHashes，不依赖 CA 信任。
//   5. /cert-hash 在 TCP 和 UDP 两路都能提供，值一致。
//
// Architecture:
//
//   Single Go binary
//     ├─ TCP :4433  (HTTP/2 + HTTP/1.1 over TLS)
//     │   ├─ /                → static SPA (embed.FS)
//     │   ├─ /cert-hash       → 64-char hex (DER fingerprint)
//     │   ├─ /cert-hash-spki  → 64-char hex (SPKI fingerprint)
//     │   └─ Alt-Svc 头        → h3=":4433"; ma=86400
//     └─ UDP :4433  (HTTP/3 + WebTransport)
//         ├─ /cert-hash       → same value as TCP path
//         └─ /wt/echo         → WT bidi pure-byte echo
//
// PASS criteria:
//   ✓ TCP listener up on :4433
//   ✓ UDP listener up on :4433
//   ✓ HTTP/2 client GET https://127.0.0.1:4433/cert-hash → 64-char hex
//   ✓ HTTP/2 response carries Alt-Svc: h3=":4433"; ma=...
//   ✓ HTTP/3 client GET https://127.0.0.1:4433/cert-hash → same hex
//   ✓ WT client connects → bidi echo round-trip matches
//
// Run (automated tests, exits 0 on success):
//   cd poc/go-quic-wt && go build -tags step13 -o bin-step13 . && ./bin-step13
//
// Run (serve mode, hold listeners alive for browser visual):
//   ./bin-step13 -serve

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

//go:embed step13_browser/index.html
var step13IndexHTML []byte

const (
	step13Port    = ":4433"
	step13EchoMsg = "hello-from-step13"
)

// rawHex encodes 32 bytes as 64-char lowercase hex (no separators).
func step13RawHex(fp [32]byte) string {
	const hex = "0123456789abcdef"
	out := make([]byte, 0, 64)
	for _, b := range fp {
		out = append(out, hex[b>>4], hex[b&0x0f])
	}
	return string(out)
}

func main() {
	serve := flag.Bool("serve", false, "hold listeners after PASS for browser visual")
	flag.Parse()

	cert, hashes, err := generateCert()
	if err != nil {
		log.Fatalf("generateCert: %v", err)
	}
	derHex := step13RawHex(hashes.DER)
	spkiHex := step13RawHex(hashes.SPKI)

	mux := http.NewServeMux()
	registerStep13Routes(mux, derHex, spkiHex)

	// --- TCP listener on :4433 (HTTP/2 + HTTP/1.1 over TLS) ---
	tcpTLS := &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"h2", "http/1.1"},
	}
	tcpServer := &http.Server{
		Addr:      step13Port,
		Handler:   altSvcMiddleware(mux),
		TLSConfig: tcpTLS,
	}
	go func() {
		log.Printf("[step13] TCP HTTPS up on https://127.0.0.1%s/ (HTTP/2)", step13Port)
		if err := tcpServer.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			log.Fatalf("TCP listen: %v", err)
		}
	}()

	// --- UDP listener on :4433 (HTTP/3 + WT) ---
	h3 := &http3.Server{
		Addr: step13Port,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			NextProtos:   []string{"h3"},
		},
		Handler:         mux,
		EnableDatagrams: true,
		QUICConfig: &quic.Config{
			EnableDatagrams:                  true,
			EnableStreamResetPartialDelivery: true,
		},
	}
	webtransport.ConfigureHTTP3Server(h3)

	wtServer := &webtransport.Server{
		H3:          h3,
		CheckOrigin: func(*http.Request) bool { return true },
	}

	mux.HandleFunc("/wt/echo", func(w http.ResponseWriter, r *http.Request) {
		session, err := wtServer.Upgrade(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		go handleStep13Echo(session)
	})

	go func() {
		log.Printf("[step13] UDP HTTP/3 + WT up on https://127.0.0.1%s/ (h3)", step13Port)
		if err := wtServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("UDP listen: %v", err)
		}
	}()

	// Give listeners a moment to bind.
	time.Sleep(500 * time.Millisecond)

	fmt.Println("✓ STEP 13 listeners up")
	fmt.Printf("  TCP HTTPS (HTTP/2):  https://127.0.0.1%s/\n", step13Port)
	fmt.Printf("  UDP HTTP/3 + WT:     https://127.0.0.1%s/wt/echo\n", step13Port)
	fmt.Printf("  cert DER  hex: %s\n", derHex)
	fmt.Printf("  cert SPKI hex: %s\n", spkiHex)
	fmt.Println()

	// Run automated tests.
	if err := runStep13Tests(derHex); err != nil {
		fmt.Fprintf(os.Stderr, "✗ STEP 13 FAIL: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("✓ STEP 13 PASS (3/3 — TCP /cert-hash, Alt-Svc, UDP WT echo)")

	if *serve {
		fmt.Println()
		fmt.Println("Holding listeners alive (-serve). Open https://127.0.0.1:4433/ in Chrome.")
		fmt.Println("Self-signed cert: launch Chrome with --ignore-certificate-errors-spki-list=" + spkiHex)
		fmt.Println("    or `chrome://flags/#allow-insecure-localhost` (loopback only).")
		select {}
	}
}

func registerStep13Routes(mux *http.ServeMux, derHex, spkiHex string) {
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(step13IndexHTML)
	})
	mux.HandleFunc("/cert-hash", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, derHex)
	})
	mux.HandleFunc("/cert-hash-spki", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, spkiHex)
	})
}

// altSvcMiddleware wraps a handler so every TCP HTTPS response advertises HTTP/3 on the same port.
func altSvcMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Alt-Svc", `h3="`+step13Port+`"; ma=86400`)
		h.ServeHTTP(w, r)
	})
}

func handleStep13Echo(s *webtransport.Session) {
	defer s.CloseWithError(0, "")

	stream, err := s.AcceptStream(s.Context())
	if err != nil {
		log.Printf("[step13/echo] AcceptStream: %v", err)
		return
	}
	defer stream.Close()

	if _, err := io.Copy(stream, stream); err != nil && err != io.EOF {
		log.Printf("[step13/echo] copy: %v", err)
	}
}

func runStep13Tests(expectedDerHex string) error {
	// Test 1: TCP HTTP/2 GET /cert-hash + Alt-Svc header.
	if err := step13TestTCP(expectedDerHex); err != nil {
		return fmt.Errorf("TCP test: %w", err)
	}

	// Test 2: UDP HTTP/3 GET /cert-hash (validates that h3 server actually serves regular GET, not just WT upgrade).
	if err := step13TestHTTP3(expectedDerHex); err != nil {
		return fmt.Errorf("HTTP/3 test: %w", err)
	}

	// Test 3: WT bidi echo on UDP HTTP/3.
	if err := step13TestWT(); err != nil {
		return fmt.Errorf("WT test: %w", err)
	}

	return nil
}

func step13TestTCP(expectedHex string) error {
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
				NextProtos:         []string{"h2", "http/1.1"},
			},
			ForceAttemptHTTP2: true,
		},
		Timeout: 5 * time.Second,
	}
	resp, err := client.Get("https://127.0.0.1" + step13Port + "/cert-hash")
	if err != nil {
		return fmt.Errorf("GET: %w", err)
	}
	defer resp.Body.Close()

	if resp.ProtoMajor != 2 {
		return fmt.Errorf("expected HTTP/2 over TCP, got %s", resp.Proto)
	}

	altSvc := resp.Header.Get("Alt-Svc")
	if altSvc == "" {
		return fmt.Errorf("missing Alt-Svc header on HTTP/2 response")
	}
	if !bytes.Contains([]byte(altSvc), []byte(`h3="`+step13Port+`"`)) {
		return fmt.Errorf("Alt-Svc missing h3 advert: %q", altSvc)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	got := string(bytes.TrimSpace(body))
	if got != expectedHex {
		return fmt.Errorf("cert-hash mismatch: want %q got %q", expectedHex, got)
	}

	fmt.Printf("✓ TCP HTTP/2 /cert-hash = %s (Alt-Svc=%q)\n", got, altSvc)
	return nil
}

func step13TestHTTP3(expectedHex string) error {
	tr := &http3.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			NextProtos:         []string{"h3"},
		},
		QUICConfig: &quic.Config{
			EnableDatagrams: true,
		},
	}
	defer tr.Close()

	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := client.Get("https://127.0.0.1" + step13Port + "/cert-hash")
	if err != nil {
		return fmt.Errorf("GET: %w", err)
	}
	defer resp.Body.Close()

	if resp.ProtoMajor != 3 {
		return fmt.Errorf("expected HTTP/3 over UDP, got %s", resp.Proto)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	got := string(bytes.TrimSpace(body))
	if got != expectedHex {
		return fmt.Errorf("cert-hash mismatch: want %q got %q", expectedHex, got)
	}

	fmt.Printf("✓ UDP HTTP/3 /cert-hash = %s (same as TCP)\n", got)
	return nil
}

// step13PinnedVerifier verifies a peer cert by SHA-256 of its DER, mirroring
// browser serverCertificateHashes semantics. Validates that single-port mux
// works WITHOUT trusting the cert through a CA chain — the realistic prod
// path for self-signed dev / first-run.
func step13PinnedVerifier(expectedDerHash [32]byte) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return fmt.Errorf("no peer cert")
		}
		got := sha256.Sum256(rawCerts[0])
		if got != expectedDerHash {
			return fmt.Errorf("cert hash mismatch")
		}
		return nil
	}
}

func step13TestWT() error {
	cert, hashes, err := step13ReadCertHashes()
	if err != nil {
		return err
	}
	_ = cert

	d := &webtransport.Dialer{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify:    true,
			VerifyPeerCertificate: step13PinnedVerifier(hashes),
			NextProtos:            []string{"h3"},
		},
		QUICConfig: &quic.Config{
			MaxIncomingStreams:               256,
			EnableDatagrams:                  true,
			EnableStreamResetPartialDelivery: true,
			KeepAlivePeriod:                  15 * time.Second,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rsp, sess, err := d.Dial(ctx, "https://127.0.0.1"+step13Port+"/wt/echo", nil)
	if err != nil {
		return fmt.Errorf("WT dial: %w", err)
	}
	if rsp.StatusCode != 200 {
		return fmt.Errorf("WT status: %d", rsp.StatusCode)
	}

	stream, err := sess.OpenStreamSync(ctx)
	if err != nil {
		return fmt.Errorf("OpenStream: %w", err)
	}
	if _, err := stream.Write([]byte(step13EchoMsg)); err != nil {
		return fmt.Errorf("stream write: %w", err)
	}
	if err := stream.Close(); err != nil {
		return fmt.Errorf("stream close: %w", err)
	}
	got, err := io.ReadAll(stream)
	if err != nil && err != io.EOF {
		return fmt.Errorf("stream read: %w", err)
	}
	if !bytes.Equal(got, []byte(step13EchoMsg)) {
		return fmt.Errorf("WT echo mismatch: want %q got %q", step13EchoMsg, got)
	}

	_ = sess.CloseWithError(0, "bye")
	fmt.Printf("✓ UDP HTTP/3 WT /wt/echo round-trip = %q\n", got)
	return nil
}

// step13ReadCertHashes fetches /cert-hash over HTTP/3 and returns a 32-byte hash.
// Used to drive the WT pinned verifier without sharing process state with main().
func step13ReadCertHashes() (string, [32]byte, error) {
	tr := &http3.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			NextProtos:         []string{"h3"},
		},
	}
	defer tr.Close()
	client := &http.Client{Transport: tr, Timeout: 3 * time.Second}
	resp, err := client.Get("https://127.0.0.1" + step13Port + "/cert-hash")
	if err != nil {
		return "", [32]byte{}, fmt.Errorf("read cert-hash: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", [32]byte{}, err
	}
	hex := string(bytes.TrimSpace(body))
	var fp [32]byte
	if _, err := decodeHex64(hex, fp[:]); err != nil {
		return "", [32]byte{}, fmt.Errorf("decode hex: %w", err)
	}
	return hex, fp, nil
}

func decodeHex64(s string, dst []byte) (int, error) {
	if len(s) != 64 {
		return 0, fmt.Errorf("want 64 chars got %d", len(s))
	}
	for i := 0; i < 32; i++ {
		hi, ok1 := hexNibble(s[2*i])
		lo, ok2 := hexNibble(s[2*i+1])
		if !ok1 || !ok2 {
			return 0, fmt.Errorf("bad hex char at %d", 2*i)
		}
		dst[i] = (hi << 4) | lo
	}
	return 32, nil
}

func hexNibble(b byte) (byte, bool) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', true
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, true
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10, true
	}
	return 0, false
}
