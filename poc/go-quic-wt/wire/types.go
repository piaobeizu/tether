// Package wire defines the shared schema between the Go server and the
// TypeScript browser frontend. tygo generates a 1:1 TS file from this
// package; both sides import the same interface shape.
//
// Format conventions that types alone CAN'T express (e.g., hash with no
// colons, echo without prefix) are documented per-type in godoc and
// validated by web/test/wire-contract.spec.ts in CI.
package wire

// HashHex64 is the canonical wire format for SHA-256 fingerprints:
// **64 lowercase hex characters, no separators**.
//
// Example:
//
//	"4c55656033cc29b05037294b2c1bdd2c294963b57b9238a2a758aec8d87eb288"
//
// NOT acceptable on the wire (verified by contract test):
//   - colon separated: "4c:55:65:..."
//   - uppercase: "4C55..."
//   - 0x prefix: "0x4c55..."
//
// Both /cert-hash and /cert-hash-spki return values of this format.
// Browser parser regex: /^[0-9a-f]{64}$/
type HashHex64 = string

// CertHashes is the response body of the (currently text-only) cert
// fingerprint endpoints. Reserved for future JSON migration.
type CertHashes struct {
	DER  HashHex64 `json:"der"`
	SPKI HashHex64 `json:"spki"`
}

// EchoTestPayload is the message shape used by the WT bidi-stream echo
// test in step3_browser/index.html.
//
// **Wire contract:** server returns the input bytes verbatim — no
// prefix, no transformation, no extra framing. Verified by browser
// test "bidi echo".
type EchoTestPayload struct {
	Text string `json:"text"`
}

// FencedBlockKind is the discriminator for D-19 fenced-block protocol.
// Reserved enumeration; v0.1 lists 4 kinds, more in v1.0+.
type FencedBlockKind string

const (
	FencedBlockDag        FencedBlockKind = "dag"
	FencedBlockForm       FencedBlockKind = "form"
	FencedBlockCandidates FencedBlockKind = "candidates"
	FencedBlockMedia      FencedBlockKind = "media"
)

// FencedBlock is the v0.1 1st-class structured-output unit (per D-19).
//
// Wire format (informational; serialized as JSON):
//
//	```<Kind>:<Skill>
//	{...content json...}
//	```
//
// daemon parses fence markers but never the content body (per D-20:
// daemon-not-aware-of-skill).
type FencedBlock struct {
	Kind    FencedBlockKind `json:"kind"`
	Skill   string          `json:"skill"`            // skill id that emitted this block
	Content string          `json:"content"`          // skill-defined JSON body
	BlockID string          `json:"blockId,omitempty"` // optional, for action callbacks
}
