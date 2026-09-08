// Package buildgate holds executable gates over the build system itself — the
// Makefile and the scripts its recipes drive — rather than over any one Go
// package.
//
// It deliberately has no non-test source file. Nothing imports it, `go build
// ./...` and `go vet ./...` ignore it (both verified to exit 0 with a test-only
// package present), and `go test ./...` picks it up — which is what CI runs
// (.github/workflows/ci.yml, "Run Go tests": `go test -count=1 -race ./...`).
// That is the whole reason the gate lives here rather than in a
// scripts/check-*.sh hung off a make target: CI never invokes make, so a gate
// that only exists in make is a gate CI never runs. The mirror of the argument
// in Makefile's check-vendor comment.
//
// This does not delegate a CI gate to make, which ci.yml's "Run Go tests"
// comment warns against. Here make is the object under test, not the means of
// testing something else.
//
// What this package needs beyond the Go toolchain: it shells out, so `go test
// ./...` is no longer self-contained once it reaches here. The isolated copy
// under test is built from `git ls-files`, which needs a real git working copy
// rather than an export of one, and the probe runs the Makefile, which needs
// make on PATH. A source tarball, or a container stage that COPYs the tree
// without .git, therefore fails this package with `fatal: not a git repository`
// — a missing prerequisite of the harness, not a defect in the code under test.
// That failure is deliberately fatal rather than a skip; see copyTrackedTree.
package buildgate

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMakeTargetsStayHermeticAgainstAnEnclosingGoWork is the gate on
// `export GOWORK ?= off` in the Makefile (tether#177, tether#181).
//
// Why it needs a gate at all: nothing else in the tree executes the Makefile.
// CI runs scripts/*.sh and `go` directly and never invokes make, and the CI
// runner's checkout has no go.work anywhere in its ancestry — so every CI check
// is green whether or not that line exists. The green is honest but has no
// discriminating power over this line, which means the line could be deleted
// and nothing anywhere would notice.
//
// The gate reconstructs the hazardous condition instead of waiting to meet it:
// a copy of the working tree placed inside a workspace that does not `use` this
// module, which is exactly the shape of a polyforge task worktree (and of the
// canonical .repo/tether clone) under gmi-ws/go.work.
//
// Verified in both directions at the time of writing, on go1.26.3 / GNU Make
// 4.3, with GOWORK absent from the environment:
//
//	line present  ->  make codegen exits 0
//	line deleted  ->  go: no such tool "tygo", and make exits 2 at the codegen
//	                  recipe
//
// No line number is quoted on purpose: it moves whenever the comment above the
// declaration is edited, and nothing would redden when it went stale.
//
// `make codegen` is the probe rather than `make build` because it needs neither
// node nor web/dist, so the gate costs about a second and no pnpm install.
func TestMakeTargetsStayHermeticAgainstAnEnclosingGoWork(t *testing.T) {
	root := repoRoot(t)
	goVersion := goDirective(t, root)

	// t.TempDir gives a per-test unique directory and removes it afterwards.
	// It must not be a fixed path: this repo is checked out several times over
	// in one polyforge workspace and agents run concurrently, so a fixed path
	// would be a shared resource between them.
	base := t.TempDir()
	mod := filepath.Join(base, "mod")
	decoy := filepath.Join(base, "decoy")

	// The copy carries tracked files at WORKING-TREE content, not at HEAD.
	// Reading HEAD would make the gate read state rather than the change: a
	// developer who deletes the line and runs `go test ./...` before committing
	// would get a pass, which is the failure mode this gate exists to prevent.
	copyTrackedTree(t, root, mod)

	// An enclosing workspace that does not `use` the copy. The decoy is fidelity
	// rather than necessity — measured, a go.work with no `use` at all is accepted
	// by go and puts the copy in the same hazard, and removing the decoy's go.mod
	// does not change this gate's verdict in either direction. It is here because
	// it stands in for "somebody else's modules", the role gmi-ws/go.work's other
	// repos play in reality, and a synthetic workspace shaped like the real one is
	// the thing worth reproducing.
	mustWriteFile(t, filepath.Join(decoy, "go.mod"),
		fmt.Sprintf("module decoy\n\ngo %s\n", goVersion))
	mustWriteFile(t, filepath.Join(base, "go.work"),
		fmt.Sprintf("go %s\n\nuse ./decoy\n", goVersion))

	// Confirm the harness actually reproduces the hazard before trusting what
	// it reports. If `go` did not resolve to the enclosing go.work, the probe
	// below would pass with or without the Makefile line — the gate would be
	// green in both directions and blind, while looking like it had verified
	// something.
	gotWork := runGo(t, mod, "env", "GOWORK")
	wantWork := filepath.Join(base, "go.work")
	if gotWork != wantWork {
		t.Fatalf("harness did not reproduce the hazard: go resolved GOWORK to %q, want the enclosing %q.\n"+
			"Without an enclosing workspace in effect this test cannot discriminate and must not report a pass.",
			gotWork, wantWork)
	}

	out, err := runMake(t, mod, "codegen")
	if err != nil {
		t.Fatalf("`make codegen` failed inside an enclosing go.work: %v\n"+
			"Read the output below before editing anything — this line is reachable by more than\n"+
			"one cause, and they need opposite fixes.\n"+
			"  * The Makefile's `export GOWORK ?= off` is gone: restore it. `export` is\n"+
			"    load-bearing (a plain make variable is not in the recipe's environment) and the\n"+
			"    value must survive into the recipe. The tell is `go` reaching outside the module\n"+
			"    — e.g. `go: no such tool \"tygo\"`.\n"+
			"  * The recipe cannot find one of its own input files: that input exists in your\n"+
			"    working tree but is not tracked by git, so it is absent from the copy under test,\n"+
			"    which this gate builds from `git ls-files`. `git add` the file the output names.\n"+
			"    The Makefile is not the problem, and restoring a line that is already there will\n"+
			"    not fix it.\n--- output ---\n%s", err, out)
	}
}

