package session

// tether#101 — the reader over cc's live-session registry.
//
// Every fixture here is a directory this test made. NOTHING in this file reads
// the real ~/.claude — that is the user's live process bookkeeping, and CCRegistry
// takes its directory as an argument precisely so that staying out of it is a
// property of the API rather than of anyone's discipline.
//
// # Liveness is REAL here, not injected
//
// A "live pid" fixture uses os.Getpid() and the token this process's own
// /proc/<pid>/stat carries, and a "dead pid" fixture uses the pid of a child that
// has already been reaped. So these tests exercise ccProcStartToken's actual
// parse against the actual kernel, which is the half of the rule most likely to
// be wrong and the half a mocked prober would never check.
//
// The cost is that they are Linux-shaped and skip elsewhere — the same trade
// registry_test.go's zombie test already makes.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// ccRegFixture builds a registry directory and writes records into it.
type ccRegFixture struct {
	dir string
}

func newCCRegFixture(t *testing.T) *ccRegFixture {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir registry: %v", err)
	}
	return &ccRegFixture{dir: dir}
}

func (f *ccRegFixture) reg() *CCRegistry { return NewCCRegistry(f.dir) }

// write files one record under <pid>.json, the name cc uses.
func (f *ccRegFixture) write(t *testing.T, rec map[string]any) {
	t.Helper()
	pid, ok := rec["pid"]
	if !ok {
		t.Fatal("fixture record has no pid; the file name is <pid>.json")
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	name := fmt.Sprintf("%v.json", pid)
	if err := os.WriteFile(filepath.Join(f.dir, name), b, 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// writeRaw files arbitrary bytes under an arbitrary name, for the shapes a
// well-formed record cannot express.
func (f *ccRegFixture) writeRaw(t *testing.T, name string, body []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), body, 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// requireLinux skips a test that needs /proc to answer truthfully.
func requireLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("liveness is one read of /proc/<pid>/stat; there is nothing to assert without /proc")
	}
}

// liveToken is this process's own start token — the value a record must carry to
// be judged live for os.Getpid().
func liveToken(t *testing.T) string {
	t.Helper()
	tok, ok := ccProcStartToken(os.Getpid())
	if !ok {
		t.Fatalf("could not read this process's own start token from /proc/%d/stat", os.Getpid())
	}
	if tok == "" {
		t.Fatal("this process's start token parsed as empty")
	}
	return tok
}

// deadPid runs a child to completion and returns its pid, which is then a pid
// nothing is running under.
//
// Reaped rather than merely signalled: an unwaited child is a zombie, and a
// zombie still HAS a /proc/<pid>/stat, so it would read as live and this fixture
// would be asserting the opposite of what it says. (registry_test.go's own
// zombie helper exists because that distinction has already mattered once in
// this package.)
func deadPid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start throwaway child: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait throwaway child: %v", err)
	}
	if _, ok := ccProcStartToken(pid); ok {
		t.Skipf("pid %d was recycled before the assertion could run", pid)
	}
	return pid
}

// bgRecord is the shape cc writes for a background job: kind "bg", entrypoint
// "cli". Measured on the reference machine 2026-08-18 — all 14 bg records carried
// entrypoint "cli", and all 124 interactive ones carried "sdk-cli".
func bgRecord(pid int, sid, procStart string) map[string]any {
	return map[string]any{
		"pid": pid, "sessionId": sid, "procStart": procStart,
		"cwd": "/w", "kind": "bg", "entrypoint": "cli",
		"jobId": "job-" + sid, "status": "busy", "version": "2.1.233",
	}
}

// interactiveRecord is the shape TETHER'S OWN spawned cc registers as: kind
// "interactive", entrypoint "sdk-cli".
//
// Re-measured for tether#101 rather than taken from the wi, because everything
// downstream of it depends on it: cc was launched with tether's exact argv
// (--print --output-format stream-json --input-format stream-json --verbose, see
// ClaudeCodeProvider.Spawn) into a throwaway config dir on 2026-08-18, and the
// record it wrote for itself carried kind "interactive" / entrypoint "sdk-cli" /
// agent "claude". Corroborated across the whole reference profile: all 124
// sdk-cli records are interactive, all 14 cli records are bg.
func interactiveRecord(pid int, sid, procStart string) map[string]any {
	return map[string]any{
		"pid": pid, "sessionId": sid, "procStart": procStart,
		"cwd": "/w", "kind": "interactive", "entrypoint": "sdk-cli",
		"version": "2.1.233",
	}
}

