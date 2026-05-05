package claude

import (
	"os"
	"strings"
	"testing"
)

// effectiveEnv unit tests — pure function, no subprocess required.

func TestEffectiveEnv_NilReturnsNil(t *testing.T) {
	if got := effectiveEnv(nil); got != nil {
		t.Errorf("nil overrides should return nil (= inherit parent env); got %v", got)
	}
}

func TestEffectiveEnv_EmptyMapReturnsNil(t *testing.T) {
	if got := effectiveEnv(map[string]string{}); got != nil {
		t.Errorf("empty map should return nil (= inherit parent env); got %v", got)
	}
}

// New key not present in parent env → appended.
func TestEffectiveEnv_NewKeyAppended(t *testing.T) {
	got := effectiveEnv(map[string]string{
		"POLYFORGE_TEST_NEW_KEY_XYZ": "1",
	})
	if !envContains(got, "POLYFORGE_TEST_NEW_KEY_XYZ=1") {
		t.Errorf("new key should be appended; got %d entries, no match", len(got))
	}
}

// Existing parent env key (PATH) → replaced exactly once.
func TestEffectiveEnv_ExistingKeyReplacedNotDuplicated(t *testing.T) {
	got := effectiveEnv(map[string]string{
		"PATH": "/sentinel/bin",
	})
	pathCount := 0
	for _, kv := range got {
		if strings.HasPrefix(kv, "PATH=") {
			pathCount++
			if kv != "PATH=/sentinel/bin" {
				t.Errorf("PATH should equal sentinel; got %q", kv)
			}
		}
	}
	if pathCount != 1 {
		t.Errorf("PATH should appear exactly once after replace; got %d occurrences", pathCount)
	}
}

// Empty-value override is allowed (subprocess sees empty string vs unset).
func TestEffectiveEnv_EmptyValueIsAllowed(t *testing.T) {
	got := effectiveEnv(map[string]string{
		"POLYFORGE_TEST_EMPTY_VAR": "",
	})
	if !envContains(got, "POLYFORGE_TEST_EMPTY_VAR=") {
		t.Errorf("empty-value override should produce 'KEY=' entry; got: %v", filterPolyforgeKeys(got))
	}
}

// Multiple overrides — mix of new + replace, all applied.
func TestEffectiveEnv_MultipleOverridesAllApplied(t *testing.T) {
	got := effectiveEnv(map[string]string{
		"POLYFORGE_TEST_A": "alpha",
		"POLYFORGE_TEST_B": "beta",
		"PATH":             "/sentinel",
	})
	if !envContains(got, "POLYFORGE_TEST_A=alpha") {
		t.Errorf("expected POLYFORGE_TEST_A=alpha")
	}
	if !envContains(got, "POLYFORGE_TEST_B=beta") {
		t.Errorf("expected POLYFORGE_TEST_B=beta")
	}
	if !envContains(got, "PATH=/sentinel") {
		t.Errorf("expected PATH=/sentinel")
	}
	// Sanity: the parent env is preserved (e.g. HOME or PWD likely present).
	if len(got) <= 3 {
		t.Errorf("expected parent env preserved; got only %d entries: %v", len(got), got)
	}
}

// Parent env entries not mentioned in overrides survive.
func TestEffectiveEnv_UnoverriddenKeysSurvive(t *testing.T) {
	// Pick a key we know is in the parent env (HOME).
	homeBefore := os.Getenv("HOME")
	if homeBefore == "" {
		t.Skip("HOME not set; cannot exercise this path portably")
	}
	got := effectiveEnv(map[string]string{
		"POLYFORGE_TEST_OVERRIDE": "x",
	})
	if !envContains(got, "HOME="+homeBefore) {
		t.Errorf("HOME should be inherited unchanged; expected HOME=%q in result", homeBefore)
	}
}

// ───────── helpers ─────────

func envContains(env []string, want string) bool {
	for _, kv := range env {
		if kv == want {
			return true
		}
	}
	return false
}

func filterPolyforgeKeys(env []string) []string {
	out := []string{}
	for _, kv := range env {
		if strings.HasPrefix(kv, "POLYFORGE_TEST_") || strings.HasPrefix(kv, "PATH=") {
			out = append(out, kv)
		}
	}
	return out
}
