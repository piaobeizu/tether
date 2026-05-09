package pair

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"
)

// TestSAS_GoldenVector pins the SAS computation against a known
// (shared_secret, transcript_hash) pair. If this test fails, the
// algorithm has drifted from spec §4 — coordinate with the Rust UI
// side before bumping the golden.
func TestSAS_GoldenVector(t *testing.T) {
	// Deterministic 32-byte inputs.
	shared := bytes.Repeat([]byte{0x42}, 32)
	thash := bytes.Repeat([]byte{0x17}, 32)

	sasKey, err := DeriveSASKey(shared, thash)
	if err != nil {
		t.Fatalf("DeriveSASKey: %v", err)
	}
	if len(sasKey) != 32 {
		t.Fatalf("sas_key length: got %d want 32", len(sasKey))
	}

	sas, err := ComputeSAS(sasKey)
	if err != nil {
		t.Fatalf("ComputeSAS: %v", err)
	}
	// Pinned golden: shared=0x42*32, thash=0x17*32, info chain per
	// spec § 4. The actual value is whatever HKDF produces; we
	// generate it once with a well-formed run and freeze it here.
	const wantSAS = "2L5UDX"
	if sas != wantSAS {
		t.Errorf("SAS: got %q want %q (algorithm drift?)", sas, wantSAS)
	}

	// Sanity: 6 chars, all from the pinned alphabet.
	if len(sas) != 6 {
		t.Errorf("SAS length: got %d want 6", len(sas))
	}
	for i, c := range sas {
		if !bytes.ContainsRune([]byte(sasAlphabet), c) {
			t.Errorf("SAS char %d (%c) not in alphabet", i, c)
		}
	}
}

// TestSAS_CrossStackGolden pins the SAS for the SAME input vector the
// Rust client pins (tether-app/src-tauri/src/wt/pair.rs::sas_golden_vector):
//
//	shared_secret  = [0x01; 32]
//	transcript_hash = [0x02; 32]
//	expected SAS   = "J8LNUS"
//
// If this divergence trips on either side, BOTH packages' goldens
// trip simultaneously and the breaking change is caught at CI time.
// This is the BLOCKER-3 fix: previously Go and Rust used different
// inputs so algorithm-level mismatches couldn't be caught by either
// suite alone.
func TestSAS_CrossStackGolden(t *testing.T) {
	shared := bytes.Repeat([]byte{0x01}, 32)
	thash := bytes.Repeat([]byte{0x02}, 32)
	sasKey, err := DeriveSASKey(shared, thash)
	if err != nil {
		t.Fatalf("DeriveSASKey: %v", err)
	}
	sas, err := ComputeSAS(sasKey)
	if err != nil {
		t.Fatalf("ComputeSAS: %v", err)
	}
	const want = "J8LNUS"
	if sas != want {
		t.Errorf("cross-stack SAS golden: got %q want %q — divergence with Rust trips both suites", sas, want)
	}
}

// TestConfirmMAC_CrossStackGolden — pin the HMAC against a vector the
// Rust side mirrors. Inputs match the Rust `confirm_mac_golden` test:
//
//	sas_key        = [0xAA; 32]
//	transcript_hash = [0xBB; 32]
//
// Expected hex digests (initiator + responder) are pinned on both
// sides. Divergence ⇒ cross-stack pair fails MAC verification and
// BOTH suites' goldens trip.
func TestConfirmMAC_CrossStackGolden(t *testing.T) {
	key := bytes.Repeat([]byte{0xAA}, 32)
	thash := bytes.Repeat([]byte{0xBB}, 32)
	initMAC := ConfirmMAC(key, RoleInitiator, thash)
	respMAC := ConfirmMAC(key, RoleResponder, thash)
	const wantInit = "1392c88c645a6088be25b285b47d52a88215b4f82a927e14844fa24f080033dd"
	const wantResp = "3e9f0fe0cb6d6b48aa8d43d47786bbaa5a7bc4c8b308c23abc1bab681d550117"
	gotInit := hex.EncodeToString(initMAC)
	gotResp := hex.EncodeToString(respMAC)
	if gotInit != wantInit {
		t.Errorf("initiator confirm-mac cross-stack golden: got %s want %s", gotInit, wantInit)
	}
	if gotResp != wantResp {
		t.Errorf("responder confirm-mac cross-stack golden: got %s want %s", gotResp, wantResp)
	}
}

