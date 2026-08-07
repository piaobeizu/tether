package server

import (
	"context"
	crand "crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/aihub"
	"github.com/piaobeizu/tether/internal/auth"
	"github.com/piaobeizu/tether/internal/auth/apitoken"
	"github.com/piaobeizu/tether/internal/auth/oauth"
	"github.com/piaobeizu/tether/internal/mcp/builtin"
	mcpgw "github.com/piaobeizu/tether/internal/mcp/gateway"
	mcphost "github.com/piaobeizu/tether/internal/mcp/host"
	mcplifecycle "github.com/piaobeizu/tether/internal/mcp/lifecycle"
	mcpreg "github.com/piaobeizu/tether/internal/mcp/registry"
	"github.com/piaobeizu/tether/internal/permission"
	"github.com/piaobeizu/tether/internal/permission/cchook"
	"github.com/piaobeizu/tether/internal/session"
	"github.com/piaobeizu/tether/internal/skill"
	"github.com/piaobeizu/tether/internal/workspace"
	"github.com/piaobeizu/tether/web"
)

// Config holds all server startup parameters.
type Config struct {
	Port           int
	CertFile       string // empty = use managed cert at ~/.tether/cert.pem
	KeyFile        string
	AcmeDomain     string // if set, obtain cert via ACME/Let's Encrypt (TLS-ALPN-01 on :443; port 80 unused)
	AcmeEmail      string // contact email for ACME registration
	DevMode        bool   // if true, proxy SPA to DevFrontendURL
	DevFrontendURL string // default http://localhost:5173 when DevMode=true
	Token          string // static access token; empty = auto-generate from ~/.tether/access-token
	Version        string // what GET /api/v1/version reports; see wire.VersionResponse
	Registry       *session.Registry
	WsRegistry     *workspace.Registry
	SkillRegistry  *skill.Registry
	acmeTLSBase    *tls.Config // populated by Run() when AcmeDomain is active

	// AihubClient backs the curated /api/v1/work/* proxy (Task A2). If nil
	// when Run() starts, Run() attempts aihub.LoadConfig() and constructs
	// one; if that also fails (no env vars, no ~/.polyforge/config.toml),
	// it is left nil and RegisterWorkAPI (mux.go) answers 503 for every
	// /api/v1/work/* route rather than failing startup.
	AihubClient *aihub.Client

	// MCP fields (v0.3.1)
	MCPPort       int    // loopback port; 0 = default 8899
	MCPConfigPath string // path to [mcp.servers] config; "" = ~/.tether/config.json
	WorkspaceRoot string // builtin tools workspace root; "" = ~/.tether/workspace
	SkipMCPInject bool   // skip ~/.claude/settings.json injection (CI/test)

	// v0.3.2: external client API token store
	APITokensPath string // path to api-tokens.json; "" = ~/.tether/api-tokens.json

	// v0.4: per-task MCP lifecycle manager.
	// If nil, Run() initialises one and stores it here.
	MCPLifecycle *mcplifecycle.LifecycleManager
}

func (c *Config) addr() string { return fmt.Sprintf(":%d", c.Port) }

func (c *Config) devFrontend() string {
	if !c.DevMode {
		return ""
	}
	if c.DevFrontendURL != "" {
		return c.DevFrontendURL
	}
	return "http://localhost:5173"
}

