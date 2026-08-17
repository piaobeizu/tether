package session

// The readable session list (tether#91).
//
// HistoryStore.ListSessions answers "which sids have a transcript" and nothing
// else, which was enough while the list rendered `sid.slice(0,16)…` in a corner
// of the file tree. It is not enough for a list a human is expected to choose
// from: a column of UUID prefixes in an order nobody can predict is a list you
// scroll past, not one you read.
//
// This file adds the two things that make a row mean something — a LABEL and a
// TIME — and it is a separate file from history.go on purpose: history.go owns
// what was said in a session, and answering "what is this session about" needs
// the two sidecars as well (wi.json, workspace.json). Reaching across all three
// is this type's job, not the transcript store's.
//
// # The ordering was not merely absent, it was wrong
//
// The pre-tether#91 list was `os.ReadDir` order — which is sorted by FILENAME,
// and the filenames are UUIDs — with the browser applying `[...sessions].reverse()`
// on top. So it looked like "newest first" and was in fact reverse-lexicographic
// order of random strings. The fix has to be a real timestamp, and a test that
// only checks "some order" would pass against that bug, so the ordering fixture
// deliberately arranges for time order to be neither the name order nor its
// reverse.

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"unicode"
)

// SessionSummary is one row of the session list.
//
// Everything except Sid is best-effort: a summary is a label for a human, and a
// session whose sidecars are missing or whose transcript starts with something
// unexpected must still be listable and clickable. Sid is the only field the UI
// needs to act (it is what openSession takes).
type SessionSummary struct {
	Sid string `json:"sid"`
	// WorkItem is the opaque label from wi.json, empty when the session has no
	// binding. The UI prefers it over Title because it is the only field a human
	// deliberately attached to this session.
	WorkItem string `json:"workItem,omitempty"`
	// Title is a fallback description derived from the session itself: the first
	// thing the user said, else the directory the session runs in. Empty when
	// neither could be read, in which case the UI falls back to the sid.
	Title string `json:"title,omitempty"`
	// UpdatedAt is the transcript's mtime in Unix milliseconds — see SessionIndex.List
	// for why mtime and not a message timestamp.
	UpdatedAt int64 `json:"updatedAt"`
	// Source names the store this row's transcript came from: SourceTether for
	// tether's own, SourceCC for a conversation cc recorded that tether never saw.
	//
	// Always populated, never omitempty. An absent field would make "tether" and
	// "this daemon predates the field" the same value, and the one consumer that
	// matters — a log line added in the same change to end exactly that kind of
	// blindness — would print `source=`. There is no compatibility argument
	// against spending the bytes: the daemon and the SPA ship as one binary
	// (web/embed.go), so no frontend can be talking to a daemon that disagrees.
	//
	// It is a FACT about where the row came from, and that is why it is the only
	// new field. The question the UI actually wants answered is "can I carry on
	// with this one", and tether cannot answer it: cc's `--resume` reports failure
	// only after a prompt has been delivered (Attachment's doc), so a resumability
	// flag computed here would be a guess that goes stale whenever
	// --workspace-root or the workspace registry moves. A row that is right most
	// of the time is the lying list this feature exists to avoid; source never
	// goes stale, and the UI states the limit rather than predicting it.
	Source string `json:"source"`
}

// SessionIndex answers "what sessions are there, and what is each one about?".
//
// It holds the three stores rather than being a method on any one of them because
// the answer genuinely needs all three, and because that keeps every field
// optional: a daemon assembled without a wi store, or without workspace bindings,
// still produces a list — just with fewer fallbacks filled in. Tests construct it
// with only the stores the case is about.
type SessionIndex struct {
	History  *HistoryStore
	WI       *WIBindingStore
	Bindings *BindingStore
	// CC is the reader over cc's own store (tether#92). nil = this daemon lists
	// only the sessions it recorded itself, which is what every daemon did before
	// that slice — the field is optional for the same reason the two above are.
	CC *CCStore
}

// titlePrefixBytes bounds how much of a transcript List reads looking for the
// first user turn.
//
// The bound that matters is not one transcript, it is N of them: this runs once
// per session on EVERY list request, so the ceiling is sessions × this, not this.
// At the ~90 sessions a real profile has, 16 KiB is ~1.4 MB of (page-cached)
// reading per request; 64 KiB would be four times that for no extra answers,
// because the first user turn is the FIRST LINE of an ordinary transcript and a
// prompt does not become more findable the further you read.
//
// A transcript itself can be tens of megabytes (one assistant turn is capped at
// MaxAssistantBufBytes = 4 MiB and nothing caps the number of turns), so an
// unbounded read is not an option at any N. A session whose opening turn is
// larger than this loses its title fallback — not its row.
const titlePrefixBytes = 16 << 10

// maxTitleLen caps the derived title. The UI truncates for layout anyway; this
// stops a pasted wall of text from becoming a 4 MiB JSON field on the wire.
const maxTitleLen = 80

