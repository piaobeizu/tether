package apitoken_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/auth/apitoken"
)

func newStore(t *testing.T) (*apitoken.Store, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "api-tokens.json")
	s, err := apitoken.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return s, path
}

func TestStore_CreateReturnsRawHexAndStoresHash(t *testing.T) {
	s, _ := newStore(t)
	raw, tok, err := s.Create("cursor-laptop")
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 64 {
		t.Fatalf("raw should be 64 hex chars, got %d", len(raw))
	}
	if tok.Name != "cursor-laptop" {
		t.Fatalf("name: %q", tok.Name)
	}
	if tok.Hash == "" || tok.Hash == raw {
		t.Fatalf("hash must be set and != raw")
	}
	if tok.ID == "" {
		t.Fatal("id must be set")
	}
}

func TestStore_CreateRejectsEmptyName(t *testing.T) {
	s, _ := newStore(t)
	for _, name := range []string{"", "   ", "\t\n"} {
		_, _, err := s.Create(name)
		if !errors.Is(err, apitoken.ErrNameRequired) {
			t.Fatalf("Create(%q) → %v; want ErrNameRequired", name, err)
		}
	}
}

func TestStore_CreateRejectsNameTooLong(t *testing.T) {
	s, _ := newStore(t)
	_, _, err := s.Create(strings.Repeat("a", 65))
	if !errors.Is(err, apitoken.ErrNameTooLong) {
		t.Fatalf("Create(65 chars) → %v; want ErrNameTooLong", err)
	}
}

func TestStore_CreateEnforcesMaxTokens(t *testing.T) {
	s, _ := newStore(t)
	for i := 0; i < 100; i++ {
		if _, _, err := s.Create("t"); err != nil {
			t.Fatalf("Create #%d unexpected error: %v", i, err)
		}
	}
	_, _, err := s.Create("overflow")
	if !errors.Is(err, apitoken.ErrTooManyTokens) {
		t.Fatalf("Create past cap → %v; want ErrTooManyTokens", err)
	}
}

func TestStore_ValidateRoundTrip(t *testing.T) {
	s, path := newStore(t)
	raw, _, err := s.Create("name")
	if err != nil {
		t.Fatal(err)
	}
	if !s.Validate(raw) {
		t.Fatal("Validate of freshly-created token must succeed")
	}
	s2, err := apitoken.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.Validate(raw) {
		t.Fatal("Validate after round-trip must succeed")
	}
}

func TestStore_RevokeRemovesEntry(t *testing.T) {
	s, _ := newStore(t)
	raw, tok, _ := s.Create("name")
	if err := s.Revoke(tok.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if s.Validate(raw) {
		t.Fatal("Validate must fail after Revoke")
	}
	if err := s.Revoke(tok.ID); !errors.Is(err, apitoken.ErrNotFound) {
		t.Fatalf("Revoke absent id → %v; want ErrNotFound", err)
	}
}

func TestStore_ListOmitsHashAndRaw(t *testing.T) {
	s, _ := newStore(t)
	raw, _, _ := s.Create("name")
	for _, tok := range s.List() {
		if tok.Hash != "" {
			t.Fatalf("List must not expose Hash field: %+v", tok)
		}
		if strings.Contains(tok.Name, raw) {
			t.Fatal("paranoia: raw token text leaked into Name")
		}
	}
}

func TestStore_FilePermissionsAre0600(t *testing.T) {
	s, path := newStore(t)
	if _, _, err := s.Create("name"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode: %o, want 0600", info.Mode().Perm())
	}
}

func TestStore_OpenIgnoresLeftoverTmpFiles(t *testing.T) {
	dir := t.TempDir()
	stray := filepath.Join(dir, "api-tokens-leftover.json")
	if err := os.WriteFile(stray, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "api-tokens.json")
	s, err := apitoken.Open(path)
	if err != nil {
		t.Fatalf("Open with leftover tmp: %v", err)
	}
	if len(s.List()) != 0 {
		t.Fatal("Open must ignore stray tmp files")
	}
}

func TestStore_OpenCreatesParentDirWith0700(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "deep", "nest")
	path := filepath.Join(nested, "api-tokens.json")
	if _, err := apitoken.Open(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(nested)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("parent dir mode: %o, want 0700", info.Mode().Perm())
	}
}

func TestCreateWithTTL_IssuedTokenIsValid(t *testing.T) {
	dir := t.TempDir()
	s, err := apitoken.Open(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, tok, err := s.CreateWithTTL("cursor", apitoken.TokenSourceOAuth, time.Hour)
	if err != nil {
		t.Fatalf("CreateWithTTL: %v", err)
	}
	if tok.Source != apitoken.TokenSourceOAuth {
		t.Errorf("source = %q, want %q", tok.Source, apitoken.TokenSourceOAuth)
	}
	if tok.ExpiresAt == nil {
		t.Fatal("ExpiresAt should not be nil")
	}
	if !s.Validate(raw) {
		t.Error("fresh token should validate")
	}
}

func TestCreateWithTTL_ExpiredTokenSkipped(t *testing.T) {
	dir := t.TempDir()
	s, err := apitoken.Open(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := s.CreateWithTTL("goose", apitoken.TokenSourceOAuth, -time.Second)
	if err != nil {
		t.Fatalf("CreateWithTTL: %v", err)
	}
	if s.Validate(raw) {
		t.Error("expired token should not validate")
	}
	_, _, ok := s.LookupByRaw(raw)
	if ok {
		t.Error("LookupByRaw should return false for expired token")
	}
}

func TestCreateWithTTL_ManualTokenNeverExpires(t *testing.T) {
	dir := t.TempDir()
	s, err := apitoken.Open(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, tok, err := s.Create("manual")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if tok.ExpiresAt != nil {
		t.Errorf("manual token should have nil ExpiresAt, got %v", tok.ExpiresAt)
	}
	if !s.Validate(raw) {
		t.Error("manual token should always validate")
	}
}

func TestCreateWithTTL_EvictsExpiredAtCap(t *testing.T) {
	dir := t.TempDir()
	s, err := apitoken.Open(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < apitoken.MaxOAuthTokens; i++ {
		if _, _, err := s.CreateWithTTL(fmt.Sprintf("c%d", i), apitoken.TokenSourceOAuth, -time.Second); err != nil {
			t.Fatalf("fill: %v", err)
		}
	}
	if _, _, err := s.CreateWithTTL("new", apitoken.TokenSourceOAuth, time.Hour); err != nil {
		t.Fatalf("post-eviction create: %v", err)
	}
}

func TestCreateWithTTL_ErrStoreFullWhenNoneExpired(t *testing.T) {
	dir := t.TempDir()
	s, err := apitoken.Open(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < apitoken.MaxOAuthTokens; i++ {
		if _, _, err := s.CreateWithTTL(fmt.Sprintf("c%d", i), apitoken.TokenSourceOAuth, time.Hour); err != nil {
			t.Fatalf("fill: %v", err)
		}
	}
	if _, _, err := s.CreateWithTTL("overflow", apitoken.TokenSourceOAuth, time.Hour); !errors.Is(err, apitoken.ErrStoreFull) {
		t.Errorf("want ErrStoreFull, got %v", err)
	}
}
