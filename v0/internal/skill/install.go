package skill

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// DefaultPoolSubdir is the path beneath the user's home dir that holds the
// global skill pool (spec §11.Z.6 + §11 Q7).
const DefaultPoolSubdir = ".tether/skills"

// BlessedListPath is the path inside the main tether repo where the
// short-name → git URL mapping is checked in. The CLI bundles this list
// at build time / falls back to an embedded copy; until then we look on
// the filesystem next to the binary or under the workspace root.
const BlessedListPath = "skills.toml"

// BlessedList is the shape of the main-repo skills.toml. Schema is small
// on purpose — short-name → git URL is enough for v0.1; richer metadata
// (rev pinning, signature, channel) is deferred to v0.1.x. See open
// questions in PR description.
type BlessedList struct {
	// Skills is keyed by short name. Each entry carries the upstream git
	// URL plus optional pinning data we may honour later.
	Skills map[string]BlessedEntry `toml:"skills"`
}

// BlessedEntry is one row in the blessed list.
type BlessedEntry struct {
	URL         string `toml:"url"`
	Ref         string `toml:"ref"`         // optional branch/tag/sha
	Description string `toml:"description"` // surfaced by `tether skill info`
}

// LoadBlessedList parses a skills.toml at the given path. Missing file is
// not an error — callers fall back to "user must supply git URL".
func LoadBlessedList(path string) (*BlessedList, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &BlessedList{Skills: map[string]BlessedEntry{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("skill: read blessed list %s: %w", path, err)
	}
	var l BlessedList
	if err := toml.Unmarshal(b, &l); err != nil {
		return nil, fmt.Errorf("skill: parse blessed list %s: %w", path, err)
	}
	if l.Skills == nil {
		l.Skills = map[string]BlessedEntry{}
	}
	return &l, nil
}

// Pool is the global skills directory (default: ~/.tether/skills/). All
// install / list / remove / info operations route through here.
type Pool struct {
	Root string
}

// DefaultPool returns the user-default pool rooted at $HOME/.tether/skills.
func DefaultPool() (*Pool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("skill: locate home dir: %w", err)
	}
	return &Pool{Root: filepath.Join(home, DefaultPoolSubdir)}, nil
}

// Ensure mkdirs the pool root. Idempotent.
func (p *Pool) Ensure() error {
	if err := os.MkdirAll(p.Root, 0o755); err != nil {
		return fmt.Errorf("skill: ensure pool %s: %w", p.Root, err)
	}
	return nil
}

// SkillDir returns <pool>/<name>. Validation is the caller's responsibility.
func (p *Pool) SkillDir(name string) string {
	return filepath.Join(p.Root, name)
}

// Has reports whether <pool>/<name> exists as a directory.
func (p *Pool) Has(name string) bool {
	st, err := os.Stat(p.SkillDir(name))
	return err == nil && st.IsDir()
}

