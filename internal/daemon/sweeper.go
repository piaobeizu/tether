// stubSweeper — GC for orphaned cc-session JSONL files (v0.1 backlog).
//
// PROBLEM. The daemon spawns cc subprocesses; each cc session creates
// a JSONL file at ~/.claude/projects/<encoded-cwd>/<sid>.jsonl per
// spec §D.1 / §E.5 / cc-sdk-route. Some of those become "stub"
// sessions: the file is created and a SessionStart hook fires, then
// cc dies before producing any real envelopes. Over time the
// projects directory accumulates these zero-value files.
//
// THRESHOLD CHOICE (documented for the v0.1 PR):
//
//   stubMaxLines = 2     — a session that received only a SessionStart
//                          hook fire (1 line) plus maybe a first
//                          attachment (1 line) and nothing else. Real
//                          sessions cross this within milliseconds of
//                          first user input.
//   stubMinAge   = 60min — must have been quiet for an hour. Bounds
//                          the worst-case false positive: a long pause
//                          mid-session looks idle, but >1h of no
//                          writes and ≤2 lines means the producer is
//                          almost certainly gone (cc never goes idle
//                          for that long while a process is alive).
//   sweepInterval = 10min — periodic walk; cheap because we only
//                          stat + count newlines, no JSON parse.
//
// SAFETY. Three layered checks before delete:
//   1. Path containment: every candidate must resolve INSIDE the
//      configured ProjectsDir (defensive — refuses to operate on
//      symlinks pointing outside, etc.).
//   2. Stub predicate: line count ≤ stubMaxLines AND mtime older
//      than stubMinAge.
//   3. Subscriber check: emitter.HasSubscribers(sid) must be false.
//      A live attach connection means someone is reading this file;
//      we never delete out from under them.
//
// DELETION STRATEGY. Hard-delete (single os.Remove). Rationale: the
// JSONL file is, by predicate, ~empty and untouched for an hour; cc
// always re-creates the file if a session resumes (per §D.1 the
// filename is a UUID per session, not reused). A two-phase rename-
// then-delete buys nothing here — there is nothing to recover, and
// the rename itself would leave .gc-pending litter if the daemon
// crashed mid-sweep. Hard-delete keeps the invariant "the projects
// dir contains only live or cold-but-real sessions" simple.
//
// OPT-IN. Disabled by default (Config.EnableStubSweeper=false) for
// v0.1 so we ship behind a flag and validate on real usage before
// turning it on for everyone. Existing test data + other people's
// flows must be unaffected.

package daemon

import (
	"bufio"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/watchdog"
)

// Sweeper threshold constants. See file-level docstring for rationale.
const (
	// stubMaxLines is the upper bound on JSONL line count for a
	// session to be considered a stub. A real session crosses this in
	// milliseconds.
	stubMaxLines = 2

	// stubMinAge is the minimum mtime-age before a candidate is
	// eligible for deletion.
	stubMinAge = 60 * time.Minute

	// sweepInterval is how often the sweeper walks the projects tree.
	sweepInterval = 10 * time.Minute

	// sweepHeartbeatInterval bounds how often the sweeper beats while
	// idle between sweeps. Independent of sweepInterval — the
	// supervisor's deadlock detector uses HeartbeatTimeout/2 (default
	// 5s/2 = 2.5s), so a 10-minute sweep gap would otherwise look
	// like a deadlock.
	sweepHeartbeatInterval = time.Second

	// sweepWalkLineCountCap bounds how many lines we count per file.
	// We only need to know "≤ stubMaxLines or not", so we stop early.
	// Saves a full scan on multi-MB live session files.
	sweepWalkLineCountCap = stubMaxLines + 1
)

// stubSweeper walks ProjectsDir on a ticker and deletes JSONL files
// that match the stub predicate AND have no live subscribers. Wired
// into the daemon supervisor as a sibling of daemonSubsystem when
// Config.EnableStubSweeper is true. See file-level docstring for the
// threshold rationale.
type stubSweeper struct {
	projectsDir string
	emitter     *agent.EnvelopeEmitter
	logf        func(format string, args ...any)

	// Test seams. Production passes zero values to use the package-
	// level constants above.
	interval     time.Duration
	maxLines     int
	minAge       time.Duration
	now          func() time.Time
	onDeleted    func(path string) // optional; for tests + metrics
}

func (s *stubSweeper) Name() string { return "gc-stub-sweeper" }

// stubSweeperConfig packages a stubSweeper's knobs for the daemon
// factory wiring.
type stubSweeperConfig struct {
	ProjectsDir string
	Emitter     *agent.EnvelopeEmitter
	Logf        func(format string, args ...any)
}

func newStubSweeper(cfg stubSweeperConfig) *stubSweeper {
	return &stubSweeper{
		projectsDir: cfg.ProjectsDir,
		emitter:     cfg.Emitter,
		logf:        cfg.Logf,
	}
}

func (s *stubSweeper) effectiveInterval() time.Duration {
	if s.interval > 0 {
		return s.interval
	}
	return sweepInterval
}

