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

// handleCertHash serves one of the certificate fingerprints as 64 hex chars.
//
// The hash is read from the holder per request rather than precomputed: the
// browser pins whatever this endpoint returns, so after a rotation it has to
// describe the cert the listener is actually presenting. A value captured at
// startup — which is what this replaced — would keep handing out the hash of a
// cert that is no longer served, and the WT handshake would fail outright.
// That is a worse failure than the expiry the rotation exists to prevent, so
// the hash and the served cert have to move together.
//
// When using a CA-signed cert (--cert-file or ACME), do NOT serve the hash —
// W3C serverCertificateHashes requires ≤14d validity + ECDSA P-256 +
// self-signed. Letting the browser pin a hash for a CA cert that violates
// these constraints causes Chrome to reject the WT connection silently with
// QUIC_NETWORK_IDLE_TIMEOUT. With 404, wt.ts falls back to standard CA
// validation (which works for any browser-trusted cert).
func handleCertHash(certs *certHolder, pick func(CertBundle) [32]byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b := certs.Get()
		if b.External {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		// This value used to be fixed for the life of the process; now it
		// changes when the cert rotates. A cached copy would be the hash of a
		// cert the server no longer presents, which is exactly the failure the
		// per-request read exists to avoid.
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprintln(w, HashHex(pick(b)))
	}
}

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
func buildMux(cfg *Config, certs *certHolder, wts *webtransport.Server, reg *session.Registry, pm *permission.Manager, authState *auth.State, mcpSrv *mcp.Server, mcpTokens *apitoken.Store, oauthH *oauth.Handlers, lm *mcplifecycle.LifecycleManager) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/cert-hash", handleCertHash(certs, func(b CertBundle) [32]byte { return b.DER }))
	mux.HandleFunc("/cert-hash-spki", handleCertHash(certs, func(b CertBundle) [32]byte { return b.SPKI }))

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

	// Task A2: curated read-only aihub work-item proxy. Registered
	// unconditionally (even with a nil client) so /api/v1/work/* answers
	// 503 "aihub not configured" itself, in the same auth-gated group as
	// the workspace/skill APIs above, instead of falling through to the
	// generic /api/v1/ 501 stub registered below.
	RegisterWorkAPI(mux, cfg.AihubClient, cfg.WorkspaceRoot)

	// /mcp on the HTTPS main port returns 501 until OAuth 2.1 lands in v0.4.
	// The real /mcp endpoint is served by MCPLoopback on the loopback HTTP port.

	mux.HandleFunc("/api/v1/providers", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(wire.ProviderListResponse{Providers: reg.Providers()}); err != nil {
			slog.Warn("providers: encode error", "err", err)
		}
	})

	// The UI asks for the version instead of carrying its own copy, so the
	// number on screen is always this binary's (see wire.VersionResponse).
	mux.HandleFunc("/api/v1/version", handleVersion(cfg.Version))

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

	// Session API: the list, each session's transcript, and each session's work
	// item. Gated on reg.History, which since tether#92 is a stricter condition
	// than the reason originally given for it: a daemon with history disabled
	// could now serve a perfectly good list out of cc's store alone. Left as is
	// deliberately — "history disabled" is not a configuration this daemon
	// actually offers (lifecycle.go always builds one), so relaxing it would add
	// an untested path to buy nothing. The index carries the other stores as
	// optional extras, so a daemon without them serves a thinner list rather than
	// none.
	if reg.History != nil {
		idx := &session.SessionIndex{
			History:  reg.History,
			WI:       cfg.WIBindings,
			Bindings: reg.Bindings,
			// The store built in lifecycle.go, not a second one. Registry.hadConversation
			// is the other consumer and it has to see the same directories, or a
			// session can be listed here and unknown there (tether#92).
			CC: reg.CC,
			// Likewise the live-session registry (tether#101). Registry.ccLiveJob is the
			// other consumer, and it is the AUTHORITATIVE one: this list only marks a
			// row, while that decides whether a resume is refused. Two instances would
			// let the list mark a row the attach path then resumes without complaint.
			CCJobs: reg.CCJobs,
		}
		listSessions, sessionSub := sessionAPIHandlers(idx, cfg.WIBindings)
		mux.HandleFunc("/api/v1/sessions", listSessions)
		mux.HandleFunc("/api/v1/sessions/", sessionSub)
	}

	// Session activity (tether#103): which sessions have a turn in flight right
	// now. Registered UNCONDITIONALLY, unlike the list above, and the difference is
	// deliberate — this answer needs neither a history store nor a transcript, so
	// gating it on reg.History would switch the marker off for a daemon that can
	// still answer it perfectly well. With both readers absent it serves `{}`,
	// which is the honest answer and the one the SPA already handles.
	//
	// The two readers are the SAME instances the rest of the daemon uses, for the
	// reason SessionIndex.CCJobs gives: two instances are two answers, and the
	// symptom is a marker that contradicts the transcript on screen.
	//
	// The path is its own TOP-LEVEL route rather than a leaf under
	// /api/v1/sessions/ — see session.SessionActivityPath for why, including the
	// neighbour that actually is a hazard: /api/v1/session/ (singular,
	// handleLockForce above) is a PREFIX handler one hyphen away from this path.
	mux.HandleFunc(session.SessionActivityPath, handleSessionActivity(&session.ActivityIndex{
		Reg:    reg,
		CCJobs: reg.CCJobs,
	}))

	mux.Handle("/", newStaticHandler(cfg.DevFrontendURL))

	// Wrap all routes: origin guard first, then auth middleware outermost.
	return authState.Middleware(WithOriginGuard(cfg.Port, mux))
}

