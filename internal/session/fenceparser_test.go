package session

import (
	"strings"
	"testing"

	"github.com/piaobeizu/tether/internal/wire"
)

// texts collects the Text spans from a Segment slice, in order, concatenated.
func texts(segs []Segment) string {
	var sb strings.Builder
	for _, s := range segs {
		sb.WriteString(s.Text)
	}
	return sb.String()
}

// blocksOf collects the Block spans from a Segment slice, in order.
func blocksOf(segs []Segment) []wire.FencedBlock {
	var out []wire.FencedBlock
	for _, s := range segs {
		if s.Block != nil {
			out = append(out, *s.Block)
		}
	}
	return out
}

// TestFenceParser_SingleFeedCompleteBlock — a complete dag block delivered
// in one Feed call is extracted whole; no Text segment surrounds it.
func TestFenceParser_SingleFeedCompleteBlock(t *testing.T) {
	p := NewFenceParser()

	input := "```dag:myskill\n{\"nodes\":[]}\n```\n"
	segs := p.Feed(input)

	if pt := texts(segs); pt != "" {
		t.Errorf("text = %q, want empty", pt)
	}
	blocks := blocksOf(segs)
	if len(blocks) != 1 {
		t.Fatalf("len(blocks) = %d, want 1", len(blocks))
	}
	blk := blocks[0]
	if blk.Kind != wire.FencedBlockDag {
		t.Errorf("Kind = %q, want dag", blk.Kind)
	}
	if blk.Skill != "myskill" {
		t.Errorf("Skill = %q, want myskill", blk.Skill)
	}
	if blk.Content != `{"nodes":[]}` {
		t.Errorf("Content = %q, want verbatim JSON body", blk.Content)
	}
	if blk.BlockID != "myskill-0" {
		t.Errorf("BlockID = %q, want myskill-0", blk.BlockID)
	}
}

// TestFenceParser_SplitAcrossThreeFeeds — the same block, split arbitrarily
// across three Feed calls, still yields exactly one block.
func TestFenceParser_SplitAcrossThreeFeeds(t *testing.T) {
	p := NewFenceParser()

	var allText strings.Builder
	var allBlocks []wire.FencedBlock

	chunks := []string{
		"```dag:myskill\n",
		"{\"a\":1}\n",
		"```\n",
	}
	for _, c := range chunks {
		segs := p.Feed(c)
		allText.WriteString(texts(segs))
		allBlocks = append(allBlocks, blocksOf(segs)...)
	}

	if allText.String() != "" {
		t.Errorf("text = %q, want empty", allText.String())
	}
	if len(allBlocks) != 1 {
		t.Fatalf("len(blocks) = %d, want 1", len(allBlocks))
	}
	if allBlocks[0].Content != `{"a":1}` {
		t.Errorf("Content = %q, want {\"a\":1}", allBlocks[0].Content)
	}
}

// TestFenceParser_PartialOpeningMarkerAcrossFeeds — the opening marker
// itself is split mid-token ("```da" | "g:skill\n{}\n```\n"). It must be
// held back, not misfired as passthrough or a bogus marker, and resolve to
// exactly one block once the rest arrives.
func TestFenceParser_PartialOpeningMarkerAcrossFeeds(t *testing.T) {
	p := NewFenceParser()

	segs1 := p.Feed("```da")
	if len(segs1) != 0 {
		t.Errorf("first Feed segments = %v, want none (no decision point yet)", segs1)
	}

	segs2 := p.Feed("g:skill\n{}\n```\n")
	if pt := texts(segs2); pt != "" {
		t.Errorf("second Feed text = %q, want empty", pt)
	}
	blocks2 := blocksOf(segs2)
	if len(blocks2) != 1 {
		t.Fatalf("second Feed blocks = %d, want 1", len(blocks2))
	}
	if blocks2[0].Kind != wire.FencedBlockDag || blocks2[0].Skill != "skill" {
		t.Errorf("block = %+v, want dag/skill", blocks2[0])
	}
	if blocks2[0].Content != "{}" {
		t.Errorf("Content = %q, want {}", blocks2[0].Content)
	}
}

