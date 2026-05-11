package server

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/piaobeizu/tether/internal/auth/apitoken"
)

type ipEntry struct {
	failures     int
	windowStart  time.Time
	blockedUntil time.Time
}

type ipLimiter struct {
	mu      sync.Mutex
	entries map[string]*ipEntry
}

func newIPLimiter() *ipLimiter {
	return &ipLimiter{entries: make(map[string]*ipEntry)}
}

// record is called on every Bearer auth failure for the given IP.
// Returns true if the IP is now rate-limited (429).
func (l *ipLimiter) record(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Sweep to bound map size under DDoS.
	if len(l.entries) > 10_000 {
		now := time.Now()
		for k, e := range l.entries {
			if now.After(e.blockedUntil) && now.After(e.windowStart.Add(60*time.Second)) {
				delete(l.entries, k)
			}
		}
	}

	e, ok := l.entries[ip]
	if !ok {
		e = &ipEntry{windowStart: time.Now()}
		l.entries[ip] = e
	}
	if time.Now().Before(e.blockedUntil) {
		return true
	}
	if time.Since(e.windowStart) > 60*time.Second {
		e.failures = 0
		e.windowStart = time.Now()
	}
	e.failures++
	if e.failures > 5 {
		e.blockedUntil = time.Now().Add(5 * time.Minute)
		return true
	}
	return false
}

// isBlocked returns true if the IP is currently in a block window.
func (l *ipLimiter) isBlocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[ip]
	if !ok {
		return false
	}
	return time.Now().Before(e.blockedUntil)
}

// MCPHTTPSHandler returns an http.Handler that validates a Bearer token
// against store before delegating to the shared mcp.Server's streamable
// HTTP handler. Pass nil logger to use slog.Default().
func MCPHTTPSHandler(mcpSrv *mcp.Server, store *apitoken.Store, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	limiter := newIPLimiter()
	var sdkHandler http.Handler
	if mcpSrv != nil {
		sdkHandler = mcp.NewStreamableHTTPHandler(
			func(_ *http.Request) *mcp.Server { return mcpSrv },
			&mcp.StreamableHTTPOptions{SessionTimeout: 30 * time.Minute},
		)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := newRequestID()
		ip := sanitiseAddr(r.RemoteAddr)

		if limiter.isBlocked(ip) {
			logger.Warn("mcp.bearer.rate_limited",
				"request_id", requestID,
				"remote_addr", ip)
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}

		raw, reason := extractBearer(r)
		if reason != "" {
			logger.Warn("mcp.apitoken.auth_failed",
				"request_id", requestID,
				"remote_addr", ip,
				"reason", reason)
			if limiter.record(ip) {
				logger.Warn("mcp.bearer.rate_limited", "remote_addr", ip)
				http.Error(w, "too many requests", http.StatusTooManyRequests)
				return
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		id, name, ok := store.LookupByRaw(raw)
		if !ok {
			logger.Warn("mcp.apitoken.auth_failed",
				"request_id", requestID,
				"remote_addr", ip,
				"reason", "unknown_token")
			if limiter.record(ip) {
				logger.Warn("mcp.bearer.rate_limited", "remote_addr", ip)
				http.Error(w, "too many requests", http.StatusTooManyRequests)
				return
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		logger.Info("mcp.apitoken.auth_ok",
			"request_id", requestID,
			"id", id,
			"name", name,
			"remote_addr", ip)
		if sdkHandler != nil {
			sdkHandler.ServeHTTP(w, r)
		}
	})
}

func extractBearer(r *http.Request) (raw string, reason string) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", "missing_header"
	}
	if !strings.HasPrefix(h, "Bearer ") {
		return "", "bad_scheme"
	}
	raw = strings.TrimPrefix(h, "Bearer ")
	if raw == "" {
		return "", "bad_scheme"
	}
	return raw, ""
}

func sanitiseAddr(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	if i := strings.LastIndex(addr, ":"); i > 0 && !strings.Contains(addr, "]") {
		return addr[:i]
	}
	return addr
}

func newRequestID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic("mcp_https: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}
