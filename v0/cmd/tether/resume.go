package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// ErrSidNotFound is returned when the session id has no matching jsonl in
// any project bucket.
var ErrSidNotFound = errors.New("session id not found in any ~/.claude/projects bucket")

// ResolveResult carries everything the caller needs to exec claude.
type ResolveResult struct {
	// Bucket is the directory name under projectsRoot containing <sid>.jsonl.
	Bucket string
	// JsonlPath is the absolute path to <bucket>/<sid>.jsonl.
	JsonlPath string
	// Cwd is the original spawn-time cwd reconstructed from Bucket. cc's
	// encoding (`/` → `-`, leading `-`) is lossy, so this is the
	// "best-effort plausible decoding" — the existing path on disk that
	// matches when there's exactly one. See decodeBucket.
	Cwd string
}

// defaultProjectsDir returns ~/.claude/projects.
func defaultProjectsDir() (string, error) {
	home := os.Getenv("HOME")
	if home == "" {
		return "", errors.New("$HOME unset; cannot locate ~/.claude/projects")
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

// ResolveSession scans projectsRoot for any bucket containing <sid>.jsonl and
// returns the resolved cwd. If `forceBucket` is non-empty it short-circuits
// the search and uses that bucket directly (still requires the jsonl to exist).
//
// On a multi-bucket collision the error message lists all candidates and
// instructs the user to re-run with --bucket.
//
// On a single-bucket-but-ambiguous-decode collision (two distinct on-disk
// paths both decode-to-existence — e.g. /parent/foo/bar AND /parent/foo-bar
// both real), the error lists every candidate cwd and instructs the user to
// pass --cwd. This is a separate failure mode from multi-bucket: the bucket
// is unique but its `/` ↔ `-` reversal isn't.
func ResolveSession(projectsRoot, sid, forceBucket string) (*ResolveResult, error) {
	if sid == "" {
		return nil, errors.New("session id is empty")
	}

	if forceBucket != "" {
		jsonl := filepath.Join(projectsRoot, forceBucket, sid+".jsonl")
		if _, err := os.Stat(jsonl); err != nil {
			return nil, fmt.Errorf("--bucket %q has no %s.jsonl: %w", forceBucket, sid, err)
		}
		cwd, alts := decodeBucket(forceBucket)
		if len(alts) > 0 {
			return nil, ambiguousCwdError(forceBucket, alts)
		}
		return &ResolveResult{
			Bucket:    forceBucket,
			JsonlPath: jsonl,
			Cwd:       cwd,
		}, nil
	}

	matches, err := findSidBuckets(projectsRoot, sid)
	if err != nil {
		return nil, err
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("%w: sid=%s root=%s", ErrSidNotFound, sid, projectsRoot)
	case 1:
		b := matches[0]
		cwd, alts := decodeBucket(b)
		if len(alts) > 0 {
			return nil, ambiguousCwdError(b, alts)
		}
		return &ResolveResult{
			Bucket:    b,
			JsonlPath: filepath.Join(projectsRoot, b, sid+".jsonl"),
			Cwd:       cwd,
		}, nil
	default:
		var lines []string
		for _, b := range matches {
			cwd, _ := decodeBucket(b)
			lines = append(lines, fmt.Sprintf("  --bucket %s   (cwd: %s)", b, cwd))
		}
		return nil, fmt.Errorf("session %s found in %d buckets; pick one:\n%s",
			sid, len(matches), strings.Join(lines, "\n"))
	}
}

// ambiguousCwdError formats the prefix-collision error message for the
// single-bucket-multiple-decodes case. Listed candidates are pre-sorted by
// decodeBucket so the message is deterministic across runs.
func ambiguousCwdError(bucket string, alternates []string) error {
	var lines []string
	for _, p := range alternates {
		lines = append(lines, "  --cwd "+p)
	}
	return fmt.Errorf("bucket %s decodes to multiple existing paths; pass --cwd to pick:\n%s",
		bucket, strings.Join(lines, "\n"))
}

// ResolveSessionWithCwd is the manual-disambiguation entry point: caller has
// already chosen the cwd (via --cwd), we just locate the jsonl and skip the
// lossy decode entirely. The cwd is validated for existence.
func ResolveSessionWithCwd(projectsRoot, sid, explicitCwd string) (*ResolveResult, error) {
	if sid == "" {
		return nil, errors.New("session id is empty")
	}
	if explicitCwd == "" {
		return nil, errors.New("--cwd is empty")
	}
	cleaned := filepath.Clean(explicitCwd)
	if !filepath.IsAbs(cleaned) {
		return nil, fmt.Errorf("--cwd %q must be absolute", explicitCwd)
	}
	if hasDotDot(cleaned) {
		return nil, fmt.Errorf("--cwd %q contains .. segments after Clean", explicitCwd)
	}
	if !pathExists(cleaned) {
		return nil, fmt.Errorf("--cwd %q does not exist", explicitCwd)
	}
	bucket := EncodeBucket(cleaned)
	jsonl := filepath.Join(projectsRoot, bucket, sid+".jsonl")
	if _, err := os.Stat(jsonl); err != nil {
		return nil, fmt.Errorf("no %s.jsonl under encoded bucket %s for --cwd %s: %w",
			sid, bucket, cleaned, err)
	}
	return &ResolveResult{
		Bucket:    bucket,
		JsonlPath: jsonl,
		Cwd:       cleaned,
	}, nil
}

// findSidBuckets returns bucket names (relative to projectsRoot) that contain
// <sid>.jsonl, sorted by the jsonl's modtime descending (most recent first)
// so that single-result callers get the freshest copy and multi-result
// callers see the most-likely-intended candidate at the top. Equal-mtime
// ties (possible on second-resolution filesystems like FAT or some NFS
// mounts) break by bucket name ascending so collision messages are
// deterministic across runs.
func findSidBuckets(projectsRoot, sid string) ([]string, error) {
	entries, err := os.ReadDir(projectsRoot)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", projectsRoot, err)
	}

	type hit struct {
		name string
		mod  int64
	}
	var hits []hit
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		jsonl := filepath.Join(projectsRoot, e.Name(), sid+".jsonl")
		st, err := os.Stat(jsonl)
		if err != nil {
			continue
		}
		hits = append(hits, hit{name: e.Name(), mod: st.ModTime().UnixNano()})
	}
	// SliceStable + composite less so the order is fully determined even when
	// modtimes tie at fs resolution.
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].mod != hits[j].mod {
			return hits[i].mod > hits[j].mod
		}
		return hits[i].name < hits[j].name
	})

	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.name
	}
	return out, nil
}

