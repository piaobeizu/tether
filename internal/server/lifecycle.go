package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/session"
)

// Config holds all server startup parameters.
type Config struct {
	Port           int
	CertFile       string // empty = use managed cert at ~/.tether/cert.pem
	KeyFile        string
	DevMode        bool   // if true, proxy SPA to DevFrontendURL
	DevFrontendURL string // default http://localhost:5173 when DevMode=true
	Registry       *session.Registry
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
	_ = binDir

	// Step 3: load or generate cert.
	bundle, err := LoadOrGenCert(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return fmt.Errorf("cert: %w", err)
	}

	// Resolve effective frontend URL for dev mode.
	cfg.DevFrontendURL = cfg.devFrontend()

	// Step 4: build and start listeners.
	srv := newServer(cfg, bundle)

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
	slog.Info("cert DER hash", "hash", HashHex(bundle.DER))

	// Step 5: block until signal or listener error.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return err
	case sig := <-sigCh:
		slog.Info("shutting down", "signal", sig)
	}

	// Step 6: graceful shutdown (≤5s).
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
