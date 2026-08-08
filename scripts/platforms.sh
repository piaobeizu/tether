# platforms.sh — the platforms tether ships. Sourced, not executed.
#
#   source scripts/platforms.sh   # defines PLATFORMS=(GOOS/GOARCH ...)
#
# scripts/release.sh builds this list, and CI cross-compiles and vets it
# (.github/workflows/ci.yml, "Cross-compile every released platform"), so
# adding a platform here puts every commit that reaches main under a compiler
# for it. Before that gate existed, internal/mcp/builtin used syscall.Setpgid
# and syscall.Kill with no build tags from 2026-05-12 to 2026-08-08: nothing in
# CI ever set GOOS, so windows broke at the tag rather than at the commit
# (tether#85).
#
# What the gate does not cover: ci.yml runs on pushes to main/pf2.* and on PRs
# to main, so a commit that reaches a tag without passing through one of those
# is still unchecked.
#
# .github/workflows/release.yml has a second copy of this list, as a job matrix.
# A YAML matrix cannot source a shell file and has to be literal, and the
# fromJSON indirection that would remove the copy is only exercised by pushing a
# tag — so the copy stays, and adding a platform means editing both files. The
# CI gate covers this file; it does not check that release.yml agrees with it.
PLATFORMS=(
	"linux/amd64"
	"linux/arm64"
	"darwin/amd64"
	"darwin/arm64"
	"windows/amd64"
)
