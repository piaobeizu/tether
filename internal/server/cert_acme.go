package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/caddyserver/certmagic"
)

// acmeChallengePort is the TCP port a CA connects to for a TLS-ALPN-01
// challenge. RFC 8737 §3 fixes it — "This connection MUST use TCP port 443" —
// so it is not configurable at either end: certmagic's solver address is
// likewise pinned to :443 (getTLSALPNPort in certmagic acmeclient.go returns
// TLSALPNChallengePort unless the package-level HTTPSPort or the issuer's
// AltTLSALPNPort is changed, and SetupACME changes neither).
const acmeChallengePort = 443

// SetupACME obtains and auto-renews a Let's Encrypt certificate for domain via
// the ACME TLS-ALPN-01 challenge, which the CA validates by connecting to
// :443. Port 80 is not used and need not be reachable.
//
// The challenge type is not a choice made here — certmagic.TLS() sets
// DefaultACME.DisableHTTPChallenge = true in its own first lines, so TLS-ALPN
// is the only solver ever registered. (This doc comment previously claimed
// HTTP-01/port 80; it was wrong from the start, and cost a wi to unwind —
// tether#79.)
//
// That leaves :443 answered by two different things at two different moments,
// and the second one is not self-evident:
//
//   - Now, at first issuance: this call blocks until the cert exists, and it
//     runs before any tether listener binds, so certmagic's solver takes :443
//     itself and gives it back.
//   - Later, at renewal: if tether is itself serving :443, certmagic cannot
//     bind and defers to us. Our listener must offer acme-tls/1 in its ALPN
//     list or renewal fails silently — see makeTLS in server.go, which is
//     where that obligation is actually discharged.
//
// The "if" in that second bullet is load-bearing and easy to read past: when
// tether serves some other port, certmagic keeps solving on :443 by itself and
// nothing in this repo participates. warnACMEPortMismatch covers that case.
//
// Certs are stored in ~/.tether/acme/ and renewed automatically in the
// background. Returns a tls.Config with GetCertificate managed by certmagic,
// plus a CertBundle with External=true so /cert-hash returns 404 (CA certs
// must not use serverCertificateHashes — see cert.go CertBundle.External).
//
// email may be empty. Let's Encrypt registers a contactless account without
// complaint, and since it stopped sending expiration notices on 2025-06-04
// (letsencrypt.org/2025/01/22/ending-expiration-emails) supplying one buys no
// warning either. What it does buy is account identity: certmagic keys ACME
// account storage by email (storageKeyUserPrefix in certmagic account.go,
// falling back to a placeholder when empty), so adding --acme-email to an
// existing deployment registers a *different* account rather than annotating
// the current one. That is a footnote for the flag help, not a reason to
// reject an empty value here.
func SetupACME(ctx context.Context, domain, email string) (*tls.Config, CertBundle, error) {
	dir, err := tetherDataDir()
	if err != nil {
		return nil, CertBundle{}, err
	}

	certmagic.Default.Storage = &certmagic.FileStorage{Path: filepath.Join(dir, "acme")}
	certmagic.DefaultACME.Email = email
	certmagic.DefaultACME.Agreed = true

	tlsCfg, err := certmagic.TLS([]string{domain})
	if err != nil {
		return nil, CertBundle{}, fmt.Errorf("ACME %s: %w", domain, err)
	}

	return tlsCfg, CertBundle{External: true}, nil
}

// warnACMEPortMismatch logs when --acme-domain is combined with a listen port
// other than 443.
//
// Nothing here can move the challenge: the CA connects to :443 by fiat (see
// acmeChallengePort), so on any other port tether is simply not on the path.
// Issuance and renewal then depend entirely on what :443 does at that moment,
// and the failure mode is the ugly one — certmagic's robustTryListen, finding
// :443 occupied, *dials* it, takes a successful dial as proof that its owner
// can answer the challenge, and returns no listener and no error (certmagic
// solvers.go). The CA duly connects to a stranger, gets `tls: no application
// protocol`, and the challenge fails with nothing in tether's logs to explain
// why. That exact confusion is what produced the wrong diagnosis this wi
// started from, so it is worth one line at startup.
//
// Not fatal: --port 8898 --acme-domain d does work when :443 is free, and
// there are legitimate setups (a forwarder on :443) where it is deliberate.
func warnACMEPortMismatch(domain string, port int) {
	if port == acmeChallengePort {
		return
	}
	slog.Warn("ACME challenges are answered on :443 regardless of --port; this daemon will not see them",
		"domain", domain, "port", port, "challenge_port", acmeChallengePort)
}

// applyACME obtains an ACME certificate and wires it into cfg, returning the
// bundle the rest of Run should use in place of the self-signed one.
//
// Split out of Run, and taking setup as a parameter, for one reason: the wiring
// is the part that breaks silently. Losing the cfg.acmeTLSBase assignment does
// not fail, log, or change startup at all — it just drops the daemon back onto
// the self-signed cert while --acme-domain says otherwise, and no test in this
// package could previously tell. Same for forgetting to warn, or passing the
// arguments to the warning in the wrong order.
//
// Errors are returned unwrapped by design; Run adds the "ACME setup" context.
func applyACME(ctx context.Context, cfg *Config,
	setup func(context.Context, string, string) (*tls.Config, CertBundle, error),
) (CertBundle, error) {
	warnACMEPortMismatch(cfg.AcmeDomain, cfg.Port)
	// --cert-file and --acme-domain together: this call is about to replace the
	// bundle Step 4 loaded, and the listener will take its tls.Config from
	// certmagic, so those files stop being served here. Worth a line — since
	// tether#73 the flag help promises they are re-read every minute, which
	// stays true of the flags and false of this combination.
	if externalPEMSource(cfg.CertFile, cfg.KeyFile) {
		slog.Warn("--acme-domain overrides --cert-file/--key-file; the files will not be served or re-read",
			"domain", cfg.AcmeDomain, "cert_file", cfg.CertFile)
	}
	slog.Info("obtaining ACME cert", "domain", cfg.AcmeDomain)

	acmeTLS, bundle, err := setup(ctx, cfg.AcmeDomain, cfg.AcmeEmail)
	if err != nil {
		return CertBundle{}, err
	}
	cfg.acmeTLSBase = acmeTLS

	slog.Info("ACME cert ready", "domain", cfg.AcmeDomain)
	return bundle, nil
}
