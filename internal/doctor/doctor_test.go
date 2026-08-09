package doctor

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/server"
)

// writeJSON writes v as JSON to path, creating parent dirs.
func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCheckMCPSettingsInject_Injected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude", "settings.json")
	writeJSON(t, path, map[string]any{
		"mcpServers": map[string]any{
			"tether": map[string]any{
				agent.TetherManagedKey: true,
				"type":                 "http",
				"url":                  "http://127.0.0.1:8899/mcp",
			},
		},
	})
	// Point home to temp dir by temporarily overriding UserHomeDir via env.
	t.Setenv("HOME", dir)
	r := checkMCPSettingsInject(&server.Config{}, false)
	if r.Status != StatusOK {
		t.Errorf("expected ok, got %s message=%q", r.Status, r.Message)
	}
}

func TestCheckMCPSettingsInject_Missing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	r := checkMCPSettingsInject(&server.Config{}, false)
	if !r.Failed() {
		t.Errorf("expected fail when settings.json absent, got %s message=%q", r.Status, r.Message)
	}
}

func TestCheckMCPSettingsInject_NotInjected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude", "settings.json")
	writeJSON(t, path, map[string]any{
		"mcpServers": map[string]any{
			"other": map[string]any{"url": "http://example.com"},
		},
	})
	t.Setenv("HOME", dir)
	r := checkMCPSettingsInject(&server.Config{}, false)
	if !r.Failed() {
		t.Errorf("expected fail when no tether entry, got %s message=%q", r.Status, r.Message)
	}
}

func TestCheckMCPAPITokens_NoFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	r := checkMCPAPITokens(false)
	if r.Failed() {
		t.Errorf("missing api-tokens.json should not be an error: %q", r.Message)
	}
}

func TestCheckMCPAPITokens_WithTokens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".tether", "api-tokens.json")
	writeJSON(t, path, map[string]any{
		"tokens": []map[string]any{
			{"id": "tok1", "name": "cursor", "hash": "abc"},
			{"id": "tok2", "name": "goose", "hash": "def"},
		},
	})
	t.Setenv("HOME", dir)
	r := checkMCPAPITokens(false)
	if r.Status != StatusOK {
		t.Errorf("expected ok, got %s: %q", r.Status, r.Message)
	}
	if r.Message != "2 external API token(s) configured" {
		t.Errorf("unexpected message: %q", r.Message)
	}
}

func TestCheckMCPAPITokens_EmptyStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".tether", "api-tokens.json")
	writeJSON(t, path, map[string]any{"tokens": []any{}})
	t.Setenv("HOME", dir)
	r := checkMCPAPITokens(false)
	if r.Failed() {
		t.Errorf("empty store should not be an error: %q", r.Message)
	}
}

func TestCheckMCPLoopback_NotRunning(t *testing.T) {
	// Point the check at a port nothing holds, rather than letting it fall back
	// to the default 8899: a developer box running a real tether daemon has
	// that port open, and this test would then assert the opposite of what it
	// is named for. (It passed either way before tether#84 — the check returned
	// OK=true whether or not it connected — so the dependency was invisible.)
	t.Setenv("HOME", t.TempDir())

	// Unreachable is a skip, not a pass and not a failure: doctor runs before
	// the server exists, so a refused connection asserts nothing about the
	// endpoint's health.
	r := checkMCPLoopback(&server.Config{MCPPort: closedPort(t)}, false)
	if r.Status != StatusSkip {
		t.Errorf("unreachable loopback should skip, got %s: %q", r.Status, r.Message)
	}
}

// closedPort returns a port that was bindable a moment ago and is now free.
// Not a guarantee — nothing can reserve a port without holding it — but the
// kernel does not hand the same ephemeral port straight back out.
func closedPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// startLoopback runs a real server.MCPLoopback — the production type, so the
// production bearer gate sits in front of the SDK's streamable handler exactly
// as it does in a daemon — and returns the port it listens on. A bare
// net.Listen would not do: the whole subject of tether#89 is the difference
// between a port being open and an MCP endpoint being this deployment's, and
// the test that used one could not see that difference either.
func startLoopback(t *testing.T, token string) int {
	t.Helper()
	port := closedPort(t)
	lb := server.NewMCPLoopback(port, mcp.NewServer(&mcp.Implementation{Name: "tether", Version: "test"}, nil), token)
	if err := lb.Start(); err != nil {
		t.Fatalf("start loopback on %d: %v", port, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = lb.Stop(ctx)
	})
	return port
}

// writeMCPToken plants the bearer token a running daemon leaves in
// ~/.tether/mcp-token.
func writeMCPToken(t *testing.T, home, token string) {
	t.Helper()
	dir := filepath.Join(home, ".tether")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mcp-token"), []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCheckMCPLoopback_OurOwnEndpointIsOK(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeMCPToken(t, home, "our-secret")
	port := startLoopback(t, "our-secret")

	r := checkMCPLoopback(&server.Config{MCPPort: port}, true)
	if r.Status != StatusOK {
		t.Fatalf("our own running loopback should be ok, got %s: %q", r.Status, r.Message)
	}
	// The handshake really happened: the message carries what initialize said,
	// which a TCP dial could not have produced.
	if !contains(r.Message, "tether test") {
		t.Errorf("expected serverInfo in message, got: %q", r.Message)
	}
}

