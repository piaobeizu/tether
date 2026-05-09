package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSpec_E1_NoJsonlWrites is a static guard for spec §6.E.1: tether
// must never write to cc's session jsonl files. The README claimed this
// was protected by "lint check or human review" — until this test was
// added, neither existed in code.
//
// Approach: scan every non-test .go file under the package and fail if
// any of them combines a write API with a jsonl-or-projects-path
// reference. False positives are possible but are easy to silence by
// explicit annotation if a legitimate write to a non-cc-jsonl path
// surfaces in the future.
func TestSpec_E1_NoJsonlWrites(t *testing.T) {
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}

	// File-write APIs we consider potentially concerning.
	writeAPIs := []string{
		"os.WriteFile(",
		"os.Create(",
		"os.OpenFile(",
		"ioutil.WriteFile(",
	}

	// Path patterns that indicate the write target is cc's session
	// storage. Tether can write its OWN files freely — only cc's
	// projects/<dir>/*.jsonl is off-limits.
	forbiddenTargets := []string{
		".jsonl",
		".claude/projects",
	}

	for _, path := range matches {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := string(raw)

		for _, api := range writeAPIs {
			if !strings.Contains(text, api) {
				continue
			}
			for _, target := range forbiddenTargets {
				if strings.Contains(text, target) {
					t.Errorf("E.1 likely violation in %s: source contains %q AND %q together — tether must not write cc's session jsonls. Annotate the source file to silence this if intentional.",
						path, api, target)
				}
			}
		}
	}
}

// TestSpec_A1_NoFd3Dependency is a static guard for spec §6.A.1: tether
// must not depend on cc's undocumented fd 3 diagnostic channel.
// happy-cli's pattern (`stdio: ['inherit', 'inherit', 'inherit', 'pipe']`)
// is explicitly rejected; tether always uses three pipes.
func TestSpec_A1_NoFd3Dependency(t *testing.T) {
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}

	for _, path := range matches {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := string(raw)

		// Any code path that sets ExtraFiles to a non-nil slice would
		// indicate fd 3+ wiring. Spawn explicitly nil's it out.
		if strings.Contains(text, "ExtraFiles") &&
			!strings.Contains(text, "ExtraFiles = nil") &&
			!strings.Contains(text, "ExtraFiles == nil") {
			t.Errorf("A.1 likely violation in %s: ExtraFiles assignment to non-nil — fd 3 wiring is forbidden",
				path)
		}
	}
}
