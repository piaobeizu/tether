package oauth_test

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/auth/oauth"
)

// testClock is an injectable clock. Expiry tests advance it explicitly instead
// of sleeping, so nothing here depends on a real-time race between a sweep and
// a deadline.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// storeN stores n distinct authorization requests and returns their req_ids in
// insertion order.
func storeN(t *testing.T, s *oauth.CodeStore, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id, err := s.StorePending("flood", "http://127.0.0.1:9/cb", "challenge", "mcp", "")
		if err != nil {
			t.Fatalf("StorePending #%d: %v", i, err)
		}
		ids = append(ids, id)
	}
	return ids
}

func TestCodeStore_PendingHappyPath(t *testing.T) {
	s := oauth.NewCodeStore()
	reqID, err := s.StorePending("cursor", "http://localhost:1234/cb", "challenge", "mcp", "state1")
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.ConsumePending(reqID)
	if err != nil {
		t.Fatalf("ConsumePending: %v", err)
	}
	if p.ClientID != "cursor" || p.RedirectURI != "http://localhost:1234/cb" {
		t.Errorf("unexpected pending: %+v", p)
	}
	// Second consume must fail (single-use).
	if _, err := s.ConsumePending(reqID); !errors.Is(err, oauth.ErrNotFound) {
		t.Errorf("second consume: want ErrNotFound, got %v", err)
	}
}

