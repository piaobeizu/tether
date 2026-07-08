// Package session — D-19 fenced-block stream transformer (tether#8 T6).
//
// FenceParser extracts ```<kind>:<skill>[#<blockId>] fenced blocks from the
// assistant text stream (docs/wire/fenced-contract.md) and suppresses their
// raw text from the passthrough (chat) stream — EXCEPT when a fence body
// overruns maxHoldBytes, in which case the daemon gives up treating it as a
// fence and surfaces the buffered raw text as an ordinary Text segment (see
// the oversize-bail path in stepInFence); that text then does reach chat +
// history like any other passthrough. FenceParser is pure — no I/O, no
// dependency on the agent/session plumbing — so it is fully unit-testable
// in isolation (see fenceparser_test.go).
package session

import (
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	"github.com/piaobeizu/tether/internal/wire"
)

// maxHoldBytes caps how much unresolved raw input FenceParser may hold
// internally before it gives up trying to resolve it as a fence marker or
// fence body and bails out to ordinary passthrough text. Two distinct
// buffers are subject to this cap (D-19 contract requires bounded
// daemon-side buffering, regardless of whether the stream ever contains a
// '\n'):
//
//   - pending: outside a fence, the accumulated prefix of the
//     line-in-progress while it is still a *possible* fence-OPEN marker.
//   - fenceBody (+ pending): inside a fence, the accumulated body since the
//     opening marker, including any partial (no-'\n'-yet) tail line.
//
// A single minified/no-newline chunk that never resolves (never sees a
// deciding '\n', and never closes a fence) must not grow either buffer past
// this bound — that was the root of the pre-redesign unbounded-buffer bug,
// which only capped the per-*line* fenceBody (i.e. only checked after a
// '\n' arrived), so a single un-terminated line could hold arbitrary bytes
// forever.
const maxHoldBytes = 64 << 10 // 64 KiB

// openFenceRE matches a fence-open line: ```<kind>:<skill>[#<blockId>].
// <kind> is restricted to the 5 known wire.FencedBlockKind values (contract
// §1); <skill> and <blockId> are free-form but may not contain whitespace,
// '#' (which introduces blockId) or a backtick. The line is trimmed of
// trailing horizontal whitespace (trimMarkerLine) before this regex runs.
var openFenceRE = regexp.MustCompile(
	"^```(dag|form|candidates|media|permission):([^\\s#`]+)(?:#(\\S+))?$",
)

// Segment is one ordered unit of Feed/Flush output: either a non-empty Text
// span or a completed Block, never both. Callers (fanOut) must consume the
// returned slice in order to preserve stream ordering (text-before-block,
// block, text-after-block) — see D-19 fix #3 (intra-Feed reordering).
type Segment struct {
	Text  string            // non-empty when this is a text segment
	Block *wire.FencedBlock // non-nil when this is a completed block
}