// This is the tether#89 bug, stated as a test: another tether daemon holding
// the port must not be reported as this deployment's healthy endpoint. The
// pre-fix check dialled and returned ok here.
func TestCheckMCPLoopback_AnotherDaemonOnThePortIsNotOK(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeMCPToken(t, home, "our-secret")
	port := startLoopback(t, "some-other-daemons-secret")

	r := checkMCPLoopback(&server.Config{MCPPort: port}, false)
	if r.Status != StatusFail {
		t.Fatalf("a foreign MCP endpoint on our port should fail, got %s: %q", r.Status, r.Message)
	}
	if !contains(r.Message, "401") {
		t.Errorf("message should say the token was rejected with a 401, got: %q", r.Message)
	}
}

// The same collision seen from a host that has no token to offer — a fresh
// $HOME, which is how doctor is run under test harnesses and from a unit file
// with its own home. Nothing can be concluded, so nothing is: not a tick.
func TestCheckMCPLoopback_NoTokenOnHostIsSkipNotOK(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	port := startLoopback(t, "some-other-daemons-secret")

	r := checkMCPLoopback(&server.Config{MCPPort: port}, false)
	if r.Status != StatusSkip {
		t.Fatalf("no bearer token means no verdict, expected skip, got %s: %q", r.Status, r.Message)
	}
	// The status alone would also be satisfied by the dial failing — closedPort
	// is inherently racy — which would leave this green while asserting nothing
	// about the branch it is named for.
	if !contains(r.Message, "no bearer token") {
		t.Errorf("skipped for the wrong reason: %q", r.Message)
	}
}

// The token falls back to the Authorization header cc's settings.json carries,
// which matters more than it looks: any graceful shutdown under this $HOME
// removes ~/.tether/mcp-token unconditionally (lifecycle.go), including a
// sibling daemon's, so a live deployment can find its own token file gone.
func TestCheckMCPLoopback_FallsBackToTheSettingsJSONToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	port := startLoopback(t, "our-secret") // and deliberately no ~/.tether/mcp-token
	writeJSON(t, filepath.Join(home, ".claude", "settings.json"), map[string]any{
		"mcpServers": map[string]any{
			"tether": map[string]any{
				agent.TetherManagedKey: true,
				"url":                  fmt.Sprintf("http://127.0.0.1:%d/mcp", port),
				"headers":              map[string]any{"Authorization": "Bearer our-secret"},
			},
		},
	})

	r := checkMCPLoopback(&server.Config{MCPPort: port}, true)
	if r.Status != StatusOK {
		t.Fatalf("expected ok from the settings.json token, got %s: %q", r.Status, r.Message)
	}
	if !contains(r.Detail, "settings.json") {
		t.Errorf("expected the detail to name the fallback token source, got: %q", r.Detail)
	}
}

// A stray space in a hand-edited settings.json is not a credential. Untrimmed
// it survives the non-empty test, gets offered, 401s, and reds a healthy host.
func TestCheckMCPLoopback_WhitespaceOnlySettingsTokenIsNotOffered(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	port := startLoopback(t, "our-secret")
	writeJSON(t, filepath.Join(home, ".claude", "settings.json"), map[string]any{
		"mcpServers": map[string]any{
			"tether": map[string]any{
				agent.TetherManagedKey: true,
				"url":                  fmt.Sprintf("http://127.0.0.1:%d/mcp", port),
				"headers":              map[string]any{"Authorization": "Bearer  "},
			},
		},
	})

	r := checkMCPLoopback(&server.Config{MCPPort: port}, false)
	if r.Status == StatusFail {
		t.Errorf("a blank token is doctor having none, not a fault on the host: %q", r.Message)
	}
	if r.Status != StatusSkip {
		t.Errorf("expected skip, got %s: %q", r.Status, r.Message)
	}
}

// The green line has to name where the port came from. The handshake settles
// whose endpoint answered and never that this deployment serves that port, so
// on a two-daemon host a bare `tether doctor` can tick the wrong daemon — and
// Detail does not count, because cmd/tether only prints it under --verbose.
func TestCheckMCPLoopback_OKMessageNamesThePortSource(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeMCPToken(t, home, "our-secret")
	port := startLoopback(t, "our-secret")
	writeJSON(t, filepath.Join(home, ".claude", "settings.json"), map[string]any{
		"mcpServers": map[string]any{
			"tether": map[string]any{
				agent.TetherManagedKey: true,
				"url":                  fmt.Sprintf("http://127.0.0.1:%d/mcp", port),
			},
		},
	})

	r := checkMCPLoopback(&server.Config{}, false) // no --mcp-port: the port is a guess
	if r.Status != StatusOK {
		t.Fatalf("expected ok, got %s: %q", r.Status, r.Message)
	}
	if !contains(r.Message, portFromSettings.String()) {
		t.Errorf("a tick reached via a guessed port must say so in the message, got: %q", r.Message)
	}
}

// Anything that is not an MCP endpoint at all. Also not a tick — and this is
// the case the old dial passed most readily, since a listening socket was the
// entire test.
func TestCheckMCPLoopback_NonMCPListenerIsSkipNotOK(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeMCPToken(t, home, "our-secret")

	port := closedPort(t)
	srv := &http.Server{
		Addr: fmt.Sprintf("127.0.0.1:%d", port),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "hello from something else\n")
		}),
	}
	l, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })

	r := checkMCPLoopback(&server.Config{MCPPort: port}, false)
	if r.Status != StatusSkip {
		t.Fatalf("a non-MCP listener should not be a verdict either way, expected skip, got %s: %q", r.Status, r.Message)
	}
}

