//go:build liveacme

// This file is excluded from the normal build. It exists because tether#79 was
// a bug that every unit test in this package could have been written around:
// --acme-domain issued a certificate fine and then failed every renewal for the
// rest of the daemon's life, and nothing short of a real ACME exchange proves
// that is fixed.
//
// Run it with a local ACME server (Let's Encrypt's own test CA), never against
// a real one — the failure modes here burn issuance rate limits:
//
//	go install github.com/letsencrypt/pebble/v2/cmd/pebble@latest
//	GOWORK=off go test -tags liveacme -count=1 -race ./internal/server/ -run TestLiveACME -v
//
// Override the binary and its test certificates with TETHER_PEBBLE_BIN and
// TETHER_PEBBLE_CERTS if they are not on the default paths.
//
// Why a local CA rather than Let's Encrypt staging: RFC 8737 pins TLS-ALPN-01
// validation to port 443, and on any machine already running tether that port
// is taken — which is precisely the confusion that produced the wrong diagnosis
// this wi started from. Pebble's tlsPort setting moves validation to a port of
// our choosing, so the whole exchange runs beside a live daemon instead of
// fighting it.
package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"go/build"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/mholt/acmez/v3"
	mcplifecycle "github.com/piaobeizu/tether/internal/mcp/lifecycle"
	"github.com/piaobeizu/tether/internal/session"
)

const (
	livePebbleDir  = "https://127.0.0.1:14000/dir"
	livePebbleMgmt = "127.0.0.1:15000"
	livePebbleAPI  = "127.0.0.1:14000"
	// The port that plays the role :443 plays in production: pebble validates
	// against it, certmagic's solver wants to bind it, and tether serves on it.
	liveChallengePort = 14443
	liveDomain        = "localhost"
)

func livePebbleCerts(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("TETHER_PEBBLE_CERTS"); p != "" {
		return p
	}
	// Wherever `go install` left the module.
	matches, _ := filepath.Glob(filepath.Join(build.Default.GOPATH, "pkg/mod/github.com/letsencrypt/pebble/v2@*/test/certs"))
	if len(matches) == 0 {
		t.Skip("pebble test certs not found; set TETHER_PEBBLE_CERTS")
	}
	return matches[len(matches)-1]
}

// startPebble runs a local ACME CA that validates TLS-ALPN-01 against
// liveChallengePort instead of 443, and returns once its directory answers.
func startPebble(t *testing.T) {
	t.Helper()
	bin := os.Getenv("TETHER_PEBBLE_BIN")
	if bin == "" {
		bin = filepath.Join(build.Default.GOPATH, "bin", "pebble")
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("pebble not installed (%v); go install github.com/letsencrypt/pebble/v2/cmd/pebble@latest", err)
	}
	certs := livePebbleCerts(t)

	cfgPath := filepath.Join(t.TempDir(), "pebble.json")
	cfg := map[string]any{"pebble": map[string]any{
		"listenAddress":                  livePebbleAPI,
		"managementListenAddress":        livePebbleMgmt,
		"certificate":                    filepath.Join(certs, "localhost", "cert.pem"),
		"privateKey":                     filepath.Join(certs, "localhost", "key.pem"),
		"httpPort":                       15002, // unused: HTTP-01 is off
		"tlsPort":                        liveChallengePort,
		"ocspResponderURL":               "",
		"externalAccountBindingRequired": false,
	}}
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal pebble config: %v", err)
	}
	if err := os.WriteFile(cfgPath, body, 0o600); err != nil {
		t.Fatalf("write pebble config: %v", err)
	}

	cmd := exec.Command(bin, "-config", cfgPath)
	// Pebble randomly sleeps and randomly rejects valid nonces to shake out
	// client bugs; neither is what this test measures. PEBBLE_AUTHZREUSE is
	// the one that matters: it defaults to 50, so half the time the second
	// order reuses phase 1's authorization and completes without a challenge
	// at all. That is not a slower pass, it is a vacuous one — the run looks
	// identical on a build with the bug.
	cmd.Env = append(os.Environ(),
		"PEBBLE_VA_NOSLEEP=1", "PEBBLE_WFE_NONCEREJECT=0", "PEBBLE_AUTHZREUSE=0")
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start pebble: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", livePebbleAPI, time.Second); err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("pebble did not come up")
}

// livePebbleSetup stands in for SetupACME: same shape, pointed at the local CA
// and at liveChallengePort. Everything downstream of it — applyACME, newServer,
// makeTLS — is the production code path.
func livePebbleSetup(t *testing.T, storage string) func(context.Context, string, string) (*tls.Config, CertBundle, error) {
	t.Helper()
	roots := x509.NewCertPool()
	pem, err := os.ReadFile(filepath.Join(livePebbleCerts(t), "pebble.minica.pem"))
	if err != nil {
		t.Fatalf("read pebble CA: %v", err)
	}
	if !roots.AppendCertsFromPEM(pem) {
		t.Fatal("pebble CA did not parse")
	}
	return func(ctx context.Context, domain, email string) (*tls.Config, CertBundle, error) {
		certmagic.Default.Storage = &certmagic.FileStorage{Path: storage}
		certmagic.DefaultACME.CA = livePebbleDir
		certmagic.DefaultACME.TestCA = livePebbleDir
		certmagic.DefaultACME.Email = email
		certmagic.DefaultACME.Agreed = true
		certmagic.DefaultACME.TrustedRoots = roots
		certmagic.DefaultACME.AltTLSALPNPort = liveChallengePort
		certmagic.DefaultACME.CertObtainTimeout = 60 * time.Second

		cfg, err := certmagic.TLS([]string{domain})
		if err != nil {
			return nil, CertBundle{}, fmt.Errorf("ACME %s: %w", domain, err)
		}
		return cfg, CertBundle{External: true}, nil
	}
}

