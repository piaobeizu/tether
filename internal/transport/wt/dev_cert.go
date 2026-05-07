// dev_cert.go — in-memory self-signed TLS cert generator for the WT
// server's dev mode.
//
// WebTransport-over-HTTP/3 mandates TLS 1.3 (HTTP/3 floor) and ALPN
// "h3". For local development + integration tests we don't want to
// require operator-managed certs, so the WT server can auto-generate
// an ECDSA P-256 self-signed cert on each boot.
//
// Production paths must NOT use this — Config.Cert / Config.Key in
// server.go take precedence, and operators load real certs from disk
// (or a secret manager) in deployment.
//
// Design notes:
//   - ECDSA P-256 for both speed and Chrome's `serverCertificateHashes`
//     compatibility (RSA-PSS is also fine, ECDSA is smaller on the wire).
//   - 7-day validity — long enough that dev test runs won't expire mid-
//     session, short enough that it can't be reused as a long-lived
//     credential if it leaks.
//   - SAN includes 127.0.0.1 + ::1 + "localhost" so loopback dialers
//     pass RFC 6125 hostname verification. Extra hosts are configurable.
//   - Returns BOTH the raw DER bytes and the SHA-256 of the full DER-
//     encoded certificate so callers (future client-side cert pinning
//     or `serverCertificateHashes` browser handoff) can compute the
//     fingerprint without re-parsing. We hash DER (not SPKI) to match
//     the W3C WebTransport `serverCertificateHashes` algorithm — that
//     dictionary's `value` field is the SHA-256 of the DER cert, and
//     web-transport-quinn's `with_server_certificate_hashes` consumes
//     the same. Pinning by SPKI does NOT interop with the browser-API
//     (or quinn-API) form, so we expose DER hash only.

package wt

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"
)

// devCertOptions tunes the dev cert generator. Zero values give safe
// defaults (loopback only, 7-day validity).
type devCertOptions struct {
	// ExtraIPs adds IP SANs beyond loopback. Used when the server
	// binds to a non-loopback address (LAN dev, GCP VM dev).
	ExtraIPs []net.IP
	// ExtraDNS adds DNS SANs.
	ExtraDNS []string
	// Validity overrides the 7-day default.
	Validity time.Duration
}

// devCert is the result of generateDevCert: the wired-up tls.Certificate
// (cert chain + private key) plus the parsed certificate and DER
// fingerprint for downstream pinning.
//
// DERSHA256 is sha256 over the FULL DER-encoded x509 leaf (Leaf.Raw),
// matching the W3C `serverCertificateHashes.value` algorithm — so the
// Rust web-transport-quinn client can pin via
// `with_server_certificate_hashes` against this exact value.
type devCert struct {
	TLS       tls.Certificate
	Leaf      *x509.Certificate
	DER       []byte
	DERSHA256 [32]byte
}

// generateDevCert mints a fresh self-signed ECDSA P-256 cert valid for
// loopback (+ caller-supplied SANs). Returns wired tls.Certificate +
// fingerprint metadata.
func generateDevCert(opts devCertOptions) (devCert, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return devCert{}, fmt.Errorf("ecdsa key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		return devCert{}, fmt.Errorf("serial: %w", err)
	}

	validity := opts.Validity
	if validity <= 0 {
		validity = 7 * 24 * time.Hour
	}

	ips := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	ips = append(ips, opts.ExtraIPs...)
	dns := []string{"localhost"}
	dns = append(dns, opts.ExtraDNS...)

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "tether-wt-dev"},
		NotBefore:    time.Now().Add(-time.Hour), // clock skew tolerance
		NotAfter:     time.Now().Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses:  ips,
		DNSNames:     dns,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return devCert{}, fmt.Errorf("create cert: %w", err)
	}

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return devCert{}, fmt.Errorf("parse cert: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return devCert{}, fmt.Errorf("marshal key: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return devCert{}, fmt.Errorf("X509KeyPair: %w", err)
	}
	// Inline the parsed leaf so callers don't pay the parse cost again.
	tlsCert.Leaf = leaf

	return devCert{
		TLS:       tlsCert,
		Leaf:      leaf,
		DER:       der,
		DERSHA256: sha256.Sum256(der),
	}, nil
}