// --mcp-port outranks cc's settings.json. Both are live MCP endpoints holding
// this host's token, so the only thing that can decide which one the check
// reports on is the precedence rule.
func TestCheckMCPLoopback_FlagOutranksSettingsJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeMCPToken(t, home, "our-secret")
	flagPort := startLoopback(t, "our-secret")
	settingsPort := startLoopback(t, "our-secret")
	writeJSON(t, filepath.Join(home, ".claude", "settings.json"), map[string]any{
		"mcpServers": map[string]any{
			"tether": map[string]any{
				agent.TetherManagedKey: true,
				"url":                  fmt.Sprintf("http://127.0.0.1:%d/mcp", settingsPort),
			},
		},
	})

	r := checkMCPLoopback(&server.Config{MCPPort: flagPort}, false)
	if r.Status != StatusOK {
		t.Fatalf("expected ok, got %s: %q", r.Status, r.Message)
	}
	if !contains(r.Message, fmt.Sprintf("127.0.0.1:%d", flagPort)) {
		t.Errorf("expected the --mcp-port endpoint %d in the message, got: %q", flagPort, r.Message)
	}
}

// ...and without the flag, settings.json is used.
func TestCheckMCPLoopback_SettingsJSONUsedWhenFlagAbsent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeMCPToken(t, home, "our-secret")
	settingsPort := startLoopback(t, "our-secret")
	writeJSON(t, filepath.Join(home, ".claude", "settings.json"), map[string]any{
		"mcpServers": map[string]any{
			"tether": map[string]any{
				agent.TetherManagedKey: true,
				"url":                  fmt.Sprintf("http://127.0.0.1:%d/mcp", settingsPort),
			},
		},
	})

	r := checkMCPLoopback(&server.Config{}, false)
	if r.Status != StatusOK {
		t.Fatalf("expected ok, got %s: %q", r.Status, r.Message)
	}
	if !contains(r.Message, fmt.Sprintf("127.0.0.1:%d", settingsPort)) {
		t.Errorf("expected the settings.json endpoint %d in the message, got: %q", settingsPort, r.Message)
	}
}

// A per-task instance entry ("tether-<slug>", internal/mcp/instance/instance.go)
// must not be mistaken for the singleton loopback. The pre-fix check scanned
// every tether-managed entry and kept the last, so with both present it aimed
// at whichever one Go's map iteration reached last — a different endpoint from
// run to run.
//
// The assertion is the port; the repetition is not asserting anything about the
// current implementation, which is a single map lookup and cannot vary. It is
// there so that a regression to the old scan cannot pass by luck: one run of
// that code picks the right entry about half the time.
func TestResolveMCPPort_IgnoresPerTaskInstanceEntries(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	singleton, perTask := closedPort(t), closedPort(t)
	writeJSON(t, filepath.Join(home, ".claude", "settings.json"), map[string]any{
		"mcpServers": map[string]any{
			"tether": map[string]any{
				agent.TetherManagedKey: true,
				"url":                  fmt.Sprintf("http://127.0.0.1:%d/mcp", singleton),
			},
			"tether-some-task": map[string]any{
				agent.TetherManagedKey: true,
				"url":                  fmt.Sprintf("http://127.0.0.1:%d/mcp", perTask),
			},
		},
	})
	for i := range 20 {
		got, src := resolveMCPPort(&server.Config{})
		if got != singleton {
			t.Fatalf("iteration %d: got port %d (%s), want the singleton entry's %d", i, got, src, singleton)
		}
	}
}

// Under --skip-mcp-inject the entry in settings.json was left by some other
// daemon, so it is evidence about that one. Falling back to the default is the
// honest answer, and the message has to admit it is a guess.
func TestResolveMCPPort_SkipMCPInjectIgnoresSettingsJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeJSON(t, filepath.Join(home, ".claude", "settings.json"), map[string]any{
		"mcpServers": map[string]any{
			"tether": map[string]any{
				agent.TetherManagedKey: true,
				"url":                  "http://127.0.0.1:19999/mcp",
			},
		},
	})
	got, src := resolveMCPPort(&server.Config{SkipMCPInject: true})
	if got != 8899 {
		t.Errorf("expected the default 8899, got %d", got)
	}
	if src.hint() == "" {
		t.Errorf("a guessed port must carry the --mcp-port hint, source was %q", src)
	}
}

func TestProbeMCPEndpoint_TerminatesTheSessionItOpened(t *testing.T) {
	// A recording stand-in rather than the SDK server, because what is being
	// asserted is what doctor sends: the DELETE that stops a preflight probe
	// from leaving 30 minutes of session state (MCPLoopback's SessionTimeout)
	// behind on a live daemon.
	var mu sync.Mutex // the handler runs on net/http's goroutines, not this one
	var methods []string
	var deleteSessionID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()
		if r.Method == http.MethodDelete {
			mu.Lock()
			deleteSessionID = r.Header.Get("Mcp-Session-Id")
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Mcp-Session-Id", "SESSION-1")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message\ndata: "+
			`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","serverInfo":{"name":"tether","version":"v9"}}}`+"\n\n")
	}))
	defer srv.Close()

	id, err := probeMCPEndpoint(srv.URL+"/mcp", "tok", 5*time.Second)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if id.ServerName != "tether" || id.ServerVersion != "v9" {
		t.Errorf("unexpected identity: %+v", id)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(methods) != 2 || methods[0] != http.MethodPost || methods[1] != http.MethodDelete {
		t.Errorf("expected POST then DELETE, got %v", methods)
	}
	if deleteSessionID != "SESSION-1" {
		t.Errorf("DELETE carried session id %q, want SESSION-1", deleteSessionID)
	}
}

