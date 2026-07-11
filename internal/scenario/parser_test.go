package scenario

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveScenarioFile covers the happy path (workspaceRoot/.repo/<repo>/
// <wiType>.<project>.md exists) and the path-traversal guard on wiType.
func TestResolveScenarioFile(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, ".repo", "polyforge-coding")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mdPath := filepath.Join(repoDir, "feature.tether.md")
	if err := os.WriteFile(mdPath, []byte("## Step: a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, ok := ResolveScenarioFile(root, "feature", "tether")
	if !ok {
		t.Fatalf("ResolveScenarioFile() ok = false, want true")
	}
	wantAbs, err := filepath.Abs(mdPath)
	if err != nil {
		t.Fatal(err)
	}
	if got != wantAbs {
		t.Errorf("ResolveScenarioFile() = %q, want %q", got, wantAbs)
	}

	// Path traversal guard: wiType containing "../" must be rejected.
	if _, ok := ResolveScenarioFile(root, "../etc", "tether"); ok {
		t.Errorf("ResolveScenarioFile() with traversal wiType should return ok=false")
	}
	if _, ok := ResolveScenarioFile(root, "feature", "../../etc"); ok {
		t.Errorf("ResolveScenarioFile() with traversal project should return ok=false")
	}
}

// TestResolveScenarioFile_GenericFallback covers the <d>/<wiType>.md fallback
// when the project-specific file doesn't exist.
func TestResolveScenarioFile_GenericFallback(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, ".repo", "polyforge-coding")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mdPath := filepath.Join(repoDir, "feature.md")
	if err := os.WriteFile(mdPath, []byte("## Step: a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, ok := ResolveScenarioFile(root, "feature", "tether")
	if !ok {
		t.Fatalf("ResolveScenarioFile() ok = false, want true (generic fallback)")
	}
	wantAbs, _ := filepath.Abs(mdPath)
	if got != wantAbs {
		t.Errorf("ResolveScenarioFile() = %q, want %q", got, wantAbs)
	}
}

// TestResolveScenarioFile_NoRepoDir covers a missing .repo dir (no panic,
// ok=false).
func TestResolveScenarioFile_NoRepoDir(t *testing.T) {
	root := t.TempDir()
	if _, ok := ResolveScenarioFile(root, "feature", "tether"); ok {
		t.Errorf("ResolveScenarioFile() with no .repo dir should return ok=false")
	}
}

// TestParseStepGraph_MissingFile asserts the graceful-degrade contract: a
// missing scenario md file is not an error.
func TestParseStepGraph_MissingFile(t *testing.T) {
	g, err := ParseStepGraph(filepath.Join(t.TempDir(), "does-not-exist.md"))
	if err != nil {
		t.Fatalf("ParseStepGraph() err = %v, want nil", err)
	}
	if g != nil {
		t.Fatalf("ParseStepGraph() = %+v, want nil (graceful degrade)", g)
	}
}

// TestParseStepGraph_IncludeAndSequentialFallback builds a 3-step scenario
// (a, b, c) where only c has an explicit previous_steps reference (delivered
// via a single-level @include), and asserts:
//   - nodes are returned in file order [a, b, c]
//   - b has no explicit reference, so the sequential-fallback rule gives it
//     Prev = [a]
//   - c has an explicit reference to "a" (via the included file), which wins
//     over the sequential fallback (which would otherwise give it [b])
func TestParseStepGraph_IncludeAndSequentialFallback(t *testing.T) {
	dir := t.TempDir()
	mdPath := filepath.Join(dir, "feature.tether.md")
	md := "## Step: a\n" +
		"first step body\n" +
		"## Step: b\n" +
		"second step body, no references\n" +
		"## Step: c\n" +
		"third step body\n" +
		"@include: common/c/SKILL.md\n"
	if err := os.WriteFile(mdPath, []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}

	includeDir := filepath.Join(dir, "common", "c")
	if err := os.MkdirAll(includeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	includePath := filepath.Join(includeDir, "SKILL.md")
	if err := os.WriteFile(includePath, []byte(`previous_steps["a"]`), 0o644); err != nil {
		t.Fatal(err)
	}

	g, err := ParseStepGraph(mdPath)
	if err != nil {
		t.Fatalf("ParseStepGraph() err = %v, want nil", err)
	}
	if g == nil {
		t.Fatalf("ParseStepGraph() = nil, want a graph")
	}
	if len(g.Nodes) != 3 {
		t.Fatalf("Nodes = %+v, want 3 nodes", g.Nodes)
	}

	ids := []string{g.Nodes[0].ID, g.Nodes[1].ID, g.Nodes[2].ID}
	want := []string{"a", "b", "c"}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("Nodes[%d].ID = %q, want %q (order = %v)", i, ids[i], want[i], ids)
		}
	}

	a, b, c := g.Nodes[0], g.Nodes[1], g.Nodes[2]
	if len(a.Prev) != 0 {
		t.Errorf("a.Prev = %v, want empty (first node)", a.Prev)
	}
	if len(b.Prev) != 1 || b.Prev[0] != "a" {
		t.Errorf("b.Prev = %v, want [a] (sequential fallback)", b.Prev)
	}
	if len(c.Prev) != 1 || c.Prev[0] != "a" {
		t.Errorf("c.Prev = %v, want [a] (explicit reference via @include, not sequential fallback [b])", c.Prev)
	}
}

// TestParseStepGraph_IncludeTraversalGuard covers the @include path-traversal
// guard: a step whose body names an @include outside the scenario dir must
// not be read (no panic, no error), and the step falls back to whatever its
// own body/sequential-fallback rule would otherwise produce.
func TestParseStepGraph_IncludeTraversalGuard(t *testing.T) {
	dir := t.TempDir()
	mdPath := filepath.Join(dir, "feature.md")
	md := "## Step: a\n" +
		"first step body\n" +
		"## Step: b\n" +
		"second step body\n" +
		"@include: ../../../etc/passwd\n"
	if err := os.WriteFile(mdPath, []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}

	g, err := ParseStepGraph(mdPath)
	if err != nil {
		t.Fatalf("ParseStepGraph() err = %v, want nil", err)
	}
	if g == nil || len(g.Nodes) != 2 {
		t.Fatalf("ParseStepGraph() = %+v, want 2 nodes", g)
	}

	b := g.Nodes[1]
	if len(b.Prev) != 1 || b.Prev[0] != "a" {
		t.Errorf("b.Prev = %v, want [a] (sequential fallback; traversal @include must not be read)", b.Prev)
	}
}

// TestParseStepGraph_DedupsAndPreservesOrder covers the dedup requirement: a
// step body referencing the same previous step twice (dot form + bracket
// form) must produce a single Prev entry, and multiple distinct references
// must preserve first-seen order.
func TestParseStepGraph_DedupsAndPreservesOrder(t *testing.T) {
	dir := t.TempDir()
	mdPath := filepath.Join(dir, "feature.md")
	md := "## Step: a\n" +
		"body\n" +
		"## Step: b\n" +
		"body\n" +
		"## Step: c\n" +
		`x = previous_steps["b"]` + "\n" +
		`y = previous_steps.a` + "\n" +
		`z = previous_steps.b` + "\n"
	if err := os.WriteFile(mdPath, []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}

	g, err := ParseStepGraph(mdPath)
	if err != nil {
		t.Fatalf("ParseStepGraph() err = %v", err)
	}
	c := g.Nodes[2]
	if len(c.Prev) != 2 || c.Prev[0] != "b" || c.Prev[1] != "a" {
		t.Errorf("c.Prev = %v, want [b a] (deduped, first-seen order)", c.Prev)
	}
}

// TestParseStepGraph_NoSelfLoop covers the self-loop guard: a step whose body
// references only its own id via previous_steps must not produce a
// self-edge; the reference is dropped and the sequential fallback (Prev =
// [the immediately preceding step]) applies instead.
func TestParseStepGraph_NoSelfLoop(t *testing.T) {
	dir := t.TempDir()
	mdPath := filepath.Join(dir, "feature.md")
	md := "## Step: a\n" +
		"first step body\n" +
		"## Step: b\n" +
		"second step body\n" +
		"## Step: c\n" +
		`x = previous_steps["c"]` + "\n"
	if err := os.WriteFile(mdPath, []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}

	g, err := ParseStepGraph(mdPath)
	if err != nil {
		t.Fatalf("ParseStepGraph() err = %v, want nil", err)
	}
	c := g.Nodes[2]
	if len(c.Prev) != 1 || c.Prev[0] != "b" {
		t.Errorf("c.Prev = %v, want [b] (self-reference previous_steps[\"c\"] dropped, sequential fallback applies)", c.Prev)
	}
}
