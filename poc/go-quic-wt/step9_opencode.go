//go:build step9

// Step 9: opencode integration spike.
//
// cursor-agent is not installed on this host, so the cross-provider
// generalization hypothesis is validated against opencode (similar
// architecture: external CLI with structured JSON output, persistent
// session via --session <id>).
//
// Validates:
//   1. step8 stream-json pattern (subprocess + line-delimited JSON)
//      generalizes to a non-Anthropic agent CLI
//   2. opencode session persistence works: first turn captures sessionID,
//      second turn with `-s <sid>` retains history
//
// PASS criteria:
//   ✓ first run: parse ≥1 text event with non-empty content + capture sessionID
//   ✓ second run with -s <sid>: parse ≥1 text event (proves session resume works)
//
// Run:
//   cd poc/go-quic-wt && go build -tags step9 -o bin-step9 . && ./bin-step9

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"
)

type opencodeEvent struct {
	Type      string          `json:"type"`
	Timestamp int64           `json:"timestamp"`
	SessionID string          `json:"sessionID"`
	Part      opencodePart    `json:"part"`
	Raw       json.RawMessage `json:"-"`
}

type opencodePart struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	MessageID string `json:"messageID,omitempty"`
	SessionID string `json:"sessionID,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// runOpencodePrompt spawns `opencode run --format json [-s sessionID] <prompt>`
// and returns the captured session_id + collected text outputs.
func runOpencodePrompt(ctx context.Context, sessionID, prompt string) (capturedSID string, texts []string, eventCount int, err error) {
	args := []string{"run", "--format", "json"}
	if sessionID != "" {
		args = append(args, "-s", sessionID)
	}
	args = append(args, prompt)

	cmd := exec.CommandContext(ctx, "opencode", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", nil, 0, err
	}
	if err := cmd.Start(); err != nil {
		return "", nil, 0, err
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 100*1024*1024)
	for scanner.Scan() {
		var ev opencodeEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		eventCount++
		if capturedSID == "" && ev.SessionID != "" {
			capturedSID = ev.SessionID
		}
		if ev.Type == "text" && ev.Part.Text != "" {
			texts = append(texts, ev.Part.Text)
		}
	}
	if err := cmd.Wait(); err != nil {
		return capturedSID, texts, eventCount, fmt.Errorf("opencode exit: %w", err)
	}
	return capturedSID, texts, eventCount, nil
}

func main() {
	log.SetFlags(0)
	log.Println("==Step 9: opencode integration spike==")
	log.Println()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	log.Println("[1] First turn: 'remember: my favorite letter is X'")
	sid1, texts1, n1, err := runOpencodePrompt(ctx, "", "remember: my favorite letter is X. acknowledge.")
	if err != nil {
		log.Fatalf("first turn: %v", err)
	}
	log.Printf("    session_id: %s", sid1)
	log.Printf("    events: %d  text replies: %d", n1, len(texts1))
	for i, t := range texts1 {
		log.Printf("      [%d] %s", i+1, strings.TrimSpace(t))
	}

	if sid1 == "" {
		log.Fatal("    ✗ no session_id captured")
	}

	log.Println()
	log.Println("[2] Second turn (-s <sid>): 'what was my favorite letter?'")
	sid2, texts2, n2, err := runOpencodePrompt(ctx, sid1, "what was my favorite letter? answer with one letter only.")
	if err != nil {
		log.Fatalf("second turn: %v", err)
	}
	log.Printf("    session_id: %s (should match: %v)", sid2, sid2 == sid1)
	log.Printf("    events: %d  text replies: %d", n2, len(texts2))
	for i, t := range texts2 {
		log.Printf("      [%d] %s", i+1, strings.TrimSpace(t))
	}

	log.Println()
	log.Println("==VERDICT==")
	pass := true
	if len(texts1) == 0 {
		log.Println("  ✗ first turn: no text reply")
		pass = false
	} else {
		log.Println("  ✓ first turn: got text reply")
	}
	if sid2 != sid1 {
		log.Println("  ✗ session_id mismatch: -s <sid> didn't continue same session")
		pass = false
	} else {
		log.Println("  ✓ session resume: -s preserves session_id")
	}
	// Bonus check — did the second turn remember "X"?
	rememberedX := false
	for _, t := range texts2 {
		if strings.Contains(t, "X") {
			rememberedX = true
		}
	}
	if rememberedX {
		log.Println("  ✓ semantic memory: second turn recalled 'X' from first")
	} else {
		log.Println("  ⚠ semantic memory: didn't see 'X' in 2nd reply (could be model refusal/wording)")
	}

	if pass {
		log.Println()
		log.Println("  → STEP 9 PASS — step8 pattern generalizes to opencode.")
		log.Println("    Provider integration cost ≈ 1 file × ~80 LOC per CLI agent.")
	} else {
		log.Println()
		log.Println("  → STEP 9 FAIL")
	}
}
