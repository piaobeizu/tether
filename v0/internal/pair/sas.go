package pair

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"

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

// completeAEADInfo is the §3.4 AD suffix appended to the transcript_hash
// when sealing / opening the pair.complete AEAD tag. Wire-stable —
// changing requires a protocol-version bump.
const completeAEADInfo = "tether-pair-complete-v1"

// ErrCompleteAEAD is returned when pair.complete tag verification fails.
// Treated as ReasonCertError per spec §3.4 / §3.5: a rogue daemon (or
// any in-path actor that survived TLS) cannot forge this tag without
// also deriving the same long_term_key from the same transcript_hash.
var ErrCompleteAEAD = errors.New("pair: pair.complete AEAD tag verification failed")

// completeAEADAd builds the AD = transcript_hash || "tether-pair-complete-v1".
// Both seal and open MUST construct this identically.
func completeAEADAd(transcriptHash []byte) []byte {
	out := make([]byte, 0, len(transcriptHash)+len(completeAEADInfo))
	out = append(out, transcriptHash...)
	out = append(out, []byte(completeAEADInfo)...)
	return out
}

// SealCompleteTag seals an empty plaintext under the long_term_key with
// AD = transcript_hash || "tether-pair-complete-v1" and a freshly
// generated 24-byte XChaCha20 nonce. Returns (nonce, tag) where tag is
// the 16-byte Poly1305 tag (= the entire AEAD ciphertext, since
// plaintext is empty).
//
// Per spec §3.4: this tag is what proves "the daemon emitting
// pair.complete derived the same long_term_key from the same transcript".
// Without it, a forged complete frame trips both endpoints into
// "succeeding" with attacker-mediated keys.
//
// The `randSrc` parameter lets tests inject a deterministic source; pass
// nil for the production crypto/rand.Reader.
func SealCompleteTag(longTermKey, transcriptHash []byte, randSrc io.Reader) (nonce, tag []byte, err error) {
	if len(longTermKey) != chacha20poly1305.KeySize {
		return nil, nil, fmt.Errorf("pair: complete seal: ltk must be %d bytes", chacha20poly1305.KeySize)
	}
	if len(transcriptHash) == 0 {
		return nil, nil, errors.New("pair: complete seal: empty transcript hash")
	}
	aead, err := chacha20poly1305.NewX(longTermKey)
	if err != nil {
		return nil, nil, fmt.Errorf("pair: complete seal: %w", err)
	}
	nonce = make([]byte, chacha20poly1305.NonceSizeX)
	src := randSrc
	if src == nil {
		src = rand.Reader
	}
	if _, err := io.ReadFull(src, nonce); err != nil {
		return nil, nil, fmt.Errorf("pair: complete seal nonce: %w", err)
	}
	ad := completeAEADAd(transcriptHash)
	// Empty plaintext + tag-only output is equivalent to AEAD.Seal(nil,
	// nonce, []byte{}, ad) — golang.org/x/crypto/chacha20poly1305 returns
	// just the 16-byte tag in this case.
	tag = aead.Seal(nil, nonce, []byte{}, ad)
	if len(tag) != chacha20poly1305.Overhead {
		return nil, nil, fmt.Errorf("pair: complete seal: unexpected tag len %d", len(tag))
	}
	return nonce, tag, nil
}

// OpenCompleteTag verifies a (nonce, tag) pair produced by
// SealCompleteTag against the same long_term_key + transcript_hash.
// Returns nil iff the tag authenticates; ErrCompleteAEAD on any
// mismatch (including malformed sizes — defense-in-depth so callers
// don't get a different error class for "bad input" vs "auth fail").
func OpenCompleteTag(longTermKey, transcriptHash, nonce, tag []byte) error {
	if len(longTermKey) != chacha20poly1305.KeySize {
		return ErrCompleteAEAD
	}
	if len(transcriptHash) == 0 {
		return ErrCompleteAEAD
	}
	if len(nonce) != chacha20poly1305.NonceSizeX {
		return ErrCompleteAEAD
	}
	if len(tag) != chacha20poly1305.Overhead {
		return ErrCompleteAEAD
	}
	aead, err := chacha20poly1305.NewX(longTermKey)
	if err != nil {
		return ErrCompleteAEAD
	}
	ad := completeAEADAd(transcriptHash)
	// Open tag-only ciphertext: pass the 16-byte tag as the entire
	// ciphertext; expected plaintext is empty.
	pt, err := aead.Open(nil, nonce, tag, ad)
	if err != nil {
		return ErrCompleteAEAD
	}
	if len(pt) != 0 {
		return ErrCompleteAEAD
	}
	return nil
}
