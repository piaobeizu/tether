package server

import (
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebmanifestMIMERegistered(t *testing.T) {
	got := mime.TypeByExtension(".webmanifest")
	if !strings.HasPrefix(got, "application/manifest+json") {
		t.Fatalf(".webmanifest MIME = %q, want application/manifest+json", got)
	}
}

func TestStaticHandler(t *testing.T) {
	h := newStaticHandler("")

	cases := []struct {
		name        string
		path        string
		wantCode    int
		wantCTPart  string
		wantBodyHas string
	}{
		{
			name:        "root serves SPA index",
			path:        "/",
			wantCode:    http.StatusOK,
			wantCTPart:  "text/html",
			wantBodyHas: "<!DOCTYPE html>",
		},
		{
			name:        "unknown page route falls back to SPA",
			path:        "/some/spa/route",
			wantCode:    http.StatusOK,
			wantCTPart:  "text/html",
			wantBodyHas: "<!DOCTYPE html>",
		},
		{
			name:       "missing favicon returns 404 (not SPA HTML)",
			path:       "/favicon.ico",
			wantCode:   http.StatusNotFound,
			wantCTPart: "text/plain",
		},
		{
			name:       "missing asset returns 404",
			path:       "/assets/does-not-exist.js",
			wantCode:   http.StatusNotFound,
			wantCTPart: "text/plain",
		},
		{
			name:       "manifest.webmanifest served as application/manifest+json",
			path:       "/manifest.webmanifest",
			wantCode:   http.StatusOK,
			wantCTPart: "application/manifest+json",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.wantCode {
				t.Fatalf("code = %d, want %d (body=%q)", rec.Code, tc.wantCode, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, tc.wantCTPart) {
				t.Fatalf("Content-Type = %q, want substring %q", ct, tc.wantCTPart)
			}
			if tc.wantBodyHas != "" && !strings.Contains(rec.Body.String(), tc.wantBodyHas) {
				t.Fatalf("body missing %q; got %q", tc.wantBodyHas, rec.Body.String())
			}
		})
	}
}

func TestIsStaticAssetPath(t *testing.T) {
	cases := map[string]bool{
		"/favicon.ico":          true,
		"/icons/icon-192.png":   true,
		"/manifest.webmanifest": true,
		"/assets/index.js":      true,
		"/":                     false,
		"/auth":                 false,
		"/some/spa/route":       false,
		"/dotted.dir/page":      false,
	}
	for p, want := range cases {
		if got := isStaticAssetPath(p); got != want {
			t.Errorf("isStaticAssetPath(%q) = %v, want %v", p, got, want)
		}
	}
}
