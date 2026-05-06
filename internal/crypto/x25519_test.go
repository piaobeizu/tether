package crypto

import (
	"bytes"
	"errors"
	"testing"
)

func TestGenerateX25519KeyPair_Random(t *testing.T) {
	a, err := GenerateX25519KeyPair()
	if err != nil {
		t.Fatalf("first generate: %v", err)
	}
	b, err := GenerateX25519KeyPair()
	if err != nil {
		t.Fatalf("second generate: %v", err)
	}
	if a.Private == b.Private {
		t.Fatal("two crypto/rand-driven keypairs should not collide")
	}
	if a.Public == b.Public {
		t.Fatal("derived publics should not match for random privates")
	}
}

func TestX25519SharedSecret_RoundTrip(t *testing.T) {
	alice, err := GenerateX25519KeyPair()
	if err != nil {
		t.Fatalf("alice: %v", err)
	}
	bob, err := GenerateX25519KeyPair()
	if err != nil {
		t.Fatalf("bob: %v", err)
	}
	ssAlice, err := X25519SharedSecret(alice.Private[:], bob.Public[:])
	if err != nil {
		t.Fatalf("alice shared: %v", err)
	}
	ssBob, err := X25519SharedSecret(bob.Private[:], alice.Public[:])
	if err != nil {
		t.Fatalf("bob shared: %v", err)
	}
	if !bytes.Equal(ssAlice, ssBob) {
		t.Fatal("ECDH outputs differ between Alice and Bob — DH symmetry broken")
	}
	if len(ssAlice) != SharedSecretSize {
		t.Fatalf("shared secret length = %d, want %d", len(ssAlice), SharedSecretSize)
	}
}

func TestX25519SharedSecret_BadInputs(t *testing.T) {
	good, err := GenerateX25519KeyPair()
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	cases := []struct {
		name string
		priv []byte
		peer []byte
	}{
		{"short private", make([]byte, X25519KeySize-1), good.Public[:]},
		{"long peer", good.Private[:], make([]byte, X25519KeySize+1)},
		{"empty private", []byte{}, good.Public[:]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := X25519SharedSecret(tc.priv, tc.peer)
			if !errors.Is(err, ErrInvalidX25519Key) {
				t.Fatalf("want ErrInvalidX25519Key, got %v", err)
			}
		})
	}
}

func TestX25519SharedSecret_LowOrderPoint(t *testing.T) {
	// The all-zero public key is the canonical low-order point: any
	// private key DH'd with it yields all-zero output, and curve25519.X25519
	// returns an error rather than the zero output.
	priv, err := GenerateX25519KeyPair()
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	zero := make([]byte, X25519KeySize)
	_, err = X25519SharedSecret(priv.Private[:], zero)
	if !errors.Is(err, ErrInvalidX25519Key) {
		t.Fatalf("want ErrInvalidX25519Key for zero peer, got %v", err)
	}
}

func TestGenerateX25519KeyPair_DeterministicReader(t *testing.T) {
	// Reproducibility check: same seed → same key.
	seed := bytes.Repeat([]byte{0x42}, X25519KeySize)
	a, err := generateX25519KeyPairFrom(bytes.NewReader(seed))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	b, err := generateX25519KeyPairFrom(bytes.NewReader(seed))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if a.Private != b.Private || a.Public != b.Public {
		t.Fatal("deterministic seed produced different keypairs")
	}
}
