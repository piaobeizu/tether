package server

import (
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/piaobeizu/tether/web"
)

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
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		spy := &spy404{ResponseWriter: w}
		fileServer.ServeHTTP(spy, r)
		if spy.code == http.StatusNotFound {
			// Unknown path — serve index.html so the SPA router handles it client-side.
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			fileServer.ServeHTTP(w, r2)
		}
	})
}
