package cc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- Version.String formatter --------------------------------------

func TestVersion_String(t *testing.T) {
	cases := []struct {
		v    Version
		want string
	}{
		{Version{Major: 1, Minor: 2, Patch: 3}, "1.2.3"},
		{Version{Major: 2, Minor: 1, Patch: 126}, "2.1.126"},
		{Version{Major: 0, Minor: 0, Patch: 0}, "0.0.0"},
	}
	for _, tc := range cases {
		if got := tc.v.String(); got != tc.want {
			t.Errorf("(%d.%d.%d).String(): got %q want %q", tc.v.Major, tc.v.Minor, tc.v.Patch, got, tc.want)
		}
	}
}

// --- pure parser ----------------------------------------------------

func TestParseVersion_KnownShapes(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		wantMajor int
		wantMinor int
		wantPatch int
	}{
		{"cc canonical with suffix", "2.1.126 (Claude Code)", 2, 1, 126},
		{"cc no suffix", "2.1.123", 2, 1, 123},
		{"leading whitespace", "   1.0.5\n", 1, 0, 5},
		{"v-prefix tolerated", "v3.4.5", 3, 4, 5},
		{"trailing build metadata", "2.1.120-rc1+build.42", 2, 1, 120},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := ParseVersion(tc.raw)
			if err != nil {
				t.Fatalf("unexpected parse error: %v", err)
			}
			if v.Major != tc.wantMajor || v.Minor != tc.wantMinor || v.Patch != tc.wantPatch {
				t.Errorf("parsed: %+v want %d.%d.%d", v, tc.wantMajor, tc.wantMinor, tc.wantPatch)
			}
		})
	}
}

func TestParseVersion_Unparseable(t *testing.T) {
	cases := []string{"", "no digits here", "1.2", "1", "x.y.z"}
	for _, raw := range cases {
		if _, err := ParseVersion(raw); err == nil {
			t.Errorf("ParseVersion(%q): expected ErrVersionUnparseable, got nil", raw)
		} else if !errors.Is(err, ErrVersionUnparseable) {
			t.Errorf("ParseVersion(%q): got %v, want chain to ErrVersionUnparseable", raw, err)
		}
	}
}

// --- Verify range gate ----------------------------------------------

func TestVerify_WithinRange(t *testing.T) {
	r := SupportedRange{Major: 2, Minor: 1, MinPatch: 120}
	for _, v := range []Version{
		{Major: 2, Minor: 1, Patch: 120},
		{Major: 2, Minor: 1, Patch: 123},
		{Major: 2, Minor: 1, Patch: 999},
	} {
		if err := Verify(v, r); err != nil {
			t.Errorf("Verify(%+v): unexpected %v", v, err)
		}
	}
}

func TestVerify_BelowMinPatch(t *testing.T) {
	r := SupportedRange{Major: 2, Minor: 1, MinPatch: 120}
	v := Version{Major: 2, Minor: 1, Patch: 119}
	err := Verify(v, r)
	if !errors.Is(err, ErrVersionUnsupported) {
		t.Errorf("Verify(2.1.119, ≥2.1.120): got %v, want ErrVersionUnsupported chain", err)
	}
	if !strings.Contains(err.Error(), "2.1.119") || !strings.Contains(err.Error(), "2.1.120") {
		t.Errorf("error should reference both detected and required: got %q", err.Error())
	}
}

func TestVerify_WrongMinor(t *testing.T) {
	r := SupportedRange{Major: 2, Minor: 1, MinPatch: 120}
	for _, v := range []Version{
		{Major: 2, Minor: 0, Patch: 999}, // older minor
		{Major: 2, Minor: 2, Patch: 0},   // newer minor — explicit re-verification needed
	} {
		err := Verify(v, r)
		if !errors.Is(err, ErrVersionUnsupported) {
			t.Errorf("Verify(%+v): want ErrVersionUnsupported chain, got %v", v, err)
		}
	}
}

