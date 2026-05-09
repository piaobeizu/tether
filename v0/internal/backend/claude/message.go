package claude

import (
	"encoding/json"
	"fmt"
)

// EventType is the top-level "type" discriminator on every NDJSON line.
type EventType string

const (
	EventSystem          EventType = "system"
	EventStreamEvent     EventType = "stream_event"
	EventAssistant       EventType = "assistant"
	EventUser            EventType = "user"
	EventResult          EventType = "result"
	EventControlRequest  EventType = "control_request"
	EventControlResponse EventType = "control_response"
	EventRateLimit       EventType = "rate_limit_event"
)

// Envelope is the minimal common shape parseable from any NDJSON line.
//
// Per spec §6.A.2 the parser must handle unknown Type values gracefully: this
// type does NOT enumerate every known field — instead it captures Type +
// the most common discriminator fields, and keeps the original bytes in Raw
// so callers can re-decode into a richer specific type when needed.
type Envelope struct {
	Type      EventType `json:"type"`
	Subtype   string    `json:"subtype,omitempty"`
	SessionID string    `json:"session_id,omitempty"`
	UUID      string    `json:"uuid,omitempty"`

	// Raw is the original byte slice from the NDJSON line. Always populated
	// by ParseLine. Use it for re-decoding into a specific event type or for
	// passthrough logging of unknown types.
	Raw json.RawMessage `json:"-"`
}

// ParseLine decodes one NDJSON line into an Envelope. Unknown Type values do
// NOT error — the caller decides whether to skip / log / re-decode.
func ParseLine(line []byte) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(line, &env); err != nil {
		return env, fmt.Errorf("envelope decode: %w", err)
	}
	// Defensive copy — bufio.Scanner reuses its buffer between lines.
	env.Raw = append(json.RawMessage(nil), line...)
	return env, nil
}

// ----- specific decoders (called on demand by consumers) -----

// SystemInit is `type=system, subtype=init`. The first event of every
// session — its session_id is canonical and tether captures it here.
type SystemInit struct {
	Envelope
	Model             string   `json:"model,omitempty"`
	ClaudeCodeVersion string   `json:"claude_code_version,omitempty"`
	PermissionMode    string   `json:"permissionMode,omitempty"`
	Tools             []string `json:"tools,omitempty"`
	APIKeySource      string   `json:"apiKeySource,omitempty"`
}

// DecodeSystemInit re-parses the envelope as a SystemInit. Returns an error
// if Type/Subtype don't match.
func (e Envelope) DecodeSystemInit() (SystemInit, error) {
	if e.Type != EventSystem || e.Subtype != "init" {
		return SystemInit{}, fmt.Errorf("not a system/init event: type=%s subtype=%s", e.Type, e.Subtype)
	}
	var s SystemInit
	if err := json.Unmarshal(e.Raw, &s); err != nil {
		return s, fmt.Errorf("system/init decode: %w", err)
	}
	return s, nil
}

// RateLimitInfo is the `rate_limit_info` object inside a `type=rate_limit_event`
// envelope. Schema observed in cc SDK NDJSON stream (sample in
// testdata/stream1.ndjson).
//
// All fields are JSON-tagged with the cc SDK's exact wire names (camelCase),
// not Go-idiomatic names. resetsAt / overageResetsAt are unix epoch seconds.
type RateLimitInfo struct {
	// Status of the primary rate limit. Observed values: "allowed". Other
	// statuses (e.g. "exceeded") are inferred from semantics; treat any
	// non-"allowed" value as a refuse signal in EvaluateRateLimit.
	Status string `json:"status,omitempty"`

	// ResetsAt is when the primary limit resets (unix seconds).
	ResetsAt int64 `json:"resetsAt,omitempty"`

	// RateLimitType describes which tier this event refers to. Observed:
	// "overage". Distinguishes primary vs overage budget reports.
	RateLimitType string `json:"rateLimitType,omitempty"`

	// OverageStatus is the status of the overage tier. Observed: "allowed".
	OverageStatus string `json:"overageStatus,omitempty"`

	// OverageResetsAt is when the overage tier resets (unix seconds).
	OverageResetsAt int64 `json:"overageResetsAt,omitempty"`

	// IsUsingOverage is true when the user is currently consuming overage
	// budget (primary already exhausted for this window).
	IsUsingOverage bool `json:"isUsingOverage,omitempty"`
}

// RateLimit is `type=rate_limit_event`. Fired periodically by cc to report
// the user's current rate-limit status. The daemon caches the latest one
// on the Session and uses it for pre-flight degrade-gracefully decisions
// (ops-concerns subitem #4).
type RateLimit struct {
	Envelope
	RateLimitInfo RateLimitInfo `json:"rate_limit_info"`
}

