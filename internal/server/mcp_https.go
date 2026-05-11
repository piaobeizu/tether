package server

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/piaobeizu/tether/internal/auth/apitoken"
)

// MCPHTTPSHandler returns an http.Handler that validates a Bearer token
// against store before delegating to the shared mcp.Server's streamable
// HTTP handler. Pass nil logger to use slog.Default().
func MCPHTTPSHandler(mcpSrv *mcp.Server, store *apitoken.Store, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	sdkHandler := mcp.NewStreamableHTTPHandler(
		func(_ *http.Request) *mcp.Server { return mcpSrv },
		&mcp.StreamableHTTPOptions{SessionTimeout: 30 * time.Minute},
	)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := newRequestID()
		raw, reason := extractBearer(r)
		if reason != "" {
			logger.Warn("mcp.apitoken.auth_failed",
				"request_id", requestID,
				"remote_addr", sanitiseAddr(r.RemoteAddr),
				"reason", reason)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		id, name, ok := store.LookupByRaw(raw)
		if !ok {
			logger.Warn("mcp.apitoken.auth_failed",
				"request_id", requestID,
				"remote_addr", sanitiseAddr(r.RemoteAddr),
				"reason", "unknown_token")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		logger.Info("mcp.apitoken.auth_ok",
			"request_id", requestID,
			"id", id,
			"name", name,
			"remote_addr", sanitiseAddr(r.RemoteAddr))
		sdkHandler.ServeHTTP(w, r)
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