func TestCodeStore_PendingExpiry(t *testing.T) {
	s := oauth.NewCodeStoreWithTTL(-time.Second) // already expired
	reqID, err := s.StorePending("goose", "http://localhost:5678/cb", "c", "mcp", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumePending(reqID); !errors.Is(err, oauth.ErrNotFound) {
		t.Errorf("want ErrNotFound for expired pending, got %v", err)
	}
}

func TestCodeStore_AuthCodeHappyPath(t *testing.T) {
	s := oauth.NewCodeStore()
	p := oauth.PendingRequest{
		ClientID:    "cursor",
		RedirectURI: "http://localhost:1234/cb",
		Challenge:   "challenge",
		Scope:       "mcp",
		State:       "s",
	}
	code, err := s.StoreCode(p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ConsumeCode(code)
	if err != nil {
		t.Fatalf("ConsumeCode: %v", err)
	}
	if got.ClientID != "cursor" {
		t.Errorf("unexpected client_id: %s", got.ClientID)
	}
	// Replay must fail.
	if _, err := s.ConsumeCode(code); !errors.Is(err, oauth.ErrNotFound) {
		t.Errorf("replay: want ErrNotFound, got %v", err)
	}
}

func TestCodeStore_AuthCodeExpiry(t *testing.T) {
	s := oauth.NewCodeStoreWithTTL(-time.Second)
	p := oauth.PendingRequest{ClientID: "x", RedirectURI: "http://localhost/cb", Challenge: "c", Scope: "mcp"}
	code, _ := s.StoreCode(p)
	if _, err := s.ConsumeCode(code); !errors.Is(err, oauth.ErrNotFound) {
		t.Errorf("want ErrNotFound for expired code, got %v", err)
	}
}

func TestCodeStore_UnknownReqID(t *testing.T) {
	s := oauth.NewCodeStore()
	if _, err := s.ConsumePending("doesnotexist"); !errors.Is(err, oauth.ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

// TestCodeStore_PendingIsCappedAtCeiling pins the *ceiling* half of the fix
// (tether#154). GET /oauth/authorize is anonymous-reachable by design, so an
// unauthenticated caller can call StorePending in a loop and never follow up
// with the POST.
//
// The TTL here is an hour and the clock never moves, so nothing ever expires
// and the sweep is a guaranteed no-op: this test can only be satisfied by the
// ceiling. Removing the sweep leaves it green; removing the ceiling turns it
// red.
func TestCodeStore_PendingIsCappedAtCeiling(t *testing.T) {
	const ceiling = 8
	const flood = ceiling + 50

	clk := newTestClock()
	s := oauth.NewCodeStoreWithOptions(oauth.CodeStoreOptions{
		TTL:        time.Hour,
		MaxPending: ceiling,
		Now:        clk.Now,
	})

	ids := storeN(t, s, flood)

	// With the ceiling absent this is 58 (one entry per request, forever).
	// With the ceiling present it is 8.
	if got := s.PendingLen(); got != ceiling {
		t.Errorf("PendingLen after %d anonymous authorize requests = %d, want %d", flood, got, ceiling)
	}

	// Eviction must be oldest-first, not newest-first: the freshest consent
	// page is the one a human is most likely looking at right now.
	// Newest-first eviction would make this ErrNotFound.
	if _, err := s.ConsumePending(ids[flood-1]); err != nil {
		t.Errorf("newest pending after flood: want it to survive, got %v", err)
	}
	// The oldest req_ids are the ones that must be gone. Without a ceiling
	// every one of these 50 would still resolve.
	for i := 0; i < flood-ceiling; i++ {
		if _, err := s.ConsumePending(ids[i]); !errors.Is(err, oauth.ErrNotFound) {
			t.Fatalf("evicted pending #%d: want ErrNotFound, got %v", i, err)
		}
	}
}

// TestCodeStore_DefaultConstructorIsCapped pins the ceiling on the constructor
// the daemon actually uses (internal/server/lifecycle.go calls
// oauth.NewCodeStore). A ceiling that only applied to test-configured stores
// would be a gate that never fires in production.
//
// Defect present: PendingLen == 2000. Fixed: 512.
func TestCodeStore_DefaultConstructorIsCapped(t *testing.T) {
	const defaultCeiling = 512 // mirrors defaultMaxPending in store.go
	const flood = 2000
	s := oauth.NewCodeStore()
	storeN(t, s, flood)
	if got := s.PendingLen(); got != defaultCeiling {
		t.Errorf("PendingLen for NewCodeStore after %d requests = %d, want %d", flood, got, defaultCeiling)
	}
}

// TestCodeStore_ExpiredPendingIsSweptFarBelowCeiling pins the *sweep* half of
// the fix. Everything here happens 500 entries below the ceiling, so the
// ceiling is a guaranteed no-op: only the sweep can shrink the map.
//
// The sweep is what keeps a long-running daemon away from the ceiling under
// organic traffic. Without it, abandoned consent pages accumulate for the
// process lifetime, and the first user to arrive after the map has drifted up
// to 512 gets somebody's live consent page evicted.
//
// Removing the ceiling leaves this green; removing the sweep turns it red.
func TestCodeStore_ExpiredPendingIsSweptFarBelowCeiling(t *testing.T) {
	const abandoned = 10

	clk := newTestClock()
	s := oauth.NewCodeStoreWithOptions(oauth.CodeStoreOptions{
		TTL: time.Minute, // MaxPending left at the 512 default, far away.
		Now: clk.Now,
	})

	storeN(t, s, abandoned)
	if got := s.PendingLen(); got != abandoned {
		t.Fatalf("PendingLen before expiry = %d, want %d", got, abandoned)
	}

	clk.Advance(90 * time.Second) // past the 1-minute TTL

	storeN(t, s, 1)

	// With the sweep absent this is 11: the 10 dead entries are still resident
	// because nothing calls ConsumePending on them, and 11 is nowhere near the
	// 512 ceiling so eviction cannot help. With the sweep present it is 1.
	if got := s.PendingLen(); got != 1 {
		t.Errorf("PendingLen after %d entries expired and one new request = %d, want 1", abandoned, got)
	}
}

// TestCodeStore_RealPendingIsNotEvictedEarly is the guard on the failure mode
// this class of fix most easily introduces: a pending entry is a consent page
// the owner is currently looking at, so evicting one early means the Allow
// button fails for a real user.
func TestCodeStore_RealPendingIsNotEvictedEarly(t *testing.T) {
	t.Run("survives every sweep for the full TTL", func(t *testing.T) {
		const ttl = 5 * time.Minute
		clk := newTestClock()
		s := oauth.NewCodeStoreWithOptions(oauth.CodeStoreOptions{TTL: ttl, Now: clk.Now})

		live, err := s.StorePending("cursor", "http://localhost:1234/cb", "challenge", "mcp", "st")
		if err != nil {
			t.Fatal(err)
		}

		// Walk the clock to just short of the deadline, storing an unrelated
		// request at each step. Every StorePending runs a sweep, so the live
		// entry is offered to the sweep 59 times.
		const step = 5 * time.Second
		steps := int((ttl - time.Second) / step) // 59 => stops at T+295s
		for i := 0; i < steps; i++ {
			clk.Advance(step)
			storeN(t, s, 1)
		}
		elapsed := time.Duration(steps) * step

		// A sweep that used the wrong comparison (dropping entries at or before
		// the deadline rather than strictly after it, or ignoring expiresAt
		// entirely) makes this ErrNotFound. Correct behaviour: the request
		// resolves with its original parameters.
		p, err := s.ConsumePending(live)
		if err != nil {
			t.Fatalf("live pending at T+%v of a %v TTL, after %d sweeps: want it to survive, got %v",
				elapsed, ttl, steps, err)
		}
		if p.ClientID != "cursor" || p.State != "st" {
			t.Errorf("live pending came back mangled: %+v", p)
		}
	})

	t.Run("survives ceiling-1 newer requests", func(t *testing.T) {
		const ceiling = 8
		clk := newTestClock()
		s := oauth.NewCodeStoreWithOptions(oauth.CodeStoreOptions{
			TTL:        time.Hour,
			MaxPending: ceiling,
			Now:        clk.Now,
		})

		live, err := s.StorePending("cursor", "http://localhost:1234/cb", "challenge", "mcp", "st")
		if err != nil {
			t.Fatal(err)
		}
		storeN(t, s, ceiling-1) // map is now exactly full, nothing evicted yet

		if got := s.PendingLen(); got != ceiling {
			t.Fatalf("PendingLen = %d, want %d (nothing should have been evicted yet)", got, ceiling)
		}
		// An off-by-one ceiling (evicting once len reaches maxPending-1, or
		// evicting before the insert rather than only when full) makes this
		// ErrNotFound.
		if _, err := s.ConsumePending(live); err != nil {
			t.Fatalf("pending evicted by only %d newer requests under a ceiling of %d: %v", ceiling-1, ceiling, err)
		}
	})

	t.Run("the ceiling only bites on the ceiling+1-th request", func(t *testing.T) {
		// Documents the exact boundary, so a future change to the eviction
		// policy has to update a stated number rather than drift silently.
		const ceiling = 8
		clk := newTestClock()
		s := oauth.NewCodeStoreWithOptions(oauth.CodeStoreOptions{
			TTL:        time.Hour,
			MaxPending: ceiling,
			Now:        clk.Now,
		})

		live, err := s.StorePending("cursor", "http://localhost:1234/cb", "challenge", "mcp", "st")
		if err != nil {
			t.Fatal(err)
		}
		storeN(t, s, ceiling) // one more than the previous subtest

		if _, err := s.ConsumePending(live); !errors.Is(err, oauth.ErrNotFound) {
			t.Errorf("oldest pending after %d newer requests under a ceiling of %d: want ErrNotFound, got %v",
				ceiling, ceiling, err)
		}
	})
}

// TestCodeStore_RejectsOversizedAuthorizeRequest pins the per-entry byte guard.
// The ceiling bounds the entry *count*; without this guard it would not bound
// memory, because every stored field comes from the query string of
// GET /oauth/authorize and the HTTP/3 listener leaves MaxHeaderBytes at
// net/http's 1 MiB default. 512 x 1 MiB is still 512 MiB.
func TestCodeStore_RejectsOversizedAuthorizeRequest(t *testing.T) {
	// Mirrors maxPendingEntryBytes in store.go. Kept as a literal so that
	// changing the constant has to change a stated number here too.
	const limit = 8 << 10

	t.Run("at the limit it is stored", func(t *testing.T) {
		s := oauth.NewCodeStore()
		clientID, redirectURI, challenge, scope := "c", "http://localhost/cb", "ch", "mcp"
		fixed := len(clientID) + len(redirectURI) + len(challenge) + len(scope)
		state := strings.Repeat("s", limit-fixed)

		if _, err := s.StorePending(clientID, redirectURI, challenge, scope, state); err != nil {
			t.Fatalf("request of exactly %d bytes: want it stored, got %v", limit, err)
		}
		if got := s.PendingLen(); got != 1 {
			t.Errorf("PendingLen = %d, want 1", got)
		}
	})

	t.Run("one byte over the limit it is rejected", func(t *testing.T) {
		s := oauth.NewCodeStore()
		clientID, redirectURI, challenge, scope := "c", "http://localhost/cb", "ch", "mcp"
		fixed := len(clientID) + len(redirectURI) + len(challenge) + len(scope)
		state := strings.Repeat("s", limit-fixed+1)

		// Guard absent: err is nil and PendingLen is 1, with ~8 KiB retained
		// from a single anonymous request. Guard present: ErrRequestTooLarge
		// and nothing retained.
		if _, err := s.StorePending(clientID, redirectURI, challenge, scope, state); !errors.Is(err, oauth.ErrRequestTooLarge) {
			t.Errorf("request of %d bytes: want ErrRequestTooLarge, got %v", limit+1, err)
		}
		if got := s.PendingLen(); got != 0 {
			t.Errorf("PendingLen after a rejected request = %d, want 0", got)
		}
	})
}

// TestCodeStore_ConcurrentStorePendingStaysCapped runs the new bookkeeping
// (sweep, ceiling, seq counter) from many goroutines at once. Under -race a
// ceiling implemented outside the mutex reports a data race; without -race an
// unsynchronised map write panics with "concurrent map writes".
//
// Defect present (no ceiling): PendingLen == 3200. Fixed: 64.
func TestCodeStore_ConcurrentStorePendingStaysCapped(t *testing.T) {
	const (
		ceiling      = 64
		goroutines   = 32
		perGoroutine = 100
	)
	s := oauth.NewCodeStoreWithOptions(oauth.CodeStoreOptions{
		TTL:        time.Hour,
		MaxPending: ceiling,
	})

	var wg sync.WaitGroup
	ids := make(chan string, goroutines*perGoroutine)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				id, err := s.StorePending("flood", "http://127.0.0.1:9/cb", "ch", "mcp", "")
				if err != nil {
					t.Errorf("StorePending: %v", err)
					return
				}
				ids <- id
			}
		}()
	}
	wg.Wait()
	close(ids)

	if got := s.PendingLen(); got != ceiling {
		t.Errorf("PendingLen after %d concurrent requests = %d, want %d",
			goroutines*perGoroutine, got, ceiling)
	}

	// Draining concurrently must not corrupt the map either.
	var drain sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		drain.Add(1)
		go func() {
			defer drain.Done()
			for id := range ids {
				_, _ = s.ConsumePending(id)
			}
		}()
	}
	drain.Wait()

	if got := s.PendingLen(); got != 0 {
		t.Errorf("PendingLen after draining every req_id = %d, want 0", got)
	}
}
