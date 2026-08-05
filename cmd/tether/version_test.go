package main

import (
	"runtime/debug"
	"strings"
	"testing"
)

func TestFormatVersion(t *testing.T) {
	const def = "v0.1.0-dev"

	buildInfo := func(mainVersion string, settings ...debug.BuildSetting) *debug.BuildInfo {
		return &debug.BuildInfo{
			Main:     debug.Module{Version: mainVersion},
			Settings: settings,
		}
	}

	tests := []struct {
		name     string
		injected string
		bi       *debug.BuildInfo
		ok       bool
		want     string
	}{
		{
			name:     "ldflags injected version returned verbatim, ignoring build info",
			injected: "v0.5.1",
			bi: buildInfo("v0.9.9",
				debug.BuildSetting{Key: "vcs.revision", Value: "0e48e1cb4c8fbdd912010909aa5de3932436a8aa"},
				debug.BuildSetting{Key: "vcs.modified", Value: "true"},
			),
			ok:   true,
			want: "v0.5.1",
		},
		{
			name:     "default + clean vcs.revision",
			injected: def,
			bi: buildInfo(def,
				debug.BuildSetting{Key: "vcs.revision", Value: "0e48e1cb4c8fbdd912010909aa5de3932436a8aa"},
				debug.BuildSetting{Key: "vcs.modified", Value: "false"},
			),
			ok:   true,
			want: "v0.1.0-dev+0e48e1cb4c8f",
		},
		{
			name:     "default + vcs.revision + dirty",
			injected: def,
			bi: buildInfo(def,
				debug.BuildSetting{Key: "vcs.revision", Value: "0e48e1cb4c8fbdd912010909aa5de3932436a8aa"},
				debug.BuildSetting{Key: "vcs.modified", Value: "true"},
			),
			ok:   true,
			want: "v0.1.0-dev+0e48e1cb4c8f.dirty",
		},
		{
			name:     "Main.Version is (devel) falls back to default as base",
			injected: def,
			bi: buildInfo("(devel)",
				debug.BuildSetting{Key: "vcs.revision", Value: "0e48e1cb4c8fbdd912010909aa5de3932436a8aa"},
				debug.BuildSetting{Key: "vcs.modified", Value: "false"},
			),
			ok:   true,
			want: "v0.1.0-dev+0e48e1cb4c8f",
		},
		{
			// The shape the currently deployed binary actually has: Go's
			// pseudo-version already ends in the short commit hash, so the
			// hash must NOT be appended a second time.
			name:     "Main.Version is a real pseudo-version used as base, hash not duplicated",
			injected: def,
			bi: buildInfo("v0.5.1-0.20260805045854-0e48e1cb4c8f",
				debug.BuildSetting{Key: "vcs.revision", Value: "0e48e1cb4c8fbdd912010909aa5de3932436a8aa"},
				debug.BuildSetting{Key: "vcs.modified", Value: "false"},
			),
			ok:   true,
			want: "v0.5.1-0.20260805045854-0e48e1cb4c8f",
		},
		{
			// Go marks a dirty pseudo-version with its own "+dirty" suffix.
			// Strip it so ".dirty" below is the single canonical marker,
			// rather than emitting "...+dirty+<hash>.dirty".
			name:     "dirty pseudo-version reports dirty exactly once",
			injected: def,
			bi: buildInfo("v0.5.1-0.20260805045854-0e48e1cb4c8f+dirty",
				debug.BuildSetting{Key: "vcs.revision", Value: "0e48e1cb4c8fbdd912010909aa5de3932436a8aa"},
				debug.BuildSetting{Key: "vcs.modified", Value: "true"},
			),
			ok:   true,
			want: "v0.5.1-0.20260805045854-0e48e1cb4c8f.dirty",
		},
		{
			name:     "vcs.modified with no revision still reports dirty",
			injected: def,
			bi: buildInfo("(devel)",
				debug.BuildSetting{Key: "vcs.modified", Value: "true"},
			),
			ok:   true,
			want: "v0.1.0-dev.dirty",
		},
		{
			name:     "build info absent returns bare default",
			injected: def,
			bi:       nil,
			ok:       false,
			want:     def,
		},
		{
			name:     "vcs settings absent returns bare base, no +",
			injected: def,
			bi:       buildInfo("v0.5.1-0.20260805045854-0e48e1cb4c8f"),
			ok:       true,
			want:     "v0.5.1-0.20260805045854-0e48e1cb4c8f",
		},
		{
			name:     "empty injection is treated as not-injected, never a blank line",
			injected: "",
			bi: buildInfo("(devel)",
				debug.BuildSetting{Key: "vcs.revision", Value: "0e48e1cb4c8fbdd912010909aa5de3932436a8aa"},
			),
			ok:   true,
			want: "v0.1.0-dev+0e48e1cb4c8f",
		},
		{
			// The shape a release build from a clean tagged clone produces.
			name:     "exact tag base plus revision",
			injected: def,
			bi: buildInfo("v0.5.1",
				debug.BuildSetting{Key: "vcs.revision", Value: "0e48e1cb4c8fbdd912010909aa5de3932436a8aa"},
				debug.BuildSetting{Key: "vcs.modified", Value: "false"},
			),
			ok:   true,
			want: "v0.5.1+0e48e1cb4c8f",
		},
		{
			name:     "revision shorter than 12 chars does not panic",
			injected: def,
			bi: buildInfo(def,
				debug.BuildSetting{Key: "vcs.revision", Value: "abc123"},
			),
			ok:   true,
			want: "v0.1.0-dev+abc123",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatVersion(tc.injected, def, tc.bi, tc.ok)
			if got != tc.want {
				t.Errorf("formatVersion(%q, %q, ..., %v) = %q, want %q", tc.injected, def, tc.ok, got, tc.want)
			}
		})
	}
}

// Every case above calls formatVersion directly, which leaves the wiring
// untested: swap resolveVersion's first two arguments and all of them still
// pass while ldflags injection silently stops working — the exact latent bug
// this change exists to fix. This test drives the real globals instead.
//
// It also pins the var-not-const invariant. `-X main.version=` is a silent
// no-op against a const (verified: no linker error, the default is simply
// printed), so if a future edit turns `version` back into a const, release
// builds quietly revert to reporting v0.1.0-dev. Assigning to it here is
// what makes that regression fail to compile.
func TestResolveVersion_UsesInjectedValue(t *testing.T) {
	orig := version
	t.Cleanup(func() { version = orig })

	version = "v9.9.9-injected"
	if got := resolveVersion(); got != "v9.9.9-injected" {
		t.Fatalf("resolveVersion() = %q, want the injected value %q", got, "v9.9.9-injected")
	}
}

// With no injection, resolveVersion must fall through to the build-info tier
// and never return an empty string, whatever the toolchain stamped.
func TestResolveVersion_UninjectedIsNonEmptyAndSingleToken(t *testing.T) {
	orig := version
	t.Cleanup(func() { version = orig })

	version = defaultVersion
	got := resolveVersion()
	if got == "" {
		t.Fatal("resolveVersion() returned an empty string")
	}
	if strings.ContainsAny(got, " \t\n") {
		t.Errorf("resolveVersion() = %q, want a single whitespace-free token", got)
	}
	if !strings.HasPrefix(got, defaultVersion) && !strings.Contains(got, ".") {
		t.Errorf("resolveVersion() = %q, want it to start from the default or a real module version", got)
	}
}
