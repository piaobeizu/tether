// Package session — message history persistence.
//
// Each session's history is stored as JSONL in:
//
//	~/.tether/sessions/<sid>/history.jsonl
//
// Format: one JSON object per line, in stream order. A line is either a
// plain text turn or a completed fenced block (D-19, tether#8 T7) — never
// both:
//
//	{"role":"user","text":"...","ts":1234567890000}
//	{"role":"assistant","text":"...","ts":1234567890000}
//	{"role":"assistant","text":"","ts":1234567890000,"block":{"kind":"dag","skill":"s","content":"...","blockId":"s-0"}}
package session

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/piaobeizu/tether/internal/wire"
)

// ValidSessionID reports whether sid may be used as a path segment under
// ~/.tether/sessions — the one guard shared by everything in the daemon that
// turns a session id into a file path.
//
// It accepts only the alphabet real cc / opencode session ids use (UUID hex +
// dashes, or `ses_` / `t-` prefixes with [A-Za-z0-9]) and bounds the length.
// Anything else — `..`, slashes, control characters, URL-encoded escapes — is
// rejected, so no caller can escape the sessions directory.
//
// # Why it lives here and is exported
//
// A sid arrives from the client on two routes (`/wt/chat?sid=` and
// `/api/v1/sessions/<sid>/messages`) and is joined into a path by two types in
// this package (HistoryStore, BindingStore). This function was originally a
// private copy in internal/server for the HTTP route only; tether#52 needed the
// same check for BindingStore and briefly grew a SECOND, weaker `validSID` in
// this package — two same-named guards with different contracts in adjacent
// packages, which is how one of them ends up being the one that matters. It is a
// single definition here instead, and internal/server delegates to it.
//
// An allowlist rather than a `..`-blocklist: a blocklist stops traversal but
// still admits unbounded-length names and arbitrary control characters, which
// turn into ENAMETOOLONG and junk directories rather than into an escape — a
// weaker guarantee for no benefit, since every real id is already in this
// alphabet.
func ValidSessionID(sid string) bool {
	if len(sid) < 8 || len(sid) > 128 {
		return false
	}
	for _, c := range sid {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '-' || c == '_':
		default:
			return false
		}
	}
	return true
}

// MaxAssistantBufBytes caps the in-memory accumulator per session so that a
// single very long streaming response can't grow unbounded. When exceeded,
// the buffer is truncated with a marker and accumulation stops until the
// next FinalizeAssistant clears state. 4 MiB tracks Anthropic's max-tokens
// ceiling for a single response with headroom.
const MaxAssistantBufBytes = 4 << 20

// tether#44 — caps for the persisted rich-turn accumulators (thinking + tools).
const (
	MaxThinkingBufBytes = 4 << 20  // same ceiling as assistant text
	MaxToolsPerTurn     = 200      // cap tool calls recorded per turn
	MaxToolResultBytes  = 16 << 10 // cap each persisted tool result (UI truncates display anyway)
)

// HistoryMessage is one entry stored in the JSONL history file: either a
// plain text turn (Block nil) or a completed fenced block (D-19, tether#8
// T7) recorded in stream order alongside surrounding text, so a page
// reload can reconstruct DAG cards exactly as they rendered live.
type HistoryMessage struct {
	Role  string            `json:"role"` // "user" | "assistant"
	Text  string            `json:"text"`
	Ts    int64             `json:"ts"` // Unix milliseconds
	Block *wire.FencedBlock `json:"block,omitempty"`
	// tether#44 — rich turn content persisted so a reload reconstructs the turn
	// as it rendered live (previously live-only, lost on refresh). Both optional
	// (omitempty) for backward compatibility with pre-#44 history lines.
	Thinking string           `json:"thinking,omitempty"` // extended-thinking text (tether#34)
	Tools    []ToolCallRecord `json:"tools,omitempty"`    // tool calls + results (tether#37/#38)
}

// ToolCallRecord mirrors the frontend ToolCall shape (store.ts) so persisted
// tool activity reconstructs onto the same Message.tools the live path builds.
type ToolCallRecord struct {
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	Input  json.RawMessage   `json:"input,omitempty"`
	Result *ToolResultRecord `json:"result,omitempty"`
}

// ToolResultRecord is a tool's output, hung on its ToolCallRecord by id.
type ToolResultRecord struct {
	Content string `json:"content"`
	IsError bool   `json:"isError"`
}

// HistoryStore manages per-session message history files.
type HistoryStore struct {
	baseDir string                   // ~/.tether/sessions
	mu      sync.Mutex               // guards pending map
	pending map[string]*assistantBuf // accumulated assistant text per sid
}

type assistantBuf struct {
	text     string
	ts       int64
	overflow bool // true once text truncated; subsequent chunks are dropped
	// tether#44 — rich turn content accumulated alongside text and flushed
	// together by FinalizeAssistant.
	thinking         string
	thinkingOverflow bool
	tools            []ToolCallRecord
}

