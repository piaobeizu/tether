package server

import (
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/piaobeizu/tether/web"
)

// newStaticHandler returns an http.Handler that:
//   - In dev mode (devFrontendURL != ""): reverse-proxies to the Vite dev server
//     for all requests not matched by other routes (§10.C.2).
//   - Otherwise: serves the embedded web/dist/ SPA via embed.FS.
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
	return http.FileServer(http.FS(sub))
}
