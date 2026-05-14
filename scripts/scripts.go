// Package scripts embeds runtime scripts needed by the tether binary.
package scripts

import _ "embed"

// CCStream is the Node.js sidecar that drives the claude-agent-sdk async iterator,
// giving token-level streaming output from claude-code sessions.
//
//go:embed cc-stream.mjs
var CCStream []byte
