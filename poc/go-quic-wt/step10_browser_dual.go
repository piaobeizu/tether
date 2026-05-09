//go:build step10

// Step 10: Browser ↔ Single Go Binary, dual WT channel end-to-end.
//
// Combines step7 (single-binary embed.FS) + step8 (cc dual-channel) and
// drives BOTH from the browser via WebTransport. Validates the v2 spec's
// full end-to-end happy path — what the simplified architecture actually
// looks like to the user.
//
// Architecture:
//
//   Browser
//     ├─ Chat panel: input → /wt/chat bidi stream
//     │   sends JSON line per user message
//     │   receives cc stream-json events as JSON lines
//     │   renders structured (text / tool_use / result)
//     └─ Shell panel: xterm.js → /wt/shell bidi stream
//         bidirectional bytes (xterm input ↔ PTY)
//
//   Single Go binary
//     ├─ :8082 plain HTTP → static index.html (embed.FS)
//     │              + /cert-hash + /cert-hash-spki
//     └─ :4433 HTTP/3 + WebTransport
//         ├─ /wt/chat  → spawn long-running cc stream-json subprocess
//         │             pipe stream ↔ cc stdin/stdout
//         └─ /wt/shell → spawn cc --resume <sid> in PTY
//                       pipe stream ↔ PTY bytes
//
// PASS criteria:
//   ✓ binary builds + starts both listeners
//   ✓ browser at http://127.0.0.1:8082/ loads page
//   ✓ chat panel: send "say AAA" → see "AAA" in chat log
//   ✓ chat panel: send second prompt — same cc process, fast reply
//   ✓ shell panel: xterm.js shows cc TUI welcome banner
//   ✓ shell panel: type /help, see help text
//   ✓ both panels work concurrently in same browser tab
//
// Run:
//   cd poc/go-quic-wt && go build -tags step10 -o bin-step10 . && ./bin-step10
//   open http://127.0.0.1:8082/

package main

