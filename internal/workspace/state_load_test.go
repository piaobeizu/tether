package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

// These tests pin the three outcomes of NewRegistry's load step (tether#65),
// because which one a corrupt ~/.tether/workspaces.json produces decides which
// operator-facing error a `?ws=` request gets.
//
// The chain they guard, end to end: NewRegistry returns an error →
// server/lifecycle.go Step 2b logs a warning and leaves cfg.WsRegistry nil (so
// session.Registry.Workspaces stays nil) → session.resolveWorkspace's nil branch
// answers wire.ErrCodeNoWorkspaceRegistry instead of ErrCodeUnknownWorkspace.
// Before this slice, load()'s error was discarded, so a corrupt file yielded a
// non-nil EMPTY registry and that branch was unreachable — every request was
// blamed on a deleted workspace. Only the FIRST link is unit-testable here; the
// rest is wiring already covered by session's own tests plus this wi's
// live-verify, so what these tests exist to catch is a regression that puts the
// `_ = r.load()` behaviour back and silently re-orphans the branch.
//
// t.Setenv (not t.Parallel) because NewRegistry resolves its path through
// os.UserHomeDir, which reads $HOME on Linux — that is the only seam for pointing
// it at a temp dir, and it is process-global.

// writeHome points $HOME at a fresh temp dir and, when content is non-nil, plants
// it as ~/.tether/workspaces.json. It returns the registry file's path.
//
// The hinge is content != nil, NOT len(content) > 0 — []byte("") is non-nil, so
// passing it plants a real zero-byte file (a case below depends on that) while
// passing nil leaves no file at all.
func writeHome(t *testing.T, content []byte) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".tether", "workspaces.json")
	if content != nil {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return path
}

// A first run has no registry file. That must stay a successful empty registry:
// erroring would refuse every `?ws=` request on a daemon that is simply new, and
// the browser would report a broken registry to a user who has just never added a
// workspace.
func TestNewRegistry_MissingFileIsNotAnError(t *testing.T) {
	writeHome(t, nil)

	r, err := NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry with no registry file = %v, want nil", err)
	}
	if r == nil {
		t.Fatal("NewRegistry returned a nil registry with a nil error")
	}
	if got := r.List(); len(got) != 0 {
		t.Errorf("List = %v, want empty", got)
	}
}

// The case this slice exists for: a file that is present but not parseable is a
// real failure and must be reported, so the daemon can leave the registry nil and
// say "the registry failed to load" rather than "that workspace is gone".
func TestNewRegistry_CorruptFileIsAnError(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"not json at all", "this is not json"},
		{"truncated array", `[{"id":"a","name":"n","path":"/tmp"`},
		{"wrong shape", `{"workspaces":[]}`}, // object where a list is expected
		// The one corruption shape that makes json.Unmarshal fail AFTER it has
		// already written into the destination slice (UnmarshalTypeError on the
		// second field, not a syntax error). Worth its own case: it is the shape
		// where "returns an error" and "the registry is unusable" could most
		// plausibly come apart, and the assertion below that r is nil is what says
		// no caller can ever see that half-filled list.
		{"right shape, wrong field type", `[{"id":123}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeHome(t, []byte(tc.body))

			r, err := NewRegistry()
			if err == nil {
				t.Fatalf("NewRegistry with a corrupt registry file = nil error (registry=%v), want an error — "+
					"a non-nil empty registry here would refuse every ?ws= request as unknown_workspace "+
					"instead of no_workspace_registry", r.List())
			}
			// Nil registry, not a usable-looking empty one: lifecycle.go Step 2b keys
			// entirely off this being nil, and a non-nil *Registry stored into the
			// WorkspaceLookup interface would be a non-nil interface holding a nil
			// pointer — which would skip resolveWorkspace's nil branch and call
			// through on a nil receiver.
			if r != nil {
				t.Errorf("NewRegistry returned a non-nil registry alongside its error: %v", r.List())
			}
		})
	}
}

// A well-formed file still loads — the guard above must not have been bought by
// making the happy path stricter.
func TestNewRegistry_ValidFileLoads(t *testing.T) {
	writeHome(t, []byte(`[{"id":"abc123","name":"proj","path":"/tmp/proj"}]`))

	r, err := NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry with a valid registry file = %v, want nil", err)
	}
	got := r.List()
	if len(got) != 1 || got[0].ID != "abc123" || got[0].Path != "/tmp/proj" {
		t.Fatalf("List = %+v, want one entry abc123 -> /tmp/proj", got)
	}
	// And the id resolves through the lookup the chat route actually uses, so the
	// loaded entry is usable and not merely present.
	if p, ok := r.Path("abc123"); !ok || p != "/tmp/proj" {
		t.Errorf("Path(abc123) = (%q, %v), want (/tmp/proj, true)", p, ok)
	}
}

// An empty file is a distinct case worth stating: `[]` is valid JSON for an empty
// list and must load, while a zero-byte file is not valid JSON and must not.
// Nothing writes a zero-byte registry deliberately, but a crash mid-save (or a
// disk-full truncation) can leave one, and that is precisely the corruption an
// operator needs named rather than mistranslated into "workspace deleted".
func TestNewRegistry_EmptyJSONArrayLoadsButZeroBytesDoesNot(t *testing.T) {
	t.Run("empty array", func(t *testing.T) {
		writeHome(t, []byte(`[]`))
		r, err := NewRegistry()
		if err != nil {
			t.Fatalf("NewRegistry with `[]` = %v, want nil", err)
		}
		if got := r.List(); len(got) != 0 {
			t.Errorf("List = %v, want empty", got)
		}
	})
	t.Run("zero bytes", func(t *testing.T) {
		writeHome(t, []byte(``))
		if _, err := NewRegistry(); err == nil {
			t.Error("NewRegistry with a zero-byte registry file = nil error, want an error")
		}
	})
}
