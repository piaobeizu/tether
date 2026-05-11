// internal/mcp/host/config.go
package host

import "strings"

// Config holds all MCP server configurations for a Manager instance.
type Config struct {
	Servers map[string]ServerConfig
}

// ServerConfig describes a single MCP server connection (stdio, v0.3).
type ServerConfig struct {
	Name       string            // set from the map key by Manager; do not set manually
	Command    []string          // argv: Command[0] is the binary, rest are args
	Env        map[string]string // literal env vars (no ${TASK_*} expansion in v0.3)
	Prefix     string            // namespace prefix override (without trailing _)
	                             // if empty: strings.ReplaceAll(Name, "-", "_")
	InheritEnv []string          // extra os env var names to pass to the subprocess (e.g. "GOPATH", "NODE_ENV")
}

// HistoryLogger is a minimal interface for writing task-history events.
// Satisfied by a no-op in v0.3; will be wired to a real logger in v0.4.
type HistoryLogger interface {
	Append(eventType string, payload any) error
}

type noopLogger struct{}

func (noopLogger) Append(string, any) error { return nil }

// NoopLogger returns a HistoryLogger that discards all events.
func NoopLogger() HistoryLogger { return noopLogger{} }

// NamespacePrefix returns the namespace prefix for tools registered by serverName.
// Uses cfg.Prefix if set, otherwise derives from cfg.Name by replacing "-" with "_".
// The trailing "_" separator is appended here.
func NamespacePrefix(cfg ServerConfig) string {
	p := cfg.Prefix
	if p == "" {
		p = strings.ReplaceAll(cfg.Name, "-", "_")
	}
	return p + "_"
}