// decodeBucket reverses cc's `/` → `-` encoding to a plausible filesystem
// path. Returns (cwd, ambiguousAlternates):
//
//   - When EXACTLY ONE decoding fully exists on disk, returns (that path, nil).
//   - When MULTIPLE distinct decodings all fully exist on disk (the
//     prefix-collision case: both `/parent/foo/bar` and `/parent/foo-bar`
//     are real directories), returns ("", [all candidates sorted asc]).
//     The caller is expected to surface this as a user-facing error and
//     prompt for explicit `--cwd` disambiguation.
//   - When NO decoding exists on disk, falls back to the naive
//     `/`-everywhere substitution and returns (that path, nil). Treat as
//     best-effort: the wrapper may pick a sibling location for buckets
//     whose original cwd has been deleted/moved.
//
// Lossiness recap: `/foo/bar` and `/foo-bar` both encode to `-foo-bar`. The
// previous version of this function silently picked branchA on tied length —
// see PR #35 review feedback. Collecting all fully-existing decodes and
// surfacing ambiguity to the caller is the safe behavior.
func decodeBucket(bucket string) (string, []string) {
	if bucket == "" {
		return "", nil
	}
	// cc encoding always prefixes with `-` (the leading `/` of an absolute
	// path). Strip the prefix; what remains is `-`-separated segments.
	rest := strings.TrimPrefix(bucket, "-")
	tokens := strings.Split(rest, "-")

	var existing []string
	collectExistingDecodes("/", tokens, 0, &existing)

	// Deduplicate (the recursion can produce the same path through different
	// branch orderings only in pathological cases, but normalize anyway) and
	// sort for deterministic ambiguity messages.
	uniq := dedupSortedAsc(existing)

	switch len(uniq) {
	case 0:
		// No on-disk match → naive fallback.
		return "/" + strings.Join(tokens, "/"), nil
	case 1:
		return uniq[0], nil
	default:
		return "", uniq
	}
}

