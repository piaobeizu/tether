package session

// Reading cc's LIVE-SESSION registry (tether#101).
//
// This is cc's second store, and a different question from the one ccsessions.go
// answers. That file reads <cc-config-dir>/projects — what conversations exist
// and what was said in them. This one reads <cc-config-dir>/sessions/<pid>.json —
// which cc PROCESSES are running right now and which session id each of them
// holds.
//
// # Why tether has to read it at all
//
// cc refuses a uuid `--resume` outright when a live NON-INTERACTIVE cc process
// holds that session id. Read verbatim out of the installed binary
// (/root/.local/share/claude/versions/2.1.233, on 2026-08-18):
//
//	async function g3e(e){ let t = await (…).listAllLiveSessions().catch(()=>[]);
//	  for (let r of t) if (r.sessionId===e && r.pid!==process.pid && r.kind && r.kind!=="interactive")
//	    return {kind:r.kind, jobId:r.jobId};
//	  return null }
//	// and at the call site, when the caller did not pass --fork-session:
//	//   `Error: Session ${sid} is currently running as a background agent (${kind}). `
//	//   + "Use `claude agents` to find and attach to it, or add --fork-session to branch off a copy."
//	//   then exit 1
//
// It writes that to stderr and exits 1 BEFORE emitting system/init, so from
// tether's side it is indistinguishable from "the transcript is gone" — both
// arrive as SessionID() == "". Attachment.resolve answered both the same way: it
// spawned a fresh session and handed the user an empty conversation with no
// explanation. This reader is what lets it tell them apart. See
// Attachment.resolve for what it does with the answer, and SessionSummary's
// RunningAs for the weaker use the session list makes of it.
//
// # It is READ ONLY, for exactly the reason ccsessions.go is
//
// Nothing here writes, creates, renames, truncates or deletes. cc's own
// listAllLiveSessions DOES unlink records it can prove are stale, and this reader
// deliberately does not join in: it is the user's live process bookkeeping, and a
// daemon that is only trying to answer a question has no business editing it.
//
// The directory is taken as an argument and nothing in this file calls
// os.UserHomeDir — the same rule CCStore follows, for the same reason (no test
// can reach the real store by accident, as a property of the API rather than of
// test discipline). internal/server/lifecycle.go does the resolving, from the
// SAME CLAUDE_CONFIG_DIR read that resolves CCProjectsDir, so the two stores
// cannot end up pointing at two different cc installs.
//
// # This file is Linux-shaped, and the degradation is the safe direction
//
// Liveness is one read of /proc/<pid>/stat (see ccProcStartToken). On a host
// without /proc — or for a pid whose /proc entry is not readable (hidepid), or a
// daemon in a different pid namespace from cc — every record reads as NOT live,
// so this reader answers "nothing is holding anything" and the daemon behaves
// exactly as it did before tether#101. That is the direction that cannot do
// harm: reading a live holder as dead costs the pre-existing silent fallback,
// while reading a dead holder as live would refuse a resume that would have
// worked, i.e. make a session unopenable. Everything here is therefore built to
// fail towards "not live".

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// CCSessionsDir names cc's live-session registry inside a cc config directory.
//
// Split out for the same reason CCProjectsDir is: the caller that knows the home
// directory resolves it, and this package never calls os.UserHomeDir. An empty
// config dir yields "" rather than "/sessions" — a reader over the filesystem
// root is not a sensible default for "we could not work out where cc keeps its
// things".
func CCSessionsDir(ccConfigDir string) string {
	if ccConfigDir == "" {
		return ""
	}
	return filepath.Join(ccConfigDir, "sessions")
}

// ccRegistryRecordBytes bounds how much of one registry record this reader will
// read.
//
// The ceiling that matters is records × this, because the session list scans the
// whole directory on every request (see CCRegistry.LiveJobs). Measured on the
// reference machine on 2026-08-18: 138 records, 44,936 bytes in total, LARGEST
// RECORD 501 bytes, median 316. 64 KiB is therefore ~130x the largest real
// record and bounds a corrupt or hostile file, while the worst case it permits
// (138 × 64 KiB = 8.6 MiB) is only reachable by a directory full of files that are
// not cc session records.
//
// cc reads its own records with a 262,144-byte cap. Ours is smaller because the
// cost model is different: cc consults this directory occasionally, the session
// list consults it once per HTTP request.
//
// A record larger than this fails to parse and is ignored, which reads as "no
// live holder" — the fail-towards-not-live rule this file's doc states.
const ccRegistryRecordBytes = 64 << 10