// The session DELETE carries the same bearer token the handshake did, so it
// needs the same redirect policy. net/http keeps Authorization across a
// redirect to the same hostname on another port, so the leak this closes is
// reachable from anything that can answer on the loopback port.
func TestProbeMCPEndpoint_TheSessionDeleteDoesNotFollowARedirectEither(t *testing.T) {
	var mu sync.Mutex
	var elsewhereSawAuth string
	elsewhere := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		mu.Lock()
		elsewhereSawAuth = r.Header.Get("Authorization")
		mu.Unlock()
	}))
	defer elsewhere.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			http.Redirect(w, r, elsewhere.URL+"/mcp", http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("Mcp-Session-Id", "SESSION-1")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w,
			`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","serverInfo":{"name":"tether","version":"v9"}}}`)
	}))
	defer srv.Close()

	if _, err := probeMCPEndpoint(srv.URL+"/mcp", "SUPER-SECRET", 5*time.Second); err != nil {
		t.Fatalf("probe: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if elsewhereSawAuth != "" {
		t.Errorf("the DELETE's redirect target received the bearer token: %q", elsewhereSawAuth)
	}
}

// A priming event — `data:` with nothing after it — must not be mistaken for
// the reply. The SDK emits one when resumption is configured, which this
// deployment's loopback does not do today; "first data frame" would have made
// that a silent skip on every healthy host the day it did.
func TestProbeMCPEndpoint_SkipsAnEmptyPrimingDataFrame(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "id: 0\ndata: \n\n")
		_, _ = io.WriteString(w, "event: message\ndata: "+
			`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","serverInfo":{"name":"tether","version":"v9"}}}`+"\n\n")
	}))
	defer srv.Close()

	id, err := probeMCPEndpoint(srv.URL+"/mcp", "tok", 5*time.Second)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if id.ServerName != "tether" {
		t.Errorf("unexpected identity: %+v", id)
	}
}

func TestProbeMCPEndpoint_DoesNotFollowARedirect(t *testing.T) {
	// Two things ride on this: the probe must answer about the socket it
	// dialled and not one a redirect names, and it must not hand this
	// deployment's bearer token to that target. The hop is to a server that
	// would happily pass the handshake, so a probe that followed it would
	// return ok.
	var elsewhereSawAuth string
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhereSawAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w,
			`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","serverInfo":{"name":"tether","version":"v9"}}}`)
	}))
	defer elsewhere.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/mcp", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	if _, err := probeMCPEndpoint(redirector.URL+"/mcp", "tok", 5*time.Second); err == nil {
		t.Error("a redirect should not produce an identity")
	}
	if elsewhereSawAuth != "" {
		t.Errorf("the redirect target received the bearer token: %q", elsewhereSawAuth)
	}
}

func TestProbeMCPEndpoint_ReadsADataFrameOverScannersDefaultLineCap(t *testing.T) {
	// bufio.Scanner refuses lines over 64KiB unless told otherwise, and an
	// initialize result can exceed that once a server fills in capabilities.
	big := strings.Repeat("x", 100_000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message\ndata: "+
			`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","instructions":"`+big+
			`","serverInfo":{"name":"tether","version":"v9"}}}`+"\n\n")
	}))
	defer srv.Close()

	id, err := probeMCPEndpoint(srv.URL+"/mcp", "tok", 5*time.Second)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if id.ServerName != "tether" {
		t.Errorf("unexpected identity: %+v", id)
	}
}

func TestProbeMCPEndpoint_AcceptsAPlainJSONReply(t *testing.T) {
	// The SDK answers initialize as SSE, but the transport permits
	// application/json and firstJSONRPCMessage has to read both.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w,
			`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","serverInfo":{"name":"tether","version":"v9"}}}`)
	}))
	defer srv.Close()

	id, err := probeMCPEndpoint(srv.URL+"/mcp", "tok", 5*time.Second)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if id.ServerName != "tether" {
		t.Errorf("unexpected identity: %+v", id)
	}
}

func TestProbeMCPEndpoint_DoesNotWaitOutAnSSEStreamThatStaysOpen(t *testing.T) {
	// The reason firstJSONRPCMessage scans instead of reading to EOF: a server
	// that keeps the event stream open after the reply would otherwise stall
	// the probe until its deadline, and a healthy endpoint would be reported as
	// a timeout.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message\ndata: "+
			`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","serverInfo":{"name":"tether","version":"v9"}}}`+"\n\n")
		w.(http.Flusher).Flush()
		<-release // hold the stream open
	}))
	defer func() { close(release); srv.Close() }()

	start := time.Now()
	if _, err := probeMCPEndpoint(srv.URL+"/mcp", "tok", 30*time.Second); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("probe waited %v on an open stream; it should return on the first data frame", elapsed)
	}
}

func TestCheckMCPSettingsInject_PortConflictFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeJSON(t, filepath.Join(home, ".claude", "settings.json"), map[string]any{
		"mcpServers": map[string]any{
			"tether": map[string]any{
				agent.TetherManagedKey: true,
				"url":                  "http://127.0.0.1:8899/mcp",
			},
		},
	})
	r := checkMCPSettingsInject(&server.Config{MCPPort: 9000}, false)
	if r.Status != StatusFail {
		t.Fatalf("an entry aimed at another daemon should fail, got %s: %q", r.Status, r.Message)
	}
	// Unchanged when the two agree, so the new rule cannot redden a host that
	// injected its own entry.
	if r := checkMCPSettingsInject(&server.Config{MCPPort: 8899}, false); r.Status != StatusOK {
		t.Errorf("matching ports should stay ok, got %s: %q", r.Status, r.Message)
	}
}

