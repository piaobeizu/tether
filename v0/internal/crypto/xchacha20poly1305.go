package crypto

import (
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
)

// newAEADKey wraps chacha20poly1305.NewX into our error vocabulary so other
// files (session.go, pairing.go) can reuse it without re-importing the
// chacha20poly1305 package.
func newAEADKey(key []byte) (cipher.AEAD, error) {
	if len(key) != AEADKeySize {
		return nil, fmt.Errorf("%w: AEAD key must be %d bytes", ErrAEAD, AEADKeySize)
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("%w: new XChaCha20-Poly1305: %v", ErrAEAD, err)
	}
	return aead, nil
}

// AEAD constants — XChaCha20-Poly1305 per RFC 8439 / draft-irtf-cfrg-xchacha.
const (
	AEADKeySize   = chacha20poly1305.KeySize   // 32
	AEADNonceSize = chacha20poly1305.NonceSizeX // 24
	AEADTagSize   = chacha20poly1305.Overhead   // 16
)

// ErrAEAD is returned when AEAD seal/open fails or inputs are malformed.
// Keep distinct from ErrInvalidX25519Key so callers can branch on cause.
var ErrAEAD = errors.New("crypto: AEAD failure")

// Envelope is the wire format produced by SealEnvelope. It is the unit of
// encrypted data the daemon hands to the server (which only sees the
// metadata fields and the opaque Ciphertext).
//
// # Wire serialization layout (Cross-Rust interop contract)
//
// Big-endian throughout. The exact byte sequence Marshal() produces is:
//
//	+----+----+--------+----+--------+----+--------+----+----+----+----+--------+----+--------+
//	| K  | S_len      | sessionId | F_len | fromDeviceId | T_len | toDeviceId  | keyVersion | nonce(24) | ct_len(4) | ciphertext |
//	+----+------------+-----------+-------+--------------+-------+-------------+------------+-----------+-----------+------------+
//	  1B   2B (BE)     S_len B     2B BE   F_len B        2B BE   T_len B       4B BE         24B         4B BE       ct_len B
//
// Where K = wire format version (currently 1).
//
// The same byte layout is what the Tauri Mobile (Rust) side must emit/parse
// to interoperate. Recommended Rust impl: bincode is NOT compatible — write
// a hand-rolled encoder using `byteorder::BigEndian` + length-prefixed
// strings. The 4-byte ciphertext length covers ciphertext+tag (Poly1305
// tag is appended by AEAD.seal as the last 16 bytes — Rust's
// chacha20poly1305 crate does the same).
//
// Ciphertext = XChaCha20-Poly1305.Seal(session_key, Nonce, plaintext, AD)
// where AD is the deterministic concatenation BuildAD produces. Both sides
// MUST construct AD identically.
type Envelope struct {
	WireVersion  uint8 // currently 1
	SessionID    string
	FromDeviceID string
	ToDeviceID   string
	KeyVersion   uint32 // spec §11.C v0.1 = 1; bump on algo upgrade
	Nonce        [AEADNonceSize]byte
	Ciphertext   []byte // includes 16B Poly1305 tag suffix
}

// EnvelopeWireV1 is the current wire format version. Bumping this requires
// dual-write/read fallback during the cutover window.
const EnvelopeWireV1 uint8 = 1

// BuildAD constructs the deterministic Additional Data for an envelope.
// Per spec §11.C:
//
//	AD = sessionId || fromDeviceId || toDeviceId || keyVersion(BE)
//
// We use length-prefixed concatenation (each 2-byte BE length + bytes,
// keyVersion as raw 4B BE) to make the construction unambiguous — naive
// concatenation would let an attacker shift bytes between the IDs to
// produce equivalent AD for different routing.
//
// Rust mirror: same 2B BE length prefixes, same UTF-8 string bytes,
// same trailing 4B BE keyVersion.
func BuildAD(sessionID, fromDeviceID, toDeviceID string, keyVersion uint32) []byte {
	out := make([]byte, 0, 2+len(sessionID)+2+len(fromDeviceID)+2+len(toDeviceID)+4)
	out = appendLPString(out, sessionID)
	out = appendLPString(out, fromDeviceID)
	out = appendLPString(out, toDeviceID)
	out = binary.BigEndian.AppendUint32(out, keyVersion)
	return out
}