// ccProcStatStartField is the index, among the fields that follow "<state> " in
// /proc/<pid>/stat, of the process start time — field 22 of the file, counting
// comm as field 2. cc reads exactly this: `stat.slice(stat.lastIndexOf(")") + 2)
// .split(" ")[19]`.
const ccProcStatStartField = 19

// ccRegistryFileName is the record-name rule, and it is cc's: only <pid>.json,
// nothing else in the directory.
var ccRegistryFileName = regexp.MustCompile(`^\d+\.json$`)

// ccRecordPid extracts the pid a record is about, from its FILE NAME.
//
// # The name is the pid, and the `pid` field inside the record is not
//
// cc builds each record as `{...parsedFields, pid: s, file: a}` where `s` is
// `parseInt(basename)` — so the number in the NAME is the pid its own liveness
// check uses, and the `pid` field in the JSON body is never read. (Read from the
// binary, 2.1.233, 2026-08-18. They agree on every record of the reference
// machine — all 138 were named exactly `<body pid>.json` — which is precisely why
// reading the body instead would have looked correct forever and then answered
// differently from cc the first time they disagreed.) Nothing here reads the body
// field at all; there is no second opinion to keep in sync.
//
// The round-trip check is cc's too, and it is not cosmetic: `parseInt` accepts
// "007" and "12abc"-shaped input that the regexp above already excludes, but
// "007.json" gets through the regexp and would otherwise be read as pid 7. cc
// unlinks such a file; this reader skips it, because it does not write.
func ccRecordPid(name string) (int, bool) {
	base := strings.TrimSuffix(name, ".json")
	pid, err := strconv.Atoi(base)
	if err != nil || strconv.Itoa(pid) != base {
		return 0, false
	}
	return pid, true
}

// ccInteractiveKind is the one kind that does NOT make cc refuse a resume.
//
// The check is `kind != "" && kind != ccInteractiveKind`, and it is written that
// way — rather than as `kind == "bg"` — because cc validates the field against
// FOUR values, read from the same binary: ["interactive","bg","daemon",
// "daemon-worker"]. Only two of them appear on the reference machine (124
// interactive, 14 bg on 2026-08-18), so a `== "bg"` implementation would agree
// with every record that exists here and silently fail to recognise a daemon or
// daemon-worker holding a session. Agreement with a sample is not evidence when
// the sample cannot express the disagreement — the same lesson EncodeProjectDir
// records one file over.
//
// An EMPTY kind is not a holder either, and that is cc's rule too (`r.kind &&`):
// a record written by a cc old enough not to have the field says nothing about
// what kind of process it is, and this reader may not guess.
const ccInteractiveKind = "interactive"

// CCLiveJob is what cc's registry says about the process holding a session id:
// the kind of process it is, and the job it belongs to when it has one.
//
// Both fields come straight out of the record — they are quotations, not
// derivations — which is what lets the refusal message name the same kind and
// jobId cc's own error would have named.
type CCLiveJob struct {
	// Kind is cc's `kind`: "bg", "daemon" or "daemon-worker". Never
	// "interactive", because such a record is not a holder (see ccInteractiveKind).
	Kind string
	// JobID is cc's `jobId`, empty when the record carries none. It is what
	// `claude agents` shows, so it is the handle a user needs to go and find the
	// thing that is holding their conversation.
	JobID string
}

// CCRegistry reads cc's live-session registry. The zero value lists nothing; see
// NewCCRegistry.
type CCRegistry struct {
	// dir is <cc-config-dir>/sessions. Required, never derived here — see the file
	// doc for why there is no default.
	dir string
}

// NewCCRegistry builds a reader over dir.
//
// An empty dir yields a reader that reports nothing live, which is the correct
// behaviour for a daemon that cannot tell where cc keeps its registry: it means
// every caller behaves as it did before tether#101 rather than as if the world
// were empty of background jobs (those are the same answer here, which is the
// point of choosing this default).
func NewCCRegistry(dir string) *CCRegistry {
	return &CCRegistry{dir: dir}
}

