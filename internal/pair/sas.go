package pair

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/piaobeizu/tether/internal/crypto"
)

// SAS algorithm pinning — these labels are wire-stable. Changing any
// of them requires a protocol-version bump.

const (
	// hkdfInfoSAS derives the 32-byte sas_key (also reused as the MAC
	// key for SAS-confirm) from the X25519 shared secret. Spec §4 step
	// 1.
	hkdfInfoSAS = "tether-sas-v1"

	// hkdfInfoSASDisplay derives the 4 raw bytes that are masked to 30
	// bits and base32-encoded for the user-visible SAS string. Spec §4
	// step 2.
	hkdfInfoSASDisplay = "tether-sas-display-v1"

	// hkdfInfoLTK derives the 32-byte long_term_key (the §11.C wrap_key
	// input). Spec §8.
	hkdfInfoLTK = "tether-ltk-v1"

	// hkdfInfoTBK derives the 32-byte transport_binding_key (reserved
	// for v0.1.x channel-binding). Spec §8 + §13 OQ-1.
	hkdfInfoTBK = "tether-tbk-v1"

	// confirmMACLabelPrefix is prepended to the role + transcript_hash
	// when computing the SAS-confirm MAC. Per spec §3.3 + §2.1: HMAC
	// input is "tether-pair-confirm-v1|" || role || "|" || transcript_hash.
	confirmMACLabelPrefix = "tether-pair-confirm-v1"
)

// sasAlphabet is the 32-character RFC 4648 base32 alphabet minus the
// visually-confusable characters 0/O/1/I/L (spec §4 step 3).
const sasAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// ErrSASMismatch is returned when a peer's pair.sas-confirm MAC does
// not validate against the locally-derived sas_key + transcript_hash.
// This is the active-MITM detection path.
var ErrSASMismatch = errors.New("pair: SAS mismatch (active MITM suspected)")

// ComputeSharedSecret runs X25519 ECDH between our private scalar and
// the peer's public key. Wraps internal/crypto/X25519SharedSecret to
// keep imports of the lower-level crypto package localized to this
// file.
func ComputeSharedSecret(myPriv, peerPub []byte) ([]byte, error) {
	return crypto.X25519SharedSecret(myPriv, peerPub)
}

// DeriveSASKey computes:
//
//	sas_key = HKDF-SHA256(ikm=shared_secret, salt=transcript_hash,
//	                     info="tether-sas-v1", L=32)
//
// per spec §4 step 1. The output is reused as the HMAC key for the
// SAS-confirm MACs.
func DeriveSASKey(shared, transcriptHash []byte) ([]byte, error) {
	if len(shared) == 0 {
		return nil, errors.New("pair: empty shared secret")
	}
	if len(transcriptHash) == 0 {
		return nil, errors.New("pair: empty transcript hash")
	}
	return crypto.DeriveKey(shared, transcriptHash, hkdfInfoSAS, 32)
}

// ComputeSAS runs spec §4 step 2 + step 3:
//
//   - Expand sas_key with info="tether-sas-display-v1" to 4 bytes.
//   - Mask low 30 bits of the big-endian uint32.
//   - Encode as 6 base32 chars (high → low) using the spec alphabet.
func ComputeSAS(sasKey []byte) (string, error) {
	bits, err := crypto.DeriveKey(sasKey, nil, hkdfInfoSASDisplay, 4)
	if err != nil {
		return "", fmt.Errorf("pair: derive sas display: %w", err)
	}
	v := binary.BigEndian.Uint32(bits) & 0x3FFFFFFF // low 30 bits
	out := make([]byte, 6)
	out[0] = sasAlphabet[(v>>25)&0x1F]
	out[1] = sasAlphabet[(v>>20)&0x1F]
	out[2] = sasAlphabet[(v>>15)&0x1F]
	out[3] = sasAlphabet[(v>>10)&0x1F]
	out[4] = sasAlphabet[(v>>5)&0x1F]
	out[5] = sasAlphabet[v&0x1F]
	return string(out), nil
}

// ConfirmMAC computes:
//
//	HMAC-SHA256(sas_key,
//	            "tether-pair-confirm-v1" || "|" || role || "|" || transcript_hash)
//
// Per spec §3.3: each side's MAC binds its role label + the same
// transcript_hash; a peer that did not see the same transcript / does
// not hold the same sas_key cannot produce a valid MAC.
func ConfirmMAC(sasKey []byte, role Role, transcriptHash []byte) []byte {
	h := hmac.New(sha256.New, sasKey)
	_, _ = h.Write([]byte(confirmMACLabelPrefix))
	_, _ = h.Write([]byte("|"))
	_, _ = h.Write([]byte(string(role)))
	_, _ = h.Write([]byte("|"))
	_, _ = h.Write(transcriptHash)
	return h.Sum(nil)
}

// VerifyConfirmMAC returns nil iff peerMAC is the byte-identical HMAC
// for the given (sasKey, role, transcriptHash). Constant-time compare.
func VerifyConfirmMAC(sasKey []byte, role Role, transcriptHash, peerMAC []byte) error {
	expected := ConfirmMAC(sasKey, role, transcriptHash)
	if !hmac.Equal(expected, peerMAC) {
		return ErrSASMismatch
	}
	return nil
}

// DeriveLongTermKey runs spec §8: derives the long_term_key (32B) and
// the transport_binding_key (32B) from the same (shared_secret,
// transcript_hash) pair, with distinct HKDF info strings so the two
// keys never collide.
func DeriveLongTermKey(shared, transcriptHash []byte) (longTermKey, transportBindingKey []byte, err error) {
	if len(shared) == 0 {
		return nil, nil, errors.New("pair: empty shared secret")
	}
	if len(transcriptHash) == 0 {
		return nil, nil, errors.New("pair: empty transcript hash")
	}
	ltk, err := crypto.DeriveKey(shared, transcriptHash, hkdfInfoLTK, 32)
	if err != nil {
		return nil, nil, fmt.Errorf("pair: derive ltk: %w", err)
	}
	tbk, err := crypto.DeriveKey(shared, transcriptHash, hkdfInfoTBK, 32)
	if err != nil {
		return nil, nil, fmt.Errorf("pair: derive tbk: %w", err)
	}
	return ltk, tbk, nil
}
