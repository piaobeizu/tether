package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/piaobeizu/tether/internal/wire"
)

// The point of GET /api/v1/version is that the UI stops carrying its own copy
// of the version, so what matters is that the value the daemon resolved is the
// value that reaches the wire — verbatim, not a default that happens to look
// right. tether#70.
func TestHandleVersion_ServesTheVersionItWasGiven(t *testing.T) {
	const want = "v0.5.1-0.20260806071712-6ce6c7453229"

	rec := httptest.NewRecorder()
	handleVersion(want)(rec, httptest.NewRequest(http.MethodGet, "/api/v1/version", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var got wire.VersionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	if got.Version != want {
		t.Fatalf("version = %q, want %q", got.Version, want)
	}
}

// A different value must produce a different body — otherwise the assertion
// above could pass against a hard-coded constant.
func TestHandleVersion_IsNotAConstant(t *testing.T) {
	bodies := map[string]string{}
	for _, v := range []string{"v1.2.3", "v9.9.9-other"} {
		rec := httptest.NewRecorder()
		handleVersion(v)(rec, httptest.NewRequest(http.MethodGet, "/api/v1/version", nil))
		var got wire.VersionResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Version != v {
			t.Fatalf("version = %q, want %q", got.Version, v)
		}
		bodies[v] = got.Version
	}
	if bodies["v1.2.3"] == bodies["v9.9.9-other"] {
		t.Fatal("both versions produced the same body — the handler ignores its argument")
	}
}

// An unknown version is reported as empty rather than guessed at; the UI shows
// a placeholder. Substituting a default here would rebuild the original bug.
func TestHandleVersion_EmptyStaysEmpty(t *testing.T) {
	rec := httptest.NewRecorder()
	handleVersion("")(rec, httptest.NewRequest(http.MethodGet, "/api/v1/version", nil))

	var got wire.VersionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Version != "" {
		t.Fatalf("version = %q, want empty", got.Version)
	}
}