// LiveJobs returns every session id currently held by a live non-interactive cc
// process, mapped to what the registry says about the holder.
//
// One scan per call, and the session list calls it once per request rather than
// once per row — see SessionIndex.List. Measured cost of a full scan on the
// reference machine (138 records, 138 /proc reads, page-cached): ~3.0 ms and
// 44 KB, against the ~1.4 MB of transcript prefixes the same request already
// reads. 5 of those 138 records were live non-interactive sessions, so the map
// this returns is small even where the directory is not.
//
// Where two live records claim the same sid the first one scanned wins the map
// value. That only decides which kind and jobId are reported, never WHETHER the
// sid is held, so nothing that matters depends on directory order.
func (s *CCRegistry) LiveJobs() map[string]CCLiveJob {
	out := make(map[string]CCLiveJob)
	s.forEachLive(func(sid string, job CCLiveJob) bool {
		if _, seen := out[sid]; !seen {
			out[sid] = job
		}
		return true
	})
	return out
}

// LiveJob reports whether sid is held right now, and by what.
//
// # Every record for the sid is considered, not the first one found
//
// One session id accumulates MANY records — measured on the reference machine on
// 2026-08-18, 7 of 103 distinct sids had more than one, the worst had 26, and the
// three sids with a live holder each had 3 records of which exactly 1 was live.
// So the answer is an OR over the records for that sid, and a reader that stopped
// at the first record it matched would answer "not held" for precisely the
// sessions this exists to recognise. Expressed as an early return on the first
// LIVE match, which is that OR, short-circuited.
func (s *CCRegistry) LiveJob(sid string) (CCLiveJob, bool) {
	if sid == "" {
		return CCLiveJob{}, false
	}
	var found CCLiveJob
	var ok bool
	s.forEachLive(func(recSid string, job CCLiveJob) bool {
		if recSid != sid {
			return true
		}
		found, ok = job, true
		return false
	})
	return found, ok
}

// forEachLive walks the registry and calls fn for every record that describes a
// LIVE, NON-INTERACTIVE cc session. fn returns false to stop the walk.
//
// It exists so that "what counts as a live holder" is stated ONCE and read twice
// — the same arrangement SessionIndex.List and SessionIndex.Messages use for
// "which store answers for this sid", and for the same reason: written twice, the
// two would be free to drift, and the symptom of drift is a session list that
// marks a row the attach path then resumes without complaint (or the reverse,
// which is the lie this whole change is about).
func (s *CCRegistry) forEachLive(fn func(sid string, job CCLiveJob) bool) {
	if s == nil || s.dir == "" {
		return
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		// A machine where cc has never run has no such directory. That is the
		// ordinary case, not a problem worth a log line — the same treatment
		// CCStore.List gives a workspace cc has never been used in.
		if !os.IsNotExist(err) {
			slog.Warn("cc registry: read sessions dir failed", "dir", s.dir, "err", err)
		}
		return
	}
	for _, e := range entries {
		if e.IsDir() || !ccRegistryFileName.MatchString(e.Name()) {
			continue
		}
		pid, ok := ccRecordPid(e.Name())
		if !ok {
			continue
		}
		rec, ok := s.readRecord(e.Name())
		if !ok {
			continue
		}
		// The sid is only ever compared and used as a map key here — it is never
		// joined into a path — so ValidSessionID is deliberately NOT applied. It is
		// the gate CCStore.find needs because a sid arrives there from a URL and
		// becomes a filename; applying it here would instead mean that if cc ever
		// changes its id format, this reader silently stops recognising holders.
		// Non-empty is the whole requirement: a record with no session id says
		// nothing about any session.
		if rec.SessionID == "" || rec.Kind == "" || rec.Kind == ccInteractiveKind {
			continue
		}
		if !ccPidHoldsRecord(pid, rec.ProcStart) {
			continue
		}
		if !fn(rec.SessionID, CCLiveJob{Kind: rec.Kind, JobID: rec.JobID}) {
			return
		}
	}
}

// readRecord decodes one record, or reports that it could not be read as one.
func (s *CCRegistry) readRecord(name string) (ccRegistryRecord, bool) {
	f, err := os.Open(filepath.Join(s.dir, name))
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("cc registry: open record failed", "dir", s.dir, "name", name, "err", err)
		}
		return ccRegistryRecord{}, false
	}
	defer f.Close()
	// LimitReader, not a size check then a read: cc appends to these files while
	// its processes run, so the bound has to be on what is READ. A record longer
	// than the cap is cut mid-JSON and fails to decode, which is the intended
	// outcome — see ccRegistryRecordBytes.
	data, err := io.ReadAll(io.LimitReader(f, ccRegistryRecordBytes))
	if err != nil {
		slog.Warn("cc registry: read record failed", "dir", s.dir, "name", name, "err", err)
		return ccRegistryRecord{}, false
	}
	var rec ccRegistryRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		// A record cc is halfway through writing, or one this build cannot parse.
		// Skipped rather than fatal, the same treatment every other reader in this
		// package gives one bad line.
		return ccRegistryRecord{}, false
	}
	return rec, true
}

