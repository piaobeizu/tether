package adapter

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// CCPluginsSubdir is the workspace-relative directory that cc's plugin
// loader scans (spec §11.Z.6 step 2). Each enabled skill becomes a
// symlink at <workspace>/.claude/plugins/<skill> pointing at the global
// pool entry.
const CCPluginsSubdir = ".claude/plugins"

// CC is the v0.1 cc adapter. It owns nothing but the symlink contract:
//
//	<global-pool>/<skill>  →  <workspace>/.claude/plugins/<skill>
//
// Spec §11.Z.4: pure symlink, no copy/rewrite, no plugin.json generation
// (skill ships its own .claude/plugin.json).
type CC struct{}

// Materialise links one skill into the workspace. If the link target
// already exists and points at the same source, it is left alone
// (idempotent re-spawn). Any non-symlink at the destination path is a
// hard error — we refuse to clobber a real directory the user might
// have committed by hand.
func (CC) Materialise(workspaceDir, skillName, skillSourceDir string) error {
	if workspaceDir == "" {
		return errors.New("adapter/cc: empty workspaceDir")
	}
	if skillName == "" {
		return errors.New("adapter/cc: empty skillName")
	}
	if skillSourceDir == "" {
		return errors.New("adapter/cc: empty skillSourceDir")
	}
	if st, err := os.Stat(skillSourceDir); err != nil {
		return fmt.Errorf("adapter/cc: skill source %s: %w", skillSourceDir, err)
	} else if !st.IsDir() {
		return fmt.Errorf("adapter/cc: skill source %s is not a directory", skillSourceDir)
	}

	pluginsDir := filepath.Join(workspaceDir, CCPluginsSubdir)
	if err := os.MkdirAll(pluginsDir, 0o755); err != nil {
		return fmt.Errorf("adapter/cc: ensure %s: %w", pluginsDir, err)
	}
	link := filepath.Join(pluginsDir, skillName)

	// Resolve absolute source so the symlink is portable across cwd
	// changes (cc spawns with workspace cwd; absolute is safest).
	absSource, err := filepath.Abs(skillSourceDir)
	if err != nil {
		return fmt.Errorf("adapter/cc: abs source: %w", err)
	}

	// Inspect the existing entry, if any.
	if fi, err := os.Lstat(link); err == nil {
		if fi.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("adapter/cc: %s exists and is not a symlink (refusing to clobber)", link)
		}
		current, rerr := os.Readlink(link)
		if rerr == nil && current == absSource {
			return nil // already correct
		}
		// Stale or wrong target — replace it.
		if err := os.Remove(link); err != nil {
			return fmt.Errorf("adapter/cc: remove stale link %s: %w", link, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("adapter/cc: stat %s: %w", link, err)
	}

	if err := os.Symlink(absSource, link); err != nil {
		return fmt.Errorf("adapter/cc: symlink %s -> %s: %w", link, absSource, err)
	}
	return nil
}

// Unmaterialise removes the workspace symlink for a skill. No-op if the
// link is absent. Refuses to delete real directories.
func (CC) Unmaterialise(workspaceDir, skillName string) error {
	link := filepath.Join(workspaceDir, CCPluginsSubdir, skillName)
	fi, err := os.Lstat(link)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("adapter/cc: stat %s: %w", link, err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("adapter/cc: %s is not a symlink (refusing to remove)", link)
	}
	if err := os.Remove(link); err != nil {
		return fmt.Errorf("adapter/cc: remove %s: %w", link, err)
	}
	return nil
}
