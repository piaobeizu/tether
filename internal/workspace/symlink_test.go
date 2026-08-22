package workspace

// Regression tests for tether#125: the workspace registry recorded a path with
// filepath.Abs and nothing more, while the rest of the daemon compared that path
// against symlink-RESOLVED ones. Two failures that were reported independently,
// from opposite ends of the product, were that one asymmetry:
//
//   - a workspace registered through a symlink listed zero cc sessions, because
//     cc resolves a cwd before encoding it into a transcript directory name, so
//     the un-resolved path named a directory cc had never written;
//   - the @-mention file tree for that workspace returned exactly `["."]`,
//     because filepath.WalkDir lstats its root and a symlink root is therefore
//     not a directory to it.
//
// Neither produced an error or a log line, which is why both were found by
// reading rather than from a report.
//
// # Which test guards which clause
//
// The fix has four parts that can be reverted one at a time, and the point of
// splitting these tests the way they are split is that each part has a test that
// no OTHER part can keep green:
//
//	clause                                        | the test that dies without it
//	----------------------------------------------+------------------------------
//	Add canonicalises before storing              | TestAddStoresTheResolvedPath
//	load canonicalises what it read               | TestLoadCanonicalisesOldEntries
//	                                              | TestAddDedupsAnOldEntry
//	listFilesRecursive resolves its own root      | TestListFilesRecursiveResolvesItsRoot
//	canonicalPath falls back instead of erroring  | TestCanonicalPathKeepsWhatItCannotResolve
//	load keeps an entry it cannot resolve         | TestLoad_KeepsAnEntryWhoseDirectoryIsGone
//
// The last two are one mechanism read at two moments, and tether#147 split them
// apart on purpose: canonicalPath's fallback is what load() spends, so the row
// above it tests the primitive and the row below it tests the consequence that
// makes the primitive load-bearing. Registry.Add no longer spends it at all —
// it refuses first, and TestAdd_RefusesAPathThatIsNotAnExistingDirectory is that
// clause's gate.
//
// TestTreeHandlerServesASymlinkedWorkspace is deliberately NOT in that table: it
// is the user-visible end-to-end proof, and EITHER the registry clause or the
// listFilesRecursive clause is enough to keep it green on its own. It is kept
// because it demonstrates the two layers are independent, and it is named here as
// non-discriminating so nobody later mistakes it for the gate.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// symlinkedDir builds `real/` with some content and a sibling symlink pointing at
// it, returning (linkPath, realPath). The real path is returned already resolved
// so a comparison against it cannot pass by accident on a platform where the
// temp directory is itself a symlink.
func symlinkedDir(t *testing.T) (link, real string) {
	t.Helper()
	base := t.TempDir()
	real = filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link = filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", real, err)
	}
	return link, resolved
}

// TestAddStoresTheResolvedPath is the root gate. Everything else in this wi is
// downstream of what Add decides to write down.
//
// The assertion is on the STORED value rather than on any symptom, because the
// stored value is what three different packages go on to compare, encode and
// chdir into — and two of them are not reachable from this package's tests.
func TestAddStoresTheResolvedPath(t *testing.T) {
	link, real := symlinkedDir(t)
	r := &Registry{path: filepath.Join(t.TempDir(), "workspaces.json")}

	ws, err := r.Add("linked", link)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if ws.Path != real {
		t.Errorf("Add(%q).Path = %q, want the resolved %q\n"+
			"An un-resolved path here encodes to a transcript directory name cc never "+
			"wrote (zero sessions listed) and makes WalkDir lstat a symlink (empty file tree).",
			link, ws.Path, real)
	}
	// And it is the stored entry, not just the returned copy, that is canonical:
	// Path() is the accessor session.WorkspaceLookup calls.
	if p, ok := r.Path(ws.ID); !ok || p != real {
		t.Errorf("Path(%s) = (%q, %v), want (%q, true)", ws.ID, p, ok, real)
	}
}

// TestAddIsIdempotentThroughEitherName — the same directory named two different
// ways is one workspace. Before the fix these produced two entries with two ids
// and, for the symlink one, no sessions and no file tree.
func TestAddIsIdempotentThroughEitherName(t *testing.T) {
	link, real := symlinkedDir(t)
	r := &Registry{path: filepath.Join(t.TempDir(), "workspaces.json")}

	first, err := r.Add("by-link", link)
	if err != nil {
		t.Fatalf("Add(link): %v", err)
	}
	second, err := r.Add("by-real-path", real)
	if err != nil {
		t.Fatalf("Add(real): %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("Add(%q) then Add(%q) produced two ids (%s, %s), want one workspace",
			link, real, first.ID, second.ID)
	}
	if got := r.List(); len(got) != 1 {
		t.Errorf("List has %d entries, want 1: %+v", len(got), got)
	}
}