// TestCCRegistryLiveJob_TheThreeRecordShapes is the heart of tether#101's
// classification, and two of its three rows are the ones that matter most.
//
// The first row is the feature. The other two are the ANTI-MISLABEL guards, and
// they guard against a failure that INVERTS the fix rather than merely losing it:
// a reader that answers "held" for a record it should not turns a session into one
// the daemon refuses to open at all.
//
// The consequence has to be counted in SIDS, not in records, and an earlier draft
// of this comment got that wrong in the flattering direction — it said "132 of 138
// records" (the number of records with dead pids), which is a fact about the whole
// directory and not about this rule. Measured on the reference profile
// (2026-08-18), the population this rule actually decides is the NON-INTERACTIVE
// records: 14 of them, spread over 7 distinct sids, of which 5 sids are live. So
// dropping the liveness check mismarks 2 sids TODAY. (The wi's "9 sessions" is the
// same error one step smaller: 9 is the count of dead bg RECORDS, and they collapse
// onto 2 sids.)
//
// Two today is worth a guard anyway, and the reason is not the count but WHY the
// count moves in one direction only: cc's sweep of stale records is structurally
// disabled in tether's environment (Docker, sandbox and non-interactive launch all
// disable it — see CCRegistry's file doc for the gate), so the dead half
// accumulates for as long as the machine runs background jobs, while the live half
// is bounded by how many run at once. 9 of those 14 records were already dead, the
// oldest 3.2 days old, median 0.8. Left unchecked, this rule does not merely stay
// slightly wrong, it gets steadily wronger.
//
// The interactive row is not a count at all, and must not be read as one: it is the
// shape TETHER'S OWN spawned agent registers as, so answering "held" for it would
// refuse the daemon's own sessions. One live interactive record on this profile
// today; the population is every session tether has open.
func TestCCRegistryLiveJob_TheThreeRecordShapes(t *testing.T) {
	requireLinux(t)
	tok := liveToken(t)
	dead := deadPid(t)
	self := os.Getpid()

	for _, tc := range []struct {
		name     string
		rec      map[string]any
		sid      string
		wantHeld bool
		wantKind string
	}{
		{
			name:     "live pid + kind bg is held",
			rec:      bgRecord(self, "sid-live-bg-00001", tok),
			sid:      "sid-live-bg-00001",
			wantHeld: true,
			wantKind: "bg",
		},
		{
			name: "dead pid + kind bg is NOT held",
			// The residue case, and the residue never gets collected — cc's sweep is off
			// in this environment (CCRegistry's file doc has the gate). 132 of the 138
			// records on the reference machine referred to pids that had exited (oldest
			// 3.2 days, median 0.8) — that figure is about the whole directory; the ones
			// this rule decides are the 9 dead NON-INTERACTIVE records, over 2 sids. cc
			// lets such a resume through (measured: it proceeds to "No conversation found
			// with session ID:" or succeeds), so tether must too.
			rec:      bgRecord(dead, "sid-dead-bg-00001", "1"),
			sid:      "sid-dead-bg-00001",
			wantHeld: false,
		},
		{
			name: "live pid + kind interactive is NOT held",
			// A session open in a terminal does not block a resume, and neither does
			// one tether spawned itself — both register interactive.
			rec:      interactiveRecord(self, "sid-live-inter-01", tok),
			sid:      "sid-live-inter-01",
			wantHeld: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCCRegFixture(t)
			f.write(t, tc.rec)

			job, held := f.reg().LiveJob(tc.sid)
			if held != tc.wantHeld {
				t.Fatalf("LiveJob(%q) held = %v, want %v", tc.sid, held, tc.wantHeld)
			}
			if !tc.wantHeld {
				if job != (CCLiveJob{}) {
					t.Errorf("LiveJob(%q) returned %+v alongside held=false; a caller reading the value without the bool must see nothing", tc.sid, job)
				}
				// The same record must also leave the LIST unmarked, or B would badge a
				// row A is about to resume without complaint.
				if jobs := f.reg().LiveJobs(); len(jobs) != 0 {
					t.Errorf("LiveJobs() = %v, want empty for a record that is not a holder", jobs)
				}
				return
			}
			if job.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", job.Kind, tc.wantKind)
			}
			if want := "job-" + tc.sid; job.JobID != want {
				t.Errorf("JobID = %q, want %q — the refusal message names it, so it has to be the record's own value", job.JobID, want)
			}
			jobs := f.reg().LiveJobs()
			if len(jobs) != 1 || jobs[tc.sid].Kind != tc.wantKind {
				t.Errorf("LiveJobs() = %v, want exactly {%q: kind %q}", jobs, tc.sid, tc.wantKind)
			}
		})
	}
}

