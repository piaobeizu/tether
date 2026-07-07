package workspace

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// runGit runs a git command in dir, failing the test on error.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func newTestRegistry(t *testing.T, root string) (*Registry, string) {
	t.Helper()
	reg := &Registry{path: filepath.Join(t.TempDir(), "workspaces.json")}
	ws, err := reg.Add("test", root)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	return reg, ws.ID
}

func TestFilesHandler_WorkspaceRootIsGitRoot(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init")
	if err := os.WriteFile(filepath.Join(root, "clean.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dirty.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-m", "initial")

	// Modify a tracked file.
	if err := os.WriteFile(filepath.Join(root, "dirty.txt"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Add an untracked file.
	if err := os.WriteFile(filepath.Join(root, "untracked.txt"), []byte("c"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg, id := newTestRegistry(t, root)
	mux := http.NewServeMux()
	RegisterAPI(mux, reg)

	entries := getFiles(t, mux, id, "")

	byName := map[string]FileEntry{}
	for _, e := range entries {
		byName[e.Name] = e
	}

	if e, ok := byName["clean.txt"]; !ok || e.Dirty {
		t.Errorf("clean.txt should not be dirty, got %+v (ok=%v)", e, ok)
	}
	if e, ok := byName["dirty.txt"]; !ok || !e.Dirty {
		t.Errorf("dirty.txt should be dirty, got %+v (ok=%v)", e, ok)
	}
	if e, ok := byName["untracked.txt"]; !ok || !e.Dirty {
		t.Errorf("untracked.txt should be dirty, got %+v (ok=%v)", e, ok)
	}
}

func TestFilesHandler_DirtyPropagatesToAncestorDir(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-m", "initial")

	if err := os.WriteFile(filepath.Join(root, "sub", "a.txt"), []byte("modified"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg, id := newTestRegistry(t, root)
	mux := http.NewServeMux()
	RegisterAPI(mux, reg)

	entries := getFiles(t, mux, id, "")
	var subEntry *FileEntry
	for i := range entries {
		if entries[i].Name == "sub" {
			subEntry = &entries[i]
		}
	}
	if subEntry == nil {
		t.Fatalf("expected 'sub' directory entry, got %+v", entries)
	}
	if !subEntry.IsDir || !subEntry.Dirty {
		t.Errorf("sub dir should be IsDir=true Dirty=true, got %+v", *subEntry)
	}

	// And listing inside sub/ should show a.txt dirty directly.
	subEntries := getFiles(t, mux, id, "sub")
	if len(subEntries) != 1 || subEntries[0].Name != "a.txt" || !subEntries[0].Dirty {
		t.Errorf("expected sub/a.txt dirty, got %+v", subEntries)
	}
}

func TestFilesHandler_SubdirIsGitRootButWorkspaceRootIsNot(t *testing.T) {
	root := t.TempDir()
	// root itself is NOT a git repo.
	repoDir := filepath.Join(root, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repoDir, "init")
	if err := os.WriteFile(filepath.Join(repoDir, "tracked.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-m", "initial")
	if err := os.WriteFile(filepath.Join(repoDir, "tracked.txt"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "new.txt"), []byte("c"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg, id := newTestRegistry(t, root)
	mux := http.NewServeMux()
	RegisterAPI(mux, reg)

	// Listing workspace root (not a git repo) — no error, but also
	// nothing under it should be reported dirty since root isn't in a repo
	// by itself; the 'repo' subdir listing below is the key check.
	rootEntries := getFiles(t, mux, id, "")
	if len(rootEntries) != 1 || rootEntries[0].Name != "repo" {
		t.Fatalf("expected single 'repo' dir at workspace root, got %+v", rootEntries)
	}

	// Listing the subdir that IS a git repo root must find dirty files
	// via per-directory git-root discovery (root != git root).
	subEntries := getFiles(t, mux, id, "repo")
	byName := map[string]FileEntry{}
	for _, e := range subEntries {
		byName[e.Name] = e
	}
	if e, ok := byName["tracked.txt"]; !ok || !e.Dirty {
		t.Errorf("tracked.txt should be dirty, got %+v (ok=%v)", e, ok)
	}
	if e, ok := byName["new.txt"]; !ok || !e.Dirty {
		t.Errorf("new.txt should be dirty (untracked), got %+v (ok=%v)", e, ok)
	}
}

func TestFilesHandler_NonGitDirAllClean(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	reg, id := newTestRegistry(t, root)
	mux := http.NewServeMux()
	RegisterAPI(mux, reg)

	entries := getFiles(t, mux, id, "")
	for _, e := range entries {
		if e.Dirty {
			t.Errorf("entry %+v should not be dirty in non-git dir", e)
		}
	}
}

func TestFilesHandler_DirEscape400(t *testing.T) {
	root := t.TempDir()
	reg, id := newTestRegistry(t, root)
	mux := http.NewServeMux()
	RegisterAPI(mux, reg)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/"+id+"/files?dir=..", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for dir=.. escape, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestFilesHandler_UnknownWorkspace404(t *testing.T) {
	reg := &Registry{path: filepath.Join(t.TempDir(), "workspaces.json")}
	mux := http.NewServeMux()
	RegisterAPI(mux, reg)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/does-not-exist/files", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown workspace, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestFilesHandler_SortOrderDirsFirstThenAlpha(t *testing.T) {
	root := t.TempDir()
	names := []string{"zeta.txt", "alpha.txt"}
	dirs := []string{"zdir", "adir"}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(root, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	reg, id := newTestRegistry(t, root)
	mux := http.NewServeMux()
	RegisterAPI(mux, reg)

	entries := getFiles(t, mux, id, "")
	gotNames := make([]string, len(entries))
	for i, e := range entries {
		gotNames[i] = e.Name
	}
	want := []string{"adir", "zdir", "alpha.txt", "zeta.txt"}
	if len(gotNames) != len(want) {
		t.Fatalf("got %v, want %v", gotNames, want)
	}
	for i := range want {
		if gotNames[i] != want[i] {
			t.Errorf("sort order mismatch at %d: got %v want %v", i, gotNames, want)
		}
	}
}

// TestFilesHandler_NonASCIIFilenameDirty guards the porcelain-quoting fix:
// git quotes non-ASCII paths (core.quotePath) unless -z is used, which would
// make a naive parser miss the dirty state entirely.
func TestFilesHandler_NonASCIIFilenameDirty(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init")
	if err := os.WriteFile(filepath.Join(root, "seed.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-m", "initial")

	// Untracked file with a non-ASCII name (would be C-escaped in default porcelain).
	uni := "café-日本語.txt"
	if err := os.WriteFile(filepath.Join(root, uni), []byte("c"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg, id := newTestRegistry(t, root)
	mux := http.NewServeMux()
	RegisterAPI(mux, reg)

	entries := getFiles(t, mux, id, "")
	var found *FileEntry
	for i := range entries {
		if entries[i].Name == uni {
			found = &entries[i]
		}
	}
	if found == nil {
		t.Fatalf("expected entry %q in listing, got %+v", uni, entries)
	}
	if !found.Dirty {
		t.Errorf("non-ASCII untracked file %q should be dirty, got %+v", uni, *found)
	}
}

// TestFilesHandler_StagedRenameMarksNewPath verifies a rename record marks the
// destination dirty and the trailing source field is consumed (not mistaken
// for a separate record).
func TestFilesHandler_StagedRenameMarksNewPath(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init")
	if err := os.WriteFile(filepath.Join(root, "old.txt"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-m", "initial")
	runGit(t, root, "mv", "old.txt", "new.txt") // staged rename

	reg, id := newTestRegistry(t, root)
	mux := http.NewServeMux()
	RegisterAPI(mux, reg)

	entries := getFiles(t, mux, id, "")
	byName := map[string]FileEntry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	if e, ok := byName["new.txt"]; !ok || !e.Dirty {
		t.Errorf("rename dest new.txt should be dirty, got %+v (ok=%v)", e, ok)
	}
	if _, ok := byName["old.txt"]; ok {
		t.Errorf("old.txt should no longer exist on disk after rename, got listing %+v", entries)
	}
}

// getFiles is a test helper that GETs the files endpoint and decodes the body.
func getFiles(t *testing.T, mux *http.ServeMux, id, dir string) []FileEntry {
	t.Helper()
	url := "/api/v1/workspaces/" + id + "/files"
	if dir != "" {
		url += "?dir=" + dir
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: expected 200, got %d: %s", url, rec.Code, rec.Body.String())
	}
	var entries []FileEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &entries); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return entries
}