// ccRegistryRecord is the part of one registry record this package understands.
//
// cc writes far more than this — pid, cwd, startedAt, version, peerProtocol,
// entrypoint, name, nameSource, nameSince, formerNames, agent, status,
// waitingFor, updatedAt, statusUpdatedAt, logPath, tmux, messagingSocketPath —
// and adds fields over time. Decoding only the three that decide "is a live
// non-interactive process holding this session, and which one" means a new field
// costs nothing and an unknown one cannot be misread.
//
// `pid` is deliberately absent even though the record carries one: the pid comes
// from the FILE NAME, which is where cc's own liveness check takes it from. See
// ccRecordPid.
type ccRegistryRecord struct {
	SessionID string `json:"sessionId"`
	// ProcStart is cc's snapshot of the pid's start time, as a decimal string of
	// clock ticks. It is what makes a pid NUMBER into a process IDENTITY — see
	// ccPidHoldsRecord.
	ProcStart string `json:"procStart"`
	Kind      string `json:"kind"`
	JobID     string `json:"jobId"`
}

// ccPidHoldsRecord reports whether the process this record was written by is
// still the process running under that pid.
//
// # Two questions, and the second one is not optional
//
// "Is pid N running" is not "is pid N still the process that wrote this record".
// Pids are recycled, and on this machine 132 of 138 records referred to pids that
// had already exited (oldest 3.2 days, median 0.8) — so the population of records
// whose pid could have been reused by something unrelated is the large majority of
// the directory. Worse in a container: the same pid NUMBER in a different pid
// namespace is a different process, and it is a process that exists. procStart is
// what separates those cases, and it is why cc records it.
//
// # It mirrors cc, including where cc is lenient
//
// cc asks kill(pid, 0) first and then compares tokens, and treats a MISSING
// recorded token as a match ("if (procStart === undefined) return true"). This
// keeps that leniency — a record from a cc old enough not to write the field is
// judged on its pid alone — because tightening it would silently stop recognising
// holders written by such a build, and the field's absence is not evidence of
// anything.
//
// Where it deliberately differs: cc also treats an UNREADABLE live token as a
// match, and this does not. One /proc read answers both questions here, so a
// failure to read it means neither question was answered, and the file doc's rule
// applies — fail towards "not live", because that costs the pre-existing silent
// fallback whereas the other direction makes a session unopenable.
func ccPidHoldsRecord(pid int, procStart string) bool {
	// pid <= 1 is cc's own floor (fE) and it is not decoration: pid 1 is the
	// container's init, which is always alive and never a cc session, and pid 0 is
	// not a process at all.
	if pid <= 1 {
		return false
	}
	token, ok := ccProcStartToken(pid)
	if !ok {
		return false
	}
	if procStart == "" {
		return true
	}
	return procStart == token
}

// ccProcStartToken returns the start-time token of a running pid, and whether the
// pid could be read at all.
//
// The parse is cc's, field for field: take everything after the LAST ')' plus one
// space, split on spaces, and read index ccProcStatStartField. Working back from
// the last ')' rather than forward from the start is what makes it correct for a
// process whose comm contains a space or a bracket — comm is field 2 and is
// printed in parentheses unescaped, so anything that counted fields from the left
// would be reading a different column for such a process.
//
// The token is returned as the raw string rather than parsed to a number, because
// the only thing done with it is compare it to the string cc wrote. Parsing would
// add a way to be wrong (and cc's own comparison is also string equality) and buy
// nothing.
func ccProcStartToken(pid int) (string, bool) {
	// Not os.ReadFile via fmt.Sprintf: strconv keeps this off the fmt path, which
	// matters only because this runs once per record per session-list request.
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		// Includes the ordinary case by far — the process has exited — so no log
		// line. Also includes /proc being absent or unreadable, which the file doc
		// covers: it reads as "not live", deliberately.
		return "", false
	}
	end := -1
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == ')' {
			end = i
			break
		}
	}
	if end < 0 || end+2 > len(b) {
		return "", false
	}
	fields := b[end+2:]
	start, n := 0, 0
	for i := 0; i <= len(fields); i++ {
		if i == len(fields) || fields[i] == ' ' {
			if n == ccProcStatStartField {
				if i == start {
					return "", false
				}
				return string(fields[start:i]), true
			}
			n++
			start = i + 1
		}
	}
	return "", false
}
