package wt

import (
	"crypto/x509"
	"net"
	"testing"
	"time"
)

// TestGenerateDevCert_Verifies confirms the auto-generated cert is a
// well-formed self-signed x509 cert that an x509.CertPool will accept.
// This is the bare correctness check; doesn't exercise the WT layer.
func TestGenerateDevCert_Verifies(t *testing.T) {
	t.Parallel()

	dc, err := generateDevCert(devCertOptions{})
	if err != nil {
		t.Fatalf("generateDevCert: %v", err)
	}

	if dc.Leaf == nil {
		t.Fatal("Leaf is nil")
	}
	if len(dc.DER) == 0 {
		t.Fatal("DER empty")
	}
	if dc.SPKISHA256 == ([32]byte{}) {
		t.Fatal("SPKISHA256 is zero")
	}

	// Self-signed roundtrip: build a pool with the leaf, verify the leaf.
	pool := x509.NewCertPool()
	pool.AddCert(dc.Leaf)

	chains, err := dc.Leaf.Verify(x509.VerifyOptions{
		Roots:       pool,
		CurrentTime: time.Now(),
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(chains) == 0 {
		t.Fatal("no verified chains")
	}

	// Validity window sanity.
	if !dc.Leaf.NotBefore.Before(time.Now()) {
		t.Errorf("NotBefore not before now: %v", dc.Leaf.NotBefore)
	}
	if !dc.Leaf.NotAfter.After(time.Now()) {
		t.Errorf("NotAfter not after now: %v", dc.Leaf.NotAfter)
	}

	// Loopback SANs default in.
	wantIPs := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	for _, want := range wantIPs {
		found := false
		for _, got := range dc.Leaf.IPAddresses {
			if got.Equal(want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing IP SAN %v (have %v)", want, dc.Leaf.IPAddresses)
		}
	}
	wantDNS := "localhost"
	foundDNS := false
	for _, d := range dc.Leaf.DNSNames {
		if d == wantDNS {
			foundDNS = true
			break
		}
	}
	if !foundDNS {
		t.Errorf("missing DNS SAN %q (have %v)", wantDNS, dc.Leaf.DNSNames)
	}
}

// TestGenerateDevCert_ExtraSANs confirms caller-supplied IPs/DNS make
// it into the cert.
func TestGenerateDevCert_ExtraSANs(t *testing.T) {
	t.Parallel()

	extraIP := net.IPv4(10, 1, 2, 3)
	extraDNS := "tether.test.example"

	dc, err := generateDevCert(devCertOptions{
		ExtraIPs: []net.IP{extraIP},
		ExtraDNS: []string{extraDNS},
		Validity: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("generateDevCert: %v", err)
	}

	foundIP := false
	for _, got := range dc.Leaf.IPAddresses {
		if got.Equal(extraIP) {
			foundIP = true
			break
		}
	}
	if !foundIP {
		t.Errorf("missing extra IP SAN %v (have %v)", extraIP, dc.Leaf.IPAddresses)
	}

	foundDNS := false
	for _, d := range dc.Leaf.DNSNames {
		if d == extraDNS {
			foundDNS = true
			break
		}
	}
	if !foundDNS {
		t.Errorf("missing extra DNS SAN %q (have %v)", extraDNS, dc.Leaf.DNSNames)
	}

	// 24h validity respected (with 1h skew tolerance for NotBefore).
	wantAfter := time.Now().Add(23 * time.Hour)
	if !dc.Leaf.NotAfter.After(wantAfter) {
		t.Errorf("NotAfter=%v should be > %v", dc.Leaf.NotAfter, wantAfter)
	}
}