// TestFenceParser_NoFenceRoundtrips — plain text with no fence markers
// passes through unchanged. Per D-19 fix #2, neither line is a possible
// fence marker (neither starts with a backtick), so BOTH stream out of Feed
// immediately — the second line does NOT wait for Flush/a trailing '\n',
// unlike the pre-redesign line-buffered parser.
func TestFenceParser_NoFenceRoundtrips(t *testing.T) {
	p := NewFenceParser()

	input := "just some plain assistant text\nwith two lines"
	segs := p.Feed(input)
	if len(blocksOf(segs)) != 0 {
		t.Errorf("blocks = %v, want none", blocksOf(segs))
	}
	if pt := texts(segs); pt != input {
		t.Errorf("text = %q, want the whole input streamed immediately", pt)
	}

	// Nothing left to hold: Flush is a no-op.
	tailSegs := p.Flush()
	if len(tailSegs) != 0 {
		t.Errorf("Flush() = %+v, want empty (nothing held)", tailSegs)
	}
}

// TestFenceParser_TextBeforeAndAfterBlock — normal text surrounding a
// fenced block in the same stream: both text spans pass through IN ORDER as
// distinct Segments around the block (no merging), and the block is
// extracted separately (D-19 fix #3).
func TestFenceParser_TextBeforeAndAfterBlock(t *testing.T) {
	p := NewFenceParser()

	input := "before text\n```dag:s\n{\"x\":1}\n```\nafter text\n"
	segs := p.Feed(input)

	if len(segs) != 3 {
		t.Fatalf("len(segs) = %d, want 3 (text, block, text); got %+v", len(segs), segs)
	}
	if segs[0].Block != nil || segs[0].Text != "before text\n" {
		t.Errorf("segs[0] = %+v, want Text %q", segs[0], "before text\n")
	}
	if segs[1].Block == nil {
		t.Fatalf("segs[1] = %+v, want a Block", segs[1])
	}
	if segs[1].Block.Content != `{"x":1}` {
		t.Errorf("segs[1].Block.Content = %q, want {\"x\":1}", segs[1].Block.Content)
	}
	if segs[2].Block != nil || segs[2].Text != "after text\n" {
		t.Errorf("segs[2] = %+v, want Text %q", segs[2], "after text\n")
	}
}

// TestFenceParser_UnclosedFenceFlushedAsTextAtFlush — an opened-but-never-closed
// fence emits no block; at graceful turn-end (Flush) its raw content (opening
// marker + body) is flushed as TEXT rather than discarded, so malformed or
// truncated agent output is never silently lost (an empty screen). Text seen
// strictly BEFORE the fence opened was already emitted by the earlier Feed call.
func TestFenceParser_UnclosedFenceFlushedAsTextAtFlush(t *testing.T) {
	p := NewFenceParser()

	segs := p.Feed("before\n```dag:s\n{\"partial\":true\n")
	if pt := texts(segs); pt != "before\n" {
		t.Errorf("text = %q, want %q (pre-fence text emitted immediately)", pt, "before\n")
	}
	if len(blocksOf(segs)) != 0 {
		t.Errorf("blocks = %v, want none (fence still open)", blocksOf(segs))
	}

	tailSegs := p.Flush()
	if len(blocksOf(tailSegs)) != 0 {
		t.Errorf("Flush() blocks = %v, want none (fence never closed)", blocksOf(tailSegs))
	}
	got := texts(tailSegs)
	want := "```dag:s\n{\"partial\":true\n"
	if got != want {
		t.Errorf("Flush() text = %q, want %q (unclosed fence flushed as raw text)", got, want)
	}
}

