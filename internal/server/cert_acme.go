package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"path/filepath"

	"github.com/caddyserver/certmagic"
)

// SetupACME obtains and auto-renews a Let's Encrypt certificate for domain
// via ACME HTTP-01 challenge (port 80 must be reachable from the internet).
// Certs are stored in ~/.tether/acme/ and renewed automatically in the
// background. Returns a tls.Config with GetCertificate managed by certmagic,
// plus a CertBundle with External=true so /cert-hash returns 404 (CA certs
// must not use serverCertificateHashes — see cert.go CertBundle.External).
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
