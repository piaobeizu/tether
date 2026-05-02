package cc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// HookHandlers is the dependency-injection seam for the HTTP server: the
// daemon plugs in its own handlers, the server owns the wire-protocol
// shape (request method, path mapping, response schema). The two
// "decision" hooks (PreToolUse, UserPromptSubmit) return HookResponse so
// the server can render cc's hookSpecificOutput envelope; the three
// observer hooks (PostToolUse, SessionStart, Stop) return only error.
//
// All methods receive the raw JSON body (json.RawMessage) verbatim — the
// daemon decides how / whether to decode. This matches the seam invariant
// "daemon doesn't interpret event content" (spec §5.0).
//
// Concurrency: implementations MUST be safe for concurrent invocation.
// cc emits hooks in parallel for parallel tool calls; net/http serves
// each request on its own goroutine.
type HookHandlers interface {
	PreToolUse(ctx context.Context, payload json.RawMessage) (HookResponse, error)
	PostToolUse(ctx context.Context, payload json.RawMessage) error
	SessionStart(ctx context.Context, payload json.RawMessage) error
	UserPromptSubmit(ctx context.Context, payload json.RawMessage) (HookResponse, error)
	Stop(ctx context.Context, payload json.RawMessage) error
}

// HookResponse is the daemon's decision for a "decision" hook. Decision
// must be one of "allow" / "deny" / "ask" (cc's permissionDecision enum,
// spec §5.2 F-05); Reason is surfaced in the cc UI when the hook denies.
type HookResponse struct {
	Decision string
	Reason   string
}

// HookServer is the local HTTP server cc POSTs hook callbacks into. cc is
// configured with the server's URLs at spawn time (see hooks.go for the
// settings.json generation that wires this up). The server binds on
// 127.0.0.1:0 (kernel picks a free port); the assigned port is read via
// Addr() after Start succeeds.
type HookServer struct {
	handlers HookHandlers

	mu       sync.Mutex
	listener net.Listener
	server   *http.Server
	addr     string
	stopped  bool
	// done is closed by the Serve goroutine on exit (after server.Serve
	// returns http.ErrServerClosed or any other error). Stop awaits done
	// before returning so callers can rely on "Stop returned == server
	// goroutine fully exited".
	done chan struct{}
	// serveErr captures the non-ErrServerClosed error (if any) from the
	// Serve goroutine. Read via ServeErr() after Stop returns.
	serveErr atomic.Pointer[error]
}

// MaxRequestBodyBytes caps the per-request body size cc may post. cc
// payloads are typically <100KB (tool inputs / outputs); 8 MiB gives
// 80x headroom while preventing unbounded memory growth from a
// misbehaving local client.
const MaxRequestBodyBytes = 8 << 20 // 8 MiB

// NewHookServer constructs a server with the given handlers. handlers may
// be nil to install no-op observers (every endpoint returns 204 with a
// safe-default decision); useful for early bring-up before daemon wiring.
func NewHookServer(h HookHandlers) *HookServer {
	if h == nil {
		h = noopHandlers{}
	}
	return &HookServer{handlers: h}
}

// Start binds the listener on 127.0.0.1:0 and launches the serving
// goroutine. Returns once the listener is ready (Addr is callable);
// error if the bind fails. Idempotent: re-Start of a stopped server
// returns ErrServerStopped.
func (s *HookServer) Start(_ context.Context) error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return ErrServerStopped
	}
	if s.listener != nil {
		s.mu.Unlock()
		return errors.New("hookserver: already started")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("hookserver: listen: %w", err)
	}
	s.listener = ln
	s.addr = ln.Addr().String()

	mux := http.NewServeMux()
	mux.HandleFunc("/hooks/pre-tool-use", s.handleDecision("PreToolUse", s.handlers.PreToolUse))
	mux.HandleFunc("/hooks/post-tool-use", s.handleObserver(s.handlers.PostToolUse))
	mux.HandleFunc("/hooks/session-start", s.handleObserver(s.handlers.SessionStart))
	mux.HandleFunc("/hooks/user-prompt-submit", s.handleDecision("UserPromptSubmit", s.handlers.UserPromptSubmit))
	mux.HandleFunc("/hooks/stop", s.handleObserver(s.handlers.Stop))
	mux.HandleFunc("/blob/", handleBlobNotImplemented)

	s.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	s.done = make(chan struct{})
	srv := s.server
	doneCh := s.done
	s.mu.Unlock()

	go func() {
		defer close(doneCh)
		err := srv.Serve(ln)
		// http.ErrServerClosed is the expected sentinel from a successful
		// Shutdown — never treat as failure. Any other error indicates the
		// listener died unexpectedly (fd exhaustion, kernel-killed, etc.).
		// Capture for ServeErr() so daemon health checks can detect.
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.serveErr.Store(&err)
		}
	}()
	return nil
}

