package server

import (
	"context"
	"testing"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/session"
)

// admissionSession is a healthy agent.Session double that adopts the id it is
// spawned with, i.e. real cc's measured behaviour (mem_2ruSlrHR ①) — which is what
// puts the entry in the registry under the sid a client will ask about, from before
// the process exists.
type admissionSession struct {
	sid    string
	events chan agent.Event
}

func (s *admissionSession) SessionID() string                        { return s.sid }
func (s *admissionSession) Alive() bool                              { return true }
func (s *admissionSession) SendPrompt(context.Context, string) error { return nil }
func (s *admissionSession) Events() <-chan agent.Event               { return s.events }
func (s *admissionSession) Interrupt() error                         { return nil }
func (s *admissionSession) Close() error                             { return nil }

type admissionProvider struct{ last *admissionSession }

func (p *admissionProvider) Name() string { return "fake" }
func (p *admissionProvider) Spawn(_ context.Context, cfg agent.SpawnConfig) (agent.Session, error) {
	sid := cfg.SessionID
	if sid == "" {
		sid = cfg.ResumeSessionID
	}
	p.last = &admissionSession{sid: sid, events: make(chan agent.Event, 4)}
	return p.last, nil
}

// TestAdmitChat_LetsASecondClientJoinASessionNobodyOwnsYet pins the chat gate's
// policy at the seam that actually runs it.
//
// tether#54 registers a session under its real sid from before its agent process
// exists, which is what makes a reconnect find it. That also makes the sid answer
// "live" immediately — and the gate this replaces read a live-but-unowned session as
// "owned by another client", because IsOwner compares against an empty owner and
// says no. Ownership is claimed only after Attachment.Resolve, which for cc waits
// for the user's first prompt, so the window is however long the user takes to
// type. A second tab of the same browser (the client id is per-credential, so tabs
// share it) would be refused, shown nothing — the frontend's error branch renders
// no message — and would reconnect on the same sid, in a loop, until the first tab
// finished its turn.
//
// Asserted here rather than only on Registry.OwnedByOther because the defect was
// the COMPOSITION at this call site: a session-level test of the registry passes
// happily while this gate asks the wrong question.
func TestAdmitChat_LetsASecondClientJoinASessionNobodyOwnsYet(t *testing.T) {
	p := &admissionProvider{}
	reg := session.NewRegistry(p)

	att, err := reg.Attach(context.Background(), "", "fake")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	sid := p.last.sid
	defer close(p.last.events)
	if !reg.IsLive(sid) {
		t.Fatal("the session is not live; this test needs tether#54's eager registration")
	}

	// Nobody has claimed it yet: neither the first client nor a second may be
	// turned away.
	if !admitChat(reg, sid, "client-1") {
		t.Error("the client that opened the session was refused its own session")
	}
	if !admitChat(reg, sid, "client-2") {
		t.Error("a second client was refused a session NOBODY owns; this is the silent " +
			"reconnect loop — the frontend renders no message for the error and retries the same sid")
	}

	// After a claim the rule is unchanged: the owner is admitted, others are not.
	if !att.SetOwner("client-1") {
		t.Fatal("first claim was rejected")
	}
	if !admitChat(reg, sid, "client-1") {
		t.Error("the owner was refused its own session")
	}
	if admitChat(reg, sid, "client-2") {
		t.Error("a second client was admitted to a session client-1 owns; the #83 gate is inert")
	}

	// Degenerate inputs are admitted: there is nothing to conflict with.
	if !admitChat(reg, "", "client-2") {
		t.Error("a connection with no sid was refused")
	}
	if !admitChat(reg, sid, "") {
		t.Error("a connection with no client id was refused")
	}
	if !admitChat(reg, "never-existed", "client-2") {
		t.Error("a sid the registry has never seen was refused")
	}
}

// TestAdmitChat_AdmitsAResumeAttemptFromTheSameClient — the tether#54 reconnect
// shape end-to-end through the gate: a sid whose agent is gone is re-attached with
// `cc --resume`, the new entry is registered under that same sid immediately, and a
// SECOND connection carrying it (the other tab) must still be admitted so it can
// reuse rather than start a competing resume.
func TestAdmitChat_AdmitsAResumeAttemptFromTheSameClient(t *testing.T) {
	p := &admissionProvider{}
	reg := session.NewRegistry(p)

	if _, err := reg.Attach(context.Background(), "old-sid", "fake"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer close(p.last.events)

	// Asserted immediately, with no polling: a poll would also pass on the retired
	// placeholder scheme, where the sid became a key only once the agent announced
	// it. The point here is that it is a key ALREADY.
	if !reg.IsLive("old-sid") {
		t.Fatal("the resume attempt is not registered under the sid it is resuming; " +
			"a second tab would miss it and start a competing `cc --resume`")
	}
	if !admitChat(reg, "old-sid", "client-1") {
		t.Error("the second tab was refused the resume attempt it should reuse")
	}
}