// planted writes a registry file whose single entry holds `path` verbatim — the
// shape a daemon from before this change left behind — and returns a Registry
// loaded from it through the real load path.
func planted(t *testing.T, path string) *Registry {
	t.Helper()
	file := filepath.Join(t.TempDir(), "workspaces.json")
	body, err := json.Marshal([]Workspace{{ID: "old1", Name: "planted", Path: path}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, body, 0o600); err != nil {
		t.Fatal(err)
	}
	r := &Registry{path: file}
	if err := r.load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	return r
}

// TestLoadCanonicalisesOldEntries — a workspace registered through a symlink
// BEFORE this change is carrying both silent failures right now. Canonicalising
// only on Add would leave it broken until the user happened to re-add it.
func TestLoadCanonicalisesOldEntries(t *testing.T) {
	link, real := symlinkedDir(t)
	r := planted(t, link)

	if p, ok := r.Path("old1"); !ok || p != real {
		t.Errorf("Path of an entry stored as %q = (%q, %v), want the resolved (%q, true)",
			link, p, ok, real)
	}
	if got := r.List(); len(got) != 1 || got[0].Path != real {
		t.Errorf("List = %+v, want one entry at %q", got, real)
	}
}

// TestAddDedupsAnOldEntry is the consequence the fix had to pay for: once Add
// canonicalises, it is comparing a canonical candidate against whatever is
// already stored. If load did not canonicalise too, re-adding a directory the
// registry already had — under EITHER name — would append a second entry for it.
// That is the specific regression this test exists to catch, and it is why the
// normalisation lives in load as well as in Add.
func TestAddDedupsAnOldEntry(t *testing.T) {
	link, real := symlinkedDir(t)

	for _, tc := range []struct {
		name  string
		added string
	}{
		{"re-added by its real path", real},
		{"re-added by the same symlink", link},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := planted(t, link)
			ws, err := r.Add("again", tc.added)
			if err != nil {
				t.Fatalf("Add: %v", err)
			}
			if ws.ID != "old1" {
				t.Errorf("Add(%q) = new id %s, want the planted entry old1", tc.added, ws.ID)
			}
			if got := r.List(); len(got) != 1 {
				t.Errorf("List has %d entries, want 1 (one directory, one workspace): %+v", len(got), got)
			}
		})
	}
}

// TestCanonicalPathKeepsWhatItCannotResolve pins the failure policy, which is
// cc's own (see canonicalPath's doc): a path that cannot be resolved keeps its
// absolute form instead of becoming an error. That, and nothing else — the name
// is the scope.
//
// # It used to assert two more things, and they now have their own names
//
// It also asserted that such a path was REGISTRABLE, and (after tether#147
// reversed that) that it was refused, and that a planted entry naming it still
// loaded. Both are gone from here, because a name that claims one scope while
// asserting three sends the next reader to the wrong place: someone who breaks
// Add and sees a test called "canonicalPath keeps what it cannot resolve" go red
// starts by suspecting canonicalPath, where nothing is wrong.
//
// Where they went, and what each one is now the gate for:
//
//	assertion                                   | the test that owns it now
//	--------------------------------------------+---------------------------------
//	Add refuses such a path                     | TestAdd_RefusesAPathThatIsNotAnExistingDirectory
//	                                            | (its "does not exist" and "is a
//	                                            | dangling symlink" rows are the
//	                                            | same two inputs — not duplicated
//	                                            | here)
//	a planted entry naming it still loads        | TestLoad_KeepsAnEntryWhoseDirectoryIsGone
//
// The division of labour those two record is the tether#147 reversal itself:
// canonicalPath still does not error (here), Add refuses anyway (there), and
// load() stays permissive (there).
func TestCanonicalPathKeepsWhatItCannotResolve(t *testing.T) {
	base := t.TempDir()
	missing := filepath.Join(base, "not-created-yet")

	got, err := canonicalPath(missing)
	if err != nil {
		t.Fatalf("canonicalPath(%q) = error %v, want the absolute path and no error", missing, err)
	}
	if got != missing {
		t.Errorf("canonicalPath(%q) = %q, want %q unchanged", missing, got, missing)
	}

	// A dangling symlink is the same policy through a different failure: the
	// target is gone, so there is nothing to resolve to.
	dangling := filepath.Join(base, "dangling")
	if err := os.Symlink(filepath.Join(base, "gone"), dangling); err != nil {
		t.Fatal(err)
	}
	if got, err := canonicalPath(dangling); err != nil || got != dangling {
		t.Errorf("canonicalPath(dangling) = (%q, %v), want (%q, nil)", got, err, dangling)
	}
}

