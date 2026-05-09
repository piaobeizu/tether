package crypto

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// SessionKeySize is the byte length of a per-cc-session AEAD key. Matches
// AEADKeySize so SealEnvelope/OpenEnvelope can use it directly.
const SessionKeySize = AEADKeySize

// ErrSessionKey is returned for session-key operations that fail (wrong
// length, decrypt failure, missing file etc).
var ErrSessionKey = errors.New("crypto: session key error")

// SessionKey is the ephemeral per-cc-session 32-byte AEAD key. Lifetime
// per spec §11.B B4: created when the cc session starts, persisted to
// keys.bin (encrypted under wrap_key) for the duration of the cc session,
// rotated on cc session start (NOT on daemon restart — daemon restart must
// recover the SAME key from disk so server-spooled ciphertext stays
// decryptable). Wiped (RotateSessionKey or DeletePersisted) when the user
// runs `tether kill <id>`.
type SessionKey struct {
	mu      sync.Mutex
	keyVer  uint32
	current []byte // 32B, may be nil after wipe
}

// NewSessionKey allocates a fresh random 32-byte session key. Per spec
// §11.B "session_key 跟 cc session 等长" — call this once at cc session
// start, then Persist() to disk, then reuse the same SessionKey for the
// session's full lifetime.
func NewSessionKey() (*SessionKey, error) {
	return newSessionKeyFrom(rand.Reader)
}

func newSessionKeyFrom(r io.Reader) (*SessionKey, error) {
	buf := make([]byte, SessionKeySize)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("%w: read random: %v", ErrSessionKey, err)
	}
	return &SessionKey{keyVer: 1, current: buf}, nil
}

// Bytes returns a copy of the current key bytes. Callers should pass the
// result directly to SealEnvelope/OpenEnvelope and not retain it.
func (sk *SessionKey) Bytes() ([]byte, error) {
	sk.mu.Lock()
	defer sk.mu.Unlock()
	if sk.current == nil {
		return nil, fmt.Errorf("%w: key wiped", ErrSessionKey)
	}
	out := make([]byte, len(sk.current))
	copy(out, sk.current)
	return out, nil
}

// KeyVersion returns the per-rotation counter (starts at 1, increments on
// each Rotate). Distinct from envelope keyVersion (spec §11.C v0.1=1) —
// this counter is local bookkeeping, never shipped on the wire.
func (sk *SessionKey) KeyVersion() uint32 {
	sk.mu.Lock()
	defer sk.mu.Unlock()
	return sk.keyVer
}

// Rotate replaces the current key with a fresh random one. Per spec §11.B
// rotate is called only on cc-session boundaries (cc session start, or
// `tether kill`) — NOT on daemon restart. Old key bytes are zeroized.
func (sk *SessionKey) Rotate() error {
	return sk.rotateFrom(rand.Reader)
}

func (sk *SessionKey) rotateFrom(r io.Reader) error {
	buf := make([]byte, SessionKeySize)
	if _, err := io.ReadFull(r, buf); err != nil {
		return fmt.Errorf("%w: read random: %v", ErrSessionKey, err)
	}
	sk.mu.Lock()
	defer sk.mu.Unlock()
	zeroize(sk.current)
	sk.current = buf
	sk.keyVer++
	return nil
}

// Wipe zeroizes the in-memory key. After Wipe, Bytes() returns ErrSessionKey
// until Rotate() is called. Use on `tether kill` to make in-memory disk
// scraping useless without touching the on-disk persisted copy
// (DeletePersisted handles that).
func (sk *SessionKey) Wipe() {
	sk.mu.Lock()
	defer sk.mu.Unlock()
	zeroize(sk.current)
	sk.current = nil
}

// --- Persistence (spec §11.B B4) ----------------------------------------
//
// Format of keys.bin on disk (single-version v1 only):
//
//	+----+----+--------+----+------------+----+--------+----+----+
//	| K  | salt(16)   | nonce(24) | keyVerBE(4) | ct_len(4) | ciphertext(N) |
//	+----+------------+-----------+-------------+-----------+---------------+
//	  1B   16B          24B          4B BE          4B BE      N bytes
//
// K = persist version (currently 1).
// salt = per-file random salt fed to HKDF along with wrap_key to derive
//        a unique persistence-AEAD key — defends against off-line
//        cross-file key reuse if wrap_key ever leaks for one device.
// AEAD = XChaCha20-Poly1305 over the raw 32-byte session key bytes,
//        AD = "tether-keysbin-v1".
//
// Cross-Rust interop note: the Mobile App typically stores its own
// session_key copy under iOS Keychain / Android Keystore (see
// tauri-plugin-keyring) and does NOT use this format. This format is
// CLI/daemon-side only. If a future "shared keys.bin" feature lands, the
// Rust side must mirror this exact byte layout.

const (
	persistFormatV1     uint8  = 1
	persistSaltSize            = 16
	persistAEADInfoSalt        = "tether-session-persist-v1"
	persistAEADADString        = "tether-keysbin-v1"
)

