package server

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const certRotateThreshold = 24 * time.Hour

// LoadOrGenCert loads a cert from explicit PEM files (bypassing rotation),
// or falls back to the managed cert store at ~/.tether/{cert,key}.pem.
// On first run or when the stored cert expires within 24h, a new cert is
// generated and persisted (§10.B.2 #5 rotation contract).
func LoadOrGenCert(certFile, keyFile string) (CertBundle, error) {
	if certFile != "" && keyFile != "" {
		bundle, err := loadPEMFiles(certFile, keyFile)
		if err != nil {
			return bundle, err
		}
		bundle.External = true
		return bundle, nil
	}
	return loadOrRotateManaged()
}

func loadOrRotateManaged() (CertBundle, error) {
	dir, err := tetherDataDir()
	if err != nil {
		return CertBundle{}, err
	}
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	if bundle, err := loadPEMFiles(certPath, keyPath); err == nil {
		if time.Until(bundle.TLS.Leaf.NotAfter) >= certRotateThreshold {
			return bundle, nil
		}
	}

	bundle, err := GenerateCert()
	if err != nil {
		return CertBundle{}, fmt.Errorf("generate cert: %w", err)
	}
	if err := persistCert(bundle, certPath, keyPath); err != nil {
		return CertBundle{}, fmt.Errorf("persist cert: %w", err)
	}
	return bundle, nil
}

func loadPEMFiles(certFile, keyFile string) (CertBundle, error) {
	tlsCert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return CertBundle{}, err
	}
	if tlsCert.Leaf == nil {
		tlsCert.Leaf, err = x509.ParseCertificate(tlsCert.Certificate[0])
		if err != nil {
			return CertBundle{}, fmt.Errorf("parse leaf: %w", err)
		}
	}
	der := tlsCert.Certificate[0]
	return CertBundle{
		TLS:  tlsCert,
		DER:  sha256Sum(der),
		SPKI: sha256Sum(tlsCert.Leaf.RawSubjectPublicKeyInfo),
	}, nil
}

func persistCert(b CertBundle, certPath, keyPath string) error {
	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: b.TLS.Certificate[0],
	})
	keyPEM, err := marshalECKey(b.TLS.PrivateKey)
	if err != nil {
		return fmt.Errorf("marshal key: %w", err)
	}
	if err := atomicWrite(certPath, certPEM, 0o600); err != nil {
		return fmt.Errorf("write cert: %w", err)
	}
	if err := atomicWrite(keyPath, keyPEM, 0o600); err != nil {
		return fmt.Errorf("write key: %w", err)
	}
	return nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func tetherDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	dir := filepath.Join(home, ".tether")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir ~/.tether: %w", err)
	}
	return dir, nil
}