// TestFenceParser_CloseWithoutTrailingNewline — the common real-world case: a
// block is the LAST thing in the turn, so the closing ``` has no trailing
// newline (the stream ends right after it). stepInFence only checks for a
// close on a '\n'-terminated line, so the marker sits in pending until Flush,
// which must recognize it and complete the block (NOT flush it as raw text).
func TestFenceParser_CloseWithoutTrailingNewline(t *testing.T) {
	p := NewFenceParser()

	segs := p.Feed("```dag:demo#d0\n{\"nodes\":[]}\n```")
	if len(blocksOf(segs)) != 0 {
		t.Fatalf("Feed emitted a block before Flush; want none (close still pending): %v", blocksOf(segs))
	}

	tail := p.Flush()
	blks := blocksOf(tail)
	if len(blks) != 1 {
		t.Fatalf("Flush() blocks = %d, want 1 (close-without-newline completes the block)", len(blks))
	}
	if blks[0].Kind != "dag" || blks[0].Skill != "demo" || blks[0].BlockID != "d0" {
		t.Errorf("block = %+v, want kind=dag skill=demo blockId=d0", blks[0])
	}
	if blks[0].Content != "{\"nodes\":[]}" {
		t.Errorf("content = %q, want %q", blks[0].Content, "{\"nodes\":[]}")
	}
	if txt := texts(tail); txt != "" {
		t.Errorf("Flush() text = %q, want empty (block completed, not flushed as text)", txt)
	}
}

// TestFenceParser_BlockIDExplicitAndAuto — an explicit #blockId is honored
// verbatim; when omitted, successive same-skill blocks get "<skill>-0",
// "<skill>-1", ... within the turn.
func TestFenceParser_BlockIDExplicitAndAuto(t *testing.T) {
	p := NewFenceParser()

	input := "" +
		"```dag:s#myid\n{}\n```\n" +
		"```dag:s\n{}\n```\n" +
		"```dag:s\n{}\n```\n"
	blocks := blocksOf(p.Feed(input))

	if len(blocks) != 3 {
		t.Fatalf("len(blocks) = %d, want 3", len(blocks))
	}
	if blocks[0].BlockID != "myid" {
		t.Errorf("blocks[0].BlockID = %q, want myid", blocks[0].BlockID)
	}
	if blocks[1].BlockID != "s-0" {
		t.Errorf("blocks[1].BlockID = %q, want s-0", blocks[1].BlockID)
	}
	if blocks[2].BlockID != "s-1" {
		t.Errorf("blocks[2].BlockID = %q, want s-1", blocks[2].BlockID)
	}
}

// TestFenceParser_BlockIDResetsPerTurn — Flush resets the per-skill auto-id
// sequence so the next turn starts again at "<skill>-0".
func TestFenceParser_BlockIDResetsPerTurn(t *testing.T) {
	p := NewFenceParser()

	blocks := blocksOf(p.Feed("```dag:s\n{}\n```\n"))
	if blocks[0].BlockID != "s-0" {
		t.Fatalf("blocks[0].BlockID = %q, want s-0", blocks[0].BlockID)
	}
	p.Flush()

	blocks = blocksOf(p.Feed("```dag:s\n{}\n```\n"))
	if blocks[0].BlockID != "s-0" {
		t.Errorf("blocks[0].BlockID after Flush = %q, want s-0 (reset)", blocks[0].BlockID)
	}
}