import (
	"crypto/tls"
	_ "embed"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/creack/pty"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

// resolveClaudePath finds the cc binary by:
//  1. TETHER_CC_PATH env override (absolute path)
//  2. PATH lookup for "claude"
//  3. Common installer locations for non-PATH'd terminals (Mac/Linux)
//
// Falls back to "claude" (lets exec fail with the original PATH error).
func resolveClaudePath() string {
	if env := os.Getenv("TETHER_CC_PATH"); env != "" {
		return env
	}
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	for _, candidate := range []string{
		filepath.Join(home, ".local/bin/claude"),
		filepath.Join(home, ".claude/local/bin/claude"),
		filepath.Join(home, ".npm-global/bin/claude"),
		"/usr/local/bin/claude",
		"/opt/homebrew/bin/claude",
	} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return "claude"
}

//go:embed step10_browser/index.html
var step10IndexHTML []byte

//go:embed step10_browser/xterm.js
var step10XtermJS []byte

//go:embed step10_browser/xterm.css
var step10XtermCSS []byte

// rawHex encodes 32 bytes as 64-char lowercase hex (no separators).
// Local copy of the helper from step7 since build tags isolate them.
func step10RawHex(fp [32]byte) string {
	const hex = "0123456789abcdef"
	out := make([]byte, 0, 64)
	for _, b := range fp {
		out = append(out, hex[b>>4], hex[b&0x0f])
	}
	return string(out)
}

var claudePath string

func main() {
	claudePath = resolveClaudePath()

	cert, hashes, err := generateCert()
	if err != nil {
		log.Fatalf("generateCert: %v", err)
	}
	derFP := formatFingerprint(hashes.DER)
	spkiFP := formatFingerprint(hashes.SPKI)
	derHex := step10RawHex(hashes.DER)
	spkiHex := step10RawHex(hashes.SPKI)

	// --- Static HTTP listener on :8082 ---
	staticMux := http.NewServeMux()
	staticMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(step10IndexHTML)
	})
	staticMux.HandleFunc("/xterm.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(step10XtermJS)
	})
	staticMux.HandleFunc("/xterm.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(step10XtermCSS)
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
		log.Println("[step10] static HTTP up on http://127.0.0.1:8082/")
		if err := http.ListenAndServe(":8082", staticMux); err != nil {
			log.Fatalf("static listen: %v", err)
		}
	}()

	// --- WT listener on :4433 ---
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
	webtransport.ConfigureHTTP3Server(h3)

	wtServer := &webtransport.Server{
		H3:          h3,
		CheckOrigin: func(*http.Request) bool { return true },
	}

	// shared session id — first chat sets it, shell reuses it
	chatSessionID := make(chan string, 1)
	var sharedSID string

	wtMux.HandleFunc("/wt/chat", func(w http.ResponseWriter, r *http.Request) {
		session, err := wtServer.Upgrade(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		go handleStep10Chat(session, &sharedSID, chatSessionID)
	})
	wtMux.HandleFunc("/wt/shell", func(w http.ResponseWriter, r *http.Request) {
		session, err := wtServer.Upgrade(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		go handleStep10Shell(session, &sharedSID)
	})

	fmt.Println("✓ STEP 10 dual-channel listener up")
	fmt.Printf("  claude binary:  %s\n", claudePath)
	fmt.Println("  static:    http://127.0.0.1:8082/")
	fmt.Println("  WT chat:   https://127.0.0.1:4433/wt/chat")
	fmt.Println("  WT shell:  https://127.0.0.1:4433/wt/shell")
	fmt.Println()
	fmt.Println("  cert DER hash:  " + derFP)
	fmt.Println("  cert SPKI hash: " + spkiFP)
	fmt.Println()
	fmt.Println("Open http://127.0.0.1:8082/ in Chrome.")

	log.Fatal(wtServer.ListenAndServe())
}

// handleStep10Chat: 1 WT bidi stream <-> 1 long-running stream-json cc subprocess.
func handleStep10Chat(s *webtransport.Session, sharedSID *string, sidChan chan<- string) {
	log.Printf("[step10/chat] session opened from %s", s.RemoteAddr())
	defer s.CloseWithError(0, "")

	stream, err := s.AcceptStream(s.Context())
	if err != nil {
		log.Printf("[step10/chat] AcceptStream: %v", err)
		return
	}
	defer stream.Close()

	cmd := exec.CommandContext(s.Context(), claudePath,
		"--print", "--verbose",
		"--output-format", "stream-json",
		"--input-format", "stream-json")
	if os.Geteuid() == 0 {
		cmd.Env = append(os.Environ(), "IS_SANDBOX=1")
	} else {
		cmd.Env = os.Environ()
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Printf("[step10/chat] stdin: %v", err)
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("[step10/chat] stdout: %v", err)
		return
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		log.Printf("[step10/chat] start: %v", err)
		return
	}
	log.Printf("[step10/chat] cc PID: %d", cmd.Process.Pid)

	// Capture session_id from first event line we see and mirror to shared state.
	stdoutTee := newSidSniffer(stdout, func(sid string) {
		if *sharedSID == "" && sid != "" {
			*sharedSID = sid
			select {
			case sidChan <- sid:
			default:
			}
			log.Printf("[step10/chat] captured session_id: %s", sid)
		}
	})

	// Browser → cc stdin
	go func() {
		_, err := io.Copy(stdin, stream)
		if err != nil && err != io.EOF {
			log.Printf("[step10/chat] browser→cc: %v", err)
		}
		stdin.Close()
	}()
	// cc stdout → browser
	_, err = io.Copy(stream, stdoutTee)
	if err != nil && err != io.EOF {
		log.Printf("[step10/chat] cc→browser: %v", err)
	}

	_ = cmd.Wait()
	log.Printf("[step10/chat] session closed")
}

// handleStep10Shell: 1 WT bidi stream <-> 1 cc PTY subprocess.
func handleStep10Shell(s *webtransport.Session, sharedSID *string) {
	log.Printf("[step10/shell] session opened from %s", s.RemoteAddr())
	defer s.CloseWithError(0, "")

	stream, err := s.AcceptStream(s.Context())
	if err != nil {
		log.Printf("[step10/shell] AcceptStream: %v", err)
		return
	}
	defer stream.Close()

	args := []string{}
	if *sharedSID != "" {
		args = append(args, "--resume", *sharedSID)
	}
	cmd := exec.CommandContext(s.Context(), claudePath, args...)
	if os.Geteuid() == 0 {
		cmd.Env = append(os.Environ(), "IS_SANDBOX=1", "TERM=xterm-256color")
	} else {
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	}

	f, err := pty.Start(cmd)
	if err != nil {
		log.Printf("[step10/shell] pty start: %v", err)
		return
	}
	defer f.Close()
	log.Printf("[step10/shell] cc PTY PID: %d (resume=%s)", cmd.Process.Pid, *sharedSID)

	// Browser → PTY
	go func() {
		_, _ = io.Copy(f, stream)
	}()
	// PTY → browser
	_, _ = io.Copy(stream, f)

	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	log.Printf("[step10/shell] session closed")
}

// sidSniffer wraps an io.Reader and inspects each line for a session_id field
// without consuming the data. Calls cb(sid) the first time it spots one.
type sidSniffer struct {
	r   io.Reader
	cb  func(string)
	buf []byte
	hit bool
}

func newSidSniffer(r io.Reader, cb func(string)) *sidSniffer {
	return &sidSniffer{r: r, cb: cb}
}

func (s *sidSniffer) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if !s.hit && n > 0 {
		s.buf = append(s.buf, p[:n]...)
		// Only inspect first 8KB — session_id appears in first system/init event.
		if len(s.buf) > 8192 {
			s.buf = s.buf[len(s.buf)-8192:]
		}
		if sid := extractSessionID(s.buf); sid != "" {
			s.hit = true
			s.cb(sid)
			s.buf = nil
		}
	}
	return n, err
}

// extractSessionID does a cheap substring lookup for `"session_id":"<uuid>"`.
// Avoids JSON parsing on the hot path; only used to seed shared state for the
// shell channel's --resume.
func extractSessionID(b []byte) string {
	const key = `"session_id":"`
	idx := -1
	for i := 0; i+len(key) <= len(b); i++ {
		match := true
		for j := 0; j < len(key); j++ {
			if b[i+j] != key[j] {
				match = false
				break
			}
		}
		if match {
			idx = i + len(key)
			break
		}
	}
	if idx < 0 {
		return ""
	}
	end := idx
	for end < len(b) && b[end] != '"' {
		end++
	}
	if end >= len(b) {
		return ""
	}
	return string(b[idx:end])
}
