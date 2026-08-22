package oauth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

const defaultTTL = 5 * time.Minute

// defaultMaxPending is the hard ceiling on how many authorization requests may
// be waiting for owner consent at once.
//
// Why a ceiling exists at all: the only writer into the pending map is
// GET /oauth/authorize, and the auth middleware deliberately exempts that
// method for anonymous callers (tether#117 chose to keep the GET exemption so
// an MCP client can render the consent page before the owner has signed in).
// On an --acme-domain deployment the listener is on the public internet by
// construction. Without a ceiling, an anonymous caller that never follows up
// with the POST grows the map without bound.
//
// Why 512. A pending entry is a consent page the owner is currently looking at,
// so evicting one breaks a real login — the ceiling has to sit far above any
// organic concurrency:
//
//   - Organic load. /oauth/authorize is hit once per MCP client pairing on a
//     single-owner daemon. Ten consent pages open at once would already be
//     extraordinary; 512 is ~50x that.
//   - Rate needed to reach it. Entries live at most ttl (5 minutes) and
//     reclaimPendingLocked drops expired ones on every insert, so the map only
//     holds requests from the last 5 minutes. Reaching 512 takes 1.7 authorize
//     requests per second sustained for the whole window — no human produces
//     that, so the ceiling is only reachable under a flood or a runaway client.
//   - Cost of being at it. Worst-case retained memory is
//     512 * (maxPendingEntryBytes + key + struct overhead) ~= 4.2 MiB, and an
//     insert while the map is full measures ~25us (the reclaim pass dominates
//     it). Picking a much larger ceiling trades those two budgets away for
//     headroom nobody uses: the same insert measures ~159us at maxPending=4096,
//     which would hand the same anonymous caller a CPU amplification in
//     exchange for closing the memory one.
const defaultMaxPending = 512

// maxPendingEntryBytes caps the caller-controlled bytes a single pending entry
// may retain. Without it the count ceiling would not bound memory: every field
// below comes from the query string of GET /oauth/authorize, and the HTTP/3
// listener does not set http3.Server.MaxHeaderBytes (internal/server/server.go),
// so quic-go falls back to http.DefaultMaxHeaderBytes — 1 MiB of :path per
// request. 512 entries of 1 MiB would still be 512 MiB.
//
// A real request is client_id + a loopback redirect_uri + a 43-byte S256
// challenge + "mcp" + state, i.e. a few hundred bytes. 8 KiB leaves more than
// 20x headroom and still sits under the ~8 KB URL limits that browsers and
// reverse proxies impose anyway.
const maxPendingEntryBytes = 8 << 10

// ErrNotFound is returned when a code or pending request is absent or expired.
var ErrNotFound = errors.New("oauth: not found or expired")

// ErrRequestTooLarge is returned by StorePending when the authorization
// request's parameters exceed maxPendingEntryBytes.
var ErrRequestTooLarge = errors.New("oauth: authorization request too large")

// PendingRequest holds the parameters from GET /oauth/authorize.
type PendingRequest struct {
	ClientID    string
	RedirectURI string
	Challenge   string
	Scope       string
	State       string
}

func (p PendingRequest) size() int {
	return len(p.ClientID) + len(p.RedirectURI) + len(p.Challenge) + len(p.Scope) + len(p.State)
}

type pendingEntry struct {
	PendingRequest
	expiresAt time.Time
	// seq is a per-store insertion counter used to pick the oldest entry when
	// the ceiling is hit. It is not derived from the clock, so entries stored
	// within the same clock tick still have a total order.
	seq uint64
}

type codeEntry struct {
	PendingRequest
	expiresAt time.Time
}

// CodeStoreOptions configures a CodeStore. The zero value selects the defaults
// used in production.
type CodeStoreOptions struct {
	// TTL is how long a pending request and an auth code stay valid.
	// Zero selects defaultTTL. A negative TTL is honoured as-is; tests pass one
	// to get an already-expired entry.
	TTL time.Duration
	// MaxPending caps concurrently outstanding pending requests.
	// Zero or negative selects defaultMaxPending.
	MaxPending int
	// Now overrides the clock. Nil selects time.Now. Tests inject a clock they
	// can advance so expiry is exercised without sleeping.
	Now func() time.Time
}

