package wire

// EnvelopeKind is the discriminator field on every wire envelope.
// The browser dispatches on this value to route to the correct handler.
// Additional kinds are added in s4+ as channels are implemented.
type EnvelopeKind string

const (
	KindMessage    EnvelopeKind = "message"    // assistant text / tool output (s4)
	KindPermission EnvelopeKind = "permission" // PreToolUse callback (s5)
	KindFenced     EnvelopeKind = "fenced"     // D-19 fenced-block structured output (s4)
	KindError      EnvelopeKind = "error"      // daemon-side error surfaced to UI
	KindResult     EnvelopeKind = "result"     // turn complete; payload is stop reason string
)

// Envelope is the top-level wrapper for all events sent over /wt/events
// and /wt/chat. The Payload field carries kind-specific JSON (D-05a §3,
// D-05b §3.1, D-19). Routing table per §10.B.4 is implemented in s4.
type Envelope struct {
	Kind      EnvelopeKind `json:"kind"`
	SessionID SessionID    `json:"sessionId,omitempty"`
	Payload   any          `json:"payload,omitempty"`
}

// FencedBlockKind is the discriminator for D-19 fenced-block protocol.
// v0.1 defines 5 kinds; more may be added in v1.0+.
type FencedBlockKind string

const (
	FencedBlockDag        FencedBlockKind = "dag"
	FencedBlockForm       FencedBlockKind = "form"
	FencedBlockCandidates FencedBlockKind = "candidates"
	FencedBlockMedia      FencedBlockKind = "media"
	FencedBlockPermission FencedBlockKind = "permission"
)

// FencedBlock is the v0.1 structured-output unit (D-19).
// Daemon parses fence markers but never the Content body (D-20:
// daemon-not-aware-of-skill). The browser dispatches on Kind.
//
// Wire serialization (informational):
//
//	```<Kind>:<Skill>
//	{...content json...}
//	```
type FencedBlock struct {
	Kind    FencedBlockKind `json:"kind"`
	Skill   string          `json:"skill"`             // skill id that emitted this block
	Content string          `json:"content"`           // skill-defined JSON body (opaque to daemon)
	BlockID string          `json:"blockId,omitempty"` // optional, for action callbacks
}

// ProviderListResponse is the response body for GET /api/v1/providers.
type ProviderListResponse struct {
	Providers []string `json:"providers"`
}

// ClientFrameKind is the discriminator for client→server frames sent on
// the /wt/control bidi stream.
type ClientFrameKind string

const (
	ClientFramePing   ClientFrameKind = "ping"
	ClientFrameAction ClientFrameKind = "action"
)

// ClientFrame is a client→server message on /wt/control. Kind selects the
// interpretation of the remaining fields: "ping" carries only TS (RTT
// probe); "action" carries SessionID/BlockID/Action/Skill — a fenced-block
// callback (D-19 §5) routed to the named session (tether#8 T8). The
// /wt/control channel is not otherwise session-scoped, so SessionID is the
// only way the daemon knows which session an action targets.
type ClientFrame struct {
	Kind      ClientFrameKind `json:"kind"`
	TS        int64           `json:"ts,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	BlockID   string          `json:"blockId,omitempty"`
	Action    string          `json:"action,omitempty"`
	Skill     string          `json:"skill,omitempty"`
}

// ControlFrame is a server→client message on /wt/control.
type ControlFrame struct {
	Kind string `json:"kind"`
	TS   int64  `json:"ts,omitempty"`
}

// ControlPong is the ControlFrame.Kind sent in reply to a ClientFramePing.
const ControlPong = "pong"
