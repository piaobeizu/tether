package permission_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/permission"
	"github.com/piaobeizu/tether/internal/wire"
)

type stubReg struct {
	last wire.Envelope
}

func (s *stubReg) BroadcastAll(env wire.Envelope) { s.last = env }

func TestPermStateRoundTrip(t *testing.T) {
	ps := permission.NewPermState()
	req := &permission.PermRequest{ID: "x1", ToolName: "Bash"}
	ch, err := ps.Add(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ps.Decide("x1", false) })
	if !ps.Decide("x1", true) {
		t.Fatal("Decide returned false for known id")
	}
	if allow := <-ch; !allow {
		t.Fatal("expected allow=true")
	}
}

func TestPermStateUnknownID(t *testing.T) {
	ps := permission.NewPermState()
	if ps.Decide("no-such-id", true) {
		t.Fatal("Decide should return false for unknown id")
	}
}

func TestPermStateDuplicateID(t *testing.T) {
	ps := permission.NewPermState()
	req := &permission.PermRequest{ID: "dup1", ToolName: "Bash"}
	_, err := ps.Add(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ps.Decide("dup1", false) })
	_, err = ps.Add(&permission.PermRequest{ID: "dup1", ToolName: "Write"})
	if err == nil {
		t.Fatal("expected error for duplicate ID, got nil")
	}
}

// TestDecideCanonicalPath verifies POST /api/v1/permission/{id}/decide returns 204.
func TestDecideCanonicalPath(t *testing.T) {
	ps := permission.NewPermState()
	reg := &stubReg{}
	mux := http.NewServeMux()
	permission.RegisterPermAPI(mux, ps, reg)

	req := &permission.PermRequest{ID: "abc123", ToolName: "Write"}
	if _, err := ps.Add(req); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ps.Decide("abc123", false) })

	body, _ := json.Marshal(map[string]any{"allow": true})
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/permission/abc123/decide",
		bytes.NewReader(body))
	mux.ServeHTTP(rec, r)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestDecideAliasPath verifies old /api/v1/agent/permission/{id}/decide still works.
func TestDecideAliasPath(t *testing.T) {
	ps := permission.NewPermState()
	reg := &stubReg{}
	mux := http.NewServeMux()
	permission.RegisterPermAPI(mux, ps, reg)

	req := &permission.PermRequest{ID: "def456", ToolName: "Bash"}
	if _, err := ps.Add(req); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ps.Decide("def456", false) })

	body, _ := json.Marshal(map[string]any{"allow": false})
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agent/permission/def456/decide",
		bytes.NewReader(body))
	mux.ServeHTTP(rec, r)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestDecideUnknownID verifies decide on unknown id returns 404.
func TestDecideUnknownID(t *testing.T) {
	ps := permission.NewPermState()
	reg := &stubReg{}
	mux := http.NewServeMux()
	permission.RegisterPermAPI(mux, ps, reg)

	body, _ := json.Marshal(map[string]any{"allow": true})
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/permission/notexist/decide",
		bytes.NewReader(body))
	mux.ServeHTTP(rec, r)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// TestDecideNegativeCases covers method rejection, malformed JSON, bad path segment.
func TestDecideNegativeCases(t *testing.T) {
	ps := permission.NewPermState()
	reg := &stubReg{}
	mux := http.NewServeMux()
	permission.RegisterPermAPI(mux, ps, reg)

	req := &permission.PermRequest{ID: "neg1", ToolName: "Bash"}
	if _, err := ps.Add(req); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ps.Decide("neg1", false) })

	cases := []struct {
		name   string
		method string
		path   string
		body   string
		want   int
	}{
		{"wrong method GET decide", http.MethodGet, "/api/v1/permission/neg1/decide", `{"allow":true}`, http.StatusMethodNotAllowed},
		{"malformed JSON decide", http.MethodPost, "/api/v1/permission/neg1/decide", `not-json`, http.StatusBadRequest},
		{"wrong path segment", http.MethodPost, "/api/v1/permission/neg1/other", `{"allow":true}`, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			mux.ServeHTTP(rec, r)
			if rec.Code != tc.want {
				t.Fatalf("expected %d, got %d", tc.want, rec.Code)
			}
		})
	}
}

// TestRequestContextCancellation verifies the handler exits cleanly when client disconnects.
func TestRequestContextCancellation(t *testing.T) {
	ps := permission.NewPermState()
	reg := &stubReg{}
	mux := http.NewServeMux()
	permission.RegisterPermAPI(mux, ps, reg)

	ctx, cancel := context.WithCancel(context.Background())
	body, _ := json.Marshal(map[string]any{
		"session_id": "s-cancel",
		"tool_name":  "Bash",
		"tool_input": json.RawMessage(`{}`),
	})
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/permission/request",
		bytes.NewReader(body)).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		mux.ServeHTTP(rec, r)
		close(done)
	}()

	cancel()
	select {
	case <-done:
		// ok
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after context cancellation")
	}
}
