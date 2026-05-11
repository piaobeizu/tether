package oauth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

const defaultTTL = 5 * time.Minute

// ErrNotFound is returned when a code or pending request is absent or expired.
var ErrNotFound = errors.New("oauth: not found or expired")

// PendingRequest holds the parameters from GET /oauth/authorize.
type PendingRequest struct {
	ClientID    string
	RedirectURI string
	Challenge   string
	Scope       string
	State       string
}

type pendingEntry struct {
	PendingRequest
	expiresAt time.Time
}

type codeEntry struct {
	PendingRequest
	expiresAt time.Time
}

// CodeStore manages pending requests and single-use auth codes in memory.
// All state is cleared on daemon restart.
type CodeStore struct {
	ttl     time.Duration
	mu      sync.Mutex
	pending map[string]pendingEntry
	codes   map[string]codeEntry
}

// NewCodeStore returns a CodeStore with the default 5-minute TTL.
func NewCodeStore() *CodeStore {
	return NewCodeStoreWithTTL(defaultTTL)
}

// NewCodeStoreWithTTL allows overriding TTL for tests.
func NewCodeStoreWithTTL(ttl time.Duration) *CodeStore {
	return &CodeStore{
		ttl:     ttl,
		pending: make(map[string]pendingEntry),
		codes:   make(map[string]codeEntry),
	}
}

// StorePending stores the authorization request and returns a random req_id.
func (s *CodeStore) StorePending(clientID, redirectURI, challenge, scope, state string) (reqID string, err error) {
	id, err := randID()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[id] = pendingEntry{
		PendingRequest: PendingRequest{
			ClientID:    clientID,
			RedirectURI: redirectURI,
			Challenge:   challenge,
			Scope:       scope,
			State:       state,
		},
		expiresAt: time.Now().Add(s.ttl),
	}
	return id, nil
}

// ConsumePending retrieves and deletes the pending request for reqID.
// Returns ErrNotFound if absent or expired.
func (s *CodeStore) ConsumePending(reqID string) (PendingRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.pending[reqID]
	delete(s.pending, reqID)
	if !ok || time.Now().After(e.expiresAt) {
		return PendingRequest{}, ErrNotFound
	}
	return e.PendingRequest, nil
}

// StoreCode stores a single-use auth code for the given pending request.
func (s *CodeStore) StoreCode(p PendingRequest) (code string, err error) {
	c, err := randID()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.codes[c] = codeEntry{PendingRequest: p, expiresAt: time.Now().Add(s.ttl)}
	return c, nil
}

// ConsumeCode retrieves and deletes the auth code (single-use).
// Returns ErrNotFound if absent or expired. The code is always deleted,
// even on expiry, to prevent timing attacks.
func (s *CodeStore) ConsumeCode(code string) (PendingRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.codes[code]
	delete(s.codes, code) // always delete
	if !ok || time.Now().After(e.expiresAt) {
		return PendingRequest{}, ErrNotFound
	}
	return e.PendingRequest, nil
}

func randID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