// SealEnvelope encrypts plaintext under sessionKey, producing an Envelope
// with a freshly-generated 24-byte random nonce.
//
// sessionKey MUST be 32 bytes (typically a session_key from session.go).
// keyVersion is currently always 1 — see spec §11.C.
//
// On success the returned Envelope can be wire-marshaled by Marshal and
// fed into the queue layer. Server peers MUST NOT attempt to open it.
func SealEnvelope(sessionKey []byte, sessionID, fromDeviceID, toDeviceID string, keyVersion uint32, plaintext []byte) (*Envelope, error) {
	if len(sessionKey) != AEADKeySize {
		return nil, fmt.Errorf("%w: session key must be %d bytes", ErrAEAD, AEADKeySize)
	}
	aead, err := chacha20poly1305.NewX(sessionKey)
	if err != nil {
		return nil, fmt.Errorf("%w: new XChaCha20-Poly1305: %v", ErrAEAD, err)
	}
	env := &Envelope{
		WireVersion:  EnvelopeWireV1,
		SessionID:    sessionID,
		FromDeviceID: fromDeviceID,
		ToDeviceID:   toDeviceID,
		KeyVersion:   keyVersion,
	}
	if _, err := io.ReadFull(rand.Reader, env.Nonce[:]); err != nil {
		return nil, fmt.Errorf("%w: read nonce: %v", ErrAEAD, err)
	}
	ad := BuildAD(sessionID, fromDeviceID, toDeviceID, keyVersion)
	env.Ciphertext = aead.Seal(nil, env.Nonce[:], plaintext, ad)
	return env, nil
}

// OpenEnvelope verifies + decrypts an Envelope produced by SealEnvelope (or
// the Rust equivalent). Returns the plaintext bytes on success. Auth
// failure surfaces as a wrapped ErrAEAD — caller MUST treat this as a hard
// failure (do NOT log the ciphertext, do NOT retry with a different key).
func OpenEnvelope(sessionKey []byte, env *Envelope) ([]byte, error) {
	if env == nil {
		return nil, fmt.Errorf("%w: nil envelope", ErrAEAD)
	}
	if len(sessionKey) != AEADKeySize {
		return nil, fmt.Errorf("%w: session key must be %d bytes", ErrAEAD, AEADKeySize)
	}
	if len(env.Ciphertext) < AEADTagSize {
		return nil, fmt.Errorf("%w: ciphertext too short", ErrAEAD)
	}
	aead, err := chacha20poly1305.NewX(sessionKey)
	if err != nil {
		return nil, fmt.Errorf("%w: new XChaCha20-Poly1305: %v", ErrAEAD, err)
	}
	ad := BuildAD(env.SessionID, env.FromDeviceID, env.ToDeviceID, env.KeyVersion)
	pt, err := aead.Open(nil, env.Nonce[:], env.Ciphertext, ad)
	if err != nil {
		return nil, fmt.Errorf("%w: open: %v", ErrAEAD, err)
	}
	return pt, nil
}

// Marshal serializes an Envelope to the wire bytes documented above.
// Cross-Rust interop: the inverse operation on the Rust side reads the
// fields in this exact order using big-endian length prefixes.
func (e *Envelope) Marshal() ([]byte, error) {
	if e == nil {
		return nil, fmt.Errorf("%w: nil envelope", ErrAEAD)
	}
	if len(e.SessionID) > 0xFFFF || len(e.FromDeviceID) > 0xFFFF || len(e.ToDeviceID) > 0xFFFF {
		return nil, fmt.Errorf("%w: identifier length exceeds 65535", ErrAEAD)
	}
	if len(e.Ciphertext) > 0xFFFFFFFF {
		return nil, fmt.Errorf("%w: ciphertext length exceeds uint32 max", ErrAEAD)
	}
	out := make([]byte, 0, 1+2+len(e.SessionID)+2+len(e.FromDeviceID)+2+len(e.ToDeviceID)+4+AEADNonceSize+4+len(e.Ciphertext))
	out = append(out, e.WireVersion)
	out = appendLPString(out, e.SessionID)
	out = appendLPString(out, e.FromDeviceID)
	out = appendLPString(out, e.ToDeviceID)
	out = binary.BigEndian.AppendUint32(out, e.KeyVersion)
	out = append(out, e.Nonce[:]...)
	out = binary.BigEndian.AppendUint32(out, uint32(len(e.Ciphertext)))
	out = append(out, e.Ciphertext...)
	return out, nil
}