// FenceParser is a stateful, per-session transformer. Text arrives in
// arbitrary fragments (stream_event content_block_delta chunks); a fence
// marker or its body may be split across multiple Feed calls.
//
// Streaming discipline (D-19 fix #2): ordinary text is emitted as soon as it
// is known NOT to be part of a fence marker — it does NOT wait for a '\n'.
// The only text ever held back is the ambiguous prefix of a line that is
// still a *possible* fence-open marker (0-3 leading backticks, or a line
// that starts with the full "```" and whose newline hasn't arrived yet — the
// full open-regex can only be evaluated once the whole line is available).
// Once a line is disproved (or exceeds maxHoldBytes with no resolution) it
// streams out immediately, character-for-character in effect (implemented
// as a bulk copy once disproved, since there's nothing left to decide).
//
// Not safe for concurrent use. Callers must serialize Feed/Flush per
// session — the registry's fanOut goroutine already does this (one
// goroutine per session's event loop), so one FenceParser lives on each
// session's *Entry.
type FenceParser struct {
	// pending holds unresolved raw bytes for whichever line is currently
	// in progress. Outside a fence it is the ambiguous marker-prefix
	// candidate; inside a fence it is the partial (no '\n' yet) tail of
	// the line being accumulated into fenceBody. Capped by maxHoldBytes.
	pending []byte

	// lineDisproved is true (outside a fence only) once the current
	// physical line has already been proven not to be a possible marker;
	// further bytes up to the next '\n' stream immediately without being
	// held in pending. Reset to false at each '\n'.
	lineDisproved bool

	inFence    bool
	openLine   string // raw opening marker line (verbatim); kept in case we bail (oversize)
	fenceKind  wire.FencedBlockKind
	fenceSkill string
	fenceID    string // explicit #blockId from the opening marker, if any
	fenceBody  strings.Builder

	// skillSeq assigns auto BlockIDs ("<skill>-<n>") when the opening marker
	// omits #<blockId>. n is the 0-based index of that skill's *auto-id'd*
	// blocks within the current turn; reset by Flush and ResetTurn.
	// Explicitly-id'd blocks do not consume a slot, so mixing explicit and
	// omitted ids for the same skill in one turn is well-defined but the
	// auto sequence only counts the omitted ones.
	//
	// Cross-turn collision by design: because skillSeq resets every turn,
	// a skill that auto-emits blocks in turn 1 (getting "<skill>-0",
	// "<skill>-1", ...) and again in turn 2 gets the SAME BlockID strings
	// back. The frontend keys live-replace by BlockID (contract §3), so
	// turn 2's "<skill>-0" will visually replace turn 1's "<skill>-0" card
	// even though they are unrelated blocks. Auto ids are only guaranteed
	// unique *within* a turn; an emitter that wants a block to persist
	// across turns (or wants to animate progress across turns) MUST use an
	// explicit "#<blockId>" (contract §3).
	skillSeq map[string]int
}

// NewFenceParser creates a FenceParser with fresh per-turn state.
func NewFenceParser() *FenceParser {
	return &FenceParser{skillSeq: make(map[string]int)}
}

// isPossibleMarkerPrefix reports whether b could still be the start of a
// "```" fence-open line: either b is itself a prefix of "```" (0-3
// backticks), or b already starts with the full "```" and we're waiting on
// the rest of the line (and its terminating '\n') to run openFenceRE.
func isPossibleMarkerPrefix(b []byte) bool {
	if len(b) <= 3 {
		for _, c := range b {
			if c != '`' {
				return false
			}
		}
		return true
	}
	return b[0] == '`' && b[1] == '`' && b[2] == '`'
}

// trimMarkerLine trims trailing horizontal whitespace (and a lone trailing
// '\r' for CRLF tolerance) before matching a line against openFenceRE or the
// bare "```" close marker (D-19 fix #5): a marker with trailing spaces
// (e.g. "```dag:s " or a closing "``` ") must still be recognized. Only the
// matching *decision* uses the trimmed copy; buffered/emitted bytes stay
// verbatim.
func trimMarkerLine(s string) string {
	return strings.TrimRight(s, " \t\r")
}

// Feed consumes a chunk of assistant text and returns the ordered Segments
// (Text / Block) it produced, in stream order. It may return an empty slice
// if the entire chunk was held back pending more input (e.g. it ended
// mid-ambiguous-line, or entirely inside a still-open fence body).
func (p *FenceParser) Feed(text string) []Segment {
	var segs []Segment
	var textBuf []byte

	flushText := func() {
		if len(textBuf) > 0 {
			segs = append(segs, Segment{Text: string(textBuf)})
			textBuf = textBuf[:0]
		}
	}

	remaining := text
	for len(remaining) > 0 {
		if p.inFence {
			consumed, blk := p.stepInFence(remaining, &textBuf)
			remaining = remaining[consumed:]
			if blk != nil {
				flushText()
				segs = append(segs, Segment{Block: blk})
			}
			continue
		}
		consumed := p.stepOutsideFence(remaining, &textBuf)
		remaining = remaining[consumed:]
	}

	flushText()
	return segs
}

