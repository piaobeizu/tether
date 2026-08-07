package session

import (
	"context"
	"errors"
	"testing"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/wire"
)

// tether#77 — reopen's give-up branches must return a classified *Refusal.
//
// The discriminator is not severity, it is whether anything else is still going
// to try. promptErrorEnvelope (internal/server/wt_chat.go) sends the browser a
// frame only for a Refusal, so a bare error from a branch that has run out of
// moves is a prompt the user typed, watched disappear, and was told nothing
// about — while the turn-ending frame from tether#59 puts the tab back to
// looking idle and healthy. Every test below is one of those branches.
//
// The two branches that must STAY bare have their own tests at the bottom.
// Getting that half wrong is the opposite failure and is just as real: an
// envelope on a path that recovers itself shows an error for a turn that then
// answers normally, and resets the browser's streaming state mid-flight.

// wantUndelivered asserts err carries ErrCodePromptUndelivered and that the code
// is retryable, which is the half a Refusal check alone would miss: marking this
// terminal would stop the browser's reconnect ladder on the one failure a
// reconnect actually repairs.
func wantUndelivered(t *testing.T, err error, cause error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: returned nil; the prompt was lost and the caller was told it landed", what)
	}
	var ref *Refusal
	if !errors.As(err, &ref) {
		t.Fatalf("%s: error %v (%T) carries no Refusal — serveChat drops it, so the tab looks "+
			"healthy and swallows this prompt and every one after it", what, err, err)
	}
	if ref.Code != wire.ErrCodePromptUndelivered {
		t.Fatalf("%s: code = %q, want %q", what, ref.Code, wire.ErrCodePromptUndelivered)
	}
	if ref.Code.Terminal() {
		t.Fatalf("%s: %q must stay retryable — a reconnect lands on the --resume path with its "+
			"fallback machinery and a fresh re-open budget", what, ref.Code)
	}
	// The classification must not cost the diagnosis. undelivered's whole shape
	// is "what was being attempted: what it hit", and the second half is the only
	// part that says WHY — it is what reaches the user's line and the operator's
	// log alike. Dropping the %w (keeping the sentence, losing the cause) is a
	// mutation that otherwise survives the entire package.
	if cause != nil && !errors.Is(err, cause) {
		t.Fatalf("%s: %v does not wrap the underlying cause %v — the classified error kept the "+
			"sentence and lost the reason", what, err, cause)
	}
}

func wantUnclassified(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: returned nil", what)
	}
	var ref *Refusal
	if errors.As(err, &ref) {
		t.Fatalf("%s: error %v carries a Refusal (%q); the browser would be shown an error for a "+
			"prompt that is about to be recovered without it", what, err, ref.Code)
	}
}

// The branch the wi is named for: the agent dies a second time on one
// connection, the single re-open is already spent, and reopen declines.
//
// reopen's doc accepted this silence outright ("a long-lived connection whose
// agent dies twice hangs the second time"). That was written before tether#59
// added the turn-ending frame, when the cost was at least legible — the spinner
// kept turning. It does not any more.
func TestSendPrompt_ASpentReopenSaysSoInsteadOfSwallowingThePrompt(t *testing.T) {
	reused := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	reopened := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	p := &reopenProvider{first: reused, second: reopened}
	_, att := seedReuse(t, p, "reused-sid")

	reused.kill()
	if err := att.SendPrompt(context.Background(), "first death"); err != nil {
		t.Fatalf("first SendPrompt: %v", err)
	}

	reopened.kill()
	wantUndelivered(t, att.SendPrompt(context.Background(), "second death"), errBrokenPipe, "second death")

	if got := p.Spawns(); got != 2 {
		t.Errorf("spawns = %d, want 2: classifying the refusal must not also start spawning again", got)
	}
}

// The replacement is spawned and adopted, and then refuses the prompt anyway.
// The budget was spent on that very replacement a few lines earlier, so the next
// prompt takes the branch above — there is nothing behind this one.
func TestSendPrompt_AReplacementThatRefusesThePromptIsReported(t *testing.T) {
	reused := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	reopened := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	// Refuses without being dead: the spawn succeeds, the entry registers, and
	// only the write fails.
	reopened.sendFails.Store(true)
	p := &reopenProvider{first: reused, second: reopened}
	_, att := seedReuse(t, p, "reused-sid")

	reused.kill()
	wantUndelivered(t, att.SendPrompt(context.Background(), "still there?"), errBrokenPipe, "replacement refused")
}