// ccWorkdirs reports the directories tether has positive evidence the user works
// in, which is the whitelist session.CCStore looks for cc transcripts under
// (tether#92).
//
// # Why a whitelist and not "everything cc has"
//
// cc files a transcript under a directory named for the cwd it ran in, and on a
// working machine most of those directories are not workspaces: the reference
// profile had 37 of them, of which 21 were throwaway job and probe directories
// and 2 were the user's actual working trees. Listing all of them would replace
// an almost-empty list with an unreadable one.
//
// # Why a closure and not a slice
//
// Workspaces can be added and removed while the daemon runs. A slice captured
// here would answer from startup's snapshot forever, and the symptom — a
// workspace you just added has no sessions until you restart — is the kind that
// gets reported as "the list is broken" months later.
//
// Registry.Workdir comes first because it is where a session that selected no
// workspace actually runs (the resolved --workspace-root), and that is the
// majority case for sessions started from a terminal.
func ccWorkdirs(cfg *Config, reg *session.Registry) func() []string {
	return func() []string {
		var out []string
		if reg.Workdir != "" {
			out = append(out, reg.Workdir)
		}
		if cfg.WsRegistry != nil {
			for _, ws := range cfg.WsRegistry.List() {
				if ws.Path != "" {
					out = append(out, ws.Path)
				}
			}
		}
		return out
	}
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
//
// Browsers omit the port for the default HTTPS port (443), so a page served
// from a :443 deployment sends same-origin requests with Origin "https://<host>"
// (no ":443"). Accept both the explicit-port and the bare form on 443, mirroring
// the OAuth-issuer normalization in Run() (lifecycle.go). Without this, every
// non-safe-method request (login, wt-ticket) is rejected 403 on a :443 host.
// handleVersion serves GET /api/v1/version with the version it is given.
//
// Taking the string as an argument rather than reading a package global is what
// makes the "does the value the daemon resolved actually reach the wire" hop
// testable — the hop that, left unguarded, is how the UI ended up displaying a
// number nobody had updated (tether#70).
//
// An empty version is reported verbatim as "": the UI shows a placeholder for
// it, which is honest, whereas substituting a plausible default here would
// recreate exactly the bug being fixed.
func handleVersion(version string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(wire.VersionResponse{Version: version}); err != nil {
			slog.Warn("version: encode error", "err", err)
		}
	}
}

func originAllowed(origin string, port int) bool {
	suffixes := []string{fmt.Sprintf(":%d", port)}
	if port == 443 {
		suffixes = append(suffixes, "")
	}
	hosts := []string{"127.0.0.1", "localhost"}
	if h := os.Getenv("TETHER_HOST"); h != "" {
		hosts = append(hosts, h)
	}
	for _, h := range hosts {
		for _, suffix := range suffixes {
			if origin == "https://"+h+suffix {
				return true
			}
		}
	}
	return false
}
