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
func ResolveSession(projectsRoot, sid, forceBucket string) (*ResolveResult, error) {
	if sid == "" {
		return nil, errors.New("session id is empty")
	}

	if forceBucket != "" {
		jsonl := filepath.Join(projectsRoot, forceBucket, sid+".jsonl")
		if _, err := os.Stat(jsonl); err != nil {
			return nil, fmt.Errorf("--bucket %q has no %s.jsonl: %w", forceBucket, sid, err)
		}
		return &ResolveResult{
			Bucket:    forceBucket,
			JsonlPath: jsonl,
			Cwd:       decodeBucket(forceBucket),
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
		return &ResolveResult{
			Bucket:    b,
			JsonlPath: filepath.Join(projectsRoot, b, sid+".jsonl"),
			Cwd:       decodeBucket(b),
		}, nil
	default:
		var lines []string
		for _, b := range matches {
			lines = append(lines, fmt.Sprintf("  --bucket %s   (cwd: %s)", b, decodeBucket(b)))
		}
		return nil, fmt.Errorf("session %s found in %d buckets; pick one:\n%s",
			sid, len(matches), strings.Join(lines, "\n"))
	}
}

// findSidBuckets returns bucket names (relative to projectsRoot) that contain
// <sid>.jsonl, sorted by the jsonl's modtime descending (most recent first)
// so that single-result callers get the freshest copy and multi-result
// callers see the most-likely-intended candidate at the top.
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
	sort.Slice(hits, func(i, j int) bool { return hits[i].mod > hits[j].mod })

	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.name
	}
	return out, nil
}

// decodeBucket reverses cc's `/` → `-` encoding to a plausible filesystem
// path, picking the longest existing prefix-match on disk to disambiguate
// the lossy mapping.
//
// Lossiness: `/foo/bar` and `/foo-bar` both encode to `-foo-bar`. Decoder
// strategy: walk left-to-right deciding for each `-` boundary whether it's
// a path separator or a literal hyphen. We only commit a `/` boundary when
// the path *up to that boundary* exists on disk; ties break in favor of
// the interpretation whose final assembled path actually exists.
//
// This is best-effort — if no path on disk matches, we fall back to the
// naive `/`-everywhere substitution. Callers should treat the result as
// "the cwd to pass to claude --resume" and accept that for buckets whose
// original path no longer exists the wrapper may pick a sibling location.
func decodeBucket(bucket string) string {
	if bucket == "" {
		return ""
	}
	// cc encoding always prefixes with `-` (the leading `/` of an absolute
	// path). Strip the prefix; what remains is `-`-separated segments.
	rest := strings.TrimPrefix(bucket, "-")
	tokens := strings.Split(rest, "-")

	// Try existence-aware decode first.
	if best := bestExistingDecode("/", tokens, 0); best != "" {
		return best
	}
	// Fallback: naive — every `-` becomes `/`.
	return "/" + strings.Join(tokens, "/")
}

// bestExistingDecode picks the longest assembled path that exists on disk by
// branching at each token boundary on "is this a path separator or literal
// hyphen". Returns "" when no full assembly exists.
//
// We only prune (gate recursion on `pathExists`) when the accumulated
// prefix ends at a segment boundary (`/`). Mid-segment prefixes are never
// directories, so checking them would prune valid branches like
// `/foo` → `/foo-bar` where only the merged form exists.
func bestExistingDecode(prefix string, tokens []string, i int) string {
	if i == len(tokens) {
		// Leaf: accept iff the assembled path exists.
		if pathExists(prefix) {
			return prefix
		}
		return ""
	}

	atBoundary := strings.HasSuffix(prefix, "/")
	if atBoundary && !pathExists(prefix) {
		// Pruning is safe at boundaries: if `/a/b/` doesn't exist, no
		// extension of it can.
		return ""
	}

	// Branch A: treat the next `-` as a path separator → start a new
	// segment with this token.
	sep := "/"
	if atBoundary {
		sep = ""
	}
	branchA := bestExistingDecode(prefix+sep+tokens[i], tokens, i+1)

	// Branch B: treat the next `-` as a literal hyphen → glue token onto
	// the current segment. Only meaningful mid-segment (not right after `/`).
	var branchB string
	if !atBoundary {
		branchB = bestExistingDecode(prefix+"-"+tokens[i], tokens, i+1)
	}

	// Prefer the branch with the longer assembled path. Both being non-empty
	// means both fully exist; tie-break by length (deeper paths preferred).
	if len(branchA) >= len(branchB) {
		return branchA
	}
	return branchB
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

// ExecClaude execve's into `claude --resume <sid>` after chdir'ing to cwd.
// On success this never returns. Returns an error only if chdir or exec fail.
//
// We use syscall.Exec rather than exec.Command + Run so that the user gets a
// PTY-attached claude with no Go process in the middle — clean signal
// handling, no extra fd, no process tree noise.
func ExecClaude(binary, cwd, sid string) error {
	resolvedBin, err := exec.LookPath(binary)
	if err != nil {
		return fmt.Errorf("locate %q: %w", binary, err)
	}
	if err := os.Chdir(cwd); err != nil {
		return fmt.Errorf("chdir %s: %w", cwd, err)
	}
	argv := []string{resolvedBin, "--resume", sid}
	return syscall.Exec(resolvedBin, argv, os.Environ())
}
