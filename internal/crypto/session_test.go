package crypto

import (
	"bytes"
	"crypto/rand"
	"errors"
	"path/filepath"
	"testing"
)

func TestNewSessionKey_RandomAndCorrectLen(t *testing.T) {
	a, err := NewSessionKey()
	if err != nil {
		t.Fatalf("a: %v", err)
	}
	b, err := NewSessionKey()
	if err != nil {
		t.Fatalf("b: %v", err)
	}
	ka, _ := a.Bytes()
	kb, _ := b.Bytes()
	if len(ka) != SessionKeySize {
		t.Fatalf("len = %d", len(ka))
	}
	if bytes.Equal(ka, kb) {
		t.Fatal("two random session keys collided — RNG bug")
	}
	if a.KeyVersion() != 1 {
		t.Fatalf("initial keyVer = %d, want 1", a.KeyVersion())
	}
}

func TestSessionKey_RotateInvalidatesOld(t *testing.T) {
	sk, err := NewSessionKey()
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	old, _ := sk.Bytes()

	// Encrypt under old key.
	env, err := SealEnvelope(old, "s", "f", "t", 1, []byte("payload"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	if err := sk.Rotate(); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if sk.KeyVersion() != 2 {
		t.Fatalf("post-rotate keyVer = %d, want 2", sk.KeyVersion())
	}
	newKey, _ := sk.Bytes()
	if bytes.Equal(old, newKey) {
		t.Fatal("rotate produced same key as before")
	}

	// New key must NOT decrypt envelope encrypted under old key.
	if _, err := OpenEnvelope(newKey, env); !errors.Is(err, ErrAEAD) {
		t.Fatalf("rotate didn't invalidate old ciphertext: %v", err)
	}
}

func TestSessionKey_WipeBlocksBytes(t *testing.T) {
	sk, err := NewSessionKey()
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	sk.Wipe()
	if _, err := sk.Bytes(); !errors.Is(err, ErrSessionKey) {
		t.Fatalf("want ErrSessionKey after wipe, got %v", err)
	}
}

func freshWrapKey(t *testing.T) []byte {
	t.Helper()
	wk := make([]byte, HKDFKeySize)
	if _, err := rand.Read(wk); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return wk
}

func TestSessionKey_PersistAndLoad(t *testing.T) {
	wk := freshWrapKey(t)
	sk, err := NewSessionKey()
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	want, _ := sk.Bytes()

	dir := t.TempDir()
	path := filepath.Join(dir, "sessions", "abc", "keys.bin")
	if err := sk.Persist(path, wk); err != nil {
		t.Fatalf("persist: %v", err)
	}

	loaded, err := LoadSessionKey(path, wk)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got, _ := loaded.Bytes()
	if !bytes.Equal(want, got) {
		t.Fatal("loaded session key differs from persisted")
	}
	if loaded.KeyVersion() != sk.KeyVersion() {
		t.Fatalf("keyVer mismatch: %d vs %d", loaded.KeyVersion(), sk.KeyVersion())
	}
}

func TestSessionKey_LoadWithWrongWrapKeyFails(t *testing.T) {
	wk1 := freshWrapKey(t)
	wk2 := freshWrapKey(t)
	sk, _ := NewSessionKey()
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.bin")
	if err := sk.Persist(path, wk1); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if _, err := LoadSessionKey(path, wk2); !errors.Is(err, ErrSessionKey) {
		t.Fatalf("want ErrSessionKey on wrong wrap key, got %v", err)
	}
}

func TestSessionKey_LoadCorruptedFails(t *testing.T) {
	wk := freshWrapKey(t)
	sk, _ := NewSessionKey()
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.bin")
	if err := sk.Persist(path, wk); err != nil {
		t.Fatalf("persist: %v", err)
	}
	// Truncate the on-disk file.
	if err := writeBytes(path, []byte{0x01, 0x02, 0x03}); err != nil {
		t.Fatalf("trunc: %v", err)
	}
	if _, err := LoadSessionKey(path, wk); !errors.Is(err, ErrSessionKey) {
		t.Fatalf("want ErrSessionKey on corrupt file, got %v", err)
	}
}

func TestSessionKey_PersistRotateLoadRoundTrip(t *testing.T) {
	// Spec §11.B B4 fingerprint: rotate (cc session start) → persist →
	// daemon "restart" (LoadSessionKey) recovers the SAME post-rotate key.
	wk := freshWrapKey(t)
	sk, _ := NewSessionKey()
	if err := sk.Rotate(); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	postRotate, _ := sk.Bytes()
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.bin")
	if err := sk.Persist(path, wk); err != nil {
		t.Fatalf("persist: %v", err)
	}
	loaded, err := LoadSessionKey(path, wk)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got, _ := loaded.Bytes()
	if !bytes.Equal(postRotate, got) {
		t.Fatal("post-rotate key not recovered from disk — daemon-restart unsafe")
	}
}

func TestDeletePersisted_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.bin")
	// Missing file is OK.
	if err := DeletePersisted(path); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
	// Create then delete.
	wk := freshWrapKey(t)
	sk, _ := NewSessionKey()
	if err := sk.Persist(path, wk); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if err := DeletePersisted(path); err != nil {
		t.Fatalf("delete present: %v", err)
	}
	if _, err := LoadSessionKey(path, wk); !errors.Is(err, ErrSessionKey) {
		t.Fatalf("post-delete load should fail, got %v", err)
	}
}

// writeBytes overwrites a file with raw content for corruption tests.
func writeBytes(path string, b []byte) error {
	return atomicWriteFile(path, b, 0o600)
}