// TestCCRegistryLiveJob_TetherOwnSpawnIsNeverAHolder states the previous test's
// third row as the property it exists for, because "interactive is skipped" and
// "tether cannot block itself" are the same fact wearing two different levels of
// consequence, and only the second one explains why the rule may never be
// relaxed.
//
// tether spawns cc with --print --output-format stream-json --input-format
// stream-json --verbose, and cc registers that as kind "interactive" +
// entrypoint "sdk-cli" (measured, cc 2.1.233, 2026-08-18: 124 of 124 sdk-cli
// records were interactive). If this ever stopped holding, every tether session
// would become unresumable the moment it was still running — so the assertion is
// written against the entrypoint too, to say out loud which population it is
// protecting.
func TestCCRegistryLiveJob_TetherOwnSpawnIsNeverAHolder(t *testing.T) {
	requireLinux(t)
	f := newCCRegFixture(t)
	rec := interactiveRecord(os.Getpid(), "tether-own-000001", liveToken(t))
	if rec["entrypoint"] != "sdk-cli" {
		t.Fatalf("fixture entrypoint = %v, want sdk-cli — this test is about the shape tether's own spawn produces", rec["entrypoint"])
	}
	f.write(t, rec)

	if _, held := f.reg().LiveJob("tether-own-000001"); held {
		t.Fatal("a live kind:interactive/entrypoint:sdk-cli record was classified as a holder; that is the shape tether's OWN cc registers as, so this would refuse every session tether spawned")
	}
}

// TestCCRegistryLiveJob_DaemonKindsAreHoldersToo kills the `kind == "bg"` mutant.
//
// cc validates kind against ["interactive","bg","daemon","daemon-worker"] (read
// from the binary, 2.1.233) and refuses on `kind && kind !== "interactive"`. Only
// interactive and bg appear on the reference machine, so an implementation that
// tested for "bg" would agree with every record in existence there and still be
// wrong. This is the test the sample cannot produce.
func TestCCRegistryLiveJob_DaemonKindsAreHoldersToo(t *testing.T) {
	requireLinux(t)
	tok := liveToken(t)
	for _, kind := range []string{"daemon", "daemon-worker"} {
		t.Run(kind, func(t *testing.T) {
			f := newCCRegFixture(t)
			rec := bgRecord(os.Getpid(), "sid-"+kind+"-0001", tok)
			rec["kind"] = kind
			f.write(t, rec)

			job, held := f.reg().LiveJob("sid-" + kind + "-0001")
			if !held {
				t.Fatalf("kind %q was not classified as a holder; cc refuses for every kind that is not \"interactive\"", kind)
			}
			if job.Kind != kind {
				t.Errorf("Kind = %q, want %q — the message quotes cc's own value", job.Kind, kind)
			}
		})
	}
}