// Persist serializes the current SessionKey, encrypts it under a key
// derived from wrap_key + a fresh per-file salt, and writes it atomically
// to path. wrap_key is the 32-byte device pairing key (DeriveWrapKey).
//
// Atomic write: write to "<path>.tmp" then rename — survives crash mid-
// write. Caller is expected to pass a path inside
// ~/.tether/users/default/sessions/<id>/keys.bin (spec §6.1).
func (sk *SessionKey) Persist(path string, wrapKey []byte) error {
	if len(wrapKey) != HKDFKeySize {
		return fmt.Errorf("%w: wrap key must be %d bytes", ErrSessionKey, HKDFKeySize)
	}
	sk.mu.Lock()
	defer sk.mu.Unlock()
	if sk.current == nil {
		return fmt.Errorf("%w: cannot persist wiped key", ErrSessionKey)
	}
	salt := make([]byte, persistSaltSize)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return fmt.Errorf("%w: read salt: %v", ErrSessionKey, err)
	}
	persistKey, err := DeriveKey(wrapKey, salt, HKDFInfoSessionPersist, AEADKeySize)
	if err != nil {
		return fmt.Errorf("%w: derive persist key: %v", ErrSessionKey, err)
	}
	defer zeroize(persistKey)

	nonce := make([]byte, AEADNonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return fmt.Errorf("%w: read nonce: %v", ErrSessionKey, err)
	}
	aead, err := newAEADKey(persistKey)
	if err != nil {
		return err
	}
	ct := aead.Seal(nil, nonce, sk.current, []byte(persistAEADADString))

	buf := make([]byte, 0, 1+persistSaltSize+AEADNonceSize+4+4+len(ct))
	buf = append(buf, persistFormatV1)
	buf = append(buf, salt...)
	buf = append(buf, nonce...)
	buf = appendUint32BE(buf, sk.keyVer)
	buf = appendUint32BE(buf, uint32(len(ct)))
	buf = append(buf, ct...)

	if err := atomicWriteFile(path, buf, 0o600); err != nil {
		return fmt.Errorf("%w: write keys.bin: %v", ErrSessionKey, err)
	}
	return nil
}

// LoadSessionKey is the inverse of Persist — used by daemon restart logic
// (spec §11.B B4: recover session_key from disk so the daemon can decrypt
// server-spooled ciphertext from the still-running cc session).
func LoadSessionKey(path string, wrapKey []byte) (*SessionKey, error) {
	if len(wrapKey) != HKDFKeySize {
		return nil, fmt.Errorf("%w: wrap key must be %d bytes", ErrSessionKey, HKDFKeySize)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: read keys.bin: %v", ErrSessionKey, err)
	}
	r := newByteReader(raw)
	ver, err := r.readByte()
	if err != nil {
		return nil, fmt.Errorf("%w: format version: %v", ErrSessionKey, err)
	}
	if ver != persistFormatV1 {
		return nil, fmt.Errorf("%w: unsupported persist format %d", ErrSessionKey, ver)
	}
	salt, err := r.readN(persistSaltSize)
	if err != nil {
		return nil, fmt.Errorf("%w: salt: %v", ErrSessionKey, err)
	}
	nonce, err := r.readN(AEADNonceSize)
	if err != nil {
		return nil, fmt.Errorf("%w: nonce: %v", ErrSessionKey, err)
	}
	keyVer, err := r.readUint32()
	if err != nil {
		return nil, fmt.Errorf("%w: keyVer: %v", ErrSessionKey, err)
	}
	ctLen, err := r.readUint32()
	if err != nil {
		return nil, fmt.Errorf("%w: ctLen: %v", ErrSessionKey, err)
	}
	ct, err := r.readN(int(ctLen))
	if err != nil {
		return nil, fmt.Errorf("%w: ciphertext: %v", ErrSessionKey, err)
	}
	if r.remaining() != 0 {
		return nil, fmt.Errorf("%w: trailing bytes", ErrSessionKey)
	}
	persistKey, err := DeriveKey(wrapKey, salt, HKDFInfoSessionPersist, AEADKeySize)
	if err != nil {
		return nil, fmt.Errorf("%w: derive persist key: %v", ErrSessionKey, err)
	}
	defer zeroize(persistKey)
	aead, err := newAEADKey(persistKey)
	if err != nil {
		return nil, err
	}
	pt, err := aead.Open(nil, nonce, ct, []byte(persistAEADADString))
	if err != nil {
		return nil, fmt.Errorf("%w: decrypt session key: %v", ErrSessionKey, err)
	}
	if len(pt) != SessionKeySize {
		return nil, fmt.Errorf("%w: bad session key length %d", ErrSessionKey, len(pt))
	}
	return &SessionKey{keyVer: keyVer, current: pt}, nil
}

// DeletePersisted removes the keys.bin file (spec §11.C: "cc session 结束 →
// CLI 持久化清掉 session_key"). Idempotent — missing file is not an error.
func DeletePersisted(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("%w: remove keys.bin: %v", ErrSessionKey, err)
	}
	return nil
}

// --- helpers ---

func zeroize(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func appendUint32BE(buf []byte, v uint32) []byte {
	tmp := [4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
	return append(buf, tmp[:]...)
}

// atomicWriteFile writes data to <path>.tmp then renames to path so a
// crash mid-write leaves the previous file intact (spec §11.C / §6.1
// persistence invariant).
func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
