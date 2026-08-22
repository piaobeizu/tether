package workspace

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// TestDeleteWorkspaceEndpoint_ReportsARolledBackRemoval — tether#162, the wire half
// of TestRemove_KeepsTheRegistrationWhenTheWriteFails.
//
// It lives in its own file rather than beside
// TestDeleteWorkspaceEndpoint_ReportsAFailedDetach only because api_test.go was
// outside this change's write scope. It belongs with that one.
//
// # What this measures that the registry-level test cannot
//
// That the rolled-back state reaches the CALLER as its own sentence. The registry
// putting the record back is invisible over HTTP: pre-fix, this same request
// answered 500 with registryInternalErrorBody — "the daemon could not complete
// this request" — which is true and is also the whole of what a caller was told
// about a workspace whose overlays had just been taken away for good. The gate is
// therefore the body, and it is asserted by EQUALITY with the sentinel's own text,
// the rule tether#147 established: a body that merely mentions overlays somewhere
// could still have been assembled from the error value.
//
// The complement is asserted too — that the body is not the generic one — because
// that is the exact value it had on the broken build, and naming it is what makes
// this a gate rather than a description.
//
// # Why the daemon-side path check is repeated here
//
// The error this refusal wraps is saveLocked's *fs.PathError, which carries the
// absolute path of the registry file. Deriving the body from the sentinel is what
// keeps it out, and that property is worth pinning at the one place where the
// wrapped error and the response meet.
func TestDeleteWorkspaceEndpoint_ReportsARolledBackRemoval(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "workspaces.json")
	reg := &Registry{path: file}

	ws, err := reg.Add("w", t.TempDir())
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	// A cleanup that SUCCEEDS: this is not the tether#156 refusal (that one is
	// TestDeleteWorkspaceEndpoint_ReportsAFailedDetach's 409). The overlays came
	// away exactly as asked, and then the registry could not be written.
	detached := 0
	reg.BindOverlayCleanup(func(string) error { detached++; return nil })

	// Only now does the registry become unwritable, so the entry above is real and
	// the file still holds it — see blockTheNextWrite for why this shape and not a
	// chmod.
	blockTheNextWrite(t, file)

	mux := http.NewServeMux()
	RegisterAPI(mux, reg)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/workspaces/"+ws.ID, nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("DELETE whose write could not land -> %d, want 500; body %q",
			rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	if detached != 1 {
		t.Fatalf("the overlay cleanup ran %d times, want 1 — without it there is no side "+
			"effect for this body to be about", detached)
	}

	got := strings.TrimSpace(rec.Body.String())
	if got != ErrRemoveNotRecorded.Error() {
		t.Errorf("500 body = %q, want exactly %q", got, ErrRemoveNotRecorded.Error())
	}
	if got == registryInternalErrorBody {
		t.Errorf("500 body = %q, the generic body.\nPre-fix: exactly this — the caller was "+
			"told the request failed and nothing about the overlays that had already been "+
			"detached and would not be coming back.", got)
	}
	if strings.Contains(got, dir) {
		t.Errorf("the 500 body names the daemon's own registry directory: %q", got)
	}

	// The registration really is still listed, so the sentence the body sends is
	// the state the daemon is actually in.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/workspaces", nil))
	if !strings.Contains(rec.Body.String(), ws.ID) {
		t.Errorf("GET after the refused DELETE = %q, want it to still list %s — the body says "+
			"the workspace is still registered", strings.TrimSpace(rec.Body.String()), ws.ID)
	}
}