// NewHistoryStore creates a store rooted at baseDir.
func NewHistoryStore(baseDir string) *HistoryStore {
	return &HistoryStore{
		baseDir: baseDir,
		pending: make(map[string]*assistantBuf),
	}
}

// RecordUser appends a user message for the given session.
func (h *HistoryStore) RecordUser(sid, text string) {
	if sid == "" || text == "" {
		return
	}
	h.append(sid, HistoryMessage{
		Role: "user",
		Text: text,
		Ts:   time.Now().UnixMilli(),
	})
}

// AccumulateAssistant buffers an assistant text chunk (streaming). Capped at
// MaxAssistantBufBytes; once exceeded, a truncation marker is appended and
// subsequent chunks are dropped until FinalizeAssistant clears state.
func (h *HistoryStore) AccumulateAssistant(sid, chunk string) {
	if sid == "" || chunk == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	buf, ok := h.pending[sid]
	if !ok {
		buf = &assistantBuf{ts: time.Now().UnixMilli()}
		h.pending[sid] = buf
	}
	if buf.overflow {
		return
	}
	if len(buf.text)+len(chunk) > MaxAssistantBufBytes {
		remaining := MaxAssistantBufBytes - len(buf.text)
		if remaining > 0 {
			buf.text += chunk[:remaining]
		}
		buf.text += "\n\n[... response truncated at " +
			strconv.Itoa(MaxAssistantBufBytes) + " bytes ...]"
		buf.overflow = true
		slog.Warn("history: assistant response truncated",
			"sid", sid, "limit_bytes", MaxAssistantBufBytes)
		return
	}
	buf.text += chunk
}

// bufLocked returns the pending accumulator for sid, creating it if absent.
// Caller MUST hold h.mu.
func (h *HistoryStore) bufLocked(sid string) *assistantBuf {
	buf, ok := h.pending[sid]
	if !ok {
		buf = &assistantBuf{ts: time.Now().UnixMilli()}
		h.pending[sid] = buf
	}
	return buf
}

// AccumulateThinking buffers extended-thinking text for the current turn
// (tether#44), flushed to history by FinalizeAssistant. Capped like assistant
// text; once exceeded, further thinking is dropped until the turn finalizes.
func (h *HistoryStore) AccumulateThinking(sid, delta string) {
	if sid == "" || delta == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	buf := h.bufLocked(sid)
	if buf.thinkingOverflow {
		return
	}
	if len(buf.thinking)+len(delta) > MaxThinkingBufBytes {
		if remaining := MaxThinkingBufBytes - len(buf.thinking); remaining > 0 {
			buf.thinking += delta[:remaining]
		}
		buf.thinkingOverflow = true
		slog.Warn("history: thinking truncated", "sid", sid, "limit_bytes", MaxThinkingBufBytes)
		return
	}
	buf.thinking += delta
}

// RecordToolUse records a tool call for the current turn (tether#44), matched
// to its result later by id. Capped at MaxToolsPerTurn per turn.
func (h *HistoryStore) RecordToolUse(sid, id, name string, input json.RawMessage) {
	if sid == "" || name == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	buf := h.bufLocked(sid)
	if len(buf.tools) >= MaxToolsPerTurn {
		return
	}
	buf.tools = append(buf.tools, ToolCallRecord{ID: id, Name: name, Input: input})
}

