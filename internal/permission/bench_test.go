package permission_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/piaobeizu/tether/internal/permission"
)

// BenchmarkManagerAddDecide measures the in-memory permission roundtrip:
// Add (registers request, allocates channel + timer) + Decide (resolves it).
// This is the hot path for every tool call that goes through the permission gate.
func BenchmarkManagerAddDecide(b *testing.B) {
	m := permission.New()
	req := &permission.Request{Source: "claude_hook", ToolName: "Bash"}

	b.ResetTimer()
	for b.Loop() {
		ch := m.Add(req)
		m.Decide(req.ID, true, "bench")
		<-ch
	}
}

// BenchmarkHTTPPermissionRoundtrip measures the full HTTP permission cycle
// (POST /request → broadcast callback decides → 200 OK) using httptest.
// This is the latency floor for the cc PreToolUse hook path.
func BenchmarkHTTPPermissionRoundtrip(b *testing.B) {
	m := permission.New()
	mux := http.NewServeMux()
	permission.RegisterAPI(mux, m, func(r *permission.Request) {
		m.Decide(r.ID, true, "bench")
	})

	body, _ := json.Marshal(map[string]any{
		"source": "claude_hook", "tool_name": "Bash",
		"tool_input": map[string]any{"command": "ls /tmp"},
	})

	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/permission/request",
			bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			b.Fatalf("unexpected status %d", w.Code)
		}
	}
}