// collectExistingDecodes walks the decode-tree appending every fully-existing
// assembled path to *out. Branch semantics mirror the previous
// bestExistingDecode: at each `-` token boundary, branchA treats it as `/`
// (new segment) and branchB treats it as a literal `-` (glue onto current
// segment). Pruning at directory boundaries keeps cost O(real-paths).
func collectExistingDecodes(prefix string, tokens []string, i int, out *[]string) {
	if i == len(tokens) {
		if pathExists(prefix) {
			*out = append(*out, prefix)
		}
		return
	}

	atBoundary := strings.HasSuffix(prefix, "/")
	if atBoundary && !pathExists(prefix) {
		// Safe prune: if `/a/b/` doesn't exist, no extension of it can.
		return
	}

	// Branch A: `-` is a path separator → start a new segment.
	sep := "/"
	if atBoundary {
		sep = ""
	}
	collectExistingDecodes(prefix+sep+tokens[i], tokens, i+1, out)

	// Branch B: `-` is literal → glue token to current segment. Only
	// meaningful mid-segment.
	if !atBoundary {
		collectExistingDecodes(prefix+"-"+tokens[i], tokens, i+1, out)
	}
}

// dedupSortedAsc returns the unique elements of in, sorted ascending.
func dedupSortedAsc(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	cp := make([]string, len(in))
	copy(cp, in)
	sort.Strings(cp)
	out := cp[:1]
	for _, s := range cp[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// EncodeBucket is the forward direction of cc's encoding: `/foo/bar` →
// `-foo-bar`. Used in tests + as documentation. Trailing `/` is dropped.
func EncodeBucket(cwd string) string {
	if cwd == "" {
		return ""
	}
	// Drop trailing slash (matches cc behavior).
	if cwd != "/" {
		cwd = strings.TrimRight(cwd, "/")
	}
	return strings.ReplaceAll(cwd, "/", "-")
}

// BuildResumeArgv produces the argv (including argv[0]) for the claude
// invocation `tether resume` execve's into. Pure helper, exported for
// tests so they can assert flag wiring without exercising the full
// process replacement.
//
// settingsPath, when non-empty, appends `--settings <path>` so cc loads
// its hook config from the daemon-owned settings.json (PR #44 hookserver
// integration). Empty preserves legacy behavior — cc reads its default
// `~/.claude/settings.json`.
func BuildResumeArgv(binary, sid, settingsPath string) []string {
	argv := []string{binary, "--resume", sid}
	if settingsPath != "" {
		argv = append(argv, "--settings", settingsPath)
	}
	return argv
}

// execClaudeSyscall is the production execve path. It's a package-level
// var so tests can stub it with a recorder that captures argv without
// actually replacing the test process. Production code MUST NOT mutate.
var execClaudeSyscall = func(argv0 string, argv []string, env []string) error {
	return syscall.Exec(argv0, argv, env)
}

// ExecClaude execve's into `claude --resume <sid> [--settings <p>]` after
// chdir'ing to cwd. On success this never returns. Returns an error only
// if chdir or exec fail.
//
// We use syscall.Exec rather than exec.Command + Run so that the user gets a
// PTY-attached claude with no Go process in the middle — clean signal
// handling, no extra fd, no process tree noise.
//
// Defense-in-depth on the cwd input: we always Clean the path and reject
// any residual `..` segments. The decoder shouldn't produce `..` (it
// only emits paths that exist on disk), but a malformed bucket name like
// `-tmp-..-etc` could in principle slip through the naive fallback —
// hard-fail here rather than silently chdir'ing to an unintended location.
//
// settingsPath, when non-empty, is forwarded to cc as `--settings <path>`
// so the daemon's hookserver wiring (PR #44) actually fires. Empty
// preserves the pre-PR behavior (cc reads ~/.claude/settings.json).
func ExecClaude(binary, cwd, sid, settingsPath string) error {
	cwd = filepath.Clean(cwd)
	if hasDotDot(cwd) {
		return fmt.Errorf("refusing to chdir: %q contains .. segments after Clean", cwd)
	}
	resolvedBin, err := exec.LookPath(binary)
	if err != nil {
		return fmt.Errorf("locate %q: %w", binary, err)
	}
	if err := os.Chdir(cwd); err != nil {
		return fmt.Errorf("chdir %s: %w", cwd, err)
	}
	argv := BuildResumeArgv(resolvedBin, sid, settingsPath)
	return execClaudeSyscall(resolvedBin, argv, os.Environ())
}

// hasDotDot reports whether p has any `..` segment after splitting on the
// OS path separator. filepath.Clean reduces redundant separators but does
// NOT eliminate leading-or-internal `..` (e.g. `Clean("/a/../b")` is `/b`,
// but `Clean("../b")` stays `../b`). Combined with our absolute-path
// invariant, any residual `..` is a defect.
func hasDotDot(p string) bool {
	for _, seg := range strings.Split(p, string(filepath.Separator)) {
		if seg == ".." {
			return true
		}
	}
	return false
}
