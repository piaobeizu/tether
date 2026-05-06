package crypto

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
)

// ErrPairing is returned for pairing-flow failures (bad key sizes, SAS
// mismatch, fingerprint mismatch).
var ErrPairing = errors.New("crypto: pairing failure")

// FingerprintPrefixLen / FingerprintSuffixLen control the visual fingerprint
// shown to users on QR scan: prefix-of-pubkey-hash + "..." +
// suffix-of-pubkey-hash. Per spec §11.C: "前 4 字节 + 后 4 字节 hex".
const (
	FingerprintPrefixLen = 4
	FingerprintSuffixLen = 4
)

// SASDigits is the number of decimal digits in the Short Authentication
// String. Per spec §11.C: "6 位 short authentication string".
const SASDigits = 6

// Fingerprint is the user-facing identifier shown on both ends of a QR
// pairing flow. Per spec §11.C the user visually confirms it matches what
// the other side displays.
//
// Encoding: hex(sha256(pubkey)[:FingerprintPrefixLen]) + ":" +
//           hex(sha256(pubkey)[-FingerprintSuffixLen:])
//
// Cross-Rust interop: Tauri Mobile must compute the same SHA-256 over the
// raw 32-byte X25519 public key and slice the same prefix/suffix. The
// `:` separator is part of the canonical form — do not localize.
func Fingerprint(publicKey []byte) (string, error) {
	if len(publicKey) != X25519KeySize {
		return "", fmt.Errorf("%w: public key must be %d bytes", ErrPairing, X25519KeySize)
	}
	h := sha256.Sum256(publicKey)
	prefix := hex.EncodeToString(h[:FingerprintPrefixLen])
	suffix := hex.EncodeToString(h[sha256.Size-FingerprintSuffixLen:])
	return prefix + ":" + suffix, nil
}

// VerifyFingerprint constant-time compares a user-presented fingerprint
// against the one computed for publicKey. Returns nil iff the fingerprints
// match exactly.
func VerifyFingerprint(publicKey []byte, claimed string) error {
	expected, err := Fingerprint(publicKey)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(expected), []byte(claimed)) != 1 {
		return fmt.Errorf("%w: fingerprint mismatch", ErrPairing)
	}
	return nil
}

// ComputeSAS derives the 6-digit Short Authentication String from the raw
// X25519 shared secret. Per spec §11.C:
//
//	SAS = HKDF(shared_secret, "tether-pair-sas") taken as a 6-digit decimal
//
// We expand 4 bytes from HKDF and reduce mod 10^SASDigits — biased but
// negligibly so for 6 digits (worst-case bias < 1/10^7). Both sides MUST
// derive the same SAS independently and the user reads them aloud to
// confirm match — defends against MITM in the server-mediated fallback
// pairing path (which is dev/debug-only per spec §11.C).
func ComputeSAS(sharedSecret []byte) (string, error) {
	if len(sharedSecret) != SharedSecretSize {
		return "", fmt.Errorf("%w: shared secret must be %d bytes", ErrPairing, SharedSecretSize)
	}
	derived, err := DeriveKey(sharedSecret, nil, HKDFInfoSAS, 4)
	if err != nil {
		return "", fmt.Errorf("%w: derive SAS: %v", ErrPairing, err)
	}
	n := binary.BigEndian.Uint32(derived)
	mod := uint32(1)
	for i := 0; i < SASDigits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", SASDigits, n%mod), nil
}

// VerifySAS constant-time compares the locally-computed SAS for
// sharedSecret against what the user entered (typically read aloud from
// the peer device). Returns nil iff they match.
func VerifySAS(sharedSecret []byte, userEntered string) error {
	expected, err := ComputeSAS(sharedSecret)
	if err != nil {
		return err
	}
	if len(expected) != len(userEntered) {
		return fmt.Errorf("%w: SAS length mismatch", ErrPairing)
	}
	if subtle.ConstantTimeCompare([]byte(expected), []byte(userEntered)) != 1 {
		return fmt.Errorf("%w: SAS mismatch", ErrPairing)
	}
	return nil
}

// PairResult is the output of CompletePairing — the wrap_key both sides
// will keep long-term, plus the local fingerprint that should be shown to
// the user as a final visual sanity check (the Mobile app can also display
// the CLI-side fingerprint computed from the peer pubkey).
//
// PeerFingerprint is what the LOCAL side computes for the PEER's public
// key — both peers must show the same value for each peer's pubkey, so
// the two devices end up displaying mirrored pairs.
type PairResult struct {
	WrapKey         []byte
	LocalFingerprint string
	PeerFingerprint  string
}

// CompletePairing runs the full pairing derivation given an already-
// generated local keypair and the peer's public key (typically scanned from
// QR). On success returns the wrap_key both sides share, plus fingerprints
// for user confirmation.
//
// Cross-Rust interop: the Mobile App runs the same X25519 ECDH and the
// same DeriveWrapKey HKDF chain. Result wrap_key bytes are byte-identical
// on both sides — that is the entire point of the protocol.
func CompletePairing(localPrivate, peerPublic []byte) (*PairResult, error) {
	if len(localPrivate) != X25519KeySize || len(peerPublic) != X25519KeySize {
		return nil, fmt.Errorf("%w: keys must be %d bytes each", ErrPairing, X25519KeySize)
	}
	shared, err := X25519SharedSecret(localPrivate, peerPublic)
	if err != nil {
		return nil, fmt.Errorf("%w: ECDH: %v", ErrPairing, err)
	}
	wrap, err := DeriveWrapKey(shared)
	if err != nil {
		return nil, fmt.Errorf("%w: derive wrap key: %v", ErrPairing, err)
	}
	// Local fingerprint = fingerprint of OUR pubkey (Mobile would show this
	// as "the CLI's fingerprint"); we recover our public from the private.
	localPub, err := derivePublic(localPrivate)
	if err != nil {
		return nil, fmt.Errorf("%w: derive local public: %v", ErrPairing, err)
	}
	localFp, err := Fingerprint(localPub)
	if err != nil {
		return nil, err
	}
	peerFp, err := Fingerprint(peerPublic)
	if err != nil {
		return nil, err
	}
	return &PairResult{
		WrapKey:          wrap,
		LocalFingerprint: localFp,
		PeerFingerprint:  peerFp,
	}, nil
}

func derivePublic(private []byte) ([]byte, error) {
	if len(private) != X25519KeySize {
		return nil, ErrInvalidX25519Key
	}
	// Reuse curve25519 via the package helper — call into the same path
	// X25519SharedSecret uses but with the basepoint (which is what
	// generateX25519KeyPairFrom does internally). We re-export through
	// X25519SharedSecret using the curve25519.Basepoint constant.
	return basepointMul(private)
}
