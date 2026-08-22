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
//
// TestTreeHandlerServesASymlinkedWorkspace is deliberately NOT in that table: it
// is the user-visible end-to-end proof, and EITHER the registry clause or the
// listFilesRecursive clause is enough to keep it green on its own. It is kept
// because it demonstrates the two layers are independent, and it is named here as
// non-discriminating so nobody later mistakes it for the gate.

import (
	"encoding/json"
	"errors"
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
// absolute form instead of becoming an error.
//
// What rides on this, as of tether#147, is load() rather than Add. An entry
// ALREADY in workspaces.json whose directory is missing or on an unmounted volume
// keeps the value it had and stays in the list; making that an error would make
// the whole registry unloadable, which server/lifecycle.go turns into "this
// daemon has no workspace registry" for every request.
//
// # This test used to assert one more thing, and tether#147 reversed it
//
// It also asserted that such a path was REGISTRABLE — that Add accepted a
// directory which did not exist yet. That was true, and it was written down as a
// design intent, and tether#147 dropped it: tether#156 had already made Enable
// refuse a registration whose directory is not on disk, so the early registration
// bought nothing, while the registry stayed able to hold arbitrary strings and
// the failure surfaced at the wrong request. The assertion below is the reversed
// half, kept in place rather than deleted so that the two halves stay visibly
// separate: canonicalPath still does NOT error (the clause this test guards), and
// Add refuses anyway (the clause TestAdd_RefusesAPathThatIsNotAnExistingDirectory
// guards).
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

	// The reversal: canonicalPath hands the unresolved path back without
	// complaining, and Add is what declines to write it down.
	r := &Registry{path: filepath.Join(t.TempDir(), "workspaces.json")}
	if _, err := r.Add("later", missing); !errors.Is(err, ErrWorkspacePathUnusable) {
		t.Errorf("Add(a directory that does not exist yet) = %v, want ErrWorkspacePathUnusable", err)
	}

	// And a registry entry that ALREADY names it survives being loaded, which is
	// the half of the old behaviour that was kept. planted() goes through load().
	if p, ok := planted(t, missing).Path("old1"); !ok || p != missing {
		t.Errorf("Path of a planted entry at a missing directory = (%q, %v), want (%q, true) — "+
			"a bookmark to a temporarily absent directory must not make the registry unloadable",
			p, ok, missing)
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
