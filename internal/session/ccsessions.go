package session

// Reading cc's OWN transcript store (tether#92).
//
// Two stores had never heard of each other. tether writes
// ~/.tether/sessions/<sid>/history.jsonl, and only when a prompt travels through
// its own WebTransport chat channel; cc writes the full record to
// <cc-config-dir>/projects/<encoded-cwd>/<sid>.jsonl on every turn, including
// the ones typed into a terminal. So a user with 38 real conversations in a
// workspace saw 8 rows, and the 30 that were missing were the ones with anything
// in them. This file is the reader that closes that gap.
//
// # It is READ ONLY, and that is a safety property, not a style note
//
// That directory is the user's actual work: on the machine this was written
// against it held 291,085 lines across 977 transcripts, and it is a host mount —
// damage there is not recoverable from inside the daemon's container. Nothing in
// this file writes, creates, renames, truncates or deletes. In particular it does
// NOT reuse agent.ccSettingsPath(): that helper MkdirAll's the directory before
// returning it, which is right for a file cc is meant to own and wrong for a
// store we are only allowed to look at.
//
// Every entry point here takes its base directory as an argument, and nothing in
// this file calls os.UserHomeDir — the caller that knows the home directory does
// the resolving (lifecycle.go). That is what makes "no test can reach the real
// store by accident" a property of the API rather than of test discipline.
// tether#92 also deleted jsonl_sync.go, which was dead code that reached the same
// store through os.UserHomeDir and, worse, encoded the path wrongly
// (projects/<sid>/<sid>.jsonl instead of projects/<encoded-cwd>/<sid>.jsonl) —
// a second, silently incorrect answer to the question this file exists to answer
// once.
//
// That paragraph used to end "It is a property of THIS TYPE, not of the package …
// If another such reader appears, this sentence stops being true." One has
// appeared: ccregistry.go reads cc's OTHER directory, <cc-config-dir>/sessions,
// and it holds the property the same way — its directory is an argument and it
// never calls os.UserHomeDir either. So the rule is now the PACKAGE's, stated
// twice on purpose, and lifecycle.go resolves both directories from a single
// CLAUDE_CONFIG_DIR read so the two readers cannot describe two different cc
// installs. A third reader that resolved its own path would break this again, and
// the way to tell is still the same: grep this package for os.UserHomeDir and
// expect nothing.
//
// # What this reader promises, and what it deliberately does not
//
// It answers "which conversations exist" and "what was said in one". It does NOT
// answer "can this one be continued". tether cannot know that before the fact:
// cc's `--resume` reports success or failure only after the first prompt has been
// delivered (see Attachment's doc), so any resumability flag computed here would
// be a prediction, and one that goes stale the moment --workspace-root or the
// workspace registry changes. The UI therefore states the limit instead of
// predicting it — see web/src/lib/SessionRow.tsx.
//
// tether#101 draws one line INSIDE that, and the line is worth knowing before
// reading the paragraph as an absolute. Resumability is still not predictable
// here. But cc's live-session registry — its OTHER directory, read by
// ccregistry.go — records which of its own processes hold which session id, and
// cc refuses a uuid `--resume` outright while a non-interactive one does. That is
// a decision cc takes from a file, before any prompt, so it is a fact rather than
// a prediction; it is also the only such fact, and it says "this is refused right
// now", never "this would otherwise work". SessionSummary.RunningAs carries it,
// and SessionSummary.Source's doc has the long form of the boundary.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// The stores a transcript can come from. One vocabulary, shared by
// SessionSummary.Source, SessionIndex.Messages and the /messages log line, so
// that "where did this come from" has one set of answers rather than a wire
// value and a log value that agree only by convention.
const (
	// SourceTether — tether recorded it, in ~/.tether/sessions/<sid>/history.jsonl.
	SourceTether = "tether"
	// SourceCC — cc recorded it and tether never saw the conversation.
	SourceCC = "cc"
	// SourceNone — no store has this session. Only Messages returns it; a row
	// that does not exist is not listed.
	SourceNone = ""
)

// ccTitlePrefixBytes bounds how much of a cc transcript ccTitle reads looking for
// the first user turn.
//
// It is NOT titlePrefixBytes, and the difference is measured rather than assumed.
// A cc transcript does not open with the user: it opens with a preamble of
// `attachment` / `queue-operation` / `file-history-snapshot` records, and across
// the 38 real top-level transcripts of one workspace (2026-08-17) the first
// actual user turn sat at byte offsets 319 … 60,103, median ≈ 47 KB. At tether's
// own 16 KiB that is 2 transcripts with a title and 36 falling back to a bare
// sid — i.e. exactly the column of UUID prefixes tether#91 existed to replace.
// 128 KiB is a bit over 2x the observed worst case.
//
// The ceiling this costs is sessions × this, not this — the same arithmetic
// titlePrefixBytes reasons about. It is smaller than it looks because the scan
// STOPS at the first user turn, so the typical read is the ~47 KB above and the
// cap only binds on a transcript that has no user turn at all in its first
// 128 KiB. Such a session loses its title, not its row.
const ccTitlePrefixBytes = 128 << 10

// ccMessagesTailBytes bounds how much of a cc transcript ccMessages reads, and
// ccMessagesWideTailBytes is the one retry it is allowed.
//
// The largest single transcript on the machine this was written against was
// 138 MB, so an unbounded read here is not a slow path, it is a way to take the
// daemon down by clicking a row. (The tether branch of SessionIndex.Messages is
// genuinely unbounded — LoadHistory os.ReadFile's the whole file. That asymmetry
// is deliberate: tether's own transcripts are written only by this daemon and the
// largest on this machine is 24 KB across 90 sessions. An earlier version of this
// comment defended the asymmetry by claiming LoadHistory "caps what it writes",
// which titlePrefixBytes' doc in this same package already refutes — nothing caps
// the number of turns.)
//
// The window is the TAIL, not the head: a chat pane opens scrolled to the most
// recent turn, and the beginning of the conversation is already the row's title.
//
// # Why there is a second, wider window
//
// The bound is on RAW JSONL BYTES, and cc's bytes are dominated by tool payloads
// that this reader reduces to a summary a fraction of their size — 4.85 MB of raw tool
// payload inside the 39 windows of one store becomes 0.37 MB served. So the last 1 MiB of a
// tool-heavy transcript can contain no conversation at all — and the result would
// be a row that has a title, is listed, and opens a chat with nothing to READ in
// it. That is the original symptom this whole change exists to remove, reappearing
// one layer down. Found by review.
//
// (Until tether#96 that sentence read "tool payloads that this reader then drops",
// and the retry fired on `len(msgs) == 0`. Now a tool-only tail produces bubbles, so
// the trigger is "no message carrying words" — see Messages. The two have to move
// together: the wording here IS the trigger's specification.)
//
// The retry is a wider window rather than a fall back to the HEAD, so that "you
// are reading the most recent messages" stays true; and it is one retry rather
// than a loop, so the worst case stays a number written here rather than a
// function of the file.
//
// The bound is soft against a file cc is actively appending to: the size is
// stat'd and then read to EOF, so the read can be marginally larger than the
// stat implied.
const (
	ccMessagesTailBytes     = 1 << 20
	ccMessagesWideTailBytes = 16 << 20
)

// ccMessagesMax caps how many messages a transcript can produce even when they
// fit in the byte window, so a transcript of very short lines cannot produce a
// JSON response with tens of thousands of elements in it.
//
// # It counts MESSAGES, and since tether#94 a message is a whole turn's text
//
// The value here used to be 400, and the sentence above it used to say "turns"
// while the trim that applies it — at the far end of ccMessagesFrom, some 400
// lines below — counted one element per cc record that converted. Those differ
// by a factor of 7.07: across the 39 top-level transcripts of one workspace
// (measured 2026-08-17) 3,350 records became user messages and 20,325 became
// assistant messages, because cc opens a new record at every tool call. So "400
// turns" was really about 57.
//
// That denominator is the population this reader EMITS, and it has to be: a
// further 941 records survive the block filter, wear type:"user" and still
// produce no message — 746 carrying isMeta and 195 <local-command-stdout>.
// (Not an exhaustive account of everything type:"user" swallows: thousands more
// are tool results, which have no text at all and never reach this arithmetic.)
// Counting those 941 gives a flattering 4.74 assistant records per turn instead
// of 6.07, and every figure derived from it lands ~30% out. An earlier draft of
// this comment used precisely that number; a ratio used to size a cap must be
// measured over the things the cap counts. (Found by review, which is the second
// time in this file that a confidently stated measurement was taken over the
// wrong population — see EncodeProjectDir.)
//
// Merging makes the unit real: on the same store the conversation goes from
// 23,675 records to 6,072 messages — one merged assistant message per turn that
// says anything (628 turns say nothing but tool calls), plus one message per
// thing the user actually said. Two consecutive user turns therefore stay TWO
// messages, which TestMessagesDoesNotMergeAcrossAUserTurn pins, so "two per
// turn" is the common case and not an invariant.
// TestMessagesCapCountsTurnsNotRecords pins the unit, because a doc comment
// asserting it is exactly what let the two drift apart.
//
// Every figure in this section is tether#94's, measured BEFORE tether#95 and left
// as it was so that it still adds up: 3,350 + 20,325 = 23,675, and 23,675 / 6,072
// = 3.90. The next section re-measures the same store after #95 rather than
// editing individual numbers in here, which is how an earlier attempt at this
// paragraph ended up asserting a 4.74 that no longer followed from the two
// figures it sat between.
//
// # Re-checked after tether#95, which moved every number above
//
// Dropping 386 noise records per store does not change the UNIT — a message is
// still a whole turn's text — but it does change the count, and this constant's
// meaning is a function of that count. Measured on the same store, before and
// after, with the same 200:
//
//	                       messages   through the 1 MiB window   text served
//	before #95                6,101                403 messages      0.79 MB
//	after  #95                5,357                368 messages      0.70 MB
//
// So #95 makes 200 cover MORE conversation, not less: measured per real user
// turn, the same cap now buys ~111 turns instead of ~98, because 386 bubbles that
// were never worth an element stopped taking one. The cap still binds on 0 of 39
// transcripts either way — the byte window binds first — so the value needs no
// change, which is the answer this re-check exists to produce rather than assume.
//
// The census reproduces the "after" row's two right-hand columns on demand rather
// than asking anyone to trust them: ccCensusCapReport in the test file re-reads
// the store through the real window and prints the message count, the bytes, and
// how often the cap binds. It does NOT reproduce the left column or the "before"
// row — a whole-store count needs a full replay rather than a windowed read, and
// nothing here can run last month's code. Written because the section above once
// claimed a unit the trim two lines away did not implement, and a number in a
// comment cannot notice when it stops being true.
//
// # The payload consequence: a ceiling this data never reaches
//
// Halving the number does not halve the response, because a MERGED ASSISTANT
// message carries 8.6 source records' worth of text on average — 20,445 assistant
// records over 2,378 assistant messages — instead of one.
//
// 8.6 is the figure a ceiling has to be built from, and getting that wrong is the
// standing hazard of this comment. A user message always carries exactly one
// record, so the mean over ALL messages is a much flatter 4.37; #94's version of
// this paragraph used its equivalent (3.90) and #95's first draft repeated the
// mistake with 4.37. The worst case is 200 messages that are all merged assistant
// turns, so:
//
//	200 × 8.6 ≈ 1,720 records' worth, against the old cap's 400 → ~4.3x
//	had the cap stayed 400: ~3,440 → ~8.6x
//
// Measured, it is 1.00x: the COUNT cap binds on 0 of 39 transcripts, so the
// multiple above is a ceiling for the transcript of very short lines this cap has
// always been for, not a change anyone will observe. Which is the point — the
// ceiling being 4.3x rather than 2x costs nothing precisely because nothing
// reaches it, and that is worth stating correctly rather than reassuringly.
//
// What this cap no longer bounds is a SINGLE message: the longest merged
// assistant message in that store is 34 KB, from a run of 114 fragments. Only the
// byte window bounds that — the same bound as before, now spread across fewer
// elements.
//
// # Re-checked again after tether#96, which hangs tool activity on those messages
//
// A merged turn now also carries its tool calls, so a message is bigger than it was
// and there are marginally more of them: a turn that says nothing but calls things
// used to produce no message at all. Measured on one frozen snapshot of the same
// store, before and after, on the SAME bytes:
//
//	                    messages   carrying words   text served   RESPONSE bytes
//	before #96               393              393       0.73 MB          0.76 MB
//	after  #96               394              393       0.73 MB          1.13 MB
//
// So the cap's meaning is unchanged — the conversation is byte-for-byte the same and
// one bubble was ADDED — and it still binds on 0 of 39 transcripts, largest
// transcript 27 messages against 200. The response grows 1.49x store-wide and 1.59x
// on the worst single transcript, which is the figure a page load pays: 44.0 KB to
// 70.1 KB.
//
// The right-hand column is the bytes the route WRITES, encoder and all, because a sum
// of field lengths would have missed what that encoder does to `<`, `>` and `&` — the
// mistake in the paragraph this one replaces, which reported a 0.51x quantity as
// 1.51x. Found by review.
//
// Which is the answer this re-check exists to produce rather than assume. What the cap
// counts had to change for it to stay true, though — see ccTrimFront.
const ccMessagesMax = 200

