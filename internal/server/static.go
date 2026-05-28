package server

import (
	"io/fs"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"

	"github.com/piaobeizu/tether/web"
)

func init() {
	// Go's built-in mime database doesn't map .webmanifest. Without this,
	// http.FileServer falls back to sniffing JSON as text/plain, which makes
	// Lighthouse and strict PWA validators reject the manifest.
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
}

// noCacheWriter injects Cache-Control: no-cache at WriteHeader time so the
// header survives even when http.FileServer/ServeContent resets headers internally.
type noCacheWriter struct {
	http.ResponseWriter
}

func (n *noCacheWriter) WriteHeader(code int) {
	n.ResponseWriter.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	n.ResponseWriter.WriteHeader(code)
}

// spy404 wraps a ResponseWriter and intercepts 404 responses so the caller
// can detect them and serve a fallback without opening the file twice.
type spy404 struct {
	http.ResponseWriter
	code int
}

func (s *spy404) WriteHeader(code int) {
	s.code = code
	if code != http.StatusNotFound {
		s.ResponseWriter.WriteHeader(code)
	}
}

func (s *spy404) Write(b []byte) (int, error) {
	if s.code == http.StatusNotFound {
		return len(b), nil // discard 404 body
	}
	return s.ResponseWriter.Write(b)
}

// newStaticHandler returns an http.Handler that:
//   - In dev mode (devFrontendURL != ""): reverse-proxies to the Vite dev server
//     for all requests not matched by other routes (§10.C.2).
//   - Otherwise: serves the embedded web/dist/ SPA via embed.FS with index.html
//     fallback for unknown paths (SPA client-side routing).
func newStaticHandler(devFrontendURL string) http.Handler {
	if devFrontendURL != "" {
		target, err := url.Parse(devFrontendURL)
		if err == nil {
			return httputil.NewSingleHostReverseProxy(target)
		}
	}
	sub, err := fs.Sub(web.DistFS, "dist")
	if err != nil {
		panic("embed.FS sub failed: " + err.Error())
	}
	// Read index.html once at startup — it's embedded, never changes at runtime.
	indexHTML, indexErr := fs.ReadFile(sub, "index.html")

	serveIndex := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		if indexErr != nil {
			http.Error(w, "index.html not found", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(indexHTML)
	}

	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// SPA shell paths: always serve index.html with no-cache.
		p := r.URL.Path
		if p == "/" || p == "/index.html" || p == "/auth" {
			serveIndex(w)
			return
		}
		spy := &spy404{ResponseWriter: w}
		fileServer.ServeHTTP(spy, r)
		if spy.code != http.StatusNotFound {
			return
		}
		// Clear headers poisoned by the 404 probe (e.g. Content-Type: text/plain).
		// http.FileServer only sets Content-Type when it's absent, so we must
		// clear it before writing our own response.
		for k := range w.Header() {
			delete(w.Header(), k)
		}
		// Static-asset-like paths (favicon.ico, foo.png, etc.) must return a
		// real 404 rather than the SPA shell — otherwise browsers happily
		// "render" index.html as a favicon and tools like Lighthouse complain.
		if isStaticAssetPath(r.URL.Path) {
			http.Error(w, "404 page not found", http.StatusNotFound)
			return
		}
		serveIndex(w)
	})
}

// isStaticAssetPath reports whether p looks like a request for a static file
// (final path segment contains a dot) rather than an SPA route.
func isStaticAssetPath(p string) bool {
	return strings.Contains(path.Base(p), ".")
}
