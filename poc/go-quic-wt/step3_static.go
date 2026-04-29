//go:build step3

// Step 3: tiny static HTTP server on :8002 to serve the browser test page.
//
// Validates that Chromium's native WebTransport API can talk to our
// webtransport-go server (the v0.1 main path under D-13: Android Tauri
// Mobile uses Chromium WebView with native WT). Desktop Chrome and
// Android Chromium share the same WT implementation so testing in one
// validates the other.
//
// Run:
//   ./bin-step1                # terminal A (writes /tmp/tether-poc2-cert.hash)
//   ./bin-step3                # terminal B (serves :8002)
//   open http://127.0.0.1:8002 # PC browser
//   # OR phone Chrome → http://<PC-IP>:8002 (see README for secure-context caveat)
//
// Listens on 0.0.0.0:8002 so a phone on the same Wi-Fi can reach it.
//
// Endpoints:
//   GET /              → serves step3_browser/index.html
//   GET /<file>        → static files from step3_browser/
//   GET /cert-hash     → returns the live SHA-256 fingerprint hex written
//                        by step1_server (so HTML doesn't need editing)

package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

func mimeFor(path string) string {
	switch filepath.Ext(path) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "application/javascript; charset=utf-8"
	case ".md":
		return "text/markdown; charset=utf-8"
	case ".json":
		return "application/json"
	default:
		return "application/octet-stream"
	}
}

func main() {
	port := 8002
	if v := os.Getenv("STEP3_PORT"); v != "" {
		fmt.Sscanf(v, "%d", &port)
	}
	const dir = "step3_browser"

	if _, err := os.Stat(dir); err != nil {
		log.Fatalf("static dir %q not found (run from poc/go-quic-wt): %v", dir, err)
	}

	mux := http.NewServeMux()

	// Read & serve files directly. `http.ServeFile` does its own URL→path
	// redirect dance that triggers 301 on bare "/" — so we read the file
	// bytes ourselves and write them with the right MIME type. Cache-Control:
	// no-store so phone refresh always picks up the latest HTML.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path
		if name == "/" {
			name = "/index.html"
		}
		clean := filepath.Clean(name)
		if strings.Contains(clean, "..") || strings.HasPrefix(clean, "..") {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
		full := filepath.Join(dir, strings.TrimPrefix(clean, "/"))
		body, err := os.ReadFile(full)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", mimeFor(clean))
		w.Write(body)
	})

	serveHash := func(path string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			b, err := os.ReadFile(path)
			if err != nil {
				w.Header().Set("Cache-Control", "no-store")
				http.Error(w, fmt.Sprintf("cert hash not found at %s — start ./bin-step1 first", path), http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Write(b)
		}
	}
	mux.HandleFunc("/cert-hash", serveHash(hashFilePath))           // DER (W3C spec)
	mux.HandleFunc("/cert-hash-spki", serveHash(hashSPKIFilePath))  // SPKI (Chrome impl)

	addr := fmt.Sprintf(":%d", port)
	srv := &http.Server{Addr: addr, Handler: mux}

	fmt.Printf("✓ STEP 3 static server up\n")
	fmt.Printf("  http://0.0.0.0:%d/   (LAN access)\n", port)
	fmt.Printf("  http://127.0.0.1:%d/ (localhost)\n", port)
	fmt.Printf("  serving:     ./%s/\n", dir)
	fmt.Printf("  cert hash:   /cert-hash → %s\n", hashFilePath)
	fmt.Printf("  (Ctrl+C to stop)\n")

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errCh:
		log.Fatalf("server: %v", err)
	case <-sigCh:
		fmt.Println("\nshutting down…")
		_ = srv.Close()
	}
}
