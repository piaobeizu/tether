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