// The bound on the tool activity a cc turn carries (tether#96). These are
// CC-SPECIFIC and deliberately not history.go's MaxToolsPerTurn /
// MaxToolResultBytes: those govern the live native path, where a turn is a handful
// of calls and there is no read window to bound anything. Changing them would
// change tether's own transcripts.
//
// # There is deliberately no per-turn COUNT cap. What bounds this instead:
//
// Every value served is a PREFIX of a value inside the ccMessagesTailBytes window,
// so the CHARACTERS served are a subset of the characters read, and the window
// bounds them. A per-turn count cap would add nothing to that and would delete
// activity the user asked to see.
//
// An earlier version of this paragraph said "strictly smaller in BYTES", which is
// false and was caught by review. Go's encoder escapes `<`, `>` and `&` to
// six-byte `\uXXXX` forms; cc's JavaScript writer does not. So a value of `&&&&`
// costs one byte on disk and six on the wire, and a record can serve more bytes
// than it was read from — measured, 4.57 MiB of tool JSON out of a 1 MiB window of
// adversarially escaped records. That expansion is a property of the RESPONSE
// FORMAT, not of this change: the same encoder has always applied it to the
// conversation text this route serves (writeJSON in internal/server). What tether#96
// adds is bounded by the window in exactly the way the text already was.
//
// Measured on the real store the expansion is 1.11x, not 6x — 0.33 MB of served
// characters becomes 0.37 MB of JSON — so the per-call cost stays what the caps below
// say it is. TestToolPayloadIsBoundedByItsCaps pins the ceiling on adversarial input,
// and TestServedValuesArePrefixesOfTheSource pins the subset property; the byte
// inequality is NOT asserted anywhere, because it is not true.
//
// Measured in that window across the 39 transcripts of one store (frozen snapshot,
// 2026-08-17): 1,319 tool calls, largest single transcript 95, largest single merged
// TURN 61. The 11,398-call session that motivated a cap is a WHOLE-FILE count — the
// daemon never reads a whole file.
const (
	// ccToolInputValueRunes bounds each string argument of a call.
	//
	// The renderer shows ONE top-level string field per known tool, whitespace
	// collapsed, truncated at 60 chars (summarizeToolInput, web/src/panes/chat).
	// 128 runes is a bit over 2x that, so the visible summary line is identical to
	// what an untruncated input would produce unless more than half of a value's
	// first 128 characters are whitespace — in which case the summary line is
	// shorter than it would have been. Cosmetic, on a summary line, and no
	// occurrence in the census. Measured: 35.9% of string values exceed this, so
	// the cap binds often and is where most of the saving comes from.
	ccToolInputValueRunes = 128
	// ccToolInputMaxKeys bounds how many string arguments survive, alphabetically.
	//
	// Headroom rather than a filter: the most string-valued input in the census has
	// 7 (pf_remember), so this binds on nothing measured. It is here because a
	// projection with no key bound has no bound at all. When it DOES bind it keeps
	// the alphabetically first, which can in principle drop the one field the
	// renderer would have shown; said plainly rather than solved on a guess about
	// which key matters.
	ccToolInputMaxKeys = 12
	// ccToolErrorBytes bounds a FAILED tool result's message. Only failures are
	// served at all — see ccMessage.errorResults.
	ccToolErrorBytes = 2 << 10
)

// ccMaxEscapeBytesPerRune is the worst the response encoder can do to one character:
// `<`, `>` and `&` become six-byte `\uXXXX` escapes. Named so the ceiling the caps
// above imply can be WRITTEN DOWN and asserted rather than assumed — see
// TestToolPayloadIsBoundedByItsCaps. Nothing multiplies by it at runtime; it exists
// because the paragraph that used to claim "served is smaller than source" needed a
// true statement in its place.
const ccMaxEscapeBytesPerRune = 6

// ccTruncated marks a value this reader cut. Same wording as history.go's marker
// so a truncated result reads the same whichever store it came from.
const ccTruncated = "\n[... truncated ...]"

// ccTurnJoin separates the fragments of one merged assistant turn.
//
// It is a BLANK line rather than a newline, and that is a requirement rather
// than a preference: the pane renders an assistant message through
// react-markdown with remark-gfm and NO remark-breaks
// (web/src/panes/canvas/Markdown.tsx), so a lone "\n" is a CommonMark soft break
// and renders as a SPACE. The fragments this joins end exactly where cc stopped
// to call a tool — in the transcript that prompted this change, on "查清楚:" and
// "先看进度:" — so a soft break would run two of them onto one line and read
// worse than the split bubbles it replaces. A blank line is a paragraph break,
// which is the shape the terminal shows, where a tool card sat between them.
const ccTurnJoin = "\n\n"

// CCStore reads cc's project store. The zero value is not usable; see NewCCStore.
type CCStore struct {
	// projectsDir is <cc-config-dir>/projects. Required, never derived here —
	// see the file doc for why there is no default.
	projectsDir string
	// dirs reports the working directories tether has positive evidence about,
	// evaluated per call rather than captured once: workspaces can be added and
	// removed while the daemon runs, and a list that silently kept answering from
	// startup's snapshot would be wrong in a way nobody would think to check.
	//
	// It is a whitelist, and that is the point. The store on the reference
	// machine held 37 project directories of which 41 (across all of them) were
	// throwaway job/probe directories; enumerating everything would bury the two
	// directories the user actually works in.
	dirs func() []string
}

// NewCCStore builds a reader over projectsDir, listing only sessions whose
// working directory is one of the ones dirs reports.
//
// Both arguments are required and neither has a default. A nil dirs, or an empty
// projectsDir, yields a store that lists nothing — which is the correct
// behaviour for a daemon that cannot tell which directories are the user's.
func NewCCStore(projectsDir string, dirs func() []string) *CCStore {
	return &CCStore{projectsDir: projectsDir, dirs: dirs}
}

// CCProjectsDir names the store inside a cc config directory.
//
// Split out so the caller that knows the home directory (lifecycle.go) does the
// resolving and this package never calls os.UserHomeDir — which is what keeps
// "no test can accidentally read the real store" a property of the API rather
// than of test discipline.
func CCProjectsDir(ccConfigDir string) string {
	if ccConfigDir == "" {
		return ""
	}
	return filepath.Join(ccConfigDir, "projects")
}

// ccDirNameMaxLen is where cc stops using the plain encoding and appends a hash
// of the original path instead. See EncodeProjectDir.
const ccDirNameMaxLen = 200

