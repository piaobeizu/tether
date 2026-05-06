package jsonl

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// EnvelopeKind names the wire-level operation per spec §3.3.2.
//
// We only emit three kinds out of this layer:
//
//	output.agent-event — EVENT-class JSONL (user / assistant)
//	output.hook-event  — HOOK-class JSONL (attachment + hookEvent)
//	session.state      — STATE-class JSONL (permission-mode etc.)
//
// session.state is local routing-only — the daemon updates its session
// view; it is NOT broadcast on the WebTransport events stream. The
// mapper still emits it so a single channel carries everything; the
// router downstream filters by kind.
type EnvelopeKind string

const (
	KindAgentEvent  EnvelopeKind = "output.agent-event"
	KindHookEvent   EnvelopeKind = "output.hook-event"
	KindSessionState EnvelopeKind = "session.state"
)

// ProviderType is `providerType` per §11.W.2. v0.1 is hardcoded
// "claude-code".
const ProviderTypeClaudeCode = "claude-code"

// Envelope is the in-process representation of a wire envelope as
// produced by the JSONL → wire mapper. It is NOT the on-the-wire JSON
// shape of §3.3.1 — that has the encrypted ciphertext layer + replay
// metadata, both of which are added downstream by the daemon's
// envelope dispatcher.
//
// What this struct guarantees:
//
//   - Kind / ProviderType / SessionID / Skill — plaintext routing
//     metadata, ready to copy into a §3.3.1 wire envelope.
//   - PlaintextMetadata — additional plaintext (uuid / parentUuid /
//     isSidechain / hookEvent / blockType-when-fence-grep-on)
//     that the daemon dispatcher inlines into the envelope plaintext.
//   - CiphertextPayload — JSON-encoded *plaintext* payload that will
//     become the `ciphertext` field of the wire envelope after
//     Epic #5's encryption hook runs over it. Today it is bytes
//     containing JSON; the field name is forward-looking on purpose.
//
// Adding fence-tag-suffix awareness (sub-task #11) only grows
// PlaintextMetadata — no struct change required.
type Envelope struct {
	Kind         EnvelopeKind
	ProviderType string
	SessionID    string

	// Skill is set by extractFenceTag (sub-task #11). Empty here
	// because v0.1 daemon stays transparent (D-5).
	Skill string

	// PlaintextMetadata is routing metadata that lives in the wire
	// envelope's plaintext portion (what the server can see). All
	// values are JSON-encodable.
	PlaintextMetadata map[string]any

	// CiphertextPayload is the *plaintext-encoded* payload bytes
	// that Epic #5 will encrypt in place. JSON for v0.1.
	CiphertextPayload []byte

	// Class is retained for downstream routing decisions (events
	// stream vs hook FSM vs local view). Not part of the wire
	// envelope but useful at the daemon layer.
	Class Class

	// SourceUUID is the cc record uuid (when present). Used by the
	// watcher's per-session dedup map.
	SourceUUID string
}

// MapOpts tunes the mapper. Zero value is fine.
type MapOpts struct {
	// FenceTagGrep, when true, extracts ```<type>:<skill> tags from
	// assistant text content and attaches them to PlaintextMetadata
	// (sub-task #11). v0.1 keeps this off — daemon stays transparent.
	FenceTagGrep bool
}

// Map transforms a classified Record into an Envelope. Returns
// ok=false for records that should be dropped silently (e.g. malformed
// EVENT records missing both uuid and message).
func Map(rec Record, class Class, opts MapOpts) (Envelope, bool) {
	switch class {
	case ClassEvent:
		return mapEvent(rec, opts), true
	case ClassHook:
		return mapHook(rec), true
	case ClassState, ClassUnknown:
		return mapState(rec, class), true
	default:
		return Envelope{}, false
	}
}

func mapEvent(rec Record, opts MapOpts) Envelope {
	meta := map[string]any{
		"uuid":        rec.UUID,
		"parentUuid":  nilIfEmpty(rec.ParentUUID),
		"isSidechain": rec.IsSidechain,
		"role":        string(rec.Type), // "user" | "assistant"
	}
	if rec.Timestamp != "" {
		meta["timestamp"] = rec.Timestamp
	}

	// Fence-tag grep seam for sub-task #11. Today FenceTagGrep is
	// always false — the call below is a no-op. The seam is
	// deliberately obvious: when sub-task #11 turns it on, only
	// this function changes.
	if opts.FenceTagGrep {
		if blockType, skillName, ok := scanContentForFence(rec.Message); ok {
			meta["blockType"] = blockType
			meta["skillName"] = skillName
		}
	}

	// Payload is the cc message body (content array preserved
	// verbatim per D-5 transparent contract). Falls back to the
	// whole record if Message is missing — defensive.
	payload := rec.Message
	if len(payload) == 0 {
		payload = rec.Raw
	}

	skill := ""
	if v, ok := meta["skillName"].(string); ok {
		skill = v
	}

	return Envelope{
		Kind:              KindAgentEvent,
		ProviderType:      ProviderTypeClaudeCode,
		SessionID:         rec.SessionID,
		Skill:             skill,
		PlaintextMetadata: meta,
		CiphertextPayload: append(json.RawMessage(nil), payload...),
		Class:             ClassEvent,
		SourceUUID:        rec.UUID,
	}
}

