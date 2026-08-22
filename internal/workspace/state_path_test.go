package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestRegistry_Path — the id→path lookup the chat route's `?ws=` parameter is
// resolved through (tether#52, session.WorkspaceLookup).
//
// The false second return is the whole contract: an unregistered id must be
// DISTINGUISHABLE from a registered one, because the caller turns it into a
// refusal. If a miss were reported as an empty path with no flag, a caller could
// spend it as "use the default directory" and an unknown id would silently select
// the daemon's own workspace root instead of being rejected.
func TestRegistry_Path(t *testing.T) {
	dir := t.TempDir()
	r := &Registry{path: dir + "/workspaces.json"}

	added, err := r.Add("project-a", dir)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, ok := r.Path(added.ID)
	if !ok {
		t.Fatal("Path = not found for a registered workspace")
	}
	if got != added.Path {
		t.Errorf("Path = %q, want %q", got, added.Path)
	}

	for _, id := range []string{"", "deadbeefdeadbeef", added.ID + "x"} {
		if p, ok := r.Path(id); ok {
			t.Errorf("Path(%q) = (%q, true), want not-found", id, p)
		}
	}

	// Removing a workspace un-resolves its id, so a stale id the browser is still
	// holding is refused rather than honoured.
	if err := r.Remove(added.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if p, ok := r.Path(added.ID); ok {
		t.Errorf("Path after Remove = (%q, true), want not-found", p)
	}
}

// -- tether#147: what Add will accept ----------------------------------------
//
// Both tests below were run against the pre-fix tree and observed to FAIL there.
// The pre-fix values are stated in the failure messages rather than described,
// because "it refuses now" is only half of a gate — the other half is that the
// value was different before, and each assertion here has to answer what it was.
//
// The two are kept apart, and each is arranged so the OTHER clause cannot keep it
// green:
//
//	clause                        | the test that dies without it
//	------------------------------+----------------------------------------------
//	Add requires an absolute path | TestAdd_RefusesARelativePath (its fixture is a
//	                              | directory that EXISTS, so the existence check
//	                              | cannot refuse it)
//	Add requires a directory that | TestAdd_RefusesAPathThatIsNotAnExistingDirectory
//	already exists                | (its fixtures are all absolute, so the IsAbs
//	                              | check cannot refuse them)

// TestAdd_RefusesAPathThatIsNotAnExistingDirectory.
//
// Three shapes of the same refusal, and the third is the one that would be missed
// by reading the code: canonicalPath cannot resolve a dangling symlink, so it
// hands back the link itself, and it is os.Stat following that link to nothing
// which refuses it.
//
// The second half of every row — that the registry is still EMPTY, on disk as
// well as in memory — is asserted separately on purpose. "Answers 400 and writes
// the entry anyway" is a distinct defect from "answers 201", and an assertion on
// the status alone holds in both states.
//
// This test is where the tether#147 reversal is asserted. Its counterpart is
// TestLoad_KeepsAnEntryWhoseDirectoryIsGone, which pins the half that was KEPT:
// load() must still accept an entry naming a directory that is not there, or one
// missing volume makes the whole registry unloadable. The two must be read
// together — either one alone reads as a policy the codebase does not have.
// canonicalPath's own fallback, which both of them rest on, is
// TestCanonicalPathKeepsWhatItCannotResolve's.
func TestAdd_RefusesAPathThatIsNotAnExistingDirectory(t *testing.T) {
	for _, tc := range []struct {
		name string
		// build returns an absolute path that must be refused.
		build func(t *testing.T, base string) string
	}{
		{"does not exist", func(t *testing.T, base string) string {
			return filepath.Join(base, "not-created-yet")
		}},
		{"exists but is a regular file", func(t *testing.T, base string) string {
			p := filepath.Join(base, "a-file")
			if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
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
			base := t.TempDir()
			path := tc.build(t, base)
			file := filepath.Join(t.TempDir(), "workspaces.json")
			r := &Registry{path: file}

			ws, err := r.Add("w", path)
			if !errors.Is(err, ErrWorkspacePathUnusable) {
				t.Fatalf("Add(%q) = (%+v, %v), want ErrWorkspacePathUnusable\n"+
					"Pre-fix this returned a nil error and a registration.", path, ws, err)
			}
			if got := r.List(); len(got) != 0 {
				t.Errorf("after a refused Add, List = %+v, want empty — a refusal that still "+
					"records the entry is the same defect wearing a 400", got)
			}
			if _, statErr := os.Stat(file); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("a refused Add wrote %s (stat error %v), want no file at all — "+
					"nothing must reach workspaces.json", file, statErr)
			}
		})
	}

	// Positive control, so the three rows above cannot be satisfied by an Add that
	// refuses everything. This is also the assertion that the file IS written on
	// the accepted path, which is what makes the "no file" checks above mean
	// something.
	t.Run("an existing directory is still accepted", func(t *testing.T) {
		dir := t.TempDir()
		file := filepath.Join(t.TempDir(), "workspaces.json")
		r := &Registry{path: file}

		ws, err := r.Add("w", dir)
		if err != nil {
			t.Fatalf("Add(%q) = %v, want it to register", dir, err)
		}
		if len(r.List()) != 1 {
			t.Errorf("List = %+v, want the one workspace", r.List())
		}
		if _, statErr := os.Stat(file); statErr != nil {
			t.Errorf("stat %s after an accepted Add: %v, want the registry written", file, statErr)
		}
		if ws.Path == "" || !filepath.IsAbs(ws.Path) {
			t.Errorf("stored path = %q, want an absolute path", ws.Path)
		}
	})
}