// EncodeProjectDir maps a working directory to the directory name cc files its
// transcripts under: every UTF-16 code unit outside [a-zA-Z0-9] becomes '-'.
//
// # This rule was READ OUT OF CC, not inferred from its output
//
// Stated because the first version of this function was inferred, and was wrong
// in a way no amount of sampling here could have caught. cc's own encoder is
//
//	function z$o(e){return e.replace(/[^a-zA-Z0-9]/g,"-")}
//	function W9(e){let t=z$o(e);if(t.length<=kie)return t;
//	               return `${t.slice(0,kie)}-${y__(e)}`}
//	function rS(){return d$.join(Tn(),"projects")}
//	kie=200
//
// (Claude Code 2.1.237, read from the installed binary on 2026-08-20; identical
// in 2.1.233 down to the minified names, and 2.1.233 is what the paragraph below
// was originally written against.) The first version mapped only '/' and '.' —
// which matched all 37 real project directories on this machine, because not one
// of those paths contains a character outside [a-zA-Z0-9/.-]. A workspace at
// /root/my_project would have produced `-root-my_project` against cc's
// `-root-my-project` and listed ZERO sessions: no error, no log line, no failing
// test. Underscores in a repository path are not exotic.
//
// The lesson generalises past this function: a claim about a third party's
// behaviour has to come from the third party, and agreement with a sample is not
// evidence when the sample cannot express the disagreement.
//
// # UTF-16 code units, not runes (tether#120)
//
// There is no `u` flag on that regex, so String.prototype.replace walks the
// string as UTF-16 code units: a rune outside the BMP is a SURROGATE PAIR there
// and is matched — and replaced — twice. `for _, r := range` walks runes, so this
// function used to emit one dash where cc emits two, and `/w/😀/repo` resolved to
// `-w---repo` against cc's `-w----repo`. Measured consequence: List() returned
// ZERO rows for that workspace and find/findStat missed the transcript too, so
// the session was neither listed nor served — again with no error and no log
// line, the same shape as the underscore bug above.
//
// The version of this comment that shipped before tether#120 knew about the
// divergence, called it "irrelevant for real paths", and asserted that the >200
// branch in resolveProjectDir covered the case where it mattered. Both halves
// were wrong. An emoji in a directory name is ordinary, and the >200 branch is
// not reached by a 9-character name — the divergence is a dash count in the
// MIDDLE of the name, which no prefix scan can recover, so the recovery path that
// was cited as cover could never have fired.
//
// `t.length` is a code-unit count as well, which is the second thing this fixes:
// resolveProjectDir picks its branch on len(name), and the two agree only while
// the encoded name is ASCII — which, since every byte written below is ASCII, is
// now unconditionally true.
//
// Runs of invalid UTF-8 in the input are the one remaining place the two sides
// could in principle disagree, since Go's range yields one U+FFFD per bad byte
// and a JS runtime's own lossy decode need not group them the same way. Not
// reachable through this daemon — a workspace path comes from config the user
// typed — and stated rather than handled because guessing at another decoder's
// error recovery is how the first version of this function went wrong.
//
// # Forward only. There is deliberately no decoder in this package.
//
// The mapping is many-to-one — '/', '.', '_' and every other non-alphanumeric
// land on the same character — so /root/code/aicoding/gmi-ws and
// /root/code/aicoding/gmi/ws produce the IDENTICAL name
// (TestEncodeProjectDirCollides asserts exactly that, because a test that only
// round-trips one well-behaved path would pass while a decoder was wrong).
// Anything that needs a session's real working directory must read it out of the
// transcript, where cc records it per entry — never infer it from the name.
//
// Being used to GENERATE a name to look for is what makes the forward direction
// safe: a name that does not exist simply lists nothing.
func EncodeProjectDir(path string) string {
	if path == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range filepath.Clean(path) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			// One dash per UTF-16 code unit, so a non-BMP rune — two code units
			// for cc's regex, one iteration of this loop — gets two. See the
			// tether#120 section above.
			b.WriteByte('-')
			if r > 0xFFFF {
				b.WriteByte('-')
			}
		}
	}
	return b.String()
}

// resolveProjectDir returns the directory cc files `path`'s transcripts under,
// or "" when there is none.
//
// Almost always this is just EncodeProjectDir. The exception is the reason this
// function exists: past ccDirNameMaxLen characters cc emits
// `<first 200 chars>-<hash of the original path>`, and that hash is not
// reproducible from here. So the long case is answered by LOOKING rather than by
// computing — one ReadDir of the projects directory, matching the prefix cc
// would have kept. A workspace whose path encodes past 200 characters would
// otherwise list nothing at all, silently.
//
// The prefix match can in principle hit more than one directory (two long paths
// agreeing in their first 200 encoded characters). The first match wins, and the
// caller deduplicates by sid, so the cost is at worst listing a neighbour's
// sessions — never a wrong claim about which directory a session ran in, because
// this package never makes one.
//
// len(name) is compared against cc's `t.length`, which is a UTF-16 code-unit
// count, and the two are the same number only because EncodeProjectDir emits
// nothing but ASCII — one byte per code unit. That held by accident until
// tether#120: a rune-counting encoder made a path cc had HASHED come out at 200
// here, so this returned a computed name for a directory that does not exist and
// the ReadDir below never ran. Slicing at ccDirNameMaxLen bytes matches
// `t.slice(0,200)` for the same reason.
func (s *CCStore) resolveProjectDir(path string) string {
	name := EncodeProjectDir(path)
	if name == "" {
		return ""
	}
	if len(name) <= ccDirNameMaxLen {
		return filepath.Join(s.projectsDir, name)
	}
	prefix := name[:ccDirNameMaxLen] + "-"
	entries, err := os.ReadDir(s.projectsDir)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("cc sessions: read projects dir failed", "dir", s.projectsDir, "err", err)
		}
		return ""
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
			return filepath.Join(s.projectsDir, e.Name())
		}
	}
	return ""
}

// List returns one row per cc session in the directories dirs reports, newest
// first being SessionIndex.List's job rather than this one's.
//
// # Sub-agent transcripts are excluded structurally
//
// A sub-agent's record lives one level deeper, at
// <projects>/<encoded>/<parent-sid>/<child-sid>.jsonl. In the workspace this was
// built against that is 888 of the 926 .jsonl files under one directory — 96% —
// so getting this wrong does not degrade the list, it drowns it. The exclusion is
// os.ReadDir plus `if e.IsDir() { continue }`: this reader never recurses, so a
// nested file is not skipped by a filter that could be relaxed, it is never
// visited. (TestListExcludesSubAgentTranscripts puts one there anyway and fails
// if it surfaces.)
func (s *CCStore) List() []SessionSummary {
	if s == nil || s.projectsDir == "" || s.dirs == nil {
		return nil
	}
	var out []SessionSummary
	// Two different working directories can encode to one name (see
	// EncodeProjectDir), and a user may well register both a workspace and the
	// daemon default that point at the same place. Either way the same file would
	// be read twice and produce two rows for ONE session, which would then fight
	// over the "current session" highlight. Deduplicated by sid, which is the
	// identity that actually matters.
	seen := make(map[string]bool)
	for _, wd := range s.dirs() {
		dir := s.resolveProjectDir(wd)
		if dir == "" {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			// A workspace cc has never been used in has no directory. That is the
			// ordinary case, not a problem worth a log line.
			if !os.IsNotExist(err) {
				slog.Warn("cc sessions: read project dir failed", "dir", dir, "err", err)
			}
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			sid, ok := ccSidFromFile(e.Name())
			if !ok || seen[sid] {
				continue
			}
			fi, err := e.Info()
			if err != nil || fi.Size() == 0 {
				continue
			}
			seen[sid] = true
			out = append(out, SessionSummary{
				Sid:       sid,
				Title:     ccTitle(filepath.Join(dir, e.Name())),
				UpdatedAt: fi.ModTime().UnixMilli(),
				Source:    SourceCC,
			})
		}
	}
	return out
}

// Has reports whether cc has a transcript for sid in one of the known
// directories.
func (s *CCStore) Has(sid string) bool {
	return s.find(sid) != ""
}

// Messages returns the tail of a cc transcript as tether history messages, and
// whether the transcript was found at all.
//
// The bool is not redundant with a nil slice: "cc has never heard of this
// session" and "cc has it and nothing in the window converts to a turn" are
// different answers, and the route above this one uses the difference to decide
// whether it is looking at a session it should be serving at all.
//
// It is the newest page of MessagePage, kept as a name because "the tail" is what
// most callers want and because its own tests are the ones that pin the tail's
// content. There is one implementation, not two.
func (s *CCStore) Messages(sid string) ([]HistoryMessage, bool) {
	page, ok := s.MessagePage(sid, TranscriptTail)
	return page.Messages, ok
}

// CCPage is one window of a cc transcript, plus where in the file it began.
//
// Earlier is a BYTE OFFSET into the transcript, and it is the offset of the first
// byte this page served — not of the window this page read. Those differ whenever
// the message cap trimmed the front (see ccMessagesFromAt), which is one of the two
// regimes a cursor has to survive.
type CCPage struct {
	Messages   []HistoryMessage
	Earlier    int64
	HasEarlier bool
}

// MessagePage returns the page of a cc transcript that ENDS at byte `before`, or
// the newest page when before is TranscriptTail (tether#107).
//
// # Why the cursor is a byte offset
//
// Because it is the only cursor this store can produce without reading the whole
// file, which is the cost the window exists to avoid. cc appends and never
// rewrites, so an offset keeps naming the same record for as long as the file
// lives. A message INDEX would need a full replay to compute; a TIMESTAMP is
// neither unique (ccMessagesFrom stamps a merged turn with its first fragment's
// time) nor monotonic across a --resume.
//
// The page size is ccMessagesTailBytes for every page, including this one, so the
// first fetch of a session costs exactly what it costs today and a session that
// never asks for an earlier page never pays for the feature. Measured on the
// largest real transcript (117.2 MiB, 2026-08-19) one such window holds 196 JSONL
// lines — 25 type:"user" and 57 type:"assistant" records, ~5.3 KB per line — so a
// page is roughly 25 bubbles: a page-sized amount of reading rather than a
// technicality. It also means ccMessagesMax does not bind there, which is what
// ccMessagesMax's own doc says about this store.
func (s *CCStore) MessagePage(sid string, before int64) (CCPage, bool) {
	path := s.find(sid)
	if path == "" {
		return CCPage{}, false
	}
	f, err := os.Open(path)
	if err != nil {
		slog.Warn("cc sessions: open transcript failed", "sid", sid, "err", err)
		return CCPage{}, false
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		slog.Warn("cc sessions: stat transcript failed", "sid", sid, "err", err)
		return CCPage{}, false
	}

	// The window's far end. Clamped to the file rather than refused: a cursor past
	// the end is what a client holding a stale offset for a truncated-and-rewritten
	// file would send, and the tail is the honest answer to it.
	until := fi.Size()
	if before != TranscriptTail && before < until {
		until = before
	}
	if until < 0 {
		until = 0
	}

	page, err := ccReadWindow(f, until, ccMessagesTailBytes)
	if err != nil {
		slog.Warn("cc sessions: read transcript failed", "sid", sid, "err", err)
		return CCPage{}, false
	}
	// No CONVERSATION in the window, and the window did not reach the start of the
	// file: the range was all tool payload. Widen once — see ccMessagesTailBytes for
	// why an empty answer here would be the original bug wearing a different hat.
	//
	// The test is "no message carrying words", not "no messages", and tether#96 is
	// why: once tool activity produces bubbles, a tail that is nothing but tool
	// records is no longer EMPTY, so a `len(msgs) == 0` trigger would stop firing
	// and serve tool cards INSTEAD of the conversation widening would have found.
	// That is this whole change failing in the one way that looks like success in a
	// screenshot. 0 of 39 real transcripts widen today, so no test that only counts
	// today's store would have noticed — see TestMessagesWidensPastAWallOfToolCalls.
	//
	// tether#107: the trigger was `fi.Size() > ccMessagesTailBytes` and is now
	// `until > ccMessagesTailBytes`, which is the SAME condition for the newest page
	// (until is then the size) and the right generalisation for an earlier one — the
	// question is whether this window reached byte 0, not how big the file is. It
	// applies to earlier pages deliberately: one rule, and a page with nothing to
	// read is no better a thing to hand a reader who clicked than to hand one who
	// opened the pane.
	if !ccHasConversation(page.Messages) && until > ccMessagesTailBytes {
		wide, err := ccReadWindow(f, until, ccMessagesWideTailBytes)
		if err != nil {
			slog.Warn("cc sessions: wide read failed", "sid", sid, "err", err)
			// The NARROW page, not an empty one. Before tether#107 this returned
			// nothing at all, which is now a worse answer than it was: an empty page
			// with no cursor is indistinguishable from "the conversation starts here",
			// so a failed widen would make the pane state a falsehood rather than show
			// tool cards. The narrow page's cursor is true whatever the wide read did.
			return page, true
		}
		slog.Debug("cc sessions: widened the transcript window", "sid", sid,
			"size", fi.Size(), "until", until, "found", len(wide.Messages))
		page = wide
	}
	return page, true
}