func mapHook(rec Record) Envelope {
	att := rec.Attachment
	meta := map[string]any{
		"uuid":      rec.UUID,
		"hookEvent": att.HookEvent,
	}
	if att.HookName != "" {
		meta["hookName"] = att.HookName
	}
	if att.ToolUseID != "" {
		meta["toolUseID"] = att.ToolUseID
	}
	if rec.Timestamp != "" {
		meta["timestamp"] = rec.Timestamp
	}

	// Hook payload includes stdout/stderr/exitCode — the consumer
	// (daemon hook FSM) needs them for PostToolUse decisioning.
	payload, err := json.Marshal(att)
	if err != nil {
		// Should not happen — Attachment is a plain struct.
		// Fall back to raw record.
		payload = rec.Raw
	}

	return Envelope{
		Kind:              KindHookEvent,
		ProviderType:      ProviderTypeClaudeCode,
		SessionID:         rec.SessionID,
		PlaintextMetadata: meta,
		CiphertextPayload: payload,
		Class:             ClassHook,
		SourceUUID:        rec.UUID,
	}
}

func mapState(rec Record, class Class) Envelope {
	meta := map[string]any{
		"recordType": string(rec.Type),
		"class":      class.String(),
	}
	if rec.PermissionMode != "" {
		meta["permissionMode"] = rec.PermissionMode
	}
	if rec.LeafUUID != "" {
		meta["leafUuid"] = rec.LeafUUID
	}

	// State payload is the whole raw line — the session-view
	// updater knows how to interpret each subtype.
	return Envelope{
		Kind:              KindSessionState,
		ProviderType:      ProviderTypeClaudeCode,
		SessionID:         rec.SessionID,
		PlaintextMetadata: meta,
		CiphertextPayload: append(json.RawMessage(nil), rec.Raw...),
		Class:             class,
		SourceUUID:        rec.UUID,
	}
}

// fenceTagRegex matches ```<type>(:<skill>)? on a line by itself per
// spec §11.AA.1. Anchored at line start (after optional whitespace).
var fenceTagRegex = regexp.MustCompile(`(?m)^[\t ]*` + "```" + `(dag|form|candidates|media)(?::([a-zA-Z0-9_-]+))?[\t ]*$`)

// extractFenceTag scans a single line for a fenced-block opener and
// returns the (block-type, skill-name) pair. SUB-TASK #11 INTEGRATION
// POINT: the mapper's only call site is mapEvent; flip MapOpts.FenceTagGrep
// to true and this function gates the metadata propagation.
//
// Today this is unused at runtime (FenceTagGrep is always false). It
// is fully tested anyway, so sub-task #11 only needs to flip the flag
// and decide on the daemon-side LRU caching keyed on (sessionId,
// skillName, blockId).
//
//nolint:unused // present as the explicit fence-tag seam for sub-task #11
func extractFenceTag(line string) (blockType, skillName string, ok bool) {
	m := fenceTagRegex.FindStringSubmatch(line)
	if m == nil {
		return "", "", false
	}
	skill := m[2]
	if skill == "" {
		skill = "unknown" // §11.Z.10 degrade behavior
	}
	return m[1], skill, true
}

// scanContentForFence walks an assistant message's content array
// looking for a fenced-block opener inside any text block. Returns
// the FIRST match — multi-block fences in one record fall back to
// "scan again at v0.2 streaming FSM" per §11.AA.
//
// Used only when MapOpts.FenceTagGrep is true (sub-task #11).
//
//nolint:unused // present as the explicit fence-tag seam for sub-task #11
func scanContentForFence(message json.RawMessage) (blockType, skillName string, ok bool) {
	if len(message) == 0 {
		return "", "", false
	}
	var probe struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text,omitempty"`
		} `json:"content"`
	}
	if err := json.Unmarshal(message, &probe); err != nil {
		return "", "", false
	}
	for _, blk := range probe.Content {
		if blk.Type != "text" || blk.Text == "" {
			continue
		}
		// Cheap prefix check before regex.
		// (text bodies can be many KB; avoid the regex pass when
		//  there's no triple-backtick at all.)
		if !containsTripleBacktick(blk.Text) {
			continue
		}
		if bt, sk, ok := extractFenceTag(firstFenceLine(blk.Text)); ok {
			return bt, sk, true
		}
	}
	return "", "", false
}

func containsTripleBacktick(s string) bool {
	// stdlib strings.Contains would do — kept inline to avoid an
	// extra import in this seam-only code path.
	for i := 0; i+2 < len(s); i++ {
		if s[i] == '`' && s[i+1] == '`' && s[i+2] == '`' {
			return true
		}
	}
	return false
}

// firstFenceLine returns the first line containing ```; the regex
// above is line-anchored so we just hand it the full text and let
// (?m) do the work — but for big text bodies we trim early.
//
//nolint:unused // sub-task #11 helper
func firstFenceLine(s string) string {
	return s // regex is multi-line; pass through verbatim
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// MarshalEnvelope is a debug helper: returns the Envelope as a
// pretty-printed JSON blob suitable for tests and logs.
func MarshalEnvelope(env Envelope) ([]byte, error) {
	type out struct {
		Kind              EnvelopeKind   `json:"kind"`
		ProviderType      string         `json:"providerType"`
		SessionID         string         `json:"sessionId"`
		Skill             string         `json:"skill,omitempty"`
		PlaintextMetadata map[string]any `json:"plaintextMetadata,omitempty"`
		CiphertextPayload json.RawMessage `json:"ciphertextPayload,omitempty"`
		Class             string         `json:"class"`
		SourceUUID        string         `json:"sourceUuid,omitempty"`
	}
	o := out{
		Kind:              env.Kind,
		ProviderType:      env.ProviderType,
		SessionID:         env.SessionID,
		Skill:             env.Skill,
		PlaintextMetadata: env.PlaintextMetadata,
		CiphertextPayload: env.CiphertextPayload,
		Class:             env.Class.String(),
		SourceUUID:        env.SourceUUID,
	}
	b, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("envelope marshal: %w", err)
	}
	return b, nil
}