// With no --mcp-port there is nothing to compare the entry against, so the
// verdict stays ok — but the port it aims cc at goes in the message, because
// the reader is then the only one who can notice it is the wrong daemon's.
func TestCheckMCPSettingsInject_NamesWhereCCWillGo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeJSON(t, filepath.Join(home, ".claude", "settings.json"), map[string]any{
		"mcpServers": map[string]any{
			"tether": map[string]any{
				agent.TetherManagedKey: true,
				"url":                  "http://127.0.0.1:8899/mcp",
			},
		},
	})
	r := checkMCPSettingsInject(&server.Config{}, false)
	if r.Status != StatusOK {
		t.Fatalf("expected ok, got %s: %q", r.Status, r.Message)
	}
	if !contains(r.Message, "8899") {
		t.Errorf("the message must name the port cc will use, got: %q", r.Message)
	}
}

func TestCheckMCPSettingsInject_SkipMCPInjectIsSkipNotFail(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no settings.json at all
	r := checkMCPSettingsInject(&server.Config{SkipMCPInject: true}, false)
	if r.Status != StatusSkip {
		t.Errorf("--skip-mcp-inject means there is nothing to check, expected skip, got %s: %q", r.Status, r.Message)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}

// The bug these cover (tether#84): checkCertState read ~/.tether/cert.pem for
// every deployment, so it reported "cert not found" against a healthy
// --cert-file host and reported on the wrong file entirely against an ACME one.
// Each test below is written so that the pre-fix code fails it.

// writeCertPEM writes a self-signed cert expiring at notAfter. Just the
// certificate — the managed and ACME paths are judged on it alone, since both
// re-create the pair themselves when it is incomplete. The operator path is
// not; see writeOperatorPair.
func writeCertPEM(t *testing.T, path string, notAfter time.Time) {
	t.Helper()
	certPEM, _ := genCertPEM(t, notAfter)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
}

// genCertPEM returns a self-signed cert and the key that signed it.
//
// The key comes back rather than being discarded because the pair has to stay a
// pair: an earlier version of writeOperatorPair generated a second, unrelated
// key, so every "healthy operator deployment" fixture was in fact a cert and a
// key that crypto/tls refuses to load together. Every assertion still passed,
// because nothing checked. The check that found it is the one this file gained
// for exactly that failure — a fixture bug is what a false pass looks like from
// the inside.
func genCertPEM(t *testing.T, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "tether-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// writeOperatorPair writes a matching cert and key into a fresh directory, the
// way certbot's live/ layout holds them.
func writeOperatorPair(t *testing.T, notAfter time.Time) (certPath, keyPath string) {
	t.Helper()
	dir := t.TempDir()
	certPath = filepath.Join(dir, "fullchain.pem")
	keyPath = filepath.Join(dir, "privkey.pem")
	certPEM, keyPEM := genCertPEM(t, notAfter)
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// managedCert seeds ~/.tether/cert.pem — the file the old check read no matter
// what, and the one the operator-file and ACME tests below must not read.
func managedCert(t *testing.T, home string, notAfter time.Time) string {
	t.Helper()
	path := filepath.Join(home, ".tether", "cert.pem")
	writeCertPEM(t, path, notAfter)
	return path
}

func TestCheckCertState_ManagedCertIsStillChecked(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	managedCert(t, home, time.Now().Add(14*24*time.Hour))

	r := checkCertState(&server.Config{}, false)
	if r.Status != StatusOK {
		t.Fatalf("status = %s, want ok: %q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "managed") {
		t.Errorf("message does not say which cert was checked: %q", r.Message)
	}
}

// A missing managed cert is not a failure, and the message has to leave room
// for the other reading: no cert flags were passed, which is also the state of
// knowing nothing about a host that may well be serving --cert-file.
func TestCheckCertState_MissingManagedCertSkipsRatherThanFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	r := checkCertState(&server.Config{}, false)
	if r.Status != StatusSkip {
		t.Fatalf("status = %s, want skip: %q", r.Status, r.Message)
	}
	for _, want := range []string{"--cert-file", "--acme-domain"} {
		if !strings.Contains(r.Message, want) {
			t.Errorf("message does not mention %s, so the reader cannot tell what to re-run: %q", want, r.Message)
		}
	}
}

// The headline case: a --cert-file deployment with no managed cert at all. The
// old check reported "cert not found — run `tether server` to generate" here.
func TestCheckCertState_OperatorFilesAreCheckedWhereTheyLive(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	certPath, keyPath := writeOperatorPair(t, time.Now().Add(60*24*time.Hour))

	r := checkCertState(&server.Config{CertFile: certPath, KeyFile: keyPath}, true)
	if r.Status != StatusOK {
		t.Fatalf("status = %s, want ok: %q", r.Status, r.Message)
	}
	if !strings.Contains(r.Detail, certPath) {
		t.Errorf("verbose detail does not name the file that was read: %q", r.Detail)
	}
}

// A named file that is not there is a real fault on this path, and a different
// one from the managed case: LoadOrGenCert returns the read error and Run
// propagates it, so the server does not start.
func TestCheckCertState_MissingOperatorCertFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// A healthy managed cert is present precisely so a check that fell back to
	// it would pass and this test would catch it.
	managedCert(t, home, time.Now().Add(14*24*time.Hour))

	r := checkCertState(&server.Config{CertFile: "/nope/fullchain.pem", KeyFile: "/nope/privkey.pem"}, false)
	if !r.Failed() {
		t.Fatalf("status = %s, want fail: %q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "/nope/fullchain.pem") {
		t.Errorf("message does not name the missing file: %q", r.Message)
	}
}

// A valid cert next to a missing key is the false pass this check could most
// easily have shipped: the cert parses, the expiry is months out, and the
// server still refuses to start because crypto/tls loads the pair or nothing.
func TestCheckCertState_OperatorCertWithoutItsKeyFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	certPath, keyPath := writeOperatorPair(t, time.Now().Add(60*24*time.Hour))
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}

	r := checkCertState(&server.Config{CertFile: certPath, KeyFile: keyPath}, false)
	if !r.Failed() {
		t.Fatalf("status = %s, want fail: %q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, keyPath) {
		t.Errorf("message does not name the missing key: %q", r.Message)
	}
}

// The managed path is excluded on purpose: loadOrRotateManaged re-mints the
// whole pair when it cannot load, so a missing key.pem is repaired rather than
// fatal, and failing the check would be the same false alarm in a new place.
func TestCheckCertState_ManagedCertWithoutItsKeyIsNotAFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	managedCert(t, home, time.Now().Add(14*24*time.Hour))

	r := checkCertState(&server.Config{}, false)
	if r.Failed() {
		t.Errorf("status = fail with no key.pem present: %q", r.Message)
	}
}

// seedACMECert writes a cert into certmagic's layout under ~/.tether/acme.
// The path literals mirror certmagic v0.25.3's storage keys and are written out
// here rather than borrowed from the implementation, so that a wrong layout in
// the implementation cannot make this test agree with it.
func seedACMECert(t *testing.T, home, domain string, notAfter time.Time) string {
	t.Helper()
	path := filepath.Join(home, ".tether", "acme", "certificates",
		"acme-v02.api.letsencrypt.org-directory", domain, domain+".crt")
	writeCertPEM(t, path, notAfter)
	return path
}

// An ACME host has a ~/.tether/cert.pem too — Run mints it at Step 4 before
// Step 4b discards it for certmagic's — and nothing rotates it afterwards. So
// the old check did not merely look in the wrong place: given a daemon up for
// more than 14 days it went red, naming an expiry that had no bearing on what
// the daemon was serving. The expired managed cert here is what makes this test
// fail against the old code rather than pass by accident.
func TestCheckCertState_ACMEReadsTheStoredCertNotTheUnservedManagedOne(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	managedCert(t, home, time.Now().Add(-time.Hour))
	want := seedACMECert(t, home, "example.com", time.Now().Add(60*24*time.Hour))

	r := checkCertState(&server.Config{AcmeDomain: "example.com"}, true)
	if r.Status != StatusOK {
		t.Fatalf("status = %s, want ok: %q", r.Status, r.Message)
	}
	if !strings.Contains(r.Detail, want) {
		t.Errorf("verbose detail = %q, want the stored ACME cert %q", r.Detail, want)
	}
}

func TestCheckCertState_ACMEWithAnEmptyStoreSkips(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	managedCert(t, home, time.Now().Add(14*24*time.Hour))

	r := checkCertState(&server.Config{AcmeDomain: "example.com"}, false)
	if r.Status != StatusSkip {
		t.Fatalf("status = %s, want skip: %q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "example.com") {
		t.Errorf("message does not name the domain: %q", r.Message)
	}
}

// Expiry is still a failure — the fix is about which file is read, not about
// softening the verdict. What changes with the source is the remedy, because
// who is supposed to renew differs: the managed loop re-mints, an operator's
// certbot does not know about tether, and certmagic renews on its own.
func TestCheckCertState_NearExpiryFailsWithARemedyForThatCertPath(t *testing.T) {
	soon := time.Now().Add(30 * time.Minute)

	t.Run("managed", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		managedCert(t, home, soon)

		r := checkCertState(&server.Config{}, false)
		if !r.Failed() {
			t.Fatalf("status = %s, want fail: %q", r.Status, r.Message)
		}
		// Kept verbatim from before tether#84: managed certs really do rotate
		// within the hour since tether#72, so this sentence is true here — and
		// only here.
		if !strings.Contains(r.Message, "a running server rotates within the hour") {
			t.Errorf("managed remedy lost: %q", r.Message)
		}
	})

	t.Run("operator files", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		certPath, keyPath := writeOperatorPair(t, soon)

		r := checkCertState(&server.Config{CertFile: certPath, KeyFile: keyPath}, false)
		if !r.Failed() {
			t.Fatalf("status = %s, want fail: %q", r.Status, r.Message)
		}
		if strings.Contains(r.Message, "rotates within the hour") {
			t.Errorf("told the operator tether will rotate a cert it cannot issue: %q", r.Message)
		}
		if !strings.Contains(r.Message, certPath) {
			t.Errorf("remedy does not name the file to renew: %q", r.Message)
		}
	})

	t.Run("acme", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		seedACMECert(t, home, "example.com", soon)

		r := checkCertState(&server.Config{AcmeDomain: "example.com"}, false)
		if !r.Failed() {
			t.Fatalf("status = %s, want fail: %q", r.Status, r.Message)
		}
		if strings.Contains(r.Message, "~/.tether writability") {
			t.Errorf("managed remedy leaked onto the ACME path: %q", r.Message)
		}
		if !strings.Contains(r.Message, "certmagic") {
			t.Errorf("remedy does not point at the thing that renews this cert: %q", r.Message)
		}
	})
}

