package crypto

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/curve25519"
)

// X25519KeySize is the byte length of every X25519 scalar / point used in
// this package — both private and public keys are 32 bytes (RFC 7748 §5).
const X25519KeySize = 32

// SharedSecretSize is the byte length of the raw output of curve25519.X25519
// (the unhashed shared secret). HKDF (hkdf.go) takes this as IKM.
const SharedSecretSize = 32

// ErrInvalidX25519Key is returned when an X25519 key has the wrong length or
// fails curve25519 validation (e.g. a low-order public key, which would
// produce an all-zero shared secret per RFC 7748 §6.1).
var ErrInvalidX25519Key = errors.New("crypto: invalid X25519 key")

// X25519KeyPair is one side of a device-pair handshake. It is intentionally
// NOT the long-term identity material on its own — the wrap_key derivation
// in pairing.go is what cements pairing identity.
//
// Cross-Rust interop: bytes are identical to what x25519-dalek's
// `StaticSecret` (private) and `PublicKey` (public) `to_bytes()` returns.
// 32 bytes, little-endian scalar form per RFC 7748 §5.
type X25519KeyPair struct {
	Private [X25519KeySize]byte
	Public  [X25519KeySize]byte
}

// GenerateX25519KeyPair generates a fresh X25519 keypair using crypto/rand.
// Per RFC 7748 §5 the X25519 function clamps the scalar internally, so we
// pass random 32 bytes through directly without manual clamping. (The Go
// curve25519 package's X25519(_, basepoint) likewise clamps inside.)
func GenerateX25519KeyPair() (*X25519KeyPair, error) {
	return generateX25519KeyPairFrom(rand.Reader)
}

// generateX25519KeyPairFrom is the testable entry point — tests can pass a
// deterministic reader (e.g. bytes.NewReader of a known seed) to make
// pairing-flow tests reproducible.
func generateX25519KeyPairFrom(r io.Reader) (*X25519KeyPair, error) {
	kp := &X25519KeyPair{}
	if _, err := io.ReadFull(r, kp.Private[:]); err != nil {
		return nil, fmt.Errorf("crypto: read X25519 private: %w", err)
	}
	pub, err := curve25519.X25519(kp.Private[:], curve25519.Basepoint)
	if err != nil {
		return nil, fmt.Errorf("crypto: derive X25519 public: %w", err)
	}
	copy(kp.Public[:], pub)
	return kp, nil
}

// basepointMul derives the X25519 public key from a private scalar by
// multiplying the basepoint. Used by pairing.go to recover our own
// pubkey when only the private scalar is on hand.
func basepointMul(private []byte) ([]byte, error) {
	if len(private) != X25519KeySize {
		return nil, ErrInvalidX25519Key
	}
	pub, err := curve25519.X25519(private, curve25519.Basepoint)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidX25519Key, err)
	}
	return pub, nil
}

// X25519SharedSecret computes the ECDH shared secret between our private key
// and a peer's public key. The output is the raw 32-byte point — callers
// MUST run it through HKDF (hkdf.go) before using as a symmetric key, per
// RFC 7748 §6.1 and the spec §11.C derivation chain
// (device_pair_secret → HKDF → wrap_key).
//
// Returns ErrInvalidX25519Key if the peer public key has wrong length or
// produces the all-zero output (low-order point — possible MITM signal).
func X25519SharedSecret(privateKey, peerPublicKey []byte) ([]byte, error) {
	if len(privateKey) != X25519KeySize || len(peerPublicKey) != X25519KeySize {
		return nil, ErrInvalidX25519Key
	}
	shared, err := curve25519.X25519(privateKey, peerPublicKey)
	if err != nil {
		// curve25519.X25519 returns an error specifically when the result
		// would be the all-zero output (low-order public key attack).
		return nil, fmt.Errorf("%w: %v", ErrInvalidX25519Key, err)
	}
	return shared, nil
}