// stepOutsideFence processes text while not in a fence, writing resolved
// passthrough bytes into *textBuf. It returns how many bytes of text it
// consumed. If it discovers a valid fence-open marker mid-way through text,
// it flips p.inFence and returns immediately (with consumed = the index
// just past that marker line's '\n'), so Feed's loop re-enters in in-fence
// mode for the rest of text within the same call (preserving order).
func (p *FenceParser) stepOutsideFence(text string, textBuf *[]byte) (consumed int) {
	i := 0
	for i < len(text) {
		if p.lineDisproved {
			j := strings.IndexByte(text[i:], '\n')
			if j < 0 {
				*textBuf = append(*textBuf, text[i:]...)
				return len(text)
			}
			*textBuf = append(*textBuf, text[i:i+j+1]...)
			i += j + 1
			p.lineDisproved = false
			continue
		}

		c := text[i]
		if c == '\n' {
			line := trimMarkerLine(string(p.pending))
			if m := openFenceRE.FindStringSubmatch(line); m != nil {
				p.inFence = true
				p.openLine = string(p.pending)
				p.fenceKind = wire.FencedBlockKind(m[1])
				p.fenceSkill = m[2]
				p.fenceID = m[3]
				p.fenceBody.Reset()
				p.pending = p.pending[:0]
				return i + 1
			}
			*textBuf = append(*textBuf, p.pending...)
			*textBuf = append(*textBuf, '\n')
			p.pending = p.pending[:0]
			i++
			continue
		}

		p.pending = append(p.pending, c)
		if !isPossibleMarkerPrefix(p.pending) {
			*textBuf = append(*textBuf, p.pending...)
			p.pending = p.pending[:0]
			p.lineDisproved = true
			i++
			continue
		}
		i++
		if len(p.pending) > maxHoldBytes {
			// No '\n' in sight and the candidate marker line itself is
			// oversize: give up on it as ambiguous and stream it out.
			*textBuf = append(*textBuf, p.pending...)
			p.pending = p.pending[:0]
			p.lineDisproved = true
		}
	}
	return len(text)
}

// stepInFence processes text while inside a fence, looking for the closing
// marker line. Returns bytes consumed from text, and a non-nil blk if a
// block completed — at which point p.inFence is already false (reset inside
// completeBlock) and Feed's loop will reprocess any remaining bytes of
// text[consumed:] outside a fence. If the buffered body exceeds
// maxHoldBytes with no closing marker found, it bails: the opening marker
// line + everything buffered so far is written to *textBuf as ordinary text
// and p.inFence is reset to false (consumed = index reached so far; any
// remainder of text is reprocessed outside-fence by the caller).
func (p *FenceParser) stepInFence(text string, textBuf *[]byte) (consumed int, blk *wire.FencedBlock) {
	i := 0
	for i < len(text) {
		c := text[i]
		p.pending = append(p.pending, c)
		i++

		if c != '\n' {
			if p.fenceBody.Len()+len(p.pending) > maxHoldBytes {
				p.bailFenceAsText(textBuf)
				return i, nil
			}
			continue
		}

		line := p.pending[:len(p.pending)-1]
		closed := trimMarkerLine(string(line)) == "```"
		p.pending = p.pending[:0]
		if closed {
			b := p.completeBlock()
			return i, &b
		}
		p.fenceBody.Write(line)
		p.fenceBody.WriteByte('\n')
		if p.fenceBody.Len() > maxHoldBytes {
			p.bailFenceAsText(textBuf)
			return i, nil
		}
	}
	return i, nil
}

// bailFenceAsText surfaces the opening marker line plus everything
// buffered for the in-progress fence (fenceBody + any partial tail line in
// pending) as ordinary text and abandons fence-tracking. Called when the
// cap in maxHoldBytes is exceeded with no closing marker found — the
// contract's "daemon-side buffering must be bounded" requirement takes
// priority over suppressing an unterminated fence's raw text.
func (p *FenceParser) bailFenceAsText(textBuf *[]byte) {
	slog.Debug("fenceparser: buffered fence content exceeded cap, flushed as passthrough",
		"skill", p.fenceSkill, "kind", p.fenceKind, "cap_bytes", maxHoldBytes)
	*textBuf = append(*textBuf, p.openLine...)
	*textBuf = append(*textBuf, '\n')
	*textBuf = append(*textBuf, p.fenceBody.String()...)
	*textBuf = append(*textBuf, p.pending...)
	p.resetFenceState()
}