// TestFenceParser_OversizeBodyFlushedAsPassthrough — a fence body that
// exceeds the cap is treated as not-a-fence: the opening marker + buffered
// body are surfaced as text, and no block is emitted.
func TestFenceParser_OversizeBodyFlushedAsPassthrough(t *testing.T) {
	p := NewFenceParser()

	var sb strings.Builder
	sb.WriteString("```dag:s\n")
	// Each line is 1025 bytes (1024 'x' + newline); ~65 lines exceeds the
	// 64 KiB cap without ever seeing a closing marker.
	line := strings.Repeat("x", 1024) + "\n"
	for i := 0; i < 70; i++ {
		sb.WriteString(line)
	}
	// No closing "```" — the oversize bail-out must trigger before any close.

	segs := p.Feed(sb.String())

	if len(blocksOf(segs)) != 0 {
		t.Errorf("blocks = %v, want none (oversize, not a fence)", blocksOf(segs))
	}
	pt := texts(segs)
	if !strings.HasPrefix(pt, "```dag:s\n") {
		t.Errorf("text missing opening marker line: %q", truncStr(pt, 80))
	}
	if !strings.Contains(pt, "xxxx") {
		t.Errorf("text missing buffered body content")
	}

	// Parser must have abandoned fence-tracking: a later, well-formed block
	// in a subsequent Feed is parsed normally (state wasn't left inFence).
	segs2 := p.Feed("```dag:s2\n{}\n```\n")
	if pt2 := texts(segs2); pt2 != "" {
		t.Errorf("text after recovery = %q, want empty", pt2)
	}
	blocks2 := blocksOf(segs2)
	if len(blocks2) != 1 {
		t.Errorf("blocks after recovery = %d, want 1 (parser recovered)", len(blocks2))
	}
}

// TestFenceParser_UnknownKindNotTreatedAsFence — an info-string whose kind
// isn't one of the 5 known FencedBlockKinds is just an ordinary code fence
// and passes through verbatim (e.g. a language-tagged code block in prose).
func TestFenceParser_UnknownKindNotTreatedAsFence(t *testing.T) {
	p := NewFenceParser()

	input := "```python:notaskill\nprint(1)\n```\n"
	segs := p.Feed(input)

	if len(blocksOf(segs)) != 0 {
		t.Errorf("blocks = %v, want none (unknown kind)", blocksOf(segs))
	}
	if pt := texts(segs); pt != input {
		t.Errorf("text = %q, want verbatim input %q", pt, input)
	}
}

// TestFenceParser_ImmediateTextNoNewline — D-19 fix #2: plain text with no
// trailing '\n' and no leading backtick must stream out of Feed immediately,
// not be held pending a future newline (the token-streaming regression).
func TestFenceParser_ImmediateTextNoNewline(t *testing.T) {
	p := NewFenceParser()

	segs := p.Feed("Hello ")
	if len(segs) != 1 || segs[0].Text != "Hello " {
		t.Fatalf("segs = %+v, want a single immediate Text segment %q", segs, "Hello ")
	}
}

// TestFenceParser_BacktickPrefixDisprovedEarly — a line starting with
// backticks that can never become a valid marker ("“x": second char after
// one backtick breaks the 0-3-backtick prefix rule) streams out as soon as
// it's disproved, without waiting for a newline.
func TestFenceParser_BacktickPrefixDisprovedEarly(t *testing.T) {
	p := NewFenceParser()

	segs := p.Feed("``x")
	if len(segs) != 1 || segs[0].Text != "``x" {
		t.Fatalf("segs = %+v, want a single immediate Text segment %q", segs, "``x")
	}
}

// TestFenceParser_FullBacktickPrefixHeldUntilNewline — once a line commits
// to a full "```" it is held (per the contract's open-regex needing the
// whole line) even though the kind ("json") isn't one of the 5 known
// kinds; it streams out once the disproving '\n' arrives, not before.
func TestFenceParser_FullBacktickPrefixHeldUntilNewline(t *testing.T) {
	p := NewFenceParser()

	segs := p.Feed("```json")
	if len(segs) != 0 {
		t.Fatalf("segs after partial line = %+v, want none (still ambiguous, no newline yet)", segs)
	}

	segs = p.Feed("\n")
	if len(segs) != 1 || segs[0].Text != "```json\n" {
		t.Fatalf("segs after newline = %+v, want a single Text segment %q", segs, "```json\n")
	}
}