// List returns every session that has a transcript, newest first.
//
// # Which sessions
//
// Two stores, ONE list (tether#92). tether's own transcripts, plus the
// conversations cc recorded in the directories this daemon knows about. They are
// merged rather than grouped or served from a second endpoint because the sid
// space is shared — 55 of one profile's 90 tether sids had a byte-identical cc
// file — so two rows for one sid would be two rows for one conversation, and the
// "current session" highlight would land on one of them arbitrarily.
//
// Where both stores have a sid, TETHER WINS and the row is labelled
// SourceTether. That is the conservative half of the merge: it leaves the
// daemon's daily path — the
// transcript the owner's live session reads — byte-for-byte what it was. The cost
// is stated rather than hidden: for those overlapping sessions tether shows its
// own shorter record (137 lines against cc's 919, over one measured profile).
// Preferring the fuller transcript there is a real question and a separate one;
// it is not this change's to decide, because getting it wrong regresses the path
// that already works.
//
// # Which sessions (tether's own)
//
// The same rule ListSessions uses — a directory with a NON-EMPTY history.jsonl —
// and for the same reason its doc gives: BindingStore (and now WIBindingStore)
// create <sid>/ before any message exists, so enumerating directories would list
// sessions that never said anything, each rendering as a clickable row with an
// empty transcript.
//
// Names that fail ValidSessionID are skipped. Not defence against an attacker —
// these come from ReadDir, not from a request — but consistency: a sid this
// daemon would refuse to serve /messages for has no business appearing in a list
// whose rows are meant to be clicked.
//
// # Why mtime and not a message timestamp
//
// Every history line carries `ts`, so "the last message's ts" is available and
// would be the more literal answer. It costs a full read and JSON parse of every
// transcript on every request, where mtime costs a stat that HasHistory is
// already doing. The two agree in practice because HistoryStore.append is the
// only writer of that file and it appends on every finalized turn — that is what
// makes mtime true, and it is a different statement from "nothing else could ever
// touch the file": copying ~/.tether with a tool that does not preserve mtimes
// would perturb it. The field is therefore named UpdatedAt (when this file last
// changed) rather than LastMessageAt.
func (x *SessionIndex) List() []SessionSummary {
	if x == nil {
		return nil
	}
	var out []SessionSummary
	// Which sids tether has already answered for. Consulted by the cc pass below,
	// which is why the tether pass runs first and not merely by habit.
	seen := make(map[string]bool)

	if x.History != nil {
		entries, err := os.ReadDir(x.History.baseDir)
		if err != nil && !os.IsNotExist(err) {
			slog.Warn("session list: read base dir failed", "base_dir", x.History.baseDir, "err", err)
		}
		out = make([]SessionSummary, 0, len(entries))
		for _, e := range entries {
			sid := e.Name()
			if !e.IsDir() || !ValidSessionID(sid) {
				continue
			}
			// One stat answers both questions — "is there a conversation here" and
			// "when was it last written". HasHistory would ask the first one with a
			// stat of its own; calling it here and then stat-ing again for the mtime
			// would double the syscalls in the hot loop for no extra information.
			fi, err := os.Stat(x.History.historyPath(sid))
			if err != nil || fi.Size() == 0 {
				continue
			}
			seen[sid] = true
			s := SessionSummary{Sid: sid, UpdatedAt: fi.ModTime().UnixMilli(), Source: SourceTether}
			s.Title = x.title(sid)
			out = append(out, s)
		}
	}

	// The cc pass. Rows tether already has are dropped here rather than merged
	// field-by-field, which is the "tether wins" rule stated once — see this
	// function's doc for what that costs and why it is the safe half.
	if x.CC != nil {
		for _, s := range x.CC.List() {
			if seen[s.Sid] {
				continue
			}
			seen[s.Sid] = true
			out = append(out, s)
		}
	}

	// The work item is attached AFTER both passes, over every row, because a
	// binding is a note a human attached to a SESSION and knows nothing about
	// which store the transcript ended up in. Doing it inside the tether loop
	// (where it used to live) would have made "bind a wi to a session, see the
	// label" work for half the list and silently not the other half.
	if x.WI != nil {
		for i := range out {
			if b, ok := x.WI.Load(out[i].Sid); ok {
				out[i].WorkItem = b.WorkItem
			}
		}
	}
	if len(out) == 0 {
		return out
	}

	// Newest first, sid as the tie-break. The tie-break is not cosmetic: two
	// sessions written in the same millisecond (or on a filesystem with coarse
	// mtime granularity) would otherwise land in ReadDir order, and the list would
	// reorder itself between two requests that returned the same data.
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt != out[j].UpdatedAt {
			return out[i].UpdatedAt > out[j].UpdatedAt
		}
		return out[i].Sid < out[j].Sid
	})
	return out
}