// TestLoad_KeepsAnEntryWhoseDirectoryIsGone — the half of the pre-tether#147
// behaviour that was deliberately KEPT, and the more serious of the two failures
// this file guards, which is why it has its own name rather than a closing
// paragraph in the test above.
//
// A registration whose directory has since been deleted, or is on a volume that
// is not mounted right now, must still load. The cost of getting this wrong is
// not one bad entry: load() returning an error leaves session.Registry.Workspaces
// nil, and server/lifecycle.go turns that into "this daemon has no workspace
// registry" for EVERY request — the whole /api/v1/workspaces family drops out of
// the mux and answers 501. One absent directory would take the entire workspace
// pane and the `?ws=` chat handshake down with it.
//
// This is why tether#147 gated Registry.Add and pointedly did not gate load(),
// and why canonicalPath's fallback had to stay: the two are the same mechanism
// read at two different moments. Add asks "may this become a registration"; load
// asks "what does this registration name", and by then refusing is far too
// expensive an answer.
//
// # Read together with TestAdd_RefusesAPathThatIsNotAnExistingDirectory
//
// On its own this reads as "missing directories are fine", which is the belief
// tether#147 removed. The other half — that the SAME path is refused as a new
// registration — is asserted THERE and deliberately not duplicated here, for the
// reason this test exists at all: an assertion about Add, living under a name that
// says load, sends whoever breaks Add to the wrong function. That is the mistake
// this test was split out of, and repeating it one level down would be worse than
// the original, because a second copy is also a second thing to keep in step.
func TestLoad_KeepsAnEntryWhoseDirectoryIsGone(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "deleted-since-it-was-registered")

	// planted() writes a registry file holding the path verbatim and loads it
	// through the real load path, which is the whole point: Add would refuse this
	// path today, so the entry has to arrive the way a pre-existing one does.
	r := planted(t, missing)

	p, ok := r.Path("old1")
	if !ok {
		t.Fatalf("Path(old1) = not found; the entry was dropped because its directory is "+
			"absent, which makes a registry unloadable over one missing volume (%q)", missing)
	}
	if p != missing {
		t.Errorf("Path(old1) = %q, want %q unchanged", p, missing)
	}
	if got := r.List(); len(got) != 1 {
		t.Errorf("List = %+v, want the one planted entry", got)
	}
}

// TestListFilesRecursiveResolvesItsRoot is the second symptom, tested against the
// function DIRECTLY rather than through the API, because the API path is also
// covered by the registry now storing a resolved path — so a test that went
// through the handler would stay green with this clause reverted.
//
// The pre-fix value is asserted by name in the failure message because `["."]` is
// such a specific artefact: WalkDir lstats the root, decides a symlink is not a
// directory, falls into the file branch, and appends Rel(root, root).
func TestListFilesRecursiveResolvesItsRoot(t *testing.T) {
	link, real := symlinkedDir(t)
	mkfile(t, real, "README.md")
	mkfile(t, real, "a.go")
	mkfile(t, real, "src/b.go")
	mkfile(t, real, "node_modules/dep/z.js") // still skipped through the symlink

	files, truncated, err := listFilesRecursive(link, 100)
	if err != nil {
		t.Fatalf("listFilesRecursive(symlink) = error %v", err)
	}
	if truncated {
		t.Errorf("truncated = true, want false")
	}
	want := []string{"README.md", "a.go", "src/b.go"}
	if !reflect.DeepEqual(files, want) {
		t.Errorf("listFilesRecursive(%q) = %v, want %v\n"+
			"A result of [\".\"] means the root was lstat'd as a symlink and fell into "+
			"the file branch — the user sees an empty @-mention picker, with no error.",
			link, files, want)
	}

	// Positive control: the non-recursive sibling was never broken (os.ReadDir
	// follows the symlink it is handed), so if THIS started failing the fixture
	// itself would be at fault rather than the function under test.
	flat, err := listFiles(link)
	if err != nil {
		t.Fatalf("listFiles(symlink) = error %v", err)
	}
	if len(flat) != 4 {
		t.Errorf("listFiles(symlink) returned %d entries, want 4 — fixture problem, not a walk problem: %+v", len(flat), flat)
	}
}

// TestTreeHandlerServesASymlinkedWorkspace is the end-to-end shape of symptom
// two: register through a symlink, ask for the tree over HTTP, get the tree.
//
// As the header note says, this one is NOT a discriminating gate — either fix
// clause keeps it green alone. It is here because it is the thing a user would
// have reported, and because it shows the two clauses do not depend on each other.
func TestTreeHandlerServesASymlinkedWorkspace(t *testing.T) {
	link, real := symlinkedDir(t)
	mkfile(t, real, "main.go")
	mkfile(t, real, "internal/x/y.go")

	reg := &Registry{path: filepath.Join(t.TempDir(), "workspaces.json")}
	ws, err := reg.Add("linked", link)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	mux := http.NewServeMux()
	RegisterAPI(mux, reg)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/"+ws.ID+"/tree", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp treeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body: %s)", err, rec.Body.String())
	}
	want := []string{"internal/x/y.go", "main.go"}
	if !reflect.DeepEqual(resp.Files, want) {
		t.Errorf("tree files = %v, want %v", resp.Files, want)
	}
}