// Addr returns the assigned 127.0.0.1:NNNN endpoint after Start. Empty
// string before Start or after Stop has cleared the listener.
func (s *HookServer) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Stop gracefully shuts down the server and waits for the Serve
// goroutine to exit. Returning means: (a) Shutdown completed (in-flight
// requests drained or ctx-canceled), AND (b) the goroutine that ran
// http.Server.Serve has fully exited. Callers can rely on this for
// teardown ordering. Idempotent: a stopped server's second Stop returns
// nil.
func (s *HookServer) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	srv := s.server
	doneCh := s.done
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	shutdownErr := srv.Shutdown(ctx)
	// Wait for Serve goroutine to exit. Shutdown returns when active
	// connections drain; the goroutine's `Serve` call returns slightly
	// after that. Bound the wait to ctx so a stuck Serve doesn't hang
	// us (would be a bug, but defense-in-depth).
	if doneCh != nil {
		select {
		case <-doneCh:
		case <-ctx.Done():
			// ctx exhausted; Shutdown error already captured. Goroutine
			// will eventually exit on its own; we lose the wait promise
			// here but Shutdown's error chain reflects the timeout.
		}
	}
	return shutdownErr
}

// ServeErr returns the non-ErrServerClosed error captured from the Serve
// goroutine, or nil. Useful for daemon health checks: if non-nil, the
// hook path is offline and supervisor should re-spawn / alert. Safe to
// call any time after Start.
func (s *HookServer) ServeErr() error {
	if e := s.serveErr.Load(); e != nil {
		return *e
	}
	return nil
}

// ErrServerStopped is returned when Start is called on a server that's
// already been Stop'd. The server is single-use; callers should construct
// a fresh HookServer rather than reuse.
var ErrServerStopped = errors.New("hookserver: server already stopped")

// --- handler glue ---------------------------------------------------

// handleDecision wraps a HookHandlers decision-style method into an
// http.HandlerFunc. Reads body, calls handler, renders cc's
// hookSpecificOutput envelope shape (spec §5.2 F-05).
func (s *HookServer) handleDecision(eventName string, fn func(context.Context, json.RawMessage) (HookResponse, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxRequestBodyBytes))
		if err != nil {
			if _, ok := err.(*http.MaxBytesError); ok {
				http.Error(w, "request body exceeds limit", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}
		resp, err := fn(r.Context(), body)
		if err != nil {
			http.Error(w, "handler: "+err.Error(), http.StatusInternalServerError)
			return
		}
		out := map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":            eventName,
				"permissionDecision":       resp.Decision,
				"permissionDecisionReason": resp.Reason,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}

// handleObserver wraps an observer HookHandlers method (no decision body).
// Returns 204 No Content on success; 500 on handler error.
func (s *HookServer) handleObserver(fn func(context.Context, json.RawMessage) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxRequestBodyBytes))
		if err != nil {
			if _, ok := err.(*http.MaxBytesError); !ok {
				http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			}
			return
		}
		if err := fn(r.Context(), body); err != nil {
			http.Error(w, "handler: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleBlobNotImplemented holds the /blob/* path namespace reserved for
// v0.2 Phase 1 (D-18 forward-compat seam, spec §5.2). Returns 501 so any
// caller hitting it knows the endpoint exists but is not yet implemented;
// reserving the path now prevents another handler from squatting later.
func handleBlobNotImplemented(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotImplemented)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":   "not_implemented",
		"reason":  "/blob/* is reserved for v0.2 Phase 1 (D-18); not implemented in v0.1",
		"path":    r.URL.Path,
	})
}

// --- noop handlers (default when handlers=nil) ---------------------

// noopHandlers is the safe-default fallback when NewHookServer is called
// with handlers=nil. Critically, the two decision hooks DENY rather than
// allow — this is fail-CLOSED security: a daemon that forgot to wire
// real handlers must NOT silently auto-approve every tool call. The deny
// reason surfaces clearly in cc's UI ("hook denied: …") so the
// misconfiguration is visible.
//
// For actual no-decision behavior in tests, construct a stub explicitly
// rather than relying on noopHandlers.
type noopHandlers struct{}

func (noopHandlers) PreToolUse(_ context.Context, _ json.RawMessage) (HookResponse, error) {
	return HookResponse{Decision: "deny", Reason: "tether handlers not configured (fail-closed default; wire HookHandlers to allow)"}, nil
}
func (noopHandlers) PostToolUse(_ context.Context, _ json.RawMessage) error      { return nil }
func (noopHandlers) SessionStart(_ context.Context, _ json.RawMessage) error     { return nil }
func (noopHandlers) UserPromptSubmit(_ context.Context, _ json.RawMessage) (HookResponse, error) {
	return HookResponse{Decision: "deny", Reason: "tether handlers not configured (fail-closed default; wire HookHandlers to allow)"}, nil
}
func (noopHandlers) Stop(_ context.Context, _ json.RawMessage) error             { return nil }
