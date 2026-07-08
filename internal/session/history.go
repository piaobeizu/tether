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

// MaxAssistantBufBytes caps the in-memory accumulator per session so that a
// single very long streaming response can't grow unbounded. When exceeded,
// the buffer is truncated with a marker and accumulation stops until the
// next FinalizeAssistant clears state. 4 MiB tracks Anthropic's max-tokens
// ceiling for a single response with headroom.
const MaxAssistantBufBytes = 4 << 20

// HistoryMessage is one entry stored in the JSONL history file: either a
// plain text turn (Block nil) or a completed fenced block (D-19, tether#8
// T7) recorded in stream order alongside surrounding text, so a page
// reload can reconstruct DAG cards exactly as they rendered live.
type HistoryMessage struct {
	Role  string            `json:"role"` // "user" | "assistant"
	Text  string            `json:"text"`
	Ts    int64             `json:"ts"` // Unix milliseconds
	Block *wire.FencedBlock `json:"block,omitempty"`
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
	overflow bool // true once we've truncated; subsequent chunks are dropped
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

	if !ok || buf.text == "" {
		return
	}
	h.append(sid, HistoryMessage{
		Role: "assistant",
		Text: buf.text,
		Ts:   buf.ts,
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

// ListSessions returns all session IDs that have history on disk.
func (h *HistoryStore) ListSessions() []string {
	entries, err := os.ReadDir(h.baseDir)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("history: list sessions failed", "base_dir", h.baseDir, "err", err)
		}
		return nil
	}
	var sids []string
	for _, e := range entries {
		if e.IsDir() {
			sids = append(sids, e.Name())
		}
	}
	return sids
}

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
