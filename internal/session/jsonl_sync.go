package session

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// JSONLSync watches ~/.claude/projects/<ccSID>/<ccSID>.jsonl for catch-up
// replay after daemon restart (D-07). It is NOT a primary event source;
// use agent.Session.Events() for realtime delivery.
type JSONLSync struct {
	ccSID   string
	onEntry func(map[string]any)
}

// NewJSONLSync creates a sync watcher for the given ccSID.
// onEntry is called for each new jsonl line during catch-up or tail.
func NewJSONLSync(ccSID string, onEntry func(map[string]any)) *JSONLSync {
	return &JSONLSync{ccSID: ccSID, onEntry: onEntry}
}

// Start begins lazy watching of the jsonl file. Blocks until ctx is cancelled.
// Safe to call from a goroutine; exits when ctx is done.
func (s *JSONLSync) Start(ctx context.Context) {
	path := s.jsonlPath()
	if path == "" {
		return
	}

	// Initial catch-up: replay all existing lines.
	if err := s.replay(path); err != nil {
		slog.Debug("jsonl catch-up", "ccSID", s.ccSID, "err", err)
	}

	// Tail: poll for new lines (simple ticker-based approach for v0.1).
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	lastSize := int64(0)
	if fi, err := os.Stat(path); err == nil {
		lastSize = fi.Size()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fi, err := os.Stat(path)
			if err != nil || fi.Size() <= lastSize {
				continue
			}
			lastSize = fi.Size()
			_ = s.replay(path)
		}
	}
}

func (s *JSONLSync) replay(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 100<<20)
	for sc.Scan() {
		var entry map[string]any
		if err := json.Unmarshal(sc.Bytes(), &entry); err == nil && s.onEntry != nil {
			s.onEntry(entry)
		}
	}
	return sc.Err()
}

func (s *JSONLSync) jsonlPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	// cc stores session files at ~/.claude/projects/<ccSID>/<ccSID>.jsonl.
	return filepath.Join(home, ".claude", "projects", s.ccSID, s.ccSID+".jsonl")
}