// TestGoworkDefaultIsExportedAndYieldsToAnOuterValue pins the two properties of
// the declaration that the end-to-end test above cannot tell apart from a
// hardcoded value: that `off` reaches the recipe *environment* (`export`), and
// that an outer value still wins (`?=`).
//
// The second half is the documented escape hatch —
// `GOWORK="$PWD/go.work" make build` for a developer who deliberately wants
// their own workspace honoured. Turning `?=` into `=` would silently remove it
// while leaving every other check green, so it gets its own assertion.
//
// `make --eval` defines a throwaway probe target without adding one to the
// Makefile's public target list. Requires GNU make (>= 3.82); the Makefile is
// already GNU-only, since it uses `$(shell ...)`.
func TestGoworkDefaultIsExportedAndYieldsToAnOuterValue(t *testing.T) {
	root := repoRoot(t)
	const probe = `pfprobe: ; @echo "$$GOWORK"`

	t.Run("default reaches the recipe environment", func(t *testing.T) {
		got, err := runMake(t, root, "--eval="+probe, "pfprobe")
		if err != nil {
			t.Fatalf("probe failed: %v\n%s", err, got)
		}
		if strings.TrimSpace(got) != "off" {
			t.Fatalf("recipe environment has GOWORK=%q, want \"off\".\n"+
				"The Makefile must carry `export GOWORK ?= off`; without `export` the value never\n"+
				"reaches the recipe environment, which is where `go` reads it.", strings.TrimSpace(got))
		}
	})

	t.Run("an outer value wins", func(t *testing.T) {
		// Absolute on purpose: go rejects a relative GOWORK with
		// "invalid GOWORK: not an absolute path". Never resolved, so any
		// absolute path serves.
		const outer = "/nonexistent/outer/go.work"
		got, err := runMakeWithEnv(t, root, []string{"GOWORK=" + outer}, "--eval="+probe, "pfprobe")
		if err != nil {
			t.Fatalf("probe failed: %v\n%s", err, got)
		}
		if strings.TrimSpace(got) != outer {
			t.Fatalf("recipe environment has GOWORK=%q, want the outer %q.\n"+
				"`?=` must yield to an outer value — that is the documented escape hatch\n"+
				"`GOWORK=\"$PWD/go.work\" make build`. A plain `=` would override it.",
				strings.TrimSpace(got), outer)
		}
	})
}

// TestGoworkProbeSurvivesAmbientMakeState is the CI-side arm of the fix for
// tether#199: probeEnv must strip the sub-make state GNU make exports to
// recipe children, or the probes above read banner-wrapped output whenever the
// test binary itself was started from a make recipe — which `make test` and
// `make go-test` always are, and which left both red on a clean checkout while
// CI, which never invokes make, stayed green.
//
// CI cannot meet that condition by itself, so this test reconstructs it the
// way TestMakeTargetsStayHermeticAgainstAnEnclosingGoWork reconstructs the
// enclosing-workspace hazard: t.Setenv injects the two variables measured (GNU
// Make 4.3) to each re-enable the "Entering/Leaving directory" banners on
// their own — MAKELEVEL, which marks the probe make as a sub-make, and
// MAKEFLAGS=w, which asks for the banners outright. If probeEnv ever stops
// stripping them, this reddens in CI rather than only on the next developer's
// `make test`.
func TestGoworkProbeSurvivesAmbientMakeState(t *testing.T) {
	root := repoRoot(t)
	const probe = `pfprobe: ; @echo "$$GOWORK"`
	// t.Setenv mutates process-global state; it also asserts this test is not
	// parallel, which is what makes that safe here.
	t.Setenv("MAKELEVEL", "1")
	t.Setenv("MAKEFLAGS", "w")
	got, err := runMake(t, root, "--eval="+probe, "pfprobe")
	if err != nil {
		t.Fatalf("probe failed: %v\n%s", err, got)
	}
	if strings.TrimSpace(got) != "off" {
		t.Fatalf("recipe environment has GOWORK=%q, want \"off\".\n"+
			"The probe inherited ambient make state: probeEnv must keep stripping MAKELEVEL\n"+
			"and MAKEFLAGS (and their siblings), or every probe in this package reads\n"+
			"banner-wrapped output when the test binary runs from a make recipe.", strings.TrimSpace(got))
	}
}

