package adapter

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCC_Materialise_CreatesSymlink(t *testing.T) {
	pool := t.TempDir()
	ws := t.TempDir()
	src := filepath.Join(pool, "dag")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (CC{}).Materialise(ws, "dag", src); err != nil {
		t.Fatalf("Materialise: %v", err)
	}
	link := filepath.Join(ws, ".claude/plugins/dag")
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("expected symlink at %s, got mode %v", link, fi.Mode())
	}
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	want, _ := filepath.Abs(src)
	if target != want {
		t.Errorf("symlink target: got %q want %q", target, want)
	}
}

func TestCC_Materialise_Idempotent(t *testing.T) {
	pool := t.TempDir()
	ws := t.TempDir()
	src := filepath.Join(pool, "dag")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	cc := CC{}
	for i := 0; i < 3; i++ {
		if err := cc.Materialise(ws, "dag", src); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
}

func TestCC_Materialise_RefusesToClobberRealDir(t *testing.T) {
	pool := t.TempDir()
	ws := t.TempDir()
	src := filepath.Join(pool, "dag")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(ws, ".claude/plugins/dag")
	if err := os.MkdirAll(link, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (CC{}).Materialise(ws, "dag", src); err == nil {
		t.Errorf("expected error when destination is a real directory")
	}
}

func TestCC_Materialise_RewritesStaleSymlink(t *testing.T) {
	pool := t.TempDir()
	ws := t.TempDir()
	src := filepath.Join(pool, "dag")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(pool, "old")
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".claude/plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(ws, ".claude/plugins/dag")
	absOld, _ := filepath.Abs(old)
	if err := os.Symlink(absOld, link); err != nil {
		t.Fatal(err)
	}
	if err := (CC{}).Materialise(ws, "dag", src); err != nil {
		t.Fatalf("Materialise: %v", err)
	}
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	wantAbs, _ := filepath.Abs(src)
	if target != wantAbs {
		t.Errorf("expected stale link rewritten to %q, got %q", wantAbs, target)
	}
}

func TestCC_Unmaterialise(t *testing.T) {
	pool := t.TempDir()
	ws := t.TempDir()
	src := filepath.Join(pool, "dag")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	cc := CC{}
	if err := cc.Materialise(ws, "dag", src); err != nil {
		t.Fatal(err)
	}
	if err := cc.Unmaterialise(ws, "dag"); err != nil {
		t.Fatalf("Unmaterialise: %v", err)
	}
	link := filepath.Join(ws, ".claude/plugins/dag")
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("expected link removed; lstat err=%v", err)
	}
	// Unmaterialising again is a no-op.
	if err := cc.Unmaterialise(ws, "dag"); err != nil {
		t.Errorf("idempotent unmaterialise: %v", err)
	}
}
