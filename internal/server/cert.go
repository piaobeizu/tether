// Package server provides the tether HTTP/3 + WebTransport server.
package server

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

// CertBundle holds the TLS certificate and its SHA-256 fingerprints.
// Chrome's WebTransport serverCertificateHashes API accepts either
// the full DER hash or the SPKI hash depending on Chrome version;
// we expose both. The /cert-hash endpoint serves DER as HashHex64.
type CertBundle struct {
	TLS  tls.Certificate
	DER  [32]byte // SHA-256 of full DER cert (W3C spec wording)
	SPKI [32]byte // SHA-256 of SubjectPublicKeyInfo (Chrome impl)
	// External is true when the cert was loaded from --cert-file/--key-file
	// (typically a CA-signed cert with >14d validity). When External is true,
	// /cert-hash MUST return 404 — passing serverCertificateHashes for a CA
	// cert that violates the W3C constraints (max 14d validity, ECDSA P-256,
	// self-signed) causes Chrome to reject the connection silently with
	// QUIC_NETWORK_IDLE_TIMEOUT. Browser then uses normal TLS validation.
	External bool
}

// GenerateCert generates a fresh self-signed ECDSA P-256 certificate
// valid for up to maxValidity, with SAN covering loopback addresses,
// localhost, and any extra IPs/DNS names from TETHER_EXTRA_IP /
// TETHER_EXTRA_DNS environment variables.
//
// DO NOT use RSA — Chrome's serverCertificateHashes rejects non-ECDSA-P-256.
// Maximum validity is capped at 14 days per §10.B.2 #5 cert-rotation contract.
func GenerateCert() (CertBundle, error) {
	const maxValidity = 14 * 24 * time.Hour

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return CertBundle{}, fmt.Errorf("ecdsa key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		return CertBundle{}, fmt.Errorf("serial: %w", err)
	}

	ips := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	dns := []string{"localhost"}

	if extra := os.Getenv("TETHER_EXTRA_IP"); extra != "" {
		for s := range strings.SplitSeq(extra, ",") {
			if s = strings.TrimSpace(s); s != "" {
				if ip := net.ParseIP(s); ip != nil {
					ips = append(ips, ip)
				}
			}
		}
	}
	if extra := os.Getenv("TETHER_EXTRA_DNS"); extra != "" {
		for s := range strings.SplitSeq(extra, ",") {
			if s = strings.TrimSpace(s); s != "" {
				dns = append(dns, s)
			}
		}
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "tether"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(maxValidity - time.Hour), // total validity = exactly 14d (Chrome ≤14d limit)
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  ips,
		DNSNames:     dns,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return CertBundle{}, fmt.Errorf("create cert: %w", err)
	}

	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return CertBundle{}, fmt.Errorf("parse cert: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return CertBundle{}, fmt.Errorf("marshal key: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return CertBundle{}, fmt.Errorf("X509KeyPair: %w", err)
	}

	return CertBundle{
		TLS:  tlsCert,
		DER:  sha256.Sum256(der),
		SPKI: sha256.Sum256(parsed.RawSubjectPublicKeyInfo),
	}, nil
}

// HashHex encodes a 32-byte SHA-256 fingerprint as 64 lowercase hex chars
// with no separators — the wire.HashHex64 format.
func HashHex(fp [32]byte) string {
	const hexChars = "0123456789abcdef"
	out := make([]byte, 64)
	for i, b := range fp {
		out[i*2] = hexChars[b>>4]
		out[i*2+1] = hexChars[b&0x0f]
	}
	return string(out)
}

func sha256Sum(b []byte) [32]byte { return sha256.Sum256(b) }

// marshalECKey marshals a crypto.PrivateKey (assumed ECDSA P-256) to PEM.
func marshalECKey(key any) ([]byte, error) {
	ecKey, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("expected *ecdsa.PrivateKey, got %T", key)
	}
	der, err := x509.MarshalECPrivateKey(ecKey)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}