func TestVerify_WrongMajor(t *testing.T) {
	r := SupportedRange{Major: 2, Minor: 1, MinPatch: 120}
	v := Version{Major: 1, Minor: 1, Patch: 200}
	if err := Verify(v, r); !errors.Is(err, ErrVersionUnsupported) {
		t.Errorf("Verify(1.1.200, range 2.1.x): got %v, want ErrVersionUnsupported", err)
	}
}

// --- DefaultSupported sanity ---------------------------------------

func TestDefaultSupported_StableValues(t *testing.T) {
	// gh-13 retro § "Held assumptions": stream-json data plane verified
	// across 2.1.120 → 2.1.123. Floor was set to .120; do not silently
	// move it without retro evidence.
	if DefaultSupported.Major != 2 || DefaultSupported.Minor != 1 || DefaultSupported.MinPatch != 120 {
		t.Errorf("DefaultSupported drifted: %+v want {2 1 120}", DefaultSupported)
	}
}

// --- Detect against fake binary -------------------------------------

// makeFakeClaudeBinary writes a small bash script that echoes the given
// version string on --version and returns its path. Cleaned up via t.Cleanup.
func makeFakeClaudeBinary(t *testing.T, output string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-claude")
	script := "#!/bin/bash\nif [[ \"$1\" == \"--version\" ]]; then\n  echo " + shellEscape(output) + "\n  exit 0\nfi\nexit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	return path
}

func shellEscape(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func TestDetect_FakeBinary_Success(t *testing.T) {
	bin := makeFakeClaudeBinary(t, "2.1.126 (Claude Code)")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	v, err := Detect(ctx, bin)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if v.Major != 2 || v.Minor != 1 || v.Patch != 126 {
		t.Errorf("parsed: %+v want 2.1.126", v)
	}
	if v.Raw != "2.1.126 (Claude Code)" {
		t.Errorf("Raw: got %q want %q", v.Raw, "2.1.126 (Claude Code)")
	}
}

func TestDetect_FakeBinary_BadOutput(t *testing.T) {
	bin := makeFakeClaudeBinary(t, "no version here")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := Detect(ctx, bin)
	if err == nil || !errors.Is(err, ErrVersionUnparseable) {
		t.Errorf("Detect with bad output: got %v, want ErrVersionUnparseable chain", err)
	}
}

func TestDetect_NonExistentBinary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := Detect(ctx, "/nonexistent/claude-binary-xyz")
	if err == nil {
		t.Fatal("Detect with nonexistent binary: expected error")
	}
	// Should NOT be ErrVersionUnparseable — that's a parse error, not a
	// subprocess error. Should NOT be ErrVersionUnsupported either.
	if errors.Is(err, ErrVersionUnparseable) || errors.Is(err, ErrVersionUnsupported) {
		t.Errorf("non-existent binary should produce subprocess error, got: %v", err)
	}
}

func TestDetectAndVerify_FakeBinary_Pass(t *testing.T) {
	bin := makeFakeClaudeBinary(t, "2.1.126 (Claude Code)")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	v, err := DetectAndVerify(ctx, bin, DefaultSupported)
	if err != nil {
		t.Fatalf("DetectAndVerify: %v", err)
	}
	if v.Patch != 126 {
		t.Errorf("DetectAndVerify version: %+v", v)
	}
}

func TestDetectAndVerify_FakeBinary_BelowFloor(t *testing.T) {
	bin := makeFakeClaudeBinary(t, "2.1.100")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := DetectAndVerify(ctx, bin, DefaultSupported)
	if !errors.Is(err, ErrVersionUnsupported) {
		t.Errorf("DetectAndVerify(2.1.100, default): got %v, want ErrVersionUnsupported", err)
	}
}

// --- ctx cancel propagation ----------------------------------------

func TestDetect_CtxCancel(t *testing.T) {
	// Fake binary that sleeps forever — verify ctx cancel kills it.
	dir := t.TempDir()
	bin := filepath.Join(dir, "slow-claude")
	if err := os.WriteFile(bin, []byte("#!/bin/bash\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Detect(ctx, bin)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected ctx cancel error")
	}
	if elapsed > 2*time.Second {
		t.Errorf("ctx cancel slow: took %v", elapsed)
	}
}