// repoRoot walks up from the working directory to the module root, identified by
// holding both go.mod and the Makefile under test.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		_, goErr := os.Stat(filepath.Join(dir, "go.mod"))
		_, mkErr := os.Stat(filepath.Join(dir, "Makefile"))
		if goErr == nil && mkErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no directory holding both go.mod and Makefile above %s", dir)
		}
		dir = parent
	}
}

// goDirective returns the version on go.mod's `go` line, so the generated
// workspace cannot fall out of step with the module it encloses.
func goDirective(t *testing.T, root string) string {
	t.Helper()
	f, err := os.Open(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("open go.mod: %v", err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(scanner.Text()), "go "); ok {
			return strings.TrimSpace(rest)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	t.Fatal("go.mod has no `go` directive")
	return ""
}

// copyTrackedTree copies every git-tracked path into dst at working-tree
// content. Tracked-file list rather than a hand-written include list because an
// include list rots the moment codegen grows an input; whole-tree rather than
// `git archive` because archive reads HEAD (see the caller).
//
// A failure here is fatal rather than a skip. A gate that quietly skips itself
// reports the same green as one that ran and passed.
func copyTrackedTree(t *testing.T, root, dst string) {
	t.Helper()
	cmd := exec.Command("git", "-C", root, "ls-files", "-z")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files in %s: %v\n%s\n"+
			"This gate needs the tracked-file list to build an isolated copy of the working tree.", root, err, stderr.String())
	}
	names := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	copied := 0
	for _, name := range names {
		if name == "" {
			continue
		}
		src := filepath.Join(root, name)
		info, err := os.Lstat(src)
		if err != nil {
			// Tracked but absent from the working tree (e.g. deleted and not
			// yet committed). Nothing to copy; the copy mirrors the tree.
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read %s: %v", src, err)
		}
		target := filepath.Join(dst, name)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", target, err)
		}
		if err := os.WriteFile(target, data, info.Mode().Perm()); err != nil {
			t.Fatalf("write %s: %v", target, err)
		}
		copied++
	}
	if copied == 0 {
		t.Fatalf("copied no files from %s — the isolated copy would be empty and the probe meaningless", root)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// probeEnv returns the ambient environment scrubbed of the two variable
// families that let the process tree this test binary happens to run under
// leak into a probe's result.
//
// GOWORK: stripping it is what makes the measurement mean anything. An
// inherited GOWORK=off would make the probe pass with the Makefile line
// deleted, and an inherited path would poison the enclosing-workspace setup —
// either way both directions go green and the gate stops discriminating while
// still reporting a pass.
//
// MAKELEVEL, MAKEFLAGS, MFLAGS, MAKE_TERMOUT, MAKE_TERMERR: the state GNU make
// hands to sub-makes. `make test` and `make go-test` are recipe lines, so this
// test binary usually runs with MAKELEVEL set — and a make that sees MAKELEVEL
// believes it is a sub-make and prints the "Entering/Leaving directory"
// banners into the probe's output, which is exactly what
// TestGoworkDefaultIsExportedAndYieldsToAnOuterValue compares. Measured on GNU
// Make 4.3: MAKELEVEL=1 with no make anywhere in the chain reproduces the
// failure, and MAKEFLAGS=w alone does too, so each strip is load-bearing on
// its own; MFLAGS repeats MAKEFLAGS, and MAKE_TERMOUT/MAKE_TERMERR would make
// the probe's environment depend on whether a human was watching. The same
// five names, for the same reason, are what the Makefile's own `make ci`
// driver strips from every step it runs. Stripping them means every probe runs
// as a top-level make, however this test binary itself was started — the tree
// under test is isolated by copyTrackedTree, and this is the same isolation
// for the environment.
//
// Extra values may be appended to override a strip deliberately.
func probeEnv(extra ...string) []string {
	env := make([]string, 0, len(os.Environ())+len(extra))
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "GOWORK", "MAKELEVEL", "MAKEFLAGS", "MFLAGS", "MAKE_TERMOUT", "MAKE_TERMERR":
			continue
		}
		env = append(env, kv)
	}
	return append(env, extra...)
}

func runMake(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	return runMakeWithEnv(t, dir, nil, args...)
}

func runMakeWithEnv(t *testing.T, dir string, extraEnv []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("make", args...)
	cmd.Dir = dir
	cmd.Env = probeEnv(extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func runGo(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = probeEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go %s in %s: %v\n%s", strings.Join(args, " "), dir, err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}
