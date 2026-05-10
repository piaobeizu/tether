// Package web exposes the compiled SPA as an embedded filesystem.
// Build step: `make build` runs `pnpm build` first (s3+); for s1/s2
// web/dist/index.html is a placeholder committed directly.
package web

import "embed"

//go:embed all:dist
var DistFS embed.FS