// ccHasConversation reports whether any of these messages carries words.
//
// A message with tools and no text is real output — it is what a turn that did
// nothing but call things looks like — but it is not something to READ, so it must
// not satisfy the widen retry. See Messages.
func ccHasConversation(msgs []HistoryMessage) bool {
	for _, m := range msgs {
		if m.Text != "" {
			return true
		}
	}
	return false
}

// ccReadWindow converts the `window` bytes of f that END at `until`.
//
// Was ccReadTail(f, size, window) until tether#107; the tail is now the case where
// `until` is the file size, so there is one reader rather than a tail reader and a
// page reader that could disagree about where a record begins.
//
// # The io.LimitReader is load-bearing, not hygiene
//
// Without it an earlier page would seek back and then read to EOF — i.e. serve the
// whole rest of the file — and the symptom would be a "load earlier" that appears
// to work while quietly costing what the unbounded read this store exists to avoid
// costs.
//
// # Why the reported offset is where the page's FIRST SERVED BYTE is
//
// So that page N+1 = [cursor-window, cursor) meets page N exactly, with no gap and
// no duplicate. `cursor` always sits immediately after a '\n' (or at 0), so the
// record that ends at `cursor` is complete in page N+1 and absent from page N.
//
// Reporting the WINDOW START instead is the obvious version and it silently loses
// one record per seam: this function drops the fragment the window opens on, and
// the next page — reading up to the window start — would truncate that same record
// into an unterminated final line, which ccMessagesFromAt discards. One record
// missing per megabyte, in the middle of a conversation, with nothing on screen to
// suggest it. TestMessagePageWalksBackWithoutGapOrDuplicate is the guard.
func ccReadWindow(f *os.File, until, window int64) (CCPage, error) {
	if until <= 0 {
		return CCPage{}, nil
	}
	start := until - window
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return CCPage{}, err
	}
	br := bufio.NewReader(io.LimitReader(f, until-start))
	// Where the first COMPLETE record in this window begins.
	base := start
	if start > 0 {
		// The window almost certainly opens mid-line. Drop that fragment: a truncated
		// JSON object would merely fail to parse, but the point of dropping it
		// explicitly is that a fragment which happens to parse would contribute half a
		// record to the transcript the user reads.
		frag, err := br.ReadBytes('\n')
		if err != nil {
			// A whole window with no line terminator in it: one record longer than the
			// window. Nothing to serve, and the cursor still has to make progress or
			// "load earlier" would be a button that does nothing forever — so it is the
			// window start, and the next page reads the megabyte before this one.
			return CCPage{Earlier: start, HasEarlier: start > 0}, nil
		}
		base += int64(len(frag))
	}
	msgs, off := ccMessagesFromAt(br)
	at := base + off
	// tether#109 — ccMessagesFromAt numbers messages relative to the reader it was
	// handed, and this is the only place that knows where that reader started. An Ord
	// is compared against Ords from OTHER pages of the same file, so a page-relative
	// one would make two windows of the same conversation disagree about the order of
	// the records they share — which is the defect this field exists to detect.
	for i := range msgs {
		msgs[i].Ord += base
	}
	return CCPage{Messages: msgs, Earlier: at, HasEarlier: at > 0}, nil
}

// find resolves sid to a transcript path, or "" when no known directory has one.
//
// ValidSessionID first, and not as a formality: sid arrives from the URL of
// GET /api/v1/sessions/<sid>/messages and is about to be joined into a path, so
// without the guard a `..`-shaped sid turns this into a reader for any .jsonl
// file on the host. The same reason HistoryStore.HasHistory checks it.
func (s *CCStore) find(sid string) string {
	path, _ := s.findStat(sid)
	return path
}

// findStat is find plus the FileInfo the search already had to read.
//
// Split out for ModTime (tether#106), which wants the mtime of exactly the file
// find selects. Returning it from here rather than stat-ing again in ModTime is
// not about the syscall: a second stat is a second observation, and between the
// two the file could be replaced, so the timestamp would describe a file this
// function did not choose.
func (s *CCStore) findStat(sid string) (string, os.FileInfo) {
	if s == nil || s.projectsDir == "" || s.dirs == nil || !ValidSessionID(sid) {
		return "", nil
	}
	for _, wd := range s.dirs() {
		dir := s.resolveProjectDir(wd)
		if dir == "" {
			continue
		}
		path := filepath.Join(dir, sid+".jsonl")
		if fi, err := os.Stat(path); err == nil && !fi.IsDir() && fi.Size() > 0 {
			return path, fi
		}
	}
	return "", nil
}

// ModTime reports when cc last wrote this session's transcript, in Unix
// milliseconds, and whether cc has one at all (tether#106).
//
// Same shape and same reasoning as HistoryStore.ModTime — see there for why the
// bool is separate from the timestamp instead of "0 means no".
func (s *CCStore) ModTime(sid string) (int64, bool) {
	_, fi := s.findStat(sid)
	if fi == nil {
		return 0, false
	}
	return fi.ModTime().UnixMilli(), true
}

// ccSidFromFile extracts the session id from a transcript file name, and reports
// whether the name is one at all.
func ccSidFromFile(name string) (string, bool) {
	sid, ok := strings.CutSuffix(name, ".jsonl")
	if !ok || !ValidSessionID(sid) {
		return "", false
	}
	return sid, true
}

// ccTitle derives a row label from a transcript: the first thing the user said.
//
// Same rule as SessionIndex.title, so a cc row and a tether row mean the same
// thing, with a different constant and a different line format underneath.
func ccTitle(path string) string {
	f, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("cc sessions: open transcript failed", "path", path, "err", err)
		}
		return ""
	}
	defer f.Close()
	return condense(ccFirstUserText(f, ccTitlePrefixBytes))
}

// ccFirstUserText scans at most limit bytes of a cc transcript for the first
// thing the user actually typed, and stops as soon as it finds it.
//
// limit is a parameter rather than the constant so a test can assert the two
// properties that make this function correct rather than merely fast — that it
// never reads past the bound, and that it stops early once it has an answer —
// by handing it a reader that counts.
func ccFirstUserText(r io.Reader, limit int64) string {
	br := bufio.NewReader(io.LimitReader(r, limit))
	for {
		line, err := br.ReadBytes('\n')
		// Only a line that ENDED is a line: the last one before the limit is
		// almost always cut mid-record, and ReadBytes hands back the fragment
		// along with the error. bufio.Scanner is not used here because a cc
		// preamble record can be far larger than its default token limit, and its
		// failure mode for that is to stop scanning — which would look exactly
		// like "this session has no user turn".
		if bytes.HasSuffix(line, []byte("\n")) {
			if t := ccUserTextFromLine(line); t != "" {
				return t
			}
		}
		if err != nil {
			return ""
		}
	}
}

// ccUserTextFromLine returns what the user typed on this line, or "" if the line
// is not a user turn.
func ccUserTextFromLine(line []byte) string {
	// Cheap reject before any JSON parsing. Roughly 25 preamble records precede
	// the first user turn and several are tens of kilobytes; unmarshalling them
	// to discover they are attachments is the whole cost of this scan.
	//
	// The needle is the quoted VALUE `"user"`, which is safe against an encoder
	// that adds whitespace around punctuation (no encoder puts spaces inside a
	// string literal) — unlike `"type":"user"`, which would silently match
	// nothing and turn every title into a bare sid. It does not match the key
	// "userType" (the closing quote comes after "userType", not after "user").
	if !bytes.Contains(line, []byte(`"user"`)) {
		return ""
	}
	var e ccEntry
	if err := json.Unmarshal(line, &e); err != nil {
		// One corrupt line is skipped, not fatal — the same treatment
		// LoadHistory gives its own. A title is the least important thing here.
		return ""
	}
	// userText answers the WHOLE question, structural exclusions included — it
	// used to answer only the shape half and leave isUserTurn to each caller,
	// which is two places to forget. See userText.
	return e.userText()
}

