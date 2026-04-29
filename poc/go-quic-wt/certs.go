// certs.go: self-signed TLS cert helper for PoC-2.
//
// WebTransport requires HTTP/3 over QUIC, which requires TLS. PoC-2 uses
// in-memory self-signed certs (ECDSA P-256, 7-day validity) — no on-disk
// state. Client side uses InsecureSkipVerify because we're not solving
// real-world cert distribution in PoC-2.
//
// All step files (step1_server.go, step2_client.go, ...) call generateCert()
// or share the same SHA-256 fingerprint by other means.

package main

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
	"os"
	"strings"
	"time"
)

// CertHashes carries multiple fingerprints. Chrome's WebTransport
// `serverCertificateHashes` API has historically been ambiguous between
// "hash of full DER cert" and "hash of SPKI"; expose both so the JS side
// can try whichever matches.
type CertHashes struct {
	DER  [32]byte // SHA-256 of x509 DER (W3C spec wording)
	SPKI [32]byte // SHA-256 of SubjectPublicKeyInfo (Chrome impl in some versions)
}

// generateCert returns a freshly-minted self-signed TLS cert + both
// SHA-256 fingerprints (DER + SPKI). PoC-2 uses ALPN h3.
func generateCert() (tls.Certificate, CertHashes, error) {
	cert, der, err := generateCertRaw()
	if err != nil {
		return tls.Certificate{}, CertHashes{}, err
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, CertHashes{}, fmt.Errorf("parse: %w", err)
	}
	return cert, CertHashes{
		DER:  sha256.Sum256(der),
		SPKI: sha256.Sum256(parsed.RawSubjectPublicKeyInfo),
	}, nil
}

func generateCertRaw() (tls.Certificate, []byte, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("ecdsa key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("serial: %w", err)
	}

	// SAN: Chrome's WebTransport with serverCertificateHashes still does
	// RFC 6125 hostname verification per W3C WebTransport spec. Include
	// loopback + any extra IPs/DNS via env vars so cert covers the actual
	// dial target.
	ips := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	dns := []string{"localhost", "tether-poc.local"}
	if extra := os.Getenv("TETHER_POC_EXTRA_IP"); extra != "" {
		// comma-separated list of additional IPs (e.g., "34.104.167.170,10.146.0.11")
		for _, s := range strings.Split(extra, ",") {
			s = strings.TrimSpace(s)
			if ip := net.ParseIP(s); ip != nil {
				ips = append(ips, ip)
			}
		}
	}
	if extra := os.Getenv("TETHER_POC_EXTRA_DNS"); extra != "" {
		for _, s := range strings.Split(extra, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				dns = append(dns, s)
			}
		}
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "tether-poc-2"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(7 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses:  ips,
		DNSNames:     dns,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("cert: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("key DER: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("X509KeyPair: %w", err)
	}

	return tlsCert, der, nil
}

// formatFingerprint formats a SHA-256 fingerprint as colon-separated hex.
func formatFingerprint(fp [32]byte) string {
	out := make([]byte, 0, 32*3)
	const hex = "0123456789abcdef"
	for i, b := range fp {
		if i > 0 {
			out = append(out, ':')
		}
		out = append(out, hex[b>>4], hex[b&0x0f])
	}
	return string(out)
}

// hashFilePath is the on-disk handoff between step1_server and step3_static.
// step1 writes the current cert fingerprint here at startup; step3's
// /cert-hash endpoint reads it so the HTML can fetch the live value
// without users editing JS.
const (
	hashFilePath     = "/tmp/tether-poc2-cert.hash"      // DER hash
	hashSPKIFilePath = "/tmp/tether-poc2-cert-spki.hash" // SPKI hash
)

// writeCertHashFile persists the DER SHA-256 fingerprint as raw 64-char hex
// (no colons, single line). Atomic via temp + rename.
func writeCertHashFile(fp [32]byte) error {
	return writeCertHashFileTo(hashFilePath, fp)
}

func writeCertHashFileTo(path string, fp [32]byte) error {
	const hex = "0123456789abcdef"
	out := make([]byte, 0, 65)
	for _, b := range fp {
		out = append(out, hex[b>>4], hex[b&0x0f])
	}
	out = append(out, '\n')

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
