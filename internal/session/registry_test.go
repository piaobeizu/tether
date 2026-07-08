package session

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/wire"
)

// fakeSession is a minimal agent.Session for driving Registry.fanOut in
// tests without a real cc subprocess. SendPrompt calls are captured
// (Prompts) rather than discarded so DeliverAction tests (tether#8 T8) can
// assert on exactly what was delivered. Interrupt calls are similarly
// counted (InterruptCalls) so InterruptSession tests (tether#8 T9) can
// assert the call reached the session without a real cc process to observe.
type fakeSession struct {
	sid    string
	events chan agent.Event

	mu             sync.Mutex
	prompts        []string
	interruptCalls int
}

func (f *fakeSession) SessionID() string { return f.sid }
func (f *fakeSession) SendPrompt(_ context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompts = append(f.prompts, text)
	return nil
}
func (f *fakeSession) Events() <-chan agent.Event { return f.events }
func (f *fakeSession) Interrupt() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.interruptCalls++
	return nil
}
func (f *fakeSession) Close() error { return nil }

// Prompts returns a snapshot of every string passed to SendPrompt so far.
func (f *fakeSession) Prompts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.prompts...)
}

// InterruptCalls returns how many times Interrupt() has been called so far.
func (f *fakeSession) InterruptCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.interruptCalls
}

// fakeProvider hands back a pre-built fakeSession regardless of SpawnConfig.
type fakeProvider struct{ sess *fakeSession }

func (p *fakeProvider) Name() string { return "fake" }
func (p *fakeProvider) Spawn(_ context.Context, _ agent.SpawnConfig) (agent.Session, error) {
	return p.sess, nil
}

// TestRegistry_FencedBlockSuppressedFromMessageAndHistory drives a session
// through the registry's real fanOut (no direct FenceParser calls) and
// asserts: (1) a KindFenced envelope carries the extracted block, (2) the
// raw fence marker/JSON never appears in any KindMessage envelope, and (3)
// HistoryStore ends up with the SUPPRESSED text, not the raw fence text.
func TestRegistry_FencedBlockSuppressedFromMessageAndHistory(t *testing.T) {
	fs := &fakeSession{sid: "sid1", events: make(chan agent.Event, 64)}
	reg := NewRegistry(&fakeProvider{sess: fs})
	reg.History = NewHistoryStore(t.TempDir())

	entry, err := reg.GetOrSpawnEntry(context.Background(), "", "fake")
	if err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}

	subCh := make(chan wire.Envelope, 16)
	entry.Subscribe(subCh)

	// Simulate a turn: plain text, then a fenced dag block on its own lines,
	// then trailing text with no final newline (a realistic last delta),
	// then turn-end.
	fs.events <- agent.Event{Kind: agent.EventInit, SessionID: "sid1"}
	fs.events <- agent.Event{Kind: agent.EventText, Text: "before text\n"}
	fs.events <- agent.Event{Kind: agent.EventText, Text: "```dag:s\n{\"a\":1}\n```\n"}
	fs.events <- agent.Event{Kind: agent.EventText, Text: "after text"}
	fs.events <- agent.Event{Kind: agent.EventResult, Text: "stop"}
	close(fs.events)

	var envs []wire.Envelope
	timeout := time.After(2 * time.Second)
