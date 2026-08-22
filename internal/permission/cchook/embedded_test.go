package cchook

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// Every test here needs a real compiled hook, and compiling one costs a `go
// build` (~1s). The tests that only RUN the hook share a single build; the ones
// that mutate the directory around it each get their own.
//
// Temp dirs, not /dev/shm: this container mounts /dev/shm noexec, so a 0755
// fixture there still fails to exec (tether#123 burned an afternoon on that).
// os.MkdirTemp/t.TempDir honour TMPDIR and fall back to /tmp, which is on the
// root overlay here — executable.
var (
	sharedHookPath string
	sharedHookErr  error
)

func TestMain(m *testing.M) {
	code := func() int {
		if _, err := exec.LookPath("go"); err != nil {
			sharedHookErr = fmt.Errorf("no go toolchain on PATH: %w", err)
			return m.Run()
		}
		dir, err := os.MkdirTemp("", "cchook-shared-*")
		if err != nil {
			sharedHookErr = err
			return m.Run()
		}
		defer os.RemoveAll(dir)
		sharedHookPath = filepath.Join(dir, "tether-permission-hook")
		if err := EnsureHookBinary(sharedHookPath); err != nil {
			sharedHookErr = fmt.Errorf("compiling the shared hook: %w", err)
		}
		return m.Run()
	}()
	os.Exit(code)
}

// sharedHook returns the one compiled hook shared by every read-only test.
// Callers must not modify it or its directory.
func sharedHook(t *testing.T) string {
	t.Helper()
	if sharedHookErr != nil {
		t.Skipf("shared hook unavailable: %v", sharedHookErr)
	}
	return sharedHookPath
}

// freshHook compiles the hook into a directory of this test's own, for the tests
// that then break it on purpose.
func freshHook(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("no go toolchain on PATH: %v", err)
	}
	binPath := filepath.Join(t.TempDir(), "tether-permission-hook")
	if err := EnsureHookBinary(binPath); err != nil {
		t.Fatalf("EnsureHookBinary: %v", err)
	}
	return binPath
}

// TestEnsureHookBinary_HashWithoutBinary_Rebuilds pins tether#117 A4a.
//
// EnsureHookBinary read only <binPath>.hash. Delete the binary and leave the
// hash behind — a `rm`, a wiped tmpfs, a half-finished upgrade — and it returned
// nil claiming "up to date". Startup then succeeded, InjectPermHook wrote the
// missing path into ~/.claude/settings.json as the PreToolUse command, and every
// tool call exited 127. cc treats every non-zero code EXCEPT 2 as non-blocking
// ("Failed with non-blocking status code: ... Treating as non-blocking.", read
// out of the installed cc binary), so every tool ran with NO permission prompt
// at all. The only trace was cc's own stderr.
func TestEnsureHookBinary_HashWithoutBinary_Rebuilds(t *testing.T) {
	binPath := freshHook(t)
	hashPath := binPath + ".hash"

	if err := os.Remove(binPath); err != nil {
		t.Fatalf("remove binary: %v", err)
	}
	// The fixture is only meaningful while the hash survives — that is the whole
	// state being tested.
	if _, err := os.Stat(hashPath); err != nil {
		t.Fatalf("fixture is wrong: .hash should still exist, got %v", err)
	}

	if err := EnsureHookBinary(binPath); err != nil {
		t.Fatalf("EnsureHookBinary on a hash-only dir: %v", err)
	}
	st, err := os.Stat(binPath)
	if err != nil {
		t.Fatalf("hook binary was NOT rebuilt: %v — a stale .hash still buys a fail-open gate", err)
	}
	if !st.Mode().IsRegular() {
		t.Errorf("rebuilt hook is not a regular file: mode=%v", st.Mode())
	}
	// Exec bits are a POSIX notion; see runnable() for why windows is excluded.
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o111 == 0 {
		t.Errorf("rebuilt hook is not executable: mode=%v", st.Mode().Perm())
	}
}

// TestEnsureHookBinary_DirectoryAtBinPath_IsNotAcceptedAsUpToDate covers the
// same "it exists, therefore it runs" fallacy one level up: os.Stat succeeds on
// a directory, so mere existence is not the property that matters.
func TestEnsureHookBinary_DirectoryAtBinPath_IsNotAcceptedAsUpToDate(t *testing.T) {
	binPath := freshHook(t)
	hashPath := binPath + ".hash"
	hash, err := os.ReadFile(hashPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(binPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(binPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hashPath, hash, 0o600); err != nil {
		t.Fatal(err)
	}

	// A directory can never be exec'd, so "up to date" must not be the answer.
	// Either the rebuild fails loudly (go build cannot write over a directory)
	// or it replaces it — silently returning nil is the one wrong outcome.
	if err := EnsureHookBinary(binPath); err == nil {
		st, statErr := os.Stat(binPath)
		if statErr != nil {
			t.Fatalf("returned nil but nothing is at binPath: %v", statErr)
		}
		if st.IsDir() {
			t.Fatal("EnsureHookBinary reported success with a DIRECTORY at binPath")
		}
	}
}

// TestEnsureHookBinary_NonExecutableBinary_Rebuilds closes the mode half. A hook
// present but not executable exits 126, which cc also treats as non-blocking —
// the same fail-open as 127 reached from a different direction (a manual `cp`, a
// restrictive umask, a restore from an archive that dropped the mode bits).
func TestEnsureHookBinary_NonExecutableBinary_Rebuilds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.Stat on windows reports 0444/0666 for every regular file, so there is no exec bit to drop")
	}
	binPath := freshHook(t)
	if err := os.Chmod(binPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureHookBinary(binPath); err != nil {
		t.Fatalf("EnsureHookBinary: %v", err)
	}
	st, err := os.Stat(binPath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o111 == 0 {
		t.Errorf("hook left non-executable: mode=%v — cc would exit 126 and treat it as non-blocking", st.Mode().Perm())
	}
}

// TestEnsureHookBinary_UpToDate_DoesNotRebuild keeps the fast path honest: with
// both the binary and a matching hash in place, nothing is rebuilt. Without this
// the A4a fix could have been "always rebuild", which would add a `go build` to
// every daemon start.
func TestEnsureHookBinary_UpToDate_DoesNotRebuild(t *testing.T) {
	binPath := freshHook(t)
	before, err := os.Stat(binPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureHookBinary(binPath); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(binPath)
	if err != nil {
		t.Fatal(err)
	}
	// os.SameFile compares inode identity, so this does not depend on mtime
	// granularity: a rebuild writes a new file.
	if !os.SameFile(before, after) {
		t.Error("EnsureHookBinary rebuilt an up-to-date hook; startup would pay for a go build every time")
	}
}
