package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/agent/permhook"
	"github.com/piaobeizu/tether/internal/session"
	"github.com/piaobeizu/tether/internal/skill"
	"github.com/piaobeizu/tether/internal/workspace"
)

// Config holds all server startup parameters.
type Config struct {
	Port           int
	CertFile       string // empty = use managed cert at ~/.tether/cert.pem
	KeyFile        string
	AcmeDomain     string // if set, obtain cert via ACME/Let's Encrypt (port 80 required)
	AcmeEmail      string // contact email for ACME registration
	DevMode        bool   // if true, proxy SPA to DevFrontendURL
	DevFrontendURL string // default http://localhost:5173 when DevMode=true
	Registry       *session.Registry
	WsRegistry     *workspace.Registry
	SkillRegistry  *skill.Registry
	acmeTLSBase    *tls.Config // populated by Run() when AcmeDomain is active
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
	// Step 1: resolve cc binary path + build session registry.
	ccPath := resolveClaudePath()
	if cfg.Registry == nil {
		provider := agent.NewClaudeCodeProvider(ccPath)
		cfg.Registry = session.NewRegistry(provider)
	}

	// Step 2: ensure ~/.tether/ data dirs.
	if _, err := tetherDataDir(); err != nil {
		return err
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
	if cfg.SkillRegistry == nil {
		skReg, err := skill.NewRegistry()
		if err != nil {
			slog.Warn("skill registry init failed", "err", err)
		} else {
			cfg.SkillRegistry = skReg
		}
	}

	// Step 3: permission hook setup (D-05b §4–§5).
	ps := NewPermState()
	noHook := os.Getenv("TETHER_NO_PERMISSION_HOOK") == "1"
	if !noHook {
		binPath := filepath.Join(binDir, "tether-permission-hook")
		if err := permhook.EnsureHookBinary(binPath); err != nil {
			return fmt.Errorf("perm hook compile: %w", err)
		}
		permEndpoint := fmt.Sprintf("https://127.0.0.1%s/api/v1/agent/permission/request", cfg.addr())
		if err := agent.InjectPermHook(binPath, permEndpoint); err != nil {
			slog.Warn("inject perm hook failed", "err", err)
		} else {
			cfg.Registry.PermEndpoint = permEndpoint
		}
	}

	// Step 4: load or generate cert.
	bundle, err := LoadOrGenCert(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return fmt.Errorf("cert: %w", err)
	}

	// Step 4b: ACME override — when --acme-domain is set, certmagic obtains and
	// auto-renews a Let's Encrypt cert (HTTP-01; port 80 must be reachable).
	if cfg.AcmeDomain != "" {
		slog.Info("obtaining ACME cert", "domain", cfg.AcmeDomain)
		acmeTLS, acmeBundle, err := SetupACME(context.Background(), cfg.AcmeDomain, cfg.AcmeEmail)
		if err != nil {
			return fmt.Errorf("ACME setup: %w", err)
		}
		bundle = acmeBundle
		cfg.acmeTLSBase = acmeTLS
		slog.Info("ACME cert ready", "domain", cfg.AcmeDomain)
	}

	// Resolve effective frontend URL for dev mode.
	cfg.DevFrontendURL = cfg.devFrontend()

	// Step 5: build and start listeners.
	srv := newServer(cfg, bundle, ps)

	errCh := make(chan error, 2)

	go func() {
		// TCP ListenAndServeTLS with empty cert/key paths uses TLSConfig.Certificates.
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