collect:
	for {
		select {
		case env := <-subCh:
			envs = append(envs, env)
			if env.Kind == wire.KindResult {
				break collect
			}
		case <-timeout:
			t.Fatal("timed out waiting for envelopes")
		}
	}

	var messages []string
	var fenced []wire.FencedBlock
	for _, env := range envs {
		switch env.Kind {
		case wire.KindMessage:
			if s, ok := env.Payload.(string); ok {
				messages = append(messages, s)
			}
		case wire.KindFenced:
			if fb, ok := env.Payload.(wire.FencedBlock); ok {
				fenced = append(fenced, fb)
			}
		}
	}

	joined := strings.Join(messages, "")
	if joined != "before text\nafter text" {
		t.Errorf("KindMessage text = %q, want %q", joined, "before text\nafter text")
	}
	if strings.Contains(joined, "dag:s") || strings.Contains(joined, `"a":1`) {
		t.Errorf("raw fence text leaked into KindMessage stream: %q", joined)
	}

	if len(fenced) != 1 {
		t.Fatalf("len(fenced) = %d, want 1", len(fenced))
	}
	if fenced[0].Kind != wire.FencedBlockDag || fenced[0].Skill != "s" || fenced[0].Content != `{"a":1}` {
		t.Errorf("fenced block = %+v, want {dag s {\"a\":1} s-0}", fenced[0])
	}
	if fenced[0].BlockID != "s-0" {
		t.Errorf("BlockID = %q, want s-0", fenced[0].BlockID)
	}

	// History must contain the SUPPRESSED text (fence removed), never the
	// raw fence marker/JSON — the KindResult receive above happens-after
	// FinalizeAssistant runs (same goroutine, program order before the send).
	//
	// Text before and after the block now persist as SEPARATE ordered
	// entries (tether#8 T7: the block is flushed as its own history entry
	// in between, see AppendBlock), so concatenate every assistant-role
	// entry's Text in order rather than expecting a single merged message.
	msgs := reg.History.LoadHistory("sid1")
	var assistantText string
	for _, m := range msgs {
		if m.Role == "assistant" {
			assistantText += m.Text
		}
	}
	if assistantText != "before text\nafter text" {
		t.Errorf("history assistant text = %q, want %q", assistantText, "before text\nafter text")
	}
	if strings.Contains(assistantText, "dag:s") || strings.Contains(assistantText, `"a":1`) {
		t.Errorf("raw fence text leaked into history: %q", assistantText)
	}
}

// TestRegistry_SegmentOrderPreservedAcrossBroadcast drives text-before,
// block, text-after through the real fanOut in a SINGLE EventText delta
// (mirroring how a fenced block that opens and closes within one stream
// chunk actually arrives) and asserts the broadcast envelopes preserve
// exact stream order: KindMessage("before..."), KindFenced, KindMessage
// ("after..."), KindResult — never blocks-then-text or text merged out of
// order (D-19 fix #3, intra-Feed reordering).
func TestRegistry_SegmentOrderPreservedAcrossBroadcast(t *testing.T) {
	fs := &fakeSession{sid: "sid2", events: make(chan agent.Event, 64)}
	reg := NewRegistry(&fakeProvider{sess: fs})

	entry, err := reg.GetOrSpawnEntry(context.Background(), "", "fake")
	if err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}

	subCh := make(chan wire.Envelope, 16)
	entry.Subscribe(subCh)

	fs.events <- agent.Event{Kind: agent.EventInit, SessionID: "sid2"}
	fs.events <- agent.Event{Kind: agent.EventText, Text: "before text\n```dag:s\n{\"x\":1}\n```\nafter text\n"}
	fs.events <- agent.Event{Kind: agent.EventResult, Text: "stop"}
	close(fs.events)

	var envs []wire.Envelope
	timeout := time.After(2 * time.Second)
collect:
	for {
		select {
		case env := <-subCh:
			envs = append(envs, env)
			if env.Kind == wire.KindResult {
				break collect
			}
		case <-timeout:
			t.Fatal("timed out waiting for envelopes")
		}
	}

	var kinds []wire.EnvelopeKind
	for _, e := range envs {
		kinds = append(kinds, e.Kind)
	}
	wantKinds := []wire.EnvelopeKind{wire.KindMessage, wire.KindFenced, wire.KindMessage, wire.KindResult}
	if len(kinds) != len(wantKinds) {
		t.Fatalf("envelope kinds = %v, want %v", kinds, wantKinds)
	}
	for i := range wantKinds {
		if kinds[i] != wantKinds[i] {
			t.Fatalf("envelope kinds = %v, want %v", kinds, wantKinds)
		}
	}
	if s, _ := envs[0].Payload.(string); s != "before text\n" {
		t.Errorf("envs[0].Payload = %q, want %q", s, "before text\n")
	}
	if s, _ := envs[2].Payload.(string); s != "after text\n" {
		t.Errorf("envs[2].Payload = %q, want %q", s, "after text\n")
	}
	fb, ok := envs[1].Payload.(wire.FencedBlock)
	if !ok || fb.Content != `{"x":1}` {
		t.Errorf("envs[1].Payload = %+v, want FencedBlock with content {\"x\":1}", envs[1].Payload)
	}
}

