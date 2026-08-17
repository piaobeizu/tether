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
// store by accident" a property of the API rather than of test discipline. It is
// a property of THIS TYPE, not of the package: tether#92 also deleted
// jsonl_sync.go, which was dead code that reached the same store through
// os.UserHomeDir and, worse, encoded the path wrongly
// (projects/<sid>/<sid>.jsonl instead of projects/<encoded-cwd>/<sid>.jsonl) —
// a second, silently incorrect answer to the question this file exists to answer
// once. If another such reader appears, this sentence stops being true.
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

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
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
// that this reader then drops. So the last 1 MiB of a tool-heavy transcript can
// contain no conversation at all — and the result would be a row that has a
// title, is listed, and opens an EMPTY chat. That is the original symptom this
// whole change exists to remove, reappearing one layer down. Found by review.
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
// further 941 records wear type:"user", pass isUserTurn, and are dropped anyway
// — 746 isMeta and 195 <local-command-stdout>, exactly accounted for. Counting
// them gives a flattering 4.74 assistant records per turn instead of 6.07, and
// every figure derived from it lands ~30% out. An earlier draft of this comment
// used precisely that number; a ratio used to size a cap must be measured over
// the things the cap counts. (Found by review, which is the second time in this
// file that a confidently stated measurement was taken over the wrong
// population — see EncodeProjectDir.)
//
// Merging makes the unit real: on the same store the conversation goes from
// 23,675 messages to 6,072 — one merged assistant message per turn that says
// anything (628 turns say nothing but tool calls), plus one message per thing
// the user actually said. Two consecutive user turns therefore stay TWO
// messages, which TestMessagesDoesNotMergeAcrossAUserTurn pins, so "two per
// turn" is the common case and not an invariant. 200 works out at ~110 turns of
// scrollback against the ~57 that 400 was delivering.
// TestMessagesCapCountsTurnsNotRecords pins the unit, because a doc comment
// asserting it is exactly what let the two drift apart.
//
// # The payload consequence: a ceiling this data never reaches
//
// Halving the number does not halve the response, because a surviving assistant
// message carries 3.9 records' worth of text on average instead of one. As an
// upper bound, 200 messages can hold ~780 source records' worth against the old
// 400 — call it 2x. Leaving it at 400 would have been ~4x.
//
// Measured, it is 1.00x. Replaying all 39 transcripts through the real
// ccMessagesTailBytes window, old and new both serve 0.77 MB, and the COUNT cap
// binds on 0 of 39 either way: the byte window binds first. So the ratio above
// is a ceiling for the transcript of very short lines this cap has always been
// for, not a change anyone will observe. What does change is the element count
// the JSON encoder walks — 1,039 messages become 380.
//
// What this cap no longer bounds is a SINGLE message: the longest merged
// assistant message in that store is 34 KB, from a run of 114 fragments. Only
// the byte window bounds that — the same bound as before, now spread across
// fewer elements.
const ccMessagesMax = 200

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
// transcripts under: every character outside [a-zA-Z0-9] becomes '-'.
//
// # This rule was READ OUT OF CC, not inferred from its output
//
// Stated because the first version of this function was inferred, and was wrong
// in a way no amount of sampling here could have caught. cc's own encoder is
//
//	replace(/[^a-zA-Z0-9]/g, "-")
//
// (Claude Code 2.1.233, read from the installed binary on 2026-08-17.) The first
// version mapped only '/' and '.' — which matched all 37 real project directories
// on this machine, because not one of those paths contains a character outside
// [a-zA-Z0-9/.-]. A workspace at /root/my_project would have produced
// `-root-my_project` against cc's `-root-my-project` and listed ZERO sessions:
// no error, no log line, no failing test. Underscores in a repository path are
// not exotic.
//
// The lesson generalises past this function: a claim about a third party's
// behaviour has to come from the third party, and agreement with a sample is not
// evidence when the sample cannot express the disagreement.
//
// (Exactness note: cc runs this over UTF-16 code units, so a character outside
// the BMP yields two dashes there and one here. Irrelevant for real paths, and
// the >200 branch below covers the case where it would matter to length.)
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
			b.WriteByte('-')
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
func (s *CCStore) Messages(sid string) ([]HistoryMessage, bool) {
	path := s.find(sid)
	if path == "" {
		return nil, false
	}
	f, err := os.Open(path)
	if err != nil {
		slog.Warn("cc sessions: open transcript failed", "sid", sid, "err", err)
		return nil, false
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		slog.Warn("cc sessions: stat transcript failed", "sid", sid, "err", err)
		return nil, false
	}

	msgs, err := ccReadTail(f, fi.Size(), ccMessagesTailBytes)
	if err != nil {
		slog.Warn("cc sessions: read transcript failed", "sid", sid, "err", err)
		return nil, false
	}
	// Nothing in the window converted, and the window was not the whole file: the
	// tail was all tool payload. Widen once — see ccMessagesTailBytes for why an
	// empty answer here would be the original bug wearing a different hat.
	if len(msgs) == 0 && fi.Size() > ccMessagesTailBytes {
		msgs, err = ccReadTail(f, fi.Size(), ccMessagesWideTailBytes)
		if err != nil {
			slog.Warn("cc sessions: wide read failed", "sid", sid, "err", err)
			return nil, true
		}
		slog.Debug("cc sessions: widened the transcript window", "sid", sid,
			"size", fi.Size(), "found", len(msgs))
	}
	return msgs, true
}