// TestCCRegistryLiveJob_EmptyKindIsNotAHolder — cc's rule is `r.kind &&`, so a
// record with no kind is not a holder. A record written by a cc old enough not to
// have the field says nothing about what kind of process it is, and guessing
// "probably background" would refuse resumes on no evidence.
func TestCCRegistryLiveJob_EmptyKindIsNotAHolder(t *testing.T) {
	requireLinux(t)
	f := newCCRegFixture(t)
	rec := bgRecord(os.Getpid(), "sid-no-kind-0001", liveToken(t))
	delete(rec, "kind")
	f.write(t, rec)

	if _, held := f.reg().LiveJob("sid-no-kind-0001"); held {
		t.Error("a record with no kind was classified as a holder")
	}
}

// deadPidsSortingBefore returns n pids that are not running AND whose decimal
// names sort before self's, so os.ReadDir (which sorts by name) visits them
// first.
//
// Ordering is the whole point: this fixture only reaches the first-record-wins
// bug if the dead records are read BEFORE the live one, and a pid handed out by
// the OS cannot be relied on to land on either side of self.
func deadPidsSortingBefore(t *testing.T, self, n int) []int {
	t.Helper()
	var out []int
	name := strconv.Itoa(self)
	for cand := self - 1; cand > 1 && len(out) < n; cand-- {
		if len(strconv.Itoa(cand)) != len(name) {
			break // a shorter number can sort after; stay inside one digit width
		}
		if _, alive := ccProcStartToken(cand); alive {
			continue
		}
		out = append(out, cand)
	}
	if len(out) < n {
		t.Skipf("could not find %d dead pids sorting before %d", n, self)
	}
	return out
}

