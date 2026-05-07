package pair

import (
	"fmt"

	"golang.org/x/crypto/curve25519"
)

// basepointMulPub multiplies the curve25519 basepoint by priv to derive
// the matching public key. Lives in the pair package (rather than
// reaching into internal/crypto) so the pair driver can take a
// deterministic io.Reader for tests without leaking that surface up
// through internal/crypto.
//
// curve25519 clamps the scalar internally per RFC 7748 §5, so passing
// a raw 32-byte random scalar produces a valid pubkey.
func basepointMulPub(priv []byte) ([]byte, error) {
	if len(priv) != 32 {
		return nil, fmt.Errorf("pair: x25519 private key must be 32 bytes (got %d)", len(priv))
	}
	pub, err := curve25519.X25519(priv, curve25519.Basepoint)
	if err != nil {
		return nil, fmt.Errorf("pair: x25519 basepoint mul: %w", err)
	}
	return pub, nil
}
