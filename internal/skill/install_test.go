package skill

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCloner pretends to clone a skill by writing a stand-in directory
// (and optionally a tether.toml). Lets install_test exercise the full
// pool layout without spawning git.
type fakeCloner struct {
	manifestSrc string // empty = skip manifest write
	failOn      string // url that should error out
}

func (f *fakeCloner) clone(ctx context.Context, url, dest, ref string) error {
	if f.failOn != "" && f.failOn == url {
		return errors.New("fakeCloner: synthetic failure")
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	// Always drop a marker so we can detect what got cloned.
	if err := os.WriteFile(filepath.Join(dest, ".cloned-from"), []byte(url), 0o644); err != nil {
		return err
	}
	if f.manifestSrc != "" {
		if err := os.WriteFile(filepath.Join(dest, "tether.toml"), []byte(f.manifestSrc), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func TestPool_InstallLayoutAndInfo(t *testing.T) {
	pool := &Pool{Root: t.TempDir()}
	fc := &fakeCloner{manifestSrc: minimalManifest}

	m, err := pool.Install(context.Background(), "https://x/y.git", "dag", InstallOptions{Cloner: fc.clone})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if m == nil || m.Skill.Name != "dag" {
		t.Fatalf("manifest not parsed; got %+v", m)
	}
	if !pool.Has("dag") {
		t.Errorf("Has(dag) = false after install")
	}
	want := filepath.Join(pool.Root, "dag", ".cloned-from")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("expected %s to exist: %v", want, err)
	}

	// Re-install without --force is a hard error
	if _, err := pool.Install(context.Background(), "https://x/y.git", "dag", InstallOptions{Cloner: fc.clone}); err == nil {
		t.Errorf("re-install without force expected to fail")
	}

	// With --force it succeeds
	if _, err := pool.Install(context.Background(), "https://x/y.git", "dag", InstallOptions{Force: true, Cloner: fc.clone}); err != nil {
		t.Errorf("re-install with force: %v", err)
	}
}

func TestPool_InstallNoManifestIsValid(t *testing.T) {
	pool := &Pool{Root: t.TempDir()}
	fc := &fakeCloner{} // no manifest written
	m, err := pool.Install(context.Background(), "https://x/y.git", "plain", InstallOptions{Cloner: fc.clone})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if m != nil {
		t.Errorf("expected nil manifest for plain plugin, got %+v", m)
	}
}

func TestPool_InstallBadManifestRollsBack(t *testing.T) {
	pool := &Pool{Root: t.TempDir()}
	fc := &fakeCloner{manifestSrc: "[skill]\nname =\n"} // broken toml
	if _, err := pool.Install(context.Background(), "https://x/y.git", "broken", InstallOptions{Cloner: fc.clone}); err == nil {
		t.Fatalf("expected install to fail on bad manifest")
	}
	if pool.Has("broken") {
		t.Errorf("expected pool dir to be cleaned up after bad-manifest rollback")
	}
}

func TestPool_ListAndRemove(t *testing.T) {
	pool := &Pool{Root: t.TempDir()}
	fc := &fakeCloner{manifestSrc: minimalManifest}
	for _, n := range []string{"dag", "media-review", "writing-plans"} {
		if _, err := pool.Install(context.Background(), "https://x/"+n+".git", n, InstallOptions{Cloner: fc.clone}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := pool.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "dag" || got[1] != "media-review" || got[2] != "writing-plans" {
		t.Errorf("List: got %v", got)
	}
	if err := pool.Remove("dag"); err != nil {
		t.Fatal(err)
	}
	if pool.Has("dag") {
		t.Errorf("Has(dag) still true after remove")
	}
	// Remove of absent skill is a no-op.
	if err := pool.Remove("not-there"); err != nil {
		t.Errorf("Remove(absent) returned %v", err)
	}
}

func TestResolveSource(t *testing.T) {
	list := &BlessedList{Skills: map[string]BlessedEntry{
		"dag": {URL: "https://github.com/tether/skill-dag.git"},
	}}

	cases := []struct {
		arg      string
		wantURL  string
		wantName string
		wantErr  bool
	}{
		{"dag", "https://github.com/tether/skill-dag.git", "dag", false},
		{"https://github.com/x/y.git", "https://github.com/x/y.git", "y", false},
		{"git@github.com:foo/bar.git", "git@github.com:foo/bar.git", "bar", false},
		{"unknown", "", "", true},
		{"with spaces", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.arg, func(t *testing.T) {
			url, name, err := ResolveSource(tc.arg, list)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected err: %v", err)
			}
			if url != tc.wantURL {
				t.Errorf("url: got %q want %q", url, tc.wantURL)
			}
			if name != tc.wantName {
				t.Errorf("name: got %q want %q", name, tc.wantName)
			}
		})
	}
}

func TestResolveSkills_FailDefault(t *testing.T) {
	pool := &Pool{Root: t.TempDir()}
	_, _, err := ResolveSkills(context.Background(), pool, nil, "/work/xiyou", []string{"dag"}, FallbackFail, nil)
	if err == nil {
		t.Fatal("expected error on missing skill")
	}
	msg := err.Error()
	for _, want := range []string{"workspace /work/xiyou requires skills not in global pool", "tether skill install dag", "--allow-missing /work/xiyou"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing %q\n%s", want, msg)
		}
	}
}

func TestResolveSkills_FailDefault_NoWorkspacePath(t *testing.T) {
	pool := &Pool{Root: t.TempDir()}
	_, _, err := ResolveSkills(context.Background(), pool, nil, "", []string{"dag"}, FallbackFail, nil)
	if err == nil {
		t.Fatal("expected error on missing skill")
	}
	if !strings.Contains(err.Error(), "<workspace>") {
		t.Errorf("expected fallback placeholder when workspace path empty; got:\n%s", err.Error())
	}
}

func TestResolveSkills_AllowMissing(t *testing.T) {
	pool := &Pool{Root: t.TempDir()}
	fc := &fakeCloner{manifestSrc: minimalManifest}
	if _, err := pool.Install(context.Background(), "https://x/dag.git", "dag", InstallOptions{Cloner: fc.clone}); err != nil {
		t.Fatal(err)
	}
	resolved, warn, err := ResolveSkills(context.Background(), pool, nil, "", []string{"dag", "media-review"}, FallbackAllowMissing, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(resolved) != 1 || resolved[0] != "dag" {
		t.Errorf("resolved: got %v", resolved)
	}
	if !strings.Contains(warn, "media-review") {
		t.Errorf("warning missing skill name: %q", warn)
	}
}

func TestResolveSkills_AutoInstall(t *testing.T) {
	pool := &Pool{Root: t.TempDir()}
	list := &BlessedList{Skills: map[string]BlessedEntry{
		"dag": {URL: "https://x/dag.git"},
	}}
	fc := &fakeCloner{manifestSrc: minimalManifest}
	resolved, _, err := ResolveSkills(context.Background(), pool, list, "", []string{"dag"}, FallbackAutoInstall, fc.clone)
	if err != nil {
		t.Fatalf("auto-install: %v", err)
	}
	if len(resolved) != 1 || resolved[0] != "dag" {
		t.Errorf("resolved: %v", resolved)
	}
	if !pool.Has("dag") {
		t.Errorf("dag was not installed by auto-install path")
	}
}

func TestResolveSkills_AutoInstallFallsBackOnUnknownName(t *testing.T) {
	pool := &Pool{Root: t.TempDir()}
	list := &BlessedList{Skills: map[string]BlessedEntry{}}
	_, _, err := ResolveSkills(context.Background(), pool, list, "", []string{"missing"}, FallbackAutoInstall, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "auto-install") {
		t.Errorf("expected auto-install context in error: %v", err)
	}
}

func TestLoadBlessedList_Absent(t *testing.T) {
	dir := t.TempDir()
	bl, err := LoadBlessedList(filepath.Join(dir, "skills.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(bl.Skills) != 0 {
		t.Errorf("expected empty list, got %v", bl.Skills)
	}
}

const minimalManifest = `
[skill]
name = "dag"
version = "0.1.0"

[skill.agents]
primary = "cc"
`