// Messages returns one session's transcript and names the store it came from.
//
// # Why this is a method on the index and not two calls in the HTTP handler
//
// "Which store answers for this sid" is ONE rule, and List above is the other
// place that applies it. Written twice — once here, once as a route that reaches
// for HistoryStore first and falls back — the two would be free to drift, and the
// symptom of drift is a row that says it came from one store while its transcript
// comes from the other. That is not a hypothetical failure mode in this
// repository; lib/session.ts and lib/SessionRow.tsx both exist because a rule
// stated at three call sites had already become three rules. So the rule is
// stated once, and List and this function are the two readings of it.
//
// The predicate is HasHistory — a non-empty history.jsonl — because that is
// almost exactly the predicate List uses to decide a session is tether's.
// Anything weaker (e.g. "LoadHistory returned something") would let a session
// appear as tether's in the list and be served from cc here.
//
// "Almost", and the gap is named rather than hidden: List additionally requires
// the session's directory entry to satisfy DirEntry.IsDir, which is false for a
// SYMLINK to a directory, while HasHistory stats through it. So a symlinked
// session directory whose sid cc also has is listed as cc's and served from
// tether's. Narrow enough to leave alone — changing List's rule would change
// pre-existing behaviour for a case nothing creates — but it is a real exception
// to "these two cannot disagree", and an absolute claim with a known exception is
// worse than a precise one.
//
// SourceNone with a nil slice means neither store has it. That is not an error:
// openSession fetches this route for a session that was created moments ago and
// has not spoken yet, and the answer is an empty transcript.
func (x *SessionIndex) Messages(sid string) ([]HistoryMessage, string) {
	if x == nil {
		return nil, SourceNone
	}
	if x.History != nil && x.History.HasHistory(sid) {
		return x.History.LoadHistory(sid), SourceTether
	}
	if x.CC != nil {
		if msgs, ok := x.CC.Messages(sid); ok {
			return msgs, SourceCC
		}
	}
	return nil, SourceNone
}

// title derives a human-readable description of a session from the session
// itself: what the user opened with, else where the session runs.
//
// Deliberately NOT the work item — that is List's job and it takes precedence
// there. Keeping them apart means a session that later loses its binding still
// has something to show, and means this function has one input (the session) and
// no opinion about the UI's preference order.
//
// Applies to TETHER's sessions only. A cc row's title comes from CCStore, which
// has no workspace binding to fall back on, so a cc session whose opening turn is
// past ccTitlePrefixBytes renders as a bare sid rather than as a directory. That
// is correct — the fallback describes where a session tether SPAWNED is running,
// and tether did not spawn these — but it does mean this function no longer
// describes every row in the list.
func (x *SessionIndex) title(sid string) string {
	if t := x.firstUserText(sid); t != "" {
		return t
	}
	if x.Bindings != nil {
		if b, ok := x.Bindings.Load(sid); ok {
			return b.Path
		}
	}
	return ""
}

// firstUserText returns a one-line summary of the first thing the user said, or
// "" when the leading prefix of the transcript contains no user turn.
func (x *SessionIndex) firstUserText(sid string) string {
	f, err := os.Open(x.History.historyPath(sid))
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("session list: open transcript failed", "sid", sid, "err", err)
		}
		return ""
	}
	defer f.Close()

	// io.ReadAll over a LimitReader, not a single Read: one Read may legally
	// return fewer bytes than asked for, which would silently shorten the window
	// this function searches.
	data, err := io.ReadAll(io.LimitReader(f, titlePrefixBytes))
	if err != nil {
		slog.Warn("session list: read transcript failed", "sid", sid, "err", err)
		return ""
	}
	if len(data) == 0 {
		return ""
	}

	// The prefix almost certainly ends mid-line. Drop the trailing fragment unless
	// the read stopped exactly on a newline — parsing a truncated JSON object would
	// merely fail, but a fragment that happens to parse would be a title assembled
	// from half a record.
	if !bytes.HasSuffix(data, []byte("\n")) {
		if i := bytes.LastIndexByte(data, '\n'); i >= 0 {
			data = data[:i+1]
		} else {
			// One line longer than the whole prefix: nothing complete to read.
			return ""
		}
	}

	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		// Decode into the two fields that matter. A corrupt line is skipped rather
		// than aborting: LoadHistory already treats individual bad lines that way,
		// and a title is the least important thing in the file.
		var m struct {
			Role string `json:"role"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(line, &m); err != nil {
			continue
		}
		if m.Role != "user" {
			continue
		}
		if t := condense(m.Text); t != "" {
			return t
		}
	}
	return ""
}

// condense flattens a prompt into a single short line: whitespace runs (including
// the newlines of a multi-line prompt) collapse to one space, and the result is
// cut to maxTitleLen runes.
//
// Cut by RUNE, not by byte, so a CJK prompt is not truncated into invalid UTF-8 —
// which would then have to be escaped by the JSON encoder and would render as
// replacement characters.
func condense(s string) string {
	fields := strings.FieldsFunc(s, unicode.IsSpace)
	if len(fields) == 0 {
		return ""
	}
	out := strings.Join(fields, " ")
	r := []rune(out)
	if len(r) > maxTitleLen {
		return string(r[:maxTitleLen]) + "…"
	}
	return out
}
