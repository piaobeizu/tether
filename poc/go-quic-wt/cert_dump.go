//go:build dump
// Dump current step1 cert and verify Chrome serverCertificateHashes requirements.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"time"

	"github.com/quic-go/quic-go"
)

func main() {
	tlsCfg := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"h3"},
	}
	conn, err := quic.DialAddr(context.Background(), "127.0.0.1:4433", tlsCfg, &quic.Config{
		EnableDatagrams:                  true,
		EnableStreamResetPartialDelivery: true,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "dial:", err)
		os.Exit(1)
	}
	state := conn.ConnectionState().TLS
	if len(state.PeerCertificates) == 0 {
		fmt.Fprintln(os.Stderr, "no peer cert")
		os.Exit(1)
	}
	cert := state.PeerCertificates[0]

	// PEM dump
	pemBlock := &pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}
	fmt.Println("--- PEM ---")
	pem.Encode(os.Stdout, pemBlock)

	// Chrome serverCertificateHashes 要求逐项 check
	fmt.Println("\n--- Chrome serverCertificateHashes 要求验证 ---")

	// 1. SubjectPublicKeyInfo 必须是 ECDSA P-256
	pk, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		fmt.Println("✗ public key NOT ECDSA")
	} else if pk.Curve != elliptic.P256() {
		fmt.Printf("✗ public key curve = %s, NOT P-256\n", pk.Curve.Params().Name)
	} else {
		fmt.Println("✓ ECDSA P-256")
	}

	// 2. 签名算法必须是 ECDSA-with-SHA-256
	if cert.SignatureAlgorithm == x509.ECDSAWithSHA256 {
		fmt.Println("✓ signature algorithm = ECDSAWithSHA256")
	} else {
		fmt.Printf("✗ signature algorithm = %s (need ECDSAWithSHA256)\n", cert.SignatureAlgorithm)
	}

	// 3. 有效期 ≤ 14 天
	validity := cert.NotAfter.Sub(cert.NotBefore)
	if validity <= 14*24*time.Hour {
		fmt.Printf("✓ validity period = %s (≤14d)\n", validity)
	} else {
		fmt.Printf("✗ validity period = %s (> 14d)\n", validity)
	}

	// 4. NotBefore ≤ now ≤ NotAfter
	now := time.Now()
	if !cert.NotBefore.After(now) && cert.NotAfter.After(now) {
		fmt.Printf("✓ valid right now: NotBefore=%s NotAfter=%s\n", cert.NotBefore.Format(time.RFC3339), cert.NotAfter.Format(time.RFC3339))
	} else {
		fmt.Printf("✗ time invalid: NotBefore=%s NotAfter=%s now=%s\n", cert.NotBefore, cert.NotAfter, now)
	}

	// 5. 不是 CA 证书
	if !cert.IsCA {
		fmt.Println("✓ IsCA = false (leaf)")
	} else {
		fmt.Println("✗ IsCA = true (must be leaf)")
	}

	// 6. CommonName 存在
	if cert.Subject.CommonName != "" {
		fmt.Printf("✓ CommonName = %q\n", cert.Subject.CommonName)
	} else {
		fmt.Println("✗ CommonName empty")
	}

	// 7. SAN
	fmt.Printf("  SAN DNSNames: %v\n", cert.DNSNames)
	fmt.Printf("  SAN IPs:      %v\n", cert.IPAddresses)

	// 8. KeyUsage / ExtKeyUsage
	fmt.Printf("  KeyUsage: 0x%x  ExtKeyUsage: %v\n", cert.KeyUsage, cert.ExtKeyUsage)

	// 9. 计算两种 hash 对照 step1 输出
	derH := sha256.Sum256(cert.Raw)
	spkiH := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	fmt.Printf("\n  SHA256(DER):  %x\n", derH)
	fmt.Printf("  SHA256(SPKI): %x\n", spkiH)

	conn.CloseWithError(0, "done")
}