// TestFenceParser_MinifiedSingleLineBody — D-19 fix #1: a fence body
// delivered without any newline (minified JSON) must not grow the internal
// hold buffer unboundedly; it is buffered until the closing marker arrives
// and yields exactly one block.
func TestFenceParser_MinifiedSingleLineBody(t *testing.T) {
	p := NewFenceParser()

	segs := p.Feed("```dag:s\n")
	if len(segs) != 0 {
		t.Fatalf("segs after open = %+v, want none", segs)
	}

	segs = p.Feed(`{"a":1}`) // no trailing newline yet
	if len(segs) != 0 {
		t.Fatalf("segs mid-body (no newline) = %+v, want none (held as in-progress body)", segs)
	}

	segs = p.Feed("\n```\n")
	blocks := blocksOf(segs)
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d, want exactly 1", len(blocks))
	}
	if blocks[0].Content != `{"a":1}` {
		t.Errorf("Content = %q, want {\"a\":1}", blocks[0].Content)
	}
}

// TestFenceParser_HugeNoNewlineChunkBailsBounded — D-19 fix #1: a huge
// single Feed call with no newline anywhere and no backtick must stream out
// immediately (disproved at the very first byte) rather than holding the
// whole chunk in an unbounded internal buffer. The huge chunk has no '\n',
// so per the ^-anchored marker rule it is all one still-open physical line;
// a '\n' is fed next to close that line, and the parser then parses a
// well-formed block normally, proving its hold state is bounded (O(1), not
// still pinned to megabytes) rather than corrupted by the huge chunk.
func TestFenceParser_HugeNoNewlineChunkBailsBounded(t *testing.T) {
	p := NewFenceParser()

	huge := strings.Repeat("x", 100<<20) // 100 MiB, no '\n', no backtick
	segs := p.Feed(huge)

	if len(segs) != 1 || segs[0].Text != huge {
		t.Fatalf("expected the whole huge chunk echoed back as one Text segment (got %d segs)", len(segs))
	}

	segs = p.Feed("\n```dag:s\n{}\n```\n")
	blocks := blocksOf(segs)
	if len(blocks) != 1 {
		t.Fatalf("blocks after huge chunk = %d, want 1 (parser recovered, bounded state)", len(blocks))
	}
}

// TestFenceParser_MarkerTrailingWhitespace — D-19 fix #5: an open marker
// and a bare close marker with trailing horizontal whitespace still parse.
func TestFenceParser_MarkerTrailingWhitespace(t *testing.T) {
	p := NewFenceParser()

	segs := p.Feed("```dag:s \n{\"a\":1}\n``` \n")
	if pt := texts(segs); pt != "" {
		t.Errorf("text = %q, want empty", pt)
	}
	blocks := blocksOf(segs)
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(blocks))
	}
	if blocks[0].Content != `{"a":1}` {
		t.Errorf("Content = %q, want {\"a\":1}", blocks[0].Content)
	}
}

// TestFenceParser_ResetTurnClearsOpenFence — D-19 fix #4: ResetTurn
// defensively discards an in-progress fence and the ambiguous hold so the
// NEXT turn's plain text is not swallowed, and resets the auto-id sequence.
func TestFenceParser_ResetTurnClearsOpenFence(t *testing.T) {
	p := NewFenceParser()

	// Turn 1: open a fence and never close it (simulating an interrupt with
	// no EventResult / Flush).
	segs := p.Feed("```dag:s\n{\"partial\":true\n")
	if len(blocksOf(segs)) != 0 {
		t.Fatalf("blocks = %v, want none (fence open)", blocksOf(segs))
	}

	p.ResetTurn()

	// Turn 2: ordinary text must come through, not be swallowed by the
	// stale open fence from turn 1.
	segs = p.Feed("hello world\n")
	if pt := texts(segs); pt != "hello world\n" {
		t.Errorf("text after ResetTurn = %q, want %q (not swallowed)", pt, "hello world\n")
	}
	if len(blocksOf(segs)) != 0 {
		t.Errorf("blocks after ResetTurn = %v, want none", blocksOf(segs))
	}

	// skillSeq must also have been reset: a fresh auto-id block starts at -0.
	segs = p.Feed("```dag:s\n{}\n```\n")
	blocks := blocksOf(segs)
	if len(blocks) != 1 || blocks[0].BlockID != "s-0" {
		t.Fatalf("blocks = %+v, want single block with BlockID s-0", blocks)
	}
}