// RecordToolResult hangs a tool's output on the matching recorded tool_use by
// id (tether#44), capping the stored content (the UI truncates display anyway).
// A result with no matching tool_use is dropped (no bubble to hang it on).
func (h *HistoryStore) RecordToolResult(sid, toolUseID, content string, isError bool) {
	if sid == "" || toolUseID == "" {
		return
	}
	if len(content) > MaxToolResultBytes {
		content = content[:MaxToolResultBytes] + "\n[... truncated ...]"
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	buf, ok := h.pending[sid]
	if !ok {
		return
	}
	for i := range buf.tools {
		if buf.tools[i].ID == toolUseID {
			buf.tools[i].Result = &ToolResultRecord{Content: content, IsError: isError}
			return
		}
	}
}

// FinalizeAssistant flushes accumulated assistant text to disk.
func (h *HistoryStore) FinalizeAssistant(sid string) {
	if sid == "" {
		return
	}
	h.mu.Lock()
	buf, ok := h.pending[sid]
	if ok {
		delete(h.pending, sid)
	}
	h.mu.Unlock()

	// tether#44 — flush if the turn has ANY content: text, thinking, or tools
	// (a thinking-only or tools-only turn must still persist).
	if !ok || (buf.text == "" && buf.thinking == "" && len(buf.tools) == 0) {
		return
	}
	h.append(sid, HistoryMessage{
		Role:     "assistant",
		Text:     buf.text,
		Ts:       buf.ts,
		Thinking: buf.thinking,
		Tools:    buf.tools,
	})
}

// AppendBlock appends a completed fenced block (D-19) to session history in
// stream order. Callers must finalize any pending assistant text first
// (FinalizeAssistant) so the JSONL order matches the live broadcast order —
// text-before-block, block, text-after-block (tether#8 T7). Registry.fanOut's
// emitSegments is the only caller and does this.
func (h *HistoryStore) AppendBlock(sid string, block wire.FencedBlock) {
	if sid == "" {
		return
	}
	h.append(sid, HistoryMessage{
		Role:  "assistant",
		Block: &block,
		Ts:    time.Now().UnixMilli(),
	})
}

// LoadHistory reads all messages for a session from disk. Returns an empty
// slice (not an error) if no history exists yet; "no history" is the
// common case for a fresh session and we don't want to noise the logs with
// ENOENT every read. All other I/O / parse failures are surfaced via slog
// so they're recoverable in incident review.
func (h *HistoryStore) LoadHistory(sid string) []HistoryMessage {
	if sid == "" {
		return nil
	}
	path := h.historyPath(sid)
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("history: load failed", "sid", sid, "err", err)
		}
		return nil
	}

	var msgs []HistoryMessage
	for i, line := range splitLines(data) {
		if len(line) == 0 {
			continue
		}
		var m HistoryMessage
		if err := json.Unmarshal(line, &m); err != nil {
			slog.Warn("history: skip corrupt line",
				"sid", sid, "line_index", i, "err", err)
			continue
		}
		msgs = append(msgs, m)
	}
	return msgs
}

// HasHistory reports whether sid has any conversation persisted on disk. It is
// the gate on tether#50's "started a new session, the old context is gone"
// notice: the notice is only honest — and only welcome — when there WAS a
// conversation to lose.
//
// The gate exists because a resume can fail for reasons that have nothing to do
// with the user losing a conversation. Measured on claude 2.1.220
// (mem_2ruSlrHR ⑦), a cc session with zero completed turns is NOT resumable at
// all: no transcript is written, so `--resume` fails exactly like an unknown id —
// minting an id is not the same as having something to come back to. Add the
// ordinary operational cases (cc pruned the transcript, the workdir moved, the
// file is corrupt) and "the resume failed" on its own says nothing about whether
// there was context worth mourning. tether's own history does.
//
// Note the pure zero-turn case cannot actually reach here through this daemon:
// the browser only learns a sid from session_ready, which is sent after cc
// consumed a prompt, so a session with no turns never hands the client a sid to
// reconnect with. The gate is defence-in-depth for that one, and load-bearing for
// the rest.
//
// Deliberately a size check rather than LoadHistory: this runs on the reconnect
// path, where parsing a long-running session's whole transcript just to learn
// "is it non-empty" would be wasted work. A zero-length file counts as no
// history, which is also what LoadHistory would conclude.
func (h *HistoryStore) HasHistory(sid string) bool {
	// ValidSessionID, not just a non-empty check: this is the one HistoryStore
	// entry point reached with a RAW client-supplied sid — Attachment.resolve asks
	// it about a.reqSID straight off `/wt/chat?sid=` — so without the guard a
	// `..`-shaped sid turns this into a stat oracle for any file named
	// history.jsonl outside the sessions directory. A rejected sid simply has no
	// history, which suppresses the notice and nothing else.
	if !ValidSessionID(sid) {
		return false
	}
	fi, err := os.Stat(h.historyPath(sid))
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("history: stat failed", "sid", sid, "err", err)
		}
		return false
	}
	return fi.Size() > 0
}

// (ListSessions used to live here, returning []string and backing
// GET /api/v1/sessions. tether#91 replaced it with SessionIndex.List
// (sessionlist.go), which answers the same question plus the two things a list a
// human reads has to carry — a label and a time. It was deleted rather than kept:
// an exported method returning a DIFFERENT shape for "which sessions exist", with
// a doc comment still claiming to back the route, is the second implementation
// that whole slice exists to prevent, and it had no callers left outside tests.
//
// The rule it enforced is not lost — see SessionIndex.List, which restates it:
// a session is listable when it has a NON-EMPTY transcript, never merely when its
// directory exists, because BindingStore and WIBindingStore both create <sid>/
// before any message does.)

func (h *HistoryStore) append(sid string, msg HistoryMessage) {
	path := h.historyPath(sid)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		slog.Warn("history: mkdir failed", "sid", sid, "path", path, "err", err)
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		slog.Warn("history: open failed", "sid", sid, "path", path, "err", err)
		return
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(msg); err != nil { // Encode adds newline
		slog.Warn("history: write failed", "sid", sid, "path", path, "err", err)
	}
}

func (h *HistoryStore) historyPath(sid string) string {
	return filepath.Join(h.baseDir, sid, "history.jsonl")
}

func splitLines(data []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			lines = append(lines, data[start:i])
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, data[start:])
	}
	return lines
}
