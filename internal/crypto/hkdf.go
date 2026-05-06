package crypto

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// HKDFKeySize is the canonical key length we derive throughout this package
// — both wrap_key and session_key are 32 bytes, matching the
// XChaCha20-Poly1305 key size (sym.KeySize).
const HKDFKeySize = 32

// Stable info-strings for HKDF expansion. Treat as wire constants per spec
// §11.C — renaming requires a keyVersion bump because peers using different
// info strings derive different keys.
const (
	// HKDFInfoPair is the info parameter for deriving wrap_key from the
	// X25519 device-pair shared secret. Pairs with x25519-dalek output on
	// the Rust side.
	HKDFInfoPair = "tether-pair-v1"

	// HKDFInfoSessionWrap is the info string for the AEAD key that wraps
	// session_key for transport in a control envelope (CLI → Mobile App).
	// Distinct from HKDFInfoPair so a leaked wrap_key does not give the
	// holder reuse across domains.
	HKDFInfoSessionWrap = "tether-session-wrap-v1"

	// HKDFInfoSessionKey is the info string for the per-session AEAD key
	// used to encrypt envelopes within a cc session.
	HKDFInfoSessionKey = "tether-session-key-v1"

	// HKDFInfoSAS derives the 6-digit Short Authentication String for the
	// server-mediated pairing fallback (dev/debug builds only — see
	// pairing.go and spec §11.C).
	HKDFInfoSAS = "tether-pair-sas"

	// HKDFInfoSessionPersist derives the AEAD key used to wrap session_key
	// at rest in keys.bin (spec §11.B B4 — daemon restart must be able to
	// recover the key to decrypt server-spooled history).
	HKDFInfoSessionPersist = "tether-session-persist-v1"
)

// ErrHKDFInput is returned when an input to DeriveKey is empty or malformed
// in a way that would weaken the derivation.
var ErrHKDFInput = errors.New("crypto: invalid HKDF input")

// DeriveKey runs HKDF-SHA256(ikm, salt, info) and returns `length` bytes.
// salt may be nil for the standard "no salt" mode (HKDF then uses a hash-
// length zero string per RFC 5869 §2.2). info MUST be one of the constants
// above — passing an arbitrary user-controlled string risks domain crossing.
func DeriveKey(ikm, salt []byte, info string, length int) ([]byte, error) {
	if len(ikm) == 0 {
		return nil, fmt.Errorf("%w: empty IKM", ErrHKDFInput)
	}
	if length <= 0 || length > 255*sha256.Size {
		// HKDF-SHA256 max output per RFC 5869 §2.3 is 255 * HashLen = 8160 B.
		return nil, fmt.Errorf("%w: bad length %d", ErrHKDFInput, length)
	}
	r := hkdf.New(sha256.New, ikm, salt, []byte(info))
	out := make([]byte, length)
	if _, err := io.ReadFull(r, out); err != nil {
		return nil, fmt.Errorf("crypto: HKDF expand: %w", err)
	}
	return out, nil
}

// DeriveWrapKey runs the canonical pairing chain documented in spec §11.C:
//
//	wrap_key = HKDF-SHA256(device_pair_secret, salt=nil, info="tether-pair-v1", 32B)
//
// device_pair_secret is the raw 32-byte ECDH output of X25519SharedSecret.
// Equivalent on the Rust side: hkdf::Hkdf::<Sha256>::new(None, &shared)
// .expand(b"tether-pair-v1", &mut wrap_key).
func DeriveWrapKey(devicePairSecret []byte) ([]byte, error) {
	if len(devicePairSecret) != SharedSecretSize {
		return nil, fmt.Errorf("%w: device pair secret must be %d bytes", ErrHKDFInput, SharedSecretSize)
	}
	return DeriveKey(devicePairSecret, nil, HKDFInfoPair, HKDFKeySize)
}
