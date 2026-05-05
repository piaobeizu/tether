package claude

// RateLimitDecision is the outcome of evaluating a RateLimitInfo against the
// pre-flight policy. Callers (typically daemon-side, ahead of forwarding a
// user message) consume this to decide whether to forward, warn, or refuse.
//
// The decision is advisory — Session.Send does NOT consult it. Higher
// layers wire it into their request path explicitly so policy stays out of
// the low-level subprocess driver.
type RateLimitDecision int

const (
	// RLDecisionAllow — primary status is "allowed" and we are not yet
	// dipping into overage. Safe to send.
	RLDecisionAllow RateLimitDecision = iota

	// RLDecisionWarn — currently using overage budget OR overage status is
	// not "allowed". Still possible to send, but the user should be told.
	RLDecisionWarn

	// RLDecisionRefuse — primary status is non-"allowed" (e.g. "exceeded")
	// or both primary and overage are blocked. Sending will likely produce
	// an error response from cc; degrade by refusing locally instead and
	// surfacing a clean reason to the user.
	RLDecisionRefuse
)

func (d RateLimitDecision) String() string {
	switch d {
	case RLDecisionAllow:
		return "allow"
	case RLDecisionWarn:
		return "warn"
	case RLDecisionRefuse:
		return "refuse"
	default:
		return "unknown"
	}
}

// EvaluateRateLimit applies the default pre-flight policy to a RateLimitInfo:
//
//   - RLDecisionRefuse if the primary `status` is set and != "allowed"
//   - RLDecisionRefuse if `overageStatus` is set and != "allowed"
//     (we have nothing left in either tier)
//   - RLDecisionWarn   if `isUsingOverage` is true (primary already done;
//     we're consuming the buffer)
//   - RLDecisionAllow  otherwise (status fields empty = unknown = optimistic
//     allow; cc not having sent any rate_limit_event yet should not block
//     all sends)
//
// Pure function over a snapshot — no Session state. Stable for table-driven
// tests and easy for callers to invoke without holding any lock.
func EvaluateRateLimit(info RateLimitInfo) RateLimitDecision {
	if info.Status != "" && info.Status != "allowed" {
		return RLDecisionRefuse
	}
	if info.OverageStatus != "" && info.OverageStatus != "allowed" {
		return RLDecisionRefuse
	}
	if info.IsUsingOverage {
		return RLDecisionWarn
	}
	return RLDecisionAllow
}
