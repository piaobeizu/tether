package agent

import (
	"os"
	"testing"
)

// TestResolveWorkdir pins the empty-Workdir fallback semantics shared by
// every AgentProvider (tether#51): a non-empty Workdir passes through
// unchanged; an empty one resolves to the daemon's own process cwd, making
// the effective directory observable instead of implied by exec's
// empty-Dir default.
func TestResolveWorkdir(t *testing.T) {
	wantCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	tests := []struct {
		name    string
		workdir string
		want    string
	}{
		{"non-empty passes through unchanged", "/some/explicit/dir", "/some/explicit/dir"},
		{"empty falls back to process cwd", "", wantCwd},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveWorkdir(tt.workdir); got != tt.want {
				t.Errorf("ResolveWorkdir(%q) = %q, want %q", tt.workdir, got, tt.want)
			}
		})
	}
}