// UnmarshalEnvelope is the inverse of Envelope.Marshal. Rejects unknown
// wire versions early so a future format bump doesn't silently corrupt.
func UnmarshalEnvelope(buf []byte) (*Envelope, error) {
	r := newByteReader(buf)
	ver, err := r.readByte()
	if err != nil {
		return nil, fmt.Errorf("%w: read wire version: %v", ErrAEAD, err)
	}
	if ver != EnvelopeWireV1 {
		return nil, fmt.Errorf("%w: unsupported wire version %d", ErrAEAD, ver)
	}
	env := &Envelope{WireVersion: ver}
	if env.SessionID, err = r.readLPString(); err != nil {
		return nil, fmt.Errorf("%w: sessionId: %v", ErrAEAD, err)
	}
	if env.FromDeviceID, err = r.readLPString(); err != nil {
		return nil, fmt.Errorf("%w: fromDeviceId: %v", ErrAEAD, err)
	}
	if env.ToDeviceID, err = r.readLPString(); err != nil {
		return nil, fmt.Errorf("%w: toDeviceId: %v", ErrAEAD, err)
	}
	if env.KeyVersion, err = r.readUint32(); err != nil {
		return nil, fmt.Errorf("%w: keyVersion: %v", ErrAEAD, err)
	}
	nonceBytes, err := r.readN(AEADNonceSize)
	if err != nil {
		return nil, fmt.Errorf("%w: nonce: %v", ErrAEAD, err)
	}
	copy(env.Nonce[:], nonceBytes)
	ctLen, err := r.readUint32()
	if err != nil {
		return nil, fmt.Errorf("%w: ciphertext length: %v", ErrAEAD, err)
	}
	if env.Ciphertext, err = r.readN(int(ctLen)); err != nil {
		return nil, fmt.Errorf("%w: ciphertext body: %v", ErrAEAD, err)
	}
	if r.remaining() != 0 {
		return nil, fmt.Errorf("%w: trailing bytes after ciphertext", ErrAEAD)
	}
	return env, nil
}

// --- internal helpers ---

func appendLPString(out []byte, s string) []byte {
	out = binary.BigEndian.AppendUint16(out, uint16(len(s)))
	out = append(out, s...)
	return out
}

type byteReader struct {
	buf []byte
	pos int
}

func newByteReader(b []byte) *byteReader { return &byteReader{buf: b} }

func (r *byteReader) remaining() int { return len(r.buf) - r.pos }

func (r *byteReader) readByte() (byte, error) {
	if r.remaining() < 1 {
		return 0, io.ErrUnexpectedEOF
	}
	b := r.buf[r.pos]
	r.pos++
	return b, nil
}

func (r *byteReader) readN(n int) ([]byte, error) {
	if n < 0 || r.remaining() < n {
		return nil, io.ErrUnexpectedEOF
	}
	out := r.buf[r.pos : r.pos+n]
	r.pos += n
	return out, nil
}

func (r *byteReader) readUint16() (uint16, error) {
	b, err := r.readN(2)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(b), nil
}

func (r *byteReader) readUint32() (uint32, error) {
	b, err := r.readN(4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b), nil
}

func (r *byteReader) readLPString() (string, error) {
	n, err := r.readUint16()
	if err != nil {
		return "", err
	}
	body, err := r.readN(int(n))
	if err != nil {
		return "", err
	}
	return string(body), nil
}
