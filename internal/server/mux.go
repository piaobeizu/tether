package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/quic-go/webtransport-go"

	"github.com/piaobeizu/tether/internal/auth"
	"github.com/piaobeizu/tether/internal/auth/apitoken"
	"github.com/piaobeizu/tether/internal/auth/oauth"
	mcplifecycle "github.com/piaobeizu/tether/internal/mcp/lifecycle"
	"github.com/piaobeizu/tether/internal/permission"
	"github.com/piaobeizu/tether/internal/session"
	"github.com/piaobeizu/tether/internal/skill"
	"github.com/piaobeizu/tether/internal/wire"
	"github.com/piaobeizu/tether/internal/workspace"
)

// buildMux constructs the shared route table used by both the TCP and UDP
// listeners. Routes per §10.B.4:
//
//	/               → SPA (embed.FS or dev proxy)
//	/cert-hash      → 64-char DER hash (wire.HashHex64)
//	/cert-hash-spki → 64-char SPKI hash
//	/api/v1/*       → REST API (stubs for s5+)
//	/wt/chat        → stream-json chat channel (s4)
//	/wt/shell       → PTY shell channel stub (s6)
//	/wt/events      → broadcast events channel (s4)
//	/wt/control     → client→server control channel: ping/pong RTT, action callbacks (tether#8 F1)
//	/wt/_smoke      → WT bidi pure-byte echo (D-22 §6 #2 acceptance gate)
func buildMux(cfg *Config, bundle CertBundle, wts *webtransport.Server, reg *session.Registry, pm *permission.Manager, authState *auth.State, mcpSrv *mcp.Server, mcpTokens *apitoken.Store, oauthH *oauth.Handlers, lm *mcplifecycle.LifecycleManager) http.Handler {
	mux := http.NewServeMux()

	derHex := HashHex(bundle.DER)
	spkiHex := HashHex(bundle.SPKI)

	// When using a CA-signed cert (--cert-file), do NOT serve the hash —
	// W3C serverCertificateHashes requires ≤14d validity + ECDSA P-256 +
	// self-signed. Letting the browser pin a hash for a CA cert that
	// violates these constraints causes Chrome to reject the WT connection
	// silently with QUIC_NETWORK_IDLE_TIMEOUT. With 404, wt.ts falls back
	// to standard CA validation (which works for any browser-trusted cert).
	mux.HandleFunc("/cert-hash", func(w http.ResponseWriter, r *http.Request) {
		if bundle.External {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, derHex)
	})
	mux.HandleFunc("/cert-hash-spki", func(w http.ResponseWriter, r *http.Request) {
		if bundle.External {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, spkiHex)
	})

	// WT smoke-test echo (D-22 §6 #2): pure byte echo, no prefix, no framing.
	mux.HandleFunc("/wt/_smoke", func(w http.ResponseWriter, r *http.Request) {
		sess, err := wts.Upgrade(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		go func() {
			defer sess.CloseWithError(0, "")
			stream, err := sess.AcceptStream(sess.Context())
			if err != nil {
				return
			}
			defer stream.Close()
			_, _ = io.Copy(stream, stream)
		}()
	})

	// s4: chat + events WT channels.
	mux.HandleFunc("/wt/chat", handleWTChat(reg, wts, authState))
	mux.HandleFunc("/wt/events", handleWTEvents(reg, wts, authState))

	// tether#8 F1: client→server control channel (ping/pong RTT, action callbacks).
	mux.HandleFunc("/wt/control", handleWTControl(reg, wts, authState))

	// s5: permission API (canonical + alias).
	broadcastFn := func(req *permission.Request) {
		reg.BroadcastAll(wire.Envelope{
			Kind:      wire.KindPermission,
			SessionID: wire.SessionID(req.SessionID),
			Payload: map[string]any{
				"id":       req.ID,
				"toolName": req.ToolName,
				"input":    req.Args,
			},
		})
	}
	permission.RegisterAPI(mux, pm, broadcastFn)

	// s6: shell WT channel + session lock API.
	mux.HandleFunc("/wt/shell", handleWTShell(reg, wts, authState))
	mux.HandleFunc("/api/v1/session/", handleLockForce(reg))

	// s7: workspace + skill REST APIs.
	if cfg.WsRegistry != nil {
		workspace.RegisterAPI(mux, cfg.WsRegistry)
	}
	if cfg.SkillRegistry != nil {
		skill.RegisterAPI(mux, cfg.SkillRegistry)
	}

	// /mcp on the HTTPS main port returns 501 until OAuth 2.1 lands in v0.4.
	// The real /mcp endpoint is served by MCPLoopback on the loopback HTTP port.

	mux.HandleFunc("/api/v1/providers", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(wire.ProviderListResponse{Providers: reg.Providers()}); err != nil {
			slog.Warn("providers: encode error", "err", err)
		}
	})

	// /mcp on HTTPS: served by MCPHTTPSHandler when store is configured (v0.3.2+).
	// One handler instance is shared for both patterns so the SDK's session map
	// is not split between /mcp and /mcp/ registrations.
	if mcpSrv != nil && mcpTokens != nil {
		mcpH := MCPHTTPSHandler(mcpSrv, mcpTokens, nil)
		mux.Handle("/mcp", mcpH)
		mux.Handle("/mcp/", mcpH)
		RegisterMCPTokensAPI(mux, mcpTokens, nil)
	}

	// OAuth 2.1 PKCE endpoints (v0.3.3). Auth middleware exempts these paths.
	if oauthH != nil {
		mux.Handle("/.well-known/oauth-authorization-server", oauthH.Metadata())
		mux.Handle("/oauth/authorize", oauthH.Authorize())
		mux.Handle("/oauth/token", oauthH.Token())
	}

	// v0.4: per-task MCP lifecycle endpoints.
	if lm != nil {
		RegisterTaskMCPAPI(mux, lm, pm)
	}

	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not implemented", http.StatusNotImplemented)
	})

	// /api/v1/auth/verify IS wrapped by authState.Middleware below; the
	// middleware lets it through via isExempt() — not by bypassing the wrapper.
	mux.HandleFunc("/api/v1/auth/verify", authState.VerifyHandler)
	mux.HandleFunc("/api/v1/auth/logout", authState.LogoutHandler)
	// /api/v1/auth/wt-ticket requires a valid session cookie (middleware enforces)
	// and issues a short-lived JWT for WebTransport connections.
	mux.HandleFunc("/api/v1/auth/wt-ticket", authState.WtTicketHandler)

	// Session history API.
	if reg.History != nil {
		listSessions, getMessages := sessionAPIHandlers(reg.History)
		mux.HandleFunc("/api/v1/sessions", listSessions)
		mux.HandleFunc("/api/v1/sessions/", getMessages)
	}

	mux.Handle("/", newStaticHandler(cfg.DevFrontendURL))

	// Wrap all routes: origin guard first, then auth middleware outermost.
	return authState.Middleware(WithOriginGuard(cfg.Port, mux))
}

// WithOriginGuard rejects non-safe-method requests (POST/PUT/PATCH/DELETE) whose
// Origin header is present but not in the daemon's allowlist. Requests without an
// Origin header (curl, other trusted clients) pass through unchanged.
func WithOriginGuard(port int, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			// safe methods: pass through unrestricted
		default:
			if origin := r.Header.Get("Origin"); origin != "" && !originAllowed(origin, port) {
				http.Error(w, "forbidden: origin not allowed", http.StatusForbidden)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}


// originAllowed returns true when origin matches one of the daemon's own HTTPS
// origins (127.0.0.1, localhost, or TETHER_HOST at the given port).
func originAllowed(origin string, port int) bool {
	portSuffix := fmt.Sprintf(":%d", port)
	hosts := []string{"127.0.0.1", "localhost"}
	if h := os.Getenv("TETHER_HOST"); h != "" {
		hosts = append(hosts, h)
	}
	for _, h := range hosts {
		if origin == "https://"+h+portSuffix {
			return true
		}
	}
	return false
}
