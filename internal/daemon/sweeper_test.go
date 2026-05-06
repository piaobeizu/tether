package daemon

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/agent"
)

// stubLine is a minimal SessionStart-like JSONL record. Content
// doesn't matter for the sweeper — it only counts newlines.
const stubLine = `{"type":"attachment","sessionId":"s","attachment":{"hookEvent":"SessionStart"}}` + "\n"

// writeJSONL writes `lines` lines (each `stubLine`) to `path` and
// then back-dates its mtime by `age`.
func writeJSONL(t *testing.T, path string, lines int, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for i := 0; i < lines; i++ {
		if _, err := f.WriteString(stubLine); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if age > 0 {
		past := time.Now().Add(-age)
		if err := os.Chtimes(path, past, past); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}
}

// fakeSubChecker mimics what stubSweeper needs from EnvelopeEmitter
// but lets tests inject "this sid has subscribers" without spinning
// up a real watcher. We can't replace the field type (it's
// *agent.EnvelopeEmitter) but we can run with a real emitter rooted
// at a clean temp dir and Subscribe() to register a subscriber.
type fakeSubChecker struct {
	mu       sync.Mutex
	withSubs map[string]bool
}

func TestSweeper_DeletesStubKeepsLive(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	bucket := filepath.Join(tmp, "encoded-cwd")
	stubPath := filepath.Join(bucket, "sess-stub.jsonl")
	livePath := filepath.Join(bucket, "sess-live.jsonl")

	// Stub: 1 line, 2h old.
	writeJSONL(t, stubPath, 1, 2*time.Hour)
	// Live: 10 lines, fresh mtime (zero age).
	writeJSONL(t, livePath, 10, 0)
	// Edge case: a 2-line, 2h-old file — should ALSO be swept
	// (≤ stubMaxLines threshold).
	edgePath := filepath.Join(bucket, "sess-edge.jsonl")
	writeJSONL(t, edgePath, 2, 2*time.Hour)
	// Edge case: a 1-line file but FRESH — should NOT be swept.
	freshStubPath := filepath.Join(bucket, "sess-freshstub.jsonl")
	writeJSONL(t, freshStubPath, 1, 0)

	s := &stubSweeper{
		projectsDir: tmp,
		emitter:     nil, // no subscribers; deletion gated only on predicate
		logf:        func(string, ...any) {},
	}
	s.sweep()

	if _, err := os.Stat(stubPath); !os.IsNotExist(err) {
		t.Errorf("expected stub path %q to be deleted, stat err=%v", stubPath, err)
	}
	if _, err := os.Stat(edgePath); !os.IsNotExist(err) {
		t.Errorf("expected edge (2-line, old) %q to be deleted, stat err=%v", edgePath, err)
	}
	if _, err := os.Stat(livePath); err != nil {
		t.Errorf("expected live path %q to survive, stat err=%v", livePath, err)
	}
	if _, err := os.Stat(freshStubPath); err != nil {
		t.Errorf("expected fresh stub %q to survive (mtime fresh), stat err=%v", freshStubPath, err)
	}
}

func TestSweeper_RefusesWhenSubscribed(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	bucket := filepath.Join(tmp, "encoded-cwd")
	if err := os.MkdirAll(bucket, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	sid := "sess-subscribed"
	path := filepath.Join(bucket, sid+".jsonl")
	// One line, very old → would normally be a sweep target.
	writeJSONL(t, path, 1, 2*time.Hour)

	// Spin a real EnvelopeEmitter so we get a real
	// HasSubscribers true/false. Root it at the bucket (not tmp) so
	// the watcher's fsnotify add succeeds; the sweeper still walks
	// `tmp` recursively.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	em, err := agent.NewEnvelopeEmitter(ctx, agent.EmitterConfig{
		ProjectsDir: bucket,
	})
	if err != nil {
		t.Fatalf("emitter: %v", err)
	}
	defer em.Close()

	_, teardown, err := em.Subscribe(sid)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer teardown()

	// Confirm subscriber check passes.
	if !em.HasSubscribers(sid) {
		t.Fatalf("expected HasSubscribers(%q)=true after Subscribe", sid)
	}

	s := &stubSweeper{
		projectsDir: tmp,
		emitter:     em,
		logf:        func(string, ...any) {},
	}
	s.sweep()

	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected subscribed file %q to survive sweep; stat err=%v", path, err)
	}

	// Race smoke: tear down the subscriber, sweep again, file
	// should now disappear.
	teardown()
	// Tiny grace for HasSubscribers to flip false (Unsubscribe is
	// synchronous under the watcher mu, but the relay goroutine
	// runs the deregister; so wait briefly).
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && em.HasSubscribers(sid) {
		time.Sleep(10 * time.Millisecond)
	}
	if em.HasSubscribers(sid) {
		t.Fatalf("subscriber did not deregister within 1s")
	}

	s.sweep()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected file %q to be deleted after unsubscribe; stat err=%v", path, err)
	}
}

