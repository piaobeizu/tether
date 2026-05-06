package jsonl

import (
	"encoding/json"
	"fmt"
)

// RecordType is the value of the "type" field on every cc JSONL line.
//
// The set is observed empirically against cc 2.1.120-2.1.123 session
// files in ~/.claude/projects/. cc may add new types in a patch bump;
// classifier.go falls through to ClassUnknown (treated as STATE) for
// any value not enumerated here.
type RecordType string

const (
	// EVENT class — have `uuid`, form parent/child chain, broadcast.
	RecordTypeUser      RecordType = "user"
	RecordTypeAssistant RecordType = "assistant"

	// HOOK class — `type=attachment` AND `attachment.hookEvent` non-empty.
	// (We classify on the attachment payload, not on the type alone.)
	RecordTypeAttachment RecordType = "attachment"

	// STATE class — pure session-state mutations.
	RecordTypePermissionMode      RecordType = "permission-mode"
	RecordTypeLastPrompt          RecordType = "last-prompt"
	RecordTypeFileHistorySnapshot RecordType = "file-history-snapshot"
	RecordTypeAITitle             RecordType = "ai-title"
	RecordTypeSystem              RecordType = "system"
)

// Record is the minimal common envelope decodable from any cc JSONL
// line. The full per-type payload is preserved in Raw for downstream
// consumers (the mapper re-decodes when it needs Content/HookEvent
// fields).
//
// Field names match cc's wire spelling (camelCase) — these are NOT
// negotiable. Adding a Go-idiomatic alias here would diverge from cc's
// JSON.
type Record struct {
	Type        RecordType `json:"type"`
	UUID        string     `json:"uuid,omitempty"`
	ParentUUID  string     `json:"parentUuid,omitempty"`
	SessionID   string     `json:"sessionId,omitempty"`
	IsSidechain bool       `json:"isSidechain,omitempty"`
	Timestamp   string     `json:"timestamp,omitempty"`
	UserType    string     `json:"userType,omitempty"`
	CWD         string     `json:"cwd,omitempty"`
	GitBranch   string     `json:"gitBranch,omitempty"`
	Version     string     `json:"version,omitempty"`

	// Attachment is non-nil only for type=attachment lines. Its
	// hookEvent field is what flips an attachment into HOOK class.
	Attachment *Attachment `json:"attachment,omitempty"`

	// Message is the cc-side message body. For type=user / type=assistant
	// it carries the role and content array (text blocks, tool_use blocks,
	// tool_result blocks). Left as RawMessage so the mapper decides when
	// to materialize it.
	Message json.RawMessage `json:"message,omitempty"`

	// PermissionMode is set on type=permission-mode lines.
	PermissionMode string `json:"permissionMode,omitempty"`

	// LeafUUID is set on type=last-prompt lines.
	LeafUUID string `json:"leafUuid,omitempty"`

	// Raw retains the original line bytes (defensive copy — fsnotify
	// scratch buffers reuse).  Always populated by ParseLine.
	Raw json.RawMessage `json:"-"`
}

// Attachment is the payload of a `type=attachment` JSONL record. The
// presence of HookEvent is what classifies the line as HOOK vs STATE.
type Attachment struct {
	Type      string          `json:"type,omitempty"`
	HookName  string          `json:"hookName,omitempty"`
	HookEvent string          `json:"hookEvent,omitempty"`
	ToolUseID string          `json:"toolUseID,omitempty"`
	Content   string          `json:"content,omitempty"`
	Stdout    string          `json:"stdout,omitempty"`
	Stderr    string          `json:"stderr,omitempty"`
	ExitCode  int             `json:"exitCode,omitempty"`
	Command   string          `json:"command,omitempty"`
	Extra     json.RawMessage `json:"-"` // reserved
}

// ParseLine decodes one JSONL line into a Record. The caller owns
// `line`; ParseLine takes a defensive copy into Record.Raw because
// callers commonly recycle scratch buffers.
//
// Unknown Type values do NOT error — the classifier handles them.
func ParseLine(line []byte) (Record, error) {
	var rec Record
	if err := json.Unmarshal(line, &rec); err != nil {
		return Record{Raw: append(json.RawMessage(nil), line...)},
			fmt.Errorf("jsonl: decode line: %w", err)
	}
	rec.Raw = append(json.RawMessage(nil), line...)
	return rec, nil
}
