package apitoken

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	MaxTokens  = 100
	MaxNameLen = 64
)

var (
	ErrNameRequired  = errors.New("apitoken: name required")
	ErrNameTooLong   = fmt.Errorf("apitoken: name exceeds %d chars", MaxNameLen)
	ErrTooManyTokens = fmt.Errorf("apitoken: store full (max %d)", MaxTokens)
	ErrNotFound      = errors.New("apitoken: not found")
)

// TokenSource identifies who created a token.
type TokenSource string

const (
	TokenSourceManual TokenSource = ""      // created via /api/v1/mcp/tokens
	TokenSourceOAuth  TokenSource = "oauth" // issued via OAuth 2.1 PKCE flow

	MaxOAuthTokens = 500
)

var ErrStoreFull = fmt.Errorf("apitoken: oauth store full (max %d)", MaxOAuthTokens)

// Token is the on-disk record. Raw token is never stored — only its SHA-256 hash.
// SHA-256 without salt is appropriate here: tokens are 32 random bytes (256-bit
// entropy), so pre-image resistance suffices and rainbow tables are infeasible.
type Token struct {
	ID        string      `json:"id"`
	Name      string      `json:"name"`
	Hash      string      `json:"hash"`
	CreatedAt time.Time   `json:"created_at"`
	ExpiresAt *time.Time  `json:"expires_at,omitempty"`
	Source    TokenSource `json:"source,omitempty"`
}

// View is what List returns: no hash, no raw, safe to serialize to clients.
// Hash is intentionally always empty — the field exists only so that callers
// can assert it is unset; it is never populated by List().
type View struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	Hash      string    `json:"-"` // always empty; never serialized
}

// Store is a thread-safe Token collection backed by a JSON file.
type Store struct {
	path string
	mu   sync.RWMutex
	toks []Token
}

