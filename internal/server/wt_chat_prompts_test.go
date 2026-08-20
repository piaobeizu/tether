package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// promptLine renders one browser→daemon prompt line EXACTLY as the composer
// writes it: web/src/panes/chat/index.tsx does
// `JSON.stringify({ text }) + '\n'` and imposes no size limit of its own, so a
// fixture built any other way would be testing a frame the browser never sends.
func promptLine(text string) string {
	b, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		panic(err)
	}
	return string(b) + "\n"
}

// promptProbe records everything readPrompts hands out. readPrompts is called
// synchronously by these tests (it returns when its reader is exhausted), so no
// locking is needed and none is hidden here — under -race a mutex could mask an
// ordering assumption the production goroutine does not actually have.
type promptProbe struct {
	delivered []string
	refused   []int
}

func (p *promptProbe) onPrompt(text string) { p.delivered = append(p.delivered, text) }
func (p *promptProbe) onOversize(size int)  { p.refused = append(p.refused, size) }
func (p *promptProbe) run(in string)        { readPrompts(strings.NewReader(in), p.onOversize, p.onPrompt) }
func (p *promptProbe) last() string         { return p.delivered[len(p.delivered)-1] }

// TestReadPrompts_APasteBiggerThanTheOldImplicitCapStillArrives is the reported
// tether#119 defect, in the size that provoked it.
//
// Before this fix the reader was `bufio.NewScanner(...)` with no
// scanner.Buffer(...) call anywhere in the file, so its per-line ceiling was
// bufio.MaxScanTokenSize — 64 KiB, chosen by the standard library and never by
// anyone here. Pasting a 70 KB stack trace made Scan() return false with
// bufio.ErrTooLong, and because that `for scanner.Scan()` loop WAS the whole
// body of the goroutine, returning false ended the goroutine. scanner.Err() was
// never consulted, so nothing anywhere learned why.
//
// The visible cost was not "one big prompt was rejected" — it was that
// serveChat's own loop below kept running, so the session stayed open and the
// tab still looked connected while every prompt typed afterwards went to a
// reader that no longer existed. Only a page reload recovered.
//
// So this test asserts the paste is DELIVERED (a 70 KB prompt is an ordinary
// thing to send, and cc's own reader accepts lines up to 100 MB — see
// internal/agent/claude_provider.go's scanBufMax), and that no refusal was
// reported for it. Shrinking maxPromptLine back to 64 KiB turns the delivery
// into a refusal and fails here.
func TestReadPrompts_APasteBiggerThanTheOldImplicitCapStillArrives(t *testing.T) {
	const size = 70 << 10 // past bufio.MaxScanTokenSize (64 KiB), well under maxPromptLine
	paste := strings.Repeat("x", size)

	var p promptProbe
	p.run(promptLine(paste) + promptLine("and the next one"))

	if len(p.refused) != 0 {
		t.Errorf("a %d-byte prompt was refused (%v); the cap must be a size someone chose, not bufio's default", size, p.refused)
	}
	if len(p.delivered) != 2 {
		t.Fatalf("delivered %d prompts, want 2 — the reader stopped on the big one", len(p.delivered))
	}
	if len(p.delivered[0]) != size {
		t.Errorf("first prompt is %d bytes, want %d", len(p.delivered[0]), size)
	}
	if p.delivered[1] != "and the next one" {
		t.Errorf("second prompt = %q, want the line that followed the paste", p.delivered[1])
	}
}

// TestReadPrompts_SurvivesALineOverTheCap pins the property the tether#119
// failure was actually made of: what the user loses to an over-cap line is that
// ONE line, not the rest of the connection.
//
// This is deliberately not "the big prompt was rejected". A reader that reports
// the refusal and then dies passes that weaker assertion while reproducing the
// entire bug — the tab stays open, looks healthy, and silently eats everything
// typed next. So the load-bearing line here is the one about the prompt that
// comes AFTER.
//
// It is also why bufio.Scanner cannot be the mechanism at any cap size:
// ErrTooLong is terminal for the scanner AND leaves the tail of the offending
// line unread, so there is no way back to a line boundary even if one wanted to
// continue. readPrompts discards to the next '\n' itself, which is what makes
// "skip this one, keep the connection" expressible.
func TestReadPrompts_SurvivesALineOverTheCap(t *testing.T) {
	over := promptLine(strings.Repeat("x", maxPromptLine)) // JSON framing pushes it past the cap
	wantSize := len(over) - 1                              // the reported size excludes the '\n'

	var p promptProbe
	p.run(over + promptLine("typed right after the rejected one"))

	if len(p.refused) != 1 {
		t.Fatalf("refusals = %v, want exactly one (the over-cap line)", p.refused)
	}
	if p.refused[0] != wantSize {
		t.Errorf("reported size = %d, want %d — the number in the error frame is what tells the user how much to cut", p.refused[0], wantSize)
	}
	if len(p.delivered) != 1 {
		t.Fatalf("delivered %d prompts, want 1 — an over-cap line must cost that line and nothing more", len(p.delivered))
	}
	if p.delivered[0] != "typed right after the rejected one" {
		t.Errorf("delivered %q; the prompt after the refused one is the one that used to vanish", p.delivered[0])
	}
}