// TestAdd_RefusesARelativePath.
//
// The fixture is a directory that EXISTS, named relative to a working directory
// this test sets. That is the whole design of the test: filepath.Abs would have
// resolved it to a real directory, so the existence check cannot refuse it and
// the IsAbs check is the only thing that can. Remove the IsAbs line and this
// registers with a 201, which is exactly what it did before tether#147.
//
// t.Chdir rather than a hand-rolled os.Chdir with a defer: it restores the
// working directory itself and it fails the test if anything else in the package
// is running in parallel, which is the failure mode a manual version hides.
func TestAdd_RefusesARelativePath(t *testing.T) {
	parent := t.TempDir()
	if err := os.Mkdir(filepath.Join(parent, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(parent)

	file := filepath.Join(t.TempDir(), "workspaces.json")
	r := &Registry{path: file}

	for _, rel := range []string{"child", "./child", filepath.Join("..", filepath.Base(parent), "child")} {
		ws, err := r.Add("w", rel)
		if !errors.Is(err, ErrWorkspacePathNotAbsolute) {
			t.Errorf("Add(%q) = (%+v, %v), want ErrWorkspacePathNotAbsolute\n"+
				"Pre-fix this resolved against the DAEMON's working directory and registered "+
				"%q, which the caller had no way to predict.",
				rel, ws, err, filepath.Join(parent, "child"))
		}
	}
	if got := r.List(); len(got) != 0 {
		t.Errorf("after refused Adds, List = %+v, want empty", got)
	}

	// The same directory, named absolutely, registers — so the rows above are
	// refusals of the FORM of the path and not of the directory.
	if _, err := r.Add("w", filepath.Join(parent, "child")); err != nil {
		t.Fatalf("Add(the same directory, absolute) = %v, want it to register — "+
			"if this fails the fixture is at fault, not the check", err)
	}
}

// TestAdd_RegistersNothingWhenTheWriteFails — tether#159.
//
// The two tests above assert the on-disk state for the paths Add REFUSES. This is
// the case they left uncovered: a path Add accepts, whose write then fails.
//
// # Both halves are asserted, because either alone passes on the broken build
//
// Pre-fix, Add appended to r.workspaces and returned saveLocked's error with the
// entry still in it. So:
//
//   - "the error came back" was TRUE before the fix (it always was — this is not
//     a test of the error);
//   - "nothing is on disk" was TRUE before the fix (the write is what failed);
//   - "nothing is in memory" was FALSE before the fix — List() returned one
//     workspace that no file had ever held, which GET /api/v1/workspaces listed,
//     Path resolved for the /wt/chat handshake, and skill.Enable would have
//     written an overlay into, all of it gone at the next restart.
//
// So the memory half is the gate and the disk half is what makes the pair mean
// "consistent" rather than just "empty": a fix that stopped writing altogether
// would satisfy the memory half on its own.
//
// The fixture is the one route to a saveLocked failure that needs no mocking, and
// the same one TestRegistryMutations_DoNotEchoDaemonSideValues uses for its 500:
// a registry file under a directory that does not exist.
func TestAdd_RegistersNothingWhenTheWriteFails(t *testing.T) {
	file := filepath.Join(t.TempDir(), "no-such-directory-9e31", "workspaces.json")
	r := &Registry{path: file}

	ws, err := r.Add("w", t.TempDir())
	if err == nil {
		t.Fatalf("Add with an unwritable registry = (%+v, nil), want the write error — "+
			"the fixture is at fault if this fires", ws)
	}
	if ws != (Workspace{}) {
		t.Errorf("Add returned %+v alongside its error, want the zero Workspace: a value "+
			"naming a workspace nobody registered is the thing this test is about", ws)
	}
	if got := r.List(); len(got) != 0 {
		t.Errorf("List = %+v, want empty.\nPre-fix: exactly this — one in-memory "+
			"registration that saveLocked never persisted, so the daemon listed and "+
			"resolved a workspace that would vanish on restart.", got)
	}
	if _, statErr := os.Stat(file); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("stat %s = %v, want it absent — the other half of the same assertion: "+
			"memory and disk agree, and they agree on NOTHING", file, statErr)
	}

	// A writable registry still registers, so the rows above are not satisfied by
	// an Add that refuses everything.
	r.path = filepath.Join(t.TempDir(), "workspaces.json")
	if _, err := r.Add("w", t.TempDir()); err != nil {
		t.Fatalf("Add with a writable registry = %v, want it to register", err)
	}
	if got := r.List(); len(got) != 1 {
		t.Fatalf("List = %+v, want the one workspace", got)
	}
}