// Open loads the store from path. Creates the parent directory (0700) if absent;
// treats a missing file as an empty store. Returns error if file exists but is corrupt.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("apitoken: mkdir parent: %w", err)
	}
	_ = os.Chmod(filepath.Dir(path), 0o700)

	s := &Store{path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("apitoken: read %s: %w", path, err)
	}
	var file struct {
		Tokens []Token `json:"tokens"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf(
			"apitoken: parse %s (file is corrupt; remove it or restore from backup): %w",
			path, err)
	}
	s.toks = file.Tokens
	return s, nil
}

// Create allocates a new token, returns its raw value once, and persists.
func (s *Store) Create(name string) (rawToken string, tok Token, err error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", Token{}, ErrNameRequired
	}
	if len(name) > MaxNameLen {
		return "", Token{}, ErrNameTooLong
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.toks) >= MaxTokens {
		return "", Token{}, ErrTooManyTokens
	}

	raw, err := randHex(32)
	if err != nil {
		return "", Token{}, err
	}
	id, err := randHex(13)
	if err != nil {
		return "", Token{}, err
	}
	sum := sha256.Sum256([]byte(raw))
	tok = Token{
		ID:        id,
		Name:      name,
		Hash:      hex.EncodeToString(sum[:]),
		CreatedAt: time.Now().UTC(),
	}
	s.toks = append(s.toks, tok)
	if err := s.persistLocked(); err != nil {
		s.toks = s.toks[:len(s.toks)-1]
		return "", Token{}, err
	}
	return raw, tok, nil
}

// CreateWithTTL creates a token with a finite TTL. Use TokenSourceOAuth for
// OAuth-issued tokens; they are capped at MaxOAuthTokens with lazy eviction.
func (s *Store) CreateWithTTL(name string, src TokenSource, ttl time.Duration) (rawToken string, tok Token, err error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", Token{}, ErrNameRequired
	}
	if len(name) > MaxNameLen {
		return "", Token{}, ErrNameTooLong
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if src == TokenSourceOAuth {
		if s.countOAuthLocked() >= MaxOAuthTokens {
			s.evictExpiredLocked()
			if s.countOAuthLocked() >= MaxOAuthTokens {
				return "", Token{}, ErrStoreFull
			}
		}
	}

	raw, err := randHex(32)
	if err != nil {
		return "", Token{}, err
	}
	id, err := randHex(13)
	if err != nil {
		return "", Token{}, err
	}
	sum := sha256.Sum256([]byte(raw))
	exp := time.Now().UTC().Add(ttl)
	tok = Token{
		ID:        id,
		Name:      name,
		Hash:      hex.EncodeToString(sum[:]),
		CreatedAt: time.Now().UTC(),
		ExpiresAt: &exp,
		Source:    src,
	}
	s.toks = append(s.toks, tok)
	if err := s.persistLocked(); err != nil {
		s.toks = s.toks[:len(s.toks)-1]
		return "", Token{}, err
	}
	return raw, tok, nil
}

// List returns metadata for every stored token — no hash, no raw.
func (s *Store) List() []View {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]View, len(s.toks))
	for i, t := range s.toks {
		out[i] = View{ID: t.ID, Name: t.Name, CreatedAt: t.CreatedAt}
	}
	return out
}

// Revoke removes the token with the given id.
func (s *Store) Revoke(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.toks {
		if t.ID == id {
			prev := append([]Token(nil), s.toks...)
			s.toks = append(s.toks[:i], s.toks[i+1:]...)
			if err := s.persistLocked(); err != nil {
				s.toks = prev
				return err
			}
			return nil
		}
	}
	return ErrNotFound
}

// Validate returns true iff raw matches a stored hash. Constant-time compare.
func (s *Store) Validate(raw string) bool {
	if raw == "" {
		return false
	}
	sum := sha256.Sum256([]byte(raw))
	got := hex.EncodeToString(sum[:])

	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.toks {
		if t.ExpiresAt != nil && time.Now().After(*t.ExpiresAt) {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(t.Hash)) == 1 {
			return true
		}
	}
	return false
}

// LookupByRaw is like Validate but also returns the matching id + name for audit logs.
func (s *Store) LookupByRaw(raw string) (id, name string, ok bool) {
	if raw == "" {
		return "", "", false
	}
	sum := sha256.Sum256([]byte(raw))
	got := hex.EncodeToString(sum[:])

	s.mu.RLock()
	defer s.mu.RUnlock()
	var match Token
	matched := 0
	for _, t := range s.toks {
		if t.ExpiresAt != nil && time.Now().After(*t.ExpiresAt) {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(t.Hash)) == 1 {
			match = t
			matched = 1
		}
	}
	if matched == 1 {
		return match.ID, match.Name, true
	}
	return "", "", false
}

func (s *Store) evictExpiredLocked() {
	now := time.Now()
	kept := s.toks[:0]
	for _, t := range s.toks {
		if t.ExpiresAt != nil && now.After(*t.ExpiresAt) {
			continue
		}
		kept = append(kept, t)
	}
	s.toks = kept
}

func (s *Store) countOAuthLocked() int {
	n := 0
	for _, t := range s.toks {
		if t.Source == TokenSourceOAuth {
			n++
		}
	}
	return n
}

// persistLocked writes atomically with fsync. Caller MUST hold s.mu write lock.
func (s *Store) persistLocked() error {
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, "api-tokens-*.json")
	if err != nil {
		return fmt.Errorf("apitoken: temp file: %w", err)
	}
	tmpPath := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("apitoken: chmod tmp: %w", err)
	}
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(struct {
		Tokens []Token `json:"tokens"`
	}{s.toks}); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("apitoken: encode: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("apitoken: fsync tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("apitoken: close tmp: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("apitoken: rename: %w", err)
	}
	if d, err := os.Open(dir); err == nil {
		if err := d.Sync(); err != nil {
			slog.Warn("apitoken: parent dir fsync failed", "dir", dir, "err", err)
		}
		_ = d.Close()
	} else {
		slog.Warn("apitoken: could not open parent dir for fsync", "dir", dir, "err", err)
	}
	return nil
}

func randHex(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("apitoken: rand: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// StartEviction starts a background goroutine that removes expired tokens
// every 5 minutes. It runs until ctx is cancelled.
func (s *Store) StartEviction(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.mu.Lock()
				s.evictExpiredLocked()
				if err := s.persistLocked(); err != nil {
					slog.Warn("apitoken: eviction persist failed", "err", err)
				}
				s.mu.Unlock()
			}
		}
	}()
}