// TestRegistry_EventInitResetsStaleOpenFence drives a turn that opens a
// fence and is then interrupted (no EventResult — Flush never runs),
// followed by a NEW turn's EventInit and plain text. It asserts the new
// turn's text is broadcast normally rather than being swallowed by the
// stale open-fence state left behind by the interrupted turn (D-19 fix #4,
// cross-turn stranding). This exercises fanOut's ResetTurn() call on
// EventInit end-to-end, not just the FenceParser unit directly.
func TestRegistry_EventInitResetsStaleOpenFence(t *testing.T) {
	fs := &fakeSession{sid: "sid3", events: make(chan agent.Event, 64)}
	reg := NewRegistry(&fakeProvider{sess: fs})

	entry, err := reg.GetOrSpawnEntry(context.Background(), "", "fake")
	if err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}

	subCh := make(chan wire.Envelope, 16)
	entry.Subscribe(subCh)

	// Turn 1: open a fence, then get interrupted — no EventResult follows.
	fs.events <- agent.Event{Kind: agent.EventInit, SessionID: "sid3"}
	fs.events <- agent.Event{Kind: agent.EventText, Text: "```dag:s\n{\"partial\":true\n"}
	// Turn 2: a fresh system/init (same session id, per-turn metadata
	// refresh) followed by ordinary text and a clean turn-end.
	fs.events <- agent.Event{Kind: agent.EventInit, SessionID: "sid3"}
	fs.events <- agent.Event{Kind: agent.EventText, Text: "hello world\n"}
	fs.events <- agent.Event{Kind: agent.EventResult, Text: "stop"}
	close(fs.events)

	var envs []wire.Envelope
	timeout := time.After(2 * time.Second)
collect:
	for {
		select {
		case env := <-subCh:
			envs = append(envs, env)
			if env.Kind == wire.KindResult {
				break collect
			}
		case <-timeout:
			t.Fatal("timed out waiting for envelopes")
		}
	}

	var messages []string
	for _, env := range envs {
		if env.Kind == wire.KindMessage {
			if s, ok := env.Payload.(string); ok {
				messages = append(messages, s)
			}
		}
		if env.Kind == wire.KindFenced {
			t.Errorf("unexpected KindFenced envelope from an interrupted, never-closed fence: %+v", env.Payload)
		}
	}

	joined := strings.Join(messages, "")
	if joined != "hello world\n" {
		t.Errorf("KindMessage text = %q, want %q (turn 2 text must not be swallowed)", joined, "hello world\n")
	}
}

// TestRegistry_BlockPersistedInHistoryOrder — (tether#8 T7) drives a single
// turn of text-before-block-then-text through the real fanOut (same input
// shape as TestRegistry_SegmentOrderPreservedAcrossBroadcast, which asserts
// the LIVE broadcast order) and asserts HistoryStore.LoadHistory returns the
// SAME three entries in the SAME order with the block payload intact — so a
// page reload reconstructs the DAG card exactly where it rendered live,
// instead of losing it (the T6-era bug this task fixes: blocks broadcast
// but never persisted).
func TestRegistry_BlockPersistedInHistoryOrder(t *testing.T) {
	fs := &fakeSession{sid: "sid4", events: make(chan agent.Event, 64)}
	reg := NewRegistry(&fakeProvider{sess: fs})
	reg.History = NewHistoryStore(t.TempDir())

	entry, err := reg.GetOrSpawnEntry(context.Background(), "", "fake")
	if err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}

	subCh := make(chan wire.Envelope, 16)
	entry.Subscribe(subCh)

	fs.events <- agent.Event{Kind: agent.EventInit, SessionID: "sid4"}
	fs.events <- agent.Event{Kind: agent.EventText, Text: "before text\n```dag:s\n{\"x\":1}\n```\nafter text\n"}
	fs.events <- agent.Event{Kind: agent.EventResult, Text: "stop"}
	close(fs.events)

	timeout := time.After(2 * time.Second)