// ccMessagesFrom converts a cc transcript window into tether history messages.
//
// # What survives: the conversation, plus a SUMMARY of what the agent did
//
// User turns, the assistant's text, and — since tether#96 — the tool calls made
// during a turn, as ToolCallRecord on the turn they belong to. What survives of a
// call is its name, its id, a bounded projection of its string arguments, and, for
// a call that FAILED, a bounded message. See ccToolInputValueRunes for the bound
// and ccMessage.errorResults for why successes carry no output.
//
// Two things are still dropped, both deliberately:
//
//   - Successful tool_result content — 3.33 MB of the 4.85 MB of raw tool payload
//     inside those windows, and serving it truncated would make the UI LIE:
//     summarizeToolResult derives "N lines" / "N matches" by counting the content
//     it was handed, so a capped result renders a confidently wrong number. An
//     honest partial result needs a fidelity design of its own.
//   - thinking blocks. Not for payload reasons: measured across all 93 top-level
//     transcripts of one real store (2026-08-17, cc 2.1.233), 22,350 of 22,350
//     thinking blocks carry `"thinking": ""` — the reasoning is encrypted into the
//     block's `signature` and there is no text to serve. Converting them would give
//     every turn a "thought" toggle that expands to nothing. This is a fact about
//     that store on that day, not about cc in general; if cc starts writing
//     plaintext thinking the decision needs re-taking, and
//     TestThinkingIsNotServedFromCC records the measurement next to the assertion.
//
// # One bubble per turn (tether#94)
//
// Consecutive assistant records are merged into one message. cc opens a NEW
// assistant record at every tool call, so a single nine-minute turn arrives as a
// row of fragments, each ending exactly where the agent stopped to call
// something. Measured over 39 transcripts: 6.07 assistant records per emitted
// turn, median run of 4, worst 114, 41% of runs at 5 or more. tether's own path
// feeding the SAME pane was reported at 0.94 — one interface, two renderings of
// one conversation, six-fold apart. One record one bubble showed the reporter
// six timestamped bubbles in a row with no user message between them, every one
// of them ending on a colon.
//
// # The boundary is an EMITTED user message, not isUserTurn
//
// A run of fragments is closed by a user message this function actually
// appended — read off out[n-1].Role. It is deliberately NOT isUserTurn, which is
// Type=="user" && !IsMeta && !IsSidechain and is therefore TRUE of every tool
// result: cc returns those as type:"user" records, one measured session carrying
// 11 of them against 3 real inputs. Closing a run on isUserTurn would end the
// turn at every tool call — the exact boundary this merge exists to erase — and
// ship a change that fragments the transcript rather than joining it.
//
// Reading the boundary off `out` is what makes that automatic rather than
// remembered: every impostor already produces no message at all (userText
// returns "" for it), so there is nothing to exclude a second time and no second
// list to keep in sync. Sidechain ASSISTANT records are dropped by the same
// existing rule, so a sub-agent speaking between two fragments does not split
// them either.
//
// # That is the seam tether#95 arrived through, and it needed nothing here
//
// #95 found 433 records reaching the pane as raw markup, and DROPPED 386 of them
// (the other 47 it renders — see ccUserShapes). The bigger half of the damage was
// not how they looked: replaying the store before and after, those 386 drops turn
// 2,736 assistant messages into 2,378, so 358 pairs of fragments that #94 could
// not join now join. Task notifications are essentially all of it — 355 of the 364
// sit directly between two assistant fragments — and that is what left the
// reporter looking at exactly the row of colon-terminated bubbles #94 set out to
// remove.
//
// Because the boundary is read off `out`, teaching userText to drop a shape is all
// it took: there is no list of "records that do not close a turn" here to update,
// which is the property this paragraph is asserting.
// TestMessagesMergesAcrossDroppedNoise pins the hop, because "the fix landed in
// userText and the merge is in ccMessagesFrom" is precisely the kind of
// two-function claim that goes untested.
//
// The two shapes #95 KEPT — `<bash-input>` and cc's interrupt marker — do close
// a run, and should: the user really did run that command, really did press that
// key, and an assistant turn genuinely ends there.
//
// Merging happens as records are read, so the cap below trims MESSAGES — see
// ccMessagesMax, whose unit this is the other half of.
//
// # Tool activity attaches to the MERGED turn, which is what keeps the turn count
// fixed
//
// cc never puts text and tool_use in one record — measured over a real store, the
// block combinations are `text`, `tool_use` and `thinking`, each alone, never
// mixed. So a tool call always arrives in a record that has no words in it, and
// before tether#96 such a record produced nothing at all.
//
// It now produces a message, and the merge rule above absorbs it: a tool record
// following assistant text merges into that text's bubble, and assistant text
// following a tool record merges into the tool record's bubble. Either way ONE
// bubble per turn, and the number of messages a window yields is unchanged — except
// for a turn that says nothing but calls things (628 of them in one real store),
// which gains a bubble it never had. That direction is the point; the direction
// that would matter is losing one, which
// TestToolsDoNotShrinkTheVisibleTranscript pins by converting the same fixture
// twice and comparing counts.
//
// A result reported in a LATER record is hung on its call by id, through a map of
// where each call landed. Positions, not pointers: append reallocates. The map is
// only read while the loop runs — the trim at the end reslices `out` and would
// invalidate every position in it.
//
// # A turn's TIMESTAMP can now be earlier, and that is the correct direction
//
// The bubble keeps the first fragment's stamp, and since a turn's first record is
// usually a tool call rather than words, that stamp is now the moment the agent
// started WORKING rather than the moment it started typing. Measured on a real
// daemon before and after this change, one turn's stamp moved from 10:00:04 to
// 10:00:02 — the two leading tool calls it had always made but never shown.
//
// Worth stating because it is a visible change nobody asked for. It is kept because
// it is what tether's own path does: history.go stamps the accumulator when the turn
// is CREATED, which is the first event of the turn whatever kind it is, so leaving cc
// stamped at first-words would have made the two paths disagree about when a turn
// began. TestToolsMergeWithoutALeadingBlankLine asserts the stamp explicitly, so this
// is a decision rather than a side effect.
func ccMessagesFrom(r io.Reader) []HistoryMessage {
	msgs, _ := ccMessagesFromAt(r)
	return msgs
}

// ccMessagesFromAt is ccMessagesFrom plus the offset, relative to r's first byte,
// at which the page it returns begins (tether#107).
//
// # The offset answers "what do I ask for to get the page before this one"
//
// It is 0 whenever nothing was trimmed, and that is not the same as "the first
// message's offset". A window can legitimately open on records that produce no
// message at all — a tool result is a type:"user" record with no text, and
// ccUserShapes drops several more — so the first message may sit some way in. If
// this returned that message's offset, a COMPLETE transcript whose first line is one
// of those would report a cursor above zero, the pane would offer to load earlier
// messages, and the click would return nothing. A false "there is more" is the exact
// failure mode tether#107 exists to remove, in the opposite direction.
//
// When the cap DID trim, the offset is the trimmed-to message's own line, because
// then there really is earlier content and it really does start there.
//
// starts is index-aligned with out: a line that opens a NEW message appends to both,
// a line that MERGES into the previous assistant turn appends to neither. That
// alignment is what makes starts[drop] the right answer for ccTrimFront's index, and
// it is why the append lives in the else branch rather than after the if.
func ccMessagesFromAt(r io.Reader) ([]HistoryMessage, int64) {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReader(r)
	}
	var out []HistoryMessage
	var starts []int64
	// Bytes consumed before the line about to be read.
	var pos int64
	at := make(map[string]ccToolPos)
	for {
		line, err := br.ReadBytes('\n')
		lineAt := pos
		pos += int64(len(line))
		// Only a terminated line is a line — the same rule ccFirstUserText uses,
		// and for the same reason. An earlier version accepted an unterminated
		// final line here while rejecting it there; real cc files end with a
		// newline, so the difference only showed up mid-append, which is exactly
		// where a half-written record that happens to parse would be surfaced to
		// the user. One rule, both readers.
		if bytes.HasSuffix(line, []byte("\n")) {
			m, failures, ok := ccMessageFromLine(line)
			if ok {
				// Still the same turn: cc opened a new record to call a tool, not
				// because the agent stopped talking. Append rather than emit, and
				// keep the FIRST fragment's stamp — that is when the response
				// began, which is also what tether's own path records for an
				// assistant message (history.go stamps the accumulator when it is
				// created, not when it is flushed).
				idx, fresh := len(out), 0
				if n := len(out); n > 0 && m.Role == "assistant" && out[n-1].Role == "assistant" {
					idx = n - 1
					fresh = len(out[idx].Tools)
					out[idx].Text = ccJoinTurn(out[idx].Text, m.Text)
					out[idx].Tools = append(out[idx].Tools, m.Tools...)
				} else {
					// tether#109 — Ord and starts are the SAME fact read two ways, and
					// they are assigned together here so they cannot drift: starts is the
					// byte to read FROM (the cursor `?before=` takes, see below), Ord is
					// the rank to order BY (1-based, see HistoryMessage.Ord). The +1 is
					// what makes them different numbers on purpose. A MERGE takes neither
					// branch, so a turn's Ord stays its first fragment's — the same rule
					// its Ts already follows.
					m.Ord = lineAt + 1
					out = append(out, m)
					starts = append(starts, lineAt)
				}
				// Index only the calls this line contributed. Re-indexing the whole
				// message would be quadratic in a turn that calls 61 things.
				for j := fresh; j < len(out[idx].Tools); j++ {
					if id := out[idx].Tools[j].ID; id != "" {
						at[id] = ccToolPos{msg: idx, tool: j}
					}
				}
			}
			// Reported even when the line produced no message, because that is
			// exactly the shape of the record carrying them: cc returns a tool
			// result as a type:"user" record with no text in it.
			for _, f := range failures {
				if p, found := at[f.toolUseID]; found {
					out[p.msg].Tools[p.tool].Result = &ToolResultRecord{Content: f.content, IsError: true}
				}
				// Not found: the call it belongs to fell outside the window. Dropped
				// rather than surfaced as a call with no name — an orphan result is
				// the ordinary consequence of reading a tail, not a defect.
			}
		}
		if err != nil {
			break
		}
	}
	// Newest turns win when there are more than the cap: the tail is what the
	// window was chosen to preserve, so trimming from the front keeps that
	// decision consistent instead of silently reversing it.
	drop := ccTrimFront(out, ccMessagesMax)
	first := int64(0)
	if drop > 0 && drop < len(starts) {
		first = starts[drop]
	}
	return out[drop:], first
}

// ccTrimFront returns how many messages to drop from the front so that at most max
// of them CARRY WORDS.
//
// # Counting words rather than messages is what stops tether#96 costing a turn
//
// The cap used to count messages, and while every message was a turn's text the two
// were the same thing. They stopped being the same when a turn that says nothing but
// calls things started producing a bubble: a transcript at the cap would then lose
// one text bubble off the FRONT for every tool-only bubble gained at the back. Not
// reachable on today's store — the largest transcript produces 27 messages against a
// cap of 200 — but "unreachable" is a measurement, and this is the one place where
// showing MORE tool activity could show LESS conversation. Found by review, which
// broke it at 180 text bubbles in, 150 out.
//
// # The element count stays bounded, which is what the cap is for
//
// Exempting tool-only bubbles sounds like removing the bound. It is not: a new
// assistant bubble only starts when the previous message was not an assistant one
// (everything else merges), so assistant bubbles are bounded by the user messages
// between them, and a user message with no words is never emitted. Hence at most
// 2*max+1 messages, all told, for a cap of max turns of conversation.
// TestMessagesCapBoundsTheElementCount pins that ceiling, because "bounded" was the
// entire reason this cap exists and an argument in a comment is not a bound.
func ccTrimFront(msgs []HistoryMessage, max int) int {
	words := 0
	for _, m := range msgs {
		if m.Text != "" {
			words++
		}
	}
	if words <= max {
		return 0
	}
	// Drop up to and including the (words-max)'th message with words in it. Any
	// tool-only bubble that follows it is kept, which is why the answer is an index
	// rather than a count of messages to remove from either end.
	drop := words - max
	for i, m := range msgs {
		if m.Text == "" {
			continue
		}
		if drop--; drop == 0 {
			return i + 1
		}
	}
	return 0
}

// ccToolPos is where one call ended up: which message, and which slot in its
// Tools. See ccMessagesFrom for why this is a position and not a pointer.
type ccToolPos struct {
	msg  int
	tool int
}

// ccJoinTurn appends one fragment of an assistant turn to the ones before it.
//
// The join is only inserted BETWEEN two pieces of text. Without that check a turn
// whose first record was a tool call — the ordinary shape, since cc never puts
// text and tool_use in the same record — would open with a blank paragraph,
// because its bubble starts life with an empty Text.
func ccJoinTurn(prev, next string) string {
	switch {
	case prev == "":
		return next
	case next == "":
		return prev
	default:
		return prev + ccTurnJoin + next
	}
}