// TestLiveACME_RenewalIsAnsweredByTheRunningListener is the A/B this whole wi
// rests on. It runs two real ACME orders against a real CA:
//
//	Phase 1 — issuance, with the port free. certmagic's own solver binds it and
//	          answers. This phase passed before the fix too; it is here to show
//	          that it is not what distinguishes the builds.
//	Phase 2 — a second order placed while tether is serving that port. The
//	          solver's bind fails, certmagic hands the challenge to whatever
//	          holds the socket, and only a listener offering acme-tls/1 with
//	          certmagic's GetCertificate can answer. This is the shape of every
//	          renewal, and it is what failed for the life of the daemon before.
//
// The probe was validated against the pre-fix build before being trusted: with
// makeTLS restored to its old form (drop the solvesACMEChallenge append),
// phase 1 still passes and phase 2 fails with
//
//	authorization failed: HTTP 403 urn:ietf:params:acme:error:unauthorized -
//	Failed to connect to 127.0.0.1:14443 for the tls-alpn-01 challenge
//
// having served zero challenge certificates. A CA reports the same ALPN refusal
// from the other side of the wire as `tls: no application protocol`, which is
// what Let's Encrypt returned when this was first hit in production.
func TestLiveACME_RenewalIsAnsweredByTheRunningListener(t *testing.T) {
	startPebble(t)

	// ---- Phase 1: issuance with the challenge port free -------------------
	cfg := &Config{
		Port:         liveChallengePort,
		Registry:     session.NewRegistry(),
		MCPLifecycle: mcplifecycle.New(),
		AcmeDomain:   liveDomain,
	}
	bundle, err := applyACME(context.Background(), cfg, livePebbleSetup(t, t.TempDir()))
	if err != nil {
		t.Fatalf("phase 1 (port free, certmagic solves for itself): %v", err)
	}
	if !bundle.External {
		t.Fatal("phase 1 returned a bundle that would re-arm self-signed rotation")
	}
	if cfg.acmeTLSBase == nil {
		t.Fatal("phase 1 issued a certificate but never wired it into the listener")
	}

	// ---- Phase 2: a second order while tether holds the port --------------
	//
	// Count the challenge handshakes this listener is asked to answer. Without
	// it the test cannot distinguish "tether answered the challenge" from
	// "the CA never asked" — and the CA not asking is the normal outcome of an
	// authorization it decided to reuse, which would make the whole phase
	// vacuous on a broken build too.
	var challengeHandshakes atomic.Int64
	inner := cfg.acmeTLSBase.GetCertificate
	cfg.acmeTLSBase.GetCertificate = func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if len(chi.SupportedProtos) == 1 && chi.SupportedProtos[0] == acmez.ACMETLS1Protocol {
			challengeHandshakes.Add(1)
		}
		return inner(chi)
	}

	srv := newServer(cfg, newCertHolder(mustGenCert(t)), nil, nil, nil, nil, nil)
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", liveChallengePort))
	if err != nil {
		t.Fatalf("listen on the challenge port: %v", err)
	}
	go func() { _ = srv.tcp.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = srv.tcp.Close() })

	// Sanity: the listener is up and is the one holding the port.
	if !liveWaitTLS(t, fmt.Sprintf("127.0.0.1:%d", liveChallengePort)) {
		t.Fatal("tether's listener never came up on the challenge port")
	}

	// force=true is load-bearing. certmagic's certificate cache is a process
	// global (NewDefault reuses it), so simply asking for the same name again
	// returns the phase-1 certificate in microseconds without touching the
	// network — which is exactly how the first version of this test passed
	// against a build that still had the bug.
	if err := certmagic.NewDefault().RenewCertSync(context.Background(), liveDomain, true); err != nil {
		t.Fatalf("phase 2 (tether holds the port, so tether must answer the challenge): %v\n"+
			"this is the tether#79 failure: the running listener did not offer acme-tls/1, "+
			"or offered it and served a certificate that did not come from certmagic", err)
	}
	if n := challengeHandshakes.Load(); n == 0 {
		t.Fatal("phase 2 renewed without this listener ever being asked for a challenge certificate; " +
			"the CA reused an authorization, so this run proves nothing about tether")
	}
}

func liveWaitTLS(t *testing.T, addr string) bool {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", addr,
			&tls.Config{InsecureSkipVerify: true, ServerName: liveDomain, NextProtos: []string{"h2"}})
		if err == nil {
			_ = c.Close()
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}