collect:
	for {
		select {
		case env := <-subCh:
			if env.Kind == wire.KindResult {
				break collect
			}
		case <-timeout:
			t.Fatal("timed out waiting for envelopes")
		}
	}

	msgs := reg.History.LoadHistory("sid4")
	if len(msgs) != 3 {
		t.Fatalf("len(msgs) = %d, want 3 (text, block, text): %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "assistant" || msgs[0].Text != "before text\n" || msgs[0].Block != nil {
		t.Errorf("msgs[0] = %+v, want assistant/\"before text\\n\", no block", msgs[0])
	}
	if msgs[1].Block == nil {
		t.Fatalf("msgs[1].Block = nil, want a FencedBlock: %+v", msgs[1])
	}
	wantBlock := wire.FencedBlock{Kind: wire.FencedBlockDag, Skill: "s", Content: `{"x":1}`, BlockID: "s-0"}
	if *msgs[1].Block != wantBlock {
		t.Errorf("msgs[1].Block = %+v, want %+v", *msgs[1].Block, wantBlock)
	}
	if msgs[1].Text != "" {
		t.Errorf("msgs[1].Text = %q, want empty (block-only entry)", msgs[1].Text)
	}
	if msgs[2].Role != "assistant" || msgs[2].Text != "after text\n" || msgs[2].Block != nil {
		t.Errorf("msgs[2] = %+v, want assistant/\"after text\\n\", no block", msgs[2])
	}
}

// TestRegistry_TextOnlySessionHistoryUnchanged — (tether#8 T7 regression) a
// turn with no fenced blocks at all must persist exactly as it did before
// this change: a single concatenated assistant history entry, no Block
// field set anywhere.
func TestRegistry_TextOnlySessionHistoryUnchanged(t *testing.T) {
	fs := &fakeSession{sid: "sid5", events: make(chan agent.Event, 64)}
	reg := NewRegistry(&fakeProvider{sess: fs})
	reg.History = NewHistoryStore(t.TempDir())

	entry, err := reg.GetOrSpawnEntry(context.Background(), "", "fake")
	if err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}

	subCh := make(chan wire.Envelope, 16)
	entry.Subscribe(subCh)

	reg.RecordUserMessage("sid5", "hello")
	fs.events <- agent.Event{Kind: agent.EventInit, SessionID: "sid5"}
	fs.events <- agent.Event{Kind: agent.EventText, Text: "hi "}
	fs.events <- agent.Event{Kind: agent.EventText, Text: "there\n"}
	fs.events <- agent.Event{Kind: agent.EventResult, Text: "stop"}
	close(fs.events)

	timeout := time.After(2 * time.Second)
