package workspace

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// mkfile creates dir/rel (with parents) containing "x".
func mkfile(t *testing.T, root, rel string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestListFilesRecursive_SkipsHeavyDirsAndReturnsSortedRelPaths(t *testing.T) {
	root := t.TempDir()
	mkfile(t, root, "a.txt")
	mkfile(t, root, "sub/b.go")
	mkfile(t, root, "sub/deep/c.md")
	// These must all be skipped (never descended into):
	mkfile(t, root, "node_modules/pkg/index.js")
	mkfile(t, root, ".git/config")
	mkfile(t, root, "dist/bundle.js")
	mkfile(t, root, "sub/node_modules/nested/y.js") // skip even when nested

	files, truncated, err := listFilesRecursive(root, 100)
	if err != nil {
		t.Fatalf("listFilesRecursive: %v", err)
	}
	if truncated {
		t.Errorf("truncated=true, want false (under cap)")
	}
	want := []string{"a.txt", "sub/b.go", "sub/deep/c.md"}
	if !reflect.DeepEqual(files, want) {
		t.Errorf("files = %v, want %v", files, want)
	}
}

func TestListFilesRecursive_CapTruncates(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		mkfile(t, root, n+".txt")
	}
	files, truncated, err := listFilesRecursive(root, 2)
	if err != nil {
		t.Fatalf("listFilesRecursive: %v", err)
	}
	if len(files) != 2 {
		t.Errorf("len(files) = %d, want 2 (capped)", len(files))
	}
	if !truncated {
		t.Errorf("truncated=false, want true (over cap)")
	}
}

func TestListFilesRecursive_EmptyRootIsEmptyNotNil(t *testing.T) {
	files, truncated, err := listFilesRecursive(t.TempDir(), 100)
	if err != nil {
		t.Fatalf("listFilesRecursive: %v", err)
	}
	if truncated {
		t.Errorf("truncated=true, want false")
	}
	if files == nil || len(files) != 0 {
		t.Errorf("files = %v, want empty non-nil slice", files)
	}
}

func TestTreeHandler_ServesRecursiveList(t *testing.T) {
	root := t.TempDir()
	mkfile(t, root, "main.go")
	mkfile(t, root, "internal/x/y.go")
	mkfile(t, root, "node_modules/dep/z.js") // skipped

	reg, id := newTestRegistry(t, root)
	mux := http.NewServeMux()
	RegisterAPI(mux, reg)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/"+id+"/tree", nil)
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
		t.Errorf("files = %v, want %v", resp.Files, want)
	}
	if resp.Truncated {
		t.Errorf("truncated=true, want false")
	}
}

func TestTreeHandler_UnknownWorkspace404(t *testing.T) {
	reg, _ := newTestRegistry(t, t.TempDir())
	mux := http.NewServeMux()
	RegisterAPI(mux, reg)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/nope/tree", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