// TestReadPrompts_HasNoBudgetForTheWholeConnection pins the second, independent
// silent death (tether#119 B2b).
//
// The old reader was `bufio.NewScanner(io.LimitReader(stream, 4<<20))`, and an
// io.LimitReader counts every byte it has EVER handed out — not the bytes of one
// line. So after 4 MiB of prompt text across a connection's whole life, Scan()
// saw EOF, the goroutine returned, and the tab went silently deaf in precisely
// the way the over-cap line did. Nothing about that depends on any single prompt
// being large, which is why raising the per-line cap does not touch it: 200
// perfectly ordinary 32 KB pastes get there on their own.
//
// A long-lived chat tab is the normal case for this product, so the ceiling this
// test walks past is one a real session reaches by being used.
func TestReadPrompts_HasNoBudgetForTheWholeConnection(t *testing.T) {
	const (
		count = 200
		each  = 32 << 10 // 200 × 32 KiB ≈ 6.4 MiB, comfortably past the old 4 MiB
	)
	var in strings.Builder
	for i := 0; i < count; i++ {
		in.WriteString(promptLine(fmt.Sprintf("%d:%s", i, strings.Repeat("y", each))))
	}
	if in.Len() <= 4<<20 {
		t.Fatalf("fixture is only %d bytes; it must exceed the old 4 MiB connection budget to mean anything", in.Len())
	}

	var p promptProbe
	p.run(in.String())

	if len(p.refused) != 0 {
		t.Errorf("refusals = %v; every one of these lines is under the per-line cap", p.refused)
	}
	if len(p.delivered) != count {
		t.Fatalf("delivered %d of %d prompts — the reader ran out of budget mid-connection", len(p.delivered), count)
	}
	if want := fmt.Sprintf("%d:", count-1); !strings.HasPrefix(p.last(), want) {
		t.Errorf("last prompt starts %q, want prefix %q", p.last()[:min(len(p.last()), 8)], want)
	}
}

// TestReadPrompts_KeepsTheSkipsItAlreadyHad guards the behaviour that was
// already correct before tether#119 and had to survive the rewrite of the
// reader underneath it. Each of these used to be handled by
// `if err := json.Unmarshal(...); err != nil || msg.Text == "" { continue }`
// over a bufio.Scanner; none of them may have become a reason to stop.
func TestReadPrompts_KeepsTheSkipsItAlreadyHad(t *testing.T) {
	t.Run("malformed and empty lines are skipped, not fatal", func(t *testing.T) {
		var p promptProbe
		p.run("not json at all\n" +
			"\n" +
			promptLine("") + // {"text":""} — a composer that submitted nothing
			`{"text":123}` + "\n" + // right key, wrong type
			promptLine("the real one"))

		if len(p.refused) != 0 {
			t.Errorf("refusals = %v, want none — none of these lines is over the cap", p.refused)
		}
		if len(p.delivered) != 1 || p.delivered[0] != "the real one" {
			t.Fatalf("delivered %q, want exactly [\"the real one\"]", p.delivered)
		}
	})

	t.Run("a final line with no newline is still delivered", func(t *testing.T) {
		// bufio.Scanner returned an unterminated trailing token, so the
		// replacement must too: dropping it would silently lose a prompt on any
		// stream that ends mid-frame.
		var p promptProbe
		p.run(strings.TrimSuffix(promptLine("last word"), "\n"))

		if len(p.delivered) != 1 || p.delivered[0] != "last word" {
			t.Fatalf("delivered %q, want [\"last word\"]", p.delivered)
		}
	})

	t.Run("an empty stream delivers nothing and does not spin", func(t *testing.T) {
		var p promptProbe
		p.run("")
		if len(p.delivered) != 0 || len(p.refused) != 0 {
			t.Fatalf("delivered %q, refused %v; want both empty", p.delivered, p.refused)
		}
	})
}