// The sibling-adoption branch (tether#76's code_review found this one). Another
// attachment on the same sid has already re-opened it, so this one adopts that
// session rather than spawning a rival — and if the adopted session refuses the
// prompt, adoption WAS the recovery. Nothing is behind it either.
func TestSendPrompt_AnAdoptedSessionThatRefusesThePromptIsReported(t *testing.T) {
	reused := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	reopened := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	p := &reopenProvider{first: reused, second: reopened}
	reg, att1 := seedReuse(t, p, "reused-sid")

	// A second tab on the same sid: Attach's reuse branch hands it the same entry.
	att2, err := reg.Attach(context.Background(), "reused-sid", "fake", "")
	if err != nil {
		t.Fatalf("second Attach: %v", err)
	}

	// Tab 1 drives the re-open, so "reused-sid" is registered to `reopened`.
	reused.kill()
	if err := att1.SendPrompt(context.Background(), "tab1"); err != nil {
		t.Fatalf("tab1 SendPrompt: %v", err)
	}
	if got := p.Spawns(); got != 2 {
		t.Fatalf("spawns = %d, want 2 before tab2 prompts", got)
	}

	// Now tab2 prompts. Its own entry is the corpse, so it reaches reopen, finds
	// the live sibling, and adopts it — which then refuses.
	reopened.sendFails.Store(true)
	wantUndelivered(t, att2.SendPrompt(context.Background(), "tab2"), errBrokenPipe, "adopted sibling refused")

	if got := p.Spawns(); got != 2 {
		t.Errorf("spawns = %d, want 2: adoption must not spawn, classified or not", got)
	}
}

// The `cur != dead` branch: a concurrent prompt already re-pointed this
// attachment while this one waited on reopenMu, so it delivers onto the
// replacement instead of spawning a second. Driven through reopen directly
// because the natural race is not deterministic — passing the STALE entry as
// `dead` is exactly the state a loser of that race observes.
func TestReopen_DeliveringOntoAlreadySwappedEntryIsReportedWhenItFails(t *testing.T) {
	reused := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	reopened := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	p := &reopenProvider{first: reused, second: reopened}
	_, att := seedReuse(t, p, "reused-sid")

	stale := attEntry(att)
	reused.kill()
	if err := att.SendPrompt(context.Background(), "the winner"); err != nil {
		t.Fatalf("winning SendPrompt: %v", err)
	}
	if attEntry(att) == stale {
		t.Fatal("the attachment was not re-pointed; this test is not on the branch it names")
	}

	reopened.sendFails.Store(true)
	wantUndelivered(t,
		att.reopen(context.Background(), stale, "the loser", errors.New("write |1: broken pipe")),
		errBrokenPipe, "delivery onto the already-swapped entry")

	if got := p.Spawns(); got != 2 {
		t.Errorf("spawns = %d, want 2: this branch must not spawn a second replacement", got)
	}
}

// --- the two branches that must STAY bare ---------------------------------

// A cancelled ctx means the browser is already gone. There is nobody to show an
// envelope to, and classifying it would put an error on a connection that ended
// for an ordinary reason.
func TestSendPrompt_AClientThatWentAwayIsNotClassified(t *testing.T) {
	reused := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	p := &reopenProvider{first: reused, second: &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}}
	_, att := seedReuse(t, p, "reused-sid")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reused.kill()
	wantUnclassified(t, att.SendPrompt(ctx, "nobody is listening"), "cancelled ctx")

	if got := p.Spawns(); got != 1 {
		t.Errorf("spawns = %d, want 1: a departed client must not trigger a re-open", got)
	}
}

// A failed `--resume` is not reopen's to recover: Resolve falls back to a fresh
// session and replays this very prompt. An envelope here would report a failure
// for a prompt that is about to be answered.
//
// Covered from the other side by attach_test.go's
// TestSendPrompt_UnrecoverableReopenIsClassifiedAndARecoverableSendIsNot; kept
// here as the negative control for THIS file, so that a change which classified
// every branch indiscriminately fails a test that lives next to the ones it
// would have made pass.
func TestSendPrompt_AFailedResumeStaysUnclassified(t *testing.T) {
	dp := &deadThenLiveProvider{
		dead: newDeadSession(),
		live: &fakeSession{sid: "recovered-sid", events: make(chan agent.Event, 8)},
	}
	reg := NewRegistry(dp)
	att, err := reg.Attach(context.Background(), "gone-sid", "fake", "")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	wantUnclassified(t, att.SendPrompt(context.Background(), "remember ALPHA"), "failed resume")
}
