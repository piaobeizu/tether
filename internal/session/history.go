// Package session — message history persistence.
//
// Each session's history is stored as JSONL in:
//   ~/.tether/sessions/<sid>/history.jsonl
//
// Format: one JSON object per line.
//   {"role":"user","text":"...","ts":1234567890000}
//   {"role":"assistant","text":"...","ts":1234567890000}
package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// HistoryMessage is one turn stored in the JSONL history file.
type HistoryMessage struct {
	Role string `json:"role"` // "user" | "assistant"
	Text string `json:"text"`
	Ts   int64  `json:"ts"` // Unix milliseconds
}

// HistoryStore manages per-session message history files.
type HistoryStore struct {
	baseDir string        // ~/.tether/sessions
	mu      sync.Mutex    // guards pending map
	pending map[string]*assistantBuf // accumulated assistant text per sid
}

type assistantBuf struct {
	text string
	ts   int64
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

// AccumulateAssistant buffers an assistant text chunk (streaming).
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

// LoadHistory reads all messages for a session from disk.
// Returns empty slice (not error) if no history exists yet.
func (h *HistoryStore) LoadHistory(sid string) []HistoryMessage {
	if sid == "" {
		return nil
	}
	path := h.historyPath(sid)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var msgs []HistoryMessage
	for _, line := range splitLines(data) {
		if len(line) == 0 {
			continue
		}
		var m HistoryMessage
		if err := json.Unmarshal(line, &m); err == nil {
			msgs = append(msgs, m)
		}
	}
	return msgs
}

// ListSessions returns all session IDs that have history on disk.
func (h *HistoryStore) ListSessions() []string {
	entries, err := os.ReadDir(h.baseDir)
	if err != nil {
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
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(msg) // Encode adds newline
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