collect:
	for {
		select {
		case env := <-subCh:
			if env.Kind == wire.KindResult {
				break collect
			}
		case <-timeout:
			t.Fatal("timed out waiting for envelopes")
		}
	}

	msgs := reg.History.LoadHistory("sid5")
	if len(msgs) != 2 {
		t.Fatalf("len(msgs) = %d, want 2 (user, assistant): %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" || msgs[0].Text != "hello" {
		t.Errorf("msgs[0] = %+v, want user/hello", msgs[0])
	}
	if msgs[1].Role != "assistant" || msgs[1].Text != "hi there\n" || msgs[1].Block != nil {
		t.Errorf("msgs[1] = %+v, want assistant/\"hi there\\n\", no block", msgs[1])
	}
}

// waitForRegistered polls until sid is registered in reg. GetOrSpawnEntry
// registers a fresh session under a temp key and re-keys it to the real sid
// on a separate goroutine once sess.SessionID() resolves (see its doc
// comment) — production code observes the same async window, so tests that
// look sid up by its real id must wait for it rather than assuming it's
// already there the instant GetOrSpawnEntry returns. Bounded so a genuine
// bug (sid never registered) fails fast instead of hanging.
func waitForRegistered(t *testing.T, reg *Registry, sid string) {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if reg.IsLive(sid) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("session %q never registered under its real sid", sid)
}

// TestRegistry_DeliverAction_Approve — (tether#8 T8) DeliverAction routes an
// "approve" fenced-block callback to the named session's agent via
// SendPrompt, wrapped in the __tether_action__ control marker documented in
// docs/wire/fenced-contract.md §5.
func TestRegistry_DeliverAction_Approve(t *testing.T) {
	fs := &fakeSession{sid: "sid-approve", events: make(chan agent.Event, 4)}
	reg := NewRegistry(&fakeProvider{sess: fs})

	if _, err := reg.GetOrSpawnEntry(context.Background(), "", "fake"); err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}
	waitForRegistered(t, reg, "sid-approve")

	if err := reg.DeliverAction("sid-approve", "approve", "s-0", "planner"); err != nil {
		t.Fatalf("DeliverAction: %v", err)
	}

	prompts := fs.Prompts()
	if len(prompts) != 1 {
		t.Fatalf("len(prompts) = %d, want 1: %+v", len(prompts), prompts)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(prompts[0]), &got); err != nil {
		t.Fatalf("delivered payload not valid JSON: %v (%q)", err, prompts[0])
	}
	inner, ok := got["__tether_action__"].(map[string]any)
	if !ok {
		t.Fatalf("delivered payload missing __tether_action__ marker: %q", prompts[0])
	}
	if inner["action"] != "approve" || inner["blockId"] != "s-0" || inner["skill"] != "planner" {
		t.Errorf("__tether_action__ = %+v, want {action:approve blockId:s-0 skill:planner}", inner)
	}
}

// TestRegistry_DeliverAction_UnknownSession — an action naming a session the
// registry has never heard of (never existed, or already ended) must be
// dropped with an error, never a panic — the /wt/control channel is not
// otherwise session-scoped, so this is an expected race, not a bug.
func TestRegistry_DeliverAction_UnknownSession(t *testing.T) {
	reg := NewRegistry()
	if err := reg.DeliverAction("does-not-exist", "approve", "s-0", "planner"); err == nil {
		t.Fatal("DeliverAction: want error for unknown session, got nil")
	}
}

// TestRegistry_InterruptSession_CallsAgentInterrupt — (tether#8 T9)
// InterruptSession must reach the named session's agent.Session.Interrupt()
// directly, NOT go through SendPrompt/__tether_action__ (that's
// DeliverAction's job for "approve"; "pause" is a transport-level signal).
func TestRegistry_InterruptSession_CallsAgentInterrupt(t *testing.T) {
	fs := &fakeSession{sid: "sid-pause", events: make(chan agent.Event, 4)}
	reg := NewRegistry(&fakeProvider{sess: fs})

	if _, err := reg.GetOrSpawnEntry(context.Background(), "", "fake"); err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}
	waitForRegistered(t, reg, "sid-pause")

	if err := reg.InterruptSession("sid-pause"); err != nil {
		t.Fatalf("InterruptSession: %v", err)
	}

	if got := fs.InterruptCalls(); got != 1 {
		t.Fatalf("InterruptCalls() = %d, want 1", got)
	}
	// InterruptSession must not fall back to SendPrompt/__tether_action__ —
	// that's a different delivery path (DeliverAction).
	if got := fs.Prompts(); len(got) != 0 {
		t.Fatalf("Prompts() = %v, want none — InterruptSession must not call SendPrompt", got)
	}
}

// TestRegistry_InterruptSession_UnknownSession — an unknown or already-ended
// sid must return an error, never panic; same expected race as
// DeliverAction's unknown-session case.
func TestRegistry_InterruptSession_UnknownSession(t *testing.T) {
	reg := NewRegistry()
	if err := reg.InterruptSession("does-not-exist"); err == nil {
		t.Fatal("InterruptSession: want error for unknown session, got nil")
	}
}
