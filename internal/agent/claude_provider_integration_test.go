//go:build integration

// Run with: go test -tags=integration -run TestClaudeStreaming_E2E ./internal/agent -v
// Requires `claude` CLI on PATH and a working ANTHROPIC_API_KEY / OAuth session.

package agent

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

func TestClaudeStreaming_E2E(t *testing.T) {
	ccPath, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude CLI not on PATH")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	p := NewClaudeCodeProvider(ccPath)
	sess, err := p.Spawn(ctx, SpawnConfig{})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer sess.Close()

	// Need ~300+ chars to reliably observe multiple deltas — short responses
	// often arrive in a single text_delta chunk.
	if err := sess.SendPrompt(ctx, "write 3 short paragraphs (~80 words each) describing different types of clouds. plain prose, no headings, no markdown"); err != nil {
		t.Fatalf("send: %v", err)
	}

	var (
		textChunks int
		assembled  string
		gotResult  bool
	)
	for ev := range sess.Events() {
		switch ev.Kind {
		case EventInit:
			t.Logf("INIT sid=%s", ev.SessionID)
		case EventText:
			textChunks++
			assembled += ev.Text
			t.Logf("TEXT chunk #%d (%d bytes): %q", textChunks, len(ev.Text), ev.Text)
		case EventResult:
			gotResult = true
			t.Logf("RESULT text=%q chunks=%d assembled=%q", ev.Text, textChunks, assembled)
		case EventError:
			t.Fatalf("EventError: %v", ev.Err)
		}
		if gotResult {
			break
		}
	}

	if !gotResult {
		t.Fatal("never received EventResult")
	}
	if textChunks < 2 {
		t.Fatalf("expected >=2 text chunks (token-level streaming), got %d", textChunks)
	}
	if assembled == "" {
		t.Fatal("assembled text is empty")
	}
}