func (s *stubSweeper) effectiveMaxLines() int {
	if s.maxLines > 0 {
		return s.maxLines
	}
	return stubMaxLines
}

func (s *stubSweeper) effectiveMinAge() time.Duration {
	if s.minAge > 0 {
		return s.minAge
	}
	return stubMinAge
}

func (s *stubSweeper) effectiveNow() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// Run satisfies watchdog.Subsystem. Loops: heartbeat each second; run
// a sweep every effectiveInterval; exit on ctx cancel.
func (s *stubSweeper) Run(ctx context.Context, hb func()) error {
	hb()
	hbT := time.NewTicker(sweepHeartbeatInterval)
	defer hbT.Stop()
	sweepT := time.NewTicker(s.effectiveInterval())
	defer sweepT.Stop()

	// Run one sweep immediately on boot — surfaces any deletable
	// files left over from a previous (crashed) daemon run without
	// waiting for the first interval.
	s.sweep()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-hbT.C:
			hb()
		case <-sweepT.C:
			s.sweep()
		}
	}
}

// sweep walks projectsDir for *.jsonl files, evaluates the stub
// predicate, and deletes when safe. Errors are logged and counted
// but do not abort the sweep.
func (s *stubSweeper) sweep() {
	root := s.projectsDir
	if root == "" {
		return
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		s.logErr("[gc] resolve root %q: %v", root, err)
		return
	}

	maxLines := s.effectiveMaxLines()
	minAge := s.effectiveMinAge()
	cutoff := s.effectiveNow().Add(-minAge)

	walkErr := filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Per fs.WalkDirFunc — non-nil err is the dir/file we
			// failed to descend into. Log and skip; do not abort.
			s.logErr("[gc] walk %q: %v", path, err)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		// Defensive: ensure resolved path is still under absRoot.
		// Symlinks pointing outside the projects dir must NOT be
		// deleted.
		if !pathContained(absRoot, path) {
			s.logErr("[gc] refuse %q: outside root %q", path, absRoot)
			return nil
		}

		s.evaluateAndMaybeDelete(path, cutoff, maxLines)
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, fs.ErrNotExist) {
		s.logErr("[gc] sweep walk: %v", walkErr)
	}
}

// evaluateAndMaybeDelete is the per-file decision: stat + line count
// + subscriber check + delete. Pulled out for unit testing.
func (s *stubSweeper) evaluateAndMaybeDelete(path string, cutoff time.Time, maxLines int) {
	st, err := os.Stat(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.logErr("[gc] stat %q: %v", path, err)
		}
		return
	}
	if !st.Mode().IsRegular() {
		return
	}
	if !st.ModTime().Before(cutoff) {
		return // too fresh
	}
	n, err := countLinesUpTo(path, sweepWalkLineCountCap)
	if err != nil {
		s.logErr("[gc] count %q: %v", path, err)
		return
	}
	if n > maxLines {
		return // not a stub
	}

	sid := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if s.emitter != nil && s.emitter.HasSubscribers(sid) {
		return // someone's listening; do not delete
	}

	if err := os.Remove(path); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.logErr("[gc] remove %q: %v", path, err)
		}
		return
	}
	if s.logf != nil {
		s.logf("[gc] swept stub session %q (lines=%d, age>%s)", path, n, s.effectiveMinAge())
	}
	if s.onDeleted != nil {
		s.onDeleted(path)
	}
}

// pathContained reports whether `child` resolves inside `root`. Both
// arguments are expected to be absolute. Implemented via filepath.Rel
// + a check that the result does not climb out via "..".
func pathContained(root, child string) bool {
	rel, err := filepath.Rel(root, child)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if strings.HasPrefix(rel, "..") {
		return false
	}
	// On unix this is the only escape; Windows would also need to
	// reject volume drift, but tether is unix-only in v0.1.
	return true
}

// countLinesUpTo counts newlines in `path` but stops as soon as it
// has seen more than `limit` lines (we only need to know N > limit,
// not the exact value). Cheap — no JSON parsing, just bufio scan.
//
// A trailing partial line (no terminating newline) IS counted, so a
// 1-line file without a trailing \n still reports 1.
func countLinesUpTo(path string, limit int) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	count := 0
	pendingPartial := false
	for {
		_, isPrefix, err := r.ReadLine()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if pendingPartial {
					count++
				}
				return count, nil
			}
			return count, err
		}
		if isPrefix {
			pendingPartial = true
			continue
		}
		count++
		pendingPartial = false
		if count > limit {
			return count, nil
		}
	}
}

func (s *stubSweeper) logErr(format string, args ...any) {
	if s.logf != nil {
		s.logf(format, args...)
	}
}

// gcSubsystemFactory builds a SubsystemFactory for the stub sweeper.
// Pulled out so daemon.Run's wiring stays compact.
func gcSubsystemFactory(cfg stubSweeperConfig) SubsystemFactory {
	return func() watchdog.Subsystem {
		return newStubSweeper(cfg)
	}
}
