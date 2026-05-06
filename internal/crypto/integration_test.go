package crypto

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"
)

// TestE2E_PairToEnvelopeRoundTrip walks the full C1 protocol end-to-end,
// modeling the two peers (CLI side + Mobile App side) entirely in-process.
// This is the spec §11.C compliance fingerprint test — if it ever breaks,
// the wire is no longer compatible with the deployed Mobile App.
//
// Steps:
//  1. Both sides generate X25519 keypairs.
//  2. Pair: each side runs CompletePairing with peer's public key →
//     identical wrap_key.
//  3. CLI generates session_key, persists to disk under wrap_key (sim of
//     the Mobile-App-receives-wrapped-session-key step would marshal into
//     a control envelope; we instead reload from disk and prove the disk
//     persistence is reversible — Mobile-side wire-wrap is structurally
//     the same operation: AEAD with a wrap_key-derived key).
//  4. CLI seals an envelope under session_key.
//  5. The "transport" delivers the marshaled bytes (server side sees
//     only this opaque blob) — we marshal then unmarshal to prove the
//     wire format round-trips.
//  6. Receiver runs replay guard, then opens envelope. Plaintext must
//     match.
//  7. Replay attempt with the same envelope-id is rejected.
func TestE2E_PairToEnvelopeRoundTrip(t *testing.T) {
	// ---- 1. keygen ----
	cliKP, err := GenerateX25519KeyPair()
	if err != nil {
		t.Fatalf("cli kp: %v", err)
	}
	mobKP, err := GenerateX25519KeyPair()
	if err != nil {
		t.Fatalf("mob kp: %v", err)
	}

	// ---- 2. pair ----
	cliPair, err := CompletePairing(cliKP.Private[:], mobKP.Public[:])
	if err != nil {
		t.Fatalf("cli pair: %v", err)
	}
	mobPair, err := CompletePairing(mobKP.Private[:], cliKP.Public[:])
	if err != nil {
		t.Fatalf("mob pair: %v", err)
	}
	if !bytes.Equal(cliPair.WrapKey, mobPair.WrapKey) {
		t.Fatal("pair: wrap_keys disagree")
	}
	wrapKey := cliPair.WrapKey

	// User-visible fingerprint mirroring (spec §11.C "scan QR → display
	// fingerprint to user"):
	if cliPair.LocalFingerprint != mobPair.PeerFingerprint ||
		cliPair.PeerFingerprint != mobPair.LocalFingerprint {
		t.Fatal("fingerprints don't mirror — UX confirmation step is broken")
	}

	// ---- 3. session_key + disk persistence ----
	cliSK, err := NewSessionKey()
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	dir := t.TempDir()
	keysPath := filepath.Join(dir, "sessions", "cc-session-1", "keys.bin")
	if err := cliSK.Persist(keysPath, wrapKey); err != nil {
		t.Fatalf("persist: %v", err)
	}

	// Daemon-restart simulation — load it back. (Spec §11.B B4 invariant.)
	cliSK2, err := LoadSessionKey(keysPath, wrapKey)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	keyA, _ := cliSK.Bytes()
	keyB, _ := cliSK2.Bytes()
	if !bytes.Equal(keyA, keyB) {
		t.Fatal("reloaded session_key differs from persisted")
	}
	sessionKey := keyA // both sides use this in-process; in the real
	// system the Mobile App would have received it via a wrap_key-encrypted
	// control envelope at session start.

	// ---- 4. seal ----
	plaintext := []byte("the quick brown fox jumps over the lazy daemon")
	const sessionID = "cc-session-1"
	const fromID = "cli-device-A"
	const toID = "mobile-device-B"
	env, err := SealEnvelope(sessionKey, sessionID, fromID, toID, 1, plaintext)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	// ---- 5. wire round-trip ----
	wire, err := env.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// "Server" can see this blob but learns no plaintext — sanity-check
	// no trivial leak.
	if bytes.Contains(wire, plaintext) {
		t.Fatal("plaintext appears verbatim in wire bytes — encryption broken")
	}

	delivered, err := UnmarshalEnvelope(wire)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// ---- 6. replay guard + open ----
	const envelopeID = "env-uuid-1"
	guard := NewReplayGuard()
	if err := guard.Check(envelopeID, time.Now()); err != nil {
		t.Fatalf("guard fresh: %v", err)
	}
	got, err := OpenEnvelope(sessionKey, delivered)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("plaintext mismatch:\n got: %q\nwant: %q", got, plaintext)
	}

	// ---- 7. replay ----
	if err := guard.Check(envelopeID, time.Now()); err == nil {
		t.Fatal("replay guard accepted the same envelope-id twice")
	}
}

// TestE2E_ServerCanNotReadAfterCompromise simulates the §11.C threat
// model: even with full ciphertext access (the "compromised server"), the
// adversary cannot recover plaintext without a session_key.
func TestE2E_ServerCanNotReadAfterCompromise(t *testing.T) {
	cli, _ := GenerateX25519KeyPair()
	mob, _ := GenerateX25519KeyPair()
	pr, _ := CompletePairing(cli.Private[:], mob.Public[:])
	sk, _ := NewSessionKey()
	keyBytes, _ := sk.Bytes()
	env, _ := SealEnvelope(keyBytes, "s", "f", "t", 1, []byte("secret payload"))
	wire, _ := env.Marshal()

	// "Server" sees `wire` only; it has no session_key.
	parsed, err := UnmarshalEnvelope(wire)
	if err != nil {
		t.Fatalf("server unmarshal: %v", err)
	}
	// Without session_key, server cannot decrypt — even if it tries with
	// wrap_key (which it doesn't have either, but as a paranoid sanity
	// check):
	if _, err := OpenEnvelope(pr.WrapKey, parsed); err == nil {
		t.Fatal("server-known wrap_key should NOT decrypt session-key envelope")
	}
	// Random key obviously doesn't work either:
	bogus := make([]byte, AEADKeySize)
	if _, err := OpenEnvelope(bogus, parsed); err == nil {
		t.Fatal("zero key should NOT decrypt envelope")
	}
}