// Run executes the §10.A.4 startup sequence, blocks until SIGINT/SIGTERM,
// then performs graceful shutdown (≤5s per K.1.5).
func Run(cfg *Config) error {
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()

	// Step 1: resolve cc binary path + build session registry.
	ccPath := resolveClaudePath()
	if cfg.Registry == nil {
		ccProvider := agent.NewClaudeCodeProvider(ccPath)
		ocProvider := agent.NewOpenCodeProvider()
		cfg.Registry = session.NewRegistry(ccProvider, ocProvider)
	}

	// Step 2: ensure ~/.tether/ data dirs.
	if _, err := tetherDataDir(); err != nil {
		return err
	}

	// Step 2a: wire up session history store + the per-session workspace bindings
	// that share its directory (tether#52). The bindings are what let a reconnect
	// carrying only a sid land back in that session's own workspace — including
	// after a daemon restart, which is the whole reason they are on disk.
	if cfg.Registry.History == nil || cfg.Registry.Bindings == nil {
		home, err := os.UserHomeDir()
		if err == nil {
			sessDir := filepath.Join(home, ".tether", "sessions")
			if cfg.Registry.History == nil {
				cfg.Registry.History = session.NewHistoryStore(sessDir)
			}
			if cfg.Registry.Bindings == nil {
				cfg.Registry.Bindings = session.NewBindingStore(sessDir)
			}
		}
	}
	binDir, err := tetherBinDir()
	if err != nil {
		return err
	}

	// Step 2b: workspace + skill registries.
	if cfg.WsRegistry == nil {
		wsReg, err := workspace.NewRegistry()
		if err != nil {
			slog.Warn("workspace registry init failed", "err", err)
		} else {
			cfg.WsRegistry = wsReg
		}
	}
	// Let the session registry resolve a chat connection's `?ws=` id against the
	// user's workspace list (tether#52). Guarded on non-nil rather than assigned
	// unconditionally: a nil *workspace.Registry stored in the interface would be a
	// NON-nil interface holding a nil pointer, so the "no registry, refuse the
	// request" branch in resolveWorkspace would be skipped in favour of a nil-
	// receiver call. Leaving the field nil when the registry failed to load is what
	// makes that failure refuse workspace requests instead of panicking on one.
	if cfg.WsRegistry != nil {
		cfg.Registry.Workspaces = cfg.WsRegistry
	}
	if cfg.SkillRegistry == nil {
		skReg, err := skill.NewRegistry()
		if err != nil {
			slog.Warn("skill registry init failed", "err", err)
		} else {
			cfg.SkillRegistry = skReg
		}
	}

	// Step 2c: aihub client for the curated /api/v1/work/* proxy (Task A2).
	// Best-effort: LoadConfig looks at TETHER_AIHUB_URL/KEY, then falls back
	// to ~/.polyforge/config.toml. A missing/incomplete config leaves
	// cfg.AihubClient nil rather than failing startup — RegisterWorkAPI
	// (mux.go) answers 503 for every /api/v1/work/* route in that case.
	if cfg.AihubClient == nil {
		if baseURL, key, ok := aihub.LoadConfig(); ok {
			cfg.AihubClient = aihub.New(baseURL, key)
		} else {
			slog.Warn("aihub not configured; /api/v1/work/* will answer 503")
		}
	}

	// Step 3: permission hook setup (D-05b §4–§5).
	pm := permission.New()
	noHook := os.Getenv("TETHER_NO_PERMISSION_HOOK") == "1"
	if !noHook {
		binPath := filepath.Join(binDir, "tether-permission-hook")
		if err := cchook.EnsureHookBinary(binPath); err != nil {
			return fmt.Errorf("perm hook compile: %w", err)
		}
		permEndpoint := fmt.Sprintf("https://127.0.0.1%s/api/v1/permission/request", cfg.addr())
		if err := agent.InjectPermHook(binPath, permEndpoint); err != nil {
			slog.Warn("inject perm hook failed", "err", err)
		} else {
			cfg.Registry.PermEndpoint = permEndpoint
		}
	}

	// Step 3b: MCP host + loopback (v0.3.1).
	mcpPort := cfg.MCPPort
	if mcpPort == 0 {
		mcpPort = 8899
	}
	wsRoot := cfg.WorkspaceRoot
	if wsRoot == "" {
		home, _ := os.UserHomeDir()
		wsRoot = filepath.Join(home, ".tether", "workspace")
		_ = os.MkdirAll(wsRoot, 0o700)
	}
	// Reflect the resolved default back onto cfg so RegisterWorkAPI (mux.go,
	// invoked later via newServer/buildMux) sees the real workspace root
	// instead of an empty string when --workspace-root wasn't passed
	// (tether#20 Task 5: the scenario-graph resolver needs a real root).
	cfg.WorkspaceRoot = wsRoot
	// The session Registry is built in Step 1, before wsRoot is known, so its
	// Workdir (the cwd of every agent subprocess — chat provider AND the PTY
	// shell pane) is wired here instead, same shape as cfg.Registry.PermEndpoint
	// in Step 3 (tether#51). Only fill it in when unset, so an embedder that
	// pre-built its own Registry keeps control (cf. the nil-guards on
	// cfg.Registry / cfg.Registry.History above).
	if cfg.Registry.Workdir == "" {
		cfg.Registry.Workdir = wsRoot
	}
	mcpCfg, err := loadMCPConfig(cfg.MCPConfigPath)
	if err != nil {
		return fmt.Errorf("mcp config: %w", err)
	}
	mcpReg := mcpreg.New()
	mcpMgr := mcphost.NewManager(mcpReg, mcphost.NoopLogger())
	if err := mcpMgr.Start(context.Background(), mcpCfg); err != nil {
		return fmt.Errorf("mcp manager: %w", err)
	}
	builtins, err := builtin.New(wsRoot)
	if err != nil {
		return fmt.Errorf("mcp builtin init: %w", err)
	}
	mcpGW := mcpgw.New(mcpMgr, mcpReg, pm, mcphost.NoopLogger())
	mcpSrv := BuildMCPServer(mcpGW, builtins, mcpReg)
	bearerToken, err := generateBearerToken()
	if err != nil {
		return fmt.Errorf("bearer token: %w", err)
	}
	// Persist token to ~/.tether/mcp-token (0600) for debugging/scripting.
	tetherDir, err := tetherDataDir() // already called in Step 2; safe to call again (idempotent)
	if err != nil {
		return fmt.Errorf("tether data dir: %w", err)
	}
	mcpTokenPath := filepath.Join(tetherDir, "mcp-token")
	if err := os.WriteFile(mcpTokenPath, []byte(bearerToken), 0o600); err != nil {
		slog.Warn("mcp: could not write token file", "path", mcpTokenPath, "err", err)
	}

	loopback := NewMCPLoopback(mcpPort, mcpSrv, bearerToken)
	if err := loopback.Start(); err != nil {
		return fmt.Errorf("mcp loopback: %w", err)
	}
	if !cfg.SkipMCPInject {
		if err := agent.InjectMCPServer(mcpPort, bearerToken, "tether"); err != nil {
			mcpMgr.StopAll()
			_ = loopback.Stop(context.Background())
			return fmt.Errorf("mcp settings inject: %w", err)
		}
	}

	// Step 3c: per-task MCP lifecycle manager (v0.4) + idle watchdog (v0.5).
	// The watchdog hibernates task instances idle past TETHER_MCP_IDLE_TIMEOUT
	// (default 15m; set "0" to disable); a hibernated instance is revived on its
	// next tool call.
	if cfg.MCPLifecycle == nil {
		idleTimeout := 15 * time.Minute
		if v := os.Getenv("TETHER_MCP_IDLE_TIMEOUT"); v != "" {
			switch d, err := time.ParseDuration(v); {
			case err != nil:
				slog.Warn("invalid TETHER_MCP_IDLE_TIMEOUT; using default", "value", v, "default", idleTimeout)
			case d < 0:
				slog.Warn("negative TETHER_MCP_IDLE_TIMEOUT disables the idle watchdog", "value", v)
				idleTimeout = d // <= 0 disables the watchdog in WithIdleWatchdog
			default:
				idleTimeout = d
			}
		}
		cfg.MCPLifecycle = mcplifecycle.New(mcplifecycle.WithIdleWatchdog(idleTimeout, 0))
	}

	// Step 4: load or generate cert.
	bundle, err := LoadOrGenCert(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return fmt.Errorf("cert: %w", err)
	}

	// Step 4b: ACME override — when --acme-domain is set, certmagic obtains and
	// auto-renews a Let's Encrypt cert (TLS-ALPN-01 over :443; port 80 unused).
	// Running before the listeners bind is what lets certmagic's solver take
	// :443 for the initial challenge; renewal later relies on the ALPN list in
	// server.go's makeTLS instead. See SetupACME and applyACME.
	if cfg.AcmeDomain != "" {
		acmeBundle, err := applyACME(context.Background(), cfg, SetupACME)
		if err != nil {
			return fmt.Errorf("ACME setup: %w", err)
		}
		bundle = acmeBundle
	}

	// Resolve effective frontend URL for dev mode.
	cfg.DevFrontendURL = cfg.devFrontend()

	// Step 4c: auth state.
	accessToken, tokenSource, err := auth.LoadOrGenToken(cfg.Token)
	if err != nil {
		return fmt.Errorf("auth token: %w", err)
	}
	// Deliberately logs the source label only, never the token value — lets
	// an operator (and live-verification) confirm which credential path
	// (flag/env/file/generated) is actually energised. The key is
	// "token_source" rather than "source" because slog reserves the latter
	// for call-site information under AddSource.
	slog.Info("auth: access token loaded", "token_source", string(tokenSource))
	// An operator-supplied token is the one tether does not control the
	// strength of, and /api/v1/auth/verify has no rate limit while the
	// listener binds every interface. Warn on an obviously weak one — the
	// generated tokens are 64 hex chars, so this never fires for them.
	// Reports neither the value nor its exact length.
	if (tokenSource == auth.TokenSourceFlag || tokenSource == auth.TokenSourceEnv) && len(accessToken) < 32 {
		slog.Warn("auth: supplied access token is short; prefer `openssl rand -hex 32`",
			"token_source", string(tokenSource))
	}
	jwtSecret, err := auth.LoadOrGenSecret()
	if err != nil {
		return fmt.Errorf("auth secret: %w", err)
	}
	authState := auth.NewState(accessToken, jwtSecret)
	// Production: decide static-asset cookie exemption by real-file existence
	// in the embedded dist (fail-closed; tether#17). In dev mode the SPA is
	// reverse-proxied to Vite, so the embedded dist doesn't describe live
	// assets — leave staticFS unset and fall back to the suffix heuristic.
	if cfg.DevFrontendURL == "" {
		if distSub, err := fs.Sub(web.DistFS, "dist"); err == nil {
			authState = authState.WithStaticFS(distSub)
		} else {
			slog.Warn("auth: embedded dist sub-FS failed; static exemption uses suffix heuristic", "err", err)
		}
	}

	// Step 4d: open API token store for external MCP clients (v0.3.2).
	apiTokensPath := cfg.APITokensPath
	if apiTokensPath == "" {
		apiTokensPath = filepath.Join(tetherDir, "api-tokens.json")
	}
	apiTokens, err := apitoken.Open(apiTokensPath)
	if err != nil {
		return fmt.Errorf("api-tokens store: %w", err)
	}
	apiTokens.StartEviction(runCtx)

	// Step 4e: OAuth 2.1 PKCE handlers (v0.3.3).
	oauthHost := "127.0.0.1"
	if h := os.Getenv("TETHER_HOST"); h != "" {
		oauthHost = h
	}
	var oauthIssuer string
	if cfg.Port == 443 {
		oauthIssuer = "https://" + oauthHost
	} else {
		oauthIssuer = fmt.Sprintf("https://%s:%d", oauthHost, cfg.Port)
	}
	oauthCS := oauth.NewCodeStore()
	oauthH := oauth.NewHandlers(oauthCS, apiTokens, oauthIssuer)

	// Step 5: build and start listeners.
	certs := newCertHolder(bundle)
	srv := newServer(cfg, certs, pm, authState, mcpSrv, apiTokens, oauthH)

	// A managed cert lives 14 days and this daemon is meant to stay up for
	// weeks, but LoadOrGenCert above only runs on the way in — so without this
	// loop the cert simply expires underneath a running process, and the
	// symptom (browser refuses the connection) points nowhere near the cause.
	// See startCertRotation for what it deliberately does not cover.
	startCertRotation(runCtx, bundle, certs, certRotateInterval, loadOrRotateManaged)

	errCh := make(chan error, 2)

	go func() {
		// Empty cert/key paths are fine: net/http skips loading from disk when
		// the TLSConfig can already produce a cert, and GetCertificate counts
		// (see http.Server.ServeTLS's configHasCert). The managed path sets
		// only GetCertificate so rotation is visible per handshake.
		if err := srv.tcp.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("TCP: %w", err)
		}
	}()

	go func() {
		if err := srv.wts.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("UDP/WT: %w", err)
		}
	}()

	// Brief pause so listeners bind before logging.
	time.Sleep(50 * time.Millisecond)

	host := "127.0.0.1"
	if h := os.Getenv("TETHER_HOST"); h != "" {
		host = h
	}
	slog.Info("✓ tether server up", "url", fmt.Sprintf("https://%s%s/", host, cfg.addr()))
	slog.Info("claude binary", "path", ccPath)
	if !bundle.External {
		slog.Info("cert DER hash", "hash", HashHex(bundle.DER))
	} else if cfg.AcmeDomain != "" {
		slog.Info("cert mode", "acme", cfg.AcmeDomain)
	}

	// Step 6: block until signal or listener error.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return err
	case sig := <-sigCh:
		slog.Info("shutting down", "signal", sig)
	}

	// Step 6: graceful shutdown (≤5s).
	if !noHook {
		_ = agent.RemovePermHook()
	}

	// MCP shutdown: drain → stop children → housekeeping.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()

	// v0.4: stop per-task instances before global stack.
	cfg.MCPLifecycle.StopAll(drainCtx)

	_ = loopback.Stop(drainCtx)
	mcpMgr.StopAll()
	if !cfg.SkipMCPInject {
		_ = agent.RemoveMCPServer("tether")
	}
	_ = os.Remove(mcpTokenPath)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = srv.tcp.Shutdown(ctx)
	_ = srv.h3.Close()
	return nil
}

func tetherBinDir() (string, error) {
	dir, err := tetherDataDir()
	if err != nil {
		return "", err
	}
	binDir := dir + "/bin"
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir ~/.tether/bin: %w", err)
	}
	return binDir, nil
}

// loadMCPConfig reads the [mcp] section from a tether config file.
// flagPath overrides the default ~/.tether/config.json.
// Returns empty config (no servers) if the file is absent.
func loadMCPConfig(flagPath string) (*mcphost.Config, error) {
	path := flagPath
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(home, ".tether", "config.json")
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &mcphost.Config{Servers: map[string]mcphost.ServerConfig{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("loadMCPConfig: %w", err)
	}
	var wrapper struct {
		MCP *mcphost.Config `json:"mcp"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return nil, fmt.Errorf("loadMCPConfig parse: %w", err)
	}
	if wrapper.MCP == nil {
		return &mcphost.Config{Servers: map[string]mcphost.ServerConfig{}}, nil
	}
	return wrapper.MCP, nil
}

// generateBearerToken returns a cryptographically random 32-byte hex token.
func generateBearerToken() (string, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(crand.Reader, b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
