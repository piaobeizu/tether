package jsonl

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// readWithTimeout drains up to `n` envelopes from ch, or times out.
func readWithTimeout(t *testing.T, ch <-chan Envelope, n int, timeout time.Duration) []Envelope {
	t.Helper()
	var out []Envelope
	deadline := time.After(timeout)
	for len(out) < n {
		select {
		case env, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, env)
		case <-deadline:
			t.Fatalf("timed out waiting for envelopes: got %d, want %d", len(out), n)
		}
	}
	return out
}

func TestWatcher_DirMode_PicksUpExistingFile(t *testing.T) {
	dir := t.TempDir()
	sid := "sess-existing"
	path := filepath.Join(dir, sid+".jsonl")
	must(t, os.WriteFile(path, []byte(
		`{"type":"user","uuid":"u1","sessionId":"`+sid+`","message":{"role":"user","content":[]}}`+"\n"), 0o644))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w, err := New(ctx, dir, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()

	ch := w.Subscribe(sid)

	// Trigger a re-read by appending another line.
	must(t, appendLine(path,
		`{"type":"assistant","uuid":"a1","sessionId":"`+sid+`","message":{"role":"assistant","content":[]}}`))

	envs := readWithTimeout(t, ch, 2, 3*time.Second)
	if len(envs) != 2 {
		t.Fatalf("got %d envelopes, want 2", len(envs))
	}
	if envs[0].SourceUUID != "u1" || envs[1].SourceUUID != "a1" {
		t.Errorf("envelope order/uuid: got %q, %q", envs[0].SourceUUID, envs[1].SourceUUID)
	}
}

func TestWatcher_NewFile_AfterStart(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w, err := New(ctx, dir, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()

	sid := "sess-new"
	ch := w.Subscribe(sid)

	// Create the JSONL file AFTER the watcher started.
	path := filepath.Join(dir, sid+".jsonl")
	must(t, os.WriteFile(path, []byte(
		`{"type":"user","uuid":"u1","sessionId":"`+sid+`","message":{"role":"user","content":[]}}`+"\n"), 0o644))

	envs := readWithTimeout(t, ch, 1, 3*time.Second)
	if envs[0].SourceUUID != "u1" {
		t.Errorf("uuid = %q", envs[0].SourceUUID)
	}
}

func TestWatcher_TruncationDetected(t *testing.T) {
	dir := t.TempDir()
	sid := "sess-trunc"
	path := filepath.Join(dir, sid+".jsonl")
	// Pre-existing content.
	must(t, os.WriteFile(path,
		[]byte(`{"type":"user","uuid":"u1","sessionId":"`+sid+`","message":{"role":"user","content":[]}}`+"\n"+
			`{"type":"assistant","uuid":"a1","sessionId":"`+sid+`","message":{"role":"assistant","content":[]}}`+"\n"),
		0o644))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w, err := New(ctx, dir, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()

	ch := w.Subscribe(sid)

	// Trigger first read by appending an empty line (no-op record
	// but advances offset via fsnotify event).
	must(t, appendLine(path,
		`{"type":"assistant","uuid":"a2","sessionId":"`+sid+`","message":{"role":"assistant","content":[]}}`))

	_ = readWithTimeout(t, ch, 3, 3*time.Second)

	// Now truncate and write fresh content.
	must(t, os.WriteFile(path,
		[]byte(`{"type":"user","uuid":"u-after-trunc","sessionId":"`+sid+`","message":{"role":"user","content":[]}}`+"\n"),
		0o644))

	envs := readWithTimeout(t, ch, 1, 3*time.Second)
	if envs[0].SourceUUID != "u-after-trunc" {
		t.Errorf("post-truncation uuid = %q, want u-after-trunc", envs[0].SourceUUID)
	}
	stats := w.StatsSnapshot()
	if stats.Truncations == 0 {
		t.Errorf("expected truncation counter > 0, got %d", stats.Truncations)
	}
}

func TestWatcher_RotationDetected(t *testing.T) {
	dir := t.TempDir()
	sid := "sess-rot"
	path := filepath.Join(dir, sid+".jsonl")
	must(t, os.WriteFile(path,
		[]byte(`{"type":"user","uuid":"u1","sessionId":"`+sid+`","message":{"role":"user","content":[]}}`+"\n"),
		0o644))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w, err := New(ctx, dir, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()

	ch := w.Subscribe(sid)

	// Read the initial line.
	must(t, appendLine(path,
		`{"type":"assistant","uuid":"a1","sessionId":"`+sid+`","message":{"role":"assistant","content":[]}}`))
	_ = readWithTimeout(t, ch, 2, 3*time.Second)

	// Rotate: remove + recreate (new inode).
	must(t, os.Remove(path))
	// Give fsnotify a moment to register the remove.
	time.Sleep(50 * time.Millisecond)
	must(t, os.WriteFile(path,
		[]byte(`{"type":"user","uuid":"u-rot","sessionId":"`+sid+`","message":{"role":"user","content":[]}}`+"\n"),
		0o644))

	envs := readWithTimeout(t, ch, 1, 3*time.Second)
	if envs[0].SourceUUID != "u-rot" {
		t.Errorf("post-rotation uuid = %q, want u-rot", envs[0].SourceUUID)
	}
}

func TestWatcher_UUIDDedup(t *testing.T) {
	dir := t.TempDir()
	sid := "sess-dedup"
	path := filepath.Join(dir, sid+".jsonl")
	must(t, os.WriteFile(path, nil, 0o644))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w, err := New(ctx, dir, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()

	ch := w.Subscribe(sid)

	// Write the same uuid twice — second occurrence must be deduped.
	rec := `{"type":"user","uuid":"dup1","sessionId":"` + sid + `","message":{"role":"user","content":[]}}`
	must(t, appendLine(path, rec))
	must(t, appendLine(path, rec))

	// Wait long enough for both to flow through fsnotify.
	envs := drainFor(ch, 500*time.Millisecond)
	if len(envs) != 1 {
		t.Errorf("expected 1 envelope after dedup, got %d", len(envs))
	}
	stats := w.StatsSnapshot()
	if stats.UUIDDuped == 0 {
		t.Errorf("expected dedup counter > 0, got %d", stats.UUIDDuped)
	}
}

func TestWatcher_MultipleSubscribers_FanOut(t *testing.T) {
	dir := t.TempDir()
	sid := "sess-fanout"
	path := filepath.Join(dir, sid+".jsonl")
	must(t, os.WriteFile(path, nil, 0o644))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w, err := New(ctx, dir, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()

	ch1 := w.Subscribe(sid)
	ch2 := w.Subscribe(sid)

	must(t, appendLine(path,
		`{"type":"user","uuid":"u1","sessionId":"`+sid+`","message":{"role":"user","content":[]}}`))

	a := readWithTimeout(t, ch1, 1, 3*time.Second)
	b := readWithTimeout(t, ch2, 1, 3*time.Second)
	if a[0].SourceUUID != "u1" || b[0].SourceUUID != "u1" {
		t.Errorf("fanout failed: %q vs %q", a[0].SourceUUID, b[0].SourceUUID)
	}
}

func TestWatcher_Subscribe_AfterClose_ReturnsClosedChan(t *testing.T) {
	dir := t.TempDir()
	w, err := New(context.Background(), dir, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ch := w.Subscribe("any")
	select {
	case _, ok := <-ch:
		if ok {
			t.Errorf("expected closed channel, got value")
		}
	case <-time.After(time.Second):
		t.Errorf("expected closed channel, got block")
	}
}

func TestWatcher_HookEvent_NotDroppedOnSlowSubscriber(t *testing.T) {
	// HOOK / STATE envelopes are best-effort drop-newest. Verify
	// that filling the buffer triggers OnDrop and increments
	// counters — without crashing the watcher.
	dir := t.TempDir()
	sid := "sess-slow"
	path := filepath.Join(dir, sid+".jsonl")
	must(t, os.WriteFile(path, nil, 0o644))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var dropCount atomic.Int64
	w, err := New(ctx, dir, Options{
		SubscriberBuffer: 2, // tiny buffer to force drops
		OnDrop: func(_ string, _ EnvelopeKind) {
			dropCount.Add(1)
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()

	_ = w.Subscribe(sid) // never drained

	// Write 20 STATE records — buffer fills at 2, rest drop.
	for i := 0; i < 20; i++ {
		must(t, appendLine(path,
			fmt.Sprintf(`{"type":"permission-mode","permissionMode":"auto","sessionId":"%s","seq":%d}`, sid, i)))
	}

	// Wait for fsnotify to settle.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			if dropCount.Load() == 0 {
				t.Fatalf("expected drops on slow subscriber, got 0")
			}
			return
		default:
			if dropCount.Load() > 0 {
				return // success
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// --- integration test: simulate a realistic cc session sequence ---

func TestWatcher_Integration_CCSessionSequence(t *testing.T) {
	dir := t.TempDir()
	sid := "11111111-2222-3333-4444-555555555555"
	path := filepath.Join(dir, sid+".jsonl")
	must(t, os.WriteFile(path, nil, 0o644))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w, err := New(ctx, dir, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()

	ch := w.Subscribe(sid)

	// Replay the realistic sequence: permission-mode → SessionStart hook →
	// user → assistant text → assistant tool_use → user tool_result → ai-title.
	steps := []struct {
		name       string
		line       string
		wantKind   EnvelopeKind
		wantClass  Class
	}{
		{"permission-mode",
			`{"type":"permission-mode","permissionMode":"auto","sessionId":"` + sid + `"}`,
			KindSessionState, ClassState},
		{"session-start hook",
			`{"type":"attachment","uuid":"u-att-1","sessionId":"` + sid + `","attachment":{"hookEvent":"SessionStart","hookName":"SessionStart:startup","exitCode":0}}`,
			KindHookEvent, ClassHook},
		{"user message",
			`{"type":"user","uuid":"u-user-1","sessionId":"` + sid + `","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`,
			KindAgentEvent, ClassEvent},
		{"assistant text",
			`{"type":"assistant","uuid":"u-asst-1","parentUuid":"u-user-1","sessionId":"` + sid + `","message":{"role":"assistant","content":[{"type":"text","text":"hello back"}]}}`,
			KindAgentEvent, ClassEvent},
		{"assistant tool_use",
			`{"type":"assistant","uuid":"u-asst-2","parentUuid":"u-asst-1","sessionId":"` + sid + `","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{}}]}}`,
			KindAgentEvent, ClassEvent},
		{"user tool_result",
			`{"type":"user","uuid":"u-user-2","parentUuid":"u-asst-2","sessionId":"` + sid + `","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}}`,
			KindAgentEvent, ClassEvent},
		{"ai-title state",
			`{"type":"ai-title","title":"sample","sessionId":"` + sid + `"}`,
			KindSessionState, ClassState},
	}

	for _, st := range steps {
		must(t, appendLine(path, st.line))
	}

	got := readWithTimeout(t, ch, len(steps), 5*time.Second)
	if len(got) != len(steps) {
		t.Fatalf("got %d envelopes, want %d", len(got), len(steps))
	}
	for i, st := range steps {
		if got[i].Kind != st.wantKind {
			t.Errorf("step %d (%s): kind = %v, want %v", i, st.name, got[i].Kind, st.wantKind)
		}
		if got[i].Class != st.wantClass {
			t.Errorf("step %d (%s): class = %v, want %v", i, st.name, got[i].Class, st.wantClass)
		}
		if got[i].SessionID != sid {
			t.Errorf("step %d (%s): sessionID = %q", i, st.name, got[i].SessionID)
		}
	}
	stats := w.StatsSnapshot()
	if stats.LinesParsed != int64(len(steps)) {
		t.Errorf("LinesParsed = %d, want %d", stats.LinesParsed, len(steps))
	}
	if stats.EnvelopesEmit != int64(len(steps)) {
		t.Errorf("EnvelopesEmit = %d, want %d", stats.EnvelopesEmit, len(steps))
	}
}

// --- helpers ---

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func appendLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		return err
	}
	return nil
}

func drainFor(ch <-chan Envelope, d time.Duration) []Envelope {
	var out []Envelope
	deadline := time.After(d)
	for {
		select {
		case env, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, env)
		case <-deadline:
			return out
		}
	}
}
