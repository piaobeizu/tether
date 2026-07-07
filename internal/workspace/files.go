package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// FileEntry describes a single directory entry returned by the files API,
// annotated with git-dirty state relative to its nearest enclosing git repo.
type FileEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"isDir"`
	Dirty bool   `json:"dirty"`
}

// listFiles returns the direct children of absDir (non-recursive), sorted
// directories-first then alphabetically, annotated with git-dirty state.
// The .git directory is never listed.
//
// Dirty computation performs per-directory git-root discovery: absDir's own
// git repo (found via `git rev-parse --show-toplevel`) is used, which may
// differ from the workspace root passed to SafeJoin. If absDir is not inside
// any git repo, every entry is reported clean.
func listFiles(absDir string) ([]FileEntry, error) {
	dirEntries, err := os.ReadDir(absDir)
	if err != nil {
		return nil, err
	}

	dirtySet, repoRoot := gitDirtySet(absDir)

	entries := make([]FileEntry, 0, len(dirEntries))
	for _, e := range dirEntries {
		// Never expose the git internals directory in the tree.
		if e.Name() == ".git" {
			continue
		}
		entry := FileEntry{Name: e.Name(), IsDir: e.IsDir()}
		if repoRoot != "" {
			entryAbs := filepath.Join(absDir, e.Name())
			relPath := repoRelPath(repoRoot, entryAbs)
			if entry.IsDir {
				entry.Dirty = dirtySet.hasPrefix(relPath + "/")
			} else {
				entry.Dirty = dirtySet.contains(relPath)
			}
		}
		entries = append(entries, entry)
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir // dirs first
		}
		return entries[i].Name < entries[j].Name
	})

	return entries, nil
}

// dirtyPathSet holds repo-root-relative dirty paths (forward-slash separated).
type dirtyPathSet map[string]struct{}

func (s dirtyPathSet) contains(p string) bool {
	_, ok := s[p]
	return ok
}

func (s dirtyPathSet) hasPrefix(prefix string) bool {
	for p := range s {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// gitDirtyTimeout bounds the git invocations so a huge or locked repo cannot
// hang the request handler.
const gitDirtyTimeout = 5 * time.Second

// gitDirtySet discovers the git repo root enclosing absDir (if any) and
// returns the set of repo-root-relative dirty paths. If absDir is not inside
// a git repo (or git times out / errors), returns an empty/clean set.
func gitDirtySet(absDir string) (dirtyPathSet, string) {
	ctx, cancel := context.WithTimeout(context.Background(), gitDirtyTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "git", "-C", absDir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return nil, ""
	}
	repoRoot := strings.TrimSpace(string(out))
	if repoRoot == "" {
		return nil, ""
	}

	// `-z` + core.quotePath=false: NUL-terminated records with VERBATIM paths
	// (no surrounding quotes, no C-escapes) — correct for non-ASCII / special
	// filenames, and removes the " -> " rename-parsing ambiguity.
	statusOut, err := exec.CommandContext(ctx, "git", "-C", repoRoot,
		"-c", "core.quotePath=false", "status", "--porcelain", "-z").Output()
	if err != nil {
		// Repo root resolved but status failed/timed out — treat as clean
		// rather than erroring (or hanging) the whole listing.
		return dirtyPathSet{}, repoRoot
	}

	return parsePorcelainZ(string(statusOut)), repoRoot
}

// parsePorcelainZ parses `git status --porcelain -z` output into a set of
// repo-root-relative dirty paths. Records are NUL-terminated; each is
// "XY <space> PATH". For rename/copy records (X or Y is 'R'/'C') the ORIGINAL
// path is a trailing NUL field that must be consumed — the destination (which
// is the dirty path) precedes it.
func parsePorcelainZ(output string) dirtyPathSet {
	set := dirtyPathSet{}
	fields := strings.Split(output, "\x00")
	for i := 0; i < len(fields); i++ {
		rec := fields[i]
		if len(rec) < 4 {
			continue // trailing empty field / malformed
		}
		x, y := rec[0], rec[1]
		path := rec[3:] // strip the "XY " prefix (2 status chars + one space)
		if path != "" {
			set[path] = struct{}{}
		}
		if x == 'R' || x == 'C' || y == 'R' || y == 'C' {
			i++ // skip the source-path field that follows a rename/copy record
		}
	}
	return set
}

// repoRelPath returns entryAbs relative to repoRoot using forward slashes,
// matching git's porcelain path format.
func repoRelPath(repoRoot, entryAbs string) string {
	rel, err := filepath.Rel(repoRoot, entryAbs)
	if err != nil {
		return entryAbs
	}
	return filepath.ToSlash(rel)
}