// TestSAS_DifferentInputsDifferentSAS — two different transcripts
// produce two different SAS strings. (The whole point of transcript
// binding.)
func TestSAS_DifferentInputsDifferentSAS(t *testing.T) {
	shared := bytes.Repeat([]byte{0x42}, 32)
	t1 := bytes.Repeat([]byte{0x17}, 32)
	t2 := bytes.Repeat([]byte{0x18}, 32)

	k1, _ := DeriveSASKey(shared, t1)
	k2, _ := DeriveSASKey(shared, t2)
	s1, _ := ComputeSAS(k1)
	s2, _ := ComputeSAS(k2)
	if s1 == s2 {
		t.Errorf("SAS collision on different transcripts: %q", s1)
	}
}

// TestConfirmMAC_GoldenVector pins the HMAC label so peers can verify
// each other's MAC byte-identically.
func TestConfirmMAC_GoldenVector(t *testing.T) {
	sasKey := bytes.Repeat([]byte{0x55}, 32)
	thash := bytes.Repeat([]byte{0xAA}, 32)

	// Initiator MAC.
	macI := ConfirmMAC(sasKey, RoleInitiator, thash)
	if len(macI) != 32 {
		t.Errorf("MAC length: got %d want 32", len(macI))
	}
	// Responder MAC over the same key+thash MUST differ — the role
	// label is the disambiguator.
	macR := ConfirmMAC(sasKey, RoleResponder, thash)
	if bytes.Equal(macI, macR) {
		t.Errorf("initiator and responder MAC must differ for the same key+thash")
	}

	// Verify round-trips.
	if err := VerifyConfirmMAC(sasKey, RoleInitiator, thash, macI); err != nil {
		t.Errorf("VerifyConfirmMAC initiator: %v", err)
	}
	if err := VerifyConfirmMAC(sasKey, RoleResponder, thash, macR); err != nil {
		t.Errorf("VerifyConfirmMAC responder: %v", err)
	}
	// Cross verification fails.
	err := VerifyConfirmMAC(sasKey, RoleInitiator, thash, macR)
	if !errors.Is(err, ErrSASMismatch) {
		t.Errorf("cross-role verify: got %v want ErrSASMismatch", err)
	}
}

// TestDeriveLongTermKey_DistinctKeys — long_term_key and
// transport_binding_key must be different bytes (different HKDF info
// strings — spec §8 + §13).
func TestDeriveLongTermKey_DistinctKeys(t *testing.T) {
	shared := bytes.Repeat([]byte{0x42}, 32)
	thash := bytes.Repeat([]byte{0x17}, 32)
	ltk, tbk, err := DeriveLongTermKey(shared, thash)
	if err != nil {
		t.Fatalf("DeriveLongTermKey: %v", err)
	}
	if len(ltk) != 32 || len(tbk) != 32 {
		t.Errorf("key sizes: ltk=%d tbk=%d (want 32/32)", len(ltk), len(tbk))
	}
	if bytes.Equal(ltk, tbk) {
		t.Errorf("ltk and tbk must differ (HKDF info strings should be distinct)")
	}
}

// TestSAS_AlphabetMatchesSpec — the alphabet constant is the exact
// 32-char string from spec §4: "ABCDEFGHJKLMNPQRSTUVWXYZ23456789".
// Spec § 4 prose says "0/O/1/I/L removed" but the literal alphabet it
// gives contains L; the literal is normative for wire compatibility.
// 0/O/1/I are confirmed absent.
func TestSAS_AlphabetMatchesSpec(t *testing.T) {
	const want = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	if sasAlphabet != want {
		t.Errorf("alphabet: got %q want %q", sasAlphabet, want)
	}
	if len(sasAlphabet) != 32 {
		t.Errorf("alphabet length: got %d want 32", len(sasAlphabet))
	}
	for _, c := range []byte{'0', 'O', '1', 'I'} {
		if bytes.ContainsRune([]byte(sasAlphabet), rune(c)) {
			t.Errorf("alphabet must not contain %c (visually confusable)", c)
		}
	}
}
