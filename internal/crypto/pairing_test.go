package crypto

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestFingerprint_DeterministicAndShape(t *testing.T) {
	kp, err := GenerateX25519KeyPair()
	if err != nil {
		t.Fatalf("kp: %v", err)
	}
	fpA, err := Fingerprint(kp.Public[:])
	if err != nil {
		t.Fatalf("fp1: %v", err)
	}
	fpB, err := Fingerprint(kp.Public[:])
	if err != nil {
		t.Fatalf("fp2: %v", err)
	}
	if fpA != fpB {
		t.Fatal("fingerprint must be deterministic for the same pubkey")
	}
	parts := strings.Split(fpA, ":")
	if len(parts) != 2 {
		t.Fatalf("expected `prefix:suffix`, got %q", fpA)
	}
	if len(parts[0]) != 2*FingerprintPrefixLen || len(parts[1]) != 2*FingerprintSuffixLen {
		t.Fatalf("unexpected fingerprint shape: %q", fpA)
	}
}

func TestFingerprint_DifferentKeysDiffer(t *testing.T) {
	a, _ := GenerateX25519KeyPair()
	b, _ := GenerateX25519KeyPair()
	fa, _ := Fingerprint(a.Public[:])
	fb, _ := Fingerprint(b.Public[:])
	if fa == fb {
		t.Fatal("two random pubkeys produced same fingerprint")
	}
}

func TestVerifyFingerprint(t *testing.T) {
	kp, _ := GenerateX25519KeyPair()
	fp, _ := Fingerprint(kp.Public[:])
	if err := VerifyFingerprint(kp.Public[:], fp); err != nil {
		t.Fatalf("verify good: %v", err)
	}
	if err := VerifyFingerprint(kp.Public[:], "ffffffff:ffffffff"); !errors.Is(err, ErrPairing) {
		t.Fatalf("verify wrong: want ErrPairing, got %v", err)
	}
}

func TestComputeSAS_ShapeAndDeterminism(t *testing.T) {
	shared := bytes.Repeat([]byte{0xAA}, SharedSecretSize)
	a, err := ComputeSAS(shared)
	if err != nil {
		t.Fatalf("a: %v", err)
	}
	b, err := ComputeSAS(shared)
	if err != nil {
		t.Fatalf("b: %v", err)
	}
	if a != b {
		t.Fatal("SAS must be deterministic for same shared secret")
	}
	if len(a) != SASDigits {
		t.Fatalf("SAS length = %d, want %d", len(a), SASDigits)
	}
	for _, c := range a {
		if c < '0' || c > '9' {
			t.Fatalf("SAS contains non-digit: %q", a)
		}
	}
}

func TestComputeSAS_DifferentSecretsDiffer(t *testing.T) {
	s1 := bytes.Repeat([]byte{0xAA}, SharedSecretSize)
	s2 := bytes.Repeat([]byte{0xBB}, SharedSecretSize)
	a, _ := ComputeSAS(s1)
	b, _ := ComputeSAS(s2)
	if a == b {
		// 1 in 1e6 collision possible by chance — fixed inputs above are safe.
		t.Fatal("SAS collided on different shared secrets")
	}
}

func TestVerifySAS(t *testing.T) {
	shared := bytes.Repeat([]byte{0x11}, SharedSecretSize)
	good, _ := ComputeSAS(shared)
	if err := VerifySAS(shared, good); err != nil {
		t.Fatalf("verify match: %v", err)
	}
	if err := VerifySAS(shared, "000000"); !errors.Is(err, ErrPairing) {
		// extremely unlikely the real SAS is "000000" for this fixed input.
		if good != "000000" {
			t.Fatalf("verify wrong: want ErrPairing, got %v", err)
		}
	}
	if err := VerifySAS(shared, "1234"); !errors.Is(err, ErrPairing) {
		t.Fatalf("verify length mismatch: want ErrPairing, got %v", err)
	}
}

func TestCompletePairing_BothSidesAgree(t *testing.T) {
	cli, err := GenerateX25519KeyPair()
	if err != nil {
		t.Fatalf("cli: %v", err)
	}
	mob, err := GenerateX25519KeyPair()
	if err != nil {
		t.Fatalf("mob: %v", err)
	}
	cliResult, err := CompletePairing(cli.Private[:], mob.Public[:])
	if err != nil {
		t.Fatalf("cli pair: %v", err)
	}
	mobResult, err := CompletePairing(mob.Private[:], cli.Public[:])
	if err != nil {
		t.Fatalf("mob pair: %v", err)
	}
	if !bytes.Equal(cliResult.WrapKey, mobResult.WrapKey) {
		t.Fatal("wrap_key must agree across pairing peers (the entire point)")
	}
	// Local fingerprint on one side equals peer fingerprint on the other.
	if cliResult.LocalFingerprint != mobResult.PeerFingerprint {
		t.Fatal("CLI's local fingerprint should equal Mobile's peer fingerprint")
	}
	if cliResult.PeerFingerprint != mobResult.LocalFingerprint {
		t.Fatal("CLI's peer fingerprint should equal Mobile's local fingerprint")
	}
}

func TestCompletePairing_BadInputs(t *testing.T) {
	good, _ := GenerateX25519KeyPair()
	if _, err := CompletePairing([]byte{0x01}, good.Public[:]); !errors.Is(err, ErrPairing) {
		t.Fatalf("short priv: %v", err)
	}
	if _, err := CompletePairing(good.Private[:], []byte{0x01}); !errors.Is(err, ErrPairing) {
		t.Fatalf("short peer: %v", err)
	}
}
