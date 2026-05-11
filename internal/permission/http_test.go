// internal/permission/http_test.go
package permission_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/piaobeizu/tether/internal/permission"
)

func TestRegisterAPI_CanonicalRequest(t *testing.T) {
	m := permission.New()
	mux := http.NewServeMux()
	var capturedReq *permission.Request
	permission.RegisterAPI(mux, m, func(r *permission.Request) {
		capturedReq = r
		m.Decide(r.ID, true, "auto-allow")
	})

	body, _ := json.Marshal(map[string]any{
		"source": "mcp:test-server", "tool_name": "echo", "tool_input": nil,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/permission/request",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200 got %d: %s", w.Code, w.Body)
	}
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["allow"] != true {
		t.Fatalf("expected allow=true, got %v", resp)
	}
	if capturedReq == nil || capturedReq.Source != "mcp:test-server" {
		t.Fatalf("broadcast not called or wrong source: %+v", capturedReq)
	}
}

func TestRegisterAPI_AliasRequest(t *testing.T) {
	m := permission.New()
	mux := http.NewServeMux()
	var capturedReq *permission.Request
	permission.RegisterAPI(mux, m, func(r *permission.Request) {
		capturedReq = r
		m.Decide(r.ID, false, "denied")
	})

	body, _ := json.Marshal(map[string]any{
		"session_id": "sess-1", "tool_name": "bash", "tool_input": map[string]any{"command": "ls"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/permission/request",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200 got %d: %s", w.Code, w.Body)
	}
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["allow"] != false {
		t.Fatalf("expected allow=false, got %v", resp)
	}
	// Alias path must default Source to "claude_hook"
	if capturedReq == nil || capturedReq.Source != "claude_hook" {
		t.Fatalf("alias must default Source=claude_hook, got %+v", capturedReq)
	}
}

func TestRegisterAPI_DecideCanonical(t *testing.T) {
	m := permission.New()
	mux := http.NewServeMux()
	permission.RegisterAPI(mux, m, nil)

	// Add a request manually
	req := &permission.Request{Source: "mcp:srv", ToolName: "foo"}
	m.Add(req)

	body, _ := json.Marshal(map[string]any{"allow": true})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/permission/"+req.ID+"/decide",
		bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("want 204 got %d: %s", w.Code, w.Body)
	}
}

func TestRegisterAPI_AliasAndCanonicalDecideIdentical(t *testing.T) {
	m := permission.New()
	mux := http.NewServeMux()
	permission.RegisterAPI(mux, m, nil)

	req := &permission.Request{Source: "claude_hook", ToolName: "write"}
	m.Add(req)

	// Call /decide via alias path
	body, _ := json.Marshal(map[string]any{"allow": true})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agent/permission/"+req.ID+"/decide",
		bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("alias /decide want 204 got %d", w.Code)
	}
}