// ccReadTail converts the last `window` bytes of f.
func ccReadTail(f *os.File, size, window int64) ([]HistoryMessage, error) {
	if size <= window {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		return ccMessagesFrom(f), nil
	}
	if _, err := f.Seek(size-window, io.SeekStart); err != nil {
		return nil, err
	}
	br := bufio.NewReader(f)
	// The window almost certainly opens mid-line. Drop that fragment: a truncated
	// JSON object would merely fail to parse, but the point of dropping it
	// explicitly is that a fragment which happens to parse would contribute half a
	// record to the transcript the user reads.
	if _, err := br.ReadBytes('\n'); err != nil {
		return nil, nil
	}
	return ccMessagesFrom(br), nil
}

// find resolves sid to a transcript path, or "" when no known directory has one.
//
// ValidSessionID first, and not as a formality: sid arrives from the URL of
// GET /api/v1/sessions/<sid>/messages and is about to be joined into a path, so
// without the guard a `..`-shaped sid turns this into a reader for any .jsonl
// file on the host. The same reason HistoryStore.HasHistory checks it.
func (s *CCStore) find(sid string) string {
	if s == nil || s.projectsDir == "" || s.dirs == nil || !ValidSessionID(sid) {
		return ""
	}
	for _, wd := range s.dirs() {
		dir := s.resolveProjectDir(wd)
		if dir == "" {
			continue
		}
		path := filepath.Join(dir, sid+".jsonl")
		if fi, err := os.Stat(path); err == nil && !fi.IsDir() && fi.Size() > 0 {
			return path
		}
	}
	return ""
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
	if !e.isUserTurn() {
		return ""
	}
	return e.userText()
}

// ccMessagesFrom converts a cc transcript window into tether history messages.
//
// # What is dropped, and why the UI says so
//
// Only the CONVERSATION survives: user turns and the assistant's text. tool_use,
// tool_result and thinking blocks are not converted. That is not an oversight —
// tool payloads are the reason a transcript reaches 138 MB, and reconstructing
// them faithfully into ToolCallRecord is a separate piece of work with its own
// fidelity questions. The row that renders this is labelled accordingly, so the
// user is told what they are looking at rather than left to notice.
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
// remembered: the impostors (tool results, isMeta preambles, isSidechain
// chatter, <local-command-stdout>) already produce no message at all, so there
// is nothing to exclude a second time and no second list to keep in sync with
// isUserTurn's. Sidechain ASSISTANT records are dropped by the same existing
// rule, so a sub-agent speaking between two fragments does not split them
// either.
//
// Merging happens as records are read, so the cap below trims MESSAGES — see
// ccMessagesMax, whose unit this is the other half of.
func ccMessagesFrom(r io.Reader) []HistoryMessage {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReader(r)
	}
	var out []HistoryMessage
	for {
		line, err := br.ReadBytes('\n')
		// Only a terminated line is a line — the same rule ccFirstUserText uses,
		// and for the same reason. An earlier version accepted an unterminated
		// final line here while rejecting it there; real cc files end with a
		// newline, so the difference only showed up mid-append, which is exactly
		// where a half-written record that happens to parse would be surfaced to
		// the user. One rule, both readers.
		if bytes.HasSuffix(line, []byte("\n")) {
			if m, ok := ccMessageFromLine(line); ok {
				// Still the same turn: cc opened a new record to call a tool, not
				// because the agent stopped talking. Append rather than emit, and
				// keep the FIRST fragment's stamp — that is when the response
				// began, which is also what tether's own path records for an
				// assistant message (history.go stamps the accumulator when it is
				// created, not when it is flushed).
				if n := len(out); n > 0 && m.Role == "assistant" && out[n-1].Role == "assistant" {
					out[n-1].Text += ccTurnJoin + m.Text
				} else {
					out = append(out, m)
				}
			}
		}
		if err != nil {
			break
		}
	}
	// Newest turns win when there are more than the cap: the tail is what the
	// window was chosen to preserve, so trimming from the front keeps that
	// decision consistent instead of silently reversing it.
	if len(out) > ccMessagesMax {
		out = out[len(out)-ccMessagesMax:]
	}
	return out
}

