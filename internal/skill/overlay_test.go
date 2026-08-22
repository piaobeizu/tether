package skill

// tether#142 — the skill overlay's write path.
//
// Three defects, one theme: the two components of the overlay path
// (`<workspace>/.claude/plugins/<skillID>`) were both raw strings off an
// authenticated request, and the registry that was supposed to know better
// silently forgot what it had loaded.
//
// Every test below was written against the pristine tree first and observed to
// FAIL there — with the exception of the four marked "regression guard", which
// pin behaviour that was already correct and must survive the fix. That
// distinction is the point of naming them. (Counted, not estimated: an earlier
// version of this sentence said "two" and there were four.)

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// fakeIndex is a WorkspaceIndex over a fixed id→dir map. A map rather than a real
// workspace.Registry keeps these tests about the overlay's containment rule and
// not about workspace canonicalisation; internal/server/mux_skill_test.go is
// where the real registry is joined to the real route table.
type fakeIndex map[string]string

func (f fakeIndex) Path(id string) (string, bool) {
	p, ok := f[id]
	return p, ok
}

// newTestRegistry builds a registry backed by a temp file, with the given skills
// installed and the given index bound. It never touches the real ~/.tether.
func newTestRegistry(t *testing.T, ws WorkspaceIndex, skills ...Skill) *Registry {
	t.Helper()
	r := &Registry{path: filepath.Join(t.TempDir(), "skills.json"), skills: skills}
	if ws != nil {
		r.BindWorkspaces(ws)
	}
	return r
}

// installedSkill returns a skill whose source directory exists, plus that dir.
func installedSkill(t *testing.T, id string) (Skill, string) {
	t.Helper()
	src := t.TempDir()
	return Skill{ID: id, Name: "n-" + id, SourcePath: src}, src
}

// -- tether#147: what Install will accept ------------------------------------

// TestInstall_RefusesASourceThatIsNotAnExistingDirectory.
//
// Every doc in this package describes a skill as a DIRECTORY, and os.Stat alone
// accepted regular files: a plain file installed, and Enable then linked a
// registered workspace's plugins entry to it for cc to read. The dangling-symlink
// row is the same refusal through a third mechanism (os.Stat follows the link and
// finds nothing).
//
// Run against the pre-fix tree: the file row returned a nil error and a
// registration, and the missing/dangling rows returned `skill path not found:
// stat <path>: no such file or directory` — an error the HTTP layer sent verbatim.
//
// The "still empty" half is asserted separately for the same reason it is in the
// workspace package: an error return that has already appended to r.skills is a
// distinct defect from no error at all.
func TestInstall_RefusesASourceThatIsNotAnExistingDirectory(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(t *testing.T, base string) string
	}{
		{"does not exist", func(t *testing.T, base string) string {
			return filepath.Join(base, "not-there")
		}},
		{"exists but is a regular file", func(t *testing.T, base string) string {
			p := filepath.Join(base, "skill.md")
			if err := os.WriteFile(p, []byte("# not a skill dir"), 0o600); err != nil {
				t.Fatal(err)
			}
			return p
		}},
		{"is a dangling symlink", func(t *testing.T, base string) string {
			p := filepath.Join(base, "dangling")
			if err := os.Symlink(filepath.Join(base, "gone"), p); err != nil {
				t.Fatal(err)
			}
			return p
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := tc.build(t, t.TempDir())
			reg := newTestRegistry(t, fakeIndex{"ws1": t.TempDir()})

			sk, err := reg.Install("s", src)
			if !errors.Is(err, ErrSkillSourceUnusable) {
				t.Fatalf("Install(%q) = (%+v, %v), want ErrSkillSourceUnusable", src, sk, err)
			}
			if got := reg.List(); len(got) != 0 {
				t.Errorf("after a refused Install, List = %+v, want empty", got)
			}
		})
	}

	// Positive control: a real directory still installs, so the rows above cannot
	// be satisfied by an Install that refuses everything.
	t.Run("an existing directory is still accepted", func(t *testing.T) {
		reg := newTestRegistry(t, fakeIndex{"ws1": t.TempDir()})
		if _, err := reg.Install("s", t.TempDir()); err != nil {
			t.Fatalf("Install(a real directory) = %v, want it to install", err)
		}
		if got := reg.List(); len(got) != 1 {
			t.Errorf("List = %+v, want the one skill", got)
		}
	})
}

// -- the asymmetry: Disable did not check its skill id -----------------------

