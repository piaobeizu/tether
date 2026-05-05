package claude

import (
	"context"
	"testing"
	"time"
)

// makeRateLimit synthesizes a rate_limit_event Envelope. raw uses the real
// cc wire schema (camelCase fields under rate_limit_info).
func makeRateLimit(t *testing.T, status, rlType, overageStatus string, isUsingOverage bool) Envelope {
	t.Helper()
	raw := []byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"` + status +
		`","resetsAt":1777593600,"rateLimitType":"` + rlType +
		`","overageStatus":"` + overageStatus +
		`","overageResetsAt":1777593600,"isUsingOverage":` +
		map[bool]string{true: "true", false: "false"}[isUsingOverage] +
		`},"uuid":"u-rl","session_id":"sid-rl"}`)
	env, err := ParseLine(raw)
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	return env
}

// ───────── Decoder tests ─────────

// Decode the real cc fixture line — verifies every field round-trips.
func TestDecodeRateLimit_RealFixtureLine(t *testing.T) {
	raw := []byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1777593600,"rateLimitType":"overage","overageStatus":"allowed","overageResetsAt":1777593600,"isUsingOverage":false},"uuid":"7ea195ff-1bc1-4a67-be07-04cb13fddc0f","session_id":"c840b5fb-96dd-4593-bc36-602689783a59"}`)
	env, err := ParseLine(raw)
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	rl, err := env.DecodeRateLimit()
	if err != nil {
		t.Fatalf("DecodeRateLimit: %v", err)
	}
	info := rl.RateLimitInfo
	if info.Status != "allowed" {
		t.Errorf("Status: %q", info.Status)
	}
	if info.ResetsAt != 1777593600 {
		t.Errorf("ResetsAt: %d", info.ResetsAt)
	}
	if info.RateLimitType != "overage" {
		t.Errorf("RateLimitType: %q", info.RateLimitType)
	}
	if info.OverageStatus != "allowed" {
		t.Errorf("OverageStatus: %q", info.OverageStatus)
	}
	if info.OverageResetsAt != 1777593600 {
		t.Errorf("OverageResetsAt: %d", info.OverageResetsAt)
	}
	if info.IsUsingOverage {
		t.Errorf("IsUsingOverage should be false")
	}
}

// Decode rejects non-rate_limit_event types.
func TestDecodeRateLimit_WrongTypeRejected(t *testing.T) {
	raw := []byte(`{"type":"system","subtype":"init","session_id":"x","uuid":"u"}`)
	env, _ := ParseLine(raw)
	if _, err := env.DecodeRateLimit(); err == nil {
		t.Errorf("expected error decoding non-rate_limit envelope, got nil")
	}
}

// ───────── EvaluateRateLimit pure-fn tests ─────────

func TestEvaluateRateLimit_TableDriven(t *testing.T) {
	cases := []struct {
		name string
		info RateLimitInfo
		want RateLimitDecision
	}{
		{"empty info → allow (optimistic)", RateLimitInfo{}, RLDecisionAllow},
		{"primary allowed, no overage", RateLimitInfo{Status: "allowed", OverageStatus: "allowed"}, RLDecisionAllow},
		{"primary allowed, using overage → warn", RateLimitInfo{Status: "allowed", OverageStatus: "allowed", IsUsingOverage: true}, RLDecisionWarn},
		{"primary exceeded → refuse", RateLimitInfo{Status: "exceeded", OverageStatus: "allowed"}, RLDecisionRefuse},
		{"primary throttled → refuse", RateLimitInfo{Status: "throttled"}, RLDecisionRefuse},
		{"overage exhausted → refuse", RateLimitInfo{Status: "allowed", OverageStatus: "exceeded"}, RLDecisionRefuse},
		{"primary refuse trumps overage allowed", RateLimitInfo{Status: "exceeded", OverageStatus: "allowed", IsUsingOverage: true}, RLDecisionRefuse},
		{"only OverageStatus set, blocked → refuse", RateLimitInfo{OverageStatus: "blocked"}, RLDecisionRefuse},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := EvaluateRateLimit(c.info)
			if got != c.want {
				t.Errorf("info=%+v: want %s, got %s", c.info, c.want, got)
			}
		})
	}
}

func TestRateLimitDecision_StringFormat(t *testing.T) {
	for _, c := range []struct {
		d    RateLimitDecision
		want string
	}{
		{RLDecisionAllow, "allow"},
		{RLDecisionWarn, "warn"},
		{RLDecisionRefuse, "refuse"},
		{RateLimitDecision(99), "unknown"},
	} {
		if got := c.d.String(); got != c.want {
			t.Errorf("decision %d: want %q, got %q", c.d, c.want, got)
		}
	}
}

// ───────── Session capture tests ─────────

func rateLimitTestSession(t *testing.T) *Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	sess, err := New(ctx, SpawnOpts{BinaryPath: "/bin/cat"}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	go drainEvents(sess)
	return sess
}

// Before any rate_limit_event is observed: LastRateLimit() returns ok=false.
func TestSession_LastRateLimit_NoneSeen(t *testing.T) {
	sess := rateLimitTestSession(t)
	info, at, ok := sess.LastRateLimit()
	if ok {
		t.Errorf("expected ok=false before any RL event, got ok=true")
	}
	if (info != RateLimitInfo{}) {
		t.Errorf("expected zero RateLimitInfo, got %+v", info)
	}
	if !at.IsZero() {
		t.Errorf("expected zero time, got %v", at)
	}
}

// Session captures the latest RL event; subsequent ones overwrite.
func TestSession_RecordRateLimit_LatestWins(t *testing.T) {
	sess := rateLimitTestSession(t)

	sess.recordRateLimit(makeRateLimit(t, "allowed", "overage", "allowed", false))
	info1, at1, ok1 := sess.LastRateLimit()
	if !ok1 {
		t.Fatalf("expected ok=true after first record")
	}
	if info1.Status != "allowed" || info1.IsUsingOverage {
		t.Errorf("first record: %+v", info1)
	}
	if at1.IsZero() {
		t.Errorf("recordedAt should be non-zero")
	}

	// Second event with different status → overwrites.
	sess.recordRateLimit(makeRateLimit(t, "exceeded", "primary", "allowed", true))
	info2, at2, ok2 := sess.LastRateLimit()
	if !ok2 {
		t.Fatalf("ok=false after second record")
	}
	if info2.Status != "exceeded" || !info2.IsUsingOverage {
		t.Errorf("second record: %+v", info2)
	}
	if !at2.After(at1) && !at2.Equal(at1) {
		t.Errorf("recordedAt should advance or equal: at1=%v at2=%v", at1, at2)
	}
}

// recordRateLimit silently ignores envelopes that fail to decode (e.g.
// wrong type). Best-effort observation.
func TestSession_RecordRateLimit_BadEnvelopeIgnored(t *testing.T) {
	sess := rateLimitTestSession(t)
	// Pass an envelope with wrong type.
	raw := []byte(`{"type":"assistant","uuid":"u-x"}`)
	env, _ := ParseLine(raw)

	sess.recordRateLimit(env) // should not panic, should not record
	if _, _, ok := sess.LastRateLimit(); ok {
		t.Errorf("non-rate_limit envelope must not flip hasRateLimit to true")
	}
}

// End-to-end: combine recording + evaluation. Exercises the typical daemon
// pre-flight pattern: pull last RL → evaluate → decide.
func TestSession_PreflightPattern_Refuse(t *testing.T) {
	sess := rateLimitTestSession(t)
	sess.recordRateLimit(makeRateLimit(t, "exceeded", "primary", "allowed", false))

	info, _, ok := sess.LastRateLimit()
	if !ok {
		t.Fatalf("expected ok=true")
	}
	got := EvaluateRateLimit(info)
	if got != RLDecisionRefuse {
		t.Errorf("expected refuse for exceeded primary status, got %s", got)
	}
}