func ccMessageFromLine(line []byte) (HistoryMessage, bool) {
	if !bytes.Contains(line, []byte(`"user"`)) && !bytes.Contains(line, []byte(`"assistant"`)) {
		return HistoryMessage{}, false
	}
	var e ccEntry
	if err := json.Unmarshal(line, &e); err != nil {
		return HistoryMessage{}, false
	}
	var role, text string
	switch {
	case e.isUserTurn():
		role, text = "user", e.userText()
	case e.Type == "assistant" && !e.IsSidechain:
		role, text = "assistant", e.Message.text()
	default:
		return HistoryMessage{}, false
	}
	if text == "" {
		return HistoryMessage{}, false
	}
	return HistoryMessage{Role: role, Text: text, Ts: ccTimestampMillis(e.Timestamp)}, true
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

// isUserTurn reports whether this record is something the human typed.
//
// FOUR different impostors wear type:"user", and a fixture that omits any one of
// them proves nothing about that one. Three are excluded here; the fourth is
// excluded in userText, because it is only recognisable from the text:
//
//   - tool results — by far the most common. One measured session had 3 real
//     inputs and 11 tool results, all of them type:"user". Excluded by
//     ccMessage.text returning only `text` blocks, so nothing has to name
//     tool_result for it to be kept out.
//   - injected meta content (IsMeta) — cc's `<local-command-caveat>` preamble.
//     Measured 14 of 14 carrying the flag, so it is a reliable signal for that.
//   - sub-agent chatter (IsSidechain).
//   - command OUTPUT (`<local-command-stdout>`) — see userText. This one was
//     missed by the first version of this file and found by review: measured 31
//     of 1146 served turns (2.7%), 24 of them carrying raw ANSI escape
//     sequences, and NONE of them isMeta or isSidechain. The comment here used
//     to claim the rule was "what did the user say, rather than a blacklist the
//     next block type escapes" — this is the thing that escaped it, so the claim
//     is now stated as what it actually is: three structural exclusions plus one
//     by shape.
func (e *ccEntry) isUserTurn() bool {
	return e.Type == "user" && !e.IsMeta && !e.IsSidechain
}

// ccCommandStdoutPrefix opens the record cc writes when it feeds a slash
// command's OUTPUT back into the conversation as if the user had said it.
//
// It carries no isMeta (measured 0 of 12), so the flag cannot catch it, and its
// content is a plain string, so the block filter cannot either. Shape is the only
// signal left.
const ccCommandStdoutPrefix = "<local-command-stdout>"

// userText is what the user typed: "" for a record that is a command's output
// rather than a human's turn, and cc's slash-command markup rendered back into
// the command it stands for.
//
// Returning "" here rather than filtering in each caller keeps ONE definition of
// "what the user said" — the title scan and the transcript reader both consume
// it, and a second opinion in one of them is how a turn ends up in the list but
// not in the transcript, or the reverse.
func (e *ccEntry) userText() string {
	t := e.Message.text()
	if strings.HasPrefix(strings.TrimSpace(t), ccCommandStdoutPrefix) {
		return ""
	}
	return ccRenderCommand(t)
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

var (
	ccCommandNameRe = regexp.MustCompile(`<command-name>([^<]*)</command-name>`)
	ccCommandArgsRe = regexp.MustCompile(`<command-args>([^<]*)</command-args>`)
)

// ccRenderCommand turns cc's slash-command markup back into the command the user
// typed.
//
// A slash command is recorded as
//
//	<command-message>pf-work</command-message>
//	<command-name>/polyforge:pf-work</command-name>
//	<command-args>silgrid#123</command-args>
//
// which, passed through untouched, renders as a row of visible XML — in 5 of the
// 38 transcripts measured. Left alone that is 13% of a list whose entire purpose
// is to be readable. Text with no <command-name> is returned unchanged; this
// deliberately does not strip tags in general, because the moment it did, a
// prompt that legitimately contains markup would be silently rewritten.
func ccRenderCommand(text string) string {
	name := ccCommandNameRe.FindStringSubmatch(text)
	if name == nil {
		return text
	}
	cmd := strings.TrimSpace(name[1])
	if cmd == "" {
		return text
	}
	if args := ccCommandArgsRe.FindStringSubmatch(text); args != nil {
		if a := strings.TrimSpace(args[1]); a != "" {
			return cmd + " " + a
		}
	}
	return cmd
}
