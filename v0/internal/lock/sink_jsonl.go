package lock

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// jsonlEvent is the on-disk shape of a TakeoverEvent. Decoupled from
// TakeoverEvent struct so the wire schema is stable even if the Go
// struct grows fields. RFC3339Nano timestamps so the lines are easy to
// `jq -r '.at'`-grep without losing nanosecond ordering.
//
// Schema (one event per line):
//
//	{
//	  "at":  "2026-05-06T01:23:45.678901234Z",
//	  "reason": "first-byte" | "stale-takeover" | "force-takeover" |
//	            "auto-release" | "release",
//	  "prev_holder": {"kind": "...", "device_id": "..."} | null,
//	  "new_holder":  {"kind": "...", "device_id": "..."} | null
//	}
//
// Zero ClientID serializes as JSON null (matches in-memory "no
// holder"). Schema is part of the §11.D audit-log contract — change
// only with a coordinated reader bump.
type jsonlEvent struct {
	At         string         `json:"at"`
	Reason     string         `json:"reason"`
	PrevHolder *jsonlClientID `json:"prev_holder"`
	NewHolder  *jsonlClientID `json:"new_holder"`
}

type jsonlClientID struct {
	Kind     string `json:"kind"`
	DeviceID string `json:"device_id"`
}

func toJSONLClient(c ClientID) *jsonlClientID {
	if c.Kind == "" && c.DeviceID == "" {
		return nil
	}
	return &jsonlClientID{Kind: c.Kind, DeviceID: c.DeviceID}
}

func fromJSONLClient(p *jsonlClientID) ClientID {
	if p == nil {
		return ClientID{}
	}
	return ClientID{Kind: p.Kind, DeviceID: p.DeviceID}
}

// EncodeTakeoverEvent renders ev as the canonical JSONL form (no
// trailing newline). Exposed primarily for tests / external readers
// that want to reproduce the on-disk shape without re-implementing
// the schema.
func EncodeTakeoverEvent(ev TakeoverEvent) ([]byte, error) {
	row := jsonlEvent{
		At:         ev.At.UTC().Format(time.RFC3339Nano),
		Reason:     string(ev.Reason),
		PrevHolder: toJSONLClient(ev.PrevHolder),
		NewHolder:  toJSONLClient(ev.NewHolder),
	}
	return json.Marshal(row)
}

// DecodeTakeoverEvent parses a single JSONL line back into a
// TakeoverEvent. The reverse of EncodeTakeoverEvent; used by tests
// and any future audit-log reader.
func DecodeTakeoverEvent(line []byte) (TakeoverEvent, error) {
	var row jsonlEvent
	if err := json.Unmarshal(line, &row); err != nil {
		return TakeoverEvent{}, err
	}
	at, err := time.Parse(time.RFC3339Nano, row.At)
	if err != nil {
		return TakeoverEvent{}, fmt.Errorf("lock: parse 'at': %w", err)
	}
	return TakeoverEvent{
		At:         at,
		Reason:     AcquireReason(row.Reason),
		PrevHolder: fromJSONLClient(row.PrevHolder),
		NewHolder:  fromJSONLClient(row.NewHolder),
	}, nil
}

// JSONLLogSink persists TakeoverEvents to an append-only JSONL file
// per spec §11.D ("audit log" row): one JSON object per line, file
// mode 0600, parent dirs 0700, fsync on every Append (small volume —
// safety > throughput). Append is mutex-serialized so concurrent
// goroutines won't tear each other's writes. Close releases the file
// handle and is idempotent.
//
// Retention/rotation: v0.1 writes forever — no size cap, no rolling.
// Operator concern (logrotate, manual prune) until spec §11.D adds a
// rotation row. Tracked as a follow-up in the §11.D table.
type JSONLLogSink struct {
	path string

	mu     sync.Mutex
	f      *os.File
	closed bool
}

// JSONLLogSinkConfig is reserved for future knobs (rotation, fsync
// strategy). v0.1 takes only the path, but the constructor accepts a
// config struct so growing it doesn't break callers.
type JSONLLogSinkConfig struct {
	// Path is the destination JSONL file. Parent dirs are created at
	// 0700 if missing. Required; empty path returns an error.
	Path string
}

// NewJSONLLogSink opens (or creates) a JSONL audit log at path. Parent
// directories are created at 0700 if missing; the file itself is
// O_APPEND|O_CREATE|O_WRONLY at 0600. Returns the live sink — caller
// is responsible for Close on shutdown.
//
// Path is checked for emptiness only; the caller is expected to have
// already resolved the §11.D layout
// `~/.tether/users/<user>/sessions/<sid>/lock.log`.
func NewJSONLLogSink(path string) (*JSONLLogSink, error) {
	return NewJSONLLogSinkWithConfig(JSONLLogSinkConfig{Path: path})
}

// NewJSONLLogSinkWithConfig is the explicit-config form.
func NewJSONLLogSinkWithConfig(cfg JSONLLogSinkConfig) (*JSONLLogSink, error) {
	if cfg.Path == "" {
		return nil, errors.New("lock: JSONLLogSink: empty path")
	}
	dir := filepath.Dir(cfg.Path)
	if dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("lock: mkdir %q: %w", dir, err)
		}
	}
	f, err := os.OpenFile(cfg.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lock: open audit log %q: %w", cfg.Path, err)
	}
	// Tighten mode in case the file pre-existed with looser perms.
	// Best-effort — a Chmod failure on Windows-style FS shouldn't
	// kill the daemon.
	_ = os.Chmod(cfg.Path, 0o600)
	return &JSONLLogSink{path: cfg.Path, f: f}, nil
}

// Path returns the file path the sink writes to. Stable for the
// lifetime of the sink.
func (s *JSONLLogSink) Path() string { return s.path }

// Append serializes ev as a single JSON line and fsyncs. Safe for
// concurrent callers — internal mutex serializes writes so lines
// never tear.
func (s *JSONLLogSink) Append(ev TakeoverEvent) error {
	row, err := EncodeTakeoverEvent(ev)
	if err != nil {
		return fmt.Errorf("lock: encode event: %w", err)
	}
	row = append(row, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.f == nil {
		return errors.New("lock: JSONLLogSink already closed")
	}
	// Single write call — kernel-level append is atomic for writes
	// smaller than PIPE_BUF on POSIX; our rows are well under that
	// (<512 bytes). The mutex above is belt-and-braces for hosts
	// with quirkier append semantics (NFS, Windows).
	if _, err := s.f.Write(row); err != nil {
		return fmt.Errorf("lock: append %q: %w", s.path, err)
	}
	if err := s.f.Sync(); err != nil {
		return fmt.Errorf("lock: fsync %q: %w", s.path, err)
	}
	return nil
}

// Close releases the underlying file handle. Idempotent — calling
// Close on an already-closed sink returns nil. Subsequent Append
// calls return an error (no silent data loss).
func (s *JSONLLogSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}