// List enumerates installed skills (lexicographic). Hidden entries
// (dotfiles) are skipped so future bookkeeping files don't leak.
func (p *Pool) List() ([]string, error) {
	entries, err := os.ReadDir(p.Root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("skill: list pool %s: %w", p.Root, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// Remove deletes <pool>/<name>. No-op if absent.
func (p *Pool) Remove(name string) error {
	if !SkillNameRe.MatchString(name) {
		return fmt.Errorf("skill: invalid name %q", name)
	}
	dir := p.SkillDir(name)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("skill: remove %s: %w", dir, err)
	}
	return nil
}

// ResolveSource turns a CLI argument (short-name or git URL) into a clone
// URL using the blessed list. Returns the resolved URL plus the canonical
// short name to install under.
func ResolveSource(arg string, list *BlessedList) (url, name string, err error) {
	if isGitURL(arg) {
		// User-supplied URL: derive name from the last path segment
		// minus a trailing .git, mirroring git's own default.
		return arg, gitURLBase(arg), nil
	}
	if !SkillNameRe.MatchString(arg) {
		return "", "", fmt.Errorf("skill: argument %q is neither a skill name nor a git URL", arg)
	}
	if list == nil {
		return "", "", fmt.Errorf("skill: no blessed list available; pass full git URL")
	}
	entry, ok := list.Skills[arg]
	if !ok {
		return "", "", fmt.Errorf("skill: %q not in blessed list (try a git URL)", arg)
	}
	if entry.URL == "" {
		return "", "", fmt.Errorf("skill: blessed entry %q has empty url", arg)
	}
	return entry.URL, arg, nil
}

// Cloner is the git-clone abstraction. Production uses GitCloneCmd;
// tests inject a fake.
type Cloner func(ctx context.Context, url, dest, ref string) error

// GitCloneCmd shells out to `git clone --depth=1 [--branch ref] <url> <dest>`.
func GitCloneCmd(ctx context.Context, url, dest, ref string) error {
	args := []string{"clone", "--depth=1"}
	if ref != "" {
		args = append(args, "--branch", ref)
	}
	args = append(args, url, dest)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("skill: git clone %s: %w", url, err)
	}
	return nil
}

// InstallOptions tweaks Install. Force re-clones on top of an existing
// directory; Cloner overrides the default for tests.
type InstallOptions struct {
	Force  bool
	Cloner Cloner
	Ref    string
}

// Install clones <url> into <pool>/<name>. Returns the parsed manifest
// when one exists at the expected path (otherwise nil — a skill without
// tether.toml is still valid, see §11.Z.1).
func (p *Pool) Install(ctx context.Context, url, name string, opts InstallOptions) (*Manifest, error) {
	if !SkillNameRe.MatchString(name) {
		return nil, fmt.Errorf("skill: invalid name %q", name)
	}
	if err := p.Ensure(); err != nil {
		return nil, err
	}
	dest := p.SkillDir(name)
	if _, err := os.Stat(dest); err == nil {
		if !opts.Force {
			return nil, fmt.Errorf("skill: %s already installed (use --force to overwrite)", name)
		}
		if err := os.RemoveAll(dest); err != nil {
			return nil, fmt.Errorf("skill: clear existing %s: %w", dest, err)
		}
	}
	cloner := opts.Cloner
	if cloner == nil {
		cloner = GitCloneCmd
	}
	if err := cloner(ctx, url, dest, opts.Ref); err != nil {
		return nil, err
	}
	m, err := readManifestIfPresent(dest)
	if err != nil {
		// Bad tether.toml is a hard error — the file is present and broken.
		// Roll back the install so the user can retry.
		_ = os.RemoveAll(dest)
		return nil, err
	}
	return m, nil
}

func readManifestIfPresent(dir string) (*Manifest, error) {
	path := filepath.Join(dir, "tether.toml")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return LoadManifest(path)
}

// Info returns the parsed manifest for an installed skill, or nil if the
// skill exists but ships no tether.toml.
func (p *Pool) Info(name string) (*Manifest, error) {
	if !p.Has(name) {
		return nil, fmt.Errorf("skill: %q not installed", name)
	}
	return readManifestIfPresent(p.SkillDir(name))
}

// MissingSkills reports which entries of `enabled` aren't yet in the pool.
// Used by the B-6 fallback (spec §11.Z.11) and by `tether spawn`.
func (p *Pool) MissingSkills(enabled []string) []string {
	var missing []string
	for _, n := range enabled {
		if !p.Has(n) {
			missing = append(missing, n)
		}
	}
	return missing
}

// FallbackMode encodes the B-6 resolution policy.
type FallbackMode int

const (
	// FallbackFail (default) — error out with a helpful message.
	FallbackFail FallbackMode = iota
	// FallbackAutoInstall — clone missing skills via the blessed list.
	FallbackAutoInstall
	// FallbackAllowMissing — proceed with whatever is installed.
	FallbackAllowMissing
)

// ResolveSkills realises the B-6 fallback (spec §11.Z.11). Behaviour:
//   - FallbackFail: returns an error listing missing skills + repair hint.
//   - FallbackAutoInstall: tries to clone each missing skill via the
//     blessed list; on any failure falls back to FallbackFail's error.
//   - FallbackAllowMissing: returns the subset of enabled that are present
//     and a non-fatal warning string (printed by the caller).
//
// The function never spawns cc — it exists so daemon/CLI startup paths
// can decide whether to proceed.
func ResolveSkills(
	ctx context.Context,
	pool *Pool,
	list *BlessedList,
	workspacePath string,
	enabled []string,
	mode FallbackMode,
	cloner Cloner,
) (resolved []string, warning string, err error) {
	missing := pool.MissingSkills(enabled)
	if len(missing) == 0 {
		return enabled, "", nil
	}
	switch mode {
	case FallbackFail:
		return nil, "", missingSkillError(workspacePath, missing)
	case FallbackAutoInstall:
		if list == nil {
			return nil, "", fmt.Errorf("skill: --auto-install requires a blessed list")
		}
		for _, name := range missing {
			url, _, rerr := ResolveSource(name, list)
			if rerr != nil {
				return nil, "", fmt.Errorf("auto-install: %w", missingSkillError(workspacePath, missing))
			}
			ref := list.Skills[name].Ref
			if _, ierr := pool.Install(ctx, url, name, InstallOptions{Cloner: cloner, Ref: ref}); ierr != nil {
				return nil, "", fmt.Errorf("auto-install %s failed: %w", name, ierr)
			}
		}
		return enabled, "", nil
	case FallbackAllowMissing:
		out := make([]string, 0, len(enabled)-len(missing))
		for _, n := range enabled {
			if pool.Has(n) {
				out = append(out, n)
			}
		}
		return out, fmt.Sprintf("warning: skipped missing skills: %s", strings.Join(missing, ", ")), nil
	default:
		return nil, "", fmt.Errorf("skill: unknown fallback mode %d", mode)
	}
}

func missingSkillError(workspacePath string, missing []string) error {
	var b strings.Builder
	if workspacePath == "" {
		b.WriteString("workspace requires skills not in global pool:\n")
	} else {
		fmt.Fprintf(&b, "workspace %s requires skills not in global pool:\n", workspacePath)
	}
	for _, n := range missing {
		fmt.Fprintf(&b, "  - %s (enabled in tether.toml)\n", n)
	}
	b.WriteString("\ninstall with:\n")
	for _, n := range missing {
		fmt.Fprintf(&b, "  tether skill install %s\n", n)
	}
	b.WriteString("\nor spawn with available skills only:\n")
	if workspacePath == "" {
		b.WriteString("  tether spawn --allow-missing <workspace>\n")
	} else {
		fmt.Fprintf(&b, "  tether spawn --allow-missing %s\n", workspacePath)
	}
	return errors.New(b.String())
}

// --- helpers --------------------------------------------------------

func isGitURL(s string) bool {
	switch {
	case strings.HasPrefix(s, "http://"),
		strings.HasPrefix(s, "https://"),
		strings.HasPrefix(s, "git@"),
		strings.HasPrefix(s, "ssh://"),
		strings.HasPrefix(s, "git://"):
		return true
	}
	return strings.HasSuffix(s, ".git")
}

// gitURLBase derives "y" from "https://github.com/x/y(.git)" / "git@github.com:x/y(.git)".
func gitURLBase(url string) string {
	u := strings.TrimSuffix(url, ".git")
	u = strings.TrimSuffix(u, "/")
	// strip protocol + auth + host
	if i := strings.LastIndex(u, "/"); i >= 0 {
		u = u[i+1:]
	}
	if i := strings.LastIndex(u, ":"); i >= 0 {
		u = u[i+1:]
	}
	return u
}
