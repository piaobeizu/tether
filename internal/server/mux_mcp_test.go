package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMCPPathsReturn501(t *testing.T) {
	mux := http.NewServeMux()
	registerMCPStubs(mux)

	cases := []string{
		"/mcp",
		"/api/v1/mcp/",
		"/api/v1/mcp/tools",
		"/api/v1/mcp/anything",
	}
	for _, path := range cases {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		mux.ServeHTTP(rec, r)
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("path %s: expected 501, got %d", path, rec.Code)
		}
	}
}
