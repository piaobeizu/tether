//go:build step7

// Step 7: SINGLE BINARY architecture proof.
//
// Validates the simplification candidate for tether v0.1+: ONE Go binary
// embeds the static frontend (HTML/CSS/JS) via embed.FS AND exposes a
// WebTransport endpoint, all in the same process.
//
// Why this matters: the current spec (D-13) ships a separate Tauri
// native client per platform PLUS a Go server. step7 demonstrates that
// the entire deployment can collapse into ONE Go binary that:
//   - Embeds the React/HTML frontend via embed.FS
//   - Serves it over plain HTTP on :8002 (secure context via localhost)
//   - Runs HTTP/3 + WebTransport on :4433 (same cert, same process)
//
// User flow:
//   1. ./bin-step7
//   2. Browser → http://127.0.0.1:8082/
//   3. JS fetches /cert-hash (same origin, served by us)
//   4. JS opens WebTransport to https://127.0.0.1:4433/wt with pinned hash
//   5. Tests pass → architecture validated
//
// Run:
//   cd poc/go-quic-wt
//   go build -tags step7 -o bin-step7 .
//   ./bin-step7
//   open http://127.0.0.1:8082/
//
// PASS criteria: same as step3 (5 ✓ in browser harness) but with ONE binary.
// Reuses step3_browser/index.html unchanged (auto-fills WT host from page origin).

package main

import (
	"crypto/tls"
	_ "embed"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

//go:embed step3_browser/index.html
var step7IndexHTML []byte

func main() {
	cert, hashes, err := generateCert()
	if err != nil {
		log.Fatalf("generateCert: %v", err)
	}

	// JS expects 64-char raw hex (no colons) — formatFingerprint returns
	// the human-friendly colon form, used only for stdout printing.
	derFP := formatFingerprint(hashes.DER)
	spkiFP := formatFingerprint(hashes.SPKI)
	derHex := rawHex(hashes.DER)
	spkiHex := rawHex(hashes.SPKI)

	// --- Listener 1: plain HTTP for embedded static frontend on :8002 ---
	staticMux := http.NewServeMux()
	staticMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(step7IndexHTML)
	})
	staticMux.HandleFunc("/cert-hash", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, derHex)
	})
	staticMux.HandleFunc("/cert-hash-spki", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, spkiHex)
	})

	go func() {
		log.Println("[step7] static HTTP up on http://127.0.0.1:8082/")
		if err := http.ListenAndServe(":8082", staticMux); err != nil {
			log.Fatalf("static listen: %v", err)
		}
	}()

	// --- Listener 2: HTTP/3 + WebTransport on :4433 (same binary, same cert) ---
	wtMux := http.NewServeMux()

	h3 := &http3.Server{
		Addr: ":4433",
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			NextProtos:   []string{"h3"},
		},
		Handler:         wtMux,
		EnableDatagrams: true,
		QUICConfig: &quic.Config{
			EnableDatagrams:                  true,
			EnableStreamResetPartialDelivery: true,
		},
	}
	// REQUIRED: advertises SETTINGS_ENABLE_WEBTRANSPORT=1 in H3 SETTINGS.
	// webtransport.Server.ListenAndServe does NOT call this implicitly.
	// Without it the browser aborts the upgrade handshake.
	webtransport.ConfigureHTTP3Server(h3)

	wtServer := &webtransport.Server{
		H3:          h3,
		CheckOrigin: func(*http.Request) bool { return true },
	}

	wtMux.HandleFunc("/wt", func(w http.ResponseWriter, r *http.Request) {
		session, err := wtServer.Upgrade(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		go handleStep7Session(session)
	})

	fmt.Println("✓ STEP 7 single-binary listener up")
	fmt.Println("  static frontend  http://127.0.0.1:8082/   (embed.FS, no disk read)")
	fmt.Println("  WebTransport     https://127.0.0.1:4433/wt (HTTP/3, self-signed)")
	fmt.Println()
	fmt.Println("  cert fingerprint DER:  " + derFP)
	fmt.Println("  cert fingerprint SPKI: " + spkiFP)
	fmt.Println()
	fmt.Println("Architecture proof: ONE process serves both the page and the WT endpoint.")
	fmt.Println("Open the URL above in Chrome and click 'Run tests'.")

	log.Fatal(wtServer.ListenAndServe())
}

// rawHex encodes 32 bytes as 64-char lowercase hex (no separators).
// Matches the format step3 JS expects from /cert-hash and /cert-hash-spki.
func rawHex(fp [32]byte) string {
	const hex = "0123456789abcdef"
	out := make([]byte, 0, 64)
	for _, b := range fp {
		out = append(out, hex[b>>4], hex[b&0x0f])
	}
	return string(out)
}

func handleStep7Session(s *webtransport.Session) {
	log.Printf("[step7] WT session opened from %s", s.RemoteAddr())
	defer s.CloseWithError(0, "")

	// Bidirectional streams: pure byte echo (no prefix — browser test expects exact bytes)
	go func() {
		for {
			stream, err := s.AcceptStream(s.Context())
			if err != nil {
				return
			}
			go func() {
				defer stream.Close()
				buf, _ := io.ReadAll(stream)
				_, _ = stream.Write(buf)
			}()
		}
	}()

	// Datagrams: pure byte echo
	for {
		data, err := s.ReceiveDatagram(s.Context())
		if err != nil {
			return
		}
		_ = s.SendDatagram(data)
	}
}
