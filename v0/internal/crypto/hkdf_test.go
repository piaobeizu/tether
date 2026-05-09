package crypto

import (
	"bytes"
	"errors"
	"testing"
)

func TestDeriveKey_Deterministic(t *testing.T) {
	ikm := bytes.Repeat([]byte{0xAB}, 32)
	out1, err := DeriveKey(ikm, nil, HKDFInfoPair, 32)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	out2, err := DeriveKey(ikm, nil, HKDFInfoPair, 32)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !bytes.Equal(out1, out2) {
		t.Fatal("HKDF must be deterministic for identical inputs")
	}
	if len(out1) != 32 {
		t.Fatalf("len = %d, want 32", len(out1))
	}
}

func TestDeriveKey_DomainSeparation(t *testing.T) {
	ikm := bytes.Repeat([]byte{0x01}, 32)
	pair, err := DeriveKey(ikm, nil, HKDFInfoPair, 32)
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	wrap, err := DeriveKey(ikm, nil, HKDFInfoSessionWrap, 32)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	sas, err := DeriveKey(ikm, nil, HKDFInfoSAS, 32)
	if err != nil {
		t.Fatalf("sas: %v", err)
	}
	if bytes.Equal(pair, wrap) || bytes.Equal(pair, sas) || bytes.Equal(wrap, sas) {
		t.Fatal("different info strings must produce different outputs")
	}
}

func TestDeriveKey_SaltAffectsOutput(t *testing.T) {
	ikm := bytes.Repeat([]byte{0x55}, 32)
	a, err := DeriveKey(ikm, []byte("salt-A"), HKDFInfoPair, 32)
	if err != nil {
		t.Fatalf("a: %v", err)
	}
	b, err := DeriveKey(ikm, []byte("salt-B"), HKDFInfoPair, 32)
	if err != nil {
		t.Fatalf("b: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("different salts must produce different outputs")
	}
}

func TestDeriveKey_Errors(t *testing.T) {
	cases := []struct {
		name   string
		ikm    []byte
		length int
	}{
		{"empty ikm", []byte{}, 32},
		{"zero length", []byte{0x01}, 0},
		{"too long", []byte{0x01}, 255*32 + 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DeriveKey(tc.ikm, nil, HKDFInfoPair, tc.length)
			if !errors.Is(err, ErrHKDFInput) {
				t.Fatalf("want ErrHKDFInput, got %v", err)
			}
		})
	}
}

func TestDeriveWrapKey(t *testing.T) {
	good := bytes.Repeat([]byte{0xCC}, SharedSecretSize)
	wk, err := DeriveWrapKey(good)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if len(wk) != HKDFKeySize {
		t.Fatalf("wrap key length = %d, want %d", len(wk), HKDFKeySize)
	}
	// Wrong size IKM rejected.
	if _, err := DeriveWrapKey([]byte{0x01, 0x02}); !errors.Is(err, ErrHKDFInput) {
		t.Fatalf("short ikm: want ErrHKDFInput, got %v", err)
	}
}