// TestCCRegistryLiveJob_OrsOverEveryRecordForTheSid kills the
// first-record-wins mutant.
//
// Measured on the reference machine 2026-08-18: 7 of 103 distinct sids carried
// more than one record (worst: 26), and each of the three sids WITH a live holder
// carried 3 records of which exactly 1 was live — including e4d1f668, the session
// the owner actually failed to open. So a reader that answered from the first
// record it found would answer "not held" for precisely the sessions this feature
// exists to recognise.
func TestCCRegistryLiveJob_OrsOverEveryRecordForTheSid(t *testing.T) {
	requireLinux(t)
	f := newCCRegFixture(t)
	self := os.Getpid()
	dead := deadPidsSortingBefore(t, self, 2)
	sid := "sid-three-recs-1"
	for _, pid := range dead {
		f.write(t, bgRecord(pid, sid, "1"))
	}
	f.write(t, bgRecord(self, sid, liveToken(t)))

	// The fixture's own premise: the live record really is read last.
	ents, err := os.ReadDir(f.dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if got, want := ents[len(ents)-1].Name(), strconv.Itoa(self)+".json"; got != want {
		t.Fatalf("last entry ReadDir returns is %q, want %q — this test only reaches the bug if the LIVE record is read last", got, want)
	}

	job, held := f.reg().LiveJob(sid)
	if !held {
		t.Fatal("three records for one sid, one of them live, and LiveJob answered not held — the answer must be an OR over every record for the sid")
	}
	if job.Kind != "bg" {
		t.Errorf("Kind = %q, want \"bg\"", job.Kind)
	}
}

// TestCCRegistryPidComesFromTheFileNameNotTheBody pins the hop that cost this
// reader a rewrite.
//
// cc assembles each record as `{...parsed, pid: s}` where `s` is the number in
// the FILE NAME; the `pid` field inside the JSON is never read (binary 2.1.233).
// The first version of this file read the body field instead. Every record on the
// reference machine has them equal — all 138 — so no fixture drawn from real data
// could tell the two implementations apart, and the disagreement would first
// appear on a machine nobody was looking at.
//
// Two halves, because either alone passes for the wrong reason: a LIVE name with
// a dead body pid must be held, and a DEAD name with a live body pid must not.
func TestCCRegistryPidComesFromTheFileNameNotTheBody(t *testing.T) {
	requireLinux(t)
	self := os.Getpid()
	tok := liveToken(t)
	dead := deadPidsSortingBefore(t, self, 1)[0]

	t.Run("live name, dead body pid", func(t *testing.T) {
		f := newCCRegFixture(t)
		rec := bgRecord(self, "sid-name-live-001", tok)
		rec["pid"] = dead // the body lies; the name is authoritative
		b, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		f.writeRaw(t, strconv.Itoa(self)+".json", b)
		if _, held := f.reg().LiveJob("sid-name-live-001"); !held {
			t.Error("not held; the pid must come from the file name, which names a live process")
		}
	})

	t.Run("dead name, live body pid", func(t *testing.T) {
		f := newCCRegFixture(t)
		rec := bgRecord(dead, "sid-name-dead-001", "1")
		rec["pid"] = self // the body lies the other way
		rec["procStart"] = tok
		b, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		f.writeRaw(t, strconv.Itoa(dead)+".json", b)
		if _, held := f.reg().LiveJob("sid-name-dead-001"); held {
			t.Error("held; the pid must come from the file name, which names a process that is gone")
		}
	})
}

// TestCCRegistryLiveJob_ProcStartMismatchIsNotAHolder kills the mutant that
// drops the procStart comparison.
//
// A record whose pid is running but whose start token disagrees is a RECYCLED
// pid — or, in a container, the same pid number in another namespace. The pid
// exists; the process that wrote the record does not.
//
// The pool exposed to that is the large majority of the directory (132 of 138
// records on the reference profile referred to exited pids) and it can only grow,
// because cc's sweep of stale records never runs in tether's environment — see
// CCRegistry's file doc. So a reader without this comparison would start refusing
// resumes on the strength of an unrelated process, and would do it more often over
// time rather than less.
func TestCCRegistryLiveJob_ProcStartMismatchIsNotAHolder(t *testing.T) {
	requireLinux(t)
	f := newCCRegFixture(t)
	tok := liveToken(t)
	f.write(t, bgRecord(os.Getpid(), "sid-recycled-001", tok+"9"))

	if _, held := f.reg().LiveJob("sid-recycled-001"); held {
		t.Fatalf("a live pid whose start token disagrees with the record (%q vs %q) was classified as a holder; then a recycled pid refuses a resume that would have worked", tok+"9", tok)
	}
	// The same pid WITH the right token is held, so the test above is about the
	// comparison and not about the fixture failing to be live at all.
	f2 := newCCRegFixture(t)
	f2.write(t, bgRecord(os.Getpid(), "sid-recycled-001", tok))
	if _, held := f2.reg().LiveJob("sid-recycled-001"); !held {
		t.Fatal("the control case (matching token) was not held; the mismatch assertion above proves nothing")
	}
}

// TestCCRegistryLiveJob_MissingProcStartFallsBackToThePid pins the one place this
// reader is deliberately lenient, and pins it as cc's behaviour rather than as
// ours: `I1` returns true when the RECORD carries no token
// ("if (procStart === undefined) return true").
//
// Tightening it would silently stop recognising holders written by a cc old
// enough not to write the field, and the field's absence is not evidence about
// the process.
func TestCCRegistryLiveJob_MissingProcStartFallsBackToThePid(t *testing.T) {
	requireLinux(t)
	f := newCCRegFixture(t)
	rec := bgRecord(os.Getpid(), "sid-no-token-001", "")
	delete(rec, "procStart")
	f.write(t, rec)

	if _, held := f.reg().LiveJob("sid-no-token-001"); !held {
		t.Error("a record with no procStart and a live pid was not classified as a holder; cc judges such a record on its pid alone")
	}
}

// TestCCRegistryLiveJob_LowPidsAreNeverHolders — pid 1 is the container's init,
// always alive and never a cc session, and pid 0 is not a process. cc's own
// liveness check has the same floor.
func TestCCRegistryLiveJob_LowPidsAreNeverHolders(t *testing.T) {
	f := newCCRegFixture(t)
	f.write(t, bgRecord(1, "sid-init-000001", ""))
	f.write(t, bgRecord(0, "sid-zero-000001", ""))

	for _, sid := range []string{"sid-init-000001", "sid-zero-000001"} {
		if _, held := f.reg().LiveJob(sid); held {
			t.Errorf("%s was classified as a holder; pid <= 1 is never a cc session", sid)
		}
	}
}

// TestCCRegistryIgnoresEverythingThatIsNotARecord — the name rule is cc's
// (^\d+\.json$), a directory is not a record, an unparseable body is not a
// record, and a body past the read cap decodes to nothing. All of them read as
// "no holder", which is this file's fail-towards-not-live rule.
func TestCCRegistryIgnoresEverythingThatIsNotARecord(t *testing.T) {
	requireLinux(t)
	f := newCCRegFixture(t)
	tok := liveToken(t)
	self := os.Getpid()

	// Right content, wrong name: cc only reads <pid>.json.
	body, err := json.Marshal(bgRecord(self, "sid-badname-0001", tok))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	f.writeRaw(t, "session-1234.json", body)
	f.writeRaw(t, "007.json", body) // a number that does not round-trip; cc unlinks it, we skip it
	f.writeRaw(t, "1234.txt", body)
	// Right name, unparseable body.
	f.writeRaw(t, "4321.json", []byte(`{"pid":4321,"sessionId":`))
	// A directory that looks like a record.
	if err := os.MkdirAll(filepath.Join(f.dir, "5678.json"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Right name, valid JSON, but padded past the read cap so the read is cut
	// mid-object and cannot decode.
	huge := map[string]any{
		"pid": self, "sessionId": "sid-huge-000001", "procStart": tok,
		"kind": "bg", "pad": strings.Repeat("x", ccRegistryRecordBytes),
	}
	hugeBody, err := json.Marshal(huge)
	if err != nil {
		t.Fatalf("marshal huge: %v", err)
	}
	if len(hugeBody) <= ccRegistryRecordBytes {
		t.Fatalf("the oversized fixture is %d bytes, which does not exceed the %d-byte cap it is meant to exceed", len(hugeBody), ccRegistryRecordBytes)
	}
	f.writeRaw(t, fmt.Sprintf("%d.json", self+1), hugeBody)

	if jobs := f.reg().LiveJobs(); len(jobs) != 0 {
		t.Errorf("LiveJobs() = %v, want empty — none of these fixtures is a record cc would read", jobs)
	}
}

// TestCCRegistryEmptyAndMissing — a reader with no directory, a nil reader, and a
// directory that does not exist all answer "nothing is held" without panicking.
// That is the state a daemon is in when it cannot work out where cc lives, and it
// has to mean "behave as you did before tether#101".
func TestCCRegistryEmptyAndMissing(t *testing.T) {
	for _, tc := range []struct {
		name string
		reg  *CCRegistry
	}{
		{"nil reader", nil},
		{"empty dir", NewCCRegistry("")},
		{"missing dir", NewCCRegistry(filepath.Join(t.TempDir(), "nope", "sessions"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if jobs := tc.reg.LiveJobs(); len(jobs) != 0 {
				t.Errorf("LiveJobs() = %v, want empty", jobs)
			}
			if _, held := tc.reg.LiveJob("sid-anything-001"); held {
				t.Error("LiveJob reported a holder")
			}
		})
	}
}

// TestCCRegistryLiveJobEmptySid — an empty sid is not a question. Asked because
// Attachment.resolve reaches this with a.reqSID, and a connection that brought no
// sid must not be told something is holding "".
func TestCCRegistryLiveJobEmptySid(t *testing.T) {
	requireLinux(t)
	f := newCCRegFixture(t)
	rec := bgRecord(os.Getpid(), "", liveToken(t))
	f.write(t, rec)

	if _, held := f.reg().LiveJob(""); held {
		t.Error("LiveJob(\"\") reported a holder")
	}
	if jobs := f.reg().LiveJobs(); len(jobs) != 0 {
		t.Errorf("LiveJobs() = %v, want empty — a record with no session id says nothing about any session", jobs)
	}
}

// TestCCSessionsDir keeps the one path-building rule in one place, exactly as
// TestCCProjectsDir does for the other store.
func TestCCSessionsDir(t *testing.T) {
	if got, want := CCSessionsDir("/home/u/.cc"), filepath.Join("/home/u/.cc", "sessions"); got != want {
		t.Errorf("CCSessionsDir = %q, want %q", got, want)
	}
	if got := CCSessionsDir(""); got != "" {
		t.Errorf("CCSessionsDir(\"\") = %q, want empty — an unknown config dir is not the filesystem root", got)
	}
	// The two stores are SIBLINGS under one config dir. Asserted because
	// lifecycle.go resolves them from a single CLAUDE_CONFIG_DIR read on the
	// strength of exactly this, and a reader pointed at the wrong pair would look
	// consistent while answering about a different cc install.
	if filepath.Dir(CCSessionsDir("/home/u/.cc")) != filepath.Dir(CCProjectsDir("/home/u/.cc")) {
		t.Error("CCSessionsDir and CCProjectsDir do not share a parent; they must be two directories of ONE cc config dir")
	}
}

// TestCCProcStartTokenMatchesTheKernel checks the parse against the kernel rather
// than against a fixture, because the parse is the part most likely to be wrong
// and a hand-written /proc line would only test that this file agrees with
// itself.
//
// It also pins the reason the scan walks BACK from the last ')': comm is field 2
// of /proc/<pid>/stat, printed in parentheses and unescaped, so a process whose
// name contains ')' or a space shifts every column for anything counting from the
// left. The child here is deliberately named with both.
func TestCCProcStartTokenMatchesTheKernel(t *testing.T) {
	requireLinux(t)
	self := os.Getpid()
	tok, ok := ccProcStartToken(self)
	if !ok || tok == "" {
		t.Fatalf("ccProcStartToken(%d) = %q, %v; want this process's own start time", self, tok, ok)
	}
	// Independent read of the same field, so a wrong index shows up as a
	// disagreement rather than as two copies of the same mistake.
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", self))
	if err != nil {
		t.Fatalf("read own stat: %v", err)
	}
	after := string(raw[strings.LastIndex(string(raw), ")")+2:])
	fields := strings.Split(after, " ")
	if len(fields) <= ccProcStatStartField {
		t.Fatalf("own stat has %d fields after comm, need > %d", len(fields), ccProcStatStartField)
	}
	if want := fields[ccProcStatStartField]; tok != want {
		t.Errorf("ccProcStartToken = %q, want %q (field %d after comm)", tok, want, ccProcStatStartField)
	}

	// A process whose comm contains ')' and a space. bash/sh set comm from the
	// binary name, so the name is imposed via a copy of /bin/sh.
	weird := filepath.Join(t.TempDir(), "we ird)name")
	sh, err := os.ReadFile("/bin/sh")
	if err != nil {
		t.Skipf("cannot copy /bin/sh to build an awkwardly named child: %v", err)
	}
	if err := os.WriteFile(weird, sh, 0o755); err != nil {
		t.Skipf("cannot write the awkwardly named child: %v", err)
	}
	cmd := exec.Command(weird, "-c", "sleep 30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start the awkwardly named child: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()
	got, ok := ccProcStartToken(cmd.Process.Pid)
	if !ok || got == "" {
		t.Fatalf("ccProcStartToken(%d) = %q, %v for a child whose comm contains ')' and a space", cmd.Process.Pid, got, ok)
	}
	kraw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", cmd.Process.Pid))
	if err != nil {
		t.Fatalf("read child stat: %v", err)
	}
	kafter := strings.Split(string(kraw[strings.LastIndex(string(kraw), ")")+2:]), " ")
	if len(kafter) > ccProcStatStartField && got != kafter[ccProcStatStartField] {
		t.Errorf("awkward-comm child: ccProcStartToken = %q, want %q", got, kafter[ccProcStatStartField])
	}
	// And the whole point: a parse that counted fields from the LEFT would have
	// read a different column for this process, because its comm contains a space.
	if !strings.Contains(string(kraw), "we ird)name") {
		t.Errorf("the child's comm did not carry the awkward name; /proc/%d/stat = %q", cmd.Process.Pid, kraw)
	}
}

// TestCCProcStartTokenOnADeadPid — a pid nothing is running under has no token,
// and the absent-token answer is what ccPidHoldsRecord turns into "not live".
func TestCCProcStartTokenOnADeadPid(t *testing.T) {
	requireLinux(t)
	pid := deadPid(t)
	if tok, ok := ccProcStartToken(pid); ok {
		t.Errorf("ccProcStartToken(%d) = %q, true for a reaped child", pid, tok)
	}
}