// ccMessageFromLine converts one transcript line into the message it becomes and
// the failed tool results it reports.
//
// The failures come back INDEPENDENTLY of the bool, and that is not a wart: the
// record carrying a tool result is a type:"user" record with no text in it, so the
// line that reports a failure is precisely a line that emits nothing. A caller that
// only looked at the message would silently serve every failed call as if it had
// succeeded.
func ccMessageFromLine(line []byte) (HistoryMessage, []ccToolError, bool) {
	if !bytes.Contains(line, []byte(`"user"`)) && !bytes.Contains(line, []byte(`"assistant"`)) {
		return HistoryMessage{}, nil, false
	}
	var e ccEntry
	if err := json.Unmarshal(line, &e); err != nil {
		return HistoryMessage{}, nil, false
	}
	// Cheap reject before decoding the content array a SECOND time: most records
	// have no tool block in them at all (measured: 2,674 text-only assistant
	// records against 3,897 tool_use ones), and the block decode for tool activity
	// carries the input and result payloads that make this file's bytes.
	hasTool := bytes.Contains(line, []byte(`"tool_`))
	var (
		role, text string
		tools      []ToolCallRecord
		failures   []ccToolError
	)
	switch {
	// Type only — every other reason a type:"user" record is not a user turn
	// (isMeta, isSidechain, a tool result, machine output wearing a tag) is
	// userText's to know, and it says so by returning "", which the check below
	// already turns into "emit nothing".
	case e.Type == "user":
		role, text = "user", e.userText()
		// isSidechain explicitly, even though a sub-agent's calls are never in the
		// id map to match against (its assistant records are excluded below, and
		// List never opens a sub-agent file). Two independent filters rather than
		// one, for the same reason ccEntry.IsSidechain exists at all.
		if hasTool && !e.IsSidechain {
			failures = e.Message.errorResults()
		}
	case e.Type == "assistant" && !e.IsSidechain:
		role, text = "assistant", e.Message.text()
		if hasTool {
			tools = e.Message.toolCalls()
		}
	default:
		return HistoryMessage{}, nil, false
	}
	// A record with tools and no words is a message: it is what a turn that only
	// called things looks like, and dropping it is what made those turns invisible.
	if text == "" && len(tools) == 0 {
		return HistoryMessage{}, failures, false
	}
	return HistoryMessage{
		Role:  role,
		Text:  text,
		Ts:    ccTimestampMillis(e.Timestamp),
		Tools: tools,
	}, failures, true
}