// TestDisable_RefusesAnIDThatIsNotAnInstalledSkill is the core of tether#142's
// third fact. Enable resolved skillID through the registry; Disable did not, and
// went straight to os.Remove. So an authenticated caller could name ANY entry in
// a plugins directory and have it deleted — including a plugin this daemon did
// not install and has no business touching.
func TestDisable_RefusesAnIDThatIsNotAnInstalledSkill(t *testing.T) {
	wsDir := t.TempDir()
	plugins := filepath.Join(wsDir, ".claude", "plugins")
	if err := os.MkdirAll(plugins, 0o755); err != nil {
		t.Fatal(err)
	}
	// Something the skill registry never created and must not destroy.
	bystander := filepath.Join(plugins, "installed-by-someone-else")
	if err := os.WriteFile(bystander, []byte("not ours"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := newTestRegistry(t, fakeIndex{"ws1": wsDir}) // nothing installed

	err := r.Disable("installed-by-someone-else", "ws1")
	if !errors.Is(err, ErrUnknownSkill) {
		t.Fatalf("Disable(unregistered id) error = %v, want ErrUnknownSkill", err)
	}
	if _, statErr := os.Stat(bystander); statErr != nil {
		t.Fatalf("the bystander entry was removed: %v", statErr)
	}
}

// TestDisable_RemovesTheLinkForAnInstalledSkill — the guard above must refuse the
// wrong id, not every id. Without this, deleting the os.Remove call entirely
// would leave the suite green.
func TestDisable_RemovesTheLinkForAnInstalledSkill(t *testing.T) {
	sk, src := installedSkill(t, "aaaa000000000001")
	wsDir := t.TempDir()
	r := newTestRegistry(t, fakeIndex{"ws1": wsDir}, sk)

	if err := r.Enable(sk.ID, "ws1"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	link := filepath.Join(wsDir, ".claude", "plugins", sk.ID)
	if got, err := os.Readlink(link); err != nil || got != src {
		t.Fatalf("Enable left %s -> %q (err %v), want -> %q", link, got, err, src)
	}

	if err := r.Disable(sk.ID, "ws1"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if _, err := os.Lstat(link); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("link still present after Disable: %v", err)
	}
}

// TestDisable_IsIdempotent — regression guard. A missing link was already a
// no-op, and adding the skill lookup must not turn "already disabled" into an
// error for a caller retrying.
func TestDisable_IsIdempotent(t *testing.T) {
	sk, _ := installedSkill(t, "aaaa000000000002")
	r := newTestRegistry(t, fakeIndex{"ws1": t.TempDir()}, sk)

	if err := r.Disable(sk.ID, "ws1"); err != nil {
		t.Fatalf("Disable with no link present: %v", err)
	}
}

// -- containment: the workspace is no longer a caller-supplied path ----------

// TestOverlayWrites_RefuseAnUnregisteredWorkspace covers facts 2 and 4 for BOTH
// endpoints in one table, because the defect was that the two disagreed.
//
// The pre-fix behaviour these replace: Enable created `.claude/plugins/` plus a
// symlink inside ANY directory named in the request body, and Disable removed
// from any such directory.
func TestOverlayWrites_RefuseAnUnregisteredWorkspace(t *testing.T) {
	sk, _ := installedSkill(t, "aaaa000000000003")
	registered := t.TempDir()
	outsider := t.TempDir() // a real directory that no registry vouched for

	for _, tc := range []struct {
		name string
		call func(r *Registry) error
	}{
		{"enable", func(r *Registry) error { return r.Enable(sk.ID, outsider) }},
		{"disable", func(r *Registry) error { return r.Disable(sk.ID, outsider) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestRegistry(t, fakeIndex{"ws1": registered}, sk)

			// Note what is being passed: the outsider DIRECTORY, in the position that
			// used to be a path and is now an id. That is the pre-fix request verbatim,
			// and it must now be a refusal rather than a write.
			if err := tc.call(r); !errors.Is(err, ErrUnknownWorkspace) {
				t.Fatalf("%s(outsider dir as id) error = %v, want ErrUnknownWorkspace", tc.name, err)
			}
			if _, err := os.Lstat(filepath.Join(outsider, ".claude")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("%s created something under the outsider directory: %v", tc.name, err)
			}
		})
	}
}

// TestEnable_LinksOnlyInsideTheRegisteredDirectory — the positive half: the
// directory written to is the one the INDEX returned, not anything the caller
// said. Passing an id whose mapped directory differs from the id itself is what
// makes that observable.
func TestEnable_LinksOnlyInsideTheRegisteredDirectory(t *testing.T) {
	sk, src := installedSkill(t, "aaaa000000000004")
	registered := t.TempDir()
	r := newTestRegistry(t, fakeIndex{"ws-id-not-a-path": registered}, sk)

	if err := r.Enable(sk.ID, "ws-id-not-a-path"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	link := filepath.Join(registered, ".claude", "plugins", sk.ID)
	got, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("expected a link at %s: %v", link, err)
	}
	if got != src {
		t.Fatalf("link target %q, want %q", got, src)
	}
}

// TestEnable_ReplacesAStaleLink — regression guard for the `_ = os.Remove(link)`
// line, which os.Symlink would otherwise fail on with EEXIST.
func TestEnable_ReplacesAStaleLink(t *testing.T) {
	sk, src := installedSkill(t, "aaaa000000000005")
	registered := t.TempDir()
	r := newTestRegistry(t, fakeIndex{"ws1": registered}, sk)

	if err := r.Enable(sk.ID, "ws1"); err != nil {
		t.Fatalf("first Enable: %v", err)
	}
	if err := r.Enable(sk.ID, "ws1"); err != nil {
		t.Fatalf("second Enable (stale link present): %v", err)
	}
	if got, _ := os.Readlink(filepath.Join(registered, ".claude", "plugins", sk.ID)); got != src {
		t.Fatalf("link target %q after re-enable, want %q", got, src)
	}
}

// TestOverlayWrites_RefuseAnUnknownSkill — both endpoints, symmetrically.
func TestOverlayWrites_RefuseAnUnknownSkill(t *testing.T) {
	registered := t.TempDir()
	r := newTestRegistry(t, fakeIndex{"ws1": registered})

	for name, call := range map[string]func() error{
		"enable":  func() error { return r.Enable("no-such-skill", "ws1") },
		"disable": func() error { return r.Disable("no-such-skill", "ws1") },
	} {
		if err := call(); !errors.Is(err, ErrUnknownSkill) {
			t.Errorf("%s(unknown skill) error = %v, want ErrUnknownSkill", name, err)
		}
	}
}

// -- fail-closed when there is no truth source ------------------------------

// TestOverlayWrites_RefuseWithNoIndexBound. A daemon whose workspace registry
// failed to load has no way to tell a workspace from any other directory, and the
// answer to that is to refuse, not to fall back to trusting the caller.
func TestOverlayWrites_RefuseWithNoIndexBound(t *testing.T) {
	sk, _ := installedSkill(t, "aaaa000000000006")
	r := newTestRegistry(t, nil, sk) // never bound

	for name, call := range map[string]func() error{
		"enable":  func() error { return r.Enable(sk.ID, "ws1") },
		"disable": func() error { return r.Disable(sk.ID, "ws1") },
	} {
		if err := call(); !errors.Is(err, ErrNoWorkspaceIndex) {
			t.Errorf("%s with no index bound: error = %v, want ErrNoWorkspaceIndex", name, err)
		}
	}
}

// TestBindWorkspaces_TreatsATypedNilAsUnbound.
//
// The trap lifecycle.go documents: a nil pointer stored in an interface makes the
// interface non-nil, so `ws == nil` is false and the next call is a nil-receiver
// call. For fakeIndex (a map type) and for any *T index, BindWorkspaces must
// store NOTHING rather than store the typed nil.
//
// # Why this asserts the stored field and not Enable's error
//
// It used to call Enable and check for ErrNoWorkspaceIndex, and review proved
// that was not a gate: workspaceDir performs the SAME nil check before it uses
// the index, so deleting BindWorkspaces' check left this green — the error came
// from the second guard, not the one under test. Only removing both turned it
// red. An assertion that holds whether or not the code under test is present is
// not an assertion about that code.
//
// So this reads r.ws directly. That is BindWorkspaces' whole observable effect,
// it is reachable because this test is in-package, and it fails for exactly one
// reason: BindWorkspaces stored something it was supposed to reject.
func TestBindWorkspaces_TreatsATypedNilAsUnbound(t *testing.T) {
	for name, idx := range map[string]WorkspaceIndex{
		"nil map type": fakeIndex(nil),
		"nil pointer":  (*nilPtrIndex)(nil),
	} {
		t.Run(name, func(t *testing.T) {
			r := &Registry{path: filepath.Join(t.TempDir(), "skills.json")}
			r.BindWorkspaces(idx)

			r.mu.RLock()
			stored := r.ws
			r.mu.RUnlock()
			if stored != nil {
				t.Fatalf("BindWorkspaces(%s) stored %#v; want nil, because a non-nil "+
					"interface wrapping a nil pointer is what hands a nil-receiver call to Path", name, stored)
			}
		})
	}
}

// TestBindWorkspaces_StoresAUsableIndex — the companion that keeps the test above
// from being satisfied by a BindWorkspaces that stores nothing at all.
func TestBindWorkspaces_StoresAUsableIndex(t *testing.T) {
	dir := t.TempDir()
	r := &Registry{path: filepath.Join(t.TempDir(), "skills.json")}
	r.BindWorkspaces(fakeIndex{"ws1": dir})

	r.mu.RLock()
	stored := r.ws
	r.mu.RUnlock()
	if stored == nil {
		t.Fatal("BindWorkspaces(non-nil index) stored nil")
	}
	if got, ok := stored.Path("ws1"); !ok || got != dir {
		t.Fatalf("stored index resolves ws1 to (%q, %v), want (%q, true)", got, ok, dir)
	}
}

// TestOverlayWrites_DoNotPanicOnATypedNilIndex keeps the end-to-end half of the
// old test: even if a typed nil reaches the field somehow, the write path refuses
// instead of dereferencing it. This one is deliberately about workspaceDir's
// guard, which is why it sets r.ws directly and bypasses BindWorkspaces.
func TestOverlayWrites_DoNotPanicOnATypedNilIndex(t *testing.T) {
	sk, _ := installedSkill(t, "aaaa000000000007")
	r := newTestRegistry(t, nil, sk)
	r.ws = (*nilPtrIndex)(nil) // what BindWorkspaces exists to prevent

	for name, call := range map[string]func() error{
		"enable":  func() error { return r.Enable(sk.ID, "ws1") },
		"disable": func() error { return r.Disable(sk.ID, "ws1") },
	} {
		if err := call(); !errors.Is(err, ErrNoWorkspaceIndex) {
			t.Errorf("%s with a typed-nil index: error = %v, want ErrNoWorkspaceIndex", name, err)
		}
	}
}

// nilPtrIndex exists only to be a nil *T that satisfies WorkspaceIndex. Its
// method would panic on a nil receiver, which is exactly the failure the guard
// prevents — so a regression here shows up as a panic, unmistakably.
type nilPtrIndex struct{ m map[string]string }

func (n *nilPtrIndex) Path(id string) (string, bool) {
	p, ok := n.m[id] // nil-receiver dereference
	return p, ok
}

// TestOverlayWrites_RefuseAWorkspaceWithANonAbsolutePath.
//
// A registry entry with an empty Path resolves to ("", true) — registered, but
// nowhere. filepath.Join("", ".claude", "plugins", id) is RELATIVE, so the
// overlay would land under the daemon's own working directory. `ok` alone does
// not catch this; the IsAbs check does.
func TestOverlayWrites_RefuseAWorkspaceWithANonAbsolutePath(t *testing.T) {
	sk, _ := installedSkill(t, "aaaa000000000008")

	for name, dir := range map[string]string{"empty": "", "relative": "some/where"} {
		t.Run(name, func(t *testing.T) {
			r := newTestRegistry(t, fakeIndex{"ws1": dir}, sk)
			if err := r.Enable(sk.ID, "ws1"); !errors.Is(err, ErrUnknownWorkspace) {
				t.Fatalf("Enable into a %s registered path: error = %v, want ErrUnknownWorkspace", name, err)
			}
		})
	}
}

// -- the filename half of the containment ------------------------------------

// TestOverlayWrites_RefuseARegistryEntryWhoseIDEscapesThePluginsDir.
//
// The traversal this blocks was LIVE before tether#142, and not in the shape the
// work item described. net/http's ServeMux cleans the ESCAPED request path while
// the handler reads the DECODED one, so `/api/v1/skills/%2e%2e/disable` survives
// cleaning and arrives with an id of ".." — and filepath.Join collapses it, so
// the old Disable removed `<workspace>/.claude` itself rather than an entry inside
// it.
//
// Requiring the id to be an installed skill closed that over HTTP, since ids come
// from newSkillID. This test pins the remaining structural claim: even if such an
// id IS in the registry — load() validates nothing it reads, so a hand-edited
// skills.json can put one there — the join refuses. Without it, the traversal
// property rests on two facts about other functions and is asserted nowhere.
func TestOverlayWrites_RefuseARegistryEntryWhoseIDEscapesThePluginsDir(t *testing.T) {
	registered := t.TempDir()
	// A canary the traversal would destroy: `<ws>/.claude` is what Join("..")
	// resolves to from inside the plugins directory.
	canary := filepath.Join(registered, ".claude")
	if err := os.MkdirAll(canary, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, badID := range []string{"..", ".", "", "../..", "sub/dir", "/etc"} {
		t.Run("id="+badID, func(t *testing.T) {
			r := newTestRegistry(t, fakeIndex{"ws1": registered},
				Skill{ID: badID, Name: "hand-edited", SourcePath: t.TempDir()})

			for name, call := range map[string]func() error{
				"enable":  func() error { return r.Enable(badID, "ws1") },
				"disable": func() error { return r.Disable(badID, "ws1") },
			} {
				err := call()
				// An empty id cannot be looked up at all, so it is refused one step
				// earlier; either refusal is correct, a write is not.
				if !errors.Is(err, ErrUnsafeSkillID) && !errors.Is(err, ErrUnknownSkill) {
					t.Errorf("%s(%q) error = %v, want ErrUnsafeSkillID (or ErrUnknownSkill)", name, badID, err)
				}
			}
			if _, err := os.Stat(canary); err != nil {
				t.Fatalf("the directory above the plugins dir was destroyed by id %q: %v", badID, err)
			}
		})
	}
}

// -- tether#156: nothing BELOW the root was ever checked ---------------------
//
// tether#142 made the overlay's directory come from the registry instead of from
// the request body, and the change that shipped it asserted that an overlay can
// only be written into a directory the registry holds. That sentence was not
// true. No component between the workspace root and the link was ever lstat'd,
// so a symlink at either of the two of them redirected os.MkdirAll, os.Symlink
// AND os.Remove out of the workspace — a write primitive and a delete primitive,
// anywhere on the host, for a caller that can put one link inside a workspace it
// is allowed to use. Not one of the 21 tests tether#142 added planted a link,
// which is exactly why they all stayed green over it.
//
// Every test in this section was run against the unfixed tree first (with only
// the new error sentinels declared, so that it compiled) and observed to fail
// there. The failures are recorded per test, and they are the ESCAPE — an entry
// created outside the root, a file deleted outside the root — not merely a
// missing error value.

// resolvedTempDir is t.TempDir() with every symlink resolved.
//
// A containment assertion compares a path against the registered root, so a
// fixture root that itself passes through a link (macOS resolves TMPDIR through
// /private) would make the comparison answer about the fixture instead of about
// the code — in either direction, and silently. Resolving once here also gives
// the fixture the shape production has: workspace.Add stores canonicalPath's
// output, which is resolved whenever it can be.
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", dir, err)
	}
	return resolved
}

// escapeLayer is one of the two directory components between the registered root
// and the overlay link. Both are inside a workspace the caller is entitled to
// use, both are followed by every filesystem call the overlay makes, and the fix
// has to refuse at both — which is why they are a table and not one test.
type escapeLayer struct {
	name string
	// seed plants the symlink and returns the directory the overlay lands in when
	// that link is followed.
	seed func(t *testing.T, root, outside string) (landing string)
}

func escapeLayers() []escapeLayer {
	return []escapeLayer{
		{
			// The overlay's OWN directory is the link.
			name: "the plugins directory is a symlink",
			seed: func(t *testing.T, root, outside string) string {
				t.Helper()
				if err := os.MkdirAll(filepath.Join(root, ".claude"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(root, ".claude", "plugins")); err != nil {
					t.Fatal(err)
				}
				return outside
			},
		},
		{
			// Its PARENT is the link, so MkdirAll creates `plugins` on the far side.
			name: "the .claude directory is a symlink",
			seed: func(t *testing.T, root, outside string) string {
				t.Helper()
				if err := os.Symlink(outside, filepath.Join(root, ".claude")); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(outside, "plugins")
			},
		},
	}
}

// TestEnable_RefusesToFollowASymlinkOutOfTheWorkspace.
//
// Observed on the unfixed tree, both rows: Enable returned nil and the directory
// outside the workspace gained an entry — the skill id for the first layer
// (`<outside>/<skillID>` → the skill source), `plugins` for the second
// (`<outside>/plugins/<skillID>`). Post-fix that directory holds zero entries and
// the call returns ErrOverlayEscapesWorkspace.
//
// The count is the assertion that answers "what is this value when the defect is
// present", and it is checked BEFORE the error so a failure reports the escape
// itself rather than the symptom.
func TestEnable_RefusesToFollowASymlinkOutOfTheWorkspace(t *testing.T) {
	for _, layer := range escapeLayers() {
		t.Run(layer.name, func(t *testing.T) {
			sk, src := installedSkill(t, "cccc000000000001")
			root := resolvedTempDir(t)
			outside := resolvedTempDir(t)
			layer.seed(t, root, outside)

			r := newTestRegistry(t, fakeIndex{"ws1": root}, sk)
			err := r.Enable(sk.ID, "ws1")

			entries, readErr := os.ReadDir(outside)
			if readErr != nil {
				t.Fatalf("read the directory outside the workspace: %v", readErr)
			}
			if len(entries) != 0 {
				names := make([]string, 0, len(entries))
				for _, e := range entries {
					names = append(names, e.Name())
				}
				overlayDir := filepath.Join(root, ".claude", "plugins")
				landed, _ := filepath.EvalSymlinks(overlayDir)
				t.Errorf("Enable created %v inside %s, which is OUTSIDE the registered workspace %s\n"+
					"  the overlay directory %s resolves to %s\n"+
					"  the link points at the skill source %s\n"+
					"  so an authenticated caller who can place one symlink inside a workspace it may "+
					"use has a write primitive anywhere this daemon's user can write",
					names, outside, root, overlayDir, landed, src)
			}
			if !errors.Is(err, ErrOverlayEscapesWorkspace) {
				t.Errorf("Enable with %s: error = %v, want ErrOverlayEscapesWorkspace", layer.name, err)
			}
		})
	}
}

// TestDisable_RefusesToFollowASymlinkOutOfTheWorkspace — the same two layers on
// the delete path, which is the half that is easy to forget: Disable's os.Remove
// walks the identical join.
//
// Observed on the unfixed tree, both rows: Disable returned nil and the victim
// file outside the workspace was gone. It is a plain file rather than a link so
// that its destruction cannot be confused with os.Remove correctly unlinking a
// symlink.
func TestDisable_RefusesToFollowASymlinkOutOfTheWorkspace(t *testing.T) {
	for _, layer := range escapeLayers() {
		t.Run(layer.name, func(t *testing.T) {
			sk, _ := installedSkill(t, "cccc000000000002")
			root := resolvedTempDir(t)
			outside := resolvedTempDir(t)
			landing := layer.seed(t, root, outside)

			if err := os.MkdirAll(landing, 0o700); err != nil {
				t.Fatal(err)
			}
			victim := filepath.Join(landing, sk.ID)
			if err := os.WriteFile(victim, []byte("not this daemon's to delete"), 0o600); err != nil {
				t.Fatal(err)
			}

			r := newTestRegistry(t, fakeIndex{"ws1": root}, sk)
			err := r.Disable(sk.ID, "ws1")

			if _, statErr := os.Lstat(victim); statErr != nil {
				t.Errorf("Disable deleted %s, which is OUTSIDE the registered workspace %s: %v\n"+
					"  the same join that writes the overlay also removes it, so an escape on the "+
					"write path is an arbitrary-delete on the way back", victim, root, statErr)
			}
			if !errors.Is(err, ErrOverlayEscapesWorkspace) {
				t.Errorf("Disable with %s: error = %v, want ErrOverlayEscapesWorkspace", layer.name, err)
			}
		})
	}
}

// TestOverlayWrites_RefuseARegisteredPathThatIsNotItsOwnResolution — the root's
// own half of the same defect.
//
// workspace.canonicalPath resolves symlinks when it can and silently keeps the
// unresolved absolute path when it cannot (a directory that did not exist yet, a
// volume that was not mounted), which is the right policy there — it matches cc's
// own, and it is what lets a not-yet-created directory be bookmarked. It means
// the stored string is a best-effort answer, and this package is where that
// maybe has to stop: an overlay write measures containment against the stored
// path, so the stored path has to BE the directory, not a name for it.
//
// Verified rather than sanitised: the legal form is "a path that resolves to
// itself", which is one comparison, instead of a list of the ways a path can
// name something else.
//
// Observed on the unfixed tree: enable returned nil and `<real>/.claude` existed.
func TestOverlayWrites_RefuseARegisteredPathThatIsNotItsOwnResolution(t *testing.T) {
	sk, _ := installedSkill(t, "cccc000000000003")
	base := resolvedTempDir(t)
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	registered := filepath.Join(base, "registered-through-a-link")
	if err := os.Symlink(real, registered); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"enable", "disable"} {
		t.Run(name, func(t *testing.T) {
			r := newTestRegistry(t, fakeIndex{"ws1": registered}, sk)
			var err error
			if name == "enable" {
				err = r.Enable(sk.ID, "ws1")
			} else {
				err = r.Disable(sk.ID, "ws1")
			}

			entries, readErr := os.ReadDir(real)
			if readErr != nil {
				t.Fatalf("read the link's target: %v", readErr)
			}
			if len(entries) != 0 {
				t.Errorf("%s wrote through the registered path's symlink: %s holds %d entries, "+
					"and the registry says this workspace is at %s", name, real, len(entries), registered)
			}
			if !errors.Is(err, ErrOverlayEscapesWorkspace) {
				t.Errorf("%s into a registered path that resolves elsewhere: error = %v, want ErrOverlayEscapesWorkspace", name, err)
			}
		})
	}
}

// TestEnable_RefusesAWorkspaceDirectoryThatIsNotThere.
//
// A registration is a bookmark — canonicalPath deliberately allows one to a
// directory that does not exist yet — and creating that directory is not
// something an overlay write should be able to do. Refusing is also the honest
// answer to "is this contained": a directory that is not there cannot be
// resolved, and "I cannot check" must be a refusal rather than a pass.
//
// Observed on the unfixed tree: for "not there" Enable returned nil, having
// created the registered directory and two levels beneath it with os.MkdirAll;
// for "a regular file" it returned a bare `mkdir <path>: not a directory`, which
// the HTTP layer called a 500.
//
// The two rows are separate because they are refused by separate checks — the
// second row is the only thing that fails if the "is it a directory" test goes
// away, since a path that is not there fails to resolve as well.
func TestEnable_RefusesAWorkspaceDirectoryItCannotUse(t *testing.T) {
	base := resolvedTempDir(t)
	regular := filepath.Join(base, "a-file-not-a-directory")
	if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	for name, root := range map[string]string{
		"the directory is not there": filepath.Join(base, "never-created"),
		"it is a regular file":       regular,
	} {
		t.Run(name, func(t *testing.T) {
			sk, _ := installedSkill(t, "cccc000000000004")
			r := newTestRegistry(t, fakeIndex{"ws1": root}, sk)

			err := r.Enable(sk.ID, "ws1")

			if !errors.Is(err, ErrWorkspaceDirUnusable) {
				t.Errorf("Enable into a registered path where %s: error = %v, want ErrWorkspaceDirUnusable", name, err)
			}
			// Discriminating for the first row only — os.MkdirAll used to create the
			// registered directory and both levels under it — and kept for the
			// second because it costs nothing; there the error identity is the gate.
			if _, statErr := os.Lstat(filepath.Join(root, ".claude")); statErr == nil {
				t.Errorf("Enable created %s/.claude; a registration is a bookmark, and an overlay "+
					"write must not materialise its target", root)
			}
		})
	}
}

// TestDisable_IsANoOpWhenTheRegisteredDirectoryIsGone — regression guard, and the
// companion the test above needs.
//
// Removing a link from a tree that is not there is already done, so this must not
// inherit Enable's refusal: DELETE-ing overlays for a workspace whose directory
// the user removed is exactly when a caller is tidying up, and an error there
// would make the tidying impossible. It also must not CREATE the tree on the way
// to discovering there is nothing to remove.
func TestDisable_IsANoOpWhenTheRegisteredDirectoryIsGone(t *testing.T) {
	sk, _ := installedSkill(t, "cccc000000000005")
	root := filepath.Join(resolvedTempDir(t), "never-created")
	r := newTestRegistry(t, fakeIndex{"ws1": root}, sk)

	if err := r.Disable(sk.ID, "ws1"); err != nil {
		t.Errorf("Disable against a registered directory that is not on disk: error = %v, want nil", err)
	}
	if _, statErr := os.Lstat(root); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("Disable created %s on its way to finding nothing to remove (Lstat = %v)", root, statErr)
	}
}

// TestEnable_RefusesWhenTheOverlayNameIsOccupied.
//
// `_ = os.Remove(link)` discarded the one failure it could actually hit. A
// non-empty directory at the overlay's name fails ENOTEMPTY, the discarded error
// left the name in place, and os.Symlink then failed EEXIST — which the HTTP
// layer mapped to 500 with the daemon's own absolute path in the body. Three
// wrong answers from one swallowed error: a caller-visible state reported as a
// daemon fault, an unactionable message, and a path disclosure.
//
// Observed on the unfixed tree: error = "symlink <src> <link>: file exists"
// (*os.LinkError), matching neither sentinel.
func TestEnable_RefusesWhenTheOverlayNameIsOccupied(t *testing.T) {
	sk, _ := installedSkill(t, "cccc000000000006")
	root := resolvedTempDir(t)
	occupied := filepath.Join(root, ".claude", "plugins", sk.ID)
	if err := os.MkdirAll(occupied, 0o700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(occupied, "someone-elses-plugin.json")
	if err := os.WriteFile(keep, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := newTestRegistry(t, fakeIndex{"ws1": root}, sk)
	err := r.Enable(sk.ID, "ws1")

	if !errors.Is(err, ErrOverlayLocationOccupied) {
		t.Errorf("Enable onto an occupied name: error = %v, want ErrOverlayLocationOccupied", err)
	}
	if _, statErr := os.Stat(keep); statErr != nil {
		t.Errorf("the occupant's contents were destroyed: %v", statErr)
	}
}

// TestEnable_ReplacesAStaleLinkButNotADirectory — the companion that keeps the
// test above from being satisfied by an Enable that refuses everything it finds.
// A stale SYMLINK is exactly what os.Remove is there to clear, and clearing it
// must survive the new refusal (TestEnable_ReplacesAStaleLink covers the
// same-target case; this one changes the target so the replacement is visible).
func TestEnable_ReplacesAStaleLinkButNotADirectory(t *testing.T) {
	sk, src := installedSkill(t, "cccc000000000007")
	root := resolvedTempDir(t)
	plugins := filepath.Join(root, ".claude", "plugins")
	if err := os.MkdirAll(plugins, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "somewhere-else"), filepath.Join(plugins, sk.ID)); err != nil {
		t.Fatal(err)
	}

	r := newTestRegistry(t, fakeIndex{"ws1": root}, sk)
	if err := r.Enable(sk.ID, "ws1"); err != nil {
		t.Fatalf("Enable over a stale link: %v", err)
	}
	if got, err := os.Readlink(filepath.Join(plugins, sk.ID)); err != nil || got != src {
		t.Fatalf("link -> %q (err %v), want the current source %q", got, err, src)
	}
}

// TestEnable_CreatesTheOverlayDirectoriesPrivate.
//
// 0o700, the mode NewRegistry already uses for ~/.tether and ~/.tether/skills.
// The overlay is daemon state that happens to live inside a user directory, and
// it names, by directory entry, every skill enabled for that workspace; there is
// no reader of it but cc, running as this daemon's user.
//
// Observed on the unfixed tree: 0o755 for both, from a single os.MkdirAll.
// Only directories this call creates are affected — an existing `.claude` that cc
// made is left exactly as it is.
func TestEnable_CreatesTheOverlayDirectoriesPrivate(t *testing.T) {
	sk, _ := installedSkill(t, "cccc000000000008")
	root := resolvedTempDir(t)
	r := newTestRegistry(t, fakeIndex{"ws1": root}, sk)

	if err := r.Enable(sk.ID, "ws1"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	for _, rel := range []string{".claude", filepath.Join(".claude", "plugins")} {
		fi, err := os.Lstat(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("Lstat %s: %v", rel, err)
		}
		if got := fi.Mode().Perm(); got != 0o700 {
			t.Errorf("%s created with mode %04o, want 0700 — the same mode NewRegistry gives ~/.tether", rel, got)
		}
	}
}

// TestDisableAll_RemovesTheOverlaysThisDaemonCreated is the teardown
// DELETE /api/v1/workspaces/{id} needs: the registration is what authorised every
// one of these links, so the registration going away has to take them with it.
// Once the record is gone the id stops resolving and Disable can no longer reach
// them at all, which is what made the leftovers unreachable rather than merely
// untidy.
//
// The two bystanders are the reason this is a removal and not a wipe of the
// plugins directory: a name this daemon never issued is not this daemon's to
// delete, whether it is a file or a link.
func TestDisableAll_RemovesTheOverlaysThisDaemonCreated(t *testing.T) {
	one, _ := installedSkill(t, "dddd000000000001")
	two, _ := installedSkill(t, "dddd000000000002")
	root := resolvedTempDir(t)
	r := newTestRegistry(t, fakeIndex{"ws1": root}, one, two)

	for _, sk := range []Skill{one, two} {
		if err := r.Enable(sk.ID, "ws1"); err != nil {
			t.Fatalf("Enable %s: %v", sk.ID, err)
		}
	}
	plugins := filepath.Join(root, ".claude", "plugins")
	bystanderFile := filepath.Join(plugins, "installed-by-someone-else")
	if err := os.WriteFile(bystanderFile, []byte("not ours"), 0o600); err != nil {
		t.Fatal(err)
	}
	bystanderLink := filepath.Join(plugins, "linked-by-someone-else")
	if err := os.Symlink(root, bystanderLink); err != nil {
		t.Fatal(err)
	}

	if err := r.DisableAll("ws1"); err != nil {
		t.Fatalf("DisableAll: %v", err)
	}

	for _, sk := range []Skill{one, two} {
		if _, err := os.Lstat(filepath.Join(plugins, sk.ID)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("overlay for %s survived DisableAll (Lstat = %v)", sk.ID, err)
		}
	}
	for _, keep := range []string{bystanderFile, bystanderLink} {
		if _, err := os.Lstat(keep); err != nil {
			t.Errorf("DisableAll destroyed %s, which this daemon never created: %v", keep, err)
		}
	}
}

// TestDisableAll_DoesNotFollowASymlinkOutOfTheWorkspace — teardown is a delete
// primitive too, so it gets the containment property rather than borrowing the
// caller's trust. It reports success (there is nothing of this daemon's on the
// far side of a link it would have refused to write through) and touches nothing.
func TestDisableAll_DoesNotFollowASymlinkOutOfTheWorkspace(t *testing.T) {
	for _, layer := range escapeLayers() {
		t.Run(layer.name, func(t *testing.T) {
			sk, _ := installedSkill(t, "dddd000000000003")
			root := resolvedTempDir(t)
			outside := resolvedTempDir(t)
			landing := layer.seed(t, root, outside)
			if err := os.MkdirAll(landing, 0o700); err != nil {
				t.Fatal(err)
			}
			victim := filepath.Join(landing, sk.ID)
			if err := os.WriteFile(victim, []byte("not this daemon's to delete"), 0o600); err != nil {
				t.Fatal(err)
			}

			r := newTestRegistry(t, fakeIndex{"ws1": root}, sk)
			if err := r.DisableAll("ws1"); err != nil {
				t.Errorf("DisableAll with %s: error = %v, want nil — a workspace whose overlay path "+
					"escapes must still be removable from the registry", layer.name, err)
			}
			if _, statErr := os.Lstat(victim); statErr != nil {
				t.Errorf("DisableAll deleted %s, OUTSIDE the registered workspace %s: %v", victim, root, statErr)
			}
		})
	}
}

// TestRemove_RefusesASkillIDThatIsNotInstalled — tether#156 fact 5.
//
// DELETE /api/v1/skills/{unknown} answered 204 while enable and disable answered
// 404 for the same id, and the asymmetry had no test at all: api_test.go only
// ever deleted an id it had just created. 404 is the semantic that was already
// written down — "the id in the URL is not a skill; 404 is about the addressed
// resource" — so the DELETE is what moves, and it moves by reporting the same
// sentinel the other two do.
//
// Observed on the unfixed tree: error = nil.
func TestRemove_RefusesASkillIDThatIsNotInstalled(t *testing.T) {
	sk, _ := installedSkill(t, "eeee000000000001")
	r := newTestRegistry(t, fakeIndex{"ws1": resolvedTempDir(t)}, sk)

	if err := r.Remove("no-such-skill"); !errors.Is(err, ErrUnknownSkill) {
		t.Errorf("Remove(unknown id) error = %v, want ErrUnknownSkill", err)
	}
	if got := r.List(); len(got) != 1 {
		t.Errorf("List = %+v, want the one installed skill untouched", got)
	}
	if err := r.Remove(sk.ID); err != nil {
		t.Errorf("Remove(installed id) error = %v, want nil", err)
	}
	if got := r.List(); len(got) != 0 {
		t.Errorf("List after removing the installed skill = %+v, want empty", got)
	}
}

// -- fact 5: a swallowed load error became a silent, irreversible wipe -------

// TestLoad_DistinguishesAMissingFileFromACorruptOne is the distinction the whole
// fix turns on, asserted directly on the classifier rather than inferred from
// NewRegistry's behaviour. Both cases return an error; only one of them means
// "something is wrong".
func TestLoad_DistinguishesAMissingFileFromACorruptOne(t *testing.T) {
	dir := t.TempDir()

	missing := &Registry{path: filepath.Join(dir, "absent.json")}
	if err := missing.load(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("load(missing file) error = %v, want an fs.ErrNotExist", err)
	}

	corruptPath := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corruptPath, []byte(`[{"id":"a","name":`), 0o600); err != nil {
		t.Fatal(err)
	}
	corrupt := &Registry{path: corruptPath}
	err := corrupt.load()
	if err == nil {
		t.Fatal("load(corrupt file) returned nil")
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("load(corrupt file) error = %v, must NOT classify as fs.ErrNotExist", err)
	}
}

// TestNewRegistry_AMissingFileIsFirstRun — regression guard, and the half that
// makes the fix safe to ship: the very first start of a fresh daemon has no
// skills.json, and that must not be an error.
func TestNewRegistry_AMissingFileIsFirstRun(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	r, err := NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry on a fresh home: %v", err)
	}
	if got := r.List(); len(got) != 0 {
		t.Fatalf("List on first run = %v, want empty", got)
	}
}

// TestNewRegistry_ACorruptFileIsReportedAndLeftAlone.
//
// `_ = r.load()` did not merely lose the parse error. It produced a healthy-
// looking registry with nil skills, and the next Install or Remove called
// saveLocked, renaming a marshalled `[]` over the file — so the user's install
// records were destroyed by the daemon's own recovery path, with no error
// anywhere. Returning the error is what stops that: lifecycle.go leaves
// cfg.SkillRegistry nil, so no write path is reachable and the bytes survive for
// an operator to fix.
func TestNewRegistry_ACorruptFileIsReportedAndLeftAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".tether", "skills.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte(`[{"id":"deadbeefdeadbeef","name":"important","sourcePath":"/srv/s"}` /* truncated on purpose */)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}

	r, err := NewRegistry()
	if err == nil {
		t.Fatalf("NewRegistry on a corrupt skills.json returned no error (registry=%v)", r)
	}
	if r != nil {
		t.Fatalf("NewRegistry returned a usable registry alongside the error: %v", r)
	}

	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("re-read skills.json: %v", readErr)
	}
	if string(after) != string(original) {
		t.Fatalf("skills.json was rewritten: %q, want the original %q", after, original)
	}
}

// TestNewRegistry_LoadsAValidFile — regression guard: the error classification
// must not have broken the ordinary restart path.
func TestNewRegistry_LoadsAValidFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".tether", "skills.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`[{"id":"abc0000000000001","name":"kept","sourcePath":"/srv/s"}]`), 0o600); err != nil {
		t.Fatal(err)
	}

	r, err := NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry on a valid file: %v", err)
	}
	got := r.List()
	if len(got) != 1 || got[0].ID != "abc0000000000001" || got[0].Name != "kept" {
		t.Fatalf("List = %+v, want the one skill from the file", got)
	}
}