// DecodeRateLimit re-parses the envelope as a RateLimit. Returns an error
// if Type doesn't match.
func (e Envelope) DecodeRateLimit() (RateLimit, error) {
	if e.Type != EventRateLimit {
		return RateLimit{}, fmt.Errorf("not a rate_limit_event: type=%s", e.Type)
	}
	var r RateLimit
	if err := json.Unmarshal(e.Raw, &r); err != nil {
		return r, fmt.Errorf("rate_limit_event decode: %w", err)
	}
	return r, nil
}

// Result is `type=result, subtype=success` (sometimes with is_error=true).
// Fired at end of every turn.
type Result struct {
	Envelope
	IsError        bool    `json:"is_error"`
	StopReason     string  `json:"stop_reason,omitempty"`
	TerminalReason string  `json:"terminal_reason,omitempty"`
	NumTurns       int     `json:"num_turns,omitempty"`
	TotalCostUSD   float64 `json:"total_cost_usd,omitempty"`
	Result         string  `json:"result,omitempty"`
}

func (e Envelope) DecodeResult() (Result, error) {
	if e.Type != EventResult {
		return Result{}, fmt.Errorf("not a result event: type=%s", e.Type)
	}
	var r Result
	if err := json.Unmarshal(e.Raw, &r); err != nil {
		return r, fmt.Errorf("result decode: %w", err)
	}
	return r, nil
}

// ControlRequest is the inbound (cc → tether) authorization or notification.
// The actual subtype + payload live inside the Request RawMessage so that
// per-subtype decoding is decoupled from envelope parsing.
type ControlRequest struct {
	Envelope
	RequestID string          `json:"request_id"`
	Request   json.RawMessage `json:"request"`
}

// Subtype peeks the nested "subtype" field of the inbound request.
// Used by the dispatcher to route can_use_tool vs unknown.
func (cr ControlRequest) RequestSubtype() (string, error) {
	var probe struct {
		Subtype string `json:"subtype"`
	}
	if err := json.Unmarshal(cr.Request, &probe); err != nil {
		return "", err
	}
	return probe.Subtype, nil
}

func (e Envelope) DecodeControlRequest() (ControlRequest, error) {
	if e.Type != EventControlRequest {
		return ControlRequest{}, fmt.Errorf("not a control_request: type=%s", e.Type)
	}
	var cr ControlRequest
	if err := json.Unmarshal(e.Raw, &cr); err != nil {
		return cr, err
	}
	return cr, nil
}

// CanUseToolPayload is the body of a control_request with subtype=can_use_tool.
type CanUseToolPayload struct {
	Subtype               string          `json:"subtype"`
	ToolName              string          `json:"tool_name"`
	Input                 json.RawMessage `json:"input,omitempty"`
	PermissionSuggestions json.RawMessage `json:"permission_suggestions,omitempty"`
}

// ControlResponse is bidirectional:
//   - inbound: cc replies to tether's outbound request (e.g. interrupt ack)
//   - outbound: tether replies to cc's request (e.g. can_use_tool allow)
type ControlResponse struct {
	Envelope
	Response ControlResponseBody `json:"response"`
}

type ControlResponseBody struct {
	Subtype   string          `json:"subtype"` // "success" | "error"
	RequestID string          `json:"request_id"`
	Error     string          `json:"error,omitempty"`
	Response  json.RawMessage `json:"response,omitempty"`
}

func (e Envelope) DecodeControlResponse() (ControlResponse, error) {
	if e.Type != EventControlResponse {
		return ControlResponse{}, fmt.Errorf("not a control_response: type=%s", e.Type)
	}
	var cr ControlResponse
	if err := json.Unmarshal(e.Raw, &cr); err != nil {
		return cr, err
	}
	return cr, nil
}

// ----- outbound (tether → cc) message envelopes -----

// UserMessage is sent on stdin to drive a new turn. Format mirrors what
// happy-cli writes; see ws-bridge.ts:623-630.
type UserMessage struct {
	Type            string          `json:"type"` // always "user"
	Message         UserMessageBody `json:"message"`
	ParentToolUseID *string         `json:"parent_tool_use_id"`
	SessionID       string          `json:"session_id"`
}

type UserMessageBody struct {
	Role    string `json:"role"`    // always "user"
	Content string `json:"content"` // plain text for v0.1
}

// NewUserMessage builds a fresh user envelope for a given session.
// SessionID may be empty on the very first turn — it gets populated after
// the first system/init arrives.
func NewUserMessage(sessionID, text string) UserMessage {
	return UserMessage{
		Type:      "user",
		Message:   UserMessageBody{Role: "user", Content: text},
		SessionID: sessionID,
	}
}

// OutboundControlRequest builds a control_request envelope to send on stdin.
// Used for outbound interrupt and (future) other tether → cc commands.
type OutboundControlRequest struct {
	Type      string          `json:"type"` // always "control_request"
	RequestID string          `json:"request_id"`
	Request   json.RawMessage `json:"request"`
}

// OutboundControlResponse is what tether writes back when answering an inbound
// can_use_tool (or future inbound subtypes).
type OutboundControlResponse struct {
	Type     string              `json:"type"` // always "control_response"
	Response ControlResponseBody `json:"response"`
}