// ccTimestampMillis converts cc's ISO-8601 stamp to the Unix milliseconds the
// wire type uses. An unparseable or absent stamp yields 0, which the frontend
// already renders as "no time known" rather than as 1970 (see sessionWhen).
func ccTimestampMillis(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

// ccEntry is the part of one cc transcript line this package understands.
//
// cc writes many record types through the same file — attachment, mode,
// permission-mode, queue-operation, file-history-snapshot, last-prompt, user,
// assistant — and adds more over time. Decoding into a struct with the four
// fields that decide "is this a turn, and whose" means an unknown type is
// ignored rather than misread, and a new field appearing costs nothing.
type ccEntry struct {
	Type string `json:"type"`
	// IsSidechain marks a record belonging to a sub-agent. Sub-agents are already
	// excluded by directory depth (see List), so this is the second of two
	// independent filters rather than the only one — a sidechain record inside a
	// top-level file would otherwise put a sub-agent's prompt in a human's row.
	IsSidechain bool `json:"isSidechain"`
	// IsMeta marks content cc injected on the user's behalf rather than something
	// the user typed: the `<local-command-caveat>Caveat: The messages below were
	// generated by the user while running local commands…` preamble, and the
	// expanded body of a slash command. It was the first "user" record in 3 of
	// the 38 transcripts measured, so ignoring it is worth 8% of the list's rows.
	IsMeta    bool       `json:"isMeta"`
	Timestamp string     `json:"timestamp"`
	Message   *ccMessage `json:"message"`
}

// isUserTurn reports whether cc's own METADATA rules this record out as
// something the human typed. It is one half of the question; the other half is
// the shape of the text, and lives in ccClassifyUserText. userText is the single
// place that asks both, and the only thing any caller needs.
//
// Two halves rather than one predicate because the two axes grow differently.
// This one has been closed since tether#92: Type, IsMeta and IsSidechain are
// fields cc's schema already had, and no impostor has ever arrived as a fourth
// flag. The shape axis is open — it has grown twice — so it is a table.
//
//   - tool results — by far the most common. One measured session had 3 real
//     inputs and 11 tool results, all of them type:"user". Excluded not here but
//     by ccMessage.text returning only `text` blocks, so nothing has to name
//     tool_result for it to be kept out.
//   - injected meta content (IsMeta) — cc's `<local-command-caveat>` preamble
//     and the expanded body of a slash command. Measured 14 of 14 carrying the
//     flag, so it is a reliable signal for that.
//   - sub-agent chatter (IsSidechain).
//
// This comment used to claim the rule was "what did the user say, rather than a
// blacklist the next block type escapes". Two batches have escaped it since —
// `<local-command-stdout>` in tether#92's review, then the four in tether#95 —
// so the claim is stated as what it is: three structural exclusions, plus a
// table of text shapes that is explicitly NOT asserted to be complete.
func (e *ccEntry) isUserTurn() bool {
	return e.Type == "user" && !e.IsMeta && !e.IsSidechain
}

// userText is what the user typed, and "" for everything that merely wears
// type:"user" — the structural impostors isUserTurn knows about, and the machine
// output ccClassifyUserText recognises by shape.
//
// # This is the ONE place that answers "what is this user record?"
//
// Returning "" here rather than filtering in each caller keeps ONE definition of
// "what the user said" — the title scan and the transcript reader both consume
// it, and a second opinion in one of them is how a turn ends up in the list but
// not in the transcript, or the reverse. tether#95 moved the isUserTurn call in
// here for exactly that reason: both callers used to make it themselves, so
// "what counts as a user turn" was spread across three functions and grew a new
// condition every time cc invented another way to talk to itself.
func (e *ccEntry) userText() string {
	if !e.isUserTurn() {
		return ""
	}
	_, text := ccClassifyUserText(e.Message.text())
	return text
}

type ccMessage struct {
	Role string `json:"role"`
	// Content is a string on some records and an array of blocks on others, so it
	// is held raw and decoded by text() rather than typed here — a struct field
	// can only be one of them, and picking either would silently drop half the
	// transcript.
	Content json.RawMessage `json:"content"`
}

// text flattens a message's content to the words in it.
//
// Blocks that are not `text` — tool_use, tool_result, thinking, image — are
// skipped. That single rule is what excludes tool results from titles: a tool
// result is a content block of type tool_result on a record whose type is
// "user", so there is nothing to blacklist, there is simply no text in it.
func (m *ccMessage) text() string {
	if m == nil || len(m.Content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return ""
	}
	var b strings.Builder
	for _, bl := range blocks {
		if bl.Type != "text" || bl.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(bl.Text)
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Tool activity (tether#96)
// ---------------------------------------------------------------------------

// ccContentBlock is one content block, decoded for the tool activity in it.
//
// Separate from the anonymous struct text() uses, and deliberately so: text() is
// on the TITLE path, where it runs over every record of a 128 KiB preamble, and
// widening it would make it copy every tool input and result it walks past just to
// discover there are no words in them. This one is only decoded for a line that
// already tested positive for a tool block.
type ccContentBlock struct {
	Type string `json:"type"`
	// tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	// tool_result
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// blocks decodes a message's content as an array of blocks, or nil when it is a
// plain string — which is a shape a tool block never arrives in, so nil is the
// right answer rather than a case to handle.
func (m *ccMessage) blocks() []ccContentBlock {
	if m == nil || len(m.Content) == 0 {
		return nil
	}
	var blocks []ccContentBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil
	}
	return blocks
}

// toolCalls is the tool activity in one assistant record.
//
// A block with no name is skipped: the row renders `{name}` and an icon looked up
// by it, so a nameless call is a blank line with a 🔧 next to it.
//
// Several blocks in one record is the parallel-call shape and is normal — cc emits
// one record per batch, not per call.
func (m *ccMessage) toolCalls() []ToolCallRecord {
	var out []ToolCallRecord
	for _, b := range m.blocks() {
		if b.Type != "tool_use" || b.Name == "" {
			continue
		}
		out = append(out, ToolCallRecord{
			ID:    b.ID,
			Name:  b.Name,
			Input: ccProjectToolInput(b.Input),
		})
	}
	return out
}

// ccToolError is a call that FAILED: which call, and what it said.
//
// The type names the decision. A successful result is not carried at all — see
// ccMessagesFrom for the two reasons (bytes, and a truncated result making
// summarizeToolResult report a wrong line count), and note the consequence: a call
// with no Result covers both "it succeeded" and "the run was interrupted before it
// came back". The renderer draws those identically anyway — an absent result and an
// empty successful one both leave the row unexpandable — so nothing is lost that
// could have been shown.
type ccToolError struct {
	toolUseID string
	content   string
}

// errorResults is the FAILED results reported by one record.
//
// A result with no tool_use_id is skipped rather than guessed at: there is nothing
// to hang it on, and hanging it on the most recent call would attach a failure to a
// call that may well have succeeded.
//
// # A failure with no MESSAGE is still served
//
// cc can write `is_error` with content that flattens to nothing — an empty array, or
// only `image` / `tool_reference` sub-blocks. Serving it is deliberate: dropping the
// result drops the only signal that the call failed, and "the agent tried X and it
// broke" is the thing that explains what it did next. A failed call that reads as a
// successful one is the worse outcome.
//
// It reaches the reader as the row's "error" preview, which summarizeToolResult
// derives from the FLAG rather than from the text (web/src/panes/chat/index.tsx). So
// an empty message costs the REASON the call failed but not the fact of it, which is
// the half this decision is about.
//
// tether#96 shipped it at the price of a dead click, because ToolCallList's
// `hasResult` then admitted `|| isError` and offered to expand into a blank block;
// tether#97 narrowed that flag to non-whitespace content, which keeps the preview and
// drops the empty expander. TestFailureWithNoMessageStillReportsTheError pins the
// choice on this side. Measured: 41 failures served across the 39 windows of one
// store and 0 of them empty, so this is a decision about a shape cc CAN write rather
// than one it does — and the census counts them, so the day it starts, the report
// says so.
func (m *ccMessage) errorResults() []ccToolError {
	var out []ccToolError
	for _, b := range m.blocks() {
		if b.Type != "tool_result" || !b.IsError || b.ToolUseID == "" {
			continue
		}
		out = append(out, ccToolError{
			toolUseID: b.ToolUseID,
			content:   ccCapBytes(ccResultText(b.Content), ccToolErrorBytes),
		})
	}
	return out
}

// ccResultText flattens a tool result's content to the words in it.
//
// cc writes it in exactly the two encodings a MESSAGE's content uses — a plain
// string (2,650 of 3,887 measured) or an array of blocks (1,237, carrying `text`,
// `tool_reference` and `image` sub-blocks) — so this is ccMessage.text's rule, not
// a second one. Reusing it rather than restating it is deliberate: a copy is how
// the two drift and one of them ends up serving a JSON array as if it were prose.
func ccResultText(raw json.RawMessage) string {
	return (&ccMessage{Content: raw}).text()
}

// ccProjectToolInput reduces a call's arguments to a bounded summary of them.
//
// # What survives, and why the rule is generic
//
// Every top-level value that IS a string, truncated to ccToolInputValueRunes, at
// most ccToolInputMaxKeys of them. Objects, arrays and numbers are dropped
// entirely; Write's whole-file `content` and Edit's old/new strings become
// prefixes.
//
// The renderer reads exactly ONE top-level string field per known tool
// (TOOL_ARG_FIELD in web/src/panes/chat/index.tsx), so copying that map into Go
// would be cheaper still. It is deliberately NOT copied: that would couple the
// daemon to a display choice and rot the first time a tool is added on one side
// only. "A bounded prefix of each string argument" is a contract this file can keep
// on its own, and it costs 5 KB per transcript more than the copied map would.
//
// nil for a non-object input, an input with no string values, or anything that
// fails to decode. nil is right rather than lossy: ToolCallRecord.Input is
// omitempty and the renderer already shows the tool name alone when there is no arg
// to show.
//
// The keys are sorted before the count cap is applied, so WHICH keys survive is
// deterministic rather than a function of Go's map iteration order. (The marshal
// would sort them anyway; that sorts the output, not the choice.)
func ccProjectToolInput(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	kept := make(map[string]string, len(keys))
	for _, k := range keys {
		if len(kept) == ccToolInputMaxKeys {
			break
		}
		var s string
		if json.Unmarshal(fields[k], &s) != nil {
			continue
		}
		kept[k] = ccCapRunes(s, ccToolInputValueRunes)
	}
	if len(kept) == 0 {
		return nil
	}
	b, err := json.Marshal(kept)
	if err != nil {
		return nil
	}
	return b
}

// ccCapRunes truncates to at most max runes, marking the cut with an ellipsis.
//
// Runes rather than bytes because the bound exists to preserve a 60-CHARACTER
// summary line: a byte bound would cut a CJK argument at a third of the characters
// an ASCII one keeps, i.e. it would bind hardest exactly where the transcript that
// prompted this change is written.
func ccCapRunes(s string, max int) string {
	n := 0
	for i := range s {
		if n == max {
			return s[:i] + "…"
		}
		n++
	}
	return s
}

// ccCapBytes truncates to at most max bytes, never mid-rune, and marks the cut.
//
// A byte bound here rather than a rune one because this one exists to bound the
// RESPONSE, and bytes are what a response is measured in. Cutting back to a rune
// boundary matters because the result goes out as JSON: a half-encoded rune would
// be re-encoded as U+FFFD and the user would read a replacement character where
// their error message was cut.
//
// The boundary walk itself lives in truncateAtRuneBoundary (history.go), which
// tether#120 extracted so that the three caps on the native history path — which
// were all cutting mid-rune — get the same behaviour from the same code rather
// than from a second copy of this loop. RecordToolResult calls this function
// whole, since its cap wants the marker too.
func ccCapBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return truncateAtRuneBoundary(s, max) + ccTruncated
}

// ---------------------------------------------------------------------------
// The shape axis: what a type:"user" record's text turns out to be
// ---------------------------------------------------------------------------

// ccUserShape is one recognised form the TEXT of a type:"user" record can take,
// and what a human should read in its place.
//
// # Why this is a table and not a fifth if-statement
//
// cc feeds its own machinery back into the conversation as user turns, and the
// set of things it feeds back GROWS. Four batches have been found so far, each
// by someone noticing markup in a bubble:
//
//	tether#92         tool results (excluded by block type, see ccMessage.text)
//	tether#92         cc's isMeta preamble
//	tether#92 review  <local-command-stdout>
//	tether#95         <task-notification>, <bash-input>, <bash-stdout>, and cc's
//	                  interrupt marker — 433 records, 12.9% of every "user
//	                  message" this reader emitted, across 39 real transcripts
//
// The first three were each fixed by adding one more condition beside userText.
// That is why there was a fourth batch to find. So the shape axis is a table with
// one row per form: a fifth batch is a ROW, in one place, next to the evidence
// for the rows already there.
//
// The rows are also the vocabulary the census reports in (see
// TestEnumerateUserRecordShapes), so "which shapes does this reader know about"
// has exactly one answer and a report cannot drift from the code that acts on it.
//
// # This list is NOT asserted to be complete
//
// It is what a census of 39 real transcripts found on 2026-08-17. Every earlier
// version of this reasoning was also complete when it was written. The census is
// shipped as a test precisely so batch five is found by RUNNING something rather
// than by a user reporting a bubble full of angle brackets.
type ccUserShape struct {
	// name labels the shape in the census report. It is cc's own literal, so the
	// report names something greppable in cc's output rather than a word invented
	// here.
	name string
	// matches recognises the shape. Its argument has leading and trailing space
	// already removed, so a row cannot forget to do that.
	matches func(trimmed string) bool
	// render returns what a human should read, or "" to emit nothing at all. Its
	// argument is the same trimmed text matches saw.
	render func(trimmed string) string
}

// The literals cc writes, with the census that justifies each row. Named
// constants rather than inline strings because the fixtures assert against these
// same names — a fixture that spelled a tag out itself would keep passing after a
// typo here turned that row off.
const (
	// ccCommandStdoutPrefix — a slash command's OUTPUT, fed back in as if the
	// user had said it. Carries no isMeta (0 of 12 when found in tether#92; 195
	// records in the #95 census), and its content is a plain string, so neither
	// the flag nor the block filter can see it. Shape is the only signal left.
	ccCommandStdoutPrefix = "<local-command-stdout>"

	// ccTaskNotificationPrefix — the harness telling cc that a background task
	// finished, delivered as a user turn. 364 records: 84% of everything the #95
	// census found, and the reason this came before making tool cards readable.
	//
	// What made them the reporter's symptom rather than merely ugly is that they
	// sit BETWEEN two assistant fragments — 355 of the 364 do — so each one also
	// blocked a merge tether#94 would otherwise have made. Dropping all 386 of the
	// records this table drops joins 358 pairs of fragments, measured by replaying
	// the store through ccMessagesFrom before and after: 2,736 assistant messages
	// become 2,378. (358 rather than 355 because two notifications landing between
	// the same pair register as neither in the per-record count, and the store is
	// appended to while it is measured.)
	ccTaskNotificationPrefix = "<task-notification>"

	// ccBashInputPrefix / ccBashInputSuffix — the user running a shell command
	// with cc's `!` prefix. 22 records, every one of the form
	// `<bash-input> cmd</bash-input>`. This one the user really did do; see
	// ccRenderBashInput for why it is kept.
	ccBashInputPrefix = "<bash-input>"
	ccBashInputSuffix = "</bash-input>"

	// ccBashStdoutPrefix / ccBashStderrPrefix — that command's OUTPUT, which is
	// not something the user said. 22 records, every one of them
	// `<bash-stdout>…</bash-stdout><bash-stderr>…</bash-stderr>` in that order,
	// so stdout always leads. stderr is registered as a leader too, on the
	// MECHANISM rather than on an observation: 0 of the 22 led with it, and if a
	// command that writes only to stderr ever does, the answer is the same. Said
	// plainly because "measured" and "reasoned" are not the same warrant.
	ccBashStdoutPrefix = "<bash-stdout>"
	ccBashStderrPrefix = "<bash-stderr>"

	// ccInterruptPrefix — cc's marker for the user hitting interrupt. 25 records
	// at the first census and 26 by the end of the change, in two forms:
	// `[Request interrupted by user]` (23) and `[Request interrupted by user for
	// tool use]` (2). In every one of them that marker is the ENTIRE text, which
	// is what lets ccIsInterrupt anchor both ends. Prose rather than markup, so no
	// tag-shaped rule would ever have caught it. The prefix stops before the
	// qualifier so both forms match one row.
	ccInterruptPrefix = "[Request interrupted by user"

	// ccCommandNameTag / ccCommandMessageTag — a slash command the user typed. The
	// row that KEEPS it is the negative control for every row above; see
	// ccUserShapes. Both openings are needed and neither is optional: of the 221
	// records in the census carrying <command-name>, 206 open on it and 15 open on
	// <command-message> — and all 221 open on one of the two, which is what lets
	// that row be anchored like every other.
	ccCommandNameTag    = "<command-name>"
	ccCommandMessageTag = "<command-message>"
)

// ccUserShapes is the whole shape axis. First match wins.
//
// # The last row is what proves the others are not a blanket rule
//
// A slash command is ALSO recorded as markup, and it IS a user turn — 206
// records in the census, plus 15 more opening on `<command-message>`. So "drop
// the records that start with a tag" is wrong, and #95's first enumeration
// proved it by counting those 221 as defects before it re-measured through this
// function. "Drop the records that CONTAIN a tag" is wrong too: genuine human
// messages in the same census CONTAIN `<svg>`, `<div>` and `<style>`, because
// people paste code. (An earlier draft of this sentence said they "open with"
// those tags. They do not — the census, which tests exactly that, reports zero
// emitted messages opening on an unrecognised tag across all 93 transcripts. The
// figure it was written from counted tags appearing ANYWHERE in the text. Same
// mistake as the one three paragraphs up, in the comment warning about it.)
//
// So every row names one specific form and says what to do with that form, and
// there is no rule about angle brackets in general. Two of the six rows KEEP the
// record, because the user really did those things and dropping them would be
// this reader deleting the user's own conversation.
//
// # Every row is anchored, and the two kept rows match the WHOLE record
//
// Rows 1-3 and 6 match a prefix; rows 4 and 5 match the entire text, end to end.
// The difference matters for the rows that RENDER rather than drop, because a
// renderer that keeps part of a record has to decide what to do with the rest,
// and "silently discard it" would be deleting user text — the exact defect this
// change exists to remove, one level down. Instead the shape simply does not
// match: `<bash-input>ls</bash-input> and then I typed this` is not the shape, so
// it falls through to the human-text row and is emitted whole, as it was before
// #95, and the census reports it as an unrecognised leading tag.
//
// Measured warrant for anchoring, re-taken over all 93 top-level transcripts of
// the store rather than the one workspace: 22 of 22 bash-input records, 26 of 26
// interrupt markers and 221 of 221 command records are exactly the shape their row
// claims. Ratios rather than counts, because this store is one the user is
// appending to WHILE it is measured — the interrupt count moved from 25 to 26
// during this change, and the emitted-message total moved five times. Every
// absolute in this file is a snapshot; the invariant is the fraction.
//
// Order: first match wins, and because every row is anchored on a distinct
// opening, all six are mutually exclusive — verified by moving row 6 to the front
// and re-classifying a task notification that quotes a command inside its
// <result>, which still classifies as the notification.
//
// That is worth stating as an outcome of anchoring rather than as a fact about
// tables. An earlier version of this paragraph claimed the same thing while row 6
// matched <command-name> ANYWHERE: back then, moving it up really did render that
// notification as `/x`, and review proved it by doing so. First-match-wins is
// documented so that adding a row stays a decision about position.
//
// # The residual risk, and the discriminator that turned out not to exist
//
// Matching a PREFIX means a human who pastes one of these records verbatim as
// the very first thing in a message loses it, where before tether#95 they would
// at least have seen their own markup.
//
// The obvious way out was to require the record's content to be a plain JSON
// string, on the theory that cc injects strings while a human's message arrives
// as text blocks. Measured over the census, that is false in BOTH directions:
// 2,713 genuine human turns carry plain-string content, and the interrupt marker
// — one of the two shapes kept here — is the only one of the six that arrives as
// a text-block array, 25 of 25. A filter built on it would have been wrong about
// humans and would have silently switched itself off for interrupts. So the risk
// stands, narrowed to "the message BEGINS with the exact tag", and is written
// down here rather than designed around on a theory nobody checked.
var ccUserShapes = []ccUserShape{
	{name: ccTaskNotificationPrefix, matches: ccOpensWith(ccTaskNotificationPrefix), render: ccDropRecord},
	{name: ccCommandStdoutPrefix, matches: ccOpensWith(ccCommandStdoutPrefix), render: ccDropRecord},
	{name: ccBashStdoutPrefix, matches: ccOpensWith(ccBashStdoutPrefix, ccBashStderrPrefix), render: ccDropRecord},
	{name: ccBashInputPrefix, matches: ccIsBashInput, render: ccRenderBashInput},
	{name: ccInterruptPrefix, matches: ccIsInterrupt, render: ccRenderInterrupt},
	{name: ccCommandNameTag, matches: ccIsSlashCommand, render: ccRenderCommand},
}

// ccShapeHumanText is ccClassifyUserText's answer for a record that matched no
// row: the user's own words, whatever happens to be in them.
const ccShapeHumanText = ""

// ccClassifyUserText answers "what shape is this user record's text, and what
// should a human read?" — the shape half of userText, and the only place that
// knows.
//
// It returns the matching row's name alongside the text so that a caller which
// needs to REPORT what it saw and a caller which needs to DISPLAY it cannot
// disagree. The census is the caller that needs the name; without it, "which
// shapes does this reader know about" would be answered by reading the table with
// a human eye, and a count taken that way is exactly how #95's first enumeration
// scored 221 correctly-handled slash commands as defects.
//
// Text that matched no row comes back UNCHANGED, whitespace included: a human's
// message is not this function's to reformat.
func ccClassifyUserText(raw string) (shape, text string) {
	trimmed := strings.TrimSpace(raw)
	for _, s := range ccUserShapes {
		if s.matches(trimmed) {
			return s.name, s.render(trimmed)
		}
	}
	return ccShapeHumanText, raw
}

// ccOpensWith builds a matcher for a shape cc writes at the very START of the
// record.
//
// Prefix rather than Contains, and that distinction is the entire safety margin
// of this table: `<task-notification>` mentioned inside a sentence a human wrote
// — this one, for instance — is a human's words, and a Contains rule would delete
// the message it appears in.
func ccOpensWith(prefixes ...string) func(string) bool {
	return func(trimmed string) bool {
		for _, p := range prefixes {
			if strings.HasPrefix(trimmed, p) {
				return true
			}
		}
		return false
	}
}

// ccDropRecord emits nothing: the record is machinery, not conversation.
func ccDropRecord(string) string { return "" }

// ccBashInputRe and ccInterruptRe match the two KEPT shapes end to end.
//
// Built from the constants with QuoteMeta rather than spelled out, so the pattern
// and the literal the fixtures use cannot drift apart — which is the whole reason
// those literals are constants.
//
// The capture is the part that survives rendering. Anchoring the far end is what
// makes "nothing was discarded" a property of the match rather than a promise in
// a comment: see ccUserShapes.
var (
	ccBashInputRe = regexp.MustCompile(`(?s)^` + regexp.QuoteMeta(ccBashInputPrefix) +
		`(.*)` + regexp.QuoteMeta(ccBashInputSuffix) + `$`)
	ccInterruptRe = regexp.MustCompile(`^` + regexp.QuoteMeta(ccInterruptPrefix) + `([^\]]*)\]$`)
)

// ccIsBashInput reports whether the record is nothing but a `!` shell command.
func ccIsBashInput(trimmed string) bool { return ccBashInputRe.MatchString(trimmed) }

// ccIsInterrupt reports whether the record is nothing but cc's interrupt marker.
func ccIsInterrupt(trimmed string) bool { return ccInterruptRe.MatchString(trimmed) }

// ccRenderBashInput renders `<bash-input> ls -la</bash-input>` as `! ls -la`.
//
// # Kept, not dropped, and that is this row's judgement
//
// The user really did run that command — they typed `!` at cc's prompt to do it.
// It is as much a user turn as anything they said, so it stays, and it keeps
// closing an assistant run because a real user action genuinely ends the turn
// before it. 22 of these silently disappearing would be the failure mode a
// blanket "drop the tagged records" rule ships: the reader deleting the user's
// own conversation and no test noticing.
//
// Rendered with the `!` the user actually typed, so the bubble reads as the
// action rather than as a quotation of it, and in PLAIN text with no markdown:
// the pane renders a user bubble as {m.text} rather than through <Markdown>
// (web/src/panes/chat/index.tsx), so backticks would show up as backticks.
//
// Only the whole-record form matches (ccIsBashInput), so there is never a
// remainder to discard — a record with anything after the closing tag is not this
// shape and is emitted as it stands. An empty command emits nothing: there is no
// action to show, and no real record has one.
func ccRenderBashInput(trimmed string) string {
	m := ccBashInputRe.FindStringSubmatch(trimmed)
	if m == nil {
		// Unreachable through the table, which matches before it renders. Returning
		// the text rather than "" so that a future caller which skips the match
		// cannot make this function delete a record.
		return trimmed
	}
	if cmd := strings.TrimSpace(m[1]); cmd != "" {
		return "! " + cmd
	}
	return ""
}

// ccRenderInterrupt renders cc's `[Request interrupted by user]` as
// `(interrupted)`.
//
// # Kept, not dropped: the user pressed the key
//
// 19 of the 25 in the census sit directly between the answer they cut off and
// whatever they typed next, so this record is the only thing that explains why
// that answer stops mid-sentence. Dropping it would silently rewrite history into
// an agent that simply trailed off.
//
// Parenthesised, with cc's third-person framing removed, because it renders in a
// bubble already labelled "you": "Request interrupted by user" reads as a system
// line quoted AT the user, and the parentheses say "this is an action, not
// something they said".
//
// The qualifier — cc writes `[Request interrupted by user for tool use]` in 2 of
// the 25 — is carried through VERBATIM rather than interpreted. What cc means by
// it has not been read out of cc, and inventing a reading here is how a comment
// in this file ends up asserting something the code does not do.
//
// A record that carries anything after the marker's `]` is NOT this shape
// (ccIsInterrupt anchors both ends) and is emitted whole instead, so the user's
// words cannot be thrown away with the marker. That matters more here than
// anywhere else in the table: this is the one shape cc writes as a text-block
// array, and ccMessage.text joins blocks with "\n", so a second block is the one
// structurally reachable way for a real message to arrive attached to a marker.
// An earlier version discarded it and pointed at the census as the safety net —
// which could not see it, because a matched shape is never a candidate.
func ccRenderInterrupt(trimmed string) string {
	m := ccInterruptRe.FindStringSubmatch(trimmed)
	if m == nil {
		return trimmed // unreachable through the table; see ccRenderBashInput
	}
	if q := strings.TrimSpace(m[1]); q != "" {
		return "(interrupted " + q + ")"
	}
	return "(interrupted)"
}

var (
	ccCommandNameRe = regexp.MustCompile(`<command-name>([^<]*)</command-name>`)
	ccCommandArgsRe = regexp.MustCompile(`<command-args>([^<]*)</command-args>`)
)

// ccOpensSlashCommand is the anchor half of ccIsSlashCommand, built once rather
// than per call.
var ccOpensSlashCommand = ccOpensWith(ccCommandNameTag, ccCommandMessageTag)

// ccIsSlashCommand reports whether the record IS cc's slash-command markup, with
// a command actually in it.
//
// Two conditions, and both earn their place:
//
// It must OPEN on one of cc's two command tags. Until #95 this row matched
// <command-name> anywhere, which meant a human sentence containing the tag —
// "please add <command-name>/foo</command-name> to the docs" — was replaced
// wholesale by "/foo", deleting everything they wrote around it. That is the same
// defect as the four this change is about, in the row held up as proof that the
// others are not a blanket rule. Anchoring costs nothing: all 221 records in the
// census open on one of the two tags.
//
// And ccSlashCommand must find a command, so the table cannot claim a shape that
// ccRenderCommand then declines to render. If this matched on the tag's mere
// PRESENCE, a malformed record would be reported as a known shape while rendering
// as raw markup — a defect the census would then be unable to see, because it
// only reports records that matched NO row. Falling through makes it a candidate.
func ccIsSlashCommand(trimmed string) bool {
	if !ccOpensSlashCommand(trimmed) {
		return false
	}
	_, ok := ccSlashCommand(trimmed)
	return ok
}

// ccRenderCommand turns cc's slash-command markup back into the command the user
// typed, and returns text with no command in it unchanged.
//
// A slash command is recorded as
//
//	<command-message>pf-work</command-message>
//	<command-name>/polyforge:pf-work</command-name>
//	<command-args>silgrid#123</command-args>
//
// which, passed through untouched, renders as a row of visible XML — in 5 of the
// 38 transcripts measured for tether#92. Left alone that is 13% of a list whose
// entire purpose is to be readable. This deliberately does not strip tags in
// general, because the moment it did, a prompt that legitimately contains markup
// would be silently rewritten.
func ccRenderCommand(text string) string {
	if cmd, ok := ccSlashCommand(text); ok {
		return cmd
	}
	return text
}

// ccSlashCommand extracts the command a slash-command record stands for.
//
// ok is false when there is no <command-name> or it is empty — the two cases in
// which there is nothing better to show than the text itself.
func ccSlashCommand(text string) (string, bool) {
	name := ccCommandNameRe.FindStringSubmatch(text)
	if name == nil {
		return "", false
	}
	cmd := strings.TrimSpace(name[1])
	if cmd == "" {
		return "", false
	}
	if args := ccCommandArgsRe.FindStringSubmatch(text); args != nil {
		if a := strings.TrimSpace(args[1]); a != "" {
			return cmd + " " + a, true
		}
	}
	return cmd, true
}
