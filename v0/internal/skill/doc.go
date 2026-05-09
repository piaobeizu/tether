// Package skill implements the v0.1 tether skill model: cc plugin standard
// directory layout + an optional supplemental tether.toml manifest. See
// spec §11.Z (D-20) for the full design.
//
// Boundary contract: the daemon (internal/backend/claude, internal/agent)
// MUST NOT import this package. Skill execution is delegated entirely to
// cc's plugin loader + hook system; this package only handles install,
// manifest parsing, workspace materialisation (symlink), and CLI surface.
//
// Packages:
//
//   - internal/skill              manifest + install + workspace resolve
//   - internal/skill/adapter      per-agent materialisers; cc.go is the
//     only real implementation in v0.1, the rest are
//     panicking stubs reserved for v0.2.
//
// Forward-compat seams (D-18): blob-register CLI is wired but stubs out
// "not implemented in v0.1" so future skill code can reference it stably.
package skill
