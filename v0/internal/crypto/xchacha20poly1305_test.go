package crypto

import (
	"bytes"
	"crypto/rand"
	"errors"
	"testing"
)

func freshKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, AEADKeySize)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return k
}

func TestSealOpen_RoundTrip(t *testing.T) {
	key := freshKey(t)
	plaintext := []byte("hello tether — server can't read this")
	env, err := SealEnvelope(key, "sess-1", "cli-A", "mob-B", 1, plaintext)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if env.WireVersion != EnvelopeWireV1 {
		t.Fatalf("wire version = %d", env.WireVersion)
	}
	if len(env.Ciphertext) < AEADTagSize {
		t.Fatal("ciphertext shorter than AEAD tag")
	}
	got, err := OpenEnvelope(key, env)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("plaintext mismatch: %q != %q", got, plaintext)
	}
}

func TestOpen_WrongKeyFails(t *testing.T) {
	key := freshKey(t)
	otherKey := freshKey(t)
	env, err := SealEnvelope(key, "s", "f", "t", 1, []byte("payload"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := OpenEnvelope(otherKey, env); !errors.Is(err, ErrAEAD) {
		t.Fatalf("want ErrAEAD on wrong key, got %v", err)
	}
}

func TestOpen_TamperedCiphertextFails(t *testing.T) {
	key := freshKey(t)
	env, err := SealEnvelope(key, "s", "f", "t", 1, []byte("p"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	env.Ciphertext[0] ^= 0xFF
	if _, err := OpenEnvelope(key, env); !errors.Is(err, ErrAEAD) {
		t.Fatalf("want ErrAEAD on tamper, got %v", err)
	}
}

func TestOpen_TamperedADFails(t *testing.T) {
	key := freshKey(t)
	env, err := SealEnvelope(key, "session-A", "from", "to", 1, []byte("p"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	env.SessionID = "session-B" // attacker swaps routing metadata
	if _, err := OpenEnvelope(key, env); !errors.Is(err, ErrAEAD) {
		t.Fatalf("want ErrAEAD on AD tamper, got %v", err)
	}
}

func TestOpen_NonceUniqueness(t *testing.T) {
	key := freshKey(t)
	envA, _ := SealEnvelope(key, "s", "f", "t", 1, []byte("x"))
	envB, _ := SealEnvelope(key, "s", "f", "t", 1, []byte("x"))
	if envA.Nonce == envB.Nonce {
		// 24-byte random nonce collision is statistically impossible. If
		// this fires we have an RNG bug.
		t.Fatal("two seals with same plaintext+key produced identical nonce")
	}
	if bytes.Equal(envA.Ciphertext, envB.Ciphertext) {
		t.Fatal("ciphertext should differ when nonces differ")
	}
}

func TestSeal_BadKeySize(t *testing.T) {
	if _, err := SealEnvelope([]byte{0x01, 0x02}, "s", "f", "t", 1, []byte("p")); !errors.Is(err, ErrAEAD) {
		t.Fatalf("want ErrAEAD on short key, got %v", err)
	}
}

func TestEnvelope_MarshalRoundTrip(t *testing.T) {
	key := freshKey(t)
	env, err := SealEnvelope(key, "session-id-42", "device-from-X", "device-to-Y", 7, []byte("hello"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	wire, err := env.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	back, err := UnmarshalEnvelope(wire)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.SessionID != env.SessionID || back.FromDeviceID != env.FromDeviceID ||
		back.ToDeviceID != env.ToDeviceID || back.KeyVersion != env.KeyVersion ||
		back.Nonce != env.Nonce || !bytes.Equal(back.Ciphertext, env.Ciphertext) {
		t.Fatal("round-tripped envelope differs from original")
	}
	pt, err := OpenEnvelope(key, back)
	if err != nil {
		t.Fatalf("open after wire round-trip: %v", err)
	}
	if !bytes.Equal(pt, []byte("hello")) {
		t.Fatalf("plaintext: %q", pt)
	}
}

func TestUnmarshal_BadVersion(t *testing.T) {
	// Hand-construct a wire frame with a bad version byte.
	bad := []byte{0xFF, 0x00, 0x00}
	if _, err := UnmarshalEnvelope(bad); !errors.Is(err, ErrAEAD) {
		t.Fatalf("want ErrAEAD on bad wire version, got %v", err)
	}
}

func TestUnmarshal_TruncatedTrailing(t *testing.T) {
	key := freshKey(t)
	env, _ := SealEnvelope(key, "s", "f", "t", 1, []byte("payload"))
	wire, _ := env.Marshal()
	// Drop trailing byte → ciphertext too short.
	if _, err := UnmarshalEnvelope(wire[:len(wire)-1]); !errors.Is(err, ErrAEAD) {
		t.Fatalf("want ErrAEAD on truncated, got %v", err)
	}
	// Append trailing junk → should also fail (trailing-bytes check).
	junked := append(append([]byte{}, wire...), 0xAA, 0xBB)
	if _, err := UnmarshalEnvelope(junked); !errors.Is(err, ErrAEAD) {
		t.Fatalf("want ErrAEAD on trailing junk, got %v", err)
	}
}

func TestBuildAD_Deterministic(t *testing.T) {
	a := BuildAD("sess", "from", "to", 1)
	b := BuildAD("sess", "from", "to", 1)
	if !bytes.Equal(a, b) {
		t.Fatal("BuildAD must be deterministic")
	}
	c := BuildAD("sess", "from", "to", 2)
	if bytes.Equal(a, c) {
		t.Fatal("different keyVersion must change AD")
	}
	// Length-prefix shielding: "ab"+"c" must not collide with "a"+"bc".
	d := BuildAD("ab", "c", "to", 1)
	e := BuildAD("a", "bc", "to", 1)
	if bytes.Equal(d, e) {
		t.Fatal("length-prefix shielding broken: AD collision across boundary shifts")
	}
}
