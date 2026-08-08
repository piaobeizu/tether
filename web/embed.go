// Package web exposes the compiled SPA as an embedded filesystem.
//
// dist/ is `vite build` output and is NOT in git (see .gitignore). Build it with
// `make build`, which runs `pnpm build` before the go build so the SPA inside the
// binary is always the one compiled from the current web/src.
//
// If you arrived here from
//
//	web/embed.go: pattern all:dist: no matching files found
//
// then dist/ has never been built in this checkout: run `make build` instead of a
// bare `go build`. The same error stops every command that loads the package
// graph — `go vet`, `go test`, `go list ./...` — so an editor/gopls that cannot
// load the module has the same one-command fix.
//
// That hard failure is deliberate and is the fix for tether#81: dist/ used to be
// committed, which let a bare `go build`/`go install` compile happily against the
// committed bundle. Nothing gated that bundle against web/src, and it did go
// stale — one commit changed web/src without rebuilding it — with every test and
// CI job still green. The deploy that would have shipped the stale SPA against a
// current backend was caught by hand. There is no placeholder dist/ to make the
// build pass — vite's emptyOutDir wipes this directory on every build, so a
// committed marker inside it could only survive by turning emptyOutDir off, which
// would let stale hashed chunks accumulate in the embed root and reintroduce the
// same silent staleness one layer down.
//
// What is NOT closed by any of this: on a tree where dist/ has already been built
// once, a bare `go build` still succeeds and embeds whatever is sitting there.
// Editing web/src, or just switching branches — dist/ no longer moves with
// `git checkout` — leaves that stale. `make build` rebuilds every time, which is
// why it is the only supported build.
//
// What a finished binary carries is checkable after the fact:
// `scripts/spa-bundle.sh print <binary>` reports the build id that
// `scripts/spa-bundle.sh stamp` compiled into dist/BUILD-ID.
package web

import "embed"

//go:embed all:dist
var DistFS embed.FS