// --cert-file without --key-file is ignored by the loader, which serves the
// managed cert instead. Reporting on the managed cert is therefore correct, and
// saying nothing about the ignored flag is how an operator ends up believing
// doctor inspected their cert.
func TestCheckCertState_LonePEMFlagIsCalledOut(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	managedCert(t, home, time.Now().Add(14*24*time.Hour))

	for _, cfg := range []*server.Config{{CertFile: "/c.pem"}, {KeyFile: "/k.pem"}} {
		r := checkCertState(cfg, false)
		if r.Status != StatusOK {
			t.Fatalf("%+v: status = %s, want ok: %q", cfg, r.Status, r.Message)
		}
		if !strings.Contains(r.Message, "only take effect together") {
			t.Errorf("%+v: ignored flag not reported: %q", cfg, r.Message)
		}
	}
}

// Both flags set means the pair is complete, so there is nothing to warn about
// and the hint must not fire.
func TestCheckCertState_NoLoneFlagNoiseWhenThePairIsComplete(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	certPath, keyPath := writeOperatorPair(t, time.Now().Add(60*24*time.Hour))

	r := checkCertState(&server.Config{CertFile: certPath, KeyFile: keyPath}, false)
	if strings.Contains(r.Message, "only take effect together") {
		t.Errorf("hint fired for a complete pair: %q", r.Message)
	}
}