// Flush is called at turn-end (EventResult). It returns any buffered
// passthrough text — the trailing partial line that never saw a '\n' (or
// never resolved as a possible marker) — as a final Segment. An UNCLOSED
// fence at graceful turn-end is flushed as raw TEXT (opening marker + body),
// NOT discarded: a well-formed emitter always closes its fences, so this
// path is exceptional (malformed / truncated output), and surfacing the raw
// content is strictly better than silently losing it (an empty screen).
// (Contrast ResetTurn, which DISCARDS an open fence — that path is for an
// abandoned/interrupted turn, where showing stale half-output is wrong.)
// Per-skill auto-BlockID counters reset for the next turn.
func (p *FenceParser) Flush() []Segment {
	var segs []Segment
	if p.inFence {
		if trimMarkerLine(string(p.pending)) == "```" {
			// Closing ``` with NO trailing newline — the block is the last
			// thing in the turn, so the stream ends right after ```. stepInFence
			// only tests for a close on a '\n'-terminated line, so this marker
			// sat unexamined in pending. Recognize it at turn end and complete
			// the block normally (this is the common real-world case: an emitter
			// whose final output is the block, with no trailing newline).
			p.pending = p.pending[:0]
			b := p.completeBlock()
			segs = append(segs, Segment{Block: &b})
		} else {
			// Genuinely unclosed fence: flush the raw content as text rather
			// than discard it, so malformed/truncated output is never silently
			// lost (an empty screen). A well-formed emitter always closes.
			slog.Debug("fenceparser: unclosed fence at turn end, flushed as text",
				"skill", p.fenceSkill, "kind", p.fenceKind)
			var b strings.Builder
			b.WriteString(p.openLine)
			b.WriteByte('\n')
			b.WriteString(p.fenceBody.String())
			b.Write(p.pending)
			p.resetFenceState()
			if b.Len() > 0 {
				segs = append(segs, Segment{Text: b.String()})
			}
		}
	} else if len(p.pending) > 0 {
		segs = append(segs, Segment{Text: string(p.pending)})
	}
	p.pending = p.pending[:0]
	p.lineDisproved = false
	p.skillSeq = make(map[string]int)
	return segs
}

// ResetTurn defensively clears all per-turn state — in-progress fence
// (discarded, no Segment emitted), the ambiguous line-prefix hold, and the
// auto-BlockID sequence — without producing any output. Unlike Flush, this
// is NOT a graceful turn-end: it exists because EventResult is best-effort
// (it can be dropped, and an interrupted turn ends with no EventResult at
// all), so a stale open fence or held partial line from an abandoned turn
// must not leak into or swallow the next turn's text. Callers (fanOut)
// should invoke this on the next turn's start signal (agent.EventInit),
// not only on EventResult.
func (p *FenceParser) ResetTurn() {
	p.resetFenceState()
	p.pending = p.pending[:0]
	p.lineDisproved = false
	p.skillSeq = make(map[string]int)
}

// completeBlock finalizes the in-progress fence into a wire.FencedBlock and
// resets fence-tracking state. Must only be called while p.inFence is true.
func (p *FenceParser) completeBlock() wire.FencedBlock {
	blockID := p.fenceID
	if blockID == "" {
		n := p.skillSeq[p.fenceSkill]
		p.skillSeq[p.fenceSkill] = n + 1
		blockID = p.fenceSkill + "-" + strconv.Itoa(n)
	}
	// Verbatim JSON body between the fences: every accumulated line ends in
	// '\n' (including the last one, since the closing "```" is itself on
	// its own line); trim exactly that one trailing newline.
	content := strings.TrimSuffix(p.fenceBody.String(), "\n")
	blk := wire.FencedBlock{
		Kind:    p.fenceKind,
		Skill:   p.fenceSkill,
		Content: content,
		BlockID: blockID,
	}
	p.resetFenceState()
	return blk
}

func (p *FenceParser) resetFenceState() {
	p.inFence = false
	p.openLine = ""
	p.fenceKind = ""
	p.fenceSkill = ""
	p.fenceID = ""
	p.fenceBody.Reset()
}