func TestSweeper_RefusesPathOutsideRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outside := t.TempDir()
	// Put a stub-eligible JSONL outside root and symlink it INTO
	// root. The sweeper must refuse to delete it.
	target := filepath.Join(outside, "real.jsonl")
	writeJSONL(t, target, 1, 2*time.Hour)
	link := filepath.Join(root, "linked.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}

	s := &stubSweeper{
		projectsDir: root,
		emitter:     nil,
		logf:        func(string, ...any) {},
	}
	s.sweep()

	// Symlink itself MAY have been removed (its target is regular,
	// path containment check passes for the link path itself). The
	// LOAD-BEARING assertion is that the actual file outside root
	// must survive — even if a hostile bucket structure put a
	// symlink inside root.
	if _, err := os.Stat(target); err != nil {
		t.Errorf("expected outside-root file %q to survive, err=%v", target, err)
	}
}

func TestSweeper_RunHeartbeats(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	var beats atomic.Int64
	hb := func() { beats.Add(1) }

	s := &stubSweeper{
		projectsDir: tmp,
		emitter:     nil,
		interval:    50 * time.Millisecond, // fast for the test
		logf:        nil,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, hb) }()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not exit on ctx timeout")
	}
	// Heartbeat ticker is 1s; we ran 2.5s; expect at least 2 beats
	// (the initial hb() at the top of Run + at least one ticker
	// beat). Bound is conservative for CI flakiness.
	if got := beats.Load(); got < 2 {
		t.Errorf("expected ≥2 heartbeats, got %d", got)
	}
}

func TestCountLinesUpTo(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()

	cases := []struct {
		name     string
		content  string
		limit    int
		want     int
	}{
		{"empty", "", 5, 0},
		{"one line no trailing nl", "abc", 5, 1},
		{"one line trailing nl", "abc\n", 5, 1},
		{"two lines", "abc\ndef\n", 5, 2},
		{"three lines limit 2", "a\nb\nc\n", 2, 3}, // count > limit triggers early return at 3
		{"long line still 1", string(make([]byte, 1<<16)) + "\n", 5, 1},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := filepath.Join(tmp, tc.name+".jsonl")
			if err := os.WriteFile(p, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			got, err := countLinesUpTo(p, tc.limit)
			if err != nil {
				t.Fatalf("count: %v", err)
			}
			if got != tc.want {
				t.Errorf("countLinesUpTo(%q, limit=%d) = %d, want %d", tc.content, tc.limit, got, tc.want)
			}
		})
	}
}

func TestPathContained(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		root  string
		child string
		want  bool
	}{
		{"same", "/a/b", "/a/b", true},
		{"child", "/a", "/a/b/c", true},
		{"sibling", "/a", "/b", false},
		{"escape", "/a/b", "/a", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := pathContained(tc.root, tc.child); got != tc.want {
				t.Errorf("pathContained(%q,%q) = %v, want %v", tc.root, tc.child, got, tc.want)
			}
		})
	}
}