// cc-binary asked PATH directly while the server asks ResolveClaudePath, so a
// host with $TETHER_CC_PATH set — or cc installed somewhere the caller's PATH
// does not cover — failed a check about a spawn that would have worked.
func TestCheckCCBinary_HonoursTheResolutionTheServerUses(t *testing.T) {
	dir := t.TempDir()
	ccPath := filepath.Join(dir, "cc-somewhere-else")
	if err := os.WriteFile(ccPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// An empty PATH is what makes this a test of the resolver rather than of
	// the machine it runs on.
	t.Setenv("PATH", "")
	t.Setenv("TETHER_CC_PATH", ccPath)

	r := checkCCBinary(true)
	if r.Status != StatusOK {
		t.Fatalf("status = %s, want ok: %q", r.Status, r.Message)
	}
	if r.Detail != ccPath {
		t.Errorf("detail = %q, want the resolved path %q", r.Detail, ccPath)
	}
}

func TestCheckCCBinary_FailsWhenTheResolvedPathIsNotExecutable(t *testing.T) {
	dir := t.TempDir()
	notExec := filepath.Join(dir, "cc-not-executable")
	if err := os.WriteFile(notExec, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "")
	t.Setenv("TETHER_CC_PATH", notExec)

	r := checkCCBinary(false)
	if !r.Failed() {
		t.Errorf("status = %s, want fail for a non-executable cc: %q", r.Status, r.Message)
	}
}

// A skip is not a failure anywhere it is read: not in Report.OK, and not in the
// derived ok field that --json still emits for pre-tether#84 consumers.
func TestCheckResultJSON_SkipSerialisesAsNotAFailure(t *testing.T) {
	for _, tc := range []struct {
		in     CheckResult
		wantOK bool
	}{
		{CheckResult{Name: "a", Status: StatusOK, Message: "all good", Detail: "d1"}, true},
		{CheckResult{Name: "b", Status: StatusSkip, Message: "could not look"}, true},
		{CheckResult{Name: "c", Status: StatusFail, Message: "broken", Detail: "d3"}, false},
	} {
		raw, err := json.Marshal(tc.in)
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Name    string `json:"name"`
			OK      bool   `json:"ok"`
			Status  string `json:"status"`
			Message string `json:"message"`
			Detail  string `json:"detail"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if got.OK != tc.wantOK {
			t.Errorf("%s: ok = %v, want %v", tc.in.Status, got.OK, tc.wantOK)
		}
		if got.Status != string(tc.in.Status) {
			t.Errorf("status = %q, want %q", got.Status, tc.in.Status)
		}
		// MarshalJSON restates the field list, so every field it forwards has
		// to be asserted here or it can be dropped without any test noticing —
		// which is what a hand-written marshaller costs. (Mutating Message and
		// Detail to "" left the suite green until this was added.)
		if got.Name != tc.in.Name || got.Message != tc.in.Message || got.Detail != tc.in.Detail {
			t.Errorf("payload lost content: got name=%q message=%q detail=%q, want name=%q message=%q detail=%q",
				got.Name, got.Message, got.Detail, tc.in.Name, tc.in.Message, tc.in.Detail)
		}
	}
}

// Report.OK is the exit code, so it has to mean "nothing failed" and not
// "everything passed" — otherwise a skip would still fail the command and the
// third state would have bought nothing.
//
// The host built here is the one the bug was reported from, minus the cert
// flags: everything doctor can check is in order, and the two things it cannot
// reach — which cert is served, and a daemon that is not running — skip. It has
// to exit 0. Asserting instead that report.OK agrees with the count of failures
// looks equivalent and is not: on a host that has both failures and skips, both
// readings say false, and a Run that failed the report over a skip passes that
// version of this test (verified by mutation).
func TestRun_SkipsDoNotFailTheReport(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".tether"), 0o700); err != nil {
		t.Fatal(err)
	}
	cc := filepath.Join(home, "cc")
	if err := os.WriteFile(cc, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TETHER_CC_PATH", cc)
	// One settings.json satisfies both the hook check and the MCP inject check;
	// its url points nowhere, which is what makes mcp-loopback skip.
	writeJSON(t, filepath.Join(home, ".claude", "settings.json"), map[string]any{
		"hooks": map[string]any{
			"PreToolUse": []any{map[string]any{agent.TetherManagedKey: true}},
		},
		"mcpServers": map[string]any{
			"tether": map[string]any{
				agent.TetherManagedKey: true,
				"url":                  fmt.Sprintf("http://127.0.0.1:%d/mcp", closedPort(t)),
			},
		},
	})

	report := Run(&server.Config{Port: closedPort(t)}, false)

	var skipped int
	for _, c := range report.Checks {
		if c.Status == StatusSkip {
			skipped++
		}
		if c.Failed() {
			t.Errorf("%s failed on a host with nothing wrong: %s", c.Name, c.Message)
		}
	}
	if skipped == 0 {
		t.Fatal("nothing skipped, so this proves nothing about skips")
	}
	if !report.OK {
		t.Errorf("report.OK = false with %d skips and no failures", skipped)
	}
}

// A cert whose location cannot be established is a skip: doctor has not learnt
// anything about the certificate, so saying it is broken would be inventing a
// verdict — the habit this whole change is about.
func TestCheckCertState_UnlocatableCertSkips(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	acme := filepath.Join(home, ".tether", "acme")
	if err := os.MkdirAll(acme, 0o700); err != nil {
		t.Fatal(err)
	}
	// A file where the certificates directory belongs: unreadable as a store,
	// and unlike a permission bit it still reproduces when the tests run as root.
	if err := os.WriteFile(filepath.Join(acme, "certificates"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := checkCertState(&server.Config{AcmeDomain: "example.com"}, false)
	if r.Status != StatusSkip {
		t.Errorf("status = %s, want skip: %q", r.Status, r.Message)
	}
}

// The remaining false alarm after the flags were added: an ACME host run
// through a bare `tether doctor`. Its ~/.tether/cert.pem is real (Step 4 mints
// it) and nothing renews it (certRenewalFor gives ACME no loop), so past day 14
// doctor goes red about a file the daemon does not serve. The verdict on that
// file stands — doctor was not told otherwise — but the reader gets the flag
// that settles it.
func TestCheckCertState_ExpiredManagedCertPointsAtAnACMEStoreIfThereIsOne(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	managedCert(t, home, time.Now().Add(-time.Hour))
	seedACMECert(t, home, "example.com", time.Now().Add(60*24*time.Hour))

	r := checkCertState(&server.Config{}, false)
	if !r.Failed() {
		t.Fatalf("status = %s, want fail — the managed cert really has expired: %q", r.Status, r.Message)
	}
	for _, want := range []string{"example.com", "--acme-domain"} {
		if !strings.Contains(r.Message, want) {
			t.Errorf("message does not mention %s: %q", want, r.Message)
		}
	}
}

// No store, no note. A managed-only host must not be told to go and look for an
// ACME deployment it does not have.
func TestCheckCertState_NoACMENoteWithoutAStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	managedCert(t, home, time.Now().Add(-time.Hour))

	r := checkCertState(&server.Config{}, false)
	if strings.Contains(r.Message, "--acme-domain") && strings.Contains(r.Message, "ACME cert store") {
		t.Errorf("invented an ACME store: %q", r.Message)
	}
}

// The note is for the managed branch only. Under --acme-domain the store is the
// subject of the check, not a hint about it.
func TestCheckCertState_NoACMENoteWhenACMEIsWhatIsBeingChecked(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedACMECert(t, home, "example.com", time.Now().Add(60*24*time.Hour))

	r := checkCertState(&server.Config{AcmeDomain: "example.com"}, false)
	if strings.Contains(r.Message, "ACME cert store") {
		t.Errorf("hint fired while checking the ACME cert itself: %q", r.Message)
	}
}

// --acme-domain with an unreadable --cert-file is a host that does not boot,
// and the check used to walk straight past it: the served cert is certmagic's,
// so nothing looked at the files. Run's Step 4 loads them anyway, before Step
// 4b hands the listener to certmagic, and returns the error.
func TestCheckCertState_UnloadableOperatorPairFailsEvenUnderACME(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedACMECert(t, home, "example.com", time.Now().Add(60*24*time.Hour))

	r := checkCertState(&server.Config{
		AcmeDomain: "example.com",
		CertFile:   "/nope/fullchain.pem",
		KeyFile:    "/nope/privkey.pem",
	}, false)
	if !r.Failed() {
		t.Fatalf("status = %s, want fail: %q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "/nope/fullchain.pem") {
		t.Errorf("message does not name the file that stops startup: %q", r.Message)
	}
}

// A key that exists but does not belong to the cert is the other way this pair
// fails to load, and os.Stat cannot see it — crypto/tls matches the public keys.
func TestCheckCertState_MismatchedOperatorKeyFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	certPath, _ := writeOperatorPair(t, time.Now().Add(60*24*time.Hour))
	_, otherKey := writeOperatorPair(t, time.Now().Add(60*24*time.Hour))

	r := checkCertState(&server.Config{CertFile: certPath, KeyFile: otherKey}, false)
	if !r.Failed() {
		t.Errorf("status = %s for a key that does not match the cert: %q", r.Status, r.Message)
	}
}

// A host where $HOME cannot be resolved — a systemd unit without it is the
// usual way — is one where `tether server` does not start at all: Step 2's
// tetherDataDir returns this same error out of Run. Turning every home-dependent
// check into a skip made `tether doctor` exit 0 there, which is a worse lie
// than the false alarm this wi removed. data-dir is the check that owns saying
// so.
func TestCheckDataDir_UndeterminableHomeIsAFailure(t *testing.T) {
	t.Setenv("HOME", "")
	if r := checkDataDir(false); !r.Failed() {
		t.Errorf("status = %s, want fail: %q", r.Status, r.Message)
	}
}

func TestRun_UndeterminableHomeFailsTheReport(t *testing.T) {
	t.Setenv("HOME", "")
	if report := Run(&server.Config{Port: closedPort(t)}, false); report.OK {
		t.Error("report.OK = true on a host where the server cannot start")
	}
}
