package workspace

import "testing"

// TestRegistry_Path — the id→path lookup the chat route's `?ws=` parameter is
// resolved through (tether#52, session.WorkspaceLookup).
//
// The false second return is the whole contract: an unregistered id must be
// DISTINGUISHABLE from a registered one, because the caller turns it into a
// refusal. If a miss were reported as an empty path with no flag, a caller could
// spend it as "use the default directory" and an unknown id would silently select
// the daemon's own workspace root instead of being rejected.
func TestRegistry_Path(t *testing.T) {
	dir := t.TempDir()
	r := &Registry{path: dir + "/workspaces.json"}

	added, err := r.Add("project-a", dir)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, ok := r.Path(added.ID)
	if !ok {
		t.Fatal("Path = not found for a registered workspace")
	}
	if got != added.Path {
		t.Errorf("Path = %q, want %q", got, added.Path)
	}

	for _, id := range []string{"", "deadbeefdeadbeef", added.ID + "x"} {
		if p, ok := r.Path(id); ok {
			t.Errorf("Path(%q) = (%q, true), want not-found", id, p)
		}
	}

	// Removing a workspace un-resolves its id, so a stale id the browser is still
	// holding is refused rather than honoured.
	if err := r.Remove(added.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if p, ok := r.Path(added.ID); ok {
		t.Errorf("Path after Remove = (%q, true), want not-found", p)
	}
}