// CodeStore manages pending requests and single-use auth codes in memory.
// All state is cleared on daemon restart.
//
// The pending map is bounded two ways, and both are load-bearing (see
// reclaimPendingLocked):
//
//   - the TTL sweep drops every expired entry on each insert. Without it
//     entries accumulate for the lifetime of the daemon, so ordinary traffic
//     would eventually push the map to the ceiling and start evicting live
//     consent pages.
//   - the ceiling (maxPending) evicts the oldest entry once the map is full.
//     Without it the sweep bounds nothing against a caller that outruns the
//     TTL: at R requests/second the map still grows to R*ttl entries.
type CodeStore struct {
	ttl        time.Duration
	maxPending int
	now        func() time.Time

	mu      sync.Mutex
	seq     uint64
	pending map[string]pendingEntry
	codes   map[string]codeEntry
}

// NewCodeStore returns a CodeStore with the default 5-minute TTL.
func NewCodeStore() *CodeStore {
	return NewCodeStoreWithOptions(CodeStoreOptions{})
}

// NewCodeStoreWithTTL allows overriding TTL for tests.
func NewCodeStoreWithTTL(ttl time.Duration) *CodeStore {
	return NewCodeStoreWithOptions(CodeStoreOptions{TTL: ttl})
}

// NewCodeStoreWithOptions builds a CodeStore from opts, filling in defaults.
func NewCodeStoreWithOptions(opts CodeStoreOptions) *CodeStore {
	ttl := opts.TTL
	if ttl == 0 {
		ttl = defaultTTL
	}
	maxPending := opts.MaxPending
	if maxPending <= 0 {
		maxPending = defaultMaxPending
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &CodeStore{
		ttl:        ttl,
		maxPending: maxPending,
		now:        now,
		pending:    make(map[string]pendingEntry),
		codes:      make(map[string]codeEntry),
	}
}

// PendingLen reports how many authorization requests are currently waiting for
// consent. It never exceeds the store's ceiling.
func (s *CodeStore) PendingLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}

// StorePending stores the authorization request and returns a random req_id.
// It returns ErrRequestTooLarge if the request parameters exceed
// maxPendingEntryBytes.
func (s *CodeStore) StorePending(clientID, redirectURI, challenge, scope, state string) (reqID string, err error) {
	p := PendingRequest{
		ClientID:    clientID,
		RedirectURI: redirectURI,
		Challenge:   challenge,
		Scope:       scope,
		State:       state,
	}
	if p.size() > maxPendingEntryBytes {
		return "", ErrRequestTooLarge
	}
	id, err := randID()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.reclaimPendingLocked(now)
	s.seq++
	s.pending[id] = pendingEntry{PendingRequest: p, expiresAt: now.Add(s.ttl), seq: s.seq}
	return id, nil
}

// reclaimPendingLocked makes room for one new pending entry. Callers must hold
// mu. It carries the two independent bounds on the pending map:
//
//  1. the TTL sweep — every entry past its deadline is deleted, unconditionally,
//     however small the map is;
//  2. the ceiling — if the map is still full afterwards, the entry stored
//     longest ago is evicted.
//
// Sweep before evict, deliberately: an expired entry is dead weight, while the
// one the ceiling would drop may still be a consent page the owner is about to
// click. Eviction is oldest-first for the same reason — the newest entry is the
// one a human is most likely looking at right now.
//
// Both bounds run inline on insert rather than on a background timer. The map
// can only grow in StorePending, so that is exactly when reclaiming is needed,
// and an inline pass needs no goroutine and therefore no shutdown hook on
// CodeStore (which internal/server/lifecycle.go constructs and never closes).
//
// The two bounds share one pass over the map rather than taking one each: the
// pass is the dominant cost of an insert while the map is full (~25us per
// insert at maxPending=512), and this endpoint is anonymous-reachable, so the
// work an unauthenticated caller can force per request is itself a budget worth
// keeping small.
func (s *CodeStore) reclaimPendingLocked(now time.Time) {
	var oldestID string
	var oldestSeq uint64
	found := false
	for id, e := range s.pending {
		if now.After(e.expiresAt) {
			delete(s.pending, id)
			continue
		}
		if !found || e.seq < oldestSeq {
			oldestID, oldestSeq, found = id, e.seq, true
		}
	}
	if found && len(s.pending) >= s.maxPending {
		delete(s.pending, oldestID)
	}
}

// ConsumePending retrieves and deletes the pending request for reqID.
// Returns ErrNotFound if absent or expired.
func (s *CodeStore) ConsumePending(reqID string) (PendingRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.pending[reqID]
	delete(s.pending, reqID)
	if !ok || s.now().After(e.expiresAt) {
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
	s.codes[c] = codeEntry{PendingRequest: p, expiresAt: s.now().Add(s.ttl)}
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
	if !ok || s.now().After(e.expiresAt) {
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
